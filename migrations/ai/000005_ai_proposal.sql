-- S4 G7b: the request an AI agent proposed and a person is asked to approve (internal/ai/proposal).
--
-- A command that needs an approval grant (order.prepare, order.review.request) used to wait with only
-- its hash: the person approved a digest, nothing kept the request, and after the grant the model
-- rebuilt it and sometimes sent another one. The platform now stores the request the agent proposed,
-- as the arguments of the tool call without the idempotency key, once the owning context has judged
-- it without writing; the approver reads it; and after the grant the platform sends exactly it.
--
-- One row per task, command, request hash and object version (proposal_id: an approval grant binds the
-- version, which the request hash does not cover). It holds nothing but the request's own
-- arguments (identifiers, revisions, hashes), the object and version an approval grant binds, and
-- what became of it: status pending (waiting for a grant), sending (claimed by a runner), sent (the
-- owning context committed it), refused (the owning context refused it after the grant) or void (the
-- admission refused it: delegation, task or grant). A request refused or void that the agent proposes
-- again, and the owning context then accepts, is pending again with the next attempt.
--
-- A proposal is never authority: the approval grant, the delegation and the task are checked again
-- when it is sent. A row is kept: it is the record of what a person approved (AI-INV-002), like the
-- grants and the model calls, and nothing deletes it.
CREATE TABLE brant_ai.proposal (
    tenant_id      text        NOT NULL,
    company_id     text        NOT NULL,
    proposal_id    text        NOT NULL CHECK (proposal_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    task_id        text        NOT NULL CHECK (task_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    delegation_id  text        NOT NULL,
    agent_id       text        NOT NULL CHECK (agent_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    initiator_id   text        NOT NULL CHECK (initiator_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    owner_team_id  text,
    command        text        NOT NULL CHECK (command ~ '^[a-z][a-z0-9_]*\.[a-z][a-z0-9_.]*$'),
    request_hash   text        NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    object_type    text        NOT NULL CHECK (object_type ~ '^[a-z]+\.[a-z_]+$'),
    object_id      text        NOT NULL CHECK (object_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    object_version bigint      NOT NULL CHECK (object_version >= 0),
    arguments      jsonb       NOT NULL CHECK (jsonb_typeof(arguments) = 'object' AND NOT arguments ? 'IdempotencyKey'
        AND octet_length(arguments::text) <= 8192),
    status         text        NOT NULL CHECK (status IN ('pending', 'sending', 'sent', 'refused', 'void')),
    row_version    bigint      NOT NULL CHECK (row_version > 0),
    attempts       integer     NOT NULL CHECK (attempts > 0),
    sends          integer     NOT NULL CHECK (sends >= 0),
    proposed_at    timestamptz NOT NULL,
    claimed_at     timestamptz,
    grant_id       text        CHECK (grant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    settled_at     timestamptz,
    outcome_code   text        CHECK (outcome_code ~ '^[A-Z][A-Z0-9_]{1,63}$'),
    outcome_path   text        CHECK (outcome_path ~ '^/[A-Za-z0-9_.:~/-]{0,199}$'),
    action_id      text        CHECK (action_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    PRIMARY KEY (tenant_id, company_id, proposal_id),
    FOREIGN KEY (tenant_id, company_id, delegation_id) REFERENCES brant_ai.delegation (tenant_id, company_id, delegation_id),
    CHECK (CASE status
        WHEN 'pending' THEN settled_at IS NULL AND outcome_code IS NULL AND outcome_path IS NULL AND action_id IS NULL
        WHEN 'sending' THEN claimed_at IS NOT NULL AND grant_id IS NOT NULL AND sends > 0
            AND settled_at IS NULL AND outcome_code IS NULL AND outcome_path IS NULL AND action_id IS NULL
        WHEN 'sent' THEN settled_at IS NOT NULL AND action_id IS NOT NULL AND outcome_code IS NULL AND outcome_path IS NULL
        ELSE settled_at IS NOT NULL AND outcome_code IS NOT NULL END)
);
CREATE INDEX proposal_task ON brant_ai.proposal (tenant_id, company_id, task_id, proposed_at);

-- What may change of a proposal, one step at a time at the next row version:
--   pending -> sending         a runner claims it for a grant (sends + 1);
--   sending -> sending         another runner takes over a claim that went stale (sends + 1);
--   sending -> pending         the target did not answer; the same request is sent again;
--   sending -> sent|refused|void   what became of it;
--   pending -> sent|void       the target's ledger already held the committed request, or the delegation is gone;
--   refused|void -> pending    the agent proposed it again and the owning context accepted it
--                              (attempts + 1; the delegation may be another).
-- Its request, object, task, agent and arguments never change, and a proposal is never deleted.
CREATE FUNCTION brant_ai.guard_proposal() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    claimed constant text[] := ARRAY['status', 'row_version', 'sends', 'claimed_at', 'grant_id'];
    released constant text[] := ARRAY['status', 'row_version', 'claimed_at', 'grant_id'];
    settled constant text[] := ARRAY['status', 'row_version', 'settled_at', 'outcome_code', 'outcome_path', 'action_id'];
    rearmed constant text[] := ARRAY['status', 'row_version', 'attempts', 'sends', 'claimed_at', 'grant_id', 'settled_at', 'outcome_code',
        'outcome_path', 'action_id', 'delegation_id', 'initiator_id', 'owner_team_id'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_ai.proposal: proposals are kept, never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'pending' OR NEW.row_version <> 1 OR NEW.attempts <> 1 OR NEW.sends <> 0 THEN
            RAISE EXCEPTION 'brant_ai.proposal: a proposal is made pending at version 1, attempt 1, sent 0 times' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.row_version = OLD.row_version + 1 THEN
        IF OLD.status = 'pending' AND NEW.status = 'sending' AND NEW.sends = OLD.sends + 1
                AND (to_jsonb(OLD) - claimed) = (to_jsonb(NEW) - claimed) THEN
            RETURN NEW;
        END IF;
        IF OLD.status = 'sending' AND NEW.status = 'sending' AND NEW.sends = OLD.sends + 1 AND NEW.claimed_at > OLD.claimed_at
                AND (to_jsonb(OLD) - claimed) = (to_jsonb(NEW) - claimed) THEN
            RETURN NEW;
        END IF;
        IF OLD.status = 'sending' AND NEW.status = 'pending' AND (to_jsonb(OLD) - released) = (to_jsonb(NEW) - released) THEN
            RETURN NEW;
        END IF;
        IF OLD.status = 'sending' AND NEW.status IN ('sent', 'refused', 'void') AND (to_jsonb(OLD) - settled) = (to_jsonb(NEW) - settled) THEN
            RETURN NEW;
        END IF;
        IF OLD.status = 'pending' AND NEW.status IN ('sent', 'void') AND (to_jsonb(OLD) - settled) = (to_jsonb(NEW) - settled) THEN
            RETURN NEW;
        END IF;
        IF OLD.status IN ('refused', 'void') AND NEW.status = 'pending' AND NEW.attempts = OLD.attempts + 1 AND NEW.sends = 0
                AND (to_jsonb(OLD) - rearmed) = (to_jsonb(NEW) - rearmed) THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'brant_ai.proposal: proposal % is % and does not go to % this way; its request never changes', OLD.proposal_id, OLD.status,
        NEW.status USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER proposal_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ai.proposal
    FOR EACH ROW EXECUTE FUNCTION brant_ai.guard_proposal();

ALTER TABLE brant_ai.proposal ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ai.proposal FORCE ROW LEVEL SECURITY;
CREATE POLICY proposal_scope ON brant_ai.proposal
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
