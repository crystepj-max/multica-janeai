package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/dispatch"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// pullRequestReviewDispatchMarker opts one autopilot into per-pull-request
// dispatch. Other autopilots keep the 60-second title duplicate guard.
// The marker has to stay in the autopilot description; it is not a title match.
const pullRequestReviewDispatchMarker = "[pr-review-dispatch:per-pr]"

// pullRequestReviewClaimLease covers the gap after a follow-up commits its
// claim and before its task row is visible. It is not the dedup identity.
// A crash in that gap expires the claim so the next pull_request event can
// start, and events that arrived during the gap stay on pending_sha.
const pullRequestReviewClaimLease = 2 * time.Minute

const (
	metaReviewRepo      = "review_repo"
	metaReviewPR        = "review_pr"
	metaInflightSHA     = "inflight_sha"
	metaInflightAt      = "inflight_at"
	metaPendingSHA      = "pending_sha"
	metaPendingAt       = "pending_at"
	metaLastReviewedSHA = "last_reviewed_sha"
	metaReviewClaimAt   = "review_claim_at"
)

// pullRequestReviewSignal is one code-changing GitHub pull_request delivery.
type pullRequestReviewSignal struct {
	Repo   string
	Number string
	SHA    string
	Action string
	At     time.Time
}

type prAdmissionAction string

const (
	prAdmitCoalesce prAdmissionAction = "coalesce"
	prAdmitReviewed prAdmissionAction = "already_reviewed"
	prAdmitStart    prAdmissionAction = "start"
)

// pullRequestAdmission is the create_issue decision for one webhook run.
// handled means this autopilot's webhook must not fall through to the title
// duplicate guard: a shared daily title would treat a second PR as a copy of
// the first. dirty means metadata was written and the caller must commit
// before returning skip, or the pending sha rolls back with the lock.
type pullRequestAdmission struct {
	handled bool
	dirty   bool
	skip    *errDispatchSkipped
	reuse   *db.Issue
	signal  pullRequestReviewSignal
}

func autopilotUsesPullRequestReviewDispatch(description string) bool {
	return strings.Contains(description, pullRequestReviewDispatchMarker)
}

