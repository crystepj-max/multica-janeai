package service

import (
	"strings"
	"testing"
)

// Mika's prompt is a product contract, not prose that can drift freely: it is
// the only place the Chief of Staff role, its boundaries, and its decision
// rules are stated, and it ships inside the server binary so every workspace
// picks up an edit on its next run. These evals pin the parts that other
// behaviour depends on, and the budget that keeps the prompt from becoming the
// thing it is trying to fix.

const (
	// mikaInstructionBudget guards the always-loaded half. Mika's prompt is
	// rendered into Agent Identity on every run, chat turns included, so
	// unbounded growth is a cost every member pays on every message. Detail
	// belongs in the multica-pm playbooks, which load on demand.
	mikaInstructionBudget = 16000
	// mikaInstructionFloor catches a prompt truncated by a bad edit or a
	// merge. The role sections below are the substance; this is the coarse
	// backstop under them.
	mikaInstructionFloor = 9000
)

func mikaPrompt(t *testing.T) string {
	t.Helper()
	return MikaSystemInstructions(MikaDefaultName)
}

// TestMikaInstructionsKeepTheIdentityContract covers the two anchors the
// handler tests and the runtime brief depend on: the prompt opens with the
// current display name (the brief announces "**You are: <name>**" from the
// agent row, so a hardcoded name would contradict it after a rename), and it
// still states the product identity without using the display name for it.
func TestMikaInstructionsKeepTheIdentityContract(t *testing.T) {
	prompt := mikaPrompt(t)

	if !strings.HasPrefix(prompt, "You are Mika,") {
		t.Fatalf("prompt must open as the current display name:\n%s", prompt[:160])
	}
	if strings.Contains(prompt, "{{AGENT_NAME}}") {
		t.Fatal("the name placeholder must be substituted")
	}
	if !strings.Contains(prompt, "built-in system agent (Mika)") {
		t.Fatal("prompt must still identify itself as Multica's built-in system agent")
	}
}

// TestMikaInstructionsStateTheRoleAndItsBoundaries pins the responsibilities
// and the prohibitions separately. A prompt that gained capabilities without
// the matching boundary is how an assistant starts closing other people's work
// or narrating progress the platform never recorded.
func TestMikaInstructionsStateTheRoleAndItsBoundaries(t *testing.T) {
	prompt := collapseSpace(mikaPrompt(t))

	sections := []string{
		"## What you own",
		"## What you do not do",
		"## Deciding where work lives",
		"## Read the state before you decide",
		"## Priority and sequence",
		"## Breaking a goal down",
		"## Milestones and dates",
		"## Dependencies",
		"## Tracking without noise",
		"## Risk and exceptions",
		"## Coordinating people, agents, and squads",
		"## How you communicate",
		"## Language",
		"## Accuracy, stability, and cost",
		"## Every request",
		"## Platform mechanics",
	}
	for _, want := range sections {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing the %q section", want)
		}
	}

	// The boundaries that keep Mika a coordinator rather than an executor,
	// and a reporter rather than an author of state.
	boundaries := []string{
		"no repository checkout, no code edits",
		"`done` is always a human decision",
		"one result comment",
		"never imply a field exists",
		"without a concrete preview and confirmation",
	}
	for _, want := range boundaries {
		if !containsUnwrapped(prompt, want) {
			t.Errorf("prompt is missing the boundary %q", want)
		}
	}
}

// TestMikaInstructionsGroundProjectManagementInRealPlatformMechanisms guards
// the failure mode this rewrite exists to avoid: teaching a workflow the
// product cannot express. Stages, backlog promotion and due dates are real;
// a milestone field and a dependency object are not, and the prompt must say
// how it represents them instead of implying they exist.
func TestMikaInstructionsGroundProjectManagementInRealPlatformMechanisms(t *testing.T) {
	prompt := collapseSpace(mikaPrompt(t))

	for _, want := range []string{
		"`--parent` and `--stage`",
		"Create later stages as `backlog` and promote one at a time",
		"Multica has no separate milestone object",
		"Dependencies live in structure, not in prose",
		"`--until-issue`",
		"`--until-children-done`",
		"`--duplicate-of`",
		"there is no separate field for it",
	} {
		if !containsUnwrapped(prompt, want) {
			t.Errorf("prompt is missing the platform-grounded rule %q", want)
		}
	}

	// The two skills are the split this prompt relies on: mechanics and
	// playbooks load on demand instead of being restated here.
	for _, skill := range []string{"`multica-platform`", "`multica-pm`"} {
		if !strings.Contains(prompt, skill) {
			t.Errorf("prompt does not point at %s, so the on-demand split is unreachable", skill)
		}
	}
	if !named((&TaskService{}).BuiltinSkills(MikaSystemKey, false), "multica-pm") {
		t.Error("the prompt names multica-pm but Mika does not receive it")
	}
}

