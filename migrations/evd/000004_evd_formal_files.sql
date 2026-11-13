-- EVD: TRD's formal documents registered as business files (WBS S4-06c; Context Map R65 TRD→EVD, Published
-- Language events, no guard lock; S4 decision D-8; IG-08 closed on PostgreSQL storage and this consumer, E-9).
-- TRD seals each formal document's bytes and binding in its own schema in the transaction that formalizes a
-- quote revision (QuoteActivated, QuoteReplaced) or generates an order revision's S/C or PI
-- (OrderDocumentGenerated); it never writes EVD in that transaction (tactical model principle 7, S3-D61). EVD's
-- Inbox consumer evd.formal_files reads the document through TRD's FormalDocument read before its own
-- transaction and registers the bytes as an ordinary business file (FileRef v1), once per event, with a
-- formal_file row that records which TRD document it holds: the quote revision and its business hash, for an
-- order document the order revision and its hash, the number, the SHA-256 TRD sealed and the event that
-- registered it.
--
-- The database keeps the rule of R65's risk ("EVD 登记的文件与 TRD 封存的字节不一致"): a formal file names the
-- business file together with the SHA-256 TRD sealed (the foreign key reaches business_file's file and hash),
-- and the insert trigger recomputes the SHA-256 and the size of the stored bytes. One file per TRD document and
-- one per event. Everything stays immutable (EVD-INV-001): the trigger refuses UPDATE and DELETE and brant_app
-- holds only SELECT and INSERT.

-- The business file's ID with its SHA-256, the key a formal file refers to.
ALTER TABLE brant_evd.business_file ADD CONSTRAINT business_file_sha256_key UNIQUE (tenant_id, company_id, file_id, sha256);

CREATE TABLE brant_evd.formal_file (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    file_id           text        NOT NULL,
    document_kind     text        NOT NULL CHECK (document_kind IN ('quote', 'sales_contract', 'proforma_invoice')),
    quote_id          text        NOT NULL CHECK (quote_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    business_revision bigint      NOT NULL CHECK (business_revision >= 1),
    business_hash     text        NOT NULL CHECK (business_hash ~ '^[0-9a-f]{64}$'),
    replaced_revision bigint      CHECK (replaced_revision >= 1 AND replaced_revision <> business_revision),
    order_id          text        CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_revision    bigint      CHECK (order_revision >= 1),
    order_hash        text        CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    number            text        NOT NULL CHECK (char_length(number) BETWEEN 1 AND 64),
    document_sha256   text        NOT NULL CHECK (document_sha256 ~ '^[0-9a-f]{64}$'),
    size_bytes        int         NOT NULL CHECK (size_bytes BETWEEN 1 AND 10485760),
    event_id          text        NOT NULL CHECK (char_length(event_id) BETWEEN 1 AND 128),
    event_type        text        NOT NULL CHECK (event_type IN ('idp-brant.trd.quote_activated.v1', 'idp-brant.trd.quote_replaced.v1',
                                      'idp-brant.trd.order_document_generated.v1')),
    registered_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, file_id),
    UNIQUE (tenant_id, company_id, event_id),
    FOREIGN KEY (tenant_id, company_id, file_id, document_sha256)
        REFERENCES brant_evd.business_file (tenant_id, company_id, file_id, sha256),
    -- A quote revision's document comes from its activation or replacement (only a replacement names the
    -- revision it replaced); an order revision's S/C or PI from its generation, with the order revision.
    CHECK (CASE document_kind
        WHEN 'quote' THEN order_id IS NULL AND order_revision IS NULL AND order_hash IS NULL
            AND (event_type = 'idp-brant.trd.quote_activated.v1' AND replaced_revision IS NULL
              OR event_type = 'idp-brant.trd.quote_replaced.v1' AND replaced_revision IS NOT NULL)
        ELSE order_id IS NOT NULL AND order_revision IS NOT NULL AND order_hash IS NOT NULL AND replaced_revision IS NULL
            AND event_type = 'idp-brant.trd.order_document_generated.v1' END)
);
CREATE UNIQUE INDEX formal_file_quote_document ON brant_evd.formal_file (tenant_id, company_id, quote_id, business_revision)
    WHERE document_kind = 'quote';
CREATE UNIQUE INDEX formal_file_order_document ON brant_evd.formal_file (tenant_id, company_id, order_id, order_revision, document_kind)
    WHERE document_kind <> 'quote';

-- The stored bytes are the ones TRD sealed: their SHA-256 and size, recomputed, whoever writes the row.
CREATE FUNCTION brant_evd.guard_formal_file() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM brant_evd.business_file f
            WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id AND f.file_id = NEW.file_id
              AND f.sha256 = NEW.document_sha256 AND encode(sha256(f.content), 'hex') = NEW.document_sha256
              AND f.byte_size = NEW.size_bytes AND f.supersedes_file_id IS NULL) THEN
        RAISE EXCEPTION 'brant_evd.formal_file: the business file holds other bytes than the document TRD sealed (R65)'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER formal_file_sealed_bytes BEFORE INSERT ON brant_evd.formal_file
    FOR EACH ROW EXECUTE FUNCTION brant_evd.guard_formal_file();
CREATE TRIGGER formal_file_immutable BEFORE UPDATE OR DELETE ON brant_evd.formal_file
    FOR EACH ROW EXECUTE FUNCTION brant_evd.refuse_change();

ALTER TABLE brant_evd.formal_file ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_evd.formal_file FORCE ROW LEVEL SECURITY;
CREATE POLICY formal_file_scope ON brant_evd.formal_file
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
