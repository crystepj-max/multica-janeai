package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// TestProbeAgentCLIs_DiscoversWorkbuddyFromAppBundle covers the WorkBuddy
// desktop app fallback: the bundled CodeBuddy-compatible CLI never lands on
// PATH, so both the workbuddy runtime identity and the codebuddy family must
// resolve through the app-bundle candidates when nothing else is installed.
func TestProbeAgentCLIs_DiscoversWorkbuddyFromAppBundle(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("WorkBuddy app-bundle discovery candidates are macOS-shaped")
	}

	fakeDir := t.TempDir()
	bundleCli := filepath.Join(fakeDir, "WorkBuddy.app", "Contents", "Resources", "app.asar.unpacked", "cli", "bin")
	if err := os.MkdirAll(bundleCli, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	bundledCli := filepath.Join(bundleCli, "codebuddy")
	writeDaemonTestExecutable(t, bundledCli, []byte("#!/usr/bin/env node\nconsole.log(\"2.147.0\")\n"))

	// Point discovery at the fake bundle.
	origPaths := workbuddyDesktopAppBundlePaths
	t.Cleanup(func() { workbuddyDesktopAppBundlePaths = origPaths })
	workbuddyDesktopAppBundlePaths = func() []string { return []string{bundledCli} }

	// Stub the login-shell resolver so it doesn't fork a shell.
	origShell := resolveAgentsViaLoginShell
	t.Cleanup(func() { resolveAgentsViaLoginShell = origShell })
	resolveAgentsViaLoginShell = func([]string) map[string]string {
		return map[string]string{}
	}
	resetShellResolveCacheForTest(t)

	t.Setenv("PATH", fakeDir)
	t.Setenv("MULTICA_WORKBUDDY_PATH", "")
	t.Setenv("MULTICA_CODEBUDDY_PATH", "")

	agents := probeAgentCLIs()

	wb, ok := agents["workbuddy"]
	if !ok {
		t.Fatal("workbuddy was not discovered from the app bundle")
	}
	if wb.Command != "workbuddy" {
		t.Errorf("workbuddy command = %q, want %q", wb.Command, "workbuddy")
	}
	if wb.Path != bundledCli {
		t.Errorf("workbuddy path = %q, want %q", wb.Path, bundledCli)
	}

	cb, ok := agents["codebuddy"]
	if !ok {
		t.Fatal("codebuddy was not discovered from the WorkBuddy app bundle")
	}
	if cb.Command != "codebuddy" {
		t.Errorf("codebuddy command = %q, want %q", cb.Command, "codebuddy")
	}
	if cb.Path != bundledCli {
		t.Errorf("codebuddy path = %q, want %q", cb.Path, bundledCli)
	}

	if !agent.IsBuiltinRuntime("workbuddy") {
		t.Error("workbuddy should be a built-in runtime identity")
	}
	if providerDisplayName("workbuddy") != "WorkBuddy" {
		t.Errorf("workbuddy display name = %q, want %q", providerDisplayName("workbuddy"), "WorkBuddy")
	}
}

// TestProbeAgentCLIs_ExplicitCodebuddyPathOverridesBundle verifies an explicit
// MULTICA_CODEBUDDY_PATH override wins and the WorkBuddy bundle fallback does
// not silently mask a configured-but-missing override for codebuddy.
func TestProbeAgentCLIs_ExplicitCodebuddyPathOverridesBundle(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("WorkBuddy app-bundle discovery candidates are macOS-shaped")
	}

	fakeDir := t.TempDir()
	bundleCli := filepath.Join(fakeDir, "WorkBuddy.app", "Contents", "Resources", "app.asar.unpacked", "cli", "bin", "codebuddy")
	if err := os.MkdirAll(filepath.Dir(bundleCli), 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	writeDaemonTestExecutable(t, bundleCli, []byte("#!/usr/bin/env node\nexit 0\n"))

	origPaths := workbuddyDesktopAppBundlePaths
	t.Cleanup(func() { workbuddyDesktopAppBundlePaths = origPaths })
	workbuddyDesktopAppBundlePaths = func() []string { return []string{bundleCli} }

	origShell := resolveAgentsViaLoginShell
	t.Cleanup(func() { resolveAgentsViaLoginShell = origShell })
	resolveAgentsViaLoginShell = func([]string) map[string]string {
		return map[string]string{}
	}
	resetShellResolveCacheForTest(t)

	t.Setenv("PATH", fakeDir)
	// workbuddy still resolves through the bundle; codebuddy has an explicit
	// override pointing at a missing path, so it must hard-miss.
	t.Setenv("MULTICA_WORKBUDDY_PATH", "")
	t.Setenv("MULTICA_CODEBUDDY_PATH", filepath.Join(fakeDir, "missing-codebuddy"))

	agents := probeAgentCLIs()

	if _, ok := agents["workbuddy"]; !ok {
		t.Fatal("workbuddy was not discovered from the app bundle")
	}
	if _, ok := agents["codebuddy"]; ok {
		t.Fatal("codebuddy must hard-miss when MULTICA_CODEBUDDY_PATH is set but invalid")
	}
}
