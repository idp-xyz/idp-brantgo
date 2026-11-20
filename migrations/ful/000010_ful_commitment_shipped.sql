-- FUL: CommitmentShipped v1, the confirmed shipped quantity of one purchase commitment, by order
-- line (S4 unit R14; Context Map R82 FUL->PUR "CommitmentShipped v1", OHS（事务内无锁读取）, guardLock
-- false; tactical model PUR-INV-013, the "减已出运" half of S5R-D05). The reverse-read pattern of
-- order_shipped_v1 (R70) and release_consumption_v1 (R79).
--
-- PUR's purchase.commitment.dispose and purchase.commitment.disposition.decide lock the commitment
-- root FOR UPDATE and then call this function with PUR's own executor inside PUR's transaction. The
-- read takes no lock. It is consistent only while every command that adds to the confirmed shipped
-- quantity of a commitment line holds that commitment's root FOR SHARE until it commits.
-- shipment.confirm is that command: it calls SupplierDeliveryGuard v2, which locks the commitment
-- root FOR SHARE, on every delivery line, and a confirmation is written only when that judgement
-- passes. A confirmation either committed before PUR got its lock, and is counted, or waits for PUR
-- and then meets the commitment as PUR left it. The function is VOLATILE so that its query reads a
-- snapshot taken when it runs, after the caller's lock. Prepared and cancelled shipments never
-- count, and a confirmed shipment's lines never change (shipment_line is insert-only, and a
-- confirmed shipment is not updated).
--
-- It returns one row per order line and unit of the commitment that has a confirmed shipment line
-- in the caller's scope (app.tenant_id and app.company_id): the sum of those lines (QuantityFormat
-- text) and the number of confirmed shipments they came from; no row when the commitment shipped
-- nothing. The grain is the commitment line, not the order line: several commitments may share one
-- order line, and one commitment's shipments are not another's. It applies no data scope (own,
-- team): the caller decided whether its actor sees the commitment. SECURITY DEFINER runs it as the
-- schema owner, which FORCE ROW LEVEL SECURITY still confines to the caller's scope; the predicates
-- repeat it. search_path is fixed and every name is qualified. brant_app gets EXECUTE in
-- deploy/postgres/10_app_grants.sql; nobody else does.
--
-- shipment_line had no index on commitment_id (shipment_line_delivery is the form: tenant and
-- company, then the fact the read filters).
CREATE INDEX shipment_line_commitment ON brant_ful.shipment_line (tenant_id, company_id, commitment_id, order_line_id);

CREATE FUNCTION brant_ful.commitment_shipped_v1(p_commitment_id text)
    RETURNS TABLE (order_line_id text, unit_id text, unit_revision bigint, shipped_quantity text, shipments bigint)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
BEGIN
    IF p_commitment_id IS NULL THEN
        RAISE EXCEPTION 'brant_ful.commitment_shipped_v1: commitment_id is required' USING ERRCODE = 'null_value_not_allowed';
    END IF;
    RETURN QUERY
    SELECT l.order_line_id, l.unit_id, l.unit_revision, pg_catalog.trim_scale(pg_catalog.sum(l.quantity))::text,
           pg_catalog.count(DISTINCT l.shipment_id)
    FROM brant_ful.shipment_line l
    JOIN brant_ful.shipment s ON s.tenant_id = l.tenant_id AND s.company_id = l.company_id AND s.shipment_id = l.shipment_id
    WHERE l.tenant_id = v_tenant AND l.company_id = v_company AND l.commitment_id = p_commitment_id AND s.status = 'confirmed'
    GROUP BY l.order_line_id, l.unit_id, l.unit_revision
    ORDER BY l.order_line_id COLLATE "C", l.unit_id COLLATE "C", l.unit_revision;
END
$$;

REVOKE ALL ON FUNCTION brant_ful.commitment_shipped_v1(text) FROM PUBLIC;
