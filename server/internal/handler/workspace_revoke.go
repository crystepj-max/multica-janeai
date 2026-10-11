package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

var errMemberRunsNotDrained = errors.New("member has active or stopping Runs")

// revokeAndRemoveMember converges server-side state after the member's Runs
// have drained: it archives their agents, cancels queued tasks, takes their
// runtimes offline, removes their daemon tokens and subscriptions, and deletes
// the member row. If an active Run or cancellation acknowledgement is pending,
// it aborts the transaction so the credentials and runtime identity remain
// available for the stop request to finish.
//
// All DB writes run inside a single transaction so a partial revocation never
// leaves the workspace half-converged — e.g. a member who is "gone" but whose
// runtime row is still active. Once the transaction commits, daemon_token
// cache entries are invalidated and events are published (see
// publishRevocation) so connected clients and other workspace members observe
// the new state immediately.
//
// The run-drain check happens inside the same transaction as the archive,
// cancellation, credential revocation and member deletion. A conflict rolls
// all of those writes back, so a daemon that still needs to acknowledge its
// stop retains the workspace access required to do so.
//
// archivedBy is the actor who triggered the revocation. For DeleteMember it's
// the requester (the admin doing the kick); for LeaveWorkspace it's the leaver
// themselves.
func (h *Handler) revokeAndRemoveMember(ctx context.Context, workspaceID, userID, memberID, archivedBy pgtype.UUID) (revocationResult, error) {
	var empty revocationResult

	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)

	qtx := h.Queries.WithTx(tx)

	// Taken FIRST, before this tx touches member or issue_subscriber. The
	// delegated auto-subscribe rule takes the same (workspace, user) lock, so
	// a run that is mid-decomposition cannot slip a new subscriber row in
	// between this tx's membership delete and its subscription cleanup below.
	// First also means every holder acquires it in the same order, so these
	// paths cannot deadlock against each other (MUL-5483 review round 7).
	if err := qtx.LockSubscriberWrites(ctx, db.LockSubscriberWritesParams{
		WorkspaceID: workspaceID,
		UserID:      userID,
	}); err != nil {
		return empty, err
	}

	runtimes, err := qtx.ListAgentRuntimesByOwner(ctx, db.ListAgentRuntimesByOwnerParams{
		WorkspaceID: workspaceID,
		OwnerID:     userID,
	})
	if err != nil {
		return empty, err
	}

	result := revocationResult{Runtimes: runtimes}

	if len(runtimes) > 0 {
		runtimeIDs := make([]pgtype.UUID, len(runtimes))
		daemonIDs := make([]string, 0, len(runtimes))
		userAgentIDs := make([]pgtype.UUID, 0)
		for i, rt := range runtimes {
			runtimeIDs[i] = rt.ID
			if rt.DaemonID.Valid && rt.DaemonID.String != "" {
				daemonIDs = append(daemonIDs, rt.DaemonID.String)
			}
			// Match runtime teardown's lock order. This prevents a new agent
			// binding from appearing after the cancellation sweep and includes
			// already-archived agents whose old tasks may still reference a
			// different runtime.
			lockedRuntime, err := qtx.LockAgentRuntime(ctx, rt.ID)
			if err != nil {
				if isNotFound(err) {
					continue
				}
				return empty, fmt.Errorf("lock runtime during member revocation: %w", err)
			}
			agents, err := qtx.ListUserAgentsByRuntimeForUpdate(ctx, rt.ID)
			if err != nil {
				return empty, fmt.Errorf("lock runtime agents during member revocation: %w", err)
			}
			if err := service.ValidateRuntimeAgentWorkspaces(lockedRuntime, agents); err != nil {
				return empty, fmt.Errorf("validate runtime agents during member revocation: %w", err)
			}
			for _, agent := range agents {
				userAgentIDs = append(userAgentIDs, agent.ID)
			}
		}

		result.ArchivedAgents, err = qtx.ArchiveAgentsByRuntime(ctx, db.ArchiveAgentsByRuntimeParams{
			ArchivedBy: archivedBy,
			RuntimeIds: runtimeIDs,
		})
		if err != nil {
			return empty, err
		}

		// Cancel by runtime AND by archived agent. agent.runtime_id can be
		// reassigned via UpdateAgent without rewriting the runtime_id on
		// historical agent_task_queue rows, so an archived agent may still
		// have queued/running tasks pinned to a different runtime — and
		// ClaimAgentTask does not gate on agent.archived_at, so those tasks
		// would otherwise stay claimable after the agent is gone.
		result.CancelledTasks, err = qtx.CancelAgentTasksByRuntimeOrAgent(ctx, db.CancelAgentTasksByRuntimeOrAgentParams{
			RuntimeIds: runtimeIDs,
			AgentIds:   userAgentIDs,
		})
		if err != nil {
			return empty, err
		}
		awaitingStop, err := qtx.CountTasksAwaitingStopForRuntimeOrAgent(ctx, db.CountTasksAwaitingStopForRuntimeOrAgentParams{
			RuntimeIds: runtimeIDs,
			AgentIds:   userAgentIDs,
		})
		if err != nil {
			return empty, fmt.Errorf("check runtime stop acknowledgements during member revocation: %w", err)
		}
		if awaitingStop > 0 {
			return empty, fmt.Errorf("%w: %d runs need a runtime stop acknowledgement before member removal", errMemberRunsNotDrained, awaitingStop)
		}
		if err = service.SettleTerminalTaskState(ctx, qtx, result.CancelledTasks...); err != nil {
			return empty, err
		}

		result.OfflineRuntimeIDs, err = qtx.ForceOfflineRuntimesByIDs(ctx, runtimeIDs)
		if err != nil {
			return empty, err
		}

		if len(daemonIDs) > 0 {
			result.RevokedTokenHashes, err = qtx.DeleteDaemonTokensByWorkspaceAndDaemons(ctx, db.DeleteDaemonTokensByWorkspaceAndDaemonsParams{
				WorkspaceID: workspaceID,
				DaemonIds:   daemonIDs,
			})
			if err != nil {
				return empty, err
			}
		}
	}

	// channel_user_binding used to carry a member FK with ON DELETE CASCADE, so
	// a removed member's IM bindings vanished automatically. MUL-3515 §4 dropped
	// every channel_* foreign key, moving that integrity rule to the application
	// layer: prune the bindings here, in the same tx as the member-row delete.
	// The inbound path also re-checks membership (see ChannelStore.IsWorkspaceMember),
	// but pruning stops a stale binding from lingering across a remove/re-add.
	if err := qtx.DeleteChannelUserBindingsByWorkspaceMember(ctx, db.DeleteChannelUserBindingsByWorkspaceMemberParams{
		WorkspaceID:   workspaceID,
		MulticaUserID: userID,
	}); err != nil {
		return empty, err
	}

	// agent_invocation_target carries member-target grants with NO database FK
	// (MUL-3963 keeps the new table FK-free, matching the MUL-3515 channel
	// generalization). Prune this leaving member's grants in the same tx as the
	// member-row delete so a re-invited user does not silently reclaim old
	// invocation permission on agents that had allow-listed them. SCOPED to
	// this workspace: the same user may belong to other workspaces, and
	// removing them here must not touch their grants on agents elsewhere.
	if err := qtx.DeleteAgentInvocationTargetsByMember(ctx, db.DeleteAgentInvocationTargetsByMemberParams{
		WorkspaceID: workspaceID,
		TargetID:    userID,
	}); err != nil {
		return empty, err
	}

	// A private quick action is visible and runnable ONLY by its creator
	// (quick_action carries no FK, so nothing removes it implicitly). Once the
	// creator is gone the row is unreachable by every remaining member while
	// still consuming the workspace's active-action limit, so drop those in
	// the same tx. Public actions are workspace furniture and survive.
	if err := qtx.DeletePrivateQuickActionsByCreator(ctx, db.DeletePrivateQuickActionsByCreatorParams{
		WorkspaceID: workspaceID,
		CreatedByID: userID,
	}); err != nil {
		return empty, err
	}

	// Saved views follow the private-quick-action rule above: a departed
	// member's PRIVATE views are invisible to everyone left yet still count
	// against quota, and a re-invite must not resurrect them. Shared views
	// stay — they are workspace furniture other members may rely on. The
	// per-user view-bar preferences are meaningless without the member.
	if err := qtx.DeletePrivateIssueViewsByOwner(ctx, db.DeletePrivateIssueViewsByOwnerParams{
		WorkspaceID: workspaceID,
		OwnerID:     userID,
	}); err != nil {
		return empty, err
	}
	if err := qtx.DeleteIssueViewPreferencesByUser(ctx, db.DeleteIssueViewPreferencesByUserParams{
		WorkspaceID: workspaceID,
		UserID:      userID,
	}); err != nil {
		return empty, err
	}

	// issue_subscriber carries no FK either (same MUL-3515 rule as the two
	// prunes above), and MUL-5483 gave agents a path that writes member
	// subscriber rows on their own initiative. Dropping them in this tx is what
	// stops a departed member from accruing inbox rows, and stops a re-invite
	// from silently restoring visibility of everything they used to watch.
	if err := qtx.DeleteSubscriptionsByMember(ctx, db.DeleteSubscriptionsByMemberParams{
		WorkspaceID: workspaceID,
		UserID:      userID,
	}); err != nil {
		return empty, err
	}

	// autopilot_subscriber is another FK-free subscription template. Leaving
	// stale rows here made the detail API return a user the member picker could
	// no longer render; the next full-replace PATCH then failed membership
	// validation, blocking every edit to that autopilot. Prune only templates in
	// this workspace so the same user's subscriptions elsewhere survive.
	if err := qtx.DeleteAutopilotSubscribersByMember(ctx, db.DeleteAutopilotSubscribersByMemberParams{
		WorkspaceID: workspaceID,
		UserID:      userID,
	}); err != nil {
		return empty, err
	}

	// Member row deletion lives inside the same tx so a successful revoke is
	// never followed by a failed member-delete (which would leave the user
	// still a member with a dead runtime), and a failed revoke never leaves
	// the user out of the workspace with a still-online runtime.
	if h.seatCapacityEnabled() {
		if err := enqueueMemberCapacityRelease(ctx, qtx, uuid.UUID(workspaceID.Bytes), uuid.UUID(memberID.Bytes)); err != nil {
			return empty, err
		}
	}
	if err := qtx.DeleteMember(ctx, memberID); err != nil {
		return empty, err
	}

	if err := tx.Commit(ctx); err != nil {
		return empty, err
	}

	return result, nil
}

