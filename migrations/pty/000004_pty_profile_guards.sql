-- PTY: TradingProfileGuard v1 and AddressRoleGuard v1 (Context Map R13 PTY->TRD "PartyRef v1、
-- TradingProfileRef v1、AddressRoleRef v1" with a qualification guard, guardLock true; ADR-BR-004
-- option B, ADR-BR-011 lock order; R13's risk: a party stopped for business still turned into an
-- order).
--
-- A downstream context (TRD freezing or activating a quote, activating an order) calls these
-- functions with its own executor inside its own transaction, after it locked its own aggregate
-- root FOR UPDATE. They decide on the facts ResolveTradingProfiles and ResolveAddressRoles decide on
-- before that transaction (profile.Usability and the address lookup), and lock the rows those facts
-- live in FOR SHARE until the downstream transaction ends:
--
--   guard_trading_profile_current_v1(party, revision): the trading profile revision row, then the
--   party row, then the bound contacts' rows in contact ID order. trading_profile.publish of the
--   next revision updates the revision row (supersedes it); party.revise, party.confirm and
--   party.deactivate update the party row; contact.revise updates the contact row. Verdicts, in
--   this order: superseded (the revision is not the active one), party_unavailable (the party is
--   not confirmed now), party_changed (the party is confirmed in another revision than the one the
--   revision binds), contact_changed (a bound contact is not confirmed at the bound row version).
--
--   guard_address_role_current_v1(party, party revision, address): the address row of the
--   confirmed revision, then the party row. Verdicts: party_unavailable (the party is not confirmed
--   now), party_revised (the party is confirmed in a later revision).
--
-- So every PTY command that would end a reference either committed before the guard read its row,
-- and the guard reports the reference stale, or waits on the row until the downstream transaction
-- ends; in READ COMMITTED a guard that waited re-reads the row as the command left it. Rows that
-- never change (a confirmed revision's addresses, a published revision's contacts) are read under
-- the lock of the row they belong to; the address row is locked too because the reference names it.
-- A guard that has decided a reference is stale returns without locking the rest: the downstream
-- refuses anyway. No PTY command holds a row these guards lock while it waits for another one:
-- party and contact commands lock their one root row FOR UPDATE; trading_profile.publish holds the
-- profile root, which no guard locks, updates the active revision (where it waits for a guard that
-- holds it) and then the working revision, which no guard locks. So the guards cannot deadlock with
-- PTY commands, in any order of calls.
--
-- Each guard always returns one row. known is false, with code REFERENCE_INVALID, for a reference
-- that does not exist in the caller's scope: an unknown party or revision, an address the revision
-- does not have, a working trading profile revision (not a TradingProfileRef), revision 0. Otherwise
-- code is NULL when the reference serves new business and REFERENCE_STALE with the reason in stale.
-- The data scope (own, team) is the lookups' to check before the transaction; the guards confine
-- every row to app.tenant_id and app.company_id. SECURITY DEFINER runs them as the schema owner,
-- which FORCE ROW LEVEL SECURITY still confines to that scope; the predicates repeat it.
-- search_path is fixed and every name is qualified. brant_app gets EXECUTE in
-- deploy/postgres/10_app_grants.sql; nobody else does.

CREATE FUNCTION brant_pty.guard_trading_profile_current_v1(p_party_id text, p_revision bigint)
    RETURNS TABLE (known boolean, status text, party_revision bigint, content_hash text, code text, stale text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
    v_rev     record;
    v_party   record;
    v_contact record;
    v_stale   text;
BEGIN
    IF p_party_id IS NULL OR p_revision IS NULL THEN
        RAISE EXCEPTION 'brant_pty.guard_trading_profile_current_v1: party_id and revision are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    SELECT r.status, r.party_revision, r.content_hash INTO v_rev
    FROM brant_pty.trading_profile_revision r
    WHERE r.tenant_id = v_tenant AND r.company_id = v_company AND r.party_id = p_party_id AND r.revision = p_revision
      AND r.status IN ('active', 'superseded')
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, NULL::bigint, NULL::text, 'REFERENCE_INVALID'::text, NULL::text;
        RETURN;
    END IF;
    IF v_rev.status <> 'active' THEN
        v_stale := 'superseded';
    ELSE
        SELECT p.status, p.revision INTO v_party
        FROM brant_pty.party p
        WHERE p.tenant_id = v_tenant AND p.company_id = v_company AND p.party_id = p_party_id
        FOR SHARE;
        IF v_party.status <> 'confirmed' THEN
            v_stale := 'party_unavailable';
        ELSIF v_party.revision <> v_rev.party_revision THEN
            v_stale := 'party_changed';
        ELSE
            FOR v_contact IN
                SELECT c.status, c.row_version, t.contact_row_version
                FROM brant_pty.trading_profile_contact t
                JOIN brant_pty.contact c ON c.tenant_id = t.tenant_id AND c.company_id = t.company_id AND c.contact_id = t.contact_id
                WHERE t.tenant_id = v_tenant AND t.company_id = v_company AND t.party_id = p_party_id AND t.revision = p_revision
                ORDER BY t.contact_id COLLATE "C"
                FOR SHARE OF c
            LOOP
                IF v_contact.status <> 'confirmed' OR v_contact.row_version <> v_contact.contact_row_version THEN
                    v_stale := 'contact_changed';
                    EXIT;
                END IF;
            END LOOP;
        END IF;
    END IF;
    RETURN QUERY SELECT true, v_rev.status, v_rev.party_revision, v_rev.content_hash,
        CASE WHEN v_stale IS NOT NULL THEN 'REFERENCE_STALE' END::text, v_stale;
END
$$;

REVOKE ALL ON FUNCTION brant_pty.guard_trading_profile_current_v1(text, bigint) FROM PUBLIC;

CREATE FUNCTION brant_pty.guard_address_role_current_v1(p_party_id text, p_party_revision bigint, p_address_id text)
    RETURNS TABLE (known boolean, role text, party_status text, current_revision bigint, code text, stale text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
    v_role    text;
    v_party   record;
    v_stale   text;
BEGIN
    IF p_party_id IS NULL OR p_party_revision IS NULL OR p_address_id IS NULL THEN
        RAISE EXCEPTION 'brant_pty.guard_address_role_current_v1: party_id, party_revision and address_id are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    SELECT a.role INTO v_role
    FROM brant_pty.party_revision_address a
    WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.party_id = p_party_id AND a.revision = p_party_revision
      AND a.address_id = p_address_id
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, NULL::text, NULL::bigint, 'REFERENCE_INVALID'::text, NULL::text;
        RETURN;
    END IF;
    SELECT p.status, p.revision INTO v_party
    FROM brant_pty.party p
    WHERE p.tenant_id = v_tenant AND p.company_id = v_company AND p.party_id = p_party_id
    FOR SHARE;
    v_stale := CASE
        WHEN v_party.status <> 'confirmed' THEN 'party_unavailable'
        WHEN v_party.revision <> p_party_revision THEN 'party_revised'
    END;
    RETURN QUERY SELECT true, v_role, v_party.status, v_party.revision,
        CASE WHEN v_stale IS NOT NULL THEN 'REFERENCE_STALE' END::text, v_stale;
END
$$;

REVOKE ALL ON FUNCTION brant_pty.guard_address_role_current_v1(text, bigint, text) FROM PUBLIC;
