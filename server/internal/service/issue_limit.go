package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// IssueLimitReachedError is retained for transport adapters that still map a
// create-time capacity rejection. Upstream removed the issue count limit, so
// number allocation no longer raises it; the type stays for API compatibility.
type IssueLimitReachedError struct {
	Limit          int64
	PolicyRevision int64
}

func (e *IssueLimitReachedError) Error() string {
	return fmt.Sprintf("workspace issue limit reached: limit %d", e.Limit)
}

// AllocateIssueNumber serializes creates on the workspace row — implicitly via
// IncrementIssueCounter for the workspace sequence, explicitly via
// LockWorkspaceForIssueCreate for project-numbered creates, so
// issueposition.NextTopPosition keeps its create-vs-create guarantee in both
// branches. A non-nil project with a code numbers the issue from that project's
// own counter and returns its code as the identifier's middle segment; the
// returned code is empty for the workspace sequence.
func AllocateIssueNumber(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, project *db.Project) (int32, string, error) {
	if project != nil && project.Code != "" {
		if _, err := q.LockWorkspaceForIssueCreate(ctx, workspaceID); err != nil {
			return 0, "", err
		}
		number, err := q.IncrementProjectIssueCounter(ctx, project.ID)
		if err != nil {
			return 0, "", err
		}
		return number, project.Code, nil
	}
	number, err := q.IncrementIssueCounter(ctx, workspaceID)
	if err != nil {
		return 0, "", err
	}
	return number, "", nil
}