// revocationResult captures everything revokeMemberRuntimes touched so the
// caller can fan out events and analytics after the transaction commits.
// Publishing inside the transaction would let subscribers observe a state the
// tx might still roll back (see TaskService.BroadcastCancelledTasks docstring).
type revocationResult struct {
	Runtimes           []db.AgentRuntime
	ArchivedAgents     []db.Agent
	CancelledTasks     []db.AgentTaskQueue
	OfflineRuntimeIDs  []db.ForceOfflineRuntimesByIDsRow
	RevokedTokenHashes []string
}

func (r revocationResult) isEmpty() bool {
	return len(r.Runtimes) == 0
}

// publishRevocation runs all post-commit side effects: invalidate daemon token
// cache, broadcast task:cancelled with per-agent reconciliation, broadcast
// agent:archived, and signal a runtime-list refresh. Safe to call on an empty
// result — it returns immediately.
func (h *Handler) publishRevocation(ctx context.Context, result revocationResult, workspaceIDStr, actorType, actorIDStr string) {
	if result.isEmpty() {
		return
	}

	for _, hash := range result.RevokedTokenHashes {
		h.DaemonTokenCache.Invalidate(ctx, hash)
	}

	// Per-task cancellation: TaskService handles status reconciliation and
	// per-task event broadcast. Run this before the agent:archived burst so
	// subscribers see "task cancelled" before the parent agent disappears
	// from active lists, matching the order ArchiveAgent uses.
	if h.TaskService != nil && len(result.CancelledTasks) > 0 {
		// Revocation only archives agents, so a per-task lookup would still
		// resolve here; the workspace is passed for the same reason as
		// everywhere else — it is known, and it is the one being revoked.
		h.TaskService.BroadcastCancelledTasks(ctx, workspaceIDStr, result.CancelledTasks)
	}

	for _, agent := range result.ArchivedAgents {
		h.publish(protocol.EventAgentArchived, workspaceIDStr, actorType, actorIDStr, map[string]any{
			"agent": h.agentToResponse(agent),
		})
	}

	// Tell connected clients to refresh the runtime list. We piggyback on
	// EventDaemonRegister with a "revoke" action — same channel the runtime
	// delete handler uses — so the frontend invalidates its cached list
	// without us having to introduce a new event type the desktop app would
	// need a build to learn about.
	if len(result.OfflineRuntimeIDs) > 0 {
		h.publish(protocol.EventDaemonRegister, workspaceIDStr, actorType, actorIDStr, map[string]any{
			"action": "revoke",
		})
	}
}

// logRevocation emits a structured info line summarising the revocation. Kept
// separate from publish so the log is identical whether or not the bus is wired.
func logRevocation(result revocationResult, workspaceID, userID string, attrs ...any) {
	if result.isEmpty() {
		return
	}
	base := []any{
		"workspace_id", workspaceID,
		"user_id", userID,
		"runtimes_revoked", len(result.Runtimes),
		"agents_archived", len(result.ArchivedAgents),
		"tasks_cancelled", len(result.CancelledTasks),
		"runtimes_taken_offline", len(result.OfflineRuntimeIDs),
		"daemon_tokens_revoked", len(result.RevokedTokenHashes),
	}
	slog.Info("member runtimes revoked", append(base, attrs...)...)
}
