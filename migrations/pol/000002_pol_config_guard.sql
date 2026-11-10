-- POL: ConfigGuard v1 (ADR-BR-004 option B, Context Map R10, R15, R48 "ConfigRef v1＋ConfigGuard") and
-- the identity of company profiles.
--
-- A downstream context (CST, TRD, PUR) calls brant_pol.guard_config_in_force_v1 with its own
-- executor inside its own transaction, after it locked its own aggregate root FOR UPDATE
-- (ADR-BR-011). The function locks the exact revision row FOR SHARE in that transaction. Every POL
-- command that ends the revision updates that row: publishing the next revision supersedes it,
-- retiring retires it. So either the POL command committed first and the guard reports the
-- revision not in force, or the command waits on the row until the downstream transaction ends. In
-- READ COMMITTED a guard that waited re-reads the row as the POL command left it.
--
-- The verdict is the revision's current state: in force when it is active now, was published at or
-- before p_at and its validity contains p_at. It always returns one row; known is false for a
-- revision that does not exist in the caller's scope. SECURITY DEFINER runs it as the schema owner,
-- which FORCE ROW LEVEL SECURITY still confines to app.tenant_id and app.company_id; the scope
-- predicates repeat that. search_path is fixed and every name is qualified. brant_app gets EXECUTE
-- in deploy/postgres/10_app_grants.sql; nobody else does.

CREATE FUNCTION brant_pol.guard_config_in_force_v1(p_kind text, p_config_id text, p_revision bigint, p_at timestamptz)
    RETURNS TABLE (known boolean, status text, code text, content_hash text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog
AS $$
DECLARE
    r record;
BEGIN
    IF p_kind IS NULL OR p_config_id IS NULL OR p_revision IS NULL OR p_at IS NULL THEN
        RAISE EXCEPTION 'brant_pol.guard_config_in_force_v1: kind, config_id, revision and instant are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    SELECT c.status, c.published_at, c.valid_from, c.valid_until, c.content_hash INTO r
    FROM brant_pol.config_revision c
    WHERE c.tenant_id = pg_catalog.current_setting('app.tenant_id', true)
      AND c.company_id = pg_catalog.current_setting('app.company_id', true)
      AND c.kind = p_kind AND c.config_id = p_config_id AND c.revision = p_revision
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, 'REFERENCE_INVALID'::text, NULL::text;
        RETURN;
    END IF;
    RETURN QUERY SELECT true, r.status,
        CASE
            WHEN r.status <> 'active' OR r.published_at > p_at THEN 'CONFIG_NOT_ACTIVE'
            WHEN p_at < r.valid_from OR (r.valid_until IS NOT NULL AND p_at >= r.valid_until) THEN 'CONFIG_EXPIRED'
        END::text,
        r.content_hash;
END
$$;

REVOKE ALL ON FUNCTION brant_pol.guard_config_in_force_v1(text, text, bigint, timestamptz) FROM PUBLIC;

-- A company profile identifies the company of its scope (Python: the profile id is the current
-- company), so a company has at most one profile.
ALTER TABLE brant_pol.config_root ADD CONSTRAINT config_root_company_profile
    CHECK (kind <> 'company_profile' OR config_id = company_id);
