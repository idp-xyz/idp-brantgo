-- INQ: customer RFQs (Python BC_T01_reference.sql rfq, rfq_line, source_use). A draft is saved
-- whole and may lack anything it does not state (INQ-INV-002); a human confirms its scope only when
-- the customer, the contact, every line and every source are known and current (INQ-INV-001); a
-- ready RFQ is cancelled or closed, never edited. References to PTY, CAT and EVD are plain IDs: no
-- foreign key crosses a schema (DB2). The confirmation records the upstream versions it saw:
-- customer and contact row versions, each line's SKU specification revision, and the evidence.
-- Triggers refuse content changes outside the draft state and any change to a cancelled or closed
-- RFQ; lines and sources change only while their RFQ is a draft.
--
-- action_result and audit_event are the command ledger of INQ (internal/foundation/action).

CREATE SCHEMA brant_inq;

CREATE TABLE brant_inq.customer_rfq (
    tenant_id            text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id           text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    rfq_id               text        NOT NULL CHECK (rfq_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status               text        NOT NULL CHECK (status IN ('draft', 'ready', 'cancelled', 'closed')),
    row_version          bigint      NOT NULL CHECK (row_version > 0),
    customer_party_id    text        CHECK (customer_party_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    contact_id           text        CHECK (contact_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    unresolved_fields    text[]      NOT NULL CHECK (cardinality(unresolved_fields) <= 200),
    owner_actor_id       text        NOT NULL,
    owner_team_id        text,
    customer_row_version bigint,
    contact_row_version  bigint,
    evidence_ids         text[],
    confirmed_by_kind    text        CHECK (confirmed_by_kind = 'human'),
    confirmed_by_id      text,
    authority_epoch      bigint      CHECK (authority_epoch > 0),
    confirmed_at         timestamptz,
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, rfq_id),
    CHECK ((authority_epoch IS NULL) = (confirmed_at IS NULL)),
    CHECK (authority_epoch IS NULL OR (customer_row_version > 0 AND contact_row_version > 0
        AND cardinality(evidence_ids) BETWEEN 1 AND 200 AND confirmed_by_id IS NOT NULL)),
    CHECK (status IN ('draft', 'cancelled') OR authority_epoch IS NOT NULL)
);

CREATE TABLE brant_inq.rfq_line (
    tenant_id         text   NOT NULL,
    company_id        text   NOT NULL,
    rfq_id            text   NOT NULL,
    line_id           text   NOT NULL CHECK (line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    position          int    NOT NULL CHECK (position BETWEEN 0 AND 199),
    sku_id            text   CHECK (sku_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    quantity          text   CHECK (quantity ~ '^(?:(?:[1-9][0-9]{0,5}|0)(?:\.[0-9]{1,6})?|1000000(?:\.0{1,6})?)$'),
    uom               text   CHECK (uom = 'PCS'),
    requested_item    text   CHECK (char_length(requested_item) BETWEEN 1 AND 200),
    source_id         text   CHECK (source_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    locator           text   CHECK (char_length(locator) BETWEEN 1 AND 200),
    sku_spec_revision bigint CHECK (sku_spec_revision > 0),
    PRIMARY KEY (tenant_id, company_id, rfq_id, line_id),
    UNIQUE (tenant_id, company_id, rfq_id, position),
    FOREIGN KEY (tenant_id, company_id, rfq_id) REFERENCES brant_inq.customer_rfq (tenant_id, company_id, rfq_id)
);

CREATE TABLE brant_inq.rfq_source (
    tenant_id  text NOT NULL,
    company_id text NOT NULL,
    rfq_id     text NOT NULL,
    source_id  text NOT NULL CHECK (source_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    PRIMARY KEY (tenant_id, company_id, rfq_id, source_id),
    FOREIGN KEY (tenant_id, company_id, rfq_id) REFERENCES brant_inq.customer_rfq (tenant_id, company_id, rfq_id)
);

CREATE FUNCTION brant_inq.guard_rfq() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status IN ('cancelled', 'closed') THEN
        RAISE EXCEPTION 'brant_inq.customer_rfq: a % RFQ does not change', OLD.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'ready' AND (to_jsonb(NEW) - ARRAY['status', 'row_version', 'updated_at'])
            IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['status', 'row_version', 'updated_at']) THEN
        RAISE EXCEPTION 'brant_inq.customer_rfq: a ready RFQ is not edited (INQ-INV-002)' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER customer_rfq_guard BEFORE UPDATE ON brant_inq.customer_rfq FOR EACH ROW EXECUTE FUNCTION brant_inq.guard_rfq();

CREATE FUNCTION brant_inq.guard_content() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    r record := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM brant_inq.customer_rfq q WHERE q.tenant_id = r.tenant_id AND q.company_id = r.company_id
            AND q.rfq_id = r.rfq_id AND q.status = 'draft') THEN
        RAISE EXCEPTION 'brant_inq.%: lines and sources change only while the RFQ is a draft', TG_TABLE_NAME
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN r;
END
$$;
CREATE TRIGGER rfq_line_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_inq.rfq_line
    FOR EACH ROW EXECUTE FUNCTION brant_inq.guard_content();
CREATE TRIGGER rfq_source_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_inq.rfq_source
    FOR EACH ROW EXECUTE FUNCTION brant_inq.guard_content();

ALTER TABLE brant_inq.customer_rfq ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_inq.customer_rfq FORCE ROW LEVEL SECURITY;
CREATE POLICY customer_rfq_scope ON brant_inq.customer_rfq
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_inq.rfq_line ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_inq.rfq_line FORCE ROW LEVEL SECURITY;
CREATE POLICY rfq_line_scope ON brant_inq.rfq_line
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_inq.rfq_source ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_inq.rfq_source FORCE ROW LEVEL SECURITY;
CREATE POLICY rfq_source_scope ON brant_inq.rfq_source
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

CREATE TABLE brant_inq.action_result (
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

CREATE TABLE brant_inq.audit_event (
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

ALTER TABLE brant_inq.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_inq.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_inq.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_inq.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_inq.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_inq.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
