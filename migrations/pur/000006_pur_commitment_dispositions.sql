-- PUR: dispositions of delivered goods and the S5R commitment rules (WBS S5R-04; simulated decisions S5R-D05 and S5R-D41,
-- E0, 2026-11-13; tactical model PUR-AGG-PurchaseCommitment, SM-PUR-COMMITMENT-DISPOSITION, SM-PUR-COMMITMENT,
-- PUR-INV-011, PUR-INV-012, PUR-INV-013; Context Map R71 DownstreamCommitments v1 and R31 PurchaseLineRef v1). Python
-- V0.50 has none of it.
--
-- A disposition (CommitmentDisposition, an entity of PurchaseCommitment) says where goods the supplier delivered for a
-- commitment line, verified and not shipped, go: return_to_supplier (back to the factory, with the evidence),
-- write_off (borne by us: the sunk cost of the order, S5R-D42) or hold_for_reuse (kept for a later order; recorded only
-- while STK is not enabled). A purchaser requests it (purchase.commitment.dispose) and an order reviewer other than the
-- requester decides it once (purchase.commitment.disposition.decide): approved or rejected with a reason. It never
-- changes the commitment's status or row version. At every commit the approved dispositions of a commitment line add up
-- to no more than what is verified delivered of it (PUR-INV-013; what shipped is FUL's and is not seen here). Its unit is
-- the order line's, the unit of the line's DemandAllocation root (PUR-INV-012).
--
-- DownstreamCommitments v1 shows an approved disposition as a row of kind commitment_disposition with the disposition
-- in its disposition column, and the demand allocation of a line net of the approved dispositions of that line
-- ("订单的未处置……需求分配"): TRD's order end counts the disposed goods as settled (TRD-INV-019).
--
-- S5R-D41: a commitment with a claim recorded and not yet verified or rejected is not cancelled; and a verification
-- never leaves a commitment line verified below its approved dispositions.

CREATE TABLE brant_pur.purchase_commitment_disposition (
    tenant_id        text          NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id       text          NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    disposition_id   text          NOT NULL CHECK (disposition_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    commitment_id    text          NOT NULL,
    status           text          NOT NULL CONSTRAINT purchase_commitment_disposition_status
        CHECK (status IN ('requested', 'approved', 'rejected')),
    row_version      bigint        NOT NULL CHECK (row_version IN (1, 2)),
    draft_id         text          NOT NULL,
    order_id         text          NOT NULL,
    order_line_id    text          NOT NULL,
    kind             text          NOT NULL CHECK (kind IN ('return_to_supplier', 'write_off', 'hold_for_reuse')),
    quantity         numeric(18,6) NOT NULL CHECK (quantity > 0),
    unit_id          text          NOT NULL,
    unit_revision    bigint        NOT NULL CHECK (unit_revision > 0),
    reason           text          NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    -- The EVD sources the request cites ([{"locator": ..., "source_id": ...}, ...]); a return to the supplier cites at
    -- least one (S5R-D05: 退回工厂，附证据).
    evidence         text          NOT NULL CHECK (left(evidence, 1) = '['),
    requested_by_kind text         NOT NULL CHECK (requested_by_kind = 'human'),
    requested_by_id  text          NOT NULL,
    request_epoch    bigint        NOT NULL CHECK (request_epoch > 0),
    requested_at     timestamptz   NOT NULL,
    decided_by_kind  text          CHECK (decided_by_kind = 'human'),
    decided_by_id    text,
    decide_epoch     bigint        CHECK (decide_epoch > 0),
    decided_at       timestamptz,
    decision         text          CHECK (decision IN ('approve', 'reject')),
    decision_reason  text          CHECK (char_length(decision_reason) BETWEEN 1 AND 1000),
    updated_at       timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, disposition_id),
    FOREIGN KEY (tenant_id, company_id, commitment_id) REFERENCES brant_pur.purchase_commitment (tenant_id, company_id, commitment_id),
    FOREIGN KEY (tenant_id, company_id, draft_id, order_line_id)
        REFERENCES brant_pur.purchase_draft_line (tenant_id, company_id, draft_id, order_line_id),
    CHECK (kind <> 'return_to_supplier' OR left(evidence, 2) = '[{'),
    CHECK ((decided_at IS NULL) = (decided_by_id IS NULL) AND (decided_by_id IS NULL) = (decided_by_kind IS NULL)
        AND (decided_by_id IS NULL) = (decide_epoch IS NULL) AND (decided_by_id IS NULL) = (decision IS NULL)),
    -- SM-PUR-COMMITMENT-DISPOSITION: what each state carries.
    CHECK (CASE status
        WHEN 'requested' THEN row_version = 1 AND decided_at IS NULL AND decision_reason IS NULL
        WHEN 'approved' THEN row_version = 2 AND decision = 'approve'
        ELSE row_version = 2 AND decision = 'reject' AND decision_reason IS NOT NULL END),
    -- PUR-INV-013: decided by a human other than the requester (role templates' independence of
    -- purchase.commitment.disposition.decide from 处置申请人), after the request.
    CHECK (decided_by_id IS DISTINCT FROM requested_by_id),
    CHECK (decided_at >= requested_at)
);

CREATE INDEX purchase_commitment_disposition_commitment ON brant_pur.purchase_commitment_disposition (tenant_id, company_id, commitment_id);
CREATE INDEX purchase_commitment_disposition_order ON brant_pur.purchase_commitment_disposition (tenant_id, company_id, order_id, order_line_id);

-- What the approved dispositions of a commitment line add up to.
CREATE FUNCTION brant_pur.commitment_line_disposed(p_tenant text, p_company text, p_commitment_id text, p_order_line_id text)
    RETURNS numeric LANGUAGE sql STABLE SET search_path = pg_catalog, pg_temp
AS $$
    SELECT coalesce(sum(x.quantity), 0) FROM brant_pur.purchase_commitment_disposition x
    WHERE x.tenant_id = p_tenant AND x.company_id = p_company AND x.commitment_id = p_commitment_id AND x.order_line_id = p_order_line_id
        AND x.status = 'approved'
$$;
REVOKE ALL ON FUNCTION brant_pur.commitment_line_disposed(text, text, text, text) FROM PUBLIC;

-- A disposition is requested for a line of a commitment that was issued (issued, confirmed or cancelled since), binding
-- its draft, order and the unit of the line's DemandAllocation root; then it is decided once, writing only the decision.
-- Nothing is deleted.
CREATE FUNCTION brant_pur.guard_purchase_commitment_disposition() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_decision text[] := ARRAY['status', 'row_version', 'updated_at', 'decided_by_kind', 'decided_by_id', 'decide_epoch', 'decided_at',
        'decision', 'decision_reason'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment_disposition: dispositions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'requested' OR NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment c
                JOIN brant_pur.purchase_draft_line l ON l.tenant_id = c.tenant_id AND l.company_id = c.company_id AND l.draft_id = c.draft_id
                JOIN brant_pur.demand_allocation a ON a.tenant_id = c.tenant_id AND a.company_id = c.company_id AND a.order_id = c.order_id
                    AND a.order_line_id = l.order_line_id
                WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id AND c.commitment_id = NEW.commitment_id
                    AND c.status IN ('issued', 'confirmed', 'cancelled') AND c.draft_id = NEW.draft_id AND c.order_id = NEW.order_id
                    AND l.order_line_id = NEW.order_line_id AND a.unit_id = NEW.unit_id AND a.unit_revision = NEW.unit_revision) THEN
            RAISE EXCEPTION 'brant_pur.purchase_commitment_disposition: a disposition is requested for a line of an issued commitment in the line''s unit (PUR-INV-012, PUR-INV-013)'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status <> 'requested' OR NEW.status NOT IN ('approved', 'rejected') OR NEW.row_version <> OLD.row_version + 1
            OR (to_jsonb(NEW) - v_decision) IS DISTINCT FROM (to_jsonb(OLD) - v_decision) THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment_disposition: disposition % (%) is decided once (SM-PUR-COMMITMENT-DISPOSITION)',
            OLD.disposition_id, OLD.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER purchase_commitment_disposition_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pur.purchase_commitment_disposition
    FOR EACH ROW EXECUTE FUNCTION brant_pur.guard_purchase_commitment_disposition();

-- PUR-INV-013 at commit: the approved dispositions of a commitment line add up to no more than what is verified
-- delivered of it.
CREATE FUNCTION brant_pur.check_purchase_commitment_disposition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF brant_pur.commitment_line_disposed(NEW.tenant_id, NEW.company_id, NEW.commitment_id, NEW.order_line_id)
            > brant_pur.commitment_line_verified(NEW.tenant_id, NEW.company_id, NEW.commitment_id, NEW.order_line_id) THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment_disposition: the approved dispositions of commitment % line % exceed its verified deliveries (PUR-INV-013)',
            NEW.commitment_id, NEW.order_line_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER purchase_commitment_disposition_within_verified AFTER INSERT OR UPDATE ON brant_pur.purchase_commitment_disposition
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_pur.check_purchase_commitment_disposition();

-- PUR-INV-008 at commit, against what the commitment buys at its revision in force (migration 000004), and PUR-INV-013:
-- no verification leaves a line of the commitment verified below its approved dispositions.
CREATE OR REPLACE FUNCTION brant_pur.check_supplier_delivery_within_commitment() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_line text;
BEGIN
    SELECT l.order_line_id INTO v_line
    FROM brant_pur.supplier_delivery_line l
    LEFT JOIN brant_pur.commitment_lines(l.tenant_id, l.company_id, l.commitment_id) q ON q.order_line_id = l.order_line_id
    WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id AND l.delivery_id = NEW.delivery_id AND l.version = NEW.version
        AND (q.purchased IS NULL
            OR brant_pur.commitment_line_verified(l.tenant_id, l.company_id, l.commitment_id, l.order_line_id) > q.purchased)
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'brant_pur.supplier_delivery_line: the verified deliveries of commitment % line % exceed what it buys (PUR-INV-008)',
            NEW.commitment_id, v_line USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT x.order_line_id INTO v_line
    FROM brant_pur.purchase_commitment_disposition x
    WHERE x.tenant_id = NEW.tenant_id AND x.company_id = NEW.company_id AND x.commitment_id = NEW.commitment_id AND x.status = 'approved'
        AND brant_pur.commitment_line_disposed(x.tenant_id, x.company_id, x.commitment_id, x.order_line_id)
            > brant_pur.commitment_line_verified(x.tenant_id, x.company_id, x.commitment_id, x.order_line_id)
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'brant_pur.supplier_delivery_line: commitment % line % would be verified below its approved dispositions (PUR-INV-013)',
            NEW.commitment_id, v_line USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;

-- SM-PUR-COMMITMENT as migration 000004 has it, and S5R-D41: a commitment is not cancelled while a claim of one of its
-- deliveries awaits its verification (PUR-INV-011: 承诺有未核实的交付声明时取消被拒).
CREATE OR REPLACE FUNCTION brant_pur.guard_purchase_commitment() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_decision text[] := ARRAY['decided_by_kind', 'decided_by_id', 'decide_epoch', 'decided_at', 'decision', 'valid_until',
        'decision_reason', 'acknowledgements'];
    v_revoke   text[] := ARRAY['revoked_by_kind', 'revoked_by_id', 'revoke_epoch', 'revoked_at', 'revoke_reason'];
    v_issue    text[] := ARRAY['issued_by_kind', 'issued_by_id', 'issue_epoch', 'issued_at', 'po_number', 'sequence_id',
        'sequence_revision', 'sent_at', 'issue_evidence', 'issue_file_id', 'issue_file_sha256'];
    v_cancel   text[] := ARRAY['cancelled_by_kind', 'cancelled_by_id', 'cancel_epoch', 'cancelled_at', 'cancel_reason', 'cancel_evidence'];
    v_moving   text[] := ARRAY['status', 'row_version', 'updated_at', 'revision'];
    v_changed  text[];
    v_kind     text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment: commitments are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'requested' OR NEW.revision <> 1 OR NOT EXISTS (SELECT 1 FROM brant_pur.purchase_draft d WHERE d.tenant_id = NEW.tenant_id
                AND d.company_id = NEW.company_id AND d.draft_id = NEW.draft_id AND d.status = 'prepared' AND d.draft_hash = NEW.draft_hash
                AND d.order_id = NEW.order_id AND d.order_revision = NEW.order_revision AND d.order_hash = NEW.order_hash
                AND d.supplier_id = NEW.supplier_id AND d.currency = NEW.currency AND d.supply_valid_until = NEW.supply_valid_until)
            OR EXISTS (SELECT 1 FROM brant_pur.purchase_commitment c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
                AND c.draft_id = NEW.draft_id AND c.status = 'cancelled') THEN
            RAISE EXCEPTION 'brant_pur.purchase_commitment: a commitment is requested for a prepared draft never committed and binds it exactly'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.revision = OLD.revision + 1 THEN
        SELECT r.kind INTO v_kind FROM brant_pur.purchase_commitment_revision r WHERE r.tenant_id = NEW.tenant_id
            AND r.company_id = NEW.company_id AND r.commitment_id = NEW.commitment_id AND r.revision = NEW.revision;
    END IF;
    v_changed := CASE
        WHEN NEW.revision <> OLD.revision THEN CASE
            WHEN OLD.status IN ('issued', 'confirmed') AND NEW.status = 'issued' AND v_kind = 'change' THEN ARRAY[]::text[]
            WHEN OLD.status IN ('issued', 'confirmed') AND NEW.status = 'cancelled' AND v_kind = 'cancel' THEN v_cancel END
        WHEN OLD.status = 'requested' AND NEW.status IN ('approved', 'rejected') THEN v_decision
        WHEN OLD.status = 'approved' AND NEW.status = 'revoked' THEN v_revoke
        WHEN OLD.status = 'approved' AND NEW.status = 'issued' THEN v_issue
        WHEN OLD.status = 'issued' AND NEW.status IN ('issued', 'confirmed') THEN ARRAY[]::text[]
    END;
    IF v_changed IS NULL OR NEW.row_version <> OLD.row_version + 1
            OR (to_jsonb(NEW) - (v_changed || v_moving)) IS DISTINCT FROM (to_jsonb(OLD) - (v_changed || v_moving))
            OR (NEW.status IN ('approved', 'rejected') AND EXISTS (SELECT 1 FROM brant_pur.purchase_draft d
                WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND d.draft_id = NEW.draft_id
                    AND d.prepared_by_id = NEW.decided_by_id))
            OR (NEW.status = 'issued' AND OLD.status = 'approved' AND NOT EXISTS (SELECT 1 FROM brant_pur.purchase_draft d
                WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND d.draft_id = NEW.draft_id AND d.status = 'prepared'))
            OR (OLD.status = 'issued' AND NEW.revision = OLD.revision AND NOT EXISTS (SELECT 1 FROM brant_pur.supplier_confirmation s
                WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id AND s.commitment_id = NEW.commitment_id
                    AND s.commitment_version = NEW.row_version AND s.commitment_revision = NEW.revision
                    AND (s.result = 'as_issued') = (NEW.status = 'confirmed')))
            OR (NEW.status = 'cancelled' AND EXISTS (SELECT 1 FROM brant_pur.supplier_delivery d
                JOIN brant_pur.supplier_delivery_version v ON v.tenant_id = d.tenant_id AND v.company_id = d.company_id
                    AND v.delivery_id = d.delivery_id
                WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND d.commitment_id = NEW.commitment_id
                    AND v.status = 'recorded')) THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment: commitment % (%) changes only along SM-PUR-COMMITMENT (PUR-INV-009, PUR-INV-010, PUR-INV-011)',
            OLD.commitment_id, OLD.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

-- DownstreamCommitments v1 (Context Map R71), the columns and contract of migrations 000002 and 000004, with S5R-D05's
-- dispositions:
--
--   purchase_commitment     as migration 000004;
--   purchase_draft          as migration 000004;
--   demand_allocation       an order line whose effective allocation is not covered by the approved dispositions of the
--                           line (quantity: the allocation net of them, in the line's unit; listed while above zero);
--   commitment_disposition  an approved disposition (object_id the disposition, draft_id its commitment's draft,
--                           order_line_id and quantity what it disposes of, order_revision that of the commitment's
--                           revision in force, state approved, disposition its kind: return_to_supplier, write_off or
--                           hold_for_reuse).
--
-- It takes no locks: every PUR command that adds to it holds the order row FOR SHARE through OrderGuard v1 (S5 decision
-- D-1); a disposition's approval only takes from it.
CREATE OR REPLACE FUNCTION brant_pur.order_commitments_v1(p_order_id text)
    RETURNS TABLE (kind text, object_id text, draft_id text, order_line_id text, order_revision bigint, state text, quantity text,
                   disposition text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
BEGIN
    IF p_order_id IS NULL THEN
        RAISE EXCEPTION 'brant_pur.order_commitments_v1: order_id is required' USING ERRCODE = 'null_value_not_allowed';
    END IF;
    RETURN QUERY
    SELECT x.kind, x.object_id, x.draft_id, x.order_line_id, x.order_revision, x.state, x.quantity, x.disposition FROM (
        SELECT 'purchase_commitment'::text AS kind, c.commitment_id AS object_id, c.draft_id, NULL::text AS order_line_id,
            coalesce(r.order_revision, c.order_revision) AS order_revision,
            CASE WHEN c.status IN ('issued', 'confirmed') AND NOT EXISTS (SELECT 1 FROM brant_pur.commitment_lines(c.tenant_id, c.company_id,
                    c.commitment_id) q WHERE brant_pur.commitment_line_verified(c.tenant_id, c.company_id, c.commitment_id, q.order_line_id)
                    < q.purchased)
                THEN 'fulfilled' ELSE c.status END AS state,
            NULL::text AS quantity, NULL::text AS disposition
        FROM brant_pur.purchase_commitment c
        JOIN brant_pur.purchase_draft d ON d.tenant_id = c.tenant_id AND d.company_id = c.company_id AND d.draft_id = c.draft_id
        LEFT JOIN brant_pur.purchase_commitment_revision r ON r.tenant_id = c.tenant_id AND r.company_id = c.company_id
            AND r.commitment_id = c.commitment_id AND r.revision = c.revision
        WHERE c.tenant_id = v_tenant AND c.company_id = v_company AND c.order_id = p_order_id
            AND (c.status IN ('issued', 'confirmed') OR (c.status IN ('requested', 'approved') AND d.status = 'prepared'))
        UNION ALL
        SELECT 'purchase_draft'::text, d.draft_id, d.draft_id, NULL::text, d.order_revision, d.status, NULL::text, NULL::text
        FROM brant_pur.purchase_draft d
        WHERE d.tenant_id = v_tenant AND d.company_id = v_company AND d.order_id = p_order_id AND d.status = 'prepared'
            AND NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment c WHERE c.tenant_id = d.tenant_id AND c.company_id = d.company_id
                AND c.draft_id = d.draft_id AND c.status IN ('issued', 'confirmed', 'cancelled'))
        UNION ALL
        SELECT 'demand_allocation'::text, a.order_line_id, NULL::text, a.order_line_id, a.order_revision, 'allocated'::text,
            pg_catalog.trim_scale(a.allocated - n.disposed)::text, NULL::text
        FROM brant_pur.demand_allocation a
        CROSS JOIN LATERAL (SELECT coalesce(sum(x.quantity), 0) AS disposed FROM brant_pur.purchase_commitment_disposition x
            WHERE x.tenant_id = a.tenant_id AND x.company_id = a.company_id AND x.order_id = a.order_id
                AND x.order_line_id = a.order_line_id AND x.status = 'approved') n
        WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.order_id = p_order_id AND a.allocated > n.disposed
        UNION ALL
        SELECT 'commitment_disposition'::text, x.disposition_id, x.draft_id, x.order_line_id, coalesce(r.order_revision, c.order_revision),
            x.status, pg_catalog.trim_scale(x.quantity)::text, x.kind
        FROM brant_pur.purchase_commitment_disposition x
        JOIN brant_pur.purchase_commitment c ON c.tenant_id = x.tenant_id AND c.company_id = x.company_id AND c.commitment_id = x.commitment_id
        LEFT JOIN brant_pur.purchase_commitment_revision r ON r.tenant_id = c.tenant_id AND r.company_id = c.company_id
            AND r.commitment_id = c.commitment_id AND r.revision = c.revision
        WHERE x.tenant_id = v_tenant AND x.company_id = v_company AND x.order_id = p_order_id AND x.status = 'approved'
    ) x
    ORDER BY x.kind, x.object_id;
END
$$;

ALTER TABLE brant_pur.purchase_commitment_disposition ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.purchase_commitment_disposition FORCE ROW LEVEL SECURITY;
CREATE POLICY purchase_commitment_disposition_scope ON brant_pur.purchase_commitment_disposition
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