// TestMikaInstructionsStayWithinBudget keeps the always-loaded half from
// absorbing the playbooks. The floor catches a truncated file; the ceiling is
// the actual policy.
func TestMikaInstructionsStayWithinBudget(t *testing.T) {
	prompt := mikaPrompt(t)
	if len(prompt) > mikaInstructionBudget {
		t.Fatalf("prompt is %d bytes, over the %d-byte budget; move detail into the multica-pm playbooks",
			len(prompt), mikaInstructionBudget)
	}
	if len(prompt) < mikaInstructionFloor {
		t.Fatalf("prompt is %d bytes, under the %d-byte floor; a section was probably dropped",
			len(prompt), mikaInstructionFloor)
	}
}

// TestMikaInstructionsSeparateContentLanguageFromValueDomain pins the rule no
// other section can carry: which half of a bilingual turn may be translated.
//
// The prompt is English while most workspaces are not, and that asymmetry has
// exactly one dangerous consequence. Measured against the platform, the risk is
// not comprehension — a frontier model reads English instructions and Chinese
// issues interchangeably — it is (a) output drifting to the instruction
// language on turns that start without a human message (wakeups, stage
// barriers, scheduled checks), and (b) translating a *field value*, which the
// database rejects outright because issue.status is a CHECK constraint over
// ASCII keys. Both need to be stated; neither is inferable from "match the
// member's language".
//
// The Chinese term table deliberately lives in multica-pm rather than here: it
// is reference data with a per-language shape, so it earns its tokens only when
// the turn is actually bilingual.
func TestMikaInstructionsSeparateContentLanguageFromValueDomain(t *testing.T) {
	prompt := collapseSpace(mikaPrompt(t))

	for _, want := range []string{
		"## Language",
		"Write content in the language the work is already in",
		"Never translate a value",
		"a wakeup, a stage barrier, or a scheduled check",
		"a rejected write, not a localised one",
	} {
		if !containsUnwrapped(prompt, want) {
			t.Errorf("prompt is missing the language rule %q", want)
		}
	}

	// The value domain is the half a translation actually breaks, so the rule
	// has to name the flags an agent types rather than gesture at "fields".
	for _, flag := range []string{"`--status`", "`--priority`", "`--stage`"} {
		if !strings.Contains(prompt, flag) {
			t.Errorf("the never-translate rule does not name %s, so it stays unactionable", flag)
		}
	}

	// The prompt delegates the term table; the playbook has to actually hold it,
	// or the pointer is a dead end in exactly the workspaces that need it.
	skill, ok := findSkill(t, "multica-pm")
	if !ok {
		t.Fatal("multica-pm is missing, so the prompt's term-table pointer resolves to nothing")
	}
	for _, want := range []string{"里程碑", "urgent", "验收标准"} {
		if !strings.Contains(skill.Content, want) {
			t.Errorf("multica-pm does not map %q, so a Chinese workspace has no term table", want)
		}
	}
}

// TestComposeMikaInstructionsLayersWorkspaceNotes pins the layering contract
// without a database: the product half always leads, an empty workspace layer
// adds nothing at all (not even the heading describing it), and the team's
// notes land after the system half so they refine it instead of replacing it.
func TestComposeMikaInstructionsLayersWorkspaceNotes(t *testing.T) {
	system := mikaPrompt(t)

	if got := ComposeMikaInstructions(MikaDefaultName, ""); got != system {
		t.Fatal("an empty workspace layer must compose to exactly the system layer")
	}
	if got := ComposeMikaInstructions(MikaDefaultName, "  \n\t "); got != system {
		t.Fatal("a blank workspace layer must compose to exactly the system layer")
	}
	if strings.Contains(system, "## Workspace notes") {
		t.Fatal("the system half must not announce a notes section it does not have")
	}

	notes := "Our main repo is acme/platform."
	composed := ComposeMikaInstructions(MikaDefaultName, notes)
	if !strings.HasPrefix(composed, system) {
		t.Fatal("the system half must lead the composed prompt")
	}
	notesAt := strings.Index(composed, "## Workspace notes")
	if notesAt < strings.Index(composed, "## Platform mechanics") {
		t.Fatal("workspace notes must come after the system half, so they refine it")
	}
	for _, want := range []string{notes, "Added by this workspace's admins:"} {
		if !strings.Contains(composed[notesAt:], want) {
			t.Fatalf("composed notes section is missing %q", want)
		}
	}
}
