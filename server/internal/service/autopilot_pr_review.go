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
	pending := issueMetadataString(existing.Metadata, metaPendingSHA)
	action, target := decidePullRequestAdmission(inFlight, lastReviewed, pending, signal.SHA)
	switch action {
	case prAdmitCoalesce:
		if !shouldReplacePending(pending, parseMetadataTime(issueMetadataString(existing.Metadata, metaPendingAt)), signal.At) {
			return pullRequestAdmission{
				handled: true,
				skip: &errDispatchSkipped{
					reason: "coalesced onto in-flight pull request review issue: " + util.UUIDToString(existing.ID),
					code:   dispatch.ReasonCoalesced,
				},
			}, nil
		}
		if err := setIssueMetadataString(ctx, q, existing, metaPendingSHA, signal.SHA); err != nil {
			return pullRequestAdmission{}, err
		}
		pendingAt := signal.At
		if pendingAt.IsZero() {
			pendingAt = time.Now().UTC()
		}
		if err := setIssueMetadataString(ctx, q, existing, metaPendingAt, pendingAt.UTC().Format(time.RFC3339Nano)); err != nil {
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
		start.SHA = target
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

// decidePullRequestAdmission picks what one delivery does once the per-PR lock
// is held. inFlight is a running task or linked run, not an open issue: a
// review can sit in_review waiting for merge and still accept a new commit.
func decidePullRequestAdmission(inFlight bool, lastReviewed, pending, incoming string) (prAdmissionAction, string) {
	if inFlight {
		return prAdmitCoalesce, incoming
	}
	if incoming == lastReviewed && pending != "" && pending != lastReviewed {
		return prAdmitStart, pending
	}
	if incoming == lastReviewed {
		return prAdmitReviewed, incoming
	}
	return prAdmitStart, incoming
}

func shouldReplacePending(existingSHA string, existingAt, incomingAt time.Time) bool {
	if strings.TrimSpace(existingSHA) == "" {
		return true
	}
	if incomingAt.IsZero() || existingAt.IsZero() {
		return true
	}
	return !incomingAt.Before(existingAt)
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
	pending := issueMetadataString(issue.Metadata, metaPendingSHA)
	inflight := issueMetadataString(issue.Metadata, metaInflightSHA)
	var followUpSHA string
	if !terminal && pending != "" && pending != inflight {
		followUpSHA = pending
		if err := setIssueMetadataString(ctx, qtx, issue, metaInflightSHA, pending); err != nil {
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
	} else if pending != "" && pending == inflight {
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
	if followUpSHA == "" {
		return nil
	}
	if err := s.enqueuePullRequestReviewFollowUp(ctx, issue); err != nil {
		s.restorePullRequestReviewClaim(ctx, issue.ID, inflight, followUpSHA)
		return err
	}
	s.clearPullRequestReviewClaim(ctx, issue.ID)
	slog.Info("pull request review follow-up enqueued",
		"issue_id", util.UUIDToString(issue.ID),
		"repo", repo,
		"pr", number,
		"sha", followUpSHA,
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

// restorePullRequestReviewClaim puts the sha back when enqueue failed, without
// overwriting a newer pending sha recorded by a webhook during the gap.
func (s *AutopilotService) restorePullRequestReviewClaim(ctx context.Context, issueID pgtype.UUID, previousInflight, target string) {
	if err := s.withPullRequestReviewLock(ctx, issueID, func(q *db.Queries, issue db.Issue) error {
		pending := issueMetadataString(issue.Metadata, metaPendingSHA)
		if pending == "" || pending == target {
			if err := setIssueMetadataString(ctx, q, issue, metaPendingSHA, target); err != nil {
				return err
			}
			if err := setIssueMetadataString(ctx, q, issue, metaPendingAt, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
		if issueMetadataString(issue.Metadata, metaInflightSHA) == target {
			if previousInflight == "" {
				if err := deleteIssueMetadataKey(ctx, q, issue, metaInflightSHA); err != nil {
					return err
				}
			} else if err := setIssueMetadataString(ctx, q, issue, metaInflightSHA, previousInflight); err != nil {
				return err
			}
		}
		return deleteIssueMetadataKey(ctx, q, issue, metaReviewClaimAt)
	}); err != nil {
		slog.Warn("restore pull request review claim failed", "issue_id", util.UUIDToString(issueID), "error", err)
	}
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
