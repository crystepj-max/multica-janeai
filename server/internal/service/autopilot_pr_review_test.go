package service

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPullRequestReviewDispatchMarker(t *testing.T) {
	if pullRequestReviewDispatchMarker != "[pr-review-dispatch:per-pr]" {
		t.Fatalf("marker = %q", pullRequestReviewDispatchMarker)
	}
	if !autopilotUsesPullRequestReviewDispatch("prefix " + pullRequestReviewDispatchMarker + " suffix") {
		t.Fatal("expected marker to opt in")
	}
	if autopilotUsesPullRequestReviewDispatch("GitHub PR 自动审查") {
		t.Fatal("plain description must keep the title guard")
	}
}

func TestParseCodeChangingPullRequest(t *testing.T) {
	sha := "ec86887d4142ab7a78ad589c5c626d8cb5a0bad4"
	payload := pullRequestEnvelope("opened", "crystepj-max/STS2-GAWAIN", 87, sha, "2026-10-22T06:49:26Z")
	signal, ok := parseCodeChangingPullRequest(payload)
	if !ok {
		t.Fatal("opened should start a review")
	}
	if signal.Repo != "crystepj-max/STS2-GAWAIN" || signal.Number != "87" || signal.SHA != sha || signal.Action != "opened" {
		t.Fatalf("signal = %+v", signal)
	}
	if signal.At.IsZero() {
		t.Fatal("expected receivedAt")
	}

	for _, action := range []string{"synchronize", "reopened", "ready_for_review"} {
		payload := pullRequestEnvelope(action, "crystepj-max/STS2-GAWAIN", 88, sha, "")
		if _, ok := parseCodeChangingPullRequest(payload); !ok {
			t.Fatalf("%s should start a review", action)
		}
	}
}

func TestParsePullRequestDoesNotStartReview(t *testing.T) {
	sha := "ec86887d4142ab7a78ad589c5c626d8cb5a0bad4"
	for _, action := range []string{"labeled", "closed", "edited", "converted_to_draft"} {
		if _, ok := parseCodeChangingPullRequest(pullRequestEnvelope(action, "acme/app", 1, sha, "")); ok {
			t.Fatalf("%s must not start a review", action)
		}
		reason := pullRequestReviewSkipReason(pullRequestEnvelope(action, "acme/app", 1, sha, ""))
		if reason == "webhook does not start a pull request review" {
			t.Fatalf("action %s should be named in the skip reason, got %q", action, reason)
		}
	}
	comment := []byte(`{"event":"github.issue_comment.created","eventPayload":{"action":"created","issue":{"number":87}}}`)
	if _, ok := parseCodeChangingPullRequest(comment); ok {
		t.Fatal("issue comment must not start a review")
	}
	push := []byte(`{"event":"github.push","eventPayload":{"ref":"refs/heads/main"}}`)
	if _, ok := parseCodeChangingPullRequest(push); ok {
		t.Fatal("push must not start a review")
	}
	review := pullRequestEnvelope("submitted", "acme/app", 4, sha, "")
	review = bytesReplaceEvent(review, "github.pull_request.submitted", "github.pull_request_review.submitted")
	if _, ok := parseCodeChangingPullRequest(review); ok {
		t.Fatal("pull_request_review must not start a review")
	}
}

func TestParsePullRequestRepoAndNumberShapes(t *testing.T) {
	body := map[string]any{
		"action": "synchronize",
		"number": "88",
		"repository": map[string]any{
			"name":  "STS2-GAWAIN",
			"owner": map[string]any{"login": "crystepj-max"},
		},
		"pull_request": map[string]any{
			"head": map[string]any{"sha": "abc123"},
		},
	}
	raw, err := json.Marshal(map[string]any{
		"event":        "github.pull_request.synchronize",
		"eventPayload": body,
	})
	if err != nil {
		t.Fatal(err)
	}
	signal, ok := parseCodeChangingPullRequest(raw)
	if !ok {
		t.Fatal("expected owner/name repo and string number to parse")
	}
	if signal.Repo != "crystepj-max/STS2-GAWAIN" || signal.Number != "88" || signal.SHA != "abc123" {
		t.Fatalf("signal = %+v", signal)
	}
}

