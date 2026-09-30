package execenv

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSkillsDirPathZcode is the regression guard for the provider-native skills
// directory mapping for zcode. ZCode CLI scans .zcode/skills/ (and
// .agents/skills/) in the workdir; before the mapping was added, skillsDirPath
// fell back to .agent_context/skills/, which zcode never inspects — so bound
// skills silently never reached the agent.
func TestSkillsDirPathZcode(t *testing.T) {
	t.Parallel()
	workDir := "/work/repo"
	got := skillsDirPath(workDir, "zcode")
	want := filepath.Join(workDir, ".zcode", "skills")
	if got != want {
		t.Fatalf("skillsDirPath(zcode) = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, filepath.Join(".zcode", "skills")) {
		t.Fatalf("skills dir %q is not the zcode-native .zcode/skills tree", got)
	}
}
