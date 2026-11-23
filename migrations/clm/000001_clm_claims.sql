-- CLM: claims, returns and remedies (tactical model CLM-AGG-ClaimCase, SM-CLM-CLAIM,
-- CLM-INV-001..003; Context Map R34 FUL ShipmentLineRef v1, R35 CreditRequest v1; WBS S6-03;
-- S6-D03).
--
-- ClaimCase is the only aggregate root. A return and a remedy are its entities, so locking
-- claim_case FOR UPDATE serializes every transition of that claim. The state is only open or
-- closed: claim.open inserts it open, return.register and remedy.decide leave it open, and
-- claim.close is the only update that closes it.
--
-- The claim stores the confirmed shipment line it was opened against. Nothing here updates
-- brant_ful, and a return does not write stock (CLM-INV-001, CLM-INV-002). shipment_line_claim_lock
-- is one row per shipment line. return.register and remedy.decide lock the claim root first and
-- that row second, so two claims of one line cannot both pass the return cap.
--
-- A claim whose remedy is reject does not count toward the cap. Every other claim does, including
-- one that has no remedy yet and one that is closed. The sum is judged in the command; the tables
-- keep the facts.
--
-- References to FUL, EVD and POL are plain IDs: no foreign key crosses a schema.
-- action_result and audit_event are CLM's command ledger (internal/foundation/action).

CREATE SCHEMA brant_clm;

