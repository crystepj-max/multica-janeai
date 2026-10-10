package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestIssueDispatchReplayOnlyWakesStillQueuedTask(t *testing.T) {
	bus := events.New()
	var queuedEvents int
	bus.Subscribe(protocol.EventTaskQueued, func(events.Event) { queuedEvents++ })
	wakeup := &stubWakeup{}
	svc := &TaskService{Bus: bus, Wakeup: wakeup}
	runtimeID := testUUID(31)
	taskID := testUUID(32)

	svc.publishIssueDispatchResult(context.Background(), db.AgentTaskQueue{
		ID: taskID, RuntimeID: runtimeID, Status: "queued",
	}, true)
	if len(wakeup.calls) != 1 {
		t.Fatalf("queued replay wakeups = %d, want 1", len(wakeup.calls))
	}
	if wakeup.calls[0].taskID != util.UUIDToString(taskID) {
		t.Fatalf("queued replay wakeup task = %q, want %q", wakeup.calls[0].taskID, util.UUIDToString(taskID))
	}

	svc.publishIssueDispatchResult(context.Background(), db.AgentTaskQueue{
		ID: taskID, RuntimeID: runtimeID, Status: "running",
	}, true)
	if len(wakeup.calls) != 1 {
		t.Fatalf("nonqueued replay wakeups = %d, want no additional wakeup", len(wakeup.calls))
	}
	if queuedEvents != 0 {
		t.Fatalf("dispatch replay queued UI events = %d, want 0", queuedEvents)
	}
}

