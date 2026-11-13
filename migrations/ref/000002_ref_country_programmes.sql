-- REF: country programme reference data (S5 A3b; owner decision E-3 of 2026-11-12, ADR-BR-017;
-- UL CNX-EmbargoCheck; internal/reference). The embargo, sanctions and export-control programmes
-- that name countries, imported under control by compliance (capability country_programme.import,
-- exclusive to the compliance role template) and read by CNX's trade compliance check, which
-- compares them with the destination and the parties' countries of an order revision
-- (CNX-INV-001, D-8). A programme naming a country is a candidate finding for a human decision,
-- never a conclusion; which lists a company uses is its screening_regulation's to say.
--
-- The tables follow the screening lists of 000001: each company imports the versions it uses
-- (tenant_id and company_id under FORCE RLS); country_programme_list is one list of a company with
-- its jurisdiction (CN, UN or foreign), which never changes, and its current version, which an
-- import moves forward under the row lock; country_programme_list_version records each import:
-- source name and URL, publication date, import time, importer and authority epoch, the declared
-- SHA-256 of the source file, the SHA-256 Brant computed over the canonical programmes, and the
-- programme and country counts; country_programme holds the programmes with their measure, the
-- sorted ISO 3166-1 alpha-2 codes they name and, optionally, their scope. Versions and programmes
-- are immutable: triggers refuse UPDATE and DELETE, and brant_app holds neither.
--
-- brant_ref.country_programme_list_current_v1 is the guard function reference data publishes
-- (ADR-BR-004 option B): a downstream transaction reads the current version of a list, holding the
-- list row FOR SHARE when asked, so an import of a new version either committed first or waits.
-- The command ledger is reference data's (action_result, audit_event of 000001).

CREATE TABLE brant_ref.country_programme_list (
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

CREATE TABLE brant_ref.country_programme_list_version (
    tenant_id        text        NOT NULL,
    company_id       text        NOT NULL,
    list_code        text        NOT NULL,
    version          text        NOT NULL CHECK (version ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,30}$'),
    source_name      text        NOT NULL CHECK (char_length(source_name) BETWEEN 1 AND 200),
    source_url       text        NOT NULL CHECK (source_url ~ '^https?://' AND char_length(source_url) <= 2000),
    published_on     date        NOT NULL,
    file_sha256      text        NOT NULL CHECK (file_sha256 ~ '^[0-9a-f]{64}$'),
    content_hash     text        NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    programme_count  integer     NOT NULL CHECK (programme_count BETWEEN 1 AND 500),
    country_count    integer     NOT NULL CHECK (country_count BETWEEN 1 AND 249),
    import_seq       bigint      NOT NULL CHECK (import_seq > 0),
    imported_by_kind text        NOT NULL CHECK (imported_by_kind = 'human'),
    imported_by_id   text        NOT NULL,
    authority_epoch  bigint      NOT NULL CHECK (authority_epoch > 0),
    imported_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, list_code, version),
    UNIQUE (tenant_id, company_id, list_code, import_seq),
    FOREIGN KEY (tenant_id, company_id, list_code) REFERENCES brant_ref.country_programme_list (tenant_id, company_id, list_code)
);

-- A new list row names its first version before that version exists; the check waits for commit.
ALTER TABLE brant_ref.country_programme_list ADD FOREIGN KEY (tenant_id, company_id, list_code, current_version)
    REFERENCES brant_ref.country_programme_list_version (tenant_id, company_id, list_code, version) DEFERRABLE INITIALLY DEFERRED;

-- canonical_country_codes reports whether codes are two-letter upper-case codes, each once, in
-- ascending order: the canonical form of a programme's countries. Reference data admits officially
-- assigned codes only (kernel.ValidCountryCode); the database checks the form.
CREATE FUNCTION brant_ref.canonical_country_codes(p_codes text[]) RETURNS boolean
    LANGUAGE sql IMMUTABLE STRICT SET search_path = pg_catalog, pg_temp
