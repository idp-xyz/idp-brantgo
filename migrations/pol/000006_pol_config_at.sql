-- POL: ConfigAt v1 (S5R-D23, simulated sign-off E0 of 2026-11-13; tactical model POL-INV-007; Context
-- Map R50 POL->CNX "ConfigAt v1（某种配置在时点 T 有效的修订，含已被取代的修订；修订不可变，不加锁）").
--
-- brant_pol.config_at_v1 answers which revision of a configuration applies at an instant by its
-- validity, whenever it was published: of the revisions ever published (active, superseded or
-- retired now) whose validity [valid_from, valid_until) contains p_at, the one published last
-- (revision number breaks a tie). A retirement ends the configuration: once a revision is retired at
-- or before p_at, no revision published before that retirement applies at p_at, so an older
-- superseded revision does not come back; a revision published after it applies again. CNX binds the
-- export_tax_policy of an export to the revision valid at the export date with it (财税〔2012〕39号:
-- the 出口日期 on the declaration decides), so a revision published after the export applies to it
-- when its validity covers it. ConfigGuard v1 (000002) decides the current state instead.
--
-- It always returns one row: known is false (code REFERENCE_INVALID) when the configuration does not
-- exist in the caller's scope; code is CONFIG_NOT_ACTIVE when no published revision applies;
-- otherwise revision, status (now) and content_hash name the revision that applies. Published
-- revisions never change, so it takes no lock: a downstream calls it with its own executor inside
-- its own transaction, or asks POL's Go query ConfigAt before it (same rule, config.At).
--
-- SECURITY DEFINER runs it as the schema owner, which FORCE ROW LEVEL SECURITY still confines to
-- app.tenant_id and app.company_id; the scope predicates repeat that. search_path is fixed and every
-- name is qualified. brant_app gets EXECUTE in deploy/postgres/10_app_grants.sql; nobody else does.

CREATE FUNCTION brant_pol.config_at_v1(p_kind text, p_config_id text, p_at timestamptz)
    RETURNS TABLE (known boolean, revision bigint, status text, code text, content_hash text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
    v_retired timestamptz;
    r         record;
BEGIN
    IF p_kind IS NULL OR p_config_id IS NULL OR p_at IS NULL THEN
        RAISE EXCEPTION 'brant_pol.config_at_v1: kind, config_id and instant are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_pol.config_root c
            WHERE c.tenant_id = v_tenant AND c.company_id = v_company AND c.kind = p_kind AND c.config_id = p_config_id) THEN
        RETURN QUERY SELECT false, NULL::bigint, NULL::text, 'REFERENCE_INVALID'::text, NULL::text;
        RETURN;
    END IF;
    -- The latest retirement at or before the instant.
    SELECT max(c.ended_at) INTO v_retired
    FROM brant_pol.config_revision c
    WHERE c.tenant_id = v_tenant AND c.company_id = v_company AND c.kind = p_kind AND c.config_id = p_config_id
      AND c.status = 'retired' AND c.ended_at <= p_at;
    SELECT c.revision, c.status, c.content_hash INTO r
    FROM brant_pol.config_revision c
    WHERE c.tenant_id = v_tenant AND c.company_id = v_company AND c.kind = p_kind AND c.config_id = p_config_id
      AND c.published_at IS NOT NULL AND c.status IN ('active', 'superseded', 'retired')
      AND (v_retired IS NULL OR c.published_at > v_retired)
      AND c.valid_from <= p_at AND (c.valid_until IS NULL OR p_at < c.valid_until)
    ORDER BY c.published_at DESC, c.revision DESC
    LIMIT 1;
    IF NOT FOUND THEN
        RETURN QUERY SELECT true, NULL::bigint, NULL::text, 'CONFIG_NOT_ACTIVE'::text, NULL::text;
        RETURN;
    END IF;
    RETURN QUERY SELECT true, r.revision, r.status, NULL::text, r.content_hash;
END
$$;

REVOKE ALL ON FUNCTION brant_pol.config_at_v1(text, text, timestamptz) FROM PUBLIC;