CREATE TABLE brant_clm.shipment_line_claim_lock (
    tenant_id        text   NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id       text   NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    shipment_id      text   NOT NULL CHECK (shipment_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    shipment_line_id text   NOT NULL CHECK (shipment_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    -- FOR UPDATE needs an UPDATE privilege. The trigger refuses every change of the row.
    lock_token       bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, company_id, shipment_id, shipment_line_id)
);

CREATE TABLE brant_clm.claim_case (
    tenant_id           text          NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id          text          NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    claim_id            text          NOT NULL CHECK (claim_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status              text          NOT NULL CHECK (status IN ('open', 'closed')),
    row_version         bigint        NOT NULL CHECK (row_version > 0),
    shipment_id         text          NOT NULL CHECK (shipment_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    shipment_line_id    text          NOT NULL CHECK (shipment_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    shipment_version    bigint        NOT NULL CHECK (shipment_version > 0),
    order_id            text          NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_revision      bigint        NOT NULL CHECK (order_revision > 0),
    order_hash          text          NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    customer_party_id   text          NOT NULL CHECK (customer_party_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_line_id       text          NOT NULL CHECK (order_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    sku_id              text          NOT NULL CHECK (sku_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    spec_revision       bigint        NOT NULL CHECK (spec_revision > 0),
    unit_id             text          NOT NULL CHECK (unit_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    unit_revision       bigint        NOT NULL CHECK (unit_revision > 0),
    shipped_quantity    numeric(18,6) NOT NULL CHECK (shipped_quantity > 0),
    currency_code       text          NOT NULL CHECK (currency_code ~ '^[A-Z]{3}$'),
    currency_revision   bigint        NOT NULL CHECK (currency_revision > 0),
    minor_units         smallint      NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    reason              text          NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    evidence_ids        text[]        NOT NULL CHECK (cardinality(evidence_ids) <= 20),
    request_hash        text          NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    owner_actor_id      text          NOT NULL,
    owner_team_id       text,
    opened_by_kind      text          NOT NULL CHECK (opened_by_kind IN ('human', 'ai')),
    opened_by_id        text          NOT NULL,
    open_epoch          bigint        NOT NULL CHECK (open_epoch > 0),
    opened_at           timestamptz   NOT NULL,
    closed_summary      text,
    closed_by_kind      text,
    closed_by_id        text,
    close_epoch         bigint,
    closed_at           timestamptz,
    PRIMARY KEY (tenant_id, company_id, claim_id),
    CONSTRAINT claim_case_closure CHECK (
        (status = 'open' AND closed_at IS NULL AND closed_by_kind IS NULL AND closed_by_id IS NULL
            AND close_epoch IS NULL AND closed_summary IS NULL)
        OR
        (status = 'closed' AND closed_at IS NOT NULL AND closed_by_kind = 'human' AND closed_by_id IS NOT NULL
            AND close_epoch > 0 AND char_length(closed_summary) BETWEEN 1 AND 1000)
    )
);

CREATE INDEX claim_case_shipment_line ON brant_clm.claim_case (tenant_id, company_id, shipment_id, shipment_line_id, claim_id);
CREATE INDEX claim_case_order ON brant_clm.claim_case (tenant_id, company_id, order_id, claim_id);

CREATE TABLE brant_clm.claim_return (
    tenant_id          text          NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id         text          NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    claim_id           text          NOT NULL CHECK (claim_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    return_id          text          NOT NULL CHECK (return_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    quantity           numeric(18,6) NOT NULL CHECK (quantity > 0),
    condition          text          NOT NULL CHECK (char_length(condition) BETWEEN 1 AND 200),
    note               text          NOT NULL DEFAULT '' CHECK (char_length(note) <= 1000),
    evidence_ids       text[]        NOT NULL CHECK (cardinality(evidence_ids) <= 20),
    request_hash       text          NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    registered_by_kind text          NOT NULL CHECK (registered_by_kind IN ('human', 'ai')),
    registered_by_id   text          NOT NULL,
    register_epoch     bigint        NOT NULL CHECK (register_epoch > 0),
    registered_at      timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, claim_id, return_id),
    FOREIGN KEY (tenant_id, company_id, claim_id) REFERENCES brant_clm.claim_case (tenant_id, company_id, claim_id)
);

CREATE TABLE brant_clm.claim_remedy (
    tenant_id      text   NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id     text   NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    claim_id       text   NOT NULL CHECK (claim_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    remedy_id      text   NOT NULL CHECK (remedy_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    kind           text   NOT NULL CHECK (kind IN ('credit_note', 'refund', 'reject', 'reshipment_ref', 'rework_ref')),
    amount_minor   text   NOT NULL CHECK (amount_minor ~ '^(0|[1-9][0-9]{0,23})$'),
    currency_code  text   NOT NULL CHECK (currency_code ~ '^[A-Z]{3}$'),
    reason         text   NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    request_hash   text   NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    decided_by_kind text  NOT NULL CHECK (decided_by_kind = 'human'),
    decided_by_id  text   NOT NULL,
    decide_epoch   bigint NOT NULL CHECK (decide_epoch > 0),
    decided_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, claim_id),
    CONSTRAINT claim_remedy_remedy_id UNIQUE (tenant_id, company_id, remedy_id),
    FOREIGN KEY (tenant_id, company_id, claim_id) REFERENCES brant_clm.claim_case (tenant_id, company_id, claim_id),
    CONSTRAINT claim_remedy_amount CHECK (
        (kind IN ('credit_note', 'refund') AND amount_minor <> '0')
        OR (kind IN ('reject', 'reshipment_ref', 'rework_ref') AND amount_minor = '0')
    )
);

-- A claim's anchor never changes. A return, a remedy and the line lock never change and are never
-- deleted. A closed claim does not change. A version moves by one.
CREATE FUNCTION brant_clm.guard_mutation() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_clm: % is not deleted', TG_TABLE_NAME;
    END IF;
    IF TG_TABLE_NAME <> 'claim_case' THEN
        RAISE EXCEPTION 'brant_clm: % does not change', TG_TABLE_NAME;
    END IF;
    IF OLD.status = 'closed' OR NEW.row_version IS DISTINCT FROM OLD.row_version + 1 THEN
        RAISE EXCEPTION 'brant_clm: a closed claim does not change and a version moves by one';
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.company_id IS DISTINCT FROM OLD.company_id
        OR NEW.claim_id IS DISTINCT FROM OLD.claim_id
        OR NEW.shipment_id IS DISTINCT FROM OLD.shipment_id
        OR NEW.shipment_line_id IS DISTINCT FROM OLD.shipment_line_id
        OR NEW.shipment_version IS DISTINCT FROM OLD.shipment_version
        OR NEW.order_id IS DISTINCT FROM OLD.order_id
        OR NEW.order_revision IS DISTINCT FROM OLD.order_revision
        OR NEW.order_hash IS DISTINCT FROM OLD.order_hash
        OR NEW.customer_party_id IS DISTINCT FROM OLD.customer_party_id
        OR NEW.order_line_id IS DISTINCT FROM OLD.order_line_id
        OR NEW.sku_id IS DISTINCT FROM OLD.sku_id
        OR NEW.spec_revision IS DISTINCT FROM OLD.spec_revision
        OR NEW.unit_id IS DISTINCT FROM OLD.unit_id
        OR NEW.unit_revision IS DISTINCT FROM OLD.unit_revision
        OR NEW.shipped_quantity IS DISTINCT FROM OLD.shipped_quantity
        OR NEW.currency_code IS DISTINCT FROM OLD.currency_code
        OR NEW.currency_revision IS DISTINCT FROM OLD.currency_revision
        OR NEW.minor_units IS DISTINCT FROM OLD.minor_units
        OR NEW.reason IS DISTINCT FROM OLD.reason
        OR NEW.evidence_ids IS DISTINCT FROM OLD.evidence_ids
        OR NEW.request_hash IS DISTINCT FROM OLD.request_hash
        OR NEW.owner_actor_id IS DISTINCT FROM OLD.owner_actor_id
        OR NEW.owner_team_id IS DISTINCT FROM OLD.owner_team_id
        OR NEW.opened_by_kind IS DISTINCT FROM OLD.opened_by_kind
        OR NEW.opened_by_id IS DISTINCT FROM OLD.opened_by_id
        OR NEW.open_epoch IS DISTINCT FROM OLD.open_epoch
        OR NEW.opened_at IS DISTINCT FROM OLD.opened_at
    THEN
        RAISE EXCEPTION 'brant_clm: a claim anchor does not change';
    END IF;
    RETURN NEW;
END
$$;

REVOKE ALL ON FUNCTION brant_clm.guard_mutation() FROM PUBLIC;

CREATE TRIGGER claim_case_mutation BEFORE UPDATE OR DELETE ON brant_clm.claim_case
    FOR EACH ROW EXECUTE FUNCTION brant_clm.guard_mutation();
CREATE TRIGGER claim_return_mutation BEFORE UPDATE OR DELETE ON brant_clm.claim_return
    FOR EACH ROW EXECUTE FUNCTION brant_clm.guard_mutation();
CREATE TRIGGER claim_remedy_mutation BEFORE UPDATE OR DELETE ON brant_clm.claim_remedy
    FOR EACH ROW EXECUTE FUNCTION brant_clm.guard_mutation();
CREATE TRIGGER shipment_line_claim_lock_mutation BEFORE UPDATE OR DELETE ON brant_clm.shipment_line_claim_lock
    FOR EACH ROW EXECUTE FUNCTION brant_clm.guard_mutation();

CREATE TABLE brant_clm.action_result (
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

CREATE TABLE brant_clm.audit_event (
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

ALTER TABLE brant_clm.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_clm.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_clm.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_clm.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_clm.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_clm.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_clm.shipment_line_claim_lock ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_clm.shipment_line_claim_lock FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_line_claim_lock_scope ON brant_clm.shipment_line_claim_lock
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_clm.claim_case ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_clm.claim_case FORCE ROW LEVEL SECURITY;
CREATE POLICY claim_case_scope ON brant_clm.claim_case
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_clm.claim_return ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_clm.claim_return FORCE ROW LEVEL SECURITY;
CREATE POLICY claim_return_scope ON brant_clm.claim_return
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_clm.claim_remedy ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_clm.claim_remedy FORCE ROW LEVEL SECURITY;
CREATE POLICY claim_remedy_scope ON brant_clm.claim_remedy
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
