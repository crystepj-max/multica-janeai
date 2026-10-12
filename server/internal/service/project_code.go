package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/mozillazg/go-pinyin"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// MaxProjectCodeLen caps a derived or client-supplied project code, and the
// middle segment of a three-part identifier. It mirrors the workspace
// issue_prefix rule (normalizeIssuePrefix) so the middle segment follows the
// same constraints as the first.
const MaxProjectCodeLen = 10

// maxProjectCodeLen is the package-local alias used by the derivation helpers.
const maxProjectCodeLen = MaxProjectCodeLen

// fallbackProjectCode is the derivation result when a title yields no usable
// character at all (emoji, punctuation, …). Uniqueness retries append a
// numeric suffix.
const fallbackProjectCode = "PRJ"

// maxProjectCodeChars is how many title characters contribute to the default
// code: three ASCII letters/digits, or the pinyin initials of the first three
// Chinese characters, or any mix in title order ("擎天柱工程" → "QTZ",
// "AI 客服助手" → "AIK").
const maxProjectCodeChars = 3

// DefaultProjectCodeFromTitle derives a project's default identifier code from
// its title. ASCII letters and digits pass through uppercased, CJK characters
// contribute the first letter of their pinyin reading, and every other rune is
// skipped. An empty result falls back to "PRJ". The result is not guaranteed
// unique within a workspace; callers retry with numeric suffixes.
func DefaultProjectCodeFromTitle(title string) string {
	var b strings.Builder
	for _, r := range title {
		if b.Len() >= maxProjectCodeChars {
			break
		}
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(unicode.ToUpper(r))
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case unicode.Is(unicode.Han, r):
			if initial := hanPinyinInitial(r); initial != 0 {
				b.WriteRune(initial)
			}
		}
	}
	if b.Len() == 0 {
		return fallbackProjectCode
	}
	return b.String()
}

// hanPinyinInitial returns the uppercase first letter of the character's
// pinyin reading, or 0 when the dictionary has no entry. Polyphonic
// characters resolve to the dictionary's common reading; a project code that
// reads wrong can be edited in the project detail view.
func hanPinyinInitial(r rune) rune {
	args := pinyin.NewArgs()
	args.Style = pinyin.FirstLetter
	out := pinyin.LazyPinyin(string(r), args)
	if len(out) == 0 {
		return 0
	}
	runes := []rune(out[0])
	// LazyPinyin echoes the original rune when nothing matched.
	if len(runes) != 1 || runes[0] == r || runes[0] < 'a' || runes[0] > 'z' {
		return 0
	}
	return unicode.ToUpper(runes[0])
}

// IsValidProjectCode reports whether s is a hand-set project code: 1-10
// uppercase letters/digits. It mirrors normalizeIssuePrefix's contract for
// workspace prefixes.
func IsValidProjectCode(s string) bool {
	if s == "" || len(s) > maxProjectCodeLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'A' && r <= 'Z':
		default:
			return false
		}
	}
	return true
}

// NormalizeProjectCode uppercases and strips non-alphanumerics from a
// client-supplied code, returning "" when nothing usable remains.
func NormalizeProjectCode(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(unicode.ToUpper(r))
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// EnsureProjectCode returns the project's code, deriving and claiming one
// inside the caller's transaction when the project predates project codes
// (code = ''). Legacy projects get their code at first issue creation, so the
// claim rolls back atomically if the create fails.
//
// Concurrency: SetProjectCodeIfEmpty takes the project row lock, so two
// simultaneous creates in the same project serialize; the loser re-reads the
// winner's code. A derived code that collides with a DIFFERENT project's code
// trips uq_project_workspace_code (23505) and retries with a numeric suffix.
func EnsureProjectCode(ctx context.Context, q *db.Queries, p db.Project) (string, error) {
	if p.Code != "" {
		return p.Code, nil
	}
	for n := 0; n < 100; n++ {
		code := DefaultProjectCodeFromTitle(p.Title)
		if n > 0 {
			code = fmt.Sprintf("%s%d", code, n+1)
		}
		updated, err := q.SetProjectCodeIfEmpty(ctx, db.SetProjectCodeIfEmptyParams{
			ID:   p.ID,
			Code: code,
		})
		if err == nil {
			return updated.Code, nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			// Another transaction claimed a code first — use it.
			fresh, err := q.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{
				ID:          p.ID,
				WorkspaceID: p.WorkspaceID,
			})
			if err != nil {
				return "", err
			}
			if fresh.Code != "" {
				return fresh.Code, nil
			}
			continue
		}
		if isUniqueViolation(err) {
			// A different project already holds this code; try the suffix.
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("could not derive a unique code for project %s", p.Title)
}
