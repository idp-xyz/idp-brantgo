-- PTY: PartyGuard v1 (Context Map R22 PTY->PUR "PartyRef v1", guardLock true; ADR-BR-004 option B,
-- ADR-BR-011 lock order; R22's risk: a supplier stopped for business still drafted for).
--
-- A downstream context (PUR preparing a purchase draft) binds a party at the row version its source
-- saw: a supplier offer or purchase terms of SUP bind their supplier as {party_id, row_version} when
-- a human verifies them, and that binding is what the downstream adopts. It calls
-- guard_party_current_v1 with its own executor inside its own transaction, after it locked its own
-- aggregate root FOR UPDATE. The function locks the party row FOR SHARE until that transaction ends;
-- party.revise, party.confirm, party.save_draft and party.deactivate update that row and move its
-- row version, so each either committed before the guard read the row, and the verdict is stale, or
-- waits until the downstream transaction ends. In READ COMMITTED a guard that waited re-reads the
-- row as the command left it.
--
-- It always returns one row. known is false, with code REFERENCE_INVALID, for a party that does not
-- exist in the caller's scope. Otherwise code is NULL when the party is confirmed at the bound row
-- version and REFERENCE_STALE with the reason in stale: party_unavailable (not confirmed now:
-- reopened as a draft or deactivated) or party_changed (confirmed at another row version), decided in
-- that order, as ResolveParties tells them apart before the transaction (status, row_version). The
-- roles are the party's now; the downstream checks the role it needs. The data scope (own, team) is
-- the lookup's to check before the transaction; the guard confines the row to app.tenant_id and
-- app.company_id. SECURITY DEFINER runs it as the schema owner, which FORCE ROW LEVEL SECURITY still
-- confines to that scope; the predicates repeat it. search_path is fixed and every name is
-- qualified. brant_app gets EXECUTE in deploy/postgres/10_app_grants.sql; nobody else does.
--
-- No PTY command holds the party row while it waits for another row a guard locks (party commands
-- lock their one root row FOR UPDATE), so the guard cannot deadlock with PTY commands.

CREATE FUNCTION brant_pty.guard_party_current_v1(p_party_id text, p_row_version bigint)
    RETURNS TABLE (known boolean, status text, row_version bigint, revision bigint, roles text[], code text, stale text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
    v_party   record;
    v_stale   text;
BEGIN
    IF p_party_id IS NULL OR p_row_version IS NULL THEN
        RAISE EXCEPTION 'brant_pty.guard_party_current_v1: party_id and row_version are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    SELECT p.status, p.row_version, p.revision, p.roles INTO v_party
    FROM brant_pty.party p
    WHERE p.tenant_id = v_tenant AND p.company_id = v_company AND p.party_id = p_party_id
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, NULL::bigint, NULL::bigint, NULL::text[], 'REFERENCE_INVALID'::text, NULL::text;
        RETURN;
    END IF;
    v_stale := CASE
        WHEN v_party.status <> 'confirmed' THEN 'party_unavailable'
        WHEN v_party.row_version <> p_row_version THEN 'party_changed'
    END;
    RETURN QUERY SELECT true, v_party.status, v_party.row_version, v_party.revision, v_party.roles,
        CASE WHEN v_stale IS NOT NULL THEN 'REFERENCE_STALE' END::text, v_stale;
END
$$;

REVOKE ALL ON FUNCTION brant_pty.guard_party_current_v1(text, bigint) FROM PUBLIC;