func TestDispatchIssueWithIdempotencyKeyConcurrentReplay(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE issue SET status = 'todo' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("set issue to todo: %v", err)
	}
	expectedRevision := dispatchTestIssueRevision(t, ctx, pool, issueID)
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM issue_dispatch_request WHERE issue_id = $1`, issueID)
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})

	svc := &TaskService{Queries: db.New(pool), TxStarter: pool, Bus: events.New()}
	workspaceUUID := util.MustParseUUID(workspaceID)
	issueUUID := util.MustParseUUID(issueID)
	actorUUID := util.MustParseUUID(userID)
	start := make(chan struct{})
	type result struct {
		task     db.AgentTaskQueue
		replayed bool
		err      error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			task, replayed, err := svc.DispatchIssueWithIdempotencyKey(
				ctx, workspaceUUID, issueUUID, "concurrent-dispatch", expectedRevision, "member", actorUUID, actorUUID, func(db.Agent) bool { return true },
			)
			results <- result{task: task, replayed: replayed, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var firstID pgtype.UUID
	created, replayedCount := 0, 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent dispatch: %v", result.err)
		}
		if !firstID.Valid {
			firstID = result.task.ID
		} else if result.task.ID != firstID {
			t.Fatalf("requests returned different task ids: %s and %s", util.UUIDToString(firstID), util.UUIDToString(result.task.ID))
		}
		if result.replayed {
			replayedCount++
		} else {
			created++
		}
	}
	if created != 1 || replayedCount != 1 {
		t.Fatalf("created=%d replayed=%d, want one creation and one replay", created, replayedCount)
	}

	var taskCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, agentID).Scan(&taskCount); err != nil {
		t.Fatalf("count dispatch tasks: %v", err)
	}
	if taskCount != 1 {
		t.Fatalf("dispatch task count = %d, want 1", taskCount)
	}

	otherIssueID := util.MustParseUUID("00000000-0000-0000-0000-000000000001")
	if _, _, err := svc.DispatchIssueWithIdempotencyKey(ctx, workspaceUUID, otherIssueID, "concurrent-dispatch", expectedRevision, "member", actorUUID, actorUUID, nil); !errors.Is(err, ErrIssueDispatchKeyConflict) {
		t.Fatalf("same key for a different issue: err = %v, want key conflict", err)
	}
	otherActorID := util.MustParseUUID("00000000-0000-0000-0000-000000000002")
	if _, _, err := svc.DispatchIssueWithIdempotencyKey(ctx, workspaceUUID, issueUUID, "concurrent-dispatch", expectedRevision, "member", otherActorID, otherActorID, nil); !errors.Is(err, ErrIssueDispatchKeyConflict) {
		t.Fatalf("same key for a different actor: err = %v, want key conflict", err)
	}
	if _, _, err := svc.DispatchIssueWithIdempotencyKey(ctx, workspaceUUID, issueUUID, "concurrent-dispatch", expectedRevision+1, "member", actorUUID, actorUUID, nil); !errors.Is(err, ErrIssueDispatchKeyConflict) {
		t.Fatalf("same key for a different expected revision: err = %v, want key conflict", err)
	}

	cancelled, err := svc.CancelTask(ctx, firstID)
	if err != nil {
		t.Fatalf("cancel original task: %v", err)
	}
	if cancelled.Status != "cancelled" {
		t.Fatalf("original task status after cancel = %q, want cancelled", cancelled.Status)
	}
	replayedTask, isReplay, err := svc.DispatchIssueWithIdempotencyKey(
		ctx, workspaceUUID, issueUUID, "concurrent-dispatch", expectedRevision, "member", actorUUID, actorUUID, func(db.Agent) bool { return true },
	)
	if err != nil {
		t.Fatalf("replay terminal task: %v", err)
	}
	if !isReplay || replayedTask.ID != firstID || replayedTask.Status != "cancelled" {
		t.Fatalf("terminal replay = (id=%s, status=%s, replayed=%t), want original cancelled task",
			util.UUIDToString(replayedTask.ID), replayedTask.Status, isReplay)
	}
}

func TestDispatchIssueDoesNotReplaceEarlierPendingTask(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE issue SET status = 'todo' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("set issue to todo: %v", err)
	}
	expectedRevision := dispatchTestIssueRevision(t, ctx, pool, issueID)
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM issue_dispatch_request WHERE issue_id = $1`, issueID)
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})

	issue := db.Issue{
		ID:           util.MustParseUUID(issueID),
		Revision:     expectedRevision,
		AssigneeID:   util.MustParseUUID(agentID),
		Priority:     "medium",
		CreatorType:  "member",
		CreatorID:    util.MustParseUUID(userID),
		WorkspaceID:  util.MustParseUUID(workspaceID),
		AssigneeType: pgtype.Text{String: "agent", Valid: true},
	}
	svc := &TaskService{Queries: db.New(pool), TxStarter: pool, Bus: events.New()}
	existing, err := svc.EnqueueTaskForIssue(ctx, issue)
	if err != nil {
		t.Fatalf("enqueue existing pending task: %v", err)
	}

	// 已有 Run 属于上一任务周期；新的 revision 仍不得绕过未停止的执行。
	if _, err := pool.Exec(ctx, `UPDATE issue SET revision = revision + 1 WHERE id = $1`, issueID); err != nil {
		t.Fatalf("advance issue revision: %v", err)
	}
	expectedRevision = dispatchTestIssueRevision(t, ctx, pool, issueID)

	_, _, err = svc.DispatchIssueWithIdempotencyKey(
		ctx, issue.WorkspaceID, issue.ID, "dispatch-with-pending-task", expectedRevision, "member", issue.CreatorID, issue.CreatorID, func(db.Agent) bool { return true },
	)
	if !errors.Is(err, ErrIssueDispatchActiveTask) {
		t.Fatalf("dispatch with existing pending task: err = %v, want ErrIssueDispatchActiveTask", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id = $1`, existing.ID).Scan(&status); err != nil {
		t.Fatalf("read existing pending task: %v", err)
	}
	if status != "queued" {
		t.Fatalf("existing task status = %q, want queued", status)
	}
	var requests int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issue_dispatch_request WHERE workspace_id = $1 AND idempotency_key = $2`, issue.WorkspaceID, "dispatch-with-pending-task").Scan(&requests); err != nil {
		t.Fatalf("count failed dispatch reservation: %v", err)
	}
	if requests != 0 {
		t.Fatalf("failed dispatch left %d idempotency records, want 0", requests)
	}
}

