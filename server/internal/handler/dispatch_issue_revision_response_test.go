package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestListTasksByIssueReturnsDispatchIssueRevision(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "DispatchRevisionListAgent", []byte("[]"))

	var issueID string
	issueNumber := int32(time.Now().UnixNano() % 1_000_000_000)
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position)
		VALUES ($1, 'dispatch-revision-list-issue', 'todo', 'medium', $2, 'member', $3, 0)
		RETURNING id
	`, testWorkspaceID, testUserID, issueNumber).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID) })

	const wantRevision int64 = 42
	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, issue_id, dispatch_issue_revision)
		VALUES ($1, (SELECT runtime_id FROM agent WHERE id = $1), 'completed', 0, $2, $3)
		RETURNING id
	`, agentID, issueID, wantRevision).Scan(&taskID); err != nil {
		t.Fatalf("create task run: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })

	req := newRequest("GET", "/api/issues/"+issueID+"/task-runs", nil)
	req = withURLParam(req, "id", issueID)
	w := httptest.NewRecorder()
	testHandler.ListTasksByIssue(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var runs []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode task list: %v", err)
	}
	for _, run := range runs {
		if run["id"] == taskID {
			if run["dispatch_issue_revision"] != float64(wantRevision) {
				t.Fatalf("dispatch_issue_revision = %#v, want %d", run["dispatch_issue_revision"], wantRevision)
			}
			return
		}
	}
	t.Fatalf("task run %s missing from response: %s", taskID, w.Body.String())
}
