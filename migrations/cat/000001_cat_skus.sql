-- CAT: tradeable SKUs and their specification revisions (Python BC_T01_reference.sql master kind
-- sku). A draft is saved whole; a human confirms it on evidence it may read, which fixes a new
-- specification revision; sku.revise_spec reopens the draft and sku.deactivate stops the SKU for new
-- business. Every confirmed revision is kept immutable, so a SkuRef (sku_id, spec_revision) always
-- names what was confirmed (CAT-INV-001). Only PCS is a base unit until units are POL configuration.
--
-- action_result and audit_event are the command ledger of CAT (internal/foundation/action).

CREATE SCHEMA brant_cat;

CREATE TABLE brant_cat.sku (
    tenant_id      text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id     text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    sku_id         text        NOT NULL CHECK (sku_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status         text        NOT NULL CHECK (status IN ('draft', 'confirmed', 'inactive')),
    row_version    bigint      NOT NULL CHECK (row_version > 0),
    spec_revision  bigint      NOT NULL CHECK (spec_revision >= 0),
    name           text        CHECK (char_length(name) BETWEEN 1 AND 200),
    base_uom       text        CHECK (base_uom = 'PCS'),
    owner_actor_id text        NOT NULL,
    owner_team_id  text,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, sku_id),
    CHECK (status = 'draft' OR (name IS NOT NULL AND base_uom IS NOT NULL AND spec_revision > 0))
);

CREATE TABLE brant_cat.sku_revision (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    sku_id            text        NOT NULL,
    spec_revision     bigint      NOT NULL CHECK (spec_revision > 0),
    row_version       bigint      NOT NULL CHECK (row_version > 0),
    name              text        NOT NULL,
    base_uom          text        NOT NULL,
    evidence_ids      text[]      NOT NULL CHECK (cardinality(evidence_ids) BETWEEN 1 AND 200),
    confirmed_by_kind text        NOT NULL CHECK (confirmed_by_kind = 'human'),
    confirmed_by_id   text        NOT NULL,
    authority_epoch   bigint      NOT NULL CHECK (authority_epoch > 0),
    confirmed_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, sku_id, spec_revision),
    FOREIGN KEY (tenant_id, company_id, sku_id) REFERENCES brant_cat.sku (tenant_id, company_id, sku_id)
);

-- Content changes only in the draft state; an inactive SKU never changes; revisions never change.
CREATE FUNCTION brant_cat.guard_sku() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status = 'inactive' THEN
        RAISE EXCEPTION 'brant_cat.sku: an inactive SKU does not change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> 'draft' AND (NEW.name IS DISTINCT FROM OLD.name OR NEW.base_uom IS DISTINCT FROM OLD.base_uom
            OR NEW.owner_actor_id IS DISTINCT FROM OLD.owner_actor_id OR NEW.owner_team_id IS DISTINCT FROM OLD.owner_team_id) THEN
        RAISE EXCEPTION 'brant_cat.sku: a confirmed SKU is revised, not edited' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER sku_guard BEFORE UPDATE ON brant_cat.sku FOR EACH ROW EXECUTE FUNCTION brant_cat.guard_sku();

CREATE FUNCTION brant_cat.refuse_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'brant_cat.%: confirmed specification revisions are immutable', TG_TABLE_NAME
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER sku_revision_immutable BEFORE UPDATE OR DELETE ON brant_cat.sku_revision
    FOR EACH ROW EXECUTE FUNCTION brant_cat.refuse_change();

ALTER TABLE brant_cat.sku ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cat.sku FORCE ROW LEVEL SECURITY;
CREATE POLICY sku_scope ON brant_cat.sku
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cat.sku_revision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cat.sku_revision FORCE ROW LEVEL SECURITY;
CREATE POLICY sku_revision_scope ON brant_cat.sku_revision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

CREATE TABLE brant_cat.action_result (
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

CREATE TABLE brant_cat.audit_event (
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

ALTER TABLE brant_cat.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cat.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_cat.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cat.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_cat.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_cat.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
