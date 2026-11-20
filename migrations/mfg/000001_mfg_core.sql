-- MFG: company enablement, collaboration plans and subcontract orders (MFG-AGG-CollaborationPlan,
-- MFG-AGG-SubcontractOrder; SM-MFG-PLAN, SM-MFG-SUBCONTRACT; MFG-INV-001, MFG-INV-002; Context Map
-- R23 SubcontractDemand v1, R25 InspectionSubject v1, R42 MaterialHandover v1; S6-D02; WBS S6-02).
--
-- The company switch defaults to off: a company with no row, or with enabled false, has no
-- operational command. mfg.module.configure is the only writer of company_setting.
--
-- CollaborationPlan is one root. Operation tasks and material responsibilities are its children,
-- so locking the plan FOR UPDATE serializes every transition. collaboration_plan.publish is the
-- only command that moves it. A draft's children may be replaced; once the plan leaves draft they
-- stay. operation.record_progress does not update the plan.
--
-- SubcontractOrder is the other root. Issue creates it issued. Progress claims are append-only
-- source claims: they do not write stock and they do not release quality (MFG-INV-001). A material
-- handover records the association to an STK movement. The four kinds stay distinct (MFG-INV-002).
-- STK is called by the composition root before this transaction, so this schema locks no stock row.
--
-- References to PUR, STK and QLT are plain ids: no foreign key crosses a schema.
-- action_result and audit_event are MFG's command ledger (internal/foundation/action).

CREATE SCHEMA brant_mfg;

CREATE TABLE brant_mfg.company_setting (
    tenant_id       text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id      text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    enabled         boolean     NOT NULL,
    row_version     bigint      NOT NULL CHECK (row_version > 0),
    updated_by_kind text        NOT NULL CHECK (updated_by_kind = 'human'),
    updated_by_id   text        NOT NULL CHECK (updated_by_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    update_epoch    bigint      NOT NULL CHECK (update_epoch > 0),
    updated_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id)
);

