-- CAT: SkuGuard v1 (Context Map R14 CAT->TRD "SkuRef v1" with a qualification guard, guardLock
-- true; ADR-BR-004 option B, ADR-BR-011 lock order; R14's risk: a quote or order taking effect on a
-- specification revision that has since been revised).
--
-- A downstream context (TRD freezing or activating a quote, activating an order) calls
-- brant_cat.guard_sku_spec_current_v1 with its own executor inside its own transaction, after it
-- locked its own aggregate root FOR UPDATE. It decides what ResolveSpecs decides before that
-- transaction (sku.CheckSpec, CAT-INV-001): whether the specification revision a downstream fact
-- stores is the SKU's current confirmed one. It locks the SKU row FOR SHARE, then the confirmed
-- revision row the reference names. sku.revise_spec, sku.confirm and sku.deactivate update the SKU
-- row, so each either committed before the guard read it, and the guard reports the reference stale
-- or invalid, or waits on the row until the downstream transaction ends; in READ COMMITTED a guard
-- that waited re-reads the row as the command left it. A confirmed revision never changes (000001);
-- its row is locked because the reference names it.
--
-- It always returns one row. known is false, with code REFERENCE_INVALID, when the SKU does not
-- exist in the caller's scope or never had the revision confirmed (revision 0 included). Otherwise
-- code is NULL for the current confirmed revision of a confirmed SKU, REFERENCE_STALE when the SKU
-- was reopened for revision (draft) or confirmed in a later revision (refresh the reference and
-- resubmit), and REFERENCE_INVALID when the SKU is inactive, which is final. status and
-- current_revision describe the SKU now: current_revision is its latest confirmed revision while it
-- is confirmed, else 0 (SpecStanding of ResolveSpecs); both are NULL for an unknown SKU. The data
-- scope (own, team) is ResolveSpecs' to check before the transaction; the guard confines the rows
-- to app.tenant_id and app.company_id. SECURITY DEFINER runs it as the schema owner, which FORCE
-- ROW LEVEL SECURITY still confines to that scope; the predicates repeat it. search_path is fixed
-- and every name is qualified. brant_app gets EXECUTE in deploy/postgres/10_app_grants.sql; nobody
-- else does.

CREATE FUNCTION brant_cat.guard_sku_spec_current_v1(p_sku_id text, p_spec_revision bigint)
    RETURNS TABLE (known boolean, status text, current_revision bigint, code text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
    v_sku     record;
    v_current bigint;
BEGIN
    IF p_sku_id IS NULL OR p_spec_revision IS NULL THEN
        RAISE EXCEPTION 'brant_cat.guard_sku_spec_current_v1: sku_id and spec_revision are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    SELECT k.status, k.spec_revision INTO v_sku
    FROM brant_cat.sku k
    WHERE k.tenant_id = v_tenant AND k.company_id = v_company AND k.sku_id = p_sku_id
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, NULL::bigint, 'REFERENCE_INVALID'::text;
        RETURN;
    END IF;
    v_current := CASE WHEN v_sku.status = 'confirmed' THEN v_sku.spec_revision ELSE 0 END;
    PERFORM 1
    FROM brant_cat.sku_revision r
    WHERE r.tenant_id = v_tenant AND r.company_id = v_company AND r.sku_id = p_sku_id AND r.spec_revision = p_spec_revision
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, v_sku.status, v_current, 'REFERENCE_INVALID'::text;
        RETURN;
    END IF;
    RETURN QUERY SELECT true, v_sku.status, v_current,
        CASE
            WHEN v_sku.status = 'inactive' THEN 'REFERENCE_INVALID'
            WHEN v_sku.status <> 'confirmed' OR p_spec_revision <> v_sku.spec_revision THEN 'REFERENCE_STALE'
        END::text;
END
$$;

REVOKE ALL ON FUNCTION brant_cat.guard_sku_spec_current_v1(text, bigint) FROM PUBLIC;
