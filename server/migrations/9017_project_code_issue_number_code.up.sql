-- Renumbered from 552: upstream claimed the 552-572 range first, and the
-- fork's copy shipped to production under the old number, so every statement
-- is idempotent and re-running here is a no-op.
-- Project-level issue numbering: issues created inside a project get a
-- three-segment identifier "WORKSPACEPREFIX-PROJECTCODE-NUMBER" counted from a
-- per-project counter. Projectless issues keep the workspace-wide sequence.
--
-- project.code holds the project's short code segment (unique per workspace,
-- enforced by a later migration); empty means "not derived yet" — legacy
-- projects get one lazily when their first issue is created.
ALTER TABLE project
    ADD COLUMN IF NOT EXISTS code TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS issue_counter INT NOT NULL DEFAULT 0;

-- Snapshot of the code segment baked into an issue's identifier at creation
-- time. Empty = numbered by the workspace sequence (identifier "PREFIX-N").
-- The snapshot, not the live project.code, keeps every issued identifier
-- stable across project renames, issue moves, and project deletion.
ALTER TABLE issue
    ADD COLUMN IF NOT EXISTS number_code TEXT NOT NULL DEFAULT '';