CREATE TABLE brant_mfg.collaboration_plan (
    tenant_id        text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id       text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    plan_id          text        NOT NULL CHECK (plan_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status           text        NOT NULL CHECK (status IN ('draft', 'published', 'in_progress', 'paused', 'completed', 'cancelled')),
    row_version      bigint      NOT NULL CHECK (row_version > 0),
    factory_party_id text        NOT NULL CHECK (factory_party_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    note             text        NOT NULL DEFAULT '' CHECK (char_length(note) <= 1000),
    request_hash     text        NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    owner_actor_id   text        NOT NULL CHECK (owner_actor_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    owner_team_id    text        CHECK (owner_team_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    opened_by_kind   text        NOT NULL CHECK (opened_by_kind IN ('human', 'ai')),
    opened_by_id     text        NOT NULL CHECK (opened_by_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    open_epoch       bigint      NOT NULL CHECK (open_epoch > 0),
    opened_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, plan_id)
);

CREATE TABLE brant_mfg.operation_task (
    tenant_id     text          NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id    text          NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    plan_id       text          NOT NULL CHECK (plan_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    operation_id  text          NOT NULL CHECK (operation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    name          text          NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
    sequence      bigint        NOT NULL CHECK (sequence > 0),
    sku_id        text          NOT NULL CHECK (sku_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    spec_revision bigint        NOT NULL CHECK (spec_revision > 0),
    quantity      numeric(18,6) NOT NULL CHECK (quantity > 0),
    unit_id       text          NOT NULL CHECK (unit_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    unit_revision bigint        NOT NULL CHECK (unit_revision > 0),
    PRIMARY KEY (tenant_id, company_id, plan_id, operation_id),
    UNIQUE (tenant_id, company_id, plan_id, sequence),
    FOREIGN KEY (tenant_id, company_id, plan_id) REFERENCES brant_mfg.collaboration_plan (tenant_id, company_id, plan_id)
);

CREATE TABLE brant_mfg.material_responsibility (
    tenant_id     text          NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id    text          NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    plan_id       text          NOT NULL CHECK (plan_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    material_id   text          NOT NULL CHECK (material_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    kind          text          NOT NULL CHECK (kind IN ('factory_purchased', 'our_supply', 'customer_supply', 'mixed')),
    sku_id        text          NOT NULL CHECK (sku_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    quantity      numeric(18,6) NOT NULL CHECK (quantity > 0),
    unit_id       text          NOT NULL CHECK (unit_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    unit_revision bigint        NOT NULL CHECK (unit_revision > 0),
    loss_basis    text          NOT NULL DEFAULT '' CHECK (char_length(loss_basis) <= 200),
    PRIMARY KEY (tenant_id, company_id, plan_id, material_id),
    FOREIGN KEY (tenant_id, company_id, plan_id) REFERENCES brant_mfg.collaboration_plan (tenant_id, company_id, plan_id)
);

CREATE TABLE brant_mfg.subcontract_order (
    tenant_id            text          NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id           text          NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    subcontract_id       text          NOT NULL CHECK (subcontract_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status               text          NOT NULL CHECK (status IN ('issued', 'in_progress', 'completed', 'cancelled')),
    row_version          bigint        NOT NULL CHECK (row_version > 0),
    plan_id              text          NOT NULL CHECK (plan_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    operation_id         text          NOT NULL CHECK (operation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    quantity             numeric(18,6) NOT NULL CHECK (quantity > 0),
    unit_id              text          NOT NULL CHECK (unit_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    unit_revision        bigint        NOT NULL CHECK (unit_revision > 0),
    currency_code        text          NOT NULL CHECK (currency_code ~ '^[A-Z]{3}$'),
    minor_units          smallint      NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    processing_fee_minor text          NOT NULL CHECK (processing_fee_minor ~ '^(0|[1-9][0-9]{0,23})$'),
    factory_party_id     text          NOT NULL CHECK (factory_party_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    sku_id               text          NOT NULL CHECK (sku_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    spec_revision        bigint        NOT NULL CHECK (spec_revision > 0),
    commitment_id        text          CHECK (commitment_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_id             text          NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_line_id        text          NOT NULL CHECK (order_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_revision       bigint        NOT NULL CHECK (order_revision > 0),
    order_hash           text          NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    subject_hash         text          NOT NULL CHECK (subject_hash ~ '^[0-9a-f]{64}$'),
    issue_hash           text          NOT NULL CHECK (issue_hash ~ '^[0-9a-f]{64}$'),
    request_hash         text          NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    owner_actor_id       text          NOT NULL CHECK (owner_actor_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    owner_team_id        text          CHECK (owner_team_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    opened_by_kind       text          NOT NULL CHECK (opened_by_kind = 'human'),
    opened_by_id         text          NOT NULL CHECK (opened_by_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    open_epoch           bigint        NOT NULL CHECK (open_epoch > 0),
    opened_at            timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, subcontract_id)
);

CREATE INDEX subcontract_order_plan ON brant_mfg.subcontract_order (tenant_id, company_id, plan_id, operation_id);

CREATE TABLE brant_mfg.operation_progress (
    tenant_id       text          NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id      text          NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    subcontract_id  text          NOT NULL CHECK (subcontract_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    progress_id     text          NOT NULL CHECK (progress_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    operation_id    text          NOT NULL CHECK (operation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    progress_kind   text          NOT NULL CHECK (progress_kind IN ('in_progress', 'completed', 'cancelled')),
    quantity        numeric(18,6) NOT NULL CHECK (quantity > 0),
    unit_id         text          NOT NULL CHECK (unit_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    reported_at     timestamptz   NOT NULL,
    request_hash    text          NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    actor_kind      text          NOT NULL CHECK (actor_kind IN ('human', 'ai')),
    actor_id        text          NOT NULL CHECK (actor_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    authority_epoch bigint        NOT NULL CHECK (authority_epoch > 0),
    PRIMARY KEY (tenant_id, company_id, subcontract_id, progress_id),
    FOREIGN KEY (tenant_id, company_id, subcontract_id) REFERENCES brant_mfg.subcontract_order (tenant_id, company_id, subcontract_id)
);

CREATE TABLE brant_mfg.material_handover (
    tenant_id           text          NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id          text          NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    movement_id         text          NOT NULL CHECK (movement_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    subcontract_id      text          NOT NULL CHECK (subcontract_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    kind                text          NOT NULL CHECK (kind IN ('handover_transfer', 'handover_consume', 'handover_return', 'handover_scrap')),
    counter_movement_id text          CHECK (counter_movement_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    sku_id              text          NOT NULL CHECK (sku_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    location_id         text          NOT NULL CHECK (location_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    batch_id            text          NOT NULL CHECK (batch_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    custody_kind        text          NOT NULL CHECK (custody_kind IN ('own_warehouse', 'external_factory_material', 'external_factory_wip', 'in_transit')),
    to_location_id      text          CHECK (to_location_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    to_batch_id         text          CHECK (to_batch_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    to_custody_kind     text          CHECK (to_custody_kind IN ('own_warehouse', 'external_factory_material', 'external_factory_wip', 'in_transit')),
    quantity            numeric(18,6) NOT NULL CHECK (quantity > 0),
    unit_id             text          NOT NULL CHECK (unit_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    unit_revision       bigint        NOT NULL CHECK (unit_revision > 0),
    reason              text          NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    request_hash        text          NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    owner_actor_id      text          NOT NULL CHECK (owner_actor_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    owner_team_id       text          CHECK (owner_team_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    recorded_at         timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, movement_id),
    FOREIGN KEY (tenant_id, company_id, subcontract_id) REFERENCES brant_mfg.subcontract_order (tenant_id, company_id, subcontract_id),
    CHECK (
        (kind IN ('handover_consume', 'handover_scrap')
            AND counter_movement_id IS NULL AND to_location_id IS NULL AND to_batch_id IS NULL AND to_custody_kind IS NULL)
        OR
        (kind IN ('handover_transfer', 'handover_return')
            AND counter_movement_id IS NOT NULL AND counter_movement_id <> movement_id
            AND to_location_id IS NOT NULL AND to_batch_id IS NOT NULL AND to_custody_kind IS NOT NULL
            AND (to_location_id, to_batch_id, to_custody_kind) IS DISTINCT FROM (location_id, batch_id, custody_kind))
    )
);

-- A plan's identity does not change. A version moves by one. A note changes only while the plan
-- is still draft. Children are not updated. They are deleted only while the plan is draft.
-- A subcontract's identity does not change. A progress claim and a handover are append-only.
CREATE FUNCTION brant_mfg.guard_mutation() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF TG_TABLE_NAME IN ('operation_progress', 'material_handover') THEN
        RAISE EXCEPTION 'brant_mfg: % is append-only', TG_TABLE_NAME;
    END IF;
    IF TG_TABLE_NAME IN ('operation_task', 'material_responsibility') THEN
        IF TG_OP = 'UPDATE' THEN
            RAISE EXCEPTION 'brant_mfg: % does not change', TG_TABLE_NAME;
        END IF;
        IF NOT EXISTS (
            SELECT 1 FROM brant_mfg.collaboration_plan p
            WHERE p.tenant_id = OLD.tenant_id AND p.company_id = OLD.company_id AND p.plan_id = OLD.plan_id AND p.status = 'draft'
        ) THEN
            RAISE EXCEPTION 'brant_mfg: a published plan keeps its operations and materials';
        END IF;
        RETURN OLD;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_mfg: % is not deleted', TG_TABLE_NAME;
    END IF;
    IF NEW.row_version IS DISTINCT FROM OLD.row_version + 1 THEN
        RAISE EXCEPTION 'brant_mfg: a version moves by one';
    END IF;
    IF TG_TABLE_NAME = 'collaboration_plan' THEN
        IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.company_id IS DISTINCT FROM OLD.company_id
            OR NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.factory_party_id IS DISTINCT FROM OLD.factory_party_id
            OR NEW.owner_actor_id IS DISTINCT FROM OLD.owner_actor_id OR NEW.owner_team_id IS DISTINCT FROM OLD.owner_team_id
            OR NEW.opened_by_kind IS DISTINCT FROM OLD.opened_by_kind OR NEW.opened_by_id IS DISTINCT FROM OLD.opened_by_id
            OR NEW.open_epoch IS DISTINCT FROM OLD.open_epoch OR NEW.opened_at IS DISTINCT FROM OLD.opened_at
        THEN
            RAISE EXCEPTION 'brant_mfg: a plan anchor does not change';
        END IF;
        IF NEW.note IS DISTINCT FROM OLD.note AND OLD.status <> 'draft' THEN
            RAISE EXCEPTION 'brant_mfg: a note changes only while the plan is draft';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_TABLE_NAME = 'subcontract_order' THEN
        IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.company_id IS DISTINCT FROM OLD.company_id
            OR NEW.subcontract_id IS DISTINCT FROM OLD.subcontract_id OR NEW.plan_id IS DISTINCT FROM OLD.plan_id
            OR NEW.operation_id IS DISTINCT FROM OLD.operation_id OR NEW.quantity IS DISTINCT FROM OLD.quantity
            OR NEW.unit_id IS DISTINCT FROM OLD.unit_id OR NEW.unit_revision IS DISTINCT FROM OLD.unit_revision
            OR NEW.currency_code IS DISTINCT FROM OLD.currency_code OR NEW.minor_units IS DISTINCT FROM OLD.minor_units
            OR NEW.processing_fee_minor IS DISTINCT FROM OLD.processing_fee_minor
            OR NEW.factory_party_id IS DISTINCT FROM OLD.factory_party_id OR NEW.sku_id IS DISTINCT FROM OLD.sku_id
            OR NEW.spec_revision IS DISTINCT FROM OLD.spec_revision OR NEW.commitment_id IS DISTINCT FROM OLD.commitment_id
            OR NEW.order_id IS DISTINCT FROM OLD.order_id OR NEW.order_line_id IS DISTINCT FROM OLD.order_line_id
            OR NEW.order_revision IS DISTINCT FROM OLD.order_revision OR NEW.order_hash IS DISTINCT FROM OLD.order_hash
            OR NEW.subject_hash IS DISTINCT FROM OLD.subject_hash OR NEW.issue_hash IS DISTINCT FROM OLD.issue_hash
            OR NEW.owner_actor_id IS DISTINCT FROM OLD.owner_actor_id OR NEW.owner_team_id IS DISTINCT FROM OLD.owner_team_id
            OR NEW.opened_by_kind IS DISTINCT FROM OLD.opened_by_kind OR NEW.opened_by_id IS DISTINCT FROM OLD.opened_by_id
            OR NEW.open_epoch IS DISTINCT FROM OLD.open_epoch OR NEW.opened_at IS DISTINCT FROM OLD.opened_at
        THEN
            RAISE EXCEPTION 'brant_mfg: a subcontract anchor does not change';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_TABLE_NAME = 'company_setting' THEN
        IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.company_id IS DISTINCT FROM OLD.company_id THEN
            RAISE EXCEPTION 'brant_mfg: a company setting does not move';
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_mfg: % is not guarded', TG_TABLE_NAME;
END
$$;

REVOKE ALL ON FUNCTION brant_mfg.guard_mutation() FROM PUBLIC;

CREATE TRIGGER collaboration_plan_mutation BEFORE UPDATE OR DELETE ON brant_mfg.collaboration_plan
    FOR EACH ROW EXECUTE FUNCTION brant_mfg.guard_mutation();
CREATE TRIGGER operation_task_mutation BEFORE UPDATE OR DELETE ON brant_mfg.operation_task
    FOR EACH ROW EXECUTE FUNCTION brant_mfg.guard_mutation();
CREATE TRIGGER material_responsibility_mutation BEFORE UPDATE OR DELETE ON brant_mfg.material_responsibility
    FOR EACH ROW EXECUTE FUNCTION brant_mfg.guard_mutation();
CREATE TRIGGER subcontract_order_mutation BEFORE UPDATE OR DELETE ON brant_mfg.subcontract_order
    FOR EACH ROW EXECUTE FUNCTION brant_mfg.guard_mutation();
CREATE TRIGGER operation_progress_mutation BEFORE UPDATE OR DELETE ON brant_mfg.operation_progress
    FOR EACH ROW EXECUTE FUNCTION brant_mfg.guard_mutation();
CREATE TRIGGER material_handover_mutation BEFORE UPDATE OR DELETE ON brant_mfg.material_handover
    FOR EACH ROW EXECUTE FUNCTION brant_mfg.guard_mutation();
CREATE TRIGGER company_setting_mutation BEFORE UPDATE OR DELETE ON brant_mfg.company_setting
    FOR EACH ROW EXECUTE FUNCTION brant_mfg.guard_mutation();

CREATE TABLE brant_mfg.action_result (
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

CREATE TABLE brant_mfg.audit_event (
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

ALTER TABLE brant_mfg.company_setting ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_mfg.company_setting FORCE ROW LEVEL SECURITY;
CREATE POLICY company_setting_scope ON brant_mfg.company_setting
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_mfg.collaboration_plan ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_mfg.collaboration_plan FORCE ROW LEVEL SECURITY;
CREATE POLICY collaboration_plan_scope ON brant_mfg.collaboration_plan
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_mfg.operation_task ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_mfg.operation_task FORCE ROW LEVEL SECURITY;
CREATE POLICY operation_task_scope ON brant_mfg.operation_task
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_mfg.material_responsibility ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_mfg.material_responsibility FORCE ROW LEVEL SECURITY;
CREATE POLICY material_responsibility_scope ON brant_mfg.material_responsibility
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_mfg.subcontract_order ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_mfg.subcontract_order FORCE ROW LEVEL SECURITY;
CREATE POLICY subcontract_order_scope ON brant_mfg.subcontract_order
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_mfg.operation_progress ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_mfg.operation_progress FORCE ROW LEVEL SECURITY;
CREATE POLICY operation_progress_scope ON brant_mfg.operation_progress
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_mfg.material_handover ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_mfg.material_handover FORCE ROW LEVEL SECURITY;
CREATE POLICY material_handover_scope ON brant_mfg.material_handover
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_mfg.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_mfg.action_result FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_mfg.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_mfg.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_mfg.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_event_scope ON brant_mfg.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