func TestDispatchIssueReusesAutomaticRunForSameIssueRevision(t *testing.T) {
	for _, status := range []string{"queued", "running", "completed"} {
		t.Run(status, func(t *testing.T) {
			pool := newResolveOriginatorPool(t)
			ctx := context.Background()
			workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
			if _, err := pool.Exec(ctx, `UPDATE issue SET status = 'todo' WHERE id = $1`, issueID); err != nil {
				t.Fatalf("set issue to todo: %v", err)
			}
			expectedRevision := dispatchTestIssueRevision(t, ctx, pool, issueID)
			t.Cleanup(func() {
				pool.Exec(context.Background(), `DELETE FROM issue_dispatch_request WHERE issue_id = $1`, issueID)
				pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
			})

			workspaceUUID := util.MustParseUUID(workspaceID)
			issueUUID := util.MustParseUUID(issueID)
			actorUUID := util.MustParseUUID(userID)
			issue, err := db.New(pool).GetIssue(ctx, issueUUID)
			if err != nil {
				t.Fatalf("load issue: %v", err)
			}
			svc := &TaskService{Queries: db.New(pool), TxStarter: pool, Bus: events.New()}
			automatic, reused, err := svc.EnqueueTaskForIssueRunTrigger(ctx, issue, "", actorUUID)
			if err != nil {
				t.Fatalf("enqueue automatic issue run: %v", err)
			}
			if reused {
				t.Fatal("first automatic issue run was marked reused")
			}
			if automatic.AgentID != util.MustParseUUID(agentID) {
				t.Fatalf("automatic run agent = %s, want %s", util.UUIDToString(automatic.AgentID), agentID)
			}
			if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status = $2 WHERE id = $1`, automatic.ID, status); err != nil {
				t.Fatalf("set automatic run status: %v", err)
			}

			dispatched, replayed, err := svc.DispatchIssueWithIdempotencyKey(
				ctx, workspaceUUID, issueUUID, "dispatch-same-revision", expectedRevision, "member", actorUUID, actorUUID, func(db.Agent) bool { return true },
			)
			if err != nil {
				t.Fatalf("dispatch same issue revision: %v", err)
			}
			if !replayed || dispatched.ID != automatic.ID || dispatched.Status != status {
				t.Fatalf("dispatch result = (id=%s, status=%s, reused=%t), want the existing %s run %s",
					util.UUIDToString(dispatched.ID), dispatched.Status, replayed, status, util.UUIDToString(automatic.ID))
			}

			var taskCount int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, agentID).Scan(&taskCount); err != nil {
				t.Fatalf("count issue runs: %v", err)
			}
			if taskCount != 1 {
				t.Fatalf("issue runs for same revision = %d, want 1", taskCount)
			}
		})
	}
}

func TestEnqueueAssignedIssueWithoutCommentStampsDispatchRevision(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	_, _, _, issueID := seedAttributionFixture(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE issue SET status = 'todo' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("set issue to todo: %v", err)
	}
	issueUUID := util.MustParseUUID(issueID)
	issue, err := db.New(pool).GetIssue(ctx, issueUUID)
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueUUID)
	})

	svc := &TaskService{Queries: db.New(pool), TxStarter: pool, Bus: events.New()}
	task, err := svc.EnqueueTaskForIssue(ctx, issue)
	if err != nil {
		t.Fatalf("enqueue assigned issue: %v", err)
	}
	if !task.DispatchIssueRevision.Valid || task.DispatchIssueRevision.Int64 != issue.Revision {
		t.Fatalf("assignment Run dispatch revision = %+v, want issue revision %d", task.DispatchIssueRevision, issue.Revision)
	}
}

func TestEnqueueSquadAssignedIssueWithoutCommentStampsDispatchRevision(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	var squadID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO squad (workspace_id, name, leader_id, creator_id)
		VALUES ($1, 'dispatch revision squad', $2, $3)
		RETURNING id`, workspaceID, agentID, userID).Scan(&squadID); err != nil {
		t.Fatalf("create squad: %v", err)
	}
	issueUUID := util.MustParseUUID(issueID)
	squadUUID := util.MustParseUUID(squadID)
	if _, err := pool.Exec(ctx, `
		UPDATE issue
		SET status = 'todo', assignee_type = 'squad', assignee_id = $1
		WHERE id = $2`, squadUUID, issueUUID); err != nil {
		t.Fatalf("set issue todo and squad assignment: %v", err)
	}
	issue, err := db.New(pool).GetIssue(ctx, issueUUID)
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueUUID)
		pool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, squadUUID)
	})

	svc := &TaskService{Queries: db.New(pool), TxStarter: pool, Bus: events.New()}
	task, err := svc.EnqueueTaskForSquadLeader(ctx, issue, util.MustParseUUID(agentID), squadUUID, pgtype.UUID{}, OriginDerived)
	if err != nil {
		t.Fatalf("enqueue squad-assigned issue: %v", err)
	}
	if !task.DispatchIssueRevision.Valid || task.DispatchIssueRevision.Int64 != issue.Revision {
		t.Fatalf("squad assignment Run dispatch revision = %+v, want issue revision %d", task.DispatchIssueRevision, issue.Revision)
	}
}

