-- EXT (S7-01): company plugin activations. The platform is not a bounded context. Downstream
-- transactions call brant_ext.guard_plugin_active_v1 FOR SHARE after their own root and the
-- authority guard (ADR-BR-011). brant_ext calls no business-context guard, so this directory sits
-- after ref and before evd.
--
-- One company has at most one active cost-providing plugin per document profile. The partial
-- unique index makes two concurrent plugin.enable commands resolve to one commit and one
-- COST_PROVIDER_CONFLICT. History keeps every transition. action_result and audit_event are the
-- command ledger (internal/foundation/action ResultsDDL, schema brant_ext).

CREATE SCHEMA brant_ext;

CREATE TABLE brant_ext.plugin_activation (
    tenant_id         text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id        text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    plugin_id         text        NOT NULL CHECK (plugin_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    document_profile  text        NOT NULL CHECK (document_profile IN ('generic', 'apparel')),
    status            text        NOT NULL CHECK (status IN ('active', 'draining_readonly', 'disabled')),
    version           text        NOT NULL CHECK (char_length(version) BETWEEN 1 AND 64),
    contract_version  text        NOT NULL CHECK (char_length(contract_version) BETWEEN 1 AND 64),
    manifest_sha256   text        NOT NULL CHECK (manifest_sha256 ~ '^[0-9a-f]{64}$'),
    cost_provider     boolean     NOT NULL,
    row_version       bigint      NOT NULL CHECK (row_version > 0),
    authority_epoch   bigint      NOT NULL CHECK (authority_epoch > 0),
    activated_by_kind text        NOT NULL CHECK (activated_by_kind = 'human'),
    activated_by_id   text        NOT NULL,
    updated_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, plugin_id)
);

-- At most one active cost provider for a company and document profile.
CREATE UNIQUE INDEX plugin_activation_one_active_cost
    ON brant_ext.plugin_activation (tenant_id, company_id, document_profile)
    WHERE status = 'active' AND cost_provider;

CREATE TABLE brant_ext.plugin_activation_history (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    plugin_id       text        NOT NULL,
    seq             bigint      NOT NULL CHECK (seq > 0),
    from_status     text        CHECK (from_status IN ('active', 'draining_readonly', 'disabled')),
    to_status       text        NOT NULL CHECK (to_status IN ('active', 'draining_readonly', 'disabled')),
    version         text        NOT NULL,
    manifest_sha256 text        NOT NULL CHECK (manifest_sha256 ~ '^[0-9a-f]{64}$'),
    authority_epoch bigint      NOT NULL CHECK (authority_epoch > 0),
    actor_kind      text        NOT NULL CHECK (actor_kind = 'human'),
    actor_id        text        NOT NULL,
    recorded_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, plugin_id, seq),
    FOREIGN KEY (tenant_id, company_id, plugin_id)
        REFERENCES brant_ext.plugin_activation (tenant_id, company_id, plugin_id)
);

CREATE TABLE brant_ext.action_result (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    command         text        NOT NULL CHECK (command ~ '^[a-z][a-z0-9_]*\.[a-z][a-z0-9_.]*$'),
    idempotency_key text        NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    request_hash    text        NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    action_id       text        NOT NULL,
    object_id       text        NOT NULL,
    row_version     bigint      NOT NULL CHECK (row_version > 0),
    status          text        NOT NULL,
    actor_kind      text        NOT NULL CHECK (actor_kind IN ('human', 'ai', 'system')),
    actor_id        text        NOT NULL,
    authority_epoch bigint      NOT NULL CHECK (authority_epoch > 0),
    correlation_id  text        NOT NULL,
    recorded_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, command, idempotency_key),
    UNIQUE (tenant_id, company_id, action_id)
);

CREATE TABLE brant_ext.audit_event (
    seq             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    event_id        text        NOT NULL UNIQUE,
    actor_kind      text        NOT NULL CHECK (actor_kind IN ('human', 'ai', 'system')),
    actor_id        text        NOT NULL,
    command         text        NOT NULL,
    object_id       text,
    outcome         text        NOT NULL CHECK (outcome IN ('succeeded', 'replayed', 'rejected', 'read')),
    code            text,
    correlation_id  text        NOT NULL,
    recorded_at     timestamptz NOT NULL,
    reason          text,
    authority_epoch bigint,
    CHECK ((outcome = 'rejected') = (code IS NOT NULL))
);

ALTER TABLE brant_ext.plugin_activation ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ext.plugin_activation FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_ext.plugin_activation_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ext.plugin_activation_history FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_ext.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ext.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_ext.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ext.audit_event FORCE ROW LEVEL SECURITY;

CREATE POLICY plugin_activation_scope ON brant_ext.plugin_activation
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY plugin_activation_history_scope ON brant_ext.plugin_activation_history
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY action_result_scope ON brant_ext.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_ext.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

-- PluginGuard v1. The caller has locked its own aggregate FOR UPDATE and authorized (authority
-- rows FOR SHARE). This function locks the activation FOR SHARE in the caller's scope and reports
-- whether that plugin is active for the named document profile. A missing row, a disabled row, a
-- draining row and a different profile are not active (PLUGIN_NOT_ENABLED). SECURITY DEFINER, fixed
-- search_path; FORCE RLS still confines the owner to app.tenant_id and app.company_id.
CREATE FUNCTION brant_ext.guard_plugin_active_v1(p_plugin_id text, p_document_profile text)
    RETURNS TABLE (active boolean, status text, version text, manifest_sha256 text, code text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
    v_row     record;
BEGIN
    IF p_plugin_id IS NULL OR p_document_profile IS NULL THEN
        RAISE EXCEPTION 'brant_ext.guard_plugin_active_v1: plugin_id and document_profile are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    SELECT a.status, a.version, a.manifest_sha256, a.document_profile INTO v_row
    FROM brant_ext.plugin_activation a
    WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.plugin_id = p_plugin_id
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, NULL::text, NULL::text, 'PLUGIN_NOT_ENABLED'::text;
        RETURN;
    END IF;
    IF v_row.status = 'active' AND v_row.document_profile = p_document_profile THEN
        RETURN QUERY SELECT true, v_row.status, v_row.version, v_row.manifest_sha256, NULL::text;
        RETURN;
    END IF;
    RETURN QUERY SELECT false, v_row.status, v_row.version, v_row.manifest_sha256, 'PLUGIN_NOT_ENABLED'::text;
END
$$;

REVOKE ALL ON FUNCTION brant_ext.guard_plugin_active_v1(text, text) FROM PUBLIC;
