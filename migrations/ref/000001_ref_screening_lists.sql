-- REF: screening list reference data (WBS S1-11, ADR-BR-017; internal/reference). Reference data is
-- not a bounded context; it is imported under control from compliance data providers or official
-- sources and read by contexts through internal/reference/api.
--
-- Each company imports the list versions it uses: every table carries tenant_id and company_id
-- under FORCE RLS like every other Brant table. A catalogue shared by all tenants would need an ADR.
--
-- screening_list is one list of a company: its jurisdiction (CN, UN or foreign), which never
-- changes, and its current version, which an import moves forward under the row lock.
-- screening_list_version records each import: source name and URL, publication date, import time,
-- importer and authority epoch, the declared SHA-256 of the source file, the SHA-256 Brant computed
-- over the canonical entries, and the entry count. Versions, entries and the name keys matching
-- uses are immutable: triggers refuse UPDATE and DELETE, and brant_app holds neither.
--
-- brant_ref.screening_list_current_v1 is the guard function reference data publishes (ADR-BR-004
-- option B): a downstream guard reads the current version of a list, holding the list row FOR
-- SHARE when asked, so an import of a new version either committed first or waits.
--
-- action_result and audit_event are the command ledger of reference data
-- (internal/foundation/action).

CREATE SCHEMA brant_ref;

CREATE TABLE brant_ref.screening_list (
    tenant_id       text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id      text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    list_code       text        NOT NULL CHECK (list_code ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,31}$'),
    jurisdiction    text        NOT NULL CHECK (jurisdiction IN ('CN', 'UN', 'foreign')),
    current_version text        NOT NULL,
    row_version     bigint      NOT NULL CHECK (row_version > 0),
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, list_code)
);

CREATE TABLE brant_ref.screening_list_version (
    tenant_id        text        NOT NULL,
    company_id       text        NOT NULL,
    list_code        text        NOT NULL,
    version          text        NOT NULL CHECK (version ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,30}$'),
    source_name      text        NOT NULL CHECK (char_length(source_name) BETWEEN 1 AND 200),
    source_url       text        NOT NULL CHECK (source_url ~ '^https?://' AND char_length(source_url) <= 2000),
    published_on     date        NOT NULL,
    file_sha256      text        NOT NULL CHECK (file_sha256 ~ '^[0-9a-f]{64}$'),
    content_hash     text        NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    entry_count      integer     NOT NULL CHECK (entry_count BETWEEN 1 AND 50000),
    import_seq       bigint      NOT NULL CHECK (import_seq > 0),
    imported_by_kind text        NOT NULL CHECK (imported_by_kind = 'human'),
    imported_by_id   text        NOT NULL,
    authority_epoch  bigint      NOT NULL CHECK (authority_epoch > 0),
    imported_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, list_code, version),
    UNIQUE (tenant_id, company_id, list_code, import_seq),
    FOREIGN KEY (tenant_id, company_id, list_code) REFERENCES brant_ref.screening_list (tenant_id, company_id, list_code)
);

