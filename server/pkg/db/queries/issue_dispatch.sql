-- name: LockIssueForDispatch :one
-- Serialize the eligibility/assignee snapshot with issue updates. A dispatch
-- cannot start from a stale plan after the issue leaves todo or changes owner.
SELECT * FROM issue
WHERE id = $1 AND workspace_id = $2
FOR NO KEY UPDATE;

-- name: ReserveIssueDispatchRequest :one
-- The unique workspace/key index makes the reservation the single winner for
-- retries. The row and agent_task_queue insert share one transaction.
INSERT INTO issue_dispatch_request (
    workspace_id, idempotency_key, issue_id, expected_issue_revision, actor_type, actor_id
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (workspace_id, idempotency_key) DO NOTHING
RETURNING *;

-- name: GetIssueDispatchRequest :one
SELECT * FROM issue_dispatch_request
WHERE workspace_id = $1 AND idempotency_key = $2;

-- name: SetIssueDispatchRequestTaskID :execrows
UPDATE issue_dispatch_request
SET task_id = $3
WHERE workspace_id = $1 AND idempotency_key = $2 AND task_id IS NULL;
