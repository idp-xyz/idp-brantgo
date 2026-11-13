-- TSK: action executions and their append-only attempt log (tactical model TSK-AGG-ActionExecution,
-- TSK-INV-003, X-INV-001, X-INV-003; Context Map TSK action.record_attempt, action.reconcile,
-- ActionReconciled, R18 and R66-R69 ActionReconciliation v1; WBS S4-02; owner decision S4 D-11;
-- baseline 17.1 and 17.2).
--
-- A task step that writes another context (EVD, INQ, SUP, CST or TRD) is one action of one business
-- intent: the intent is unique in the scope, so the same intent is never registered as a new action.
-- The action is recorded before its command is sent (TSK-INV-003): the target context and command,
-- the idempotency key the command carries — the action's own ID, which TSK derives from the business
-- intent — and the request hash the target's ledger records. Its outcome is learnt only from the
-- target's ledger by that key and hash (ActionReconciliation v1): committed (the target's stored
-- receipt; terminal), conflict (the key holds another request; terminal), not_committed (nothing under
-- the key; the next attempt sends the same key again), and, as the executor saw them, rejected (the
-- target refused the attempt with a code and holds nothing) and unknown (the answer was lost). An
-- action whose attempt is submitted or unknown blocks the task's stop barrier, verification, release
-- and ends (the count A1's TSK commands ask for).
--
-- Every change of an action moves its version by one and appends one row to action_attempt: the
-- attempt it concerns, the command, the state after it, the target's answer, the code, the claim's
-- control epoch, the delegation and approval grant the attempt named (references only, never
-- authority: baseline 17.1), the actor and the authority epoch it ran under. At commit, an action's
-- current version has its log row and that row shows the action as it is. Log rows are never changed
-- or deleted; actions are never deleted.

CREATE TABLE brant_tsk.action_execution (
    tenant_id           text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id          text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    action_id           text        NOT NULL CHECK (action_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    business_intent_id  text        NOT NULL CHECK (business_intent_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    task_id             text        NOT NULL,
    target_context      text        NOT NULL CHECK (target_context IN ('evd', 'inq', 'sup', 'cst', 'trd')),
    target_command      text        NOT NULL CHECK (target_command ~ '^[a-z][a-z0-9_]*\.[a-z][a-z0-9_.]*$'
                                        AND char_length(target_command) <= 100),
    idempotency_key     text        NOT NULL,
    request_hash        text        NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    state               text        NOT NULL CHECK (state IN ('submitted', 'unknown', 'rejected', 'not_committed', 'committed',
                                        'conflict')),
    action_version      bigint      NOT NULL CHECK (action_version > 0),
    attempts            integer     NOT NULL CHECK (attempts >= 0),
    result_action_id    text        CHECK (result_action_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    result_object_id    text        CHECK (result_object_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    result_row_version  bigint      CHECK (result_row_version > 0),
    result_status       text        CHECK (char_length(result_status) BETWEEN 1 AND 100),
    registered_by_kind  text        NOT NULL CHECK (registered_by_kind IN ('human', 'ai')),
    registered_by_id    text        NOT NULL,
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, action_id),
    -- One business intent is one action (TSK-INV-003).
    UNIQUE (tenant_id, company_id, business_intent_id),
    FOREIGN KEY (tenant_id, company_id, task_id) REFERENCES brant_tsk.business_task (tenant_id, company_id, task_id),
    -- The target command carries the action's own ID as its idempotency key (baseline 17.1).
    CHECK (idempotency_key = action_id),
    -- A committed action holds the target's stored receipt, and only a committed one.
    CHECK (num_nulls(result_action_id, result_object_id, result_row_version, result_status) IN (0, 4)),
    CHECK ((state = 'committed') = (result_action_id IS NOT NULL)),
    -- Every state but a committed or conflicting answer found before the first attempt follows one.
    CHECK (state IN ('committed', 'conflict') OR attempts > 0),
    CHECK (updated_at >= created_at)
);

-- The open actions of a task (submitted, unknown) are counted inside the task's commands.
CREATE INDEX action_execution_task ON brant_tsk.action_execution (tenant_id, company_id, task_id, state);

-- An action is inserted at version 1, submitted with its first attempt or settled by the target's
-- answer; it changes one version at a time along its own moves (an open attempt is settled by the
-- target's answer or the executor's observation; a settled, not committed action opens the next
-- attempt or is reconciled again; committed and conflict are final), never changing what it was
-- registered with, and opens an attempt exactly when it moves to submitted. It is never deleted.
CREATE FUNCTION brant_tsk.guard_action_execution() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_fixed text[] := ARRAY['tenant_id', 'company_id', 'action_id', 'business_intent_id', 'task_id', 'target_context', 'target_command',
                            'idempotency_key', 'request_hash', 'registered_by_kind', 'registered_by_id', 'created_at'];
    v_moves text[] := ARRAY[
        'submitted>unknown', 'submitted>rejected', 'submitted>not_committed', 'submitted>committed', 'submitted>conflict',
        'unknown>not_committed', 'unknown>committed', 'unknown>conflict',
        'rejected>submitted', 'rejected>not_committed', 'rejected>committed', 'rejected>conflict',
        'not_committed>submitted', 'not_committed>not_committed', 'not_committed>committed', 'not_committed>conflict'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_tsk.action_execution: actions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.action_version <> 1 OR NOT (NEW.state IN ('submitted', 'committed', 'conflict'))
                OR NEW.attempts <> (CASE WHEN NEW.state = 'submitted' THEN 1 ELSE 0 END) THEN
            RAISE EXCEPTION 'brant_tsk.action_execution: an action is registered at version 1 with its first attempt or the target''s answer'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.action_version <> OLD.action_version + 1 THEN
        RAISE EXCEPTION 'brant_tsk.action_execution: action % moves from version % to %, not by one', OLD.action_id, OLD.action_version,
            NEW.action_version USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT (OLD.state || '>' || NEW.state) = ANY (v_moves) THEN
        RAISE EXCEPTION 'brant_tsk.action_execution: action % cannot move % -> %', OLD.action_id, OLD.state, NEW.state
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.attempts <> OLD.attempts + (CASE WHEN NEW.state = 'submitted' THEN 1 ELSE 0 END) THEN
        RAISE EXCEPTION 'brant_tsk.action_execution: action % opens an attempt exactly when it is submitted', OLD.action_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (SELECT jsonb_object_agg(k, to_jsonb(NEW) -> k) FROM unnest(v_fixed) k) IS DISTINCT FROM
            (SELECT jsonb_object_agg(k, to_jsonb(OLD) -> k) FROM unnest(v_fixed) k) THEN
        RAISE EXCEPTION 'brant_tsk.action_execution: what action % was registered with never changes', OLD.action_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER action_execution_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_tsk.action_execution
    FOR EACH ROW EXECUTE FUNCTION brant_tsk.guard_action_execution();

CREATE TABLE brant_tsk.action_attempt (
    tenant_id           text        NOT NULL,
    company_id          text        NOT NULL,
    action_id           text        NOT NULL,
    action_version      bigint      NOT NULL CHECK (action_version > 0),
    attempt             integer     NOT NULL CHECK (attempt >= 0),
    command             text        NOT NULL CHECK (command IN ('action.record_attempt', 'action.reconcile')),
    state               text        NOT NULL CHECK (state IN ('submitted', 'unknown', 'rejected', 'not_committed', 'committed',
                                        'conflict')),
    answer              text        NOT NULL CHECK (answer IN ('committed', 'payload_conflict', 'not_committed')),
    code                text        CHECK (code ~ '^[A-Z][A-Z0-9_]{1,63}$'),
    control_epoch       bigint      NOT NULL CHECK (control_epoch >= 0),
    delegation_id       text        CHECK (delegation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    grant_id            text        CHECK (grant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    result_action_id    text,
    result_object_id    text,
    result_row_version  bigint,
    result_status       text,
    actor_kind          text        NOT NULL CHECK (actor_kind IN ('human', 'ai')),
    actor_id            text        NOT NULL,
    authority_epoch     bigint      NOT NULL CHECK (authority_epoch > 0),
    recorded_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, action_id, action_version),
    FOREIGN KEY (tenant_id, company_id, action_id) REFERENCES brant_tsk.action_execution (tenant_id, company_id, action_id),
    -- The state follows the target's answer: committed and conflict are its answers; every other
    -- state is recorded while the target holds nothing under the key.
    CHECK (CASE answer WHEN 'committed' THEN state = 'committed' WHEN 'payload_conflict' THEN state = 'conflict'
        ELSE state IN ('submitted', 'unknown', 'rejected', 'not_committed') END),
    -- The executor records the attempts and what it saw of them; a reconciliation records what the
    -- target's ledger holds.
    CHECK (state NOT IN ('submitted', 'unknown', 'rejected') OR command = 'action.record_attempt'),
    CHECK (state <> 'not_committed' OR command = 'action.reconcile'),
    CHECK (code IS NULL OR state IN ('rejected', 'unknown')),
    CHECK (state <> 'rejected' OR code IS NOT NULL),
    CHECK (num_nulls(result_action_id, result_object_id, result_row_version, result_status) IN (0, 4)),
    CHECK ((state = 'committed') = (result_action_id IS NOT NULL))
);

-- The log only grows.
CREATE FUNCTION brant_tsk.guard_action_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'brant_tsk.action_attempt: the attempts of an action are never changed or deleted'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER action_attempt_guard BEFORE UPDATE OR DELETE ON brant_tsk.action_attempt
    FOR EACH ROW EXECUTE FUNCTION brant_tsk.guard_action_attempt();

-- At commit, an action's current version has its log row, and that row shows the action as it is.
CREATE FUNCTION brant_tsk.check_action_execution() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_action record;
BEGIN
    SELECT a.action_id, a.action_version, a.state, a.attempts, a.result_action_id INTO v_action FROM brant_tsk.action_execution a
    WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id AND a.action_id = NEW.action_id;
    IF NOT EXISTS (SELECT 1 FROM brant_tsk.action_attempt l WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id
            AND l.action_id = NEW.action_id AND l.action_version = v_action.action_version AND l.state = v_action.state
            AND l.attempt = v_action.attempts AND l.result_action_id IS NOT DISTINCT FROM v_action.result_action_id) THEN
        RAISE EXCEPTION 'brant_tsk.action_execution: version % of action % has no log row', v_action.action_version, v_action.action_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER action_execution_at_commit AFTER INSERT OR UPDATE ON brant_tsk.action_execution
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_tsk.check_action_execution();

ALTER TABLE brant_tsk.action_execution ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_tsk.action_execution FORCE ROW LEVEL SECURITY;
CREATE POLICY action_execution_scope ON brant_tsk.action_execution
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_tsk.action_attempt ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_tsk.action_attempt FORCE ROW LEVEL SECURITY;
CREATE POLICY action_attempt_scope ON brant_tsk.action_attempt
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
