package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/requestkey"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

var (
	ErrIssueDispatchTransactionsRequired = errors.New("issue dispatch requires transaction support")
	ErrIssueDispatchKeyConflict          = errors.New("Idempotency-Key was already used for a different dispatch request")
	ErrIssueDispatchRecordIncomplete     = errors.New("issue dispatch idempotency record has no task")
	ErrIssueDispatchActorRequired        = errors.New("issue dispatch requires a member or agent actor")
	ErrIssueDispatchRevisionRequired     = errors.New("issue dispatch requires a positive expected revision")
	ErrIssueDispatchNotTodo              = errors.New("issue dispatch requires the issue to remain in todo")
	ErrIssueDispatchActiveTask           = errors.New("issue already has an active or pending task for its assignee")
	ErrIssueDispatchUnsupportedAssignee  = errors.New("issue dispatch requires an agent or squad assignee")
	ErrIssueDispatchTargetUnavailable    = errors.New("issue dispatch target has no usable runtime")
)

type IssueDispatchRevisionConflictError struct {
	Expected int64
	Actual   int64
}

func (e *IssueDispatchRevisionConflictError) Error() string {
	return fmt.Sprintf("issue changed since planning: expected revision %d, current revision %d", e.Expected, e.Actual)
}

func (e *IssueDispatchRevisionConflictError) Unwrap() error {
	return ErrIssueDispatchRevisionConflict
}

var ErrIssueDispatchRevisionConflict = errors.New("issue dispatch revision conflict")

