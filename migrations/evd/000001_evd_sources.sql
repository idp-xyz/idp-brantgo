-- EVD: text sources with their four-layer identity (T01 §18.2; Python BC_T01_reference.sql
-- source_*). SourceContent is deduplicated by SHA-256 within a tenant and company; SourceOrigin is
-- where a text came from, an email identified by provider, account and message ID; SourceImport
-- records each accepted registration. All three are immutable (EVD-INV-001): a trigger refuses
-- UPDATE and DELETE, and brant_app holds only SELECT and INSERT. An origin is owned by the actor who
-- registered it first; the owner's data scope decides who else sees it (BC-D02).
--
-- action_result and audit_event are the command ledger of EVD (internal/foundation/action).

CREATE SCHEMA brant_evd;

CREATE TABLE brant_evd.source_content (
    tenant_id    text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id   text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    content_id   text        NOT NULL,
    sha256       text        NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    content_text text        NOT NULL CHECK (char_length(content_text) BETWEEN 1 AND 50000),
    created_at   timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, content_id),
    UNIQUE (tenant_id, company_id, sha256)
);

CREATE TABLE brant_evd.source_origin (
    tenant_id      text        NOT NULL,
    company_id     text        NOT NULL,
    origin_id      text        NOT NULL CHECK (origin_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    content_id     text        NOT NULL,
    source_kind    text        NOT NULL CHECK (source_kind IN ('manual_text', 'email_text')),
    provider       text        CHECK (provider ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    account_ref    text        CHECK (account_ref ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    message_id     text        CHECK (char_length(message_id) BETWEEN 1 AND 200),
    author_claim   text        CHECK (char_length(author_claim) BETWEEN 1 AND 200),
    owner_actor_id text        NOT NULL,
    owner_team_id  text,
    registered_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, origin_id),
    UNIQUE (tenant_id, company_id, provider, account_ref, message_id),
    FOREIGN KEY (tenant_id, company_id, content_id) REFERENCES brant_evd.source_content (tenant_id, company_id, content_id),
    CHECK (CASE source_kind
        WHEN 'manual_text' THEN provider IS NULL AND account_ref IS NULL AND message_id IS NULL AND author_claim IS NULL
        ELSE provider IS NOT NULL AND account_ref IS NOT NULL AND message_id IS NOT NULL AND author_claim IS NOT NULL END)
);

CREATE TABLE brant_evd.source_import (
    tenant_id   text        NOT NULL,
    company_id  text        NOT NULL,
    import_id   text        NOT NULL,
    origin_id   text        NOT NULL,
    actor_kind  text        NOT NULL CHECK (actor_kind IN ('human', 'ai', 'system')),
    actor_id    text        NOT NULL,
    imported_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, import_id),
    FOREIGN KEY (tenant_id, company_id, origin_id) REFERENCES brant_evd.source_origin (tenant_id, company_id, origin_id)
);
CREATE INDEX source_import_origin ON brant_evd.source_import (tenant_id, company_id, origin_id, imported_at);

CREATE FUNCTION brant_evd.refuse_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'brant_evd.%: sources are immutable (EVD-INV-001)', TG_TABLE_NAME USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER source_content_immutable BEFORE UPDATE OR DELETE ON brant_evd.source_content
    FOR EACH ROW EXECUTE FUNCTION brant_evd.refuse_change();
CREATE TRIGGER source_origin_immutable BEFORE UPDATE OR DELETE ON brant_evd.source_origin
    FOR EACH ROW EXECUTE FUNCTION brant_evd.refuse_change();
CREATE TRIGGER source_import_immutable BEFORE UPDATE OR DELETE ON brant_evd.source_import
    FOR EACH ROW EXECUTE FUNCTION brant_evd.refuse_change();

ALTER TABLE brant_evd.source_content ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_evd.source_content FORCE ROW LEVEL SECURITY;
CREATE POLICY source_content_scope ON brant_evd.source_content
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_evd.source_origin ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_evd.source_origin FORCE ROW LEVEL SECURITY;
CREATE POLICY source_origin_scope ON brant_evd.source_origin
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_evd.source_import ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_evd.source_import FORCE ROW LEVEL SECURITY;
CREATE POLICY source_import_scope ON brant_evd.source_import
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

CREATE TABLE brant_evd.action_result (
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

CREATE TABLE brant_evd.audit_event (
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

ALTER TABLE brant_evd.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_evd.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_evd.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_evd.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_evd.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_evd.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
