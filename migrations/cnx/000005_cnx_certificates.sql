-- CNX: official certificate results (tactical model CNX-AGG-CertificateResult with its value objects
-- CertificateKind, Issuer, ApplicableObject, Market and ValidityInterval; CNX-INV-007; MD-40; Context Map
-- R56 CertificateResultRef v1; baseline CN-R14, CN-AT14; WBS S5-10 CNX half; S5 decisions E-1 (manual
-- paths), D-17 (certificate.register is human only)). Python V0.50 has no CNX.
--
-- A certificate result is the root: an official result the issuing authority gave — a certificate of
-- origin, a copy of an export licence or an inspection certificate — as an authorized human registered
-- it after verifying the original: the issuer and number, the object it applies to (SKU specification
-- revisions, an FUL export lot or an FUL shipment, with the lines CNX read of it), the destination
-- market, the validity, the EVD file and the verifier. History is never overwritten (只追加；更正形成新
-- 版本): every registration is an immutable revision, numbered from 1 without gaps, sealed as canonical
-- JSON with its SHA-256; a correction is the next revision under the root's FOR UPDATE lock, and the root
-- moves by one row version to it. The root keeps the current revision's key (kind, issuer, number),
-- unique in the company. The kind is fixed by the first registration.
--
-- References to FUL and EVD are plain IDs and hashes: no foreign key crosses a schema (DB2). A licence
-- copy may name an export licence of CNX's own.

CREATE TABLE brant_cnx.certificate_result (
    tenant_id          text        NOT NULL,
    company_id         text        NOT NULL,
    certificate_id     text        NOT NULL CHECK (certificate_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    kind               text        NOT NULL CHECK (kind IN ('certificate_of_origin', 'license_copy', 'inspection_certificate')),
    issuer             text        NOT NULL CHECK (char_length(issuer) BETWEEN 1 AND 200),
    number             text        NOT NULL CHECK (char_length(number) BETWEEN 1 AND 64),
    current_revision   integer     NOT NULL CHECK (current_revision > 0),
    row_version        bigint      NOT NULL CHECK (row_version > 0),
    registered_by_kind text        NOT NULL CHECK (registered_by_kind = 'human'),
    registered_by_id   text        NOT NULL,
    authority_epoch    bigint      NOT NULL CHECK (authority_epoch > 0),
    registered_at      timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, certificate_id),
    -- the issuer's number of a kind names one certificate of the company (its current revision)
    UNIQUE (tenant_id, company_id, kind, issuer, number),
    CHECK (row_version = current_revision),
    CHECK (updated_at >= registered_at)
);

-- A certificate opens at revision 1 and moves by one revision at a time; its identity, kind and first
-- registration never change; nothing is deleted.
CREATE FUNCTION brant_cnx.guard_certificate_result() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cnx.certificate_result: certificates are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.current_revision <> 1 OR NEW.updated_at <> NEW.registered_at THEN
            RAISE EXCEPTION 'brant_cnx.certificate_result: a certificate opens at revision 1' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - ARRAY['issuer', 'number', 'current_revision', 'row_version', 'updated_at'])
            IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['issuer', 'number', 'current_revision', 'row_version', 'updated_at'])
            OR NEW.current_revision <> OLD.current_revision + 1 OR NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'brant_cnx.certificate_result: certificate % keeps its identity and kind and moves by one revision', OLD.certificate_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER certificate_result_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.certificate_result
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_certificate_result();

