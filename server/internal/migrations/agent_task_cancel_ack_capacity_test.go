package migrations

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAgentTaskCancelAckCapacityDownRefusesLiveWork(t *testing.T) {
	// This migration test deliberately ignores DATABASE_URL. The separate
	// variable makes it impossible to fall back to the local Multica database.
	dbURL := os.Getenv("MULTICA_MIGRATION_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("integration test requires an isolated database in MULTICA_MIGRATION_TEST_DATABASE_URL")
	}
	config, err := pgx.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse isolated Postgres URL: %v", err)
	}
	if config.Database == "multica" {
		t.Fatal("refusing to use the default multica database; set MULTICA_MIGRATION_TEST_DATABASE_URL to an isolated database")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to isolated Postgres: %v", err)
	}
	defer pool.Close()

	schema := "cancel_ack_down_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatalf("create isolated migration schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+quotedSchema+" CASCADE")
	})

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire isolated Postgres connection: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT set_config('search_path', $1, false)`, schema); err != nil {
		t.Fatalf("set isolated migration search path: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		CREATE TABLE agent_task_queue (
			id UUID PRIMARY KEY,
			status TEXT NOT NULL
		);
	`); err != nil {
		t.Fatalf("create task queue fixture: %v", err)
	}
	applyMigrationFile(t, ctx, conn.Conn(), "552_agent_task_cancel_ack_capacity.up.sql")

	taskID := uuid.NewString()
	if _, err := conn.Exec(ctx, `INSERT INTO agent_task_queue (id, status) VALUES ($1, 'running')`, taskID); err != nil {
		t.Fatalf("insert running task fixture: %v", err)
	}
	expectCancelAckCapacityDownRefusal(t, ctx, conn.Conn())
	assertCancelAckColumnExists(t, ctx, conn.Conn())

	if _, err := conn.Exec(ctx, `UPDATE agent_task_queue SET status = 'cancelled' WHERE id = $1`, taskID); err != nil {
		t.Fatalf("cancel running task fixture: %v", err)
	}
	var ackPending bool
	if err := conn.QueryRow(ctx, `SELECT cancel_ack_pending FROM agent_task_queue WHERE id = $1`, taskID).Scan(&ackPending); err != nil {
		t.Fatalf("read cancellation acknowledgement state: %v", err)
	}
	if !ackPending {
		t.Fatal("running-to-cancelled transition did not require an acknowledgement")
	}
	expectCancelAckCapacityDownRefusal(t, ctx, conn.Conn())
	assertCancelAckColumnExists(t, ctx, conn.Conn())

	if _, err := conn.Exec(ctx, `UPDATE agent_task_queue SET cancel_ack_pending = FALSE WHERE id = $1`, taskID); err != nil {
		t.Fatalf("acknowledge stopped task fixture: %v", err)
	}
	applyMigrationFile(t, ctx, conn.Conn(), "552_agent_task_cancel_ack_capacity.down.sql")
	var columnExists bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = 'agent_task_queue'
			  AND column_name = 'cancel_ack_pending'
		)
	`).Scan(&columnExists); err != nil {
		t.Fatalf("check removed acknowledgement column: %v", err)
	}
	if columnExists {
		t.Fatal("down migration kept cancel_ack_pending after work drained")
	}
}

func expectCancelAckCapacityDownRefusal(t *testing.T, ctx context.Context, conn interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}) {
	t.Helper()
	_, err := conn.Exec(ctx, readMigrationFile(t, "552_agent_task_cancel_ack_capacity.down.sql"))
	if err == nil || !strings.Contains(err.Error(), "cannot roll back agent task cancellation acknowledgements") {
		t.Fatalf("down migration error = %v, want refusal while live work remains", err)
	}
}

func assertCancelAckColumnExists(t *testing.T, ctx context.Context, conn interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) {
	t.Helper()
	var exists bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = 'agent_task_queue'
			  AND column_name = 'cancel_ack_pending'
		)
	`).Scan(&exists); err != nil {
		t.Fatalf("check preserved acknowledgement column: %v", err)
	}
	if !exists {
		t.Fatal("down migration removed cancel_ack_pending despite refusing rollback")
	}
}
