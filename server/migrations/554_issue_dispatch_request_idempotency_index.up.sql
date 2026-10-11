CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_dispatch_request_workspace_key
    ON issue_dispatch_request (workspace_id, idempotency_key);
