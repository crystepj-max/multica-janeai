CREATE TABLE issue_dispatch_request (
    workspace_id UUID NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (octet_length(idempotency_key) <= 255),
    issue_id UUID NOT NULL,
    expected_issue_revision BIGINT NOT NULL CHECK (expected_issue_revision > 0),
    actor_type TEXT NOT NULL CHECK (actor_type IN ('member', 'agent')),
    actor_id UUID NOT NULL,
    task_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE issue_dispatch_request IS
    'Durable idempotency records for explicit issue dispatch requests. The reservation and queued task commit in one transaction.';
