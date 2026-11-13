-- FUL: ReleaseConsumption v1, what FUL shipped under each QLT quality release (S5R unit R5, WBS
-- S5R-05; Context Map R79 FUL->QLT "ReleaseConsumption v1", OHS（事务内无锁读取）, guardLock false;
-- decision S5R-D18; tactical model QLT-INV-002, SM-QLT-RELEASE; the reverse-read pattern of S5-D33).
--
-- QLT's quality_release.revoke locks the release row FOR UPDATE and then calls this function with
-- its own executor inside its own transaction; it records the frozen quantity, the release's quantity
-- less what FUL shipped under it. The read takes no lock: shipment.confirm, the only command that adds
-- to the answer, holds the same release row FOR SHARE through QLT's QualityReleaseGuard v1 until it
-- commits, so a confirmation either committed before QLT got its lock, and is counted, or waits for
-- the revocation and then meets the release revoked. The function is VOLATILE so that its query reads
-- a snapshot taken when it runs, after the caller's lock.
--
-- It returns one row per unit of the confirmed shipment lines that name the release in the caller's
-- scope (app.tenant_id and app.company_id): the sum of their quantities (QuantityFormat text) and the
-- number of confirmed shipments they came from; no row when nothing shipped under the release.
-- Prepared and cancelled shipments never count. It applies no data scope (own, team): the caller
-- decided whether its actor sees the release. SECURITY DEFINER runs it as the schema owner, which
-- FORCE ROW LEVEL SECURITY still confines to the caller's scope; the predicates repeat it.
-- search_path is fixed and every name is qualified. brant_app gets EXECUTE in
-- deploy/postgres/10_app_grants.sql; nobody else does.
CREATE FUNCTION brant_ful.release_consumption_v1(p_release_id text)
    RETURNS TABLE (unit_id text, unit_revision bigint, shipped_quantity text, shipments bigint)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
BEGIN
    IF p_release_id IS NULL THEN
        RAISE EXCEPTION 'brant_ful.release_consumption_v1: release_id is required' USING ERRCODE = 'null_value_not_allowed';
    END IF;
    RETURN QUERY
    SELECT l.unit_id, l.unit_revision, pg_catalog.trim_scale(pg_catalog.sum(l.quantity))::text, pg_catalog.count(DISTINCT l.shipment_id)
    FROM brant_ful.shipment_line l
    JOIN brant_ful.shipment s ON s.tenant_id = l.tenant_id AND s.company_id = l.company_id AND s.shipment_id = l.shipment_id
    WHERE l.tenant_id = v_tenant AND l.company_id = v_company AND l.quality_release_id = p_release_id AND s.status = 'confirmed'
    GROUP BY l.unit_id, l.unit_revision
    ORDER BY l.unit_id COLLATE "C", l.unit_revision;
END
$$;

REVOKE ALL ON FUNCTION brant_ful.release_consumption_v1(text) FROM PUBLIC;