func (s *AutopilotService) admitPullRequestReview(ctx context.Context, q *db.Queries, ap db.Autopilot, run *db.AutopilotRun) (pullRequestAdmission, error) {
	if run == nil || run.Source != "webhook" || !autopilotUsesPullRequestReviewDispatch(ap.Description.String) {
		return pullRequestAdmission{}, nil
	}
	signal, ok := parseCodeChangingPullRequest(run.TriggerPayload)
	if !ok {
		return pullRequestAdmission{
			handled: true,
			skip: &errDispatchSkipped{
				reason: pullRequestReviewSkipReason(run.TriggerPayload),
				code:   dispatch.ReasonCoalesced,
			},
		}, nil
	}
	if err := q.LockIssueDuplicateKey(ctx, pullRequestReviewLockKey(ap.WorkspaceID, ap.ID, signal.Repo, signal.Number)); err != nil {
		return pullRequestAdmission{}, fmt.Errorf("lock pull request review: %w", err)
	}
	terminal, err := issuestatus.ExpandCategories(ctx, q, ap.WorkspaceID, []string{issuestatus.CategoryDone, issuestatus.CategoryClosed})
	if err != nil {
		return pullRequestAdmission{}, fmt.Errorf("expand terminal statuses: %w", err)
	}
	existing, err := q.FindOpenPullRequestReviewIssue(ctx, db.FindOpenPullRequestReviewIssueParams{
		WorkspaceID:        ap.WorkspaceID,
		OriginID:           ap.ID,
		TerminalStatusKeys: terminal,
		ProjectID:          ap.ProjectID,
		ReviewRepo:         signal.Repo,
		ReviewPr:           signal.Number,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return pullRequestAdmission{handled: true, signal: signal}, nil
	}
	if err != nil {
		return pullRequestAdmission{}, fmt.Errorf("find open pull request review: %w", err)
	}
	inFlight, err := pullRequestReviewInFlight(ctx, q, existing)
	if err != nil {
		return pullRequestAdmission{}, err
	}
	lastReviewed := issueMetadataString(existing.Metadata, metaLastReviewedSHA)
	inflight := loadReviewSHA(existing.Metadata, metaInflightSHA, metaInflightAt)
	pending := loadReviewSHA(existing.Metadata, metaPendingSHA, metaPendingAt)
	incoming := (reviewSHA{sha: signal.SHA, at: signal.At}).normalized()
	action, best := decidePullRequestAdmission(inFlight, lastReviewed, inflight, pending, incoming)
	switch action {
	case prAdmitCoalesce:
		if !shouldRecordCoalescedPending(inflight, pending, incoming, best) {
			return pullRequestAdmission{
				handled: true,
				skip: &errDispatchSkipped{
					reason: "coalesced onto in-flight pull request review issue: " + util.UUIDToString(existing.ID),
					code:   dispatch.ReasonCoalesced,
				},
			}, nil
		}
		if err := setIssueMetadataString(ctx, q, existing, metaPendingSHA, best.sha); err != nil {
			return pullRequestAdmission{}, err
		}
		if err := setOrDeleteIssueMetadataTime(ctx, q, existing, metaPendingAt, best.at); err != nil {
			return pullRequestAdmission{}, err
		}
		return pullRequestAdmission{
			handled: true,
			dirty:   true,
			skip: &errDispatchSkipped{
				reason: "coalesced onto in-flight pull request review issue: " + util.UUIDToString(existing.ID),
				code:   dispatch.ReasonCoalesced,
			},
		}, nil
	case prAdmitReviewed:
		return pullRequestAdmission{
			handled: true,
			skip: &errDispatchSkipped{
				reason: "pull request head already reviewed: " + signal.Repo + "#" + signal.Number + " " + signal.SHA,
				code:   dispatch.ReasonCoalesced,
			},
		}, nil
	default:
		start := signal
		start.SHA = best.sha
		start.At = best.at
		if err := stampPullRequestReviewStart(ctx, q, existing, start); err != nil {
			return pullRequestAdmission{}, err
		}
		return pullRequestAdmission{handled: true, reuse: &existing, signal: start}, nil
	}
}

func pullRequestReviewSkipReason(payload []byte) string {
	action := pullRequestEventAction(payload)
	if action == "" {
		return "webhook does not start a pull request review"
	}
	return "pull request event does not start a review: " + action
}

func pullRequestReviewLockKey(workspaceID, autopilotID pgtype.UUID, repo, number string) string {
	return "autopilot-pr-review|" + util.UUIDToString(workspaceID) + "|" + util.UUIDToString(autopilotID) + "|" + strings.ToLower(strings.TrimSpace(repo)) + "|" + strings.TrimSpace(number)
}

// reviewSHA is one commit held in the review record and the arrival time of
// that delivery. A zero time means the arrival time was not stored. It is not
// the claim lease.
type reviewSHA struct {
	sha string
	at  time.Time
}

func (s reviewSHA) normalized() reviewSHA {
	s.sha = strings.TrimSpace(s.sha)
	if s.sha == "" {
		return reviewSHA{}
	}
	if !s.at.IsZero() {
		s.at = s.at.UTC()
	}
	return s
}

func unreviewedSHA(sha, lastReviewed string, at time.Time) reviewSHA {
	value := (reviewSHA{sha: sha, at: at}).normalized()
	if value.sha == "" || value.sha == strings.TrimSpace(lastReviewed) {
		return reviewSHA{}
	}
	return value
}

// laterReviewSHA keeps the newer unreviewed commit.
// Equal times keep the candidate already stored. A missing time loses to a
// known time. When both times are missing, the stored candidate stays.
// The same commit keeps the later known arrival time.
func laterReviewSHA(kept, next reviewSHA) reviewSHA {
	kept = kept.normalized()
	next = next.normalized()
	if next.sha == "" {
		return kept
	}
	if kept.sha == "" {
		return next
	}
	if kept.sha == next.sha {
		if kept.at.IsZero() || (!next.at.IsZero() && next.at.After(kept.at)) {
			return next
		}
		return kept
	}
	switch {
	case !kept.at.IsZero() && !next.at.IsZero():
		if next.at.After(kept.at) {
			return next
		}
		return kept
	case !kept.at.IsZero():
		return kept
	case !next.at.IsZero():
		return next
	default:
		return kept
	}
}

func loadReviewSHA(metadata []byte, shaKey, atKey string) reviewSHA {
	return (reviewSHA{
		sha: issueMetadataString(metadata, shaKey),
		at:  parseMetadataTime(issueMetadataString(metadata, atKey)),
	}).normalized()
}

// chooseUnreviewedSHA folds inflight, then pending, then incoming.
// An earlier candidate wins when the arrival times are equal.
func chooseUnreviewedSHA(lastReviewed string, inflight, pending, incoming reviewSHA) reviewSHA {
	best := laterReviewSHA(
		unreviewedSHA(inflight.sha, lastReviewed, inflight.at),
		unreviewedSHA(pending.sha, lastReviewed, pending.at),
	)
	return laterReviewSHA(best, unreviewedSHA(incoming.sha, lastReviewed, incoming.at))
}

// promotedReviewSHA is the pending commit a finished review should claim.
// An empty result means pending is missing, already reviewed, or not newer
// than inflight. The returned time is that commit's own arrival time.
func promotedReviewSHA(lastReviewed string, inflight, pending reviewSHA) reviewSHA {
	inflight = inflight.normalized()
	best := laterReviewSHA(
		unreviewedSHA(inflight.sha, lastReviewed, inflight.at),
		unreviewedSHA(pending.sha, lastReviewed, pending.at),
	)
	if best.sha == "" || best.sha == inflight.sha {
		return reviewSHA{}
	}
	return best
}

// decidePullRequestAdmission picks what one delivery does once the per-PR lock
// is held. inFlight is a running task or linked run, not an open issue: a
// review can sit in_review waiting for merge and still accept a new commit.
// The returned commit is the newest unreviewed candidate. Its time belongs to
// that candidate, not to a losing delivery.
func decidePullRequestAdmission(inFlight bool, lastReviewed string, inflight, pending, incoming reviewSHA) (prAdmissionAction, reviewSHA) {
	best := chooseUnreviewedSHA(lastReviewed, inflight, pending, incoming)
	if inFlight {
		return prAdmitCoalesce, best
	}
	if best.sha == "" {
		return prAdmitReviewed, incoming.normalized()
	}
	return prAdmitStart, best
}

// shouldReplacePending reports whether an incoming delivery may replace the
// stored pending commit. A missing time never erases a known time. When both
// times are missing, the stored commit stays. Equal times keep the stored one.
func shouldReplacePending(existingSHA string, existingAt, incomingAt time.Time) bool {
	if strings.TrimSpace(existingSHA) == "" {
		return true
	}
	switch {
	case !existingAt.IsZero() && !incomingAt.IsZero():
		return incomingAt.After(existingAt)
	case !existingAt.IsZero():
		return false
	case !incomingAt.IsZero():
		return true
	default:
		return false
	}
}

// shouldRecordCoalescedPending writes pending only when this delivery is the
// newest unreviewed commit and it is not already the inflight commit.
func shouldRecordCoalescedPending(inflight, pending, incoming, best reviewSHA) bool {
	best = best.normalized()
	incoming = incoming.normalized()
	inflight = inflight.normalized()
	pending = pending.normalized()
	if best.sha == "" || best.sha != incoming.sha || best.sha == inflight.sha {
		return false
	}
	return shouldReplacePending(pending.sha, pending.at, best.at)
}

func stampPullRequestReviewStart(ctx context.Context, q *db.Queries, issue db.Issue, signal pullRequestReviewSignal) error {
	if err := setIssueMetadataString(ctx, q, issue, metaReviewRepo, signal.Repo); err != nil {
		return err
	}
	if err := setIssueMetadataString(ctx, q, issue, metaReviewPR, signal.Number); err != nil {
		return err
	}
	if err := setIssueMetadataString(ctx, q, issue, metaInflightSHA, signal.SHA); err != nil {
		return err
	}
	if err := setOrDeleteIssueMetadataTime(ctx, q, issue, metaInflightAt, signal.At); err != nil {
		return err
	}
	if err := deleteIssueMetadataKey(ctx, q, issue, metaPendingSHA); err != nil {
		return err
	}
	if err := deleteIssueMetadataKey(ctx, q, issue, metaPendingAt); err != nil {
		return err
	}
	return deleteIssueMetadataKey(ctx, q, issue, metaReviewClaimAt)
}

func pullRequestReviewInFlight(ctx context.Context, q *db.Queries, issue db.Issue) (bool, error) {
	if reviewClaimIsFresh(issue.Metadata, time.Now()) {
		return true, nil
	}
	active, err := q.HasActiveTaskForIssue(ctx, issue.ID)
	if err != nil || active {
		return active, err
	}
	_, err = q.GetAutopilotRunByIssue(ctx, issue.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load pull request review run: %w", err)
	}
	return true, nil
}

func reviewClaimIsFresh(metadata []byte, now time.Time) bool {
	claimedAt := parseMetadataTime(issueMetadataString(metadata, metaReviewClaimAt))
	if claimedAt.IsZero() {
		return false
	}
	return now.Sub(claimedAt) < pullRequestReviewClaimLease
}

// SchedulePullRequestReviewFollowUp starts at most one review of the newest
// sha recorded while a review was in flight. A terminal task and an issue
// status that closes the run can both call it; the per-PR lock makes the
// second call a no-op.
func (s *AutopilotService) SchedulePullRequestReviewFollowUp(ctx context.Context, issueID pgtype.UUID) {
	if s == nil || s.Queries == nil || s.TxStarter == nil || !issueID.Valid {
		return
	}
	if err := s.schedulePullRequestReviewFollowUp(ctx, issueID); err != nil {
		slog.Warn("pull request review follow-up failed", "issue_id", util.UUIDToString(issueID), "error", err)
		s.releaseStuckPullRequestReviewRun(ctx, issueID)
	}
}

func (s *AutopilotService) schedulePullRequestReviewFollowUp(ctx context.Context, issueID pgtype.UUID) error {
	issue, err := s.Queries.GetIssue(ctx, issueID)
	if err != nil {
		return err
	}
	if issueMetadataString(issue.Metadata, metaReviewRepo) == "" {
		return nil
	}
	if !issue.OriginType.Valid || issue.OriginType.String != "autopilot" || !issue.OriginID.Valid {
		return nil
	}

	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin pull request review follow-up: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)

	repo := issueMetadataString(issue.Metadata, metaReviewRepo)
	number := issueMetadataString(issue.Metadata, metaReviewPR)
	if err := qtx.LockIssueDuplicateKey(ctx, pullRequestReviewLockKey(issue.WorkspaceID, issue.OriginID, repo, number)); err != nil {
		return fmt.Errorf("lock pull request review follow-up: %w", err)
	}
	issue, err = qtx.GetIssue(ctx, issueID)
	if err != nil {
		return err
	}
	active, err := qtx.HasActiveTaskForIssue(ctx, issue.ID)
	if err != nil {
		return err
	}
	if active {
		return nil
	}

	var completed *db.AutopilotRun
	var autopilot db.Autopilot
	run, err := qtx.GetAutopilotRunByIssue(ctx, issue.ID)
	if err == nil {
		row, cerr := qtx.UpdateAutopilotRunTerminalWithQuota(ctx, db.UpdateAutopilotRunTerminalWithQuotaParams{
			TerminalStatus: "completed",
			RunID:          run.ID,
			Consume:        true,
		})
		if cerr != nil {
			return fmt.Errorf("complete pull request review run: %w", cerr)
		}
		converted := autopilotRunFromTerminalRow(row)
		completed = &converted
		autopilot, err = qtx.GetAutopilot(ctx, run.AutopilotID)
		if err != nil {
			return err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	terminal, err := issueHasTerminalStatus(ctx, qtx, issue)
	if err != nil {
		return err
	}
	lastReviewed := issueMetadataString(issue.Metadata, metaLastReviewedSHA)
	inflight := loadReviewSHA(issue.Metadata, metaInflightSHA, metaInflightAt)
	pending := loadReviewSHA(issue.Metadata, metaPendingSHA, metaPendingAt)
	followUp := promotedReviewSHA(lastReviewed, inflight, pending)
	if !terminal && followUp.sha != "" {
		// Read pending_at before deleting it. inflight_at keeps that arrival
		// time; review_claim_at is only the lease.
		if err := setIssueMetadataString(ctx, qtx, issue, metaInflightSHA, followUp.sha); err != nil {
			return err
		}
		if err := setOrDeleteIssueMetadataTime(ctx, qtx, issue, metaInflightAt, followUp.at); err != nil {
			return err
		}
		if err := deleteIssueMetadataKey(ctx, qtx, issue, metaPendingSHA); err != nil {
			return err
		}
		if err := deleteIssueMetadataKey(ctx, qtx, issue, metaPendingAt); err != nil {
			return err
		}
		if err := setIssueMetadataString(ctx, qtx, issue, metaReviewClaimAt, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	} else if pending.sha != "" && followUp.sha == "" {
		if err := deleteIssueMetadataKey(ctx, qtx, issue, metaPendingSHA); err != nil {
			return err
		}
		if err := deleteIssueMetadataKey(ctx, qtx, issue, metaPendingAt); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit pull request review follow-up: %w", err)
	}
	if completed != nil {
		s.captureAutopilotRunCompleted(autopilot, *completed)
		if s.Bus != nil {
			s.publishRunDone(util.UUIDToString(issue.WorkspaceID), *completed, "completed")
		}
	}
	if followUp.sha == "" || terminal {
		return nil
	}
	if err := s.enqueuePullRequestReviewFollowUp(ctx, issue); err != nil {
		s.restorePullRequestReviewClaim(ctx, issue.ID, inflight, followUp)
		return err
	}
	s.clearPullRequestReviewClaim(ctx, issue.ID)
	slog.Info("pull request review follow-up enqueued",
		"issue_id", util.UUIDToString(issue.ID),
		"repo", repo,
		"pr", number,
		"sha", followUp.sha,
	)
	return nil
}

func (s *AutopilotService) enqueuePullRequestReviewFollowUp(ctx context.Context, issue db.Issue) error {
	if !issue.OriginID.Valid {
		return fmt.Errorf("enqueue pull request review follow-up: issue has no autopilot")
	}
	ap, err := s.Queries.GetAutopilot(ctx, issue.OriginID)
	if err != nil {
		return err
	}
	leader, _, err := s.resolveAutopilotLeader(ctx, ap)
	if err != nil {
		return err
	}
	err = s.enqueueAutopilotAssigneeTask(ctx, ap, issue, leader, pgtype.UUID{}, pgtype.UUID{})
	if errors.Is(err, ErrDuplicatePendingTask) {
		return nil
	}
	return err
}

func (s *AutopilotService) clearPullRequestReviewClaim(ctx context.Context, issueID pgtype.UUID) {
	if err := s.withPullRequestReviewLock(ctx, issueID, func(q *db.Queries, issue db.Issue) error {
		return deleteIssueMetadataKey(ctx, q, issue, metaReviewClaimAt)
	}); err != nil {
		slog.Warn("clear pull request review claim failed", "issue_id", util.UUIDToString(issueID), "error", err)
	}
}

// restorePullRequestReviewClaim puts the claimed commit back when enqueue
// failed. previous and claimed must come from the claim that just committed.
// Arrival times are the original ones, not the time of this restore.
func (s *AutopilotService) restorePullRequestReviewClaim(ctx context.Context, issueID pgtype.UUID, previous, claimed reviewSHA) {
	if err := s.withPullRequestReviewLock(ctx, issueID, func(q *db.Queries, issue db.Issue) error {
		return restorePullRequestReviewAfterEnqueueFailure(ctx, q, issue, previous, claimed)
	}); err != nil {
		slog.Warn("restore pull request review claim failed", "issue_id", util.UUIDToString(issueID), "error", err)
	}
}

func restorePullRequestReviewAfterEnqueueFailure(ctx context.Context, q *db.Queries, issue db.Issue, previous, claimed reviewSHA) error {
	claimed = claimed.normalized()
	previous = previous.normalized()
	if claimed.sha == "" {
		return fmt.Errorf("restore pull request review claim: empty claimed sha")
	}
	currentPending := loadReviewSHA(issue.Metadata, metaPendingSHA, metaPendingAt)
	currentInflight := loadReviewSHA(issue.Metadata, metaInflightSHA, metaInflightAt)
	if currentInflight.sha != claimed.sha {
		// Another writer owns inflight. Leave it and its claim lease alone.
		candidate := laterReviewSHA(currentPending, claimed)
		newer := laterReviewSHA(currentInflight, candidate)
		if candidate.sha == "" || candidate.sha == currentInflight.sha || newer.sha != candidate.sha {
			return nil
		}
		if err := setIssueMetadataString(ctx, q, issue, metaPendingSHA, candidate.sha); err != nil {
			return err
		}
		return setOrDeleteIssueMetadataTime(ctx, q, issue, metaPendingAt, candidate.at)
	}

	newest := laterReviewSHA(currentPending, claimed)
	restored := previous
	if newest.sha == restored.sha && !newest.at.IsZero() && (restored.at.IsZero() || newest.at.After(restored.at)) {
		restored.at = newest.at
	}
	if newest.sha != "" && newest.sha != restored.sha {
		if err := setIssueMetadataString(ctx, q, issue, metaPendingSHA, newest.sha); err != nil {
			return err
		}
		if err := setOrDeleteIssueMetadataTime(ctx, q, issue, metaPendingAt, newest.at); err != nil {
			return err
		}
	}
	if restored.sha == "" {
		if err := deleteIssueMetadataKey(ctx, q, issue, metaInflightSHA); err != nil {
			return err
		}
		if err := deleteIssueMetadataKey(ctx, q, issue, metaInflightAt); err != nil {
			return err
		}
	} else {
		if err := setIssueMetadataString(ctx, q, issue, metaInflightSHA, restored.sha); err != nil {
			return err
		}
		if err := setOrDeleteIssueMetadataTime(ctx, q, issue, metaInflightAt, restored.at); err != nil {
			return err
		}
	}
	if newest.sha == "" || newest.sha == restored.sha {
		if err := deleteIssueMetadataKey(ctx, q, issue, metaPendingSHA); err != nil {
			return err
		}
		if err := deleteIssueMetadataKey(ctx, q, issue, metaPendingAt); err != nil {
			return err
		}
	}
	return deleteIssueMetadataKey(ctx, q, issue, metaReviewClaimAt)
}

func (s *AutopilotService) releaseStuckPullRequestReviewRun(ctx context.Context, issueID pgtype.UUID) {
	issue, err := s.Queries.GetIssue(ctx, issueID)
	if err != nil || issueMetadataString(issue.Metadata, metaReviewRepo) == "" {
		return
	}
	active, err := s.Queries.HasActiveTaskForIssue(ctx, issueID)
	if err != nil || active {
		return
	}
	run, err := s.Queries.GetAutopilotRunByIssue(ctx, issueID)
	if err != nil {
		return
	}
	updated, err := s.completeAutopilotRun(ctx, db.UpdateAutopilotRunCompletedParams{ID: run.ID})
	if err != nil {
		slog.Warn("release stuck pull request review run failed", "issue_id", util.UUIDToString(issueID), "error", err)
		return
	}
	ap, aerr := s.Queries.GetAutopilot(ctx, updated.AutopilotID)
	if aerr != nil {
		return
	}
	s.captureAutopilotRunCompleted(ap, updated)
	if s.Bus != nil {
		s.publishRunDone(util.UUIDToString(ap.WorkspaceID), updated, "completed")
	}
}

func (s *AutopilotService) withPullRequestReviewLock(ctx context.Context, issueID pgtype.UUID, fn func(q *db.Queries, issue db.Issue) error) error {
	issue, err := s.Queries.GetIssue(ctx, issueID)
	if err != nil {
		return err
	}
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)
	if err := qtx.LockIssueDuplicateKey(ctx, pullRequestReviewLockKey(issue.WorkspaceID, issue.OriginID, issueMetadataString(issue.Metadata, metaReviewRepo), issueMetadataString(issue.Metadata, metaReviewPR))); err != nil {
		return err
	}
	issue, err = qtx.GetIssue(ctx, issueID)
	if err != nil {
		return err
	}
	if err := fn(qtx, issue); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *AutopilotService) enqueueAutopilotAssigneeTask(ctx context.Context, ap db.Autopilot, issue db.Issue, leader db.Agent, actorUserID, triggerID pgtype.UUID) error {
	if s.TaskSvc == nil {
		return fmt.Errorf("enqueue autopilot task: task service is not configured")
	}
	if ap.AssigneeType == "squad" {
		if !s.autopilotAdmitInvoke(ctx, ap, leader, actorUserID, triggerID) {
			return fmt.Errorf("not allowed to invoke private squad leader")
		}
		if actorUserID.Valid {
			if _, err := s.TaskSvc.EnqueueTaskForSquadLeaderByActor(ctx, issue, leader.ID, ap.AssigneeID, actorUserID); err != nil {
				return fmt.Errorf("enqueue squad leader task: %w", err)
			}
			return nil
		}
		if _, err := s.TaskSvc.EnqueueTaskForSquadLeader(ctx, issue, leader.ID, ap.AssigneeID, pgtype.UUID{}, OriginDerived); err != nil {
			return fmt.Errorf("enqueue squad leader task: %w", err)
		}
		return nil
	}
	if actorUserID.Valid {
		if _, err := s.TaskSvc.EnqueueTaskForIssueByActor(ctx, issue, actorUserID); err != nil {
			return fmt.Errorf("enqueue task for issue: %w", err)
		}
		return nil
	}
	if _, err := s.TaskSvc.EnqueueTaskForIssue(ctx, issue); err != nil {
		return fmt.Errorf("enqueue task for issue: %w", err)
	}
	return nil
}

func issueHasTerminalStatus(ctx context.Context, q *db.Queries, issue db.Issue) (bool, error) {
	keys, err := issuestatus.ExpandCategories(ctx, q, issue.WorkspaceID, []string{issuestatus.CategoryDone, issuestatus.CategoryClosed})
	if err != nil {
		return false, err
	}
	for _, key := range keys {
		if issue.Status == key {
			return true, nil
		}
	}
	return false, nil
}

func setOrDeleteIssueMetadataTime(ctx context.Context, q *db.Queries, issue db.Issue, key string, at time.Time) error {
	if at.IsZero() {
		return deleteIssueMetadataKey(ctx, q, issue, key)
	}
	return setIssueMetadataString(ctx, q, issue, key, at.UTC().Format(time.RFC3339Nano))
}

func setIssueMetadataString(ctx context.Context, q *db.Queries, issue db.Issue, key, value string) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = q.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{
		Key:         key,
		Value:       raw,
		ID:          issue.ID,
		WorkspaceID: issue.WorkspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("set issue metadata %s: %w", key, err)
	}
	return nil
}

func deleteIssueMetadataKey(ctx context.Context, q *db.Queries, issue db.Issue, key string) error {
	_, err := q.DeleteIssueMetadataKey(ctx, db.DeleteIssueMetadataKeyParams{
		Key:         key,
		ID:          issue.ID,
		WorkspaceID: issue.WorkspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete issue metadata %s: %w", key, err)
	}
	return nil
}

func issueMetadataString(metadata []byte, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &obj); err != nil {
		return ""
	}
	raw, ok := obj[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		return strconv.FormatInt(int64(number), 10)
	}
	return ""
}

func parseMetadataTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed
	}
	return time.Time{}
}

