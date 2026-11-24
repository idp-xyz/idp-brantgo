-- TRD (S7-01): one document profile on a quote root and on each sales order. The profile is set
-- when the row is created and does not change. An order copies the quote's profile. OrderGuard v1
-- returns it as its last column so a caller that names the earlier columns is unchanged.
--
-- Delivery tolerance is not this migration. That column arrives with trd/000014 (S7-03).

ALTER TABLE brant_trd.quote_root
    ADD COLUMN document_profile text NOT NULL DEFAULT 'generic'
        CHECK (document_profile IN ('generic', 'apparel'));

ALTER TABLE brant_trd.sales_order
    ADD COLUMN document_profile text NOT NULL DEFAULT 'generic'
        CHECK (document_profile IN ('generic', 'apparel'));

CREATE FUNCTION brant_trd.guard_document_profile() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.document_profile IS DISTINCT FROM OLD.document_profile THEN
        RAISE EXCEPTION 'brant_trd: document_profile does not change after the row is created'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER quote_root_document_profile BEFORE UPDATE OF document_profile ON brant_trd.quote_root
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_document_profile();
CREATE TRIGGER sales_order_document_profile BEFORE UPDATE OF document_profile ON brant_trd.sales_order
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_document_profile();

DROP FUNCTION brant_trd.guard_order_active_v1(text, bigint, text);

CREATE FUNCTION brant_trd.guard_order_active_v1(p_order_id text, p_order_revision bigint, p_order_hash text)
    RETURNS TABLE (known boolean, status text, order_revision bigint, order_hash text, quote_id text, revision bigint,
                   business_hash text, review_id text, approval_revoked boolean, code text, document_profile text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
    v_order   record;
    v_revoked boolean;
BEGIN
    IF p_order_id IS NULL OR p_order_revision IS NULL OR p_order_hash IS NULL THEN
        RAISE EXCEPTION 'brant_trd.guard_order_active_v1: order_id, order_revision and order_hash are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    SELECT o.status, o.order_revision, o.order_hash, o.quote_id, o.revision, o.business_hash, o.activated_review_id,
           o.document_profile
        INTO v_order
    FROM brant_trd.sales_order o
    WHERE o.tenant_id = v_tenant AND o.company_id = v_company AND o.order_id = p_order_id
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, NULL::bigint, NULL::text, NULL::text, NULL::bigint, NULL::text, NULL::text, false,
            'REFERENCE_INVALID'::text, NULL::text;
        RETURN;
    END IF;
    v_revoked := EXISTS (SELECT 1 FROM brant_trd.sales_order_review r WHERE r.tenant_id = v_tenant AND r.company_id = v_company
        AND r.order_id = p_order_id AND r.review_id = v_order.activated_review_id AND r.status = 'revoked');
    RETURN QUERY SELECT true, v_order.status, v_order.order_revision, v_order.order_hash, v_order.quote_id, v_order.revision,
        v_order.business_hash, v_order.activated_review_id, v_revoked,
        CASE
            WHEN v_order.order_revision <> p_order_revision OR v_order.order_hash <> p_order_hash THEN 'REFERENCE_STALE'
            WHEN v_order.status <> 'active' THEN 'ORDER_NOT_ACTIVE'
            WHEN v_revoked THEN 'ORDER_APPROVAL_REVOKED'
        END::text,
        v_order.document_profile;
END
$$;

REVOKE ALL ON FUNCTION brant_trd.guard_order_active_v1(text, bigint, text) FROM PUBLIC;