func TestDecidePullRequestAdmission(t *testing.T) {
	older := time.Date(2026, 10, 22, 6, 49, 0, 0, time.UTC)
	newer := older.Add(time.Minute)
	tests := []struct {
		name       string
		inFlight   bool
		last       string
		inflight   reviewSHA
		pending    reviewSHA
		incoming   reviewSHA
		wantAction prAdmissionAction
		wantTarget string
		wantAt     time.Time
	}{
		{
			name:       "in flight keeps newer pending over older incoming",
			inFlight:   true,
			inflight:   reviewSHA{sha: "a", at: older},
			pending:    reviewSHA{sha: "b", at: newer},
			incoming:   reviewSHA{sha: "c", at: older},
			wantAction: prAdmitCoalesce,
			wantTarget: "b",
			wantAt:     newer,
		},
		{
			name:       "in flight does not replace newer inflight with older incoming",
			inFlight:   true,
			inflight:   reviewSHA{sha: "b", at: newer},
			incoming:   reviewSHA{sha: "c", at: older},
			wantAction: prAdmitCoalesce,
			wantTarget: "b",
			wantAt:     newer,
		},
		{
			name:       "in flight records newer incoming",
			inFlight:   true,
			inflight:   reviewSHA{sha: "a", at: older},
			incoming:   reviewSHA{sha: "b", at: newer},
			wantAction: prAdmitCoalesce,
			wantTarget: "b",
			wantAt:     newer,
		},
		{
			name:       "same sha already reviewed",
			last:       "a",
			incoming:   reviewSHA{sha: "a", at: older},
			wantAction: prAdmitReviewed,
			wantTarget: "a",
			wantAt:     older,
		},
		{
			name:       "pending later than incoming starts pending",
			last:       "a",
			pending:    reviewSHA{sha: "b", at: newer},
			incoming:   reviewSHA{sha: "c", at: older},
			wantAction: prAdmitStart,
			wantTarget: "b",
			wantAt:     newer,
		},
		{
			name:       "incoming later than pending starts incoming",
			last:       "a",
			pending:    reviewSHA{sha: "b", at: older},
			incoming:   reviewSHA{sha: "c", at: newer},
			wantAction: prAdmitStart,
			wantTarget: "c",
			wantAt:     newer,
		},
		{
			name:       "timeless incoming does not replace pending",
			last:       "a",
			pending:    reviewSHA{sha: "b"},
			incoming:   reviewSHA{sha: "c"},
			wantAction: prAdmitStart,
			wantTarget: "b",
		},
		{
			name:       "equal time keeps pending over incoming",
			last:       "a",
			pending:    reviewSHA{sha: "b", at: older},
			incoming:   reviewSHA{sha: "c", at: older},
			wantAction: prAdmitStart,
			wantTarget: "b",
			wantAt:     older,
		},
		{
			name:       "unreviewed inflight beats an already reviewed incoming",
			last:       "a",
			inflight:   reviewSHA{sha: "b", at: newer},
			incoming:   reviewSHA{sha: "a", at: older},
			wantAction: prAdmitStart,
			wantTarget: "b",
			wantAt:     newer,
		},
		{
			name:       "idle new sha",
			incoming:   reviewSHA{sha: "c", at: older},
			wantAction: prAdmitStart,
			wantTarget: "c",
			wantAt:     older,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, target := decidePullRequestAdmission(tt.inFlight, tt.last, tt.inflight, tt.pending, tt.incoming)
			if action != tt.wantAction || target.sha != tt.wantTarget || !target.at.Equal(tt.wantAt) {
				t.Fatalf("got %s %s %s, want %s %s %s", action, target.sha, target.at, tt.wantAction, tt.wantTarget, tt.wantAt)
			}
		})
	}
}

func TestShouldRecordCoalescedPending(t *testing.T) {
	older := time.Date(2026, 10, 22, 6, 49, 0, 0, time.UTC)
	newer := older.Add(time.Minute)
	inflight := reviewSHA{sha: "inflight", at: newer}
	incoming := reviewSHA{sha: "old", at: older}
	if shouldRecordCoalescedPending(inflight, reviewSHA{}, incoming, inflight) {
		t.Fatal("older incoming must not overwrite an empty pending while inflight is newer")
	}
	latest := reviewSHA{sha: "new", at: newer.Add(time.Minute)}
	if !shouldRecordCoalescedPending(inflight, reviewSHA{}, latest, latest) {
		t.Fatal("newer incoming should be recorded")
	}
	pending := reviewSHA{sha: "pending", at: newer}
	if shouldRecordCoalescedPending(inflight, pending, incoming, pending) {
		t.Fatal("an older incoming must not replace the stored pending")
	}
}

