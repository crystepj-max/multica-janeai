package agent

import (
	"context"
	"log/slog"
	"path/filepath"
	"runtime"
	"testing"
)

// TestWorkbuddyNewDispatchesToCodebuddyBackend asserts that
// ResolveBackend("workbuddy") dispatches to the codebuddy backend via the
// descriptor registry — the core contract that WorkBuddy is a runtime
// identity on the codebuddy protocol (the bundled CLI is a CodeBuddy Code
// fork speaking the same stream-json protocol), not a separate family.
func TestWorkbuddyNewDispatchesToCodebuddyBackend(t *testing.T) {
	if IsSupportedType("workbuddy") {
		t.Errorf("workbuddy must not be in SupportedTypes (it is a runtime identity, not a protocol family)")
	}
	b, err := ResolveBackend("workbuddy", Config{Logger: slog.Default()})
	if err != nil {
		t.Fatalf("ResolveBackend(workbuddy) returned error: %v", err)
	}
	cb, ok := b.(*codebuddyBackend)
	if !ok {
		t.Fatalf("ResolveBackend(workbuddy) returned %T, want *codebuddyBackend", b)
	}
	if cb.defaultExecutable != "workbuddy" {
		t.Errorf("defaultExecutable = %q, want %q", cb.defaultExecutable, "workbuddy")
	}
	if cb.providerLabel != "workbuddy" {
		t.Errorf("providerLabel = %q, want %q", cb.providerLabel, "workbuddy")
	}
	if cb.executableName() != "workbuddy" {
		t.Errorf("executableName() = %q, want %q", cb.executableName(), "workbuddy")
	}
	if cb.label() != "workbuddy" {
		t.Errorf("label() = %q, want %q", cb.label(), "workbuddy")
	}
}

// TestWorkbuddyRuntimeProtocolFamily verifies the compatibility target used
// by custom runtime profile validation: a workbuddy-typed profile must
// resolve to the codebuddy family.
func TestWorkbuddyRuntimeProtocolFamily(t *testing.T) {
	family, ok := RuntimeProtocolFamily("workbuddy")
	if !ok {
		t.Fatal("RuntimeProtocolFamily(workbuddy) = false, want true")
	}
	if family != "codebuddy" {
		t.Errorf("family = %q, want %q", family, "codebuddy")
	}
}

// TestCodebuddyBackendDefaults verifies the stock backend keeps the codebuddy
// executable and label when no runtime identity override is applied.
func TestCodebuddyBackendDefaults(t *testing.T) {
	b, err := ResolveBackend("codebuddy", Config{Logger: slog.Default()})
	if err != nil {
		t.Fatalf("ResolveBackend(codebuddy): %v", err)
	}
	cb, ok := b.(*codebuddyBackend)
	if !ok {
		t.Fatalf("ResolveBackend(codebuddy) returned %T, want *codebuddyBackend", b)
	}
	if cb.executableName() != "codebuddy" {
		t.Errorf("executableName() = %q, want %q", cb.executableName(), "codebuddy")
	}
	if cb.label() != "codebuddy" {
		t.Errorf("label() = %q, want %q", cb.label(), "codebuddy")
	}
}

// TestWorkbuddyModelDiscoveryWired verifies the workbuddy descriptor carries a
// model-discovery strategy and that ListModels("workbuddy") returns a usable
// catalog through it. WorkBuddy's stdio ACP mode never answers (CLI 2.147.0),
// so the fake CLI refuses `--acp` immediately and answers `--help` with the
// "Currently supported: (…)" list the parser reads — the same degradation the
// real bundled CLI goes through, without executing any user-installed CLI.
func TestWorkbuddyModelDiscoveryWired(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	desc, ok := BuiltinRuntimeByID("workbuddy")
	if !ok {
		t.Fatal("workbuddy descriptor missing from BuiltinRuntimes")
	}
	if desc.ModelDiscovery == nil {
		t.Fatal("workbuddy descriptor has no ModelDiscovery strategy; model picker would be empty")
	}

	fakePath := filepath.Join(t.TempDir(), "workbuddy")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  --acp) exit 1 ;;\n" +
		"  --help) printf '%s\\n' '  --model <model>  Model for the current session. Please provide the model ID. Currently supported: (fast-model, balanced-model, deep-model, hy4-preview)'\n" +
		"    exit 0 ;;\n" +
		"  *) exit 1 ;;\n" +
		"esac\n"
	writeTestExecutable(t, fakePath, []byte(script))

	catalog, err := ListModels(context.Background(), "workbuddy", Command{Path: fakePath})
	if err != nil {
		t.Fatalf("ListModels(workbuddy) returned error: %v", err)
	}
	if len(catalog.Models) != 4 {
		t.Fatalf("ListModels(workbuddy) returned %d models, want 4 from the --help parser", len(catalog.Models))
	}
	if catalog.Models[0].ID != "fast-model" || !catalog.Models[1].Default {
		t.Errorf("unexpected catalog head: fast-model=%+v, next=%+v", catalog.Models[0], catalog.Models[1])
	}
	if catalog.Fallback {
		t.Error("catalog marked Fallback; the --help parser should have engaged")
	}
}

// TestParseWorkbuddyHelpModels feeds a trimmed sample of the real WorkBuddy
// CLI 2.147.0 `--help` output through the parser and checks IDs, ordering,
// default marking, labels, and vendor grouping.
func TestParseWorkbuddyHelpModels(t *testing.T) {
	sample := `  --model <model>                                  Model for the current session. Please provide the model ID. Currently supported: (fast-model, balanced-model, deep-model, hy4-preview, hy3, hy3-x, deepseek-v4.1-flash, glm-5.3, glm-5.3-flash, glm-5.2, glm-5.1, glm-5v-turbo, minimax-m3, kimi-k3-1, kimi-k2.8-preview, kimi-k2.7, kimi-k2.6, deepseek-v4-pro)
  --fallback-model <model>                         Enable automatic fallback to specified model when default model is overloaded (only works with --print)
`
	models := parseWorkbuddyHelpModels([]byte(sample))
	if len(models) != 18 {
		t.Fatalf("parsed %d models, want 18", len(models))
	}
	if models[0].ID != "fast-model" {
		t.Errorf("first ID = %q, want fast-model", models[0].ID)
	}
	if models[3].ID != "hy4-preview" || models[3].Label != "Hy4 Preview" || models[3].Provider != "hunyuan" {
		t.Errorf("hy4-preview entry wrong: %+v", models[3])
	}
	if models[7].ID != "glm-5.3" || models[7].Provider != "zhipu" {
		t.Errorf("glm-5.3 entry wrong: %+v", models[7])
	}
	var defaults int
	for _, m := range models {
		if m.Default {
			defaults++
			if m.ID != "balanced-model" {
				t.Errorf("Default marked on %q, want balanced-model", m.ID)
			}
		}
	}
	if defaults != 1 {
		t.Errorf("got %d default models, want exactly 1", defaults)
	}
	if got := models[6].Label; got != "DeepSeek V4.1 Flash" {
		t.Errorf("deepseek label = %q", got)
	}
	// No marker → nil, so the caller keeps its previous answer.
	if parseWorkbuddyHelpModels([]byte("no supported list here")) != nil {
		t.Error("parser returned models for text without the marker")
	}
}
