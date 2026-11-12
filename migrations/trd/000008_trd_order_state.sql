-- TRD 000008: the order-state read (S5 unit B4; A6 gap 1; S5 decision D-1's pattern, ADR-BR-018).
--
-- brant_trd.order_state_v1 says what an order is now — whether it exists in the caller's scope,
-- its status, order revision, order hash and row version — without taking a lock. A downstream
-- context calls it with its own executor inside its own transaction, after its own locks, to
-- decide on the order's state as the last TRD command that committed left it. STL's credit.release
-- calls it after locking its credit root and the reservation row FOR UPDATE: order.activate and
-- order.amendment.apply hold that reservation row FOR SHARE through CreditGuard v1 until they
-- commit, so a release that waited for one reads the order it activated or amended; one that
-- locked the row first commits before the guard reads it, and the guard sees the release.
--
-- It returns one row in the caller's scope (app.tenant_id, app.company_id): known false, with the
-- other columns NULL, for an order that does not exist there. It applies no data scope (the
-- caller's resource was bound to the order under its own rules). SECURITY DEFINER, fixed
-- search_path, qualified names; brant_app alone may execute it (deploy/postgres/10_app_grants.sql).
CREATE FUNCTION brant_trd.order_state_v1(p_order_id text)
    RETURNS TABLE (known boolean, status text, order_revision bigint, order_hash text, row_version bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
BEGIN
    IF p_order_id IS NULL THEN
        RAISE EXCEPTION 'brant_trd.order_state_v1: order_id is required' USING ERRCODE = 'null_value_not_allowed';
    END IF;
    RETURN QUERY
    SELECT true, o.status, o.order_revision, o.order_hash, o.row_version
    FROM brant_trd.sales_order o
    WHERE o.tenant_id = v_tenant AND o.company_id = v_company AND o.order_id = p_order_id;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, NULL::bigint, NULL::text, NULL::bigint;
    END IF;
END
$$;

REVOKE ALL ON FUNCTION brant_trd.order_state_v1(text) FROM PUBLIC;