AS $$
    SELECT cardinality(p_codes) BETWEEN 1 AND 249
       AND pg_catalog.array_to_string(p_codes, ',') ~ '^[A-Z]{2}(,[A-Z]{2})*$'
       AND p_codes = ARRAY(SELECT DISTINCT c COLLATE "C" FROM pg_catalog.unnest(p_codes) AS u(c) ORDER BY 1)
$$;

CREATE TABLE brant_ref.country_programme (
    tenant_id    text   NOT NULL,
    company_id   text   NOT NULL,
    list_code    text   NOT NULL,
    version      text   NOT NULL,
    programme_id text   NOT NULL CHECK (programme_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    name         text   NOT NULL CHECK (char_length(name) BETWEEN 1 AND 500),
    measure      text   NOT NULL CHECK (measure IN ('embargo', 'arms_embargo', 'sanctions', 'export_control')),
    countries    text[] NOT NULL CHECK (brant_ref.canonical_country_codes(countries)),
    scope        text   CHECK (char_length(scope) BETWEEN 1 AND 2000),
    content_hash text   NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (tenant_id, company_id, list_code, version, programme_id),
    FOREIGN KEY (tenant_id, company_id, list_code, version)
        REFERENCES brant_ref.country_programme_list_version (tenant_id, company_id, list_code, version)
);

-- A list keeps its identity and jurisdiction; its import counter only counts up; it is never deleted.
CREATE FUNCTION brant_ref.guard_country_programme_list() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_ref.country_programme_list: lists are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.list_code, NEW.jurisdiction, NEW.created_at)
            IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.list_code, OLD.jurisdiction, OLD.created_at)
       OR NEW.row_version <> OLD.row_version + 1 THEN
        RAISE EXCEPTION 'brant_ref.country_programme_list: a list keeps its jurisdiction and counts its imports'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER country_programme_list_guard BEFORE UPDATE OR DELETE ON brant_ref.country_programme_list
    FOR EACH ROW EXECUTE FUNCTION brant_ref.guard_country_programme_list();

CREATE FUNCTION brant_ref.refuse_programme_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'brant_ref.%: imported country programme data is immutable', TG_TABLE_NAME
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER country_programme_list_version_immutable BEFORE UPDATE OR DELETE ON brant_ref.country_programme_list_version
    FOR EACH ROW EXECUTE FUNCTION brant_ref.refuse_programme_change();
CREATE TRIGGER country_programme_immutable BEFORE UPDATE OR DELETE ON brant_ref.country_programme
    FOR EACH ROW EXECUTE FUNCTION brant_ref.refuse_programme_change();

ALTER TABLE brant_ref.country_programme_list ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ref.country_programme_list FORCE ROW LEVEL SECURITY;
CREATE POLICY country_programme_list_scope ON brant_ref.country_programme_list
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ref.country_programme_list_version ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ref.country_programme_list_version FORCE ROW LEVEL SECURITY;
CREATE POLICY country_programme_list_version_scope ON brant_ref.country_programme_list_version
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ref.country_programme ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ref.country_programme FORCE ROW LEVEL SECURITY;
CREATE POLICY country_programme_scope ON brant_ref.country_programme
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

-- The guard function of country programme lists: the current version of a list in the caller's
-- scope, or NULL, holding the list row FOR SHARE when p_lock.
CREATE FUNCTION brant_ref.country_programme_list_current_v1(p_list_code text, p_lock boolean)
    RETURNS text
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_version text;
BEGIN
    IF p_lock THEN
        SELECT l.current_version INTO v_version FROM brant_ref.country_programme_list l
        WHERE l.tenant_id = current_setting('app.tenant_id', true) AND l.company_id = current_setting('app.company_id', true)
          AND l.list_code = p_list_code
        FOR SHARE;
    ELSE
        SELECT l.current_version INTO v_version FROM brant_ref.country_programme_list l
        WHERE l.tenant_id = current_setting('app.tenant_id', true) AND l.company_id = current_setting('app.company_id', true)
          AND l.list_code = p_list_code;
    END IF;
    RETURN v_version;
END
$$;
REVOKE ALL ON FUNCTION brant_ref.country_programme_list_current_v1(text, boolean) FROM PUBLIC;
