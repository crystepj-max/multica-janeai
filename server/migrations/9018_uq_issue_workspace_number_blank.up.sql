-- Replacement uniqueness for the workspace-numbered sequence, scoped to
-- workspace-numbered rows only. Project-numbered rows (number_code <> '')
-- are excluded so a project counter may reuse a number already issued by the
-- workspace sequence. The pre-556 uq_issue_workspace_number constraint stays
-- in place until this index exists, so uniqueness holds throughout.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_issue_workspace_number_blank
    ON issue (workspace_id, number) WHERE number_code = '';
