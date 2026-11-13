-- TSK: business tasks and their append-only history (tactical model TSK-AGG-BusinessTask,
-- SM-TSK-TASK, TSK-INV-001, TSK-INV-002, TSK-INV-004; Context Map TSK, R17 TSK->TRD, R18 TRD->TSK;
-- WBS S4-01; owner decisions S4 D-1, D-2, D-4; baseline 8.5 and 16.1).
--
-- A task pursues one goal (quote_ready or order_active, at the goal version it was created under)
-- for a company. Its lifecycle has the 13 states of SM-TSK-TASK and moves only along that state
-- machine's transitions, one task_version at a time (task_version CAS); the guard trigger refuses
-- any other move, any change of what a task was created with and a control epoch that goes back.
-- Its control mode (ai or manual) is apart from the lifecycle. The control epoch is TRD's: TSK
-- records the epoch TRD returned for the task's last object control command on its bound quote root
-- (0 before the first bind) and never invents one; TRD is the fence (MD-31).
--
-- Every change of a task appends one row to task_transition with the task version it produced, the
-- command, the states it moved between, the control mode and epoch after it, the control change it
-- made (bind, take_control, handoff, cancel, finish_stop, release: the ControlTransition history), the
-- actor and the authority epoch the command ran under. At commit, the task's current version has its
-- history row and that row shows the task as it is. History rows are never changed or deleted.
--
-- One open (non-terminal) task serves a goal for a source (来源防重, SM-TSK-TASK created->scoping):
-- a partial unique index. A task's quote root, RFQ and customer are plain IDs: no foreign key crosses
-- a schema (DB2). action_result and audit_event are TSK's command ledger (internal/foundation/action).

CREATE SCHEMA brant_tsk;

