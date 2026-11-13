-- internal/ai (brant_ai; plan S4 decision D-6, unit C2, WBS S4-03b): the evidence of AI execution —
-- the ExecutionBundles AI tasks run with (baseline 17.3), the budget each AI task runs under and the
-- ledger of every model call and tool call the execution entry reserved before making it (baseline
-- 17.4; AI-INV-003: budget and retry limits are checked at the entry before every model or tool call,
-- not by prompts; concurrent reservations never exceed the budget).
--
-- No prompt or model output is stored here, only their SHA-256, the model ID the provider returned,
-- the tokens it reported, the latency and the cost (decision E-6: synthetic data, hashes and
-- evidence files, logs without content).

-- An ExecutionBundle as the runner recorded it: content is the canonical JSON whose SHA-256 is
-- sha256 (the application and contract versions, TaskProfile, model profile, prompt template hash,
-- tools with their schema hashes, canonicalization, default budget). Never changed or deleted.
CREATE TABLE brant_ai.execution_bundle (
    tenant_id      text        NOT NULL,
    company_id     text        NOT NULL,
    bundle_id      text        NOT NULL CHECK (bundle_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    sha256         text        NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    content        jsonb       NOT NULL CHECK (jsonb_typeof(content) = 'object' AND octet_length(content::text) <= 1048576),
    recorded_by_id text        NOT NULL CHECK (recorded_by_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    recorded_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, bundle_id, sha256)
);

-- The budget of an AI task: the task's own (task.create budget) or the runner's default when the
-- task names none, fixed at the first reservation. max_cost_minor is in minor units of currency;
-- NULL means no cost cap.
CREATE TABLE brant_ai.task_budget (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    task_id         text        NOT NULL CHECK (task_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    max_model_calls integer     NOT NULL CHECK (max_model_calls BETWEEN 1 AND 100000),
    max_retries     integer     NOT NULL CHECK (max_retries BETWEEN 0 AND 100),
    currency        text        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units     integer     NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    max_cost_minor  numeric(38, 0) CHECK (max_cost_minor >= 0),
    source          text        NOT NULL CHECK (source IN ('task', 'runner')),
    created_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, task_id)
);

-- One row per model call or tool call of an AI task, inserted as a reservation before the call
-- (state reserved, reserved_minor = the cost reserved) and settled once after it: succeeded,
-- failed (the call answered with a refusal or an error that cost what it cost), unavailable (the
-- model or tool could not answer; retried within max_retries) or unknown (the outcome or its cost is
-- not known; the reservation stays counted until a later settlement). retry marks a call that
-- repeats an unavailable one. The model ID is the one the provider returned (baseline 17.3).
CREATE TABLE brant_ai.model_call (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    call_id         text        NOT NULL CHECK (call_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    task_id         text        NOT NULL,
    seq             integer     NOT NULL CHECK (seq >= 1),
    kind            text        NOT NULL CHECK (kind IN ('model', 'tool')),
    retry           boolean     NOT NULL,
    state           text        NOT NULL CHECK (state IN ('reserved', 'succeeded', 'failed', 'unavailable', 'unknown')),
    actor_id        text        NOT NULL CHECK (actor_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    task_version    bigint      NOT NULL CHECK (task_version >= 1),
    bundle_id       text        NOT NULL,
    bundle_sha256   text        NOT NULL CHECK (bundle_sha256 ~ '^[0-9a-f]{64}$'),
    profile_id      text        CHECK (profile_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    provider        text        CHECK (provider ~ '^[a-z][a-z0-9_]{0,31}$'),
    model_requested text        CHECK (char_length(model_requested) BETWEEN 1 AND 200),
    prompt_sha256   text        CHECK (prompt_sha256 ~ '^[0-9a-f]{64}$'),
    tool_command    text        CHECK (tool_command ~ '^[a-z][a-z0-9_]*\.[a-z][a-z0-9_.]*$' OR tool_command = 'invalid'),
    currency        text        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units     integer     NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    reserved_minor  numeric(38, 0) NOT NULL CHECK (reserved_minor >= 0),
    reserved_at     timestamptz NOT NULL,
    model_returned  text        CHECK (char_length(model_returned) BETWEEN 1 AND 200),
    input_sha256    text        CHECK (input_sha256 ~ '^[0-9a-f]{64}$'),
    output_sha256   text        CHECK (output_sha256 ~ '^[0-9a-f]{64}$'),
    input_tokens    bigint      CHECK (input_tokens >= 0),
    output_tokens   bigint      CHECK (output_tokens >= 0),
    latency_ms      bigint      CHECK (latency_ms >= 0),
    finish_reason   text        CHECK (char_length(finish_reason) BETWEEN 1 AND 40),
    action_id       text        CHECK (action_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    code            text        CHECK (code ~ '^[A-Z][A-Z0-9_]{1,63}$'),
    cost_minor      numeric(38, 0) CHECK (cost_minor >= 0),
    settled_at      timestamptz,
    PRIMARY KEY (tenant_id, company_id, call_id),
    UNIQUE (tenant_id, company_id, task_id, seq),
    FOREIGN KEY (tenant_id, company_id, task_id) REFERENCES brant_ai.task_budget (tenant_id, company_id, task_id),
    FOREIGN KEY (tenant_id, company_id, bundle_id, bundle_sha256)
        REFERENCES brant_ai.execution_bundle (tenant_id, company_id, bundle_id, sha256),
    -- A model call names its profile, provider, requested model and prompt template; a tool call its
    -- command.
    CHECK (CASE WHEN kind = 'model'
        THEN profile_id IS NOT NULL AND provider IS NOT NULL AND model_requested IS NOT NULL AND prompt_sha256 IS NOT NULL
            AND tool_command IS NULL
        ELSE profile_id IS NULL AND provider IS NULL AND model_requested IS NULL AND prompt_sha256 IS NULL
            AND tool_command IS NOT NULL AND model_returned IS NULL AND input_tokens IS NULL AND output_tokens IS NULL END),
    -- A reservation is settled once; only a settled call has an outcome, and its cost is known
    -- unless the outcome is unknown.
    CHECK ((state = 'reserved') = (settled_at IS NULL)),
    CHECK (CASE WHEN state IN ('reserved', 'unknown') THEN cost_minor IS NULL ELSE cost_minor IS NOT NULL END),
    CHECK (state <> 'reserved' OR (model_returned IS NULL AND output_sha256 IS NULL AND input_tokens IS NULL
        AND output_tokens IS NULL AND latency_ms IS NULL AND finish_reason IS NULL AND action_id IS NULL AND code IS NULL)),
    CHECK (state NOT IN ('failed', 'unavailable') OR code IS NOT NULL)
);
CREATE INDEX model_call_task ON brant_ai.model_call (tenant_id, company_id, task_id, seq);

-- The bundle and the budget never change; a call's reservation never changes, it is settled from
-- reserved once (to succeeded, failed, unavailable or unknown), and an unknown one once more (to a
-- known outcome). Nothing is deleted.
CREATE FUNCTION brant_ai.guard_execution_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    settled constant text[] := ARRAY['state', 'model_returned', 'input_sha256', 'output_sha256', 'input_tokens',
        'output_tokens', 'latency_ms', 'finish_reason', 'action_id', 'code', 'cost_minor', 'settled_at'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION '%: execution evidence is never deleted', TG_TABLE_NAME USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_TABLE_NAME = 'model_call' THEN
        IF TG_OP = 'INSERT' THEN
            IF NEW.state <> 'reserved' THEN
                RAISE EXCEPTION 'brant_ai.model_call: a call is reserved before it is made' USING ERRCODE = 'integrity_constraint_violation';
            END IF;
            RETURN NEW;
        END IF;
        IF ((OLD.state = 'reserved' AND NEW.state IN ('succeeded', 'failed', 'unavailable', 'unknown'))
                OR (OLD.state = 'unknown' AND NEW.state IN ('succeeded', 'failed', 'unavailable')))
                AND (to_jsonb(OLD) - settled) = (to_jsonb(NEW) - settled) THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'brant_ai.model_call: call % is %; its reservation never changes and it is settled once', OLD.call_id, OLD.state
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION '%: recorded once, never changed', TG_TABLE_NAME USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER execution_bundle_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ai.execution_bundle
    FOR EACH ROW EXECUTE FUNCTION brant_ai.guard_execution_evidence();
CREATE TRIGGER task_budget_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ai.task_budget
    FOR EACH ROW EXECUTE FUNCTION brant_ai.guard_execution_evidence();
CREATE TRIGGER model_call_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ai.model_call
    FOR EACH ROW EXECUTE FUNCTION brant_ai.guard_execution_evidence();

ALTER TABLE brant_ai.execution_bundle ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ai.execution_bundle FORCE ROW LEVEL SECURITY;
CREATE POLICY execution_bundle_scope ON brant_ai.execution_bundle
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ai.task_budget ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ai.task_budget FORCE ROW LEVEL SECURITY;
CREATE POLICY task_budget_scope ON brant_ai.task_budget
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ai.model_call ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ai.model_call FORCE ROW LEVEL SECURITY;
CREATE POLICY model_call_scope ON brant_ai.model_call
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