func TestEnqueueAssignedIssueWithoutRevisionFailsClosed(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE issue SET status = 'todo' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("set issue to todo: %v", err)
	}
	issueUUID := util.MustParseUUID(issueID)
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueUUID)
	})

	svc := &TaskService{Queries: db.New(pool), TxStarter: pool, Bus: events.New()}
	_, err := svc.EnqueueTaskForIssue(ctx, db.Issue{
		ID:           issueUUID,
		WorkspaceID:  util.MustParseUUID(workspaceID),
		CreatorType:  "member",
		CreatorID:    util.MustParseUUID(userID),
		AssigneeType: pgtype.Text{String: "agent", Valid: true},
		AssigneeID:   util.MustParseUUID(agentID),
		Priority:     "medium",
	})
	if !errors.Is(err, ErrIssueDispatchRevisionRequired) {
		t.Fatalf("enqueue without issue revision: err = %v, want ErrIssueDispatchRevisionRequired", err)
	}
	var tasks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueUUID).Scan(&tasks); err != nil {
		t.Fatalf("count tasks after invalid revision: %v", err)
	}
	if tasks != 0 {
		t.Fatalf("invalid-revision enqueue created %d tasks, want 0", tasks)
	}
}

func TestEnqueueSquadAssignedIssueWithoutRevisionFailsClosed(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	var squadID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO squad (workspace_id, name, leader_id, creator_id)
		VALUES ($1, 'missing revision squad', $2, $3)
		RETURNING id`, workspaceID, agentID, userID).Scan(&squadID); err != nil {
		t.Fatalf("create squad: %v", err)
	}
	issueUUID := util.MustParseUUID(issueID)
	squadUUID := util.MustParseUUID(squadID)
	if _, err := pool.Exec(ctx, `
		UPDATE issue
		SET status = 'todo', assignee_type = 'squad', assignee_id = $1
		WHERE id = $2`, squadUUID, issueUUID); err != nil {
		t.Fatalf("set issue todo and squad assignment: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueUUID)
		pool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, squadUUID)
	})

	svc := &TaskService{Queries: db.New(pool), TxStarter: pool, Bus: events.New()}
	_, err := svc.EnqueueTaskForSquadLeader(ctx, db.Issue{
		ID:           issueUUID,
		WorkspaceID:  util.MustParseUUID(workspaceID),
		CreatorType:  "member",
		CreatorID:    util.MustParseUUID(userID),
		AssigneeType: pgtype.Text{String: "squad", Valid: true},
		AssigneeID:   squadUUID,
		Priority:     "medium",
	}, util.MustParseUUID(agentID), squadUUID, pgtype.UUID{}, OriginDerived)
	if !errors.Is(err, ErrIssueDispatchRevisionRequired) {
		t.Fatalf("enqueue squad issue without revision: err = %v, want ErrIssueDispatchRevisionRequired", err)
	}
	var tasks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueUUID).Scan(&tasks); err != nil {
		t.Fatalf("count tasks after invalid revision: %v", err)
	}
	if tasks != 0 {
		t.Fatalf("invalid-revision squad enqueue created %d tasks, want 0", tasks)
	}
}

func TestAutomaticAndExplicitIssueDispatchRaceCreatesOneRun(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE issue SET status = 'todo' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("set issue to todo: %v", err)
	}
	expectedRevision := dispatchTestIssueRevision(t, ctx, pool, issueID)
	issueUUID := util.MustParseUUID(issueID)
	workspaceUUID := util.MustParseUUID(workspaceID)
	actorUUID := util.MustParseUUID(userID)
	issue, err := db.New(pool).GetIssue(ctx, issueUUID)
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool.Exec(cleanupCtx, `DELETE FROM issue_dispatch_request WHERE issue_id = $1`, issueUUID)
		pool.Exec(cleanupCtx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueUUID)
	})

	suffix := strings.ReplaceAll(issueID, "-", "")
	sequence := pgx.Identifier{"public", "r01_dispatch_gate_" + suffix}.Sanitize()
	function := pgx.Identifier{"public", "r01_dispatch_gate_fn_" + suffix}.Sanitize()
	trigger := pgx.Identifier{"r01_dispatch_gate_trigger_" + suffix}.Sanitize()
	if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE SEQUENCE %s START WITH 1", sequence)); err != nil {
		t.Fatalf("create dispatch barrier sequence: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool.Exec(cleanupCtx, fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON agent_task_queue", trigger))
		pool.Exec(cleanupCtx, fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", function))
		pool.Exec(cleanupCtx, fmt.Sprintf("DROP SEQUENCE IF EXISTS %s", sequence))
	})
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.issue_id::text = TG_ARGV[0] AND NEW.dispatch_issue_revision IS NOT NULL THEN
				PERFORM nextval(TG_ARGV[1]::regclass);
				PERFORM pg_advisory_xact_lock(hashtextextended(TG_ARGV[0], 0));
			END IF;
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER %s BEFORE INSERT ON agent_task_queue
		FOR EACH ROW EXECUTE FUNCTION %s('%s', '%s');`,
		function, trigger, function, issueID, sequence)); err != nil {
		t.Fatalf("install dispatch insert barrier: %v", err)
	}
	barrierConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire dispatch barrier connection: %v", err)
	}
	defer barrierConn.Release()
	if _, err := barrierConn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, issueID); err != nil {
		t.Fatalf("hold dispatch barrier: %v", err)
	}
	barrierHeld := true
	defer func() {
		if !barrierHeld {
			return
		}
		if _, err := barrierConn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, issueID); err != nil {
			t.Errorf("release dispatch barrier: %v", err)
		}
	}()

	svc := &TaskService{Queries: db.New(pool), TxStarter: pool, Bus: events.New()}
	type result struct {
		task   db.AgentTaskQueue
		reused bool
		source string
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		task, reused, err := svc.EnqueueTaskForIssueRunTrigger(ctx, issue, "", actorUUID)
		results <- result{task: task, reused: reused, source: "automatic", err: err}
	}()
	go func() {
		defer wg.Done()
		<-start
		task, reused, err := svc.DispatchIssueWithIdempotencyKey(
			ctx, workspaceUUID, issueUUID, "automatic-dispatch-race", expectedRevision,
			"member", actorUUID, actorUUID, func(db.Agent) bool { return true },
		)
		results <- result{task: task, reused: reused, source: "explicit", err: err}
	}()
	close(start)

	deadline := time.Now().Add(8 * time.Second)
	for {
		var lastValue int64
		var called bool
		if err := pool.QueryRow(ctx, fmt.Sprintf("SELECT last_value, is_called FROM %s", sequence)).Scan(&lastValue, &called); err != nil {
			t.Fatalf("observe requests at insert barrier: %v", err)
		}
		if called && lastValue >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d dispatch request(s) reached the database insert barrier; want both", lastValue)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := barrierConn.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, issueID); err != nil {
		t.Fatalf("release dispatch barrier: %v", err)
	}
	barrierHeld = false

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("automatic/explicit dispatch requests did not finish: %v", ctx.Err())
	}
	close(results)

	var firstID pgtype.UUID
	created, reusedCount := 0, 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("%s dispatch: %v", result.source, result.err)
		}
		if !firstID.Valid {
			firstID = result.task.ID
		} else if result.task.ID != firstID {
			t.Fatalf("%s dispatch returned Run %s; prior request returned %s", result.source,
				util.UUIDToString(result.task.ID), util.UUIDToString(firstID))
		}
		if result.reused {
			reusedCount++
		} else {
			created++
		}
	}
	if created != 1 || reusedCount != 1 {
		t.Fatalf("created=%d reused=%d, want one created Run and one reuse", created, reusedCount)
	}

	var taskCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND dispatch_issue_revision = $3`,
		issueUUID, util.MustParseUUID(agentID), expectedRevision).Scan(&taskCount); err != nil {
		t.Fatalf("count revision-bound runs: %v", err)
	}
	if taskCount != 1 {
		t.Fatalf("revision-bound Run count = %d, want exactly one", taskCount)
	}
}

func TestDispatchIssueRejectsEarlierActiveTaskEvenWhenIssueRemainsTodo(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE issue SET status = 'todo' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("set issue to todo: %v", err)
	}
	expectedRevision := dispatchTestIssueRevision(t, ctx, pool, issueID)
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM issue_dispatch_request WHERE issue_id = $1`, issueID)
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})

	issue := db.Issue{
		ID:           util.MustParseUUID(issueID),
		Revision:     expectedRevision,
		AssigneeID:   util.MustParseUUID(agentID),
		Priority:     "medium",
		CreatorType:  "member",
		CreatorID:    util.MustParseUUID(userID),
		WorkspaceID:  util.MustParseUUID(workspaceID),
		AssigneeType: pgtype.Text{String: "agent", Valid: true},
	}
	svc := &TaskService{Queries: db.New(pool), TxStarter: pool, Bus: events.New()}
	activeTask, err := svc.EnqueueTaskForIssue(ctx, issue)
	if err != nil {
		t.Fatalf("enqueue active fixture task: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, activeTask.ID); err != nil {
		t.Fatalf("mark fixture task running: %v", err)
	}

	// 已有 Run 属于上一任务周期；新的 revision 仍不得绕过未停止的执行。
	if _, err := pool.Exec(ctx, `UPDATE issue SET revision = revision + 1 WHERE id = $1`, issueID); err != nil {
		t.Fatalf("advance issue revision: %v", err)
	}
	expectedRevision = dispatchTestIssueRevision(t, ctx, pool, issueID)

	_, _, err = svc.DispatchIssueWithIdempotencyKey(
		ctx, issue.WorkspaceID, issue.ID, "dispatch-while-running", expectedRevision, "member", issue.CreatorID, issue.CreatorID, func(db.Agent) bool { return true },
	)
	if !errors.Is(err, ErrIssueDispatchActiveTask) {
		t.Fatalf("dispatch todo issue with running task: err = %v, want ErrIssueDispatchActiveTask", err)
	}
	var requests, tasks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issue_dispatch_request WHERE workspace_id = $1 AND idempotency_key = $2`, issue.WorkspaceID, "dispatch-while-running").Scan(&requests); err != nil {
		t.Fatalf("count active-task dispatch reservation: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issue.ID).Scan(&tasks); err != nil {
		t.Fatalf("count tasks after active-task rejection: %v", err)
	}
	if requests != 0 || tasks != 1 {
		t.Fatalf("active-task rejection left reservations=%d tasks=%d, want 0 and 1", requests, tasks)
	}
}

func TestDispatchIssueRequiresTodoAndRollsBackReservation(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, _, issueID := seedAttributionFixture(t, pool)
	expectedRevision := dispatchTestIssueRevision(t, ctx, pool, issueID)
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM issue_dispatch_request WHERE issue_id = $1`, issueID)
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})

	svc := &TaskService{Queries: db.New(pool), TxStarter: pool, Bus: events.New()}
	_, _, err := svc.DispatchIssueWithIdempotencyKey(
		ctx,
		util.MustParseUUID(workspaceID),
		util.MustParseUUID(issueID),
		"dispatch-backlog-issue",
		expectedRevision,
		"member",
		util.MustParseUUID(userID),
		util.MustParseUUID(userID),
		func(db.Agent) bool { return true },
	)
	if !errors.Is(err, ErrIssueDispatchNotTodo) {
		t.Fatalf("dispatch backlog issue: err = %v, want ErrIssueDispatchNotTodo", err)
	}

	var requests, tasks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issue_dispatch_request WHERE workspace_id = $1 AND idempotency_key = $2`, workspaceID, "dispatch-backlog-issue").Scan(&requests); err != nil {
		t.Fatalf("count issue dispatch reservations: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&tasks); err != nil {
		t.Fatalf("count queued tasks: %v", err)
	}
	if requests != 0 || tasks != 0 {
		t.Fatalf("rejected dispatch left reservations=%d tasks=%d, want both zero", requests, tasks)
	}
}

func TestDispatchIssueRejectsStaleIssueRevisionAndRollsBackReservation(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, _, issueID := seedAttributionFixture(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE issue SET status = 'todo' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("set issue to todo: %v", err)
	}
	var plannedRevision int64
	if err := pool.QueryRow(ctx, `SELECT revision FROM issue WHERE id = $1`, issueID).Scan(&plannedRevision); err != nil {
		t.Fatalf("read planned issue revision: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE issue SET revision = revision + 1 WHERE id = $1`, issueID); err != nil {
		t.Fatalf("change issue revision after planning: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM issue_dispatch_request WHERE issue_id = $1`, issueID)
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})

	svc := &TaskService{Queries: db.New(pool), TxStarter: pool, Bus: events.New()}
	_, _, err := svc.DispatchIssueWithIdempotencyKey(
		ctx,
		util.MustParseUUID(workspaceID),
		util.MustParseUUID(issueID),
		"dispatch-stale-plan",
		plannedRevision,
		"member",
		util.MustParseUUID(userID),
		util.MustParseUUID(userID),
		func(db.Agent) bool { return true },
	)
	var conflict *IssueDispatchRevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("dispatch with stale revision: err = %v, want IssueDispatchRevisionConflictError", err)
	}
	if conflict.Expected != plannedRevision || conflict.Actual != plannedRevision+1 {
		t.Fatalf("revision conflict = (%d, %d), want (%d, %d)", conflict.Expected, conflict.Actual, plannedRevision, plannedRevision+1)
	}
	var requests, tasks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issue_dispatch_request WHERE workspace_id = $1 AND idempotency_key = $2`, workspaceID, "dispatch-stale-plan").Scan(&requests); err != nil {
		t.Fatalf("count stale dispatch reservations: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&tasks); err != nil {
		t.Fatalf("count tasks after stale dispatch: %v", err)
	}
	if requests != 0 || tasks != 0 {
		t.Fatalf("stale dispatch left reservations=%d tasks=%d, want both zero", requests, tasks)
	}
}

func dispatchTestIssueRevision(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issueID string) int64 {
	t.Helper()
	var revision int64
	if err := pool.QueryRow(ctx, `SELECT revision FROM issue WHERE id = $1`, issueID).Scan(&revision); err != nil {
		t.Fatalf("read issue revision: %v", err)
	}
	return revision
}

func TestCreateAgentTaskDispatchRevisionConflictKeepsTransactionUsable(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	_, userID, agentID, issueID := seedAttributionFixture(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE issue SET status = 'todo' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("set issue to todo: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM issue_dispatch_request WHERE issue_id = $1`, issueID)
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})

	issueIDValue := util.MustParseUUID(issueID)
	issue, err := db.New(pool).GetIssue(ctx, issueIDValue)
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	revision := pgtype.Int8{Int64: issue.Revision, Valid: true}
	service := &TaskService{Queries: db.New(pool), TxStarter: pool, Bus: events.New()}
	automatic, reused, err := service.EnqueueTaskForIssueRunTrigger(ctx, issue, "", util.MustParseUUID(userID))
	if err != nil {
		t.Fatalf("enqueue automatic run: %v", err)
	}
	if reused {
		t.Fatal("new automatic run was marked reused")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin dispatch transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	queries := db.New(tx)
	agent, err := queries.GetAgent(ctx, util.MustParseUUID(agentID))
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}
	_, err = queries.CreateAgentTask(ctx, db.CreateAgentTaskParams{
		ID:                    dbid.NewV7(),
		AgentID:               agent.ID,
		RuntimeID:             agent.RuntimeID,
		IssueID:               issue.ID,
		Priority:              1,
		DispatchIssueRevision: revision,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("duplicate revision insert error = %v, want pgx.ErrNoRows from DO NOTHING", err)
	}
	existing, err := queries.GetIssueTaskForDispatchRevision(ctx, db.GetIssueTaskForDispatchRevisionParams{
		IssueID: issue.ID, AgentID: agent.ID, DispatchIssueRevision: revision,
	})
	if err != nil {
		t.Fatalf("load existing run after duplicate insert: %v", err)
	}
	if existing.ID != automatic.ID {
		t.Fatalf("existing run id = %s, want %s", util.UUIDToString(existing.ID), util.UUIDToString(automatic.ID))
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit dispatch transaction after duplicate insert: %v", err)
	}
}
