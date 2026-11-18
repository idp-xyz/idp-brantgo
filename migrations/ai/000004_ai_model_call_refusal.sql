-- internal/ai (brant_ai): a refused tool call keeps where and why it was refused (WBS S4-03; S4 unit G3).
--
-- The ledger held the code of a refused tool call and nothing else. A turn of the agent runner
-- starts from the records, not from a stored conversation (decision E-6), so a model that was
-- refused in one turn met the same refusal in the next: the three live trials of 2026-11-18 sent
-- the same quote draft thirteen times. A refused tool call (state failed) now also records
--
--   refusal_path    the request member the rejection points at, a JSON Pointer of plain characters
--                   (a path that carries anything else, such as a member name a model made up, is
--                   not recorded);
--   refusal_reason  the reason the refusing context stated, and only when it is one of the static
--                   texts of the product's source (internal/ai/capability StatedReason); a reason
--                   built at run time is not recorded.
--
-- Neither is model output or a business record: no prompt, argument or answer is stored, as before.
-- Both are outcome columns: set when the call is settled, never changed afterwards.

ALTER TABLE brant_ai.model_call
    ADD COLUMN refusal_path   text CHECK (refusal_path ~ '^/[A-Za-z0-9_.:~/-]{0,199}$'),
    ADD COLUMN refusal_reason text CHECK (char_length(refusal_reason) BETWEEN 1 AND 200);

ALTER TABLE brant_ai.model_call ADD CONSTRAINT model_call_refusal_check
    CHECK ((refusal_path IS NULL AND refusal_reason IS NULL) OR (kind = 'tool' AND state = 'failed'));

CREATE OR REPLACE FUNCTION brant_ai.guard_execution_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    settled constant text[] := ARRAY['state', 'model_returned', 'input_sha256', 'output_sha256', 'input_tokens',
        'output_tokens', 'latency_ms', 'finish_reason', 'action_id', 'code', 'cost_minor', 'settled_at',
        'refusal_path', 'refusal_reason'];
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
