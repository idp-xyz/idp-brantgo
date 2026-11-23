-- CRM (S6-04, S6-D04): optional leads, opportunities and follow-ups. A lead is appended by
-- lead.capture and never revised. An opportunity is created at stage qualified and moves only
-- along SM-CRM-OPPORTUNITY (stage and row_version); its identity, title and expected amount do
-- not change. A follow-up is a child of the opportunity, appended under the opportunity row lock,
-- and is not a business task. No foreign key leaves this schema (DB2). INQ cites an opportunity
-- by a plain id (R40) and does not lock it.
--
-- action_result and audit_event are the command ledger of CRM (internal/foundation/action).
-- Integration events go to the shared Bento outbox, as they do for every other context.

CREATE SCHEMA brant_crm;

CREATE TABLE brant_crm.lead (
    tenant_id      text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id     text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    lead_id        text        NOT NULL CHECK (lead_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    source_channel text        NOT NULL CHECK (char_length(source_channel) BETWEEN 1 AND 200),
    company_name   text        NOT NULL CHECK (char_length(company_name) BETWEEN 1 AND 200),
    contact_name   text        NOT NULL CHECK (char_length(contact_name) BETWEEN 1 AND 200),
    summary        text        NOT NULL CHECK (char_length(summary) BETWEEN 1 AND 200),
    status         text        NOT NULL CHECK (status = 'captured'),
    row_version    bigint      NOT NULL CHECK (row_version > 0),
    owner_actor_id text        NOT NULL,
    owner_team_id  text,
    created_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, lead_id)
);

CREATE TABLE brant_crm.opportunity (
    tenant_id             text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id            text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    opportunity_id        text        NOT NULL CHECK (opportunity_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    lead_id               text        CHECK (lead_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    customer_id           text        CHECK (customer_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    title                 text        NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
    stage                 text        NOT NULL CHECK (stage IN ('qualified', 'proposal', 'negotiation', 'won', 'lost', 'abandoned')),
    expected_amount_minor bigint,
    currency              text,
    row_version           bigint      NOT NULL CHECK (row_version > 0),
    owner_actor_id        text        NOT NULL,
    owner_team_id         text,
    created_at            timestamptz NOT NULL,
    updated_at            timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, opportunity_id),
    CHECK ((expected_amount_minor IS NULL) = (currency IS NULL)),
    CHECK (expected_amount_minor IS NULL OR expected_amount_minor > 0),
    CHECK (currency IS NULL OR currency ~ '^[A-Z]{3}$'),
    FOREIGN KEY (tenant_id, company_id, lead_id) REFERENCES brant_crm.lead (tenant_id, company_id, lead_id)
);

CREATE TABLE brant_crm.follow_up (
    tenant_id      text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id     text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    follow_up_id   text        NOT NULL CHECK (follow_up_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    opportunity_id text        NOT NULL CHECK (opportunity_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    summary        text        NOT NULL CHECK (char_length(summary) BETWEEN 1 AND 200),
    next_step      text        NOT NULL CHECK (char_length(next_step) <= 200),
    recorded_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, follow_up_id),
    FOREIGN KEY (tenant_id, company_id, opportunity_id)
        REFERENCES brant_crm.opportunity (tenant_id, company_id, opportunity_id)
);

CREATE INDEX follow_up_opportunity ON brant_crm.follow_up (tenant_id, company_id, opportunity_id, recorded_at, follow_up_id);

-- A lead is append-only. SELECT FOR UPDATE does not fire this trigger.
CREATE FUNCTION brant_crm.guard_lead() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'brant_crm.lead: a lead is not revised' USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER lead_guard BEFORE UPDATE OR DELETE ON brant_crm.lead
    FOR EACH ROW EXECUTE FUNCTION brant_crm.guard_lead();

-- Identity, title and the expected amount stay as captured. The stage moves and the row version
-- only increases. The domain decides which stage succeeds which.
CREATE FUNCTION brant_crm.guard_opportunity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.lead_id IS DISTINCT FROM OLD.lead_id
        OR NEW.customer_id IS DISTINCT FROM OLD.customer_id
        OR NEW.title IS DISTINCT FROM OLD.title
        OR NEW.expected_amount_minor IS DISTINCT FROM OLD.expected_amount_minor
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.owner_actor_id IS DISTINCT FROM OLD.owner_actor_id
        OR NEW.owner_team_id IS DISTINCT FROM OLD.owner_team_id
        OR NEW.row_version <= OLD.row_version THEN
        RAISE EXCEPTION 'brant_crm.opportunity: only the stage and the row version change'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER opportunity_guard BEFORE UPDATE ON brant_crm.opportunity
    FOR EACH ROW EXECUTE FUNCTION brant_crm.guard_opportunity();

CREATE FUNCTION brant_crm.guard_opportunity_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'brant_crm.opportunity: an opportunity is not deleted' USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER opportunity_delete_guard BEFORE DELETE ON brant_crm.opportunity
    FOR EACH ROW EXECUTE FUNCTION brant_crm.guard_opportunity_delete();

CREATE FUNCTION brant_crm.guard_follow_up() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'brant_crm.follow_up: a follow-up is not revised' USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER follow_up_guard BEFORE UPDATE OR DELETE ON brant_crm.follow_up
    FOR EACH ROW EXECUTE FUNCTION brant_crm.guard_follow_up();

ALTER TABLE brant_crm.lead ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_crm.lead FORCE ROW LEVEL SECURITY;
CREATE POLICY lead_scope ON brant_crm.lead
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_crm.opportunity ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_crm.opportunity FORCE ROW LEVEL SECURITY;
CREATE POLICY opportunity_scope ON brant_crm.opportunity
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_crm.follow_up ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_crm.follow_up FORCE ROW LEVEL SECURITY;
CREATE POLICY follow_up_scope ON brant_crm.follow_up
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

CREATE TABLE brant_crm.action_result (
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

CREATE TABLE brant_crm.audit_event (
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

ALTER TABLE brant_crm.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_crm.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_crm.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_crm.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_crm.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_crm.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