// DispatchIssueWithIdempotencyKey creates one queued Run for a todo issue at
// the caller's expected revision. The request row and task row commit
// atomically, so a retry after a lost response returns the original task.
func (s *TaskService) DispatchIssueWithIdempotencyKey(
	ctx context.Context,
	workspaceID pgtype.UUID,
	issueID pgtype.UUID,
	key string,
	expectedRevision int64,
	actorType string,
	actorID pgtype.UUID,
	actorUserID pgtype.UUID,
	canInvoke func(agent db.Agent) bool,
) (db.AgentTaskQueue, bool, error) {
	if s == nil || s.TxStarter == nil {
		return db.AgentTaskQueue{}, false, ErrIssueDispatchTransactionsRequired
	}
	key, err := requestkey.ParseIdempotencyKey(key)
	if err != nil {
		return db.AgentTaskQueue{}, false, err
	}
	if expectedRevision < 1 {
		return db.AgentTaskQueue{}, false, ErrIssueDispatchRevisionRequired
	}
	if (actorType != "member" && actorType != "agent") || !actorID.Valid {
		return db.AgentTaskQueue{}, false, ErrIssueDispatchActorRequired
	}

	var (
		task     db.AgentTaskQueue
		replayed bool
	)
	err = s.runInTx(ctx, func(qtx *db.Queries) error {
		_, err := qtx.ReserveIssueDispatchRequest(ctx, db.ReserveIssueDispatchRequestParams{
			WorkspaceID:           workspaceID,
			IdempotencyKey:        key,
			IssueID:               issueID,
			ExpectedIssueRevision: expectedRevision,
			ActorType:             actorType,
			ActorID:               actorID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			existing, lookupErr := qtx.GetIssueDispatchRequest(ctx, db.GetIssueDispatchRequestParams{
				WorkspaceID:    workspaceID,
				IdempotencyKey: key,
			})
			if lookupErr != nil {
				return fmt.Errorf("load issue dispatch idempotency record: %w", lookupErr)
			}
			if existing.IssueID != issueID || existing.ActorType != actorType || existing.ActorID != actorID {
				return ErrIssueDispatchKeyConflict
			}
			if existing.ExpectedIssueRevision != expectedRevision {
				return ErrIssueDispatchKeyConflict
			}
			if !existing.TaskID.Valid {
				return ErrIssueDispatchRecordIncomplete
			}
			task, err = qtx.GetAgentTask(ctx, existing.TaskID)
			if err != nil {
				return fmt.Errorf("load idempotent issue dispatch task: %w", err)
			}
			if !task.IssueID.Valid || task.IssueID != issueID {
				return ErrIssueDispatchRecordIncomplete
			}
			agent, agentErr := qtx.GetAgent(ctx, task.AgentID)
			if agentErr != nil {
				return fmt.Errorf("load idempotent issue dispatch agent: %w", agentErr)
			}
			if canInvoke != nil && !canInvoke(agent) {
				return ErrRerunInvokeNotAllowed
			}
			replayed = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("reserve issue dispatch idempotency key: %w", err)
		}

		issue, err := qtx.LockIssueForDispatch(ctx, db.LockIssueForDispatchParams{
			ID:          issueID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			return fmt.Errorf("lock issue for dispatch: %w", err)
		}
		if issue.Revision != expectedRevision {
			return &IssueDispatchRevisionConflictError{Expected: expectedRevision, Actual: issue.Revision}
		}
		if issue.TriageState.Valid {
			return ErrIssueInTriage
		}
		if issue.Status != "todo" || issuestatus.Effective(ctx, qtx, issue.WorkspaceID, issue.Status) != "todo" {
			return ErrIssueDispatchNotTodo
		}

		var (
			targetAgentID pgtype.UUID
			squadID       pgtype.UUID
			isLeader      bool
		)
		switch issue.AssigneeType.String {
		case "agent":
			if !issue.AssigneeID.Valid {
				return ErrIssueDispatchUnsupportedAssignee
			}
			targetAgentID = issue.AssigneeID
		case "squad":
			if !issue.AssigneeID.Valid {
				return ErrIssueDispatchUnsupportedAssignee
			}
			squad, squadErr := qtx.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{
				ID:          issue.AssigneeID,
				WorkspaceID: issue.WorkspaceID,
			})
			if squadErr != nil {
				return fmt.Errorf("load issue dispatch squad: %w", squadErr)
			}
			targetAgentID = squad.LeaderID
			squadID = squad.ID
			isLeader = true
		default:
			return ErrIssueDispatchUnsupportedAssignee
		}

		agent, err := qtx.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{
			ID:          targetAgentID,
			WorkspaceID: issue.WorkspaceID,
		})
		if err != nil {
			return fmt.Errorf("load issue dispatch agent: %w", err)
		}
		if agent.ArchivedAt.Valid || !agent.RuntimeID.Valid {
			return ErrIssueDispatchTargetUnavailable
		}
		if canInvoke != nil && !canInvoke(agent) {
			return ErrRerunInvokeNotAllowed
		}
		dispatchRevision := pgtype.Int8{Int64: expectedRevision, Valid: true}
		existing, existingErr := qtx.GetIssueTaskForDispatchRevision(ctx, db.GetIssueTaskForDispatchRevisionParams{
			IssueID: issue.ID, AgentID: targetAgentID, DispatchIssueRevision: dispatchRevision,
		})
		if existingErr == nil {
			active, err := qtx.HasActiveTaskForIssueAndAgent(ctx, db.HasActiveTaskForIssueAndAgentParams{
				IssueID: issue.ID,
				AgentID: targetAgentID,
			})
			if err != nil {
				return fmt.Errorf("check active issue dispatch task: %w", err)
			}
			if active {
				return ErrIssueDispatchActiveTask
			}
			task = existing
			replayed = true
			updated, err := qtx.SetIssueDispatchRequestTaskID(ctx, db.SetIssueDispatchRequestTaskIDParams{
				WorkspaceID: workspaceID, IdempotencyKey: key, TaskID: task.ID,
			})
			if err != nil {
				return fmt.Errorf("attach existing issue dispatch task to idempotency record: %w", err)
			}
			if updated != 1 {
				return fmt.Errorf("attach existing issue dispatch task to idempotency record: expected one row, updated %d", updated)
			}
			return nil
		}
		if !errors.Is(existingErr, pgx.ErrNoRows) {
			return fmt.Errorf("check issue dispatch revision task: %w", existingErr)
		}
		active, err := qtx.HasActiveTaskForIssueAndAgent(ctx, db.HasActiveTaskForIssueAndAgentParams{
			IssueID: issue.ID,
			AgentID: targetAgentID,
		})
		if err != nil {
			return fmt.Errorf("check active issue dispatch task: %w", err)
		}
		if active {
			return ErrIssueDispatchActiveTask
		}

		// Copy only the shared collaborators. TaskService also owns sync.Map,
		// atomics, and mutex-protected caches, so copying the whole value is unsafe
		// and fails the repository's copylocks vet check.
		txService := &TaskService{
			Queries:              qtx,
			TxStarter:            s.TxStarter,
			deferEnqueueEvents:   true,
			Hub:                  s.Hub,
			Bus:                  s.Bus,
			Analytics:            s.Analytics,
			Metrics:              s.Metrics,
			Wakeup:               s.Wakeup,
			Entitlements:         s.Entitlements,
			SourceContextStorage: s.SourceContextStorage,
			FeatureFlags:         s.FeatureFlags,
			EmptyClaim:           s.EmptyClaim,
			ReclaimCheck:         s.ReclaimCheck,
			Composio:             s.Composio,
			QuickActions:         s.QuickActions,
		}
		if isLeader {
			task, replayed, err = txService.EnqueueTaskForSquadLeaderRunTriggerWithHandoff(ctx, issue, targetAgentID, squadID, "", actorUserID)
		} else {
			task, replayed, err = txService.EnqueueTaskForIssueRunTrigger(ctx, issue, "", actorUserID)
		}
		if err != nil {
			return fmt.Errorf("enqueue issue dispatch task: %w", err)
		}
		updated, err := qtx.SetIssueDispatchRequestTaskID(ctx, db.SetIssueDispatchRequestTaskIDParams{
			WorkspaceID:    workspaceID,
			IdempotencyKey: key,
			TaskID:         task.ID,
		})
		if err != nil {
			return fmt.Errorf("attach issue dispatch task to idempotency record: %w", err)
		}
		if updated != 1 {
			return fmt.Errorf("attach issue dispatch task to idempotency record: expected one row, updated %d", updated)
		}
		return nil
	})
	if err != nil {
		return db.AgentTaskQueue{}, false, err
	}
	s.publishIssueDispatchResult(ctx, task, replayed)
	return task, replayed, nil
}

func (s *TaskService) publishIssueDispatchResult(ctx context.Context, task db.AgentTaskQueue, replayed bool) {
	if replayed {
		// The database transaction can commit before the original request process
		// broadcasts or wakes the daemon. A retry is the recovery signal for that
		// crash window, but only a still-queued task needs a wakeup. Keep this to
		// the daemon hint: replaying the UI event or enqueue metric would count a
		// second enqueue even though no new task was created.
		if task.Status == "queued" {
			s.notifyTaskAvailable(task)
		}
		return
	}
	s.broadcastTaskEvent(ctx, protocol.EventTaskQueued, task)
	s.NotifyTaskEnqueued(ctx, task)
}
