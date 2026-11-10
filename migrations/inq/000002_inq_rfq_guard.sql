-- INQ: RfqGuard v1 (ADR-BR-004 option B, Context Map R08 INQ→CST "ready 询价与完整行集合" and R12
-- INQ→TRD, both with a guard lock).
--
-- A downstream context (CST, TRD) calls brant_inq.guard_rfq_ready_v1 with its own executor inside
-- its own transaction, after it locked its own aggregate root FOR UPDATE (ADR-BR-011). The function
-- locks the RFQ root row FOR SHARE in that transaction. Every INQ command that ends a ready RFQ
-- (rfq.cancel, rfq.close) updates that row, so either the INQ command committed first and the
-- guard reports the RFQ stale, or the command waits on the row until the downstream transaction
-- ends. In READ COMMITTED a guard that waited re-reads the row as the INQ command left it.
--
-- The verdict is the RFQ's current state: ready, at the row version the downstream bound, with the
-- line set it bound. Lines change only while an RFQ is a draft (000001), so a ready RFQ keeps its
-- lines; the guard still recomputes their hash, so a binding of a partial or different line set is
-- stale too. The hash is the SHA-256 in lower-case hex of the lines in byte order of their line IDs,
-- each line its line ID, SKU ID, specification revision, quantity and unit joined by U+001F, the
-- lines joined by U+001E (inq/api RfqLinesHash computes the same in Go).
--
-- It always returns one row; known is false for an RFQ that does not exist in the caller's scope.
-- SECURITY DEFINER runs it as the schema owner, which FORCE ROW LEVEL SECURITY still confines to
-- app.tenant_id and app.company_id; the scope predicates repeat that. search_path is fixed and every
-- name is qualified. brant_app gets EXECUTE in deploy/postgres/10_app_grants.sql; nobody else does.

CREATE FUNCTION brant_inq.guard_rfq_ready_v1(p_rfq_id text, p_row_version bigint, p_lines_hash text)
    RETURNS TABLE (known boolean, status text, row_version bigint, lines_hash text, code text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog
AS $$
DECLARE
    r record;
    h text;
BEGIN
    IF p_rfq_id IS NULL OR p_row_version IS NULL OR p_lines_hash IS NULL THEN
        RAISE EXCEPTION 'brant_inq.guard_rfq_ready_v1: rfq_id, row_version and lines_hash are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    SELECT q.status, q.row_version INTO r
    FROM brant_inq.customer_rfq q
    WHERE q.tenant_id = pg_catalog.current_setting('app.tenant_id', true)
      AND q.company_id = pg_catalog.current_setting('app.company_id', true)
      AND q.rfq_id = p_rfq_id
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, NULL::bigint, NULL::text, 'REFERENCE_INVALID'::text;
        RETURN;
    END IF;
    SELECT pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(coalesce(pg_catalog.string_agg(
               l.line_id || E'\x1f' || coalesce(l.sku_id, '') || E'\x1f' || coalesce(l.sku_spec_revision::text, '')
               || E'\x1f' || coalesce(l.quantity, '') || E'\x1f' || coalesce(l.uom, ''),
               E'\x1e' ORDER BY l.line_id COLLATE "C"), ''), 'UTF8')), 'hex') INTO h
    FROM brant_inq.rfq_line l
    WHERE l.tenant_id = pg_catalog.current_setting('app.tenant_id', true)
      AND l.company_id = pg_catalog.current_setting('app.company_id', true)
      AND l.rfq_id = p_rfq_id;
    RETURN QUERY SELECT true, r.status, r.row_version, h,
        CASE WHEN r.status <> 'ready' OR r.row_version <> p_row_version OR h <> p_lines_hash THEN 'REFERENCE_STALE' END::text;
END
$$;

REVOKE ALL ON FUNCTION brant_inq.guard_rfq_ready_v1(text, bigint, text) FROM PUBLIC;
