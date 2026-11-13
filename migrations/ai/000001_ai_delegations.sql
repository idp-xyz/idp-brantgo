-- internal/ai (brant_ai; plan S4 decision D-6, ADR-BR-003): the AI layer's delegations and approval
-- grants (Context Map platformCommands AI, MD-48; AI-INV-002) and the ledger of its four human-only
-- commands ai.delegation.grant/revoke and ai.approval_grant.issue/revoke. The model-call ledger and
-- ExecutionBundles follow in 000002 (unit C2).
--
-- A delegation lets one AI execution identity call the listed capabilities for one task, up to an
-- autonomy level, until it expires; the human initiator who granted it must still hold each
-- delegated permission whenever the agent acts. An approval grant lets the agent of one delegation
-- make exactly one call: the command, its request hash, the object and its version, before the
-- expiry; its use records the idempotency key of that call, so a resend of the same call is the same
-- use. Neither is an approval of a quote, an order or a purchase commitment.

CREATE SCHEMA brant_ai;

CREATE TABLE brant_ai.delegation (
    tenant_id     text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id    text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    delegation_id text        NOT NULL CHECK (delegation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    initiator_id  text        NOT NULL CHECK (initiator_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    agent_id      text        NOT NULL CHECK (agent_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    task_id       text        NOT NULL CHECK (task_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    autonomy      text        NOT NULL CHECK (autonomy IN ('read', 'draft', 'submit')),
    capabilities  text[]      NOT NULL CHECK (cardinality(capabilities) BETWEEN 1 AND 64 AND array_position(capabilities, NULL) IS NULL),
    expires_at    timestamptz NOT NULL,
    status        text        NOT NULL CHECK (status IN ('active', 'revoked')),
    row_version   bigint      NOT NULL CHECK (row_version > 0),
    owner_team_id text,
    granted_epoch bigint      NOT NULL CHECK (granted_epoch > 0),
    granted_at    timestamptz NOT NULL,
    revoked_by_id text,
    revoked_epoch bigint      CHECK (revoked_epoch > 0),
    revoked_at    timestamptz,
    revoke_reason text        CHECK (char_length(revoke_reason) BETWEEN 1 AND 500),
    PRIMARY KEY (tenant_id, company_id, delegation_id),
    CHECK (initiator_id <> agent_id),
    CHECK (expires_at > granted_at),
    CHECK (CASE WHEN status = 'revoked' THEN revoked_by_id IS NOT NULL AND revoked_epoch IS NOT NULL AND revoked_at IS NOT NULL
        AND revoke_reason IS NOT NULL
        ELSE revoked_by_id IS NULL AND revoked_epoch IS NULL AND revoked_at IS NULL AND revoke_reason IS NULL END)
);
CREATE INDEX delegation_task ON brant_ai.delegation (tenant_id, company_id, task_id, agent_id);

CREATE TABLE brant_ai.approval_grant (
    tenant_id      text        NOT NULL,
    company_id     text        NOT NULL,
    grant_id       text        NOT NULL CHECK (grant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    delegation_id  text        NOT NULL,
    agent_id       text        NOT NULL,
    task_id        text        NOT NULL,
    command        text        NOT NULL CHECK (command ~ '^[a-z][a-z0-9_]*\.[a-z][a-z0-9_.]*$'),
    request_hash   text        NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    object_type    text        NOT NULL CHECK (object_type ~ '^[a-z]+\.[a-z_]+$'),
    object_id      text        NOT NULL CHECK (object_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    object_version bigint      NOT NULL CHECK (object_version >= 0),
    expires_at     timestamptz NOT NULL,
    status         text        NOT NULL CHECK (status IN ('issued', 'used', 'revoked')),
    row_version    bigint      NOT NULL CHECK (row_version > 0),
    issuer_id      text        NOT NULL CHECK (issuer_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    owner_team_id  text,
    issued_epoch   bigint      NOT NULL CHECK (issued_epoch > 0),
    issued_at      timestamptz NOT NULL,
    used_key       text        CHECK (used_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    used_by_id     text,
    used_epoch     bigint      CHECK (used_epoch > 0),
    used_at        timestamptz,
    revoked_by_id  text,
    revoked_epoch  bigint      CHECK (revoked_epoch > 0),
    revoked_at     timestamptz,
    revoke_reason  text        CHECK (char_length(revoke_reason) BETWEEN 1 AND 500),
    PRIMARY KEY (tenant_id, company_id, grant_id),
    FOREIGN KEY (tenant_id, company_id, delegation_id) REFERENCES brant_ai.delegation (tenant_id, company_id, delegation_id),
    CHECK (expires_at > issued_at),
    CHECK (CASE WHEN status = 'used' THEN used_key IS NOT NULL AND used_by_id IS NOT NULL AND used_epoch IS NOT NULL AND used_at IS NOT NULL
        ELSE used_key IS NULL AND used_by_id IS NULL AND used_epoch IS NULL AND used_at IS NULL END),
    CHECK (CASE WHEN status = 'revoked' THEN revoked_by_id IS NOT NULL AND revoked_epoch IS NOT NULL AND revoked_at IS NOT NULL
        AND revoke_reason IS NOT NULL
        ELSE revoked_by_id IS NULL AND revoked_epoch IS NULL AND revoked_at IS NULL AND revoke_reason IS NULL END)
);
CREATE INDEX approval_grant_request ON brant_ai.approval_grant (tenant_id, company_id, delegation_id, command, request_hash);

-- A delegation is granted active and revoked once; nothing else of it changes. A grant is issued
-- for a command its active delegation lists, by an agent and task that are the delegation's, and is
-- then used once or revoked once; its binding never changes. Neither is deleted.
CREATE FUNCTION brant_ai.guard_delegation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    revoked constant text[] := ARRAY['status', 'row_version', 'revoked_by_id', 'revoked_epoch', 'revoked_at', 'revoke_reason'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_ai.delegation: delegations are revoked, never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'active' OR NEW.row_version <> 1 THEN
            RAISE EXCEPTION 'brant_ai.delegation: a delegation is granted active at version 1' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'active' AND NEW.status = 'revoked' AND NEW.row_version = OLD.row_version + 1
            AND (to_jsonb(OLD) - revoked) = (to_jsonb(NEW) - revoked) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_ai.delegation: delegation % is %; it is revoked once and its binding never changes', OLD.delegation_id, OLD.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER delegation_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ai.delegation
    FOR EACH ROW EXECUTE FUNCTION brant_ai.guard_delegation();

CREATE FUNCTION brant_ai.guard_approval_grant() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    used constant text[] := ARRAY['status', 'row_version', 'used_key', 'used_by_id', 'used_epoch', 'used_at'];
    revoked constant text[] := ARRAY['status', 'row_version', 'revoked_by_id', 'revoked_epoch', 'revoked_at', 'revoke_reason'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_ai.approval_grant: grants are used or revoked, never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'issued' OR NEW.row_version <> 1 OR NOT EXISTS (SELECT 1 FROM brant_ai.delegation d
                WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND d.delegation_id = NEW.delegation_id
                AND d.status = 'active' AND d.agent_id = NEW.agent_id AND d.task_id = NEW.task_id
                AND NEW.command = ANY (d.capabilities)) THEN
            RAISE EXCEPTION 'brant_ai.approval_grant: a grant is issued at version 1 for a command its active delegation lists, to its agent and task'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'issued' AND NEW.status = 'used' AND NEW.row_version = OLD.row_version + 1
            AND (to_jsonb(OLD) - used) = (to_jsonb(NEW) - used) THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'issued' AND NEW.status = 'revoked' AND NEW.row_version = OLD.row_version + 1
            AND (to_jsonb(OLD) - revoked) = (to_jsonb(NEW) - revoked) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_ai.approval_grant: grant % is %; it is used or revoked once and its binding never changes', OLD.grant_id, OLD.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER approval_grant_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ai.approval_grant
    FOR EACH ROW EXECUTE FUNCTION brant_ai.guard_approval_grant();

ALTER TABLE brant_ai.delegation ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ai.delegation FORCE ROW LEVEL SECURITY;
CREATE POLICY delegation_scope ON brant_ai.delegation
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ai.approval_grant ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ai.approval_grant FORCE ROW LEVEL SECURITY;
CREATE POLICY approval_grant_scope ON brant_ai.approval_grant
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

-- The command ledger of internal/ai (internal/foundation/action, ResultsDDL).
CREATE TABLE brant_ai.action_result (
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

CREATE TABLE brant_ai.audit_event (
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

ALTER TABLE brant_ai.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ai.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_ai.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ai.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_ai.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_ai.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
