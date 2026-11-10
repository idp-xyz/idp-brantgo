-- EVD: business files (Python foundation business_file). The bytes, their SHA-256 and the declared
-- type are immutable (EVD-INV-001): a trigger refuses UPDATE and DELETE and brant_app holds only
-- SELECT and INSERT. A new version names the file it supersedes; the unique index lets a file have
-- at most one successor, so versions never branch. At most 10 MiB per file.

CREATE TABLE brant_evd.business_file (
    tenant_id          text        NOT NULL,
    company_id         text        NOT NULL,
    file_id            text        NOT NULL CHECK (file_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    file_name          text        NOT NULL CHECK (char_length(file_name) BETWEEN 1 AND 255 AND file_name !~ '[/\\]'),
    mime_type          text        NOT NULL CHECK (mime_type IN ('text/plain', 'application/pdf',
                                       'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet')),
    content            bytea       NOT NULL,
    sha256             text        NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    byte_size          int         NOT NULL CHECK (byte_size BETWEEN 1 AND 10485760 AND byte_size = octet_length(content)),
    supersedes_file_id text,
    owner_actor_id     text        NOT NULL,
    owner_team_id      text,
    registered_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, file_id),
    FOREIGN KEY (tenant_id, company_id, supersedes_file_id) REFERENCES brant_evd.business_file (tenant_id, company_id, file_id),
    CHECK (supersedes_file_id IS DISTINCT FROM file_id)
);
CREATE UNIQUE INDEX business_file_one_successor ON brant_evd.business_file (tenant_id, company_id, supersedes_file_id)
    WHERE supersedes_file_id IS NOT NULL;

CREATE TRIGGER business_file_immutable BEFORE UPDATE OR DELETE ON brant_evd.business_file
    FOR EACH ROW EXECUTE FUNCTION brant_evd.refuse_change();

ALTER TABLE brant_evd.business_file ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_evd.business_file FORCE ROW LEVEL SECURITY;
CREATE POLICY business_file_scope ON brant_evd.business_file
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
