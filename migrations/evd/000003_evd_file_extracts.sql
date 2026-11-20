-- EVD: file extracts as sources (WBS S4-04b; Context Map EVD boundary "不解析 PDF／XLSX（解析产生候选，
-- 属 AI 能力层）", ADR-BR-010). A source of kind file_extract is the text the AI layer's deterministic
-- extractor (internal/ai/ingest) read from a business file under an input profile (baseline 14.2,
-- Q01.upload.v0.1). EVD holds no parsing code: bootstrap gives it the extractor, EVD keeps what it
-- read. The extract's plain text (its segments joined by line feeds, at most 50,000 characters by
-- the profile) is the origin's source_content like any other source; file_extract holds the located
-- form the locators point into (brant-extract/1: a header, one record per segment with its locator,
-- the reader's notes), its SHA-256, the extractor version that defines the locators and the file it
-- was read from. One extract per file, extractor version and profile, shared by the origins that
-- register it. Everything stays immutable (EVD-INV-001): the trigger refuses UPDATE and DELETE and brant_app
-- holds only SELECT and INSERT. A reader sees a file_extract origin only while seeing both the origin
-- and the file (CT16); the application re-checks that on every read, lookup and replay.

CREATE TABLE brant_evd.file_extract (
    tenant_id    text        NOT NULL,
    company_id   text        NOT NULL,
    extract_id   text        NOT NULL,
    file_id      text        NOT NULL,
    file_sha256  text        NOT NULL CHECK (file_sha256 ~ '^[0-9a-f]{64}$'),
    extract_kind text        NOT NULL CHECK (extract_kind IN ('text', 'pdf', 'xlsx')),
    extractor    text        NOT NULL CHECK (char_length(extractor) BETWEEN 1 AND 64),
    profile      text        NOT NULL CHECK (char_length(profile) BETWEEN 1 AND 64),
    characters   int         NOT NULL CHECK (characters BETWEEN 1 AND 50000),
    pages        int         NOT NULL CHECK (pages BETWEEN 0 AND 100000),
    sheets       text[]      NOT NULL CHECK (cardinality(sheets) <= 1000),
    sha256       text        NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    located_form text        NOT NULL CHECK (octet_length(located_form) BETWEEN 1 AND 8388608),
    created_at   timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, extract_id),
    UNIQUE (tenant_id, company_id, file_id, extractor, profile),
    FOREIGN KEY (tenant_id, company_id, file_id) REFERENCES brant_evd.business_file (tenant_id, company_id, file_id),
    -- The hash is that of the located form, whoever wrote the row.
    CHECK (sha256 = encode(sha256(convert_to(located_form, 'UTF8')), 'hex'))
);

CREATE TRIGGER file_extract_immutable BEFORE UPDATE OR DELETE ON brant_evd.file_extract
    FOR EACH ROW EXECUTE FUNCTION brant_evd.refuse_change();

ALTER TABLE brant_evd.file_extract ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_evd.file_extract FORCE ROW LEVEL SECURITY;
CREATE POLICY file_extract_scope ON brant_evd.file_extract
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

-- An origin of kind file_extract names its extract and has no channel identity.
ALTER TABLE brant_evd.source_origin ADD COLUMN extract_id text;
ALTER TABLE brant_evd.source_origin ADD CONSTRAINT source_origin_extract_fkey FOREIGN KEY (tenant_id, company_id, extract_id)
    REFERENCES brant_evd.file_extract (tenant_id, company_id, extract_id);
ALTER TABLE brant_evd.source_origin DROP CONSTRAINT source_origin_source_kind_check;
ALTER TABLE brant_evd.source_origin ADD CONSTRAINT source_origin_source_kind_check
    CHECK (source_kind IN ('manual_text', 'email_text', 'file_extract'));
ALTER TABLE brant_evd.source_origin DROP CONSTRAINT source_origin_check;
ALTER TABLE brant_evd.source_origin ADD CONSTRAINT source_origin_channel_check CHECK (CASE source_kind
    WHEN 'email_text' THEN provider IS NOT NULL AND account_ref IS NOT NULL AND message_id IS NOT NULL AND author_claim IS NOT NULL
    ELSE provider IS NULL AND account_ref IS NULL AND message_id IS NULL AND author_claim IS NULL END);
ALTER TABLE brant_evd.source_origin ADD CONSTRAINT source_origin_extract_check
    CHECK ((source_kind = 'file_extract') = (extract_id IS NOT NULL));