func TestPromotedReviewSHAKeepsOriginalArrivalTime(t *testing.T) {
	older := time.Date(2026, 10, 22, 6, 49, 0, 0, time.UTC)
	newer := older.Add(time.Minute)
	got := promotedReviewSHA("reviewed", reviewSHA{sha: "s1", at: older}, reviewSHA{sha: "s2", at: newer})
	if got.sha != "s2" || !got.at.Equal(newer) {
		t.Fatalf("promoted = %+v", got)
	}
	if got := promotedReviewSHA("reviewed", reviewSHA{sha: "s1", at: newer}, reviewSHA{sha: "s2", at: older}); got.sha != "" {
		t.Fatalf("older pending was promoted: %+v", got)
	}
	if got := promotedReviewSHA("s1", reviewSHA{sha: "s1", at: older}, reviewSHA{}); got.sha != "" {
		t.Fatalf("reviewed inflight was promoted: %+v", got)
	}
}

func TestShouldReplacePending(t *testing.T) {
	older := time.Date(2026, 10, 22, 6, 49, 0, 0, time.UTC)
	newer := older.Add(time.Minute)
	if !shouldReplacePending("", time.Time{}, newer) {
		t.Fatal("empty pending should accept the delivery")
	}
	if shouldReplacePending("new", newer, older) {
		t.Fatal("older delivery must not downgrade pending")
	}
	if !shouldReplacePending("old", older, newer) {
		t.Fatal("newer delivery should replace pending")
	}
	if shouldReplacePending("old", older, time.Time{}) {
		t.Fatal("missing timestamp must not replace a known time")
	}
	if shouldReplacePending("old", time.Time{}, time.Time{}) {
		t.Fatal("two missing timestamps must keep the stored sha")
	}
	if !shouldReplacePending("old", time.Time{}, newer) {
		t.Fatal("a known incoming time should fill a missing stored time")
	}
	if shouldReplacePending("old", older, older) {
		t.Fatal("equal timestamps must keep the stored sha")
	}
}

func TestReviewClaimFreshness(t *testing.T) {
	now := time.Date(2026, 10, 22, 7, 0, 0, 0, time.UTC)
	fresh, err := json.Marshal(map[string]string{metaReviewClaimAt: now.Add(-time.Minute).Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	if !reviewClaimIsFresh(fresh, now) {
		t.Fatal("a one-minute-old claim is still in flight")
	}
	stale, err := json.Marshal(map[string]string{metaReviewClaimAt: now.Add(-pullRequestReviewClaimLease).Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	if reviewClaimIsFresh(stale, now) {
		t.Fatal("an expired claim must let the next delivery start")
	}
	if reviewClaimIsFresh(nil, now) {
		t.Fatal("missing claim is not in flight")
	}
}

func TestIssueMetadataStringAcceptsNumber(t *testing.T) {
	raw := []byte(`{"review_pr":87,"review_repo":"acme/app"}`)
	if got := issueMetadataString(raw, metaReviewPR); got != "87" {
		t.Fatalf("number metadata = %q", got)
	}
	if got := issueMetadataString(raw, metaReviewRepo); got != "acme/app" {
		t.Fatalf("string metadata = %q", got)
	}
}

func pullRequestEnvelope(action, repo string, number int, sha, receivedAt string) []byte {
	payload, err := json.Marshal(map[string]any{
		"event": "github.pull_request." + action,
		"eventPayload": map[string]any{
			"action": action,
			"number": number,
			"repository": map[string]any{
				"full_name": repo,
			},
			"pull_request": map[string]any{
				"number": number,
				"head":   map[string]any{"sha": sha},
			},
		},
		"request": map[string]any{"receivedAt": receivedAt},
	})
	if err != nil {
		panic(err)
	}
	return payload
}

func bytesReplaceEvent(payload []byte, old, new string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		panic(err)
	}
	obj["event"] = new
	raw, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	_ = old
	return raw
}
