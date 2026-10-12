-- Best-effort restore of the pre-552 blanket constraint. Fails when
-- project-numbered issues already reused workspace numbers; in that case the
-- data must be renumbered before the constraint can come back.
ALTER TABLE issue ADD CONSTRAINT uq_issue_workspace_number UNIQUE (workspace_id, number);
