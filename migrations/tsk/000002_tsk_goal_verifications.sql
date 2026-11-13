-- TSK: goal verifications (tactical model BusinessTask entity OutcomeEvidence; SM-TSK-TASK verifying ->
-- completed | waiting_input | waiting_external by goal.verify; TSK-INV-001; Context Map TSK truth
-- 目标核验结论, R18 GoalReadiness v1; baseline 8.5 OutcomeEvidence, 15.4 完成断言; WBS S4-01d).
--
-- Every goal.verify records one verification, keyed by the task version it produced: its history row
-- in task_transition (command goal.verify, from verifying) is the change it made. A verification keeps
-- who judged the goal (TRD's GoalReadiness v1, or TSK when the task names no quote root or TRD
-- refused the read), the goal and its version, the records judged (the quote root at its version, the
-- business revision at its hash, the order at its revision and hash), TRD's control of a bound root as
-- TRD showed it, the actions of the task that were submitted or unknown, every assertion with its
-- checks and evidence (JSON array; Q01.execution, TSK's own, last), the outcome over them with its
-- code, the unmet assertions, the instant TRD judged and the instant TSK recorded.
--
-- The outcome decides the state: satisfied and nothing else completes the task (only TRD's assessment
-- can be satisfied, with no unmet assertion and no unsettled action), unsatisfied leads to
-- waiting_input, unknown and error to waiting_external. At commit, every goal.verify history row has
-- its verification, so a task reaches completed only with a satisfied verification (TSK-INV-001).
-- Verifications are never changed or deleted. The quote, order and task of another context are plain
-- IDs: no foreign key crosses a schema (DB2).

CREATE TABLE brant_tsk.goal_verification (
    tenant_id          text        NOT NULL,
    company_id         text        NOT NULL,
    task_id            text        NOT NULL,
    task_version       bigint      NOT NULL CHECK (task_version > 1),
    lifecycle          text        NOT NULL CHECK (lifecycle IN ('completed', 'waiting_input', 'waiting_external')),
    goal_code          text        NOT NULL CHECK (goal_code IN ('quote_ready', 'order_active')),
    goal_version       bigint      NOT NULL CHECK (goal_version > 0),
    assessor           text        NOT NULL CHECK (assessor IN ('trd.goal_readiness.v1', 'tsk')),
    outcome            text        NOT NULL CHECK (outcome IN ('satisfied', 'unsatisfied', 'unknown', 'error')),
    code               text        CHECK (code IN ('REQUIRED_CHECK_UNSATISFIED', 'REQUIRED_CHECK_UNKNOWN')),
    unmet              text[]      NOT NULL CHECK (cardinality(unmet) <= 20),
    quote_id           text        CHECK (quote_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    root_version       bigint      CHECK (root_version > 0),
    business_revision  bigint      CHECK (business_revision > 0),
    business_hash      text        CHECK (business_hash ~ '^[0-9a-f]{64}$'),
    order_id           text        CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_revision     bigint      CHECK (order_revision > 0),
    order_hash         text        CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    control_state      text        CHECK (control_state IN ('unbound', 'bound', 'stopping', 'manual')),
    control_task_id    text,
    control_epoch      bigint      CHECK (control_epoch >= 0),
    unsettled_actions  integer     NOT NULL CHECK (unsettled_actions >= 0),
    assertions         jsonb       NOT NULL CHECK (jsonb_typeof(assertions) = 'array' AND jsonb_array_length(assertions) > 0),
    checked_at         timestamptz NOT NULL,
    recorded_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, task_id, task_version),
    FOREIGN KEY (tenant_id, company_id, task_id, task_version)
        REFERENCES brant_tsk.task_transition (tenant_id, company_id, task_id, task_version),
    -- The outcome and the state it leads to (SM-TSK-TASK's three goal.verify rows).
    CHECK ((outcome = 'satisfied') = (lifecycle = 'completed')),
    CHECK (outcome <> 'unsatisfied' OR lifecycle = 'waiting_input'),
    CHECK (outcome NOT IN ('unknown', 'error') OR lifecycle = 'waiting_external'),
    CHECK (code IS NOT DISTINCT FROM CASE outcome WHEN 'satisfied' THEN NULL WHEN 'unsatisfied' THEN 'REQUIRED_CHECK_UNSATISFIED'
        ELSE 'REQUIRED_CHECK_UNKNOWN' END),
    CHECK ((outcome = 'satisfied') = (cardinality(unmet) = 0)),
    -- Only TRD's assessment completes a task, with every action of the task settled (TSK-INV-001,
    -- TSK-INV-003); TRD judged a named quote root.
    CHECK (outcome <> 'satisfied' OR (assessor = 'trd.goal_readiness.v1' AND unsettled_actions = 0)),
    CHECK (assessor <> 'trd.goal_readiness.v1' OR (quote_id IS NOT NULL AND root_version IS NOT NULL)),
    CHECK (num_nulls(business_revision, business_hash) IN (0, 2)),
    CHECK (num_nulls(order_id, order_revision, order_hash) IN (0, 3)),
    CHECK (num_nulls(control_state, control_task_id, control_epoch) IN (0, 3))
);

-- A verification is the change of its history row: goal.verify from verifying to the state its outcome
-- leads to, on the task's goal. Verifications only grow.
CREATE FUNCTION brant_tsk.guard_goal_verification() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_tsk.goal_verification: a verification is never changed or deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_tsk.task_transition h WHERE h.tenant_id = NEW.tenant_id AND h.company_id = NEW.company_id
            AND h.task_id = NEW.task_id AND h.task_version = NEW.task_version AND h.command = 'goal.verify'
            AND h.from_state = 'verifying' AND h.to_state = NEW.lifecycle) THEN
        RAISE EXCEPTION 'brant_tsk.goal_verification: version % of task % is no goal.verify from verifying to %', NEW.task_version,
            NEW.task_id, NEW.lifecycle USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_tsk.business_task t WHERE t.tenant_id = NEW.tenant_id AND t.company_id = NEW.company_id
            AND t.task_id = NEW.task_id AND t.goal_code = NEW.goal_code AND t.goal_version = NEW.goal_version
            AND t.target_quote_id IS NOT DISTINCT FROM NEW.quote_id) THEN
        RAISE EXCEPTION 'brant_tsk.goal_verification: task % has another goal or quote', NEW.task_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER goal_verification_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_tsk.goal_verification
    FOR EACH ROW EXECUTE FUNCTION brant_tsk.guard_goal_verification();

-- At commit, a goal.verify history row has its verification (no task is completed or sent back to wait
-- by goal.verify without the record of what was judged).
CREATE FUNCTION brant_tsk.check_goal_verified() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM brant_tsk.goal_verification v WHERE v.tenant_id = NEW.tenant_id AND v.company_id = NEW.company_id
            AND v.task_id = NEW.task_id AND v.task_version = NEW.task_version) THEN
        RAISE EXCEPTION 'brant_tsk.task_transition: goal.verify at version % of task % has no verification', NEW.task_version,
            NEW.task_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER task_transition_goal_verified AFTER INSERT ON brant_tsk.task_transition
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.command = 'goal.verify') EXECUTE FUNCTION brant_tsk.check_goal_verified();

ALTER TABLE brant_tsk.goal_verification ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_tsk.goal_verification FORCE ROW LEVEL SECURITY;
CREATE POLICY goal_verification_scope ON brant_tsk.goal_verification
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
