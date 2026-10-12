ALTER TABLE agent_task_queue
    ADD COLUMN dispatch_issue_revision BIGINT
    CHECK (dispatch_issue_revision IS NULL OR dispatch_issue_revision > 0);

COMMENT ON COLUMN agent_task_queue.dispatch_issue_revision IS
    'Issue revision whose assignment/status trigger requested this task. Non-NULL only for automatic issue dispatch and explicit issue dispatch; a unique index coalesces both paths for the same issue, agent, and revision.';