func parseCodeChangingPullRequest(payload []byte) (pullRequestReviewSignal, bool) {
	signal, isPullRequest := parsePullRequestEvent(payload)
	if !isPullRequest || !isCodeChangingPullRequestAction(signal.Action) {
		return signal, false
	}
	if signal.Repo == "" || signal.Number == "" || signal.SHA == "" {
		return signal, false
	}
	return signal, true
}

func pullRequestEventAction(payload []byte) string {
	signal, isPullRequest := parsePullRequestEvent(payload)
	if !isPullRequest {
		return ""
	}
	return signal.Action
}

func parsePullRequestEvent(payload []byte) (pullRequestReviewSignal, bool) {
	if len(payload) == 0 {
		return pullRequestReviewSignal{}, false
	}
	var envelope struct {
		Event        string          `json:"event"`
		EventPayload json.RawMessage `json:"eventPayload"`
		Request      struct {
			ReceivedAt string `json:"receivedAt"`
		} `json:"request"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return pullRequestReviewSignal{}, false
	}
	body := envelope.EventPayload
	if len(body) == 0 {
		body = payload
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return pullRequestReviewSignal{}, false
	}
	if !isPullRequestEventName(envelope.Event) && !hasPullRequestObject(obj) {
		return pullRequestReviewSignal{}, false
	}
	action := actionFromEventName(envelope.Event)
	if action == "" {
		action, _ = obj["action"].(string)
	}
	pr, _ := obj["pull_request"].(map[string]any)
	number := jsonNumberString(obj["number"])
	if number == "" && pr != nil {
		number = jsonNumberString(pr["number"])
	}
	var sha string
	if pr != nil {
		if head, _ := pr["head"].(map[string]any); head != nil {
			sha, _ = head["sha"].(string)
		}
	}
	sha = strings.TrimSpace(sha)
	receivedAt := parseMetadataTime(envelope.Request.ReceivedAt)
	return pullRequestReviewSignal{
		Repo:   repositoryFullName(obj),
		Number: number,
		SHA:    sha,
		Action: strings.TrimSpace(action),
		At:     receivedAt,
	}, true
}

func isPullRequestEventName(event string) bool {
	return eventName(event) == "pull_request"
}

func hasPullRequestObject(obj map[string]any) bool {
	_, ok := obj["pull_request"].(map[string]any)
	return ok
}

func eventName(event string) string {
	parts := strings.Split(event, ".")
	if len(parts) >= 2 && isForgeProvider(parts[0]) {
		return parts[1]
	}
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

func actionFromEventName(event string) string {
	parts := strings.Split(event, ".")
	if len(parts) >= 3 && isForgeProvider(parts[0]) {
		return strings.Join(parts[2:], ".")
	}
	return ""
}

func isForgeProvider(prefix string) bool {
	switch prefix {
	case "github", "gitlab", "bitbucket", "gitea":
		return true
	default:
		return false
	}
}

func isCodeChangingPullRequestAction(action string) bool {
	switch action {
	case "opened", "synchronize", "reopened", "ready_for_review":
		return true
	default:
		return false
	}
}

func repositoryFullName(obj map[string]any) string {
	repo, _ := obj["repository"].(map[string]any)
	if repo == nil {
		return ""
	}
	if full, _ := repo["full_name"].(string); strings.TrimSpace(full) != "" {
		return strings.TrimSpace(full)
	}
	owner, _ := repo["owner"].(map[string]any)
	login, _ := owner["login"].(string)
	name, _ := repo["name"].(string)
	login = strings.TrimSpace(login)
	name = strings.TrimSpace(name)
	if login == "" || name == "" {
		return ""
	}
	return login + "/" + name
}

func jsonNumberString(value any) string {
	switch typed := value.(type) {
	case float64:
		if typed <= 0 {
			return ""
		}
		return strconv.FormatInt(int64(typed), 10)
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" || trimmed == "0" {
			return ""
		}
		return trimmed
	default:
		return ""
	}
}
