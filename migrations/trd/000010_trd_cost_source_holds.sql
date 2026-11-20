-- TRD 000010: what a denied cost source leaves on TRD's active objects (WBS S4-06b; WF-03, PL-07,
-- SC-04 activation first; S4 decision D-9; S3-D67; ADR-BR-004, ADR-BR-013; TRD-INV-014).
--
-- CST's CostSourceDenied names the snapshots whose evaluation rests on the denied source. TRD's
-- Inbox consumer trd.cost_source_holds (system:inbox.trd.cost_source_holds) registers, once per
-- denial:
--
-- * a risk hold, cause cost_source_denied and code COST_SOURCE_DENIED, on the active revision of a
--   root whose commit evidence names a denied snapshot while no contract formed on it — no live
--   (valid or disputed) acceptance accepts it and the root's conversion slot is free. Like the hold
--   of a revoked approval (S3-D67) it pauses the revision's forward use — responses, acceptance,
--   conversion, QuoteRevisionGuard v1, quote_ready — and is never lifted: a replacement or a
--   cancellation ends the revision. Its identifier is the cause and CST's attestation, so one
--   denial holds a revision once; the hold moves the root version.
-- * a risk record on an active order whose activation's commit evidence names a denied snapshot.
--   The contract formed (ADR-BR-013): the record changes nothing of the order — status, row version,
--   OrderGuard v1 and order_state_v1 stay as they are — and blocks no later step (decision D-9). A
--   revision a contract formed on is not held: its order's approval acknowledges the denial as an
--   upstream risk (COST_SNAPSHOT_INELIGIBLE, TRD-INV-014).
--
-- Holds of a revoked approval keep their S3 rules (human, approval revoked, no cost columns).

