-- Identifier uniqueness for project-numbered issues: the snapshot code plus
-- the per-project number must be unique within the workspace. Workspace-numbered
-- rows (number_code = '') are excluded; their uniqueness is 553's index.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_issue_workspace_code_number
    ON issue (workspace_id, number_code, number) WHERE number_code <> '';
