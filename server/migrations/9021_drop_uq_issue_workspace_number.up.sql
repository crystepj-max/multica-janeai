-- The blanket (workspace_id, number) uniqueness is replaced by the two partial
-- indexes from 553/554: project-numbered issues may legitimately share a number
-- value with a workspace-numbered issue, and with issues of other projects.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS uq_issue_workspace_number;