ALTER TABLE brant_trd.quote_risk_hold
    DROP CONSTRAINT quote_risk_hold_cause_check,
    DROP CONSTRAINT quote_risk_hold_code_check,
    DROP CONSTRAINT quote_risk_hold_registered_by_kind_check,
    DROP CONSTRAINT quote_risk_hold_check,
    ALTER COLUMN approval_id DROP NOT NULL,
    ADD COLUMN cost_snapshot_id text CHECK (cost_snapshot_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    ADD COLUMN cost_source_id   text CHECK (cost_source_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    ADD COLUMN attestation_id   text CHECK (attestation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    ADD COLUMN event_id         text CHECK (char_length(event_id) BETWEEN 1 AND 128),
    ADD CONSTRAINT quote_risk_hold_cause_check CHECK (
        (cause = 'approval_revoked' AND code = 'APPROVAL_REVOKED' AND registered_by_kind = 'human' AND approval_id IS NOT NULL
            AND hold_id = cause || ':' || approval_id
            AND num_nonnulls(cost_snapshot_id, cost_source_id, attestation_id, event_id) = 0)
        OR (cause = 'cost_source_denied' AND code = 'COST_SOURCE_DENIED' AND registered_by_kind = 'system' AND approval_id IS NULL
            AND hold_id = cause || ':' || attestation_id
            AND num_nulls(cost_snapshot_id, cost_source_id, attestation_id, event_id) = 0));

-- A hold of a revoked approval is registered on the active revision whose formalization approval
-- was revoked (S3); a hold of a denied cost source on the active revision whose commit evidence
-- names the snapshot, while no live acceptance accepts it and the root's conversion slot is free.
-- Never changed or deleted.
CREATE OR REPLACE FUNCTION brant_trd.guard_quote_risk_hold() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.quote_risk_hold: a risk hold never changes and is never lifted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.cause = 'approval_revoked' AND NOT EXISTS (SELECT 1 FROM brant_trd.quote_revision r
            JOIN brant_trd.quote_commit_evidence e ON e.tenant_id = r.tenant_id AND e.company_id = r.company_id AND e.quote_id = r.quote_id
                AND e.revision = r.revision
            JOIN brant_trd.quote_approval a ON a.tenant_id = e.tenant_id AND a.company_id = e.company_id AND a.quote_id = e.quote_id
                AND a.approval_id = e.approval_id
            WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.quote_id = NEW.quote_id AND r.revision = NEW.revision
                AND r.status = 'active' AND e.approval_id = NEW.approval_id AND a.effectivity = 'revoked') THEN
        RAISE EXCEPTION 'brant_trd.quote_risk_hold: a hold is registered on the active revision whose formalization approval was revoked'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.cause = 'cost_source_denied' AND (NOT EXISTS (SELECT 1 FROM brant_trd.quote_revision r
            JOIN brant_trd.quote_commit_evidence e ON e.tenant_id = r.tenant_id AND e.company_id = r.company_id AND e.quote_id = r.quote_id
                AND e.revision = r.revision
            WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.quote_id = NEW.quote_id AND r.revision = NEW.revision
                AND r.status = 'active' AND e.cost_snapshot_id = NEW.cost_snapshot_id)
            OR EXISTS (SELECT 1 FROM brant_trd.acceptance a WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id
                AND a.quote_id = NEW.quote_id AND a.revision = NEW.revision AND a.effectivity IN ('valid', 'disputed'))
            OR EXISTS (SELECT 1 FROM brant_trd.quote_conversion_slot s WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id
                AND s.quote_id = NEW.quote_id)) THEN
        RAISE EXCEPTION 'brant_trd.quote_risk_hold: a cost source hold is registered on the active revision whose commit evidence names the snapshot, before a contract formed on it'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

-- The consumer finds the revisions and orders a denial names by their snapshot.
CREATE INDEX quote_commit_evidence_cost_snapshot ON brant_trd.quote_commit_evidence (tenant_id, company_id, cost_snapshot_id);
CREATE INDEX sales_order_commit_evidence_cost_snapshot ON brant_trd.sales_order_commit_evidence (tenant_id, company_id, cost_snapshot_id);

CREATE TABLE brant_trd.sales_order_risk (
    tenant_id          text        NOT NULL,
    company_id         text        NOT NULL,
    order_id           text        NOT NULL,
    risk_id            text        NOT NULL,
    cause              text        NOT NULL CHECK (cause = 'cost_source_denied'),
    code               text        NOT NULL CHECK (code = 'COST_SOURCE_DENIED'),
    -- The order as the record found it: its revision and row version, which the record does not move.
    order_revision     bigint      NOT NULL CHECK (order_revision > 0),
    order_row_version  bigint      NOT NULL CHECK (order_row_version > 1),
    cost_snapshot_id   text        NOT NULL CHECK (cost_snapshot_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    cost_source_id     text        NOT NULL CHECK (cost_source_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    attestation_id     text        NOT NULL CHECK (attestation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    event_id           text        NOT NULL CHECK (char_length(event_id) BETWEEN 1 AND 128),
    registered_by_kind text        NOT NULL CHECK (registered_by_kind = 'system'),
    registered_by_id   text        NOT NULL,
    register_epoch     bigint      NOT NULL CHECK (register_epoch > 0),
    registered_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, order_id, risk_id),
    FOREIGN KEY (tenant_id, company_id, order_id) REFERENCES brant_trd.sales_order (tenant_id, company_id, order_id),
    CHECK (risk_id = cause || ':' || attestation_id)
);

-- A risk record is registered on an active order whose activation's commit evidence names the
-- snapshot, at the order's revision and row version as they are. Never changed or deleted.
CREATE FUNCTION brant_trd.guard_sales_order_risk() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.sales_order_risk: a risk record never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_trd.sales_order o
            JOIN brant_trd.sales_order_commit_evidence e ON e.tenant_id = o.tenant_id AND e.company_id = o.company_id AND e.order_id = o.order_id
            WHERE o.tenant_id = NEW.tenant_id AND o.company_id = NEW.company_id AND o.order_id = NEW.order_id AND o.status = 'active'
                AND o.order_revision = NEW.order_revision AND o.row_version = NEW.order_row_version
                AND e.cost_snapshot_id = NEW.cost_snapshot_id) THEN
        RAISE EXCEPTION 'brant_trd.sales_order_risk: a risk record is registered on the active order whose activation names the snapshot, as the order is'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER sales_order_risk_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.sales_order_risk
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_sales_order_risk();

ALTER TABLE brant_trd.sales_order_risk ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.sales_order_risk FORCE ROW LEVEL SECURITY;
CREATE POLICY sales_order_risk_scope ON brant_trd.sales_order_risk
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
