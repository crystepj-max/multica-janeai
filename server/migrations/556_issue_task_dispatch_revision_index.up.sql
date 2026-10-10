CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_task_issue_dispatch_revision
    ON agent_task_queue (issue_id, agent_id, dispatch_issue_revision)
    WHERE issue_id IS NOT NULL AND dispatch_issue_revision IS NOT NULL;