-- A new list row names its first version before that version exists; the check waits for commit.
ALTER TABLE brant_ref.screening_list ADD FOREIGN KEY (tenant_id, company_id, list_code, current_version)
    REFERENCES brant_ref.screening_list_version (tenant_id, company_id, list_code, version) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE brant_ref.screening_list_entry (
    tenant_id    text   NOT NULL,
    company_id   text   NOT NULL,
    list_code    text   NOT NULL,
    version      text   NOT NULL,
    entry_id     text   NOT NULL CHECK (entry_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    primary_name text   NOT NULL CHECK (char_length(primary_name) BETWEEN 1 AND 500),
    aliases      text[] NOT NULL CHECK (cardinality(aliases) <= 100),
    entity_type  text   NOT NULL CHECK (entity_type IN ('entity', 'individual', 'vessel', 'aircraft', 'other')),
    country      text   CHECK (country ~ '^[A-Z]{2}$'),
    content_hash text   NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (tenant_id, company_id, list_code, version, entry_id),
    FOREIGN KEY (tenant_id, company_id, list_code, version)
        REFERENCES brant_ref.screening_list_version (tenant_id, company_id, list_code, version)
);

-- The keys of every entry's primary name and aliases under name-key-v1, for exact matching.
CREATE TABLE brant_ref.screening_entry_name (
    tenant_id  text NOT NULL,
    company_id text NOT NULL,
    list_code  text NOT NULL,
    version    text NOT NULL,
    name_key   text NOT NULL CHECK (octet_length(name_key) BETWEEN 1 AND 1000),
    entry_id   text NOT NULL,
    name       text NOT NULL,
    PRIMARY KEY (tenant_id, company_id, list_code, version, name_key, entry_id),
    FOREIGN KEY (tenant_id, company_id, list_code, version, entry_id)
        REFERENCES brant_ref.screening_list_entry (tenant_id, company_id, list_code, version, entry_id)
);

-- A list keeps its identity and jurisdiction; its import counter only counts up; it is never deleted.
CREATE FUNCTION brant_ref.guard_list() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_ref.screening_list: lists are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.list_code, NEW.jurisdiction, NEW.created_at)
            IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.list_code, OLD.jurisdiction, OLD.created_at)
       OR NEW.row_version <> OLD.row_version + 1 THEN
        RAISE EXCEPTION 'brant_ref.screening_list: a list keeps its jurisdiction and counts its imports'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER screening_list_guard BEFORE UPDATE OR DELETE ON brant_ref.screening_list
    FOR EACH ROW EXECUTE FUNCTION brant_ref.guard_list();

CREATE FUNCTION brant_ref.refuse_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'brant_ref.%: imported screening list data is immutable', TG_TABLE_NAME
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER screening_list_version_immutable BEFORE UPDATE OR DELETE ON brant_ref.screening_list_version
    FOR EACH ROW EXECUTE FUNCTION brant_ref.refuse_change();
CREATE TRIGGER screening_list_entry_immutable BEFORE UPDATE OR DELETE ON brant_ref.screening_list_entry
    FOR EACH ROW EXECUTE FUNCTION brant_ref.refuse_change();
CREATE TRIGGER screening_entry_name_immutable BEFORE UPDATE OR DELETE ON brant_ref.screening_entry_name
    FOR EACH ROW EXECUTE FUNCTION brant_ref.refuse_change();

ALTER TABLE brant_ref.screening_list ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ref.screening_list FORCE ROW LEVEL SECURITY;
CREATE POLICY screening_list_scope ON brant_ref.screening_list
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ref.screening_list_version ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ref.screening_list_version FORCE ROW LEVEL SECURITY;
CREATE POLICY screening_list_version_scope ON brant_ref.screening_list_version
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ref.screening_list_entry ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ref.screening_list_entry FORCE ROW LEVEL SECURITY;
CREATE POLICY screening_list_entry_scope ON brant_ref.screening_list_entry
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ref.screening_entry_name ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ref.screening_entry_name FORCE ROW LEVEL SECURITY;
CREATE POLICY screening_entry_name_scope ON brant_ref.screening_entry_name
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

-- The guard function of reference data: the current version of a list in the caller's scope, or
-- NULL, holding the list row FOR SHARE when p_lock.
CREATE FUNCTION brant_ref.screening_list_current_v1(p_list_code text, p_lock boolean)
    RETURNS text
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_version text;
BEGIN
    IF p_lock THEN
        SELECT l.current_version INTO v_version FROM brant_ref.screening_list l
        WHERE l.tenant_id = current_setting('app.tenant_id', true) AND l.company_id = current_setting('app.company_id', true)
          AND l.list_code = p_list_code
        FOR SHARE;
    ELSE
        SELECT l.current_version INTO v_version FROM brant_ref.screening_list l
        WHERE l.tenant_id = current_setting('app.tenant_id', true) AND l.company_id = current_setting('app.company_id', true)
          AND l.list_code = p_list_code;
    END IF;
    RETURN v_version;
END
$$;
REVOKE ALL ON FUNCTION brant_ref.screening_list_current_v1(text, boolean) FROM PUBLIC;

CREATE TABLE brant_ref.action_result (
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

CREATE TABLE brant_ref.audit_event (
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

ALTER TABLE brant_ref.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ref.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_ref.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ref.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_ref.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_ref.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
