-- Keep a cancelled task in its agent's execution-capacity reservation until
-- the daemon that owned the run confirms the runner has stopped.
-- Existing cancelled rows keep the default FALSE: earlier versions did not
-- persist whether their stop acknowledgement had already arrived. Before this
-- migration is deployed, drain existing Runs or stop their runtimes; the old
-- rows cannot be classified safely from task status alone.
ALTER TABLE agent_task_queue
    ADD COLUMN IF NOT EXISTS cancel_ack_pending BOOLEAN NOT NULL DEFAULT FALSE;

CREATE OR REPLACE FUNCTION mark_agent_task_cancel_ack_pending() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status = 'cancelled'
       AND OLD.status IN ('dispatched', 'running', 'waiting_local_directory') THEN
        NEW.cancel_ack_pending := TRUE;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_agent_task_cancel_ack_pending
    BEFORE UPDATE OF status ON agent_task_queue
    FOR EACH ROW
    EXECUTE FUNCTION mark_agent_task_cancel_ack_pending();
