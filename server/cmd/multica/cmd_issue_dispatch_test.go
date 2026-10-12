package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
)

func TestRunIssueDispatchSendsExpectedRevision(t *testing.T) {
	const expectedRevision int64 = 19
	var gotRevision int64
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/issues/"+testIssueUUID+"/dispatch" {
			t.Errorf("request = %s %s, want POST issue dispatch", r.Method, r.URL.Path)
		}
		gotKey = r.Header.Get("Idempotency-Key")
		var body struct {
			ExpectedRevision int64 `json:"expected_revision"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode dispatch body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotRevision = body.ExpectedRevision
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"task-dispatched"}`))
	}))
	defer srv.Close()
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "workspace-test")
	t.Setenv("MULTICA_TOKEN", "test-token")

	cmd := &cobra.Command{Use: "dispatch"}
	cmd.Flags().String("idempotency-key", "", "")
	cmd.Flags().Int64("expected-revision", 0, "")
	cmd.Flags().String("output", "json", "")
	_ = cmd.Flags().Set("idempotency-key", "dispatch-key-1")
	_ = cmd.Flags().Set("expected-revision", "19")

	if _, err := captureStdout(t, func() error {
		return runIssueDispatch(cmd, []string{testIssueUUID})
	}); err != nil {
		t.Fatalf("runIssueDispatch: %v", err)
	}
	if gotKey != "dispatch-key-1" {
		t.Fatalf("Idempotency-Key = %q, want dispatch-key-1", gotKey)
	}
	if gotRevision != expectedRevision {
		t.Fatalf("expected_revision = %d, want %d", gotRevision, expectedRevision)
	}
}

func TestRunIssueDispatchRequiresPositiveExpectedRevisionBeforeRequest(t *testing.T) {
	cmd := &cobra.Command{Use: "dispatch"}
	cmd.Flags().String("idempotency-key", "", "")
	cmd.Flags().Int64("expected-revision", 0, "")
	cmd.Flags().String("output", "json", "")
	_ = cmd.Flags().Set("idempotency-key", "dispatch-key-1")

	if err := runIssueDispatch(cmd, []string{testIssueUUID}); err == nil {
		t.Fatal("runIssueDispatch without expected revision returned nil")
	}
}
