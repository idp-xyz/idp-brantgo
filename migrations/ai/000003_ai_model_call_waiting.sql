-- A tool call that waits for a person's approval grant is not a failure (S4 G1, D1 finding 5).
-- state waiting, with code WAITING_APPROVAL, is that wait. A refusal stays failed, with its own code.
-- A wait is settled from reserved once, like any other known outcome, and its cost is known (zero
-- when the scripted profile is free). It is not settled again.

DO $$
DECLARE r record;
BEGIN
    FOR r IN
        SELECT con.conname
        FROM pg_constraint con
        WHERE con.conrelid = 'brant_ai.model_call'::regclass AND con.contype = 'c'
            AND pg_get_constraintdef(con.oid) LIKE '%state%'
    LOOP
        EXECUTE format('ALTER TABLE brant_ai.model_call DROP CONSTRAINT %I', r.conname);
    END LOOP;
END
$$;

ALTER TABLE brant_ai.model_call ADD CONSTRAINT model_call_state_check
    CHECK (state IN ('reserved', 'succeeded', 'failed', 'unavailable', 'unknown', 'waiting'));
ALTER TABLE brant_ai.model_call ADD CONSTRAINT model_call_settled_check
    CHECK ((state = 'reserved') = (settled_at IS NULL));
ALTER TABLE brant_ai.model_call ADD CONSTRAINT model_call_cost_check
    CHECK (CASE WHEN state IN ('reserved', 'unknown') THEN cost_minor IS NULL ELSE cost_minor IS NOT NULL END);
ALTER TABLE brant_ai.model_call ADD CONSTRAINT model_call_reservation_check
    CHECK (state <> 'reserved' OR (model_returned IS NULL AND output_sha256 IS NULL AND input_tokens IS NULL
        AND output_tokens IS NULL AND latency_ms IS NULL AND finish_reason IS NULL AND action_id IS NULL AND code IS NULL));
ALTER TABLE brant_ai.model_call ADD CONSTRAINT model_call_failure_code_check
    CHECK (state NOT IN ('failed', 'unavailable') OR code IS NOT NULL);
ALTER TABLE brant_ai.model_call ADD CONSTRAINT model_call_waiting_code_check
    CHECK (state <> 'waiting' OR code = 'WAITING_APPROVAL');

CREATE OR REPLACE FUNCTION brant_ai.guard_execution_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
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
        IF ((OLD.state = 'reserved' AND NEW.state IN ('succeeded', 'failed', 'unavailable', 'unknown', 'waiting'))
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