CREATE TABLE brant_tsk.business_task (
    tenant_id                text          NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id               text          NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    task_id                  text          NOT NULL CHECK (task_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    goal_code                text          NOT NULL CHECK (goal_code IN ('quote_ready', 'order_active')),
    goal_version             bigint        NOT NULL CHECK (goal_version > 0),
    lifecycle                text          NOT NULL CHECK (lifecycle IN ('created', 'scoping', 'running', 'waiting_input',
                                               'waiting_approval', 'waiting_external', 'verifying', 'paused', 'stopping', 'completed',
                                               'closed_unfulfilled', 'cancelled', 'failed')),
    control_mode             text          NOT NULL CHECK (control_mode IN ('ai', 'manual')),
    control_epoch            bigint        NOT NULL CHECK (control_epoch >= 0),
    task_version             bigint        NOT NULL CHECK (task_version > 0),
    initiator_kind           text          NOT NULL CHECK (initiator_kind IN ('human', 'system')),
    initiator_id             text          NOT NULL CHECK (initiator_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    owner_id                 text          NOT NULL CHECK (owner_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    owner_team_id            text,
    agent_id                 text          CHECK (agent_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    target_rfq_id            text          CHECK (target_rfq_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    target_quote_id          text          CHECK (target_quote_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    target_customer_id       text          CHECK (target_customer_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    source_kind              text          CHECK (source_kind IN ('evd_source', 'rfq')),
    source_id                text          CHECK (source_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    deadline                 timestamptz,
    budget_max_model_calls   integer       CHECK (budget_max_model_calls BETWEEN 1 AND 100000),
    budget_max_retries       integer       CHECK (budget_max_retries BETWEEN 0 AND 100),
    budget_cost_currency     text          CHECK (budget_cost_currency ~ '^[A-Z]{3}$'),
    budget_cost_minor_units  smallint      CHECK (budget_cost_minor_units BETWEEN 0 AND 4),
    budget_cost_minor        numeric(30,0) CHECK (budget_cost_minor >= 0),
    bundle_id                text          CHECK (bundle_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    bundle_sha256            text          CHECK (bundle_sha256 ~ '^[0-9a-f]{64}$'),
    binding_quote_id         text          CHECK (binding_quote_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    binding_state            text          CHECK (binding_state IN ('bound', 'released')),
    binding_epoch            bigint        CHECK (binding_epoch > 0),
    bound_at                 timestamptz,
    released_at              timestamptz,
    wait_kind                text          CHECK (wait_kind IN ('input', 'approval', 'external')),
    wait_reason              text          CHECK (char_length(wait_reason) BETWEEN 1 AND 1000),
    wait_responsible_id      text          CHECK (wait_responsible_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    wait_items               text[]        CHECK (cardinality(wait_items) <= 20),
    awaiting_kind            text          CHECK (awaiting_kind ~ '^[a-z][a-z0-9_]{0,39}$'),
    awaiting_id              text          CHECK (awaiting_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    awaiting_version         bigint        CHECK (awaiting_version >= 0),
    wait_due_at              timestamptz,
    wait_since               timestamptz,
    pause_cause              text          CHECK (pause_cause IN ('budget_exhausted', 'model_unavailable', 'tool_unavailable', 'conflict',
                                               'operator')),
    pause_reason             text          CHECK (char_length(pause_reason) BETWEEN 1 AND 1000),
    pause_responsible_id     text          CHECK (pause_responsible_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    resume_condition         text          CHECK (char_length(resume_condition) BETWEEN 1 AND 1000),
    paused_at                timestamptz,
    paused_from              text          CHECK (paused_from IN ('created', 'scoping', 'running', 'waiting_input', 'waiting_approval',
                                               'waiting_external', 'verifying')),
    stop_purpose             text          CHECK (stop_purpose IN ('take_control', 'handoff', 'cancel')),
    stop_from                text          CHECK (stop_from IN ('created', 'scoping', 'running', 'waiting_input', 'waiting_approval',
                                               'waiting_external', 'verifying')),
    stop_reason              text          CHECK (char_length(stop_reason) BETWEEN 1 AND 1000),
    stop_by_kind             text          CHECK (stop_by_kind = 'human'),
    stop_by_id               text,
    stop_at                  timestamptz,
    scope_settled_at         timestamptz,
    completion_actor         text          CHECK (completion_actor IN ('ai_with_required_review', 'manual')),
    end_cause                text          CHECK (end_cause IN ('customer_declined', 'quote_expired', 'superseded', 'not_pursued', 'cancelled',
                                               'failed')),
    end_reason               text          CHECK (char_length(end_reason) BETWEEN 1 AND 1000),
    end_responsible_id       text          CHECK (end_responsible_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    ended_by_kind            text          CHECK (ended_by_kind IN ('human', 'system')),
    ended_by_id              text,
    ended_at                 timestamptz,
    request_hash             text          NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    created_at               timestamptz   NOT NULL,
    updated_at               timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, task_id),
    -- An AI task names its agent (D-2).
    CHECK (control_mode <> 'ai' OR agent_id IS NOT NULL),
    CHECK (num_nulls(source_kind, source_id) IN (0, 2)),
    CHECK (num_nulls(budget_max_model_calls, budget_max_retries) IN (0, 2)),
    CHECK (num_nulls(budget_cost_currency, budget_cost_minor_units, budget_cost_minor) IN (0, 3)),
    CHECK (budget_cost_currency IS NULL OR budget_max_model_calls IS NOT NULL),
    CHECK (num_nulls(bundle_id, bundle_sha256) IN (0, 2)),
    -- The binding TSK recorded: the root, its state and TRD's epoch, which is the task's control
    -- epoch; no epoch before the first bind.
    CHECK (num_nulls(binding_quote_id, binding_state, binding_epoch, bound_at) IN (0, 4)),
    CHECK ((released_at IS NOT NULL) = (binding_state IS NOT DISTINCT FROM 'released')),
    CHECK (CASE WHEN binding_state IS NULL THEN control_epoch = 0 ELSE binding_epoch = control_epoch END),
    CHECK (binding_quote_id IS NULL OR binding_quote_id = target_quote_id),
    -- A waiting task records what it waits for, and keeps it while it is paused or stopping from that
    -- wait, to resume to it (SM-TSK-TASK paused -> waiting_*); a paused task records the state it was
    -- paused in, and task.pause's cause, reason, responsible person and resume condition.
    CHECK (num_nulls(wait_kind, wait_reason, wait_responsible_id, wait_since) IN (0, 4)),
    CHECK (wait_kind IS NOT NULL OR num_nulls(wait_items, awaiting_kind, wait_due_at) = 3),
    CHECK (num_nulls(awaiting_kind, awaiting_id, awaiting_version) IN (0, 3)),
    CHECK (wait_kind IS NULL OR lifecycle = 'waiting_' || wait_kind OR (lifecycle = 'paused' AND paused_from = 'waiting_' || wait_kind)
        OR (lifecycle = 'stopping' AND stop_from = 'waiting_' || wait_kind)),
    CHECK (wait_kind IS NOT NULL OR NOT (lifecycle IN ('waiting_input', 'waiting_approval', 'waiting_external')
        OR (lifecycle = 'paused' AND paused_from IN ('waiting_input', 'waiting_approval', 'waiting_external'))
        OR (lifecycle = 'stopping' AND stop_from IN ('waiting_input', 'waiting_approval', 'waiting_external')))),
    CHECK ((lifecycle = 'paused') = (paused_from IS NOT NULL)),
    CHECK (num_nulls(pause_cause, pause_reason, pause_responsible_id, resume_condition, paused_at) IN (0, 5)),
    CHECK (pause_cause IS NULL OR lifecycle = 'paused'),
    -- A stopping task has its stop request, which is kept until the next one.
    CHECK (num_nulls(stop_purpose, stop_from, stop_reason, stop_by_kind, stop_by_id, stop_at) IN (0, 6)),
    CHECK (lifecycle <> 'stopping' OR stop_purpose IS NOT NULL),
    -- completed only with its completion actor, never ai_unassisted in P0 (TSK-INV-001, baseline 16.1);
    -- the other ends with their cause.
    CHECK ((completion_actor IS NOT NULL) = (lifecycle = 'completed')),
    CHECK ((end_cause IS NOT NULL) = (lifecycle IN ('closed_unfulfilled', 'cancelled', 'failed'))),
    CHECK (num_nulls(end_cause, end_reason, ended_by_kind, ended_by_id, ended_at) IN (0, 5)),
    CHECK (end_cause IS DISTINCT FROM 'failed' OR end_responsible_id IS NOT NULL),
    CHECK (end_cause IS NULL OR lifecycle = CASE end_cause WHEN 'cancelled' THEN 'cancelled' WHEN 'failed' THEN 'failed'
        ELSE 'closed_unfulfilled' END),
    -- running, verifying and completed only once the scope is settled (scoping->running, or the
    -- resume that clears what scoping waited for).
    CHECK (scope_settled_at IS NOT NULL OR lifecycle NOT IN ('running', 'verifying', 'completed')),
    CHECK (updated_at >= created_at)
);

-- 来源防重: one open task serves a goal for a source (AN06: a repeated reply or a re-claim creates no
-- second task).
CREATE UNIQUE INDEX business_task_open_source ON brant_tsk.business_task (tenant_id, company_id, goal_code, source_kind, source_id)
    WHERE source_kind IS NOT NULL AND lifecycle NOT IN ('completed', 'closed_unfulfilled', 'cancelled', 'failed');

-- A task is inserted created at version 1 without control, and changes only along SM-TSK-TASK: one
-- version at a time, through a pair of states the state machine connects (a terminal task keeps its
-- state; only task.release, which gives the root back, changes it), never going back on its control
-- epoch, never changing what it was created with; its target quote is set once, by its binding. It is
-- never deleted.
CREATE FUNCTION brant_tsk.guard_task() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_fixed text[] := ARRAY['tenant_id', 'company_id', 'task_id', 'goal_code', 'goal_version', 'initiator_kind', 'initiator_id', 'owner_id',
                            'owner_team_id', 'agent_id', 'target_rfq_id', 'target_customer_id', 'source_kind', 'source_id', 'deadline',
                            'budget_max_model_calls', 'budget_max_retries', 'budget_cost_currency', 'budget_cost_minor_units',
                            'budget_cost_minor', 'bundle_id', 'bundle_sha256', 'request_hash', 'created_at'];
    v_moves text[] := ARRAY[
        'created>scoping', 'created>paused', 'created>stopping',
        'scoping>running', 'scoping>waiting_input', 'scoping>paused', 'scoping>stopping',
        'running>running', 'running>waiting_input', 'running>waiting_approval', 'running>waiting_external', 'running>verifying',
        'running>paused', 'running>stopping', 'running>closed_unfulfilled', 'running>failed',
        'waiting_input>running', 'waiting_input>paused', 'waiting_input>stopping', 'waiting_input>closed_unfulfilled', 'waiting_input>failed',
        'waiting_approval>running', 'waiting_approval>paused', 'waiting_approval>stopping', 'waiting_approval>closed_unfulfilled',
        'waiting_approval>failed',
        'waiting_external>running', 'waiting_external>paused', 'waiting_external>stopping', 'waiting_external>closed_unfulfilled',
        'waiting_external>failed',
        'verifying>completed', 'verifying>waiting_input', 'verifying>waiting_approval', 'verifying>waiting_external', 'verifying>paused',
        'verifying>stopping', 'verifying>closed_unfulfilled', 'verifying>failed',
        'paused>running', 'paused>waiting_input', 'paused>waiting_approval', 'paused>waiting_external', 'paused>stopping',
        'paused>closed_unfulfilled', 'paused>failed', 'paused>paused',
        'stopping>paused', 'stopping>cancelled',
        'completed>completed', 'closed_unfulfilled>closed_unfulfilled', 'cancelled>cancelled', 'failed>failed'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_tsk.business_task: tasks are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.lifecycle <> 'created' OR NEW.task_version <> 1 OR NEW.control_epoch <> 0 OR NEW.binding_state IS NOT NULL
                OR NEW.stop_purpose IS NOT NULL OR NEW.wait_kind IS NOT NULL OR NEW.pause_cause IS NOT NULL OR NEW.scope_settled_at IS NOT NULL THEN
            RAISE EXCEPTION 'brant_tsk.business_task: a task is created in created at version 1 without control'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.task_version <> OLD.task_version + 1 THEN
        RAISE EXCEPTION 'brant_tsk.business_task: task % moves from version % to %, not by one', OLD.task_id, OLD.task_version,
            NEW.task_version USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT (OLD.lifecycle || '>' || NEW.lifecycle) = ANY (v_moves) THEN
        RAISE EXCEPTION 'brant_tsk.business_task: SM-TSK-TASK has no transition % -> % (task %)', OLD.lifecycle, NEW.lifecycle,
            OLD.task_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.control_epoch < OLD.control_epoch THEN
        RAISE EXCEPTION 'brant_tsk.business_task: task % records control epoch % after %', OLD.task_id, NEW.control_epoch,
            OLD.control_epoch USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (SELECT jsonb_object_agg(k, to_jsonb(NEW) -> k) FROM unnest(v_fixed) k) IS DISTINCT FROM
            (SELECT jsonb_object_agg(k, to_jsonb(OLD) -> k) FROM unnest(v_fixed) k) THEN
        RAISE EXCEPTION 'brant_tsk.business_task: what task % was created with never changes', OLD.task_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.target_quote_id IS NOT NULL AND NEW.target_quote_id IS DISTINCT FROM OLD.target_quote_id THEN
        RAISE EXCEPTION 'brant_tsk.business_task: task % keeps its target quote %', OLD.task_id, OLD.target_quote_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.completion_actor IS NOT NULL AND NEW.completion_actor IS DISTINCT FROM OLD.completion_actor THEN
        RAISE EXCEPTION 'brant_tsk.business_task: task % keeps its completion', OLD.task_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER business_task_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_tsk.business_task
    FOR EACH ROW EXECUTE FUNCTION brant_tsk.guard_task();

CREATE TABLE brant_tsk.task_transition (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    task_id         text        NOT NULL,
    task_version    bigint      NOT NULL CHECK (task_version > 0),
    command         text        NOT NULL CHECK (command IN ('task.create', 'task.scope', 'task.wait', 'task.verify', 'goal.verify',
                                    'task.pause', 'task.resume', 'task.take_control', 'task.handoff', 'task.cancel', 'task.finish_stop',
                                    'task.close_unfulfilled', 'task.fail', 'task.bind_object', 'task.release')),
    from_state      text,
    to_state        text        NOT NULL,
    control_mode    text        NOT NULL CHECK (control_mode IN ('ai', 'manual')),
    control_epoch   bigint      NOT NULL CHECK (control_epoch >= 0),
    control_change  text        CHECK (control_change IN ('bind', 'take_control', 'handoff', 'cancel', 'finish_stop', 'release')),
    actor_kind      text        NOT NULL CHECK (actor_kind IN ('human', 'system')),
    actor_id        text        NOT NULL,
    authority_epoch bigint      NOT NULL CHECK (authority_epoch > 0),
    recorded_at     timestamptz NOT NULL,
    reason          text        CHECK (char_length(reason) BETWEEN 1 AND 1000),
    PRIMARY KEY (tenant_id, company_id, task_id, task_version),
    FOREIGN KEY (tenant_id, company_id, task_id) REFERENCES brant_tsk.business_task (tenant_id, company_id, task_id),
    CHECK ((command = 'task.create') = (from_state IS NULL)),
    CHECK ((command = 'task.create') = (task_version = 1)),
    CHECK ((control_change IS NOT NULL) = (command IN ('task.bind_object', 'task.take_control', 'task.handoff', 'task.cancel',
        'task.finish_stop', 'task.release'))),
    CHECK (control_change IS NULL OR control_change = CASE command WHEN 'task.bind_object' THEN 'bind' WHEN 'task.take_control' THEN
        'take_control' WHEN 'task.handoff' THEN 'handoff' WHEN 'task.cancel' THEN 'cancel' WHEN 'task.finish_stop' THEN 'finish_stop'
        ELSE 'release' END)
);

-- The history only grows.
CREATE FUNCTION brant_tsk.guard_task_transition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'brant_tsk.task_transition: the history of a task is never changed or deleted'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER task_transition_guard BEFORE UPDATE OR DELETE ON brant_tsk.task_transition
    FOR EACH ROW EXECUTE FUNCTION brant_tsk.guard_task_transition();

-- At commit, a task's current version has its history row, and that row shows the task as it is.
CREATE FUNCTION brant_tsk.check_task() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_task record;
BEGIN
    SELECT t.task_id, t.task_version, t.lifecycle, t.control_mode, t.control_epoch INTO v_task FROM brant_tsk.business_task t
    WHERE t.tenant_id = NEW.tenant_id AND t.company_id = NEW.company_id AND t.task_id = NEW.task_id;
    IF NOT EXISTS (SELECT 1 FROM brant_tsk.task_transition h WHERE h.tenant_id = NEW.tenant_id AND h.company_id = NEW.company_id
            AND h.task_id = NEW.task_id AND h.task_version = v_task.task_version AND h.to_state = v_task.lifecycle
            AND h.control_mode = v_task.control_mode AND h.control_epoch = v_task.control_epoch) THEN
        RAISE EXCEPTION 'brant_tsk.business_task: version % of task % has no history row', v_task.task_version, v_task.task_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER business_task_at_commit AFTER INSERT OR UPDATE ON brant_tsk.business_task
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_tsk.check_task();

CREATE TABLE brant_tsk.action_result (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    command         text        NOT NULL CHECK (command ~ '^[a-z][a-z0-9_]*\.[a-z][a-z0-9_.]*$'),
    idempotency_key text        NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    request_hash    text        NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    action_id       text        NOT NULL,
    object_id       text        NOT NULL,
    row_version     bigint      NOT NULL CHECK (row_version > 0),
    status          text        NOT NULL,
    actor_kind      text        NOT NULL CHECK (actor_kind IN ('human', 'ai', 'system')),
    actor_id        text        NOT NULL,
    authority_epoch bigint      NOT NULL CHECK (authority_epoch > 0),
    correlation_id  text        NOT NULL,
    recorded_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, command, idempotency_key),
    UNIQUE (tenant_id, company_id, action_id)
);

CREATE TABLE brant_tsk.audit_event (
    seq             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    event_id        text        NOT NULL UNIQUE,
    actor_kind      text        NOT NULL CHECK (actor_kind IN ('human', 'ai', 'system')),
    actor_id        text        NOT NULL,
    command         text        NOT NULL,
    object_id       text,
    outcome         text        NOT NULL CHECK (outcome IN ('succeeded', 'replayed', 'rejected', 'read')),
    code            text,
    correlation_id  text        NOT NULL,
    recorded_at     timestamptz NOT NULL,
    reason          text,
    authority_epoch bigint,
    CHECK ((outcome = 'rejected') = (code IS NOT NULL))
);

ALTER TABLE brant_tsk.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_tsk.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_tsk.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_tsk.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_tsk.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_tsk.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_tsk.business_task ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_tsk.business_task FORCE ROW LEVEL SECURITY;
CREATE POLICY business_task_scope ON brant_tsk.business_task
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_tsk.task_transition ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_tsk.task_transition FORCE ROW LEVEL SECURITY;
CREATE POLICY task_transition_scope ON brant_tsk.task_transition
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
