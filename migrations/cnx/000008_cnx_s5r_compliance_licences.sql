-- CNX: the S5R decisions on trade compliance and export licences (WBS S5R-09; simulated sign-off E0,
-- 2026-11-13; tactical model CNX-INV-001, CNX-INV-002, SM-CNX-COMPLIANCE; Context Map R36, R56, R58).
-- Python V0.50 has no CNX.
--
-- S5R-D10: TradeComplianceGuard v1 keeps its signature and becomes stricter. A pending check whose
-- latest decision (by its evaluation's sequence) is blocked — a blocked conclusion compliance is
-- evaluating again — still answers blocked, reason blocked_under_review, until a new decision.
--
-- S5R-D17: a clear decision records the authorizations (export licence, exemption or the authority's
-- approval; EVD sources the decision cites, each with its validity) an embargo or sanctions finding
-- is cleared on; the clear conclusion's validity never passes theirs (the application caps it).
--
-- S5R-D04: a licence states its usage mode — single_use (一批一证, the legal mode of dual-use export
-- licences and the default: one live allocation, none once declared) or multi_use (with the EVD
-- sources of the licence face that says so) — and its customs office (一证一关). Licences registered
-- before keep no customs office and are single_use, the legal default. An allocation records the
-- shipment it ships (an export lot's shipment, a shipment itself) and, for an export lot, the
-- shipment line of each lot line, so ExportLicenseGuard v1 (same signature) can let the allocations
-- of a shipment's export lots cover its lines: one licence per declaration, several licences per
-- shipment through its export lots. A shipment never holds its own allocation beside allocations of
-- its lots. Allocations made before keep no shipment and count as before (a lot's not at all).
--
-- S5R-D40: an allocation records where its end user came from: stated on the shipment, or derived
-- from the consignee, then the customer.
--
-- S5R-D03: export_license.release on basis local_cancellation releases everything outstanding of an
-- allocation whose target FUL cancelled, while no declaration case covering the target is submitted
-- or later (submitted, unknown, received, accepted, released, amended): kind 'cancelled', result
-- source 'local_cancellation', no receipt, the target's cancelled version recorded. Declared usage is
-- still released only on an external result (MD-44).

-- S5R-D10 ------------------------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION brant_cnx.trade_compliance_state(p_order_id text, p_order_revision bigint, p_order_hash text, p_at timestamptz,
        p_lock boolean)
    RETURNS TABLE (clear boolean, status text, reason text, check_id text, decision_id text, license_required boolean,
                   regulation_id text, regulation_revision bigint, valid_until timestamptz, code text)
    LANGUAGE plpgsql VOLATILE SECURITY INVOKER SET search_path = pg_catalog, pg_temp
AS $$
#variable_conflict use_column
DECLARE
    v_tenant   text := pg_catalog.current_setting('app.tenant_id', true);
    v_company  text := pg_catalog.current_setting('app.company_id', true);
    v_check    record;
    v_decision record;
    v_latest   text;
    v_last     text;
    v_last_reg text;
    v_last_rev bigint;
    v_review   boolean := false;
    v_list     record;
    v_code     text := 'TRADE_COMPLIANCE_NOT_CLEAR';
BEGIN
    IF p_order_id IS NULL OR p_order_revision IS NULL OR p_order_hash IS NULL OR p_at IS NULL OR p_lock IS NULL THEN
        RAISE EXCEPTION 'brant_cnx.trade_compliance_state: order, order revision, order hash, instant and lock mode are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    IF p_lock THEN
        SELECT c.check_id, c.order_hash, c.status, c.current_evaluation_id, c.current_decision_id INTO v_check
        FROM brant_cnx.trade_compliance_check c
        WHERE c.tenant_id = v_tenant AND c.company_id = v_company AND c.order_id = p_order_id AND c.order_revision = p_order_revision
        FOR SHARE;
    ELSE
        SELECT c.check_id, c.order_hash, c.status, c.current_evaluation_id, c.current_decision_id INTO v_check
        FROM brant_cnx.trade_compliance_check c
        WHERE c.tenant_id = v_tenant AND c.company_id = v_company AND c.order_id = p_order_id AND c.order_revision = p_order_revision;
    END IF;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, 'pending'::text,
            CASE WHEN EXISTS (SELECT 1 FROM brant_cnx.trade_compliance_check o WHERE o.tenant_id = v_tenant AND o.company_id = v_company
                AND o.order_id = p_order_id) THEN 'order_revised' ELSE 'not_checked' END::text,
            ''::text, ''::text, false, NULL::text, NULL::bigint, NULL::timestamptz, v_code;
        RETURN;
    END IF;
    IF v_check.current_decision_id IS NOT NULL THEN
        SELECT d.license_required, d.regulation_id, d.regulation_revision, d.valid_until INTO v_decision
        FROM brant_cnx.trade_compliance_decision d
        WHERE d.tenant_id = v_tenant AND d.company_id = v_company AND d.check_id = v_check.check_id
            AND d.decision_id = v_check.current_decision_id;
    END IF;
    -- S5R-D10: decisions are only inserted under the check's FOR UPDATE, one per evaluation, so the
    -- check's lock holds its latest decision too.
    IF v_check.status = 'pending' THEN
        SELECT d.decision_id, d.conclusion, d.regulation_id, d.regulation_revision INTO v_latest, v_last, v_last_reg, v_last_rev
        FROM brant_cnx.trade_compliance_decision d
        JOIN brant_cnx.trade_compliance_evaluation e ON e.tenant_id = d.tenant_id AND e.company_id = d.company_id
            AND e.check_id = d.check_id AND e.evaluation_id = d.evaluation_id
        WHERE d.tenant_id = v_tenant AND d.company_id = v_company AND d.check_id = v_check.check_id
        ORDER BY e.seq DESC LIMIT 1;
        v_review := coalesce(v_last = 'blocked', false);
    END IF;
    IF v_check.order_hash <> p_order_hash THEN
        RETURN QUERY SELECT false, CASE WHEN v_review THEN 'blocked' ELSE v_check.status END::text, 'order_changed'::text, v_check.check_id,
            coalesce(v_check.current_decision_id, CASE WHEN v_review THEN v_latest END, ''), false,
            NULL::text, NULL::bigint, NULL::timestamptz, v_code;
        RETURN;
    END IF;
    IF v_review THEN
        RETURN QUERY SELECT false, 'blocked'::text, 'blocked_under_review'::text, v_check.check_id, v_latest, false,
            v_last_reg, v_last_rev, NULL::timestamptz, v_code;
        RETURN;
    END IF;
    IF v_check.status = 'pending' THEN
        RETURN QUERY SELECT false, 'pending'::text,
            CASE WHEN v_check.current_evaluation_id IS NULL THEN 'not_evaluated' ELSE 'awaiting_decision' END::text,
            v_check.check_id, ''::text, false, NULL::text, NULL::bigint, NULL::timestamptz, v_code;
        RETURN;
    END IF;
    IF v_check.status = 'blocked' THEN
        RETURN QUERY SELECT false, 'blocked'::text, 'blocked'::text, v_check.check_id, v_check.current_decision_id, false,
            v_decision.regulation_id, v_decision.regulation_revision, NULL::timestamptz, v_code;
        RETURN;
    END IF;
    IF v_decision.valid_until <= p_at THEN
        RETURN QUERY SELECT false, 'expired'::text, 'expired'::text, v_check.check_id, v_check.current_decision_id, v_decision.license_required,
            v_decision.regulation_id, v_decision.regulation_revision, v_decision.valid_until, v_code;
        RETURN;
    END IF;
    FOR v_list IN
        SELECT u.list_code AS code, u.version AS version FROM brant_cnx.trade_compliance_list_used u
        WHERE u.tenant_id = v_tenant AND u.company_id = v_company AND u.check_id = v_check.check_id
            AND u.evaluation_id = v_check.current_evaluation_id
        ORDER BY u.list_code
    LOOP
        IF brant_ref.country_programme_list_current_v1(v_list.code, p_lock) IS DISTINCT FROM v_list.version THEN
            RETURN QUERY SELECT false, 'pending'::text, 'list_updated'::text, v_check.check_id, v_check.current_decision_id,
                v_decision.license_required, v_decision.regulation_id, v_decision.regulation_revision, v_decision.valid_until, v_code;
            RETURN;
        END IF;
    END LOOP;
    RETURN QUERY SELECT true, 'clear'::text, 'cleared'::text, v_check.check_id, v_check.current_decision_id, v_decision.license_required,
        v_decision.regulation_id, v_decision.regulation_revision, v_decision.valid_until, NULL::text;
END
$$;

-- S5R-D17 ------------------------------------------------------------------------------------------

ALTER TABLE brant_cnx.trade_compliance_decision ADD COLUMN authorizations text;
ALTER TABLE brant_cnx.trade_compliance_decision ADD CONSTRAINT trade_compliance_decision_authorizations_check
    CHECK (authorizations IS NULL OR (left(authorizations, 2) = '[{' AND conclusion = 'clear'));

-- S5R-D04, S5R-D40 ---------------------------------------------------------------------------------

ALTER TABLE brant_cnx.export_license ADD COLUMN usage_mode text NOT NULL DEFAULT 'single_use'
    CHECK (usage_mode IN ('single_use', 'multi_use'));
ALTER TABLE brant_cnx.export_license ALTER COLUMN usage_mode DROP DEFAULT;
ALTER TABLE brant_cnx.export_license ADD COLUMN usage_evidence text CHECK (left(usage_evidence, 2) = '[{');
ALTER TABLE brant_cnx.export_license ADD COLUMN customs_office text CHECK (customs_office ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,31}$');
ALTER TABLE brant_cnx.export_license ADD CONSTRAINT export_license_usage_evidence_mode
    CHECK (usage_mode = 'multi_use' OR usage_evidence IS NULL);

-- A licence is registered at version 1 with its customs office, and multi_use with the evidence of
-- its face (S5R-D04); afterwards only its version (by one) and updated_at move.
CREATE OR REPLACE FUNCTION brant_cnx.guard_export_license() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cnx.export_license: licences are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.row_version <> 1 OR NEW.updated_at <> NEW.registered_at THEN
            RAISE EXCEPTION 'brant_cnx.export_license: a licence is registered at version 1' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.customs_office IS NULL OR (NEW.usage_mode = 'multi_use') <> (NEW.usage_evidence IS NOT NULL) THEN
            RAISE EXCEPTION 'brant_cnx.export_license: a licence states its customs office, and multi_use its evidence (S5R-D04)'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - ARRAY['row_version', 'updated_at']) IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['row_version', 'updated_at'])
            OR NEW.row_version <> OLD.row_version + 1 OR NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'brant_cnx.export_license: licence % keeps its terms and moves by one version', OLD.license_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

ALTER TABLE brant_cnx.export_license_allocation ADD COLUMN shipment_id text CHECK (shipment_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$');
ALTER TABLE brant_cnx.export_license_allocation ADD COLUMN end_user_source text CHECK (end_user_source IN ('stated', 'consignee', 'customer'));
ALTER TABLE brant_cnx.export_license_allocation ADD CONSTRAINT export_license_allocation_shipment_check
    CHECK (target_kind = 'export_lot' OR shipment_id IS NULL OR shipment_id = target_id);
CREATE INDEX export_license_allocation_shipment ON brant_cnx.export_license_allocation (tenant_id, company_id, shipment_id)
    WHERE shipment_id IS NOT NULL;

-- Whether a declaration case covering a FUL shipment or export lot is submitted or later (S5R-D03):
-- customs has, or may have, the declaration. Rejected, withdrawn and later outcomes of an attempt
-- that never reached customs do not count.
CREATE FUNCTION brant_cnx.export_target_declared(p_tenant text, p_company text, p_kind text, p_target text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY INVOKER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT EXISTS (SELECT 1 FROM brant_cnx.declaration_case c WHERE c.tenant_id = p_tenant AND c.company_id = p_company
        AND ((p_kind = 'export_lot' AND c.export_lot_id = p_target) OR (p_kind = 'shipment' AND c.shipment_id = p_target))
        AND c.status IN ('submitted', 'unknown', 'received', 'accepted', 'released', 'amended'))
$$;
REVOKE ALL ON FUNCTION brant_cnx.export_target_declared(text, text, text, text) FROM PUBLIC;

-- Whether an allocation of a licence was declared: a declaration case covering its target is
-- submitted or later, or an amended receipt released part of it (S5R-D04: a single_use licence is
-- not allocated again once declared).
CREATE FUNCTION brant_cnx.export_license_declared(p_tenant text, p_company text, p_license text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY INVOKER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT EXISTS (SELECT 1 FROM brant_cnx.export_license_allocation a WHERE a.tenant_id = p_tenant AND a.company_id = p_company
        AND a.license_id = p_license AND (brant_cnx.export_target_declared(p_tenant, p_company, a.target_kind, a.target_id)
            OR EXISTS (SELECT 1 FROM brant_cnx.export_license_release r WHERE r.tenant_id = a.tenant_id AND r.company_id = a.company_id
                AND r.allocation_id = a.allocation_id AND r.kind = 'amended')))
$$;
REVOKE ALL ON FUNCTION brant_cnx.export_license_declared(text, text, text) FROM PUBLIC;

-- An allocation's outstanding quantity (its quantity less its releases').
CREATE FUNCTION brant_cnx.export_allocation_outstanding(p_tenant text, p_company text, p_allocation text) RETURNS numeric
    LANGUAGE sql STABLE SECURITY INVOKER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT a.quantity - coalesce((SELECT sum(r.quantity) FROM brant_cnx.export_license_release r WHERE r.tenant_id = a.tenant_id
        AND r.company_id = a.company_id AND r.allocation_id = a.allocation_id), 0)
    FROM brant_cnx.export_license_allocation a WHERE a.tenant_id = p_tenant AND a.company_id = p_company AND a.allocation_id = p_allocation
$$;
REVOKE ALL ON FUNCTION brant_cnx.export_allocation_outstanding(text, text, text) FROM PUBLIC;

-- An allocation is only inserted, under its licence's FOR UPDATE lock, in the licence's currency,
-- as its target's next attempt after every earlier one was released in full, and never takes Σ
-- outstanding above the licensed quantity or value (CNX-INV-002). S5R-D04: it names its shipment
-- and the source of its end user; a single_use licence holds one live allocation and none once
-- declared; a shipment never holds its own allocation beside live allocations of its export lots.
CREATE OR REPLACE FUNCTION brant_cnx.guard_export_license_allocation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_license record;
    v_used    record;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cnx.export_license_allocation: allocations never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT l.licensed_quantity, l.licensed_value_minor, l.currency_code, l.usage_mode INTO v_license FROM brant_cnx.export_license l
    WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id AND l.license_id = NEW.license_id FOR UPDATE;
    IF NOT FOUND OR v_license.currency_code <> NEW.currency_code THEN
        RAISE EXCEPTION 'brant_cnx.export_license_allocation: an allocation is in its licence''s currency'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.shipment_id IS NULL OR NEW.end_user_source IS NULL THEN
        RAISE EXCEPTION 'brant_cnx.export_license_allocation: an allocation names its shipment and its end user''s source (S5R-D04, D40)'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.attempt <> 1 + (SELECT count(*) FROM brant_cnx.export_license_allocation a WHERE a.tenant_id = NEW.tenant_id
                AND a.company_id = NEW.company_id AND a.target_kind = NEW.target_kind AND a.target_id = NEW.target_id)
            OR EXISTS (SELECT 1 FROM brant_cnx.export_license_allocation a WHERE a.tenant_id = NEW.tenant_id
                AND a.company_id = NEW.company_id AND a.target_kind = NEW.target_kind AND a.target_id = NEW.target_id
                AND brant_cnx.export_allocation_outstanding(a.tenant_id, a.company_id, a.allocation_id) > 0) THEN
        RAISE EXCEPTION 'brant_cnx.export_license_allocation: target % % holds an allocation', NEW.target_kind, NEW.target_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM brant_cnx.export_license_allocation a WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id
            AND ((NEW.target_kind = 'export_lot' AND a.target_kind = 'shipment' AND a.target_id = NEW.shipment_id)
                OR (NEW.target_kind = 'shipment' AND a.target_kind = 'export_lot' AND a.shipment_id = NEW.target_id))
            AND brant_cnx.export_allocation_outstanding(a.tenant_id, a.company_id, a.allocation_id) > 0) THEN
        RAISE EXCEPTION 'brant_cnx.export_license_allocation: shipment % holds allocations of itself and of its export lots (S5R-D04)',
            NEW.shipment_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF v_license.usage_mode = 'single_use' AND (EXISTS (SELECT 1 FROM brant_cnx.export_license_allocation a WHERE a.tenant_id = NEW.tenant_id
                AND a.company_id = NEW.company_id AND a.license_id = NEW.license_id
                AND brant_cnx.export_allocation_outstanding(a.tenant_id, a.company_id, a.allocation_id) > 0)
            OR brant_cnx.export_license_declared(NEW.tenant_id, NEW.company_id, NEW.license_id)) THEN
        RAISE EXCEPTION 'brant_cnx.export_license_allocation: single_use licence % holds a live or declared allocation (S5R-D04)', NEW.license_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT * INTO v_used FROM brant_cnx.export_license_outstanding(NEW.tenant_id, NEW.company_id, NEW.license_id);
    IF v_used.quantity + NEW.quantity > v_license.licensed_quantity OR v_used.value_minor + NEW.value_minor > v_license.licensed_value_minor THEN
        RAISE EXCEPTION 'brant_cnx.export_license_allocation: licence % would be exceeded (CNX-INV-002)', NEW.license_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

-- S5R-D03 ------------------------------------------------------------------------------------------

ALTER TABLE brant_cnx.export_license_release ADD COLUMN basis text NOT NULL DEFAULT 'external_result'
    CHECK (basis IN ('external_result', 'local_cancellation'));
ALTER TABLE brant_cnx.export_license_release ALTER COLUMN basis DROP DEFAULT;
ALTER TABLE brant_cnx.export_license_release ADD COLUMN target_version bigint CHECK (target_version >= 0);
ALTER TABLE brant_cnx.export_license_release DROP CONSTRAINT export_license_release_kind_check;
ALTER TABLE brant_cnx.export_license_release ADD CONSTRAINT export_license_release_kind_check
    CHECK (kind IN ('amended', 'withdrawn', 'cancelled'));
ALTER TABLE brant_cnx.export_license_release DROP CONSTRAINT export_license_release_result_source_check;
ALTER TABLE brant_cnx.export_license_release ADD CONSTRAINT export_license_release_result_source_check
    CHECK (result_source IN ('stated', 'declaration_receipt', 'local_cancellation'));
ALTER TABLE brant_cnx.export_license_release DROP CONSTRAINT export_license_release_evidence_check;
ALTER TABLE brant_cnx.export_license_release ADD CONSTRAINT export_license_release_evidence_check CHECK (left(evidence, 1) = '[');
ALTER TABLE brant_cnx.export_license_release ALTER COLUMN declaration_number DROP NOT NULL;
ALTER TABLE brant_cnx.export_license_release ALTER COLUMN receipt_text DROP NOT NULL;
ALTER TABLE brant_cnx.export_license_release ALTER COLUMN received_at DROP NOT NULL;
ALTER TABLE brant_cnx.export_license_release ADD CONSTRAINT export_license_release_basis_fields CHECK (
    (basis = 'local_cancellation') = (kind = 'cancelled') AND (basis = 'local_cancellation') = (result_source = 'local_cancellation')
    AND (basis = 'local_cancellation' OR (declaration_number IS NOT NULL AND receipt_text IS NOT NULL AND received_at IS NOT NULL
        AND left(evidence, 2) = '[{' AND target_version IS NULL))
    AND (basis = 'external_result' OR (declaration_number IS NULL AND receipt_text IS NULL AND received_at IS NULL AND receipt_id IS NULL
        AND target_version IS NOT NULL)));

-- A release is only inserted, under its licence's FOR UPDATE lock, and never releases more of its
-- allocation's quantity or value than is outstanding. S5R-D03: a local cancellation releases all
-- that is outstanding, and only while no declaration case covering the target is submitted or later.
CREATE OR REPLACE FUNCTION brant_cnx.guard_export_license_release() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_allocation record;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cnx.export_license_release: releases never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    PERFORM 1 FROM brant_cnx.export_license l WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id
        AND l.license_id = NEW.license_id FOR UPDATE;
    SELECT a.target_kind, a.target_id, a.quantity - coalesce(sum(r.quantity), 0) AS quantity,
        a.value_minor - coalesce(sum(r.value_minor), 0) AS value_minor
    INTO v_allocation
    FROM brant_cnx.export_license_allocation a
    LEFT JOIN brant_cnx.export_license_release r ON r.tenant_id = a.tenant_id AND r.company_id = a.company_id AND r.allocation_id = a.allocation_id
    WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id AND a.allocation_id = NEW.allocation_id
    GROUP BY a.target_kind, a.target_id, a.quantity, a.value_minor;
    IF NOT FOUND OR NEW.quantity > v_allocation.quantity OR NEW.value_minor > v_allocation.value_minor THEN
        RAISE EXCEPTION 'brant_cnx.export_license_release: allocation % has less outstanding', NEW.allocation_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.basis = 'local_cancellation' AND (NEW.quantity <> v_allocation.quantity OR NEW.value_minor <> v_allocation.value_minor
            OR brant_cnx.export_target_declared(NEW.tenant_id, NEW.company_id, v_allocation.target_kind, v_allocation.target_id)) THEN
        RAISE EXCEPTION 'brant_cnx.export_license_release: a local cancellation releases all of an undeclared allocation % (S5R-D03, MD-44)',
            NEW.allocation_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

-- S5R-D04: ExportLicenseGuard v1 (same signature and columns). The verdict for a shipment at p_at
-- in the caller's scope. The shipment's own latest allocation decides while it has quantity
-- outstanding (or when no export lot of the shipment holds an allocation), as before: allocated,
-- required/released or expired, its licence row then the allocation row locked FOR SHARE with
-- p_lock. Otherwise the current allocations of the shipment's export lots (each lot's latest
-- attempt) cover it together: their licence rows (licence ID order), then their allocation rows
-- (allocation ID order) locked FOR SHARE with p_lock; covered with reason lots_allocated when every
-- one has all its quantity outstanding on a licence valid at p_at — license_id and allocation_id
-- list them comma-separated, quantity and value_minor sum what is outstanding when their licences
-- share a unit and a currency (NULL otherwise), valid_until is the earliest, and lines_hash covers
-- the shipment lines the lots' lines come from (LinesHash, the lot lines of one shipment line
-- summed) — otherwise required/released (one released in full), required/quantity_exceeded (one
-- released in part) or expired/expired. A shipment without an allocation of either kind locks
-- nothing.
CREATE OR REPLACE FUNCTION brant_cnx.export_license_state(p_shipment_id text, p_at timestamptz, p_lock boolean)
    RETURNS TABLE (covered boolean, status text, reason text, license_id text, allocation_id text, quantity text, unit text,
                   value_minor text, currency text, lines_hash text, valid_until timestamptz, code text)
    LANGUAGE plpgsql VOLATILE SECURITY INVOKER SET search_path = pg_catalog, pg_temp
