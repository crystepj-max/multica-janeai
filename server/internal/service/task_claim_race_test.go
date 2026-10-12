package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func newTaskClaimRacePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return sharedTestPool(t)
}

func TestClaimTaskConcurrentCapacityRespected(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)

	agentID := createClaimCapacityFixture(t, ctx, pool)
	agentUUID := util.MustParseUUID(agentID)

	triggerName := fmt.Sprintf("claim_capacity_sleep_%d", time.Now().UnixNano())
	functionName := triggerName + "_fn"
	createSleepTrigger(t, ctx, pool, triggerName, functionName, agentID)
	svc := NewTaskService(queries, pool, nil, events.New())

	const workers = 2
	start := make(chan struct{})
	claimed := make(chan string, workers)
	errs := make(chan error, workers)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			task, err := svc.ClaimTask(ctx, agentUUID)
			if err != nil {
				errs <- err
				return
			}
			if task != nil {
				claimed <- util.UUIDToString(task.ID)
			}
		}()
	}

	close(start)
	wg.Wait()
	close(claimed)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("claim task: %v", err)
		}
	}

	var claimedIDs []string
	for id := range claimed {
		claimedIDs = append(claimedIDs, id)
	}
	if len(claimedIDs) != 1 {
		t.Fatalf("expected exactly 1 claimed task with max_concurrent_tasks=1, got %d (%v)", len(claimedIDs), claimedIDs)
	}

	var active int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE agent_id = $1 AND status IN ('dispatched', 'running', 'waiting_local_directory')
	`, agentID).Scan(&active); err != nil {
		t.Fatalf("count active tasks: %v", err)
	}
	if active != 1 {
		t.Fatalf("expected 1 active task after concurrent claims, got %d", active)
	}
}

func TestClaimTaskWaitsForCancelledRunAckAfterRuntimeRebind(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)
	agentID := createClaimCapacityFixture(t, ctx, pool)
	agentUUID := util.MustParseUUID(agentID)
	svc := NewTaskService(queries, pool, nil, events.New())

	var oldRuntimeID, workspaceID, ownerID string
	if err := pool.QueryRow(ctx, `
		SELECT runtime_id, workspace_id, owner_id
		FROM agent WHERE id = $1
	`, agentID).Scan(&oldRuntimeID, &workspaceID, &ownerID); err != nil {
		t.Fatalf("load claim fixture agent: %v", err)
	}
	oldDaemonID := fmt.Sprintf("claim-old-daemon-%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `UPDATE agent_runtime SET daemon_id = $2 WHERE id = $1`, oldRuntimeID, oldDaemonID); err != nil {
		t.Fatalf("bind old runtime daemon identity: %v", err)
	}

	var newRuntimeID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (
			workspace_id, daemon_id, name, runtime_mode, provider,
			status, device_info, metadata, last_seen_at, visibility, owner_id
		)
		VALUES ($1, $2, 'Claim Handoff Runtime', 'cloud', 'claim_capacity_test',
			'online', 'test runtime', '{}'::jsonb, now(), 'private', $3)
		RETURNING id
	`, workspaceID, "claim-new-daemon", ownerID).Scan(&newRuntimeID); err != nil {
		t.Fatalf("create replacement runtime: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool.Exec(cleanupCtx, `UPDATE agent SET runtime_id = $2 WHERE id = $1`, agentID, oldRuntimeID)
		pool.Exec(cleanupCtx, `DELETE FROM agent_task_queue WHERE agent_id = $1`, agentID)
		pool.Exec(cleanupCtx, `DELETE FROM agent_runtime WHERE id = $1`, newRuntimeID)
	})

	rows, err := pool.Query(ctx, `
		SELECT id FROM agent_task_queue WHERE agent_id = $1 ORDER BY created_at, id LIMIT 2
	`, agentID)
	if err != nil {
		t.Fatalf("list claim fixture tasks: %v", err)
	}
	var taskIDs []string
	for rows.Next() {
		var taskID string
		if err := rows.Scan(&taskID); err != nil {
			rows.Close()
			t.Fatalf("scan claim fixture task: %v", err)
		}
		taskIDs = append(taskIDs, taskID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("read claim fixture tasks: %v", err)
	}
	rows.Close()
	if len(taskIDs) != 2 {
		t.Fatalf("claim fixture tasks = %d, want 2", len(taskIDs))
	}

	if _, err := pool.Exec(ctx, `
		UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1
	`, taskIDs[0]); err != nil {
		t.Fatalf("mark old-runtime task running: %v", err)
	}
	if _, err := svc.CancelTask(ctx, util.MustParseUUID(taskIDs[0])); err != nil {
		t.Fatalf("cancel old-runtime task: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE agent SET runtime_id = $2 WHERE id = $1
	`, agentID, newRuntimeID); err != nil {
		t.Fatalf("rebind agent to replacement runtime: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE agent_task_queue SET runtime_id = $2 WHERE id = $1
	`, taskIDs[1], newRuntimeID); err != nil {
		t.Fatalf("move queued task to replacement runtime: %v", err)
	}

	claimed, err := svc.ClaimTask(ctx, agentUUID)
	if err != nil {
		t.Fatalf("claim before cancellation ack: %v", err)
	}
	if claimed != nil {
		t.Fatalf("claimed task %s while the old runtime cancellation was unacknowledged", util.UUIDToString(claimed.ID))
	}

	rowsAffected, err := queries.AckAgentTaskCancellation(ctx, db.AckAgentTaskCancellationParams{
		TaskID:   util.MustParseUUID(taskIDs[0]),
		DaemonID: pgtype.Text{String: "different-daemon", Valid: true},
	})
	if err != nil {
		t.Fatalf("ack from a different daemon: %v", err)
	}
	if rowsAffected != 0 {
		t.Fatalf("wrong-daemon ack released %d cancellation reservation(s), want 0", rowsAffected)
	}
	claimed, err = svc.ClaimTask(ctx, agentUUID)
	if err != nil {
		t.Fatalf("claim after rejected ack: %v", err)
	}
	if claimed != nil {
		t.Fatalf("claimed task %s after a wrong-daemon ack", util.UUIDToString(claimed.ID))
	}

	rowsAffected, err = queries.AckAgentTaskCancellation(ctx, db.AckAgentTaskCancellationParams{
		TaskID:   util.MustParseUUID(taskIDs[0]),
		DaemonID: pgtype.Text{String: oldDaemonID, Valid: true},
	})
	if err != nil {
		t.Fatalf("ack from the old runtime daemon: %v", err)
	}
	if rowsAffected != 1 {
		t.Fatalf("old-daemon ack released %d cancellation reservation(s), want 1", rowsAffected)
	}
	claimed, err = svc.ClaimTask(ctx, agentUUID)
	if err != nil {
		t.Fatalf("claim after old-runtime ack: %v", err)
	}
	if claimed == nil || util.UUIDToString(claimed.ID) != taskIDs[1] {
		t.Fatalf("claim after ack = %v, want queued task %s", claimed, taskIDs[1])
	}
}

func createSleepTrigger(t *testing.T, ctx context.Context, pool *pgxpool.Pool, triggerName, functionName, agentID string) {
	t.Helper()

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s()
		RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		BEGIN
			PERFORM pg_sleep(0.1);
			RETURN NEW;
		END;
		$$;
	`, quoteIdent(functionName))); err != nil {
		t.Fatalf("create sleep trigger function: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE UPDATE OF status ON agent_task_queue
		FOR EACH ROW
		WHEN (OLD.status = 'queued' AND NEW.status = 'dispatched' AND NEW.agent_id = %s::uuid)
		EXECUTE FUNCTION %s();
	`, quoteIdent(triggerName), quoteLiteral(agentID), quoteIdent(functionName))); err != nil {
		t.Fatalf("create sleep trigger: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool.Exec(cleanupCtx, fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON agent_task_queue", quoteIdent(triggerName)))
		pool.Exec(cleanupCtx, fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", quoteIdent(functionName)))
	})
}

func createClaimCapacityFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()

	suffix := time.Now().UnixNano()
	email := fmt.Sprintf("claim-capacity-%d@multica.ai", suffix)
	slug := fmt.Sprintf("claim-capacity-%d", suffix)

	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO "user" (name, email)
		VALUES ($1, $2)
		RETURNING id
	`, "Claim Capacity Test", email).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}

	var workspaceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, "Claim Capacity Test", slug, "temporary task claim race test workspace", "CCR").Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role)
		VALUES ($1, $2, 'owner')
	`, workspaceID, userID); err != nil {
		t.Fatalf("create member: %v", err)
	}

	var runtimeID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (
			workspace_id, daemon_id, name, runtime_mode, provider,
			status, device_info, metadata, last_seen_at, visibility, owner_id
		)
		VALUES ($1, NULL, $2, 'cloud', 'claim_capacity_test', 'online', 'test runtime', '{}'::jsonb, now(), 'private', $3)
		RETURNING id
	`, workspaceID, "Claim Capacity Runtime", userID).Scan(&runtimeID); err != nil {
		t.Fatalf("create runtime: %v", err)
	}

	var agentID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (
			workspace_id, name, description, runtime_mode, runtime_config,
			runtime_id, visibility, max_concurrent_tasks, owner_id
		)
		VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'private', 1, $4)
		RETURNING id
	`, workspaceID, "Claim Capacity Agent", runtimeID, userID).Scan(&agentID); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool.Exec(cleanupCtx, `DELETE FROM agent_task_queue WHERE agent_id = $1`, agentID)
		pool.Exec(cleanupCtx, `DELETE FROM issue WHERE workspace_id = $1`, workspaceID)
		pool.Exec(cleanupCtx, `DELETE FROM agent WHERE id = $1`, agentID)
		pool.Exec(cleanupCtx, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
		pool.Exec(cleanupCtx, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, workspaceID, userID)
		pool.Exec(cleanupCtx, `DELETE FROM workspace WHERE id = $1`, workspaceID)
		pool.Exec(cleanupCtx, `DELETE FROM "user" WHERE id = $1`, userID)
	})

	for i := 0; i < 2; i++ {
		var issueID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position)
			VALUES ($1, $2, 'in_progress', 'none', $3, 'member', $4, $5)
			RETURNING id
		`, workspaceID, fmt.Sprintf("claim capacity issue %d", i+1), userID, 900000+i, i).Scan(&issueID); err != nil {
			t.Fatalf("create issue %d: %v", i+1, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO agent_task_queue (agent_id, issue_id, status, priority, context, runtime_id)
			VALUES ($1, $2, 'queued', 0, '{}'::jsonb, $3)
		`, agentID, issueID, runtimeID); err != nil {
			t.Fatalf("create task %d: %v", i+1, err)
		}
	}

	return agentID
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}
