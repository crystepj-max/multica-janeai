-- Project codes are unique within a workspace. Legacy projects keep code = ''
-- until their first issue lazily derives one; empty codes are excluded so the
-- backfill-free rollout never violates the index.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_project_workspace_code
    ON project (workspace_id, code) WHERE code <> '';
