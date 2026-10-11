package service

import (
	"context"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/entitlement"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// IssueLimitReachedError carries Cloud's effective limit and policy revision
// to transport adapters. It never includes a plan name or subscription state.
type IssueLimitReachedError struct {
	Limit          int64
	PolicyRevision int64
}

func (e *IssueLimitReachedError) Error() string {
	return fmt.Sprintf("workspace issue limit reached: limit %d", e.Limit)
}

// IssueCountPolicy is the validated mechanical instruction received from
// Cloud. Resolve it before opening a database transaction so a network call
// never holds product-database locks.
type IssueCountPolicy struct {
	Action         entitlement.Action
	Limit          int64
	PolicyRevision int64
}

func ResolveIssueCountPolicy(ctx context.Context, provider entitlement.Provider, workspaceID pgtype.UUID) IssueCountPolicy {
	if provider == nil || !workspaceID.Valid {
		return IssueCountPolicy{Action: entitlement.ActionOff}
	}
	decision := provider.Gate(ctx, uuid.UUID(workspaceID.Bytes), entitlement.GateIssueCount)
	policy := IssueCountPolicy{
		Action:         decision.Gate.Action,
		PolicyRevision: decision.PolicyRevision,
	}
	if decision.Gate.Limit != nil {
		policy.Limit = int64(*decision.Gate.Limit)
	}
	switch policy.Action {
	case entitlement.ActionOff:
		policy.Limit = 0
	case entitlement.ActionEnforce:
		if policy.Limit > 0 {
			return policy
		}
		policy.Action = entitlement.ActionOff
		policy.Limit = 0
	default:
		policy.Action = entitlement.ActionOff
		policy.Limit = 0
	}
	return policy
}

// CheckIssueCreateCapacity performs a read-only admission preflight for flows
// that do work before the issue transaction starts, such as agent quick create.
// It is deliberately not a reservation: callers must still use
// AllocateIssueNumber in the final create transaction to serialize concurrent
// admissions against the workspace row.
func CheckIssueCreateCapacity(ctx context.Context, q *db.Queries, provider entitlement.Provider, workspaceID pgtype.UUID) error {
	policy := ResolveIssueCountPolicy(ctx, provider, workspaceID)
	if policy.Action != entitlement.ActionEnforce {
		return nil
	}
	used, err := CountIssueUsage(ctx, q, workspaceID, policy)
	if err != nil {
		return err
	}
	if used >= policy.Limit {
		return &IssueLimitReachedError{Limit: policy.Limit, PolicyRevision: policy.PolicyRevision}
	}
	return nil
}

// AllocateIssueNumber serializes creates on the workspace row — implicitly via
// IncrementIssueCounter for the workspace sequence, explicitly via
// LockWorkspaceForIssueCreate for project-numbered creates, so
// issueposition.NextTopPosition keeps its create-vs-create guarantee in both
// branches — then checks the current number of issue rows inside the same
// transaction. A non-nil project with a code numbers the issue from that
// project's own counter and returns its code as the identifier's middle
// segment; the returned code is empty for the workspace sequence. The caller
// must roll the transaction back on IssueLimitReachedError; that also rolls
// back the counter increment. Deleting an issue frees capacity because
// issue_counter is deliberately not used as quota usage.
func AllocateIssueNumber(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, project *db.Project, policy IssueCountPolicy) (int32, string, error) {
	if project != nil && project.Code != "" {
		if _, err := q.LockWorkspaceForIssueCreate(ctx, workspaceID); err != nil {
			return 0, "", err
		}
		number, err := q.IncrementProjectIssueCounter(ctx, project.ID)
		if err != nil {
			return 0, "", err
		}
		if err := checkIssueQuota(ctx, q, workspaceID, policy); err != nil {
			return 0, "", err
		}
		return number, project.Code, nil
	}
	number, err := q.IncrementIssueCounter(ctx, workspaceID)
	if err != nil {
		return 0, "", err
	}
	if err := checkIssueQuota(ctx, q, workspaceID, policy); err != nil {
		return 0, "", err
	}
	return number, "", nil
}

func checkIssueQuota(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, policy IssueCountPolicy) error {
	if policy.Action != entitlement.ActionEnforce {
		return nil
	}
	used, err := q.CountIssuesUpTo(ctx, db.CountIssuesUpToParams{
		WorkspaceID: workspaceID,
		Limit:       policy.Limit,
	})
	if err != nil {
		return err
	}
	if used >= policy.Limit {
		return &IssueLimitReachedError{Limit: policy.Limit, PolicyRevision: policy.PolicyRevision}
	}
	return nil
}

// CountIssueUsage performs a bounded read for a limited policy. The returned
// value is exact while below the limit and capped at the limit once the
// workspace is full.
func CountIssueUsage(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, policy IssueCountPolicy) (int64, error) {
	if policy.Action != entitlement.ActionEnforce {
		return 0, nil
	}
	sampleLimit := policy.Limit
	if sampleLimit < math.MaxInt64 {
		sampleLimit++
	}
	sampled, err := q.CountIssuesUpTo(ctx, db.CountIssuesUpToParams{
		WorkspaceID: workspaceID,
		Limit:       sampleLimit,
	})
	if err != nil {
		return 0, err
	}
	if sampled > policy.Limit {
		return policy.Limit, nil
	}
	return sampled, nil
}