CREATE TABLE brant_cnx.certificate_revision (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    certificate_id    text        NOT NULL,
    revision          integer     NOT NULL CHECK (revision > 0),
    kind              text        NOT NULL CHECK (kind IN ('certificate_of_origin', 'license_copy', 'inspection_certificate')),
    issuer            text        NOT NULL CHECK (char_length(issuer) BETWEEN 1 AND 200),
    number            text        NOT NULL CHECK (char_length(number) BETWEEN 1 AND 64),
    issued_on         date        NOT NULL,
    object_kind       text        NOT NULL CHECK (object_kind IN ('sku_spec', 'export_lot', 'shipment')),
    object_id         text,
    market            text        NOT NULL CHECK (market ~ '^[A-Z]{2}$'),
    valid_from        timestamptz NOT NULL,
    valid_until       timestamptz NOT NULL,
    file_id           text        NOT NULL,
    file_sha256       text        NOT NULL CHECK (file_sha256 ~ '^[0-9a-f]{64}$'),
    license_id        text,
    document          text        NOT NULL CHECK (left(document, 1) = '{'),
    document_sha256   text        NOT NULL CHECK (document_sha256 ~ '^[0-9a-f]{64}$'),
    reason            text        CHECK (char_length(reason) BETWEEN 1 AND 1000),
    verified_by_kind  text        NOT NULL CHECK (verified_by_kind = 'human'),
    verified_by_id    text        NOT NULL,
    authority_epoch   bigint      NOT NULL CHECK (authority_epoch > 0),
    registered_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, certificate_id, revision),
    FOREIGN KEY (tenant_id, company_id, certificate_id) REFERENCES brant_cnx.certificate_result (tenant_id, company_id, certificate_id),
    FOREIGN KEY (tenant_id, company_id, license_id) REFERENCES brant_cnx.export_license (tenant_id, company_id, license_id),
    CHECK (valid_until > valid_from),
    -- an FUL export lot or shipment is named; SKU specifications are listed in the document
    CHECK ((object_kind = 'sku_spec') = (object_id IS NULL)),
    -- only a licence copy names a licence; every correction states why
    CHECK (license_id IS NULL OR kind = 'license_copy'),
    CHECK ((revision = 1) = (reason IS NULL))
);
CREATE INDEX certificate_revision_object ON brant_cnx.certificate_revision (tenant_id, company_id, object_kind, object_id);

-- Revisions are only inserted, numbered without gaps, under the root's FOR UPDATE lock, after the root
-- moved to them: the revision states the root's kind and current key.
CREATE FUNCTION brant_cnx.guard_certificate_revision() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_root brant_cnx.certificate_result%ROWTYPE;
    v_next integer;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cnx.certificate_revision: revisions never change and are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT * INTO v_root FROM brant_cnx.certificate_result r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
        AND r.certificate_id = NEW.certificate_id FOR UPDATE;
    SELECT 1 + count(*) INTO v_next FROM brant_cnx.certificate_revision v WHERE v.tenant_id = NEW.tenant_id AND v.company_id = NEW.company_id
        AND v.certificate_id = NEW.certificate_id;
    IF NEW.revision <> v_next OR v_root.current_revision <> NEW.revision OR v_root.kind <> NEW.kind OR v_root.issuer <> NEW.issuer
            OR v_root.number <> NEW.number THEN
        RAISE EXCEPTION 'brant_cnx.certificate_revision: revision % of % is the next one and states the certificate''s kind and key',
            NEW.revision, NEW.certificate_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER certificate_revision_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.certificate_revision
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_certificate_revision();

-- At commit, the root stands on a revision that exists (no root moves without its revision).
CREATE FUNCTION brant_cnx.check_certificate_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM brant_cnx.certificate_revision v WHERE v.tenant_id = NEW.tenant_id AND v.company_id = NEW.company_id
            AND v.certificate_id = NEW.certificate_id AND v.revision = NEW.current_revision) THEN
        RAISE EXCEPTION 'brant_cnx.certificate_result: certificate % stands on revision % that was never registered', NEW.certificate_id,
            NEW.current_revision USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER certificate_result_revision_exists AFTER INSERT OR UPDATE ON brant_cnx.certificate_result
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_cnx.check_certificate_revision();

ALTER TABLE brant_cnx.certificate_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.certificate_result FORCE ROW LEVEL SECURITY;
CREATE POLICY certificate_result_scope ON brant_cnx.certificate_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.certificate_revision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.certificate_revision FORCE ROW LEVEL SECURITY;
CREATE POLICY certificate_revision_scope ON brant_cnx.certificate_revision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
