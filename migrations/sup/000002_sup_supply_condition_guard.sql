-- SUP: SupplyConditionGuard v1 (ADR-BR-004 option B, Context Map R09 SUP→CST and R20 SUP→PUR
-- "SupplyConditionRef v1＋SupplyConditionGuard", guardLock true).
--
-- A downstream context (CST freezing a cost snapshot, PUR preparing a purchase draft) calls
-- brant_sup.guard_supply_condition_v1 with its own executor inside its own transaction, after it
-- locked its own aggregate root FOR UPDATE (ADR-BR-011). The function locks the exact revision row
-- FOR SHARE in that transaction; for purchase terms of dependent validity it then locks the source
-- offer revision row FOR SHARE as well. Every SUP command that ends a revision updates that row:
-- verifying or publishing the next revision supersedes it, withdrawing or retiring ends it. So
-- either the SUP command committed first and the guard reports the revision not in force, or the
-- command waits on the row until the downstream transaction ends. In READ COMMITTED a guard that
-- waited re-reads the row as the SUP command left it.
--
-- The verdict is the revision's current state: in force when it is active now, was verified at or
-- before p_at and its validity [valid_from, valid_until) contains p_at
-- (SUPPLY_CONDITION_NOT_CURRENT, SUPPLY_CONDITION_EXPIRED otherwise); dependent purchase terms also
-- need their source offer revision in force by the same rule (T02 §13.8, A46). It always returns one
-- row; known is false for a revision that does not exist in the caller's scope. SECURITY DEFINER
-- runs it as the schema owner, which FORCE ROW LEVEL SECURITY still confines to app.tenant_id and
-- app.company_id; the scope predicates repeat that. search_path is fixed and every name is
-- qualified. brant_app gets EXECUTE in deploy/postgres/10_app_grants.sql; nobody else does.

CREATE FUNCTION brant_sup.guard_supply_condition_v1(p_kind text, p_id text, p_revision bigint, p_at timestamptz)
    RETURNS TABLE (known boolean, status text, code text, content_hash text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog
AS $$
DECLARE
    r record;
    s record;
    v text;
BEGIN
    IF p_kind IS NULL OR p_id IS NULL OR p_revision IS NULL OR p_at IS NULL THEN
        RAISE EXCEPTION 'brant_sup.guard_supply_condition_v1: kind, id, revision and instant are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    SELECT c.status, c.verified_at, c.valid_from, c.valid_until, c.content_hash, c.validity_mode, c.source_offer_id,
           c.source_offer_revision INTO r
    FROM brant_sup.condition_revision c
    WHERE c.tenant_id = pg_catalog.current_setting('app.tenant_id', true)
      AND c.company_id = pg_catalog.current_setting('app.company_id', true)
      AND c.kind = p_kind AND c.condition_id = p_id AND c.revision = p_revision
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, 'REFERENCE_INVALID'::text, NULL::text;
        RETURN;
    END IF;
    v := CASE
        WHEN r.status <> 'active' OR r.verified_at IS NULL OR r.verified_at > p_at THEN 'SUPPLY_CONDITION_NOT_CURRENT'
        WHEN p_at < r.valid_from OR p_at >= r.valid_until THEN 'SUPPLY_CONDITION_EXPIRED'
    END;
    IF v IS NULL AND r.validity_mode = 'dependent_validity' THEN
        SELECT c.status, c.verified_at, c.valid_from, c.valid_until INTO s
        FROM brant_sup.condition_revision c
        WHERE c.tenant_id = pg_catalog.current_setting('app.tenant_id', true)
          AND c.company_id = pg_catalog.current_setting('app.company_id', true)
          AND c.kind = 'supplier_offer' AND c.condition_id = r.source_offer_id AND c.revision = r.source_offer_revision
        FOR SHARE;
        IF NOT FOUND THEN
            v := 'SUPPLY_CONDITION_NOT_CURRENT';
        ELSE
            v := CASE
                WHEN s.status <> 'active' OR s.verified_at IS NULL OR s.verified_at > p_at THEN 'SUPPLY_CONDITION_NOT_CURRENT'
                WHEN p_at < s.valid_from OR p_at >= s.valid_until THEN 'SUPPLY_CONDITION_EXPIRED'
            END;
        END IF;
    END IF;
    RETURN QUERY SELECT true, r.status, v, r.content_hash;
END
$$;

REVOKE ALL ON FUNCTION brant_sup.guard_supply_condition_v1(text, text, bigint, timestamptz) FROM PUBLIC;
