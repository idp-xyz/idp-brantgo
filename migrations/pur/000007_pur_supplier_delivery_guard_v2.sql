-- PUR: SupplierDeliveryGuard v2 (Context Map R59 PUR→FUL beside v1, guardLock; simulated decision S5R-D05, E0,
-- 2026-11-13; tactical model FUL-INV-003, PUR-INV-008, PUR-INV-013; S5R unit R13, the follow-up of R4's and R6's
-- reports). Python V0.50 has none of it.
--
-- An approved disposition (migration 000006) takes delivered goods of a commitment line out of the order, but
-- SupplierDeliveryGuard v1 answers for one delivery line and knows nothing of dispositions, and it locks the delivery
-- root, which a disposition never touches: disposed goods could still ship while the order is active. v2 answers what
-- v1 answers for the same (delivery, line, version) and, for a known line, what its commitment line adds up to now:
--
--   line_verified_quantity  the verified quantities of the versions verified now of every delivery of the commitment
--                           line (brant_pur.commitment_line_verified);
--   line_disposed_quantity  what the line's approved dispositions took out of the order
--                           (brant_pur.commitment_line_disposed);
--
-- so that FUL ships of a commitment line, with what its confirmed shipments shipped of it, at most verified − disposed
-- (FUL-INV-003). It locks the commitment root FOR SHARE until the caller's transaction ends, and not the delivery root:
-- supplier_delivery.record and .verify lock the delivery root and then this commitment root FOR UPDATE, and
-- purchase.commitment.dispose and .disposition.decide lock the commitment root FOR UPDATE, so each of them either
-- committed before the guard took the lock — and the guard, reading after it, sees what it did — or waits until the
-- caller's transaction ends. Holding no delivery root, a caller that names several deliveries of one commitment never
-- waits for a delivery while it holds the commitment a verification of that delivery waits for. The delivery's
-- commitment never changes; it is read without a lock to find the root. v1 is unchanged.

CREATE FUNCTION brant_pur.guard_supplier_delivery_v2(p_delivery_id text, p_line_id text, p_version bigint)
    RETURNS TABLE (known boolean, status text, verified_version bigint, commitment_id text, order_id text, order_line_id text,
                   supplier_id text, sku_id text, spec_revision bigint, unit_id text, unit_revision bigint, verified_quantity text,
                   line_verified_quantity text, line_disposed_quantity text, code text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant     text := pg_catalog.current_setting('app.tenant_id', true);
    v_company    text := pg_catalog.current_setting('app.company_id', true);
    v_commitment text;
    v_root       record;
    v_line       record;
    v_found      boolean := false;
BEGIN
    IF p_delivery_id IS NULL OR p_line_id IS NULL OR p_version IS NULL THEN
        RAISE EXCEPTION 'brant_pur.guard_supplier_delivery_v2: delivery_id, line_id and version are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    SELECT d.commitment_id INTO v_commitment
    FROM brant_pur.supplier_delivery d
    WHERE d.tenant_id = v_tenant AND d.company_id = v_company AND d.delivery_id = p_delivery_id;
    IF v_commitment IS NOT NULL THEN
        PERFORM 1 FROM brant_pur.purchase_commitment c
        WHERE c.tenant_id = v_tenant AND c.company_id = v_company AND c.commitment_id = v_commitment
        FOR SHARE;
        SELECT d.verified_version, d.commitment_id, d.order_id, d.supplier_id INTO v_root
        FROM brant_pur.supplier_delivery d
        WHERE d.tenant_id = v_tenant AND d.company_id = v_company AND d.delivery_id = p_delivery_id;
        SELECT v.status, l.order_line_id, l.sku_id, l.spec_revision, l.unit_id, l.unit_revision, l.verified_quantity INTO v_line
        FROM brant_pur.supplier_delivery_version v
        JOIN brant_pur.supplier_delivery_line l ON l.tenant_id = v.tenant_id AND l.company_id = v.company_id AND l.delivery_id = v.delivery_id
            AND l.version = v.version
        WHERE v.tenant_id = v_tenant AND v.company_id = v_company AND v.delivery_id = p_delivery_id AND v.version = p_version
            AND l.order_line_id = p_line_id;
        v_found := FOUND;
    END IF;
    IF NOT v_found THEN
        RETURN QUERY SELECT false, NULL::text, NULL::bigint, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::bigint, NULL::text,
            NULL::bigint, NULL::text, NULL::text, NULL::text, 'SUPPLIER_DELIVERY_UNVERIFIED'::text;
        RETURN;
    END IF;
    RETURN QUERY SELECT true, v_line.status, v_root.verified_version, v_root.commitment_id, v_root.order_id, v_line.order_line_id,
        v_root.supplier_id, v_line.sku_id, v_line.spec_revision, v_line.unit_id, v_line.unit_revision,
        CASE WHEN v_line.status = 'verified' THEN pg_catalog.trim_scale(v_line.verified_quantity)::text END,
        pg_catalog.trim_scale(brant_pur.commitment_line_verified(v_tenant, v_company, v_commitment, p_line_id))::text,
        pg_catalog.trim_scale(brant_pur.commitment_line_disposed(v_tenant, v_company, v_commitment, p_line_id))::text,
        CASE v_line.status WHEN 'verified' THEN NULL WHEN 'superseded' THEN 'REFERENCE_STALE' ELSE 'SUPPLIER_DELIVERY_UNVERIFIED' END::text;
END
$$;

REVOKE ALL ON FUNCTION brant_pur.guard_supplier_delivery_v2(text, text, bigint) FROM PUBLIC;
