-- Keep task writers from changing the state between the drain check and the
-- trigger/column removal. The lock is held for the duration of this migration.
LOCK TABLE agent_task_queue IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM agent_task_queue
        WHERE status IN ('dispatched', 'running', 'waiting_local_directory')
           OR cancel_ack_pending
    ) THEN
        RAISE EXCEPTION 'cannot roll back agent task cancellation acknowledgements while active Runs or pending stop acknowledgements remain';
    END IF;
END
$$;

DROP TRIGGER IF EXISTS trg_agent_task_cancel_ack_pending ON agent_task_queue;
DROP FUNCTION IF EXISTS mark_agent_task_cancel_ack_pending();
ALTER TABLE agent_task_queue DROP COLUMN IF EXISTS cancel_ack_pending;