AS $$
#variable_conflict use_column
DECLARE
    v_tenant      text := pg_catalog.current_setting('app.tenant_id', true);
    v_company     text := pg_catalog.current_setting('app.company_id', true);
    v_own         text;
    v_own_license text;
    v_own_live    boolean := false;
    v_license     record;
    v_allocation  record;
    v_quantity    numeric;
    v_value       numeric;
    v_lots        text[];
    v_licenses    text[];
    v_lot         record;
    v_released    boolean := false;
    v_partial     boolean := false;
    v_expired     boolean := false;
    v_units       text[];
    v_currencies  text[];
    v_sum_q       numeric := 0;
    v_sum_v       numeric := 0;
    v_until       timestamptz;
    v_hash        text;
    v_code        text := 'EXPORT_LICENSE_REQUIRED';
BEGIN
    IF p_shipment_id IS NULL OR p_at IS NULL OR p_lock IS NULL THEN
        RAISE EXCEPTION 'brant_cnx.export_license_state: shipment, instant and lock mode are required' USING ERRCODE = 'null_value_not_allowed';
    END IF;
    SELECT a.allocation_id, a.license_id, brant_cnx.export_allocation_outstanding(a.tenant_id, a.company_id, a.allocation_id) > 0
    INTO v_own, v_own_license, v_own_live FROM brant_cnx.export_license_allocation a
    WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.target_kind = 'shipment' AND a.target_id = p_shipment_id
    ORDER BY a.attempt DESC LIMIT 1;
    IF NOT coalesce(v_own_live, false) THEN
        SELECT array_agg(x.allocation_id ORDER BY x.allocation_id), array_agg(DISTINCT x.license_id ORDER BY x.license_id)
        INTO v_lots, v_licenses
        FROM (SELECT DISTINCT ON (a.target_id) a.allocation_id, a.license_id FROM brant_cnx.export_license_allocation a
              WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.target_kind = 'export_lot' AND a.shipment_id = p_shipment_id
              ORDER BY a.target_id, a.attempt DESC) x;
    END IF;
    IF v_lots IS NULL THEN
        IF v_own IS NULL THEN
            RETURN QUERY SELECT false, 'required'::text, 'not_allocated'::text, ''::text, ''::text, NULL::text, NULL::text, NULL::text,
                NULL::text, NULL::text, NULL::timestamptz, v_code;
            RETURN;
        END IF;
        IF p_lock THEN
            SELECT l.unit, l.currency_code, l.valid_from, l.valid_until INTO v_license FROM brant_cnx.export_license l
            WHERE l.tenant_id = v_tenant AND l.company_id = v_company AND l.license_id = v_own_license FOR SHARE;
            SELECT a.quantity, a.value_minor, a.lines_hash INTO v_allocation FROM brant_cnx.export_license_allocation a
            WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.allocation_id = v_own FOR SHARE;
        ELSE
            SELECT l.unit, l.currency_code, l.valid_from, l.valid_until INTO v_license FROM brant_cnx.export_license l
            WHERE l.tenant_id = v_tenant AND l.company_id = v_company AND l.license_id = v_own_license;
            SELECT a.quantity, a.value_minor, a.lines_hash INTO v_allocation FROM brant_cnx.export_license_allocation a
            WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.allocation_id = v_own;
        END IF;
        SELECT v_allocation.quantity - coalesce(sum(r.quantity), 0), v_allocation.value_minor - coalesce(sum(r.value_minor), 0)
        INTO v_quantity, v_value
        FROM brant_cnx.export_license_release r
        WHERE r.tenant_id = v_tenant AND r.company_id = v_company AND r.allocation_id = v_own;
        IF v_quantity <= 0 THEN
            RETURN QUERY SELECT false, 'required'::text, 'released'::text, v_own_license, v_own,
                trim_scale(v_quantity)::text, v_license.unit, trim_scale(v_value)::text, v_license.currency_code, v_allocation.lines_hash,
                v_license.valid_until, v_code;
            RETURN;
        END IF;
        IF NOT (p_at >= v_license.valid_from AND p_at < v_license.valid_until) THEN
            RETURN QUERY SELECT false, 'expired'::text, 'expired'::text, v_own_license, v_own, trim_scale(v_quantity)::text,
                v_license.unit, trim_scale(v_value)::text, v_license.currency_code, v_allocation.lines_hash, v_license.valid_until, v_code;
            RETURN;
        END IF;
        RETURN QUERY SELECT true, 'allocated'::text, 'allocated'::text, v_own_license, v_own, trim_scale(v_quantity)::text,
            v_license.unit, trim_scale(v_value)::text, v_license.currency_code, v_allocation.lines_hash, v_license.valid_until, NULL::text;
        RETURN;
    END IF;
    IF p_lock THEN
        PERFORM 1 FROM brant_cnx.export_license l WHERE l.tenant_id = v_tenant AND l.company_id = v_company AND l.license_id = ANY (v_licenses)
            ORDER BY l.license_id FOR SHARE;
        PERFORM 1 FROM brant_cnx.export_license_allocation a WHERE a.tenant_id = v_tenant AND a.company_id = v_company
            AND a.allocation_id = ANY (v_lots) ORDER BY a.allocation_id FOR SHARE;
    END IF;
    FOR v_lot IN
        SELECT a.quantity AS allocated, l.unit, l.currency_code, l.valid_from, l.valid_until,
            a.quantity - coalesce((SELECT sum(r.quantity) FROM brant_cnx.export_license_release r WHERE r.tenant_id = a.tenant_id
                AND r.company_id = a.company_id AND r.allocation_id = a.allocation_id), 0) AS outstanding,
            a.value_minor - coalesce((SELECT sum(r.value_minor) FROM brant_cnx.export_license_release r WHERE r.tenant_id = a.tenant_id
                AND r.company_id = a.company_id AND r.allocation_id = a.allocation_id), 0) AS outstanding_value
        FROM brant_cnx.export_license_allocation a
        JOIN brant_cnx.export_license l ON l.tenant_id = a.tenant_id AND l.company_id = a.company_id AND l.license_id = a.license_id
        WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.allocation_id = ANY (v_lots)
        ORDER BY a.allocation_id
    LOOP
        v_released := v_released OR v_lot.outstanding <= 0;
        v_partial := v_partial OR (v_lot.outstanding > 0 AND v_lot.outstanding < v_lot.allocated);
        v_expired := v_expired OR NOT (p_at >= v_lot.valid_from AND p_at < v_lot.valid_until);
        v_sum_q := v_sum_q + greatest(v_lot.outstanding, 0);
        v_sum_v := v_sum_v + greatest(v_lot.outstanding_value, 0);
        v_until := least(v_until, v_lot.valid_until);
        IF NOT (v_lot.unit = ANY (coalesce(v_units, ARRAY[]::text[]))) THEN
            v_units := array_append(v_units, v_lot.unit);
        END IF;
        IF NOT (v_lot.currency_code = ANY (coalesce(v_currencies, ARRAY[]::text[]))) THEN
            v_currencies := array_append(v_currencies, v_lot.currency_code);
        END IF;
    END LOOP;
    SELECT encode(sha256(convert_to(coalesce(string_agg(s.line, E'\n' ORDER BY s.line_id COLLATE "C"), ''), 'UTF8')), 'hex') INTO v_hash
    FROM (SELECT e ->> 'shipment_line_id' AS line_id,
              (e ->> 'shipment_line_id') || '|' || (e ->> 'sku_id') || '|' || (e ->> 'spec_revision') || '|'
                  || round(sum((e ->> 'quantity')::numeric), 6)::text || '|' || (e ->> 'unit') AS line
          FROM brant_cnx.export_license_allocation a, jsonb_array_elements(a.lines::jsonb) e
          WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.allocation_id = ANY (v_lots)
          GROUP BY e ->> 'shipment_line_id', e ->> 'sku_id', e ->> 'spec_revision', e ->> 'unit') s;
    RETURN QUERY SELECT NOT (v_released OR v_partial OR v_expired),
        CASE WHEN v_released OR v_partial THEN 'required' WHEN v_expired THEN 'expired' ELSE 'allocated' END::text,
        CASE WHEN v_released THEN 'released' WHEN v_partial THEN 'quantity_exceeded' WHEN v_expired THEN 'expired' ELSE 'lots_allocated' END::text,
        array_to_string(v_licenses, ','), array_to_string(v_lots, ','),
        CASE WHEN cardinality(v_units) = 1 THEN trim_scale(v_sum_q)::text END, CASE WHEN cardinality(v_units) = 1 THEN v_units[1] END,
        CASE WHEN cardinality(v_currencies) = 1 THEN trim_scale(v_sum_v)::text END,
        CASE WHEN cardinality(v_currencies) = 1 THEN v_currencies[1] END, v_hash, v_until,
        CASE WHEN v_released OR v_partial OR v_expired THEN v_code END;
END
$$;
