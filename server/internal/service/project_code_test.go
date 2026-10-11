package service

import "testing"

func TestDefaultProjectCodeFromTitle(t *testing.T) {
	cases := []struct {
		title string
		want  string
	}{
		// First three CJK characters contribute their pinyin initials; the
		// trailing words are ignored.
		{"擎天柱工程", "QTZ"},
		// ASCII characters pass through and count toward the same budget.
		{"AI 客服助手", "AIK"},
		{"apollo 11", "APO"},
		{"App 2.0", "APP"},
		// Mixed scripts interleave in title order; ASCII may exhaust the
		// budget before the first CJK character.
		{"WiFi万能钥匙", "WIF"},
		{"A客服助手", "AKF"},
		// Punctuation, emoji, and spaces are skipped entirely.
		{"「擎」天·柱!", "QTZ"},
		// Nothing usable falls back to PRJ.
		{"!!!🎉", "PRJ"},
		{"", "PRJ"},
	}
	for _, tc := range cases {
		if got := DefaultProjectCodeFromTitle(tc.title); got != tc.want {
			t.Errorf("DefaultProjectCodeFromTitle(%q) = %q; want %q", tc.title, got, tc.want)
		}
	}
}

func TestNormalizeAndValidateProjectCode(t *testing.T) {
	if got := NormalizeProjectCode(" ab-c9 "); got != "ABC9" {
		t.Errorf("NormalizeProjectCode = %q; want ABC9", got)
	}
	if got := NormalizeProjectCode("🎉"); got != "" {
		t.Errorf("NormalizeProjectCode(emoji) = %q; want empty", got)
	}
	for _, ok := range []string{"A", "QTZ", "APP9", "ABCDEFGHIJ"} {
		if !IsValidProjectCode(ok) {
			t.Errorf("IsValidProjectCode(%q) = false; want true", ok)
		}
	}
	for _, bad := range []string{"", "ABCDEFGHIJK", "qtz", "Q T", "项目"} {
		if IsValidProjectCode(bad) {
			t.Errorf("IsValidProjectCode(%q) = true; want false", bad)
		}
	}
}
