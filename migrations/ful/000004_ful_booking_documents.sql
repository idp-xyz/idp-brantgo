-- FUL: bookings of shipments and shipment document sets (tactical model FUL-AGG-Shipment entity
-- BookingRecord, FUL-AGG-ShipmentDocumentSet, SM-FUL-DOCUMENT, FUL-INV-004; UL FUL-BookingRecord,
-- FUL-ShipmentDocumentSet; master data MD-39; Context Map R44 PTY->FUL, R49 POL->FUL, R74 EVD->FUL;
-- baseline BT12, CN-AT17, INT-05; WBS S5-08; owner decisions D-12, D-17).
--
-- A booking is recorded on a prepared or confirmed shipment in versions numbered from 1: a
-- correction is a new version with its reason, every version is kept, nothing changes or is deleted.
-- It names the forwarder's or carrier's PTY service role revision and cites the evidence of its
-- confirmation. The shipment's own row and version do not move.
--
-- A document set belongs to one shipment (one set per shipment) and holds versions of four kinds:
-- the commercial invoice and the packing list, which FUL renders with shipment-text/0.1.0 into one
-- file per language, and the records of the external bill of lading and certificate of origin, an
-- EVD file each. A version is inserted as a draft with its content (canonical JSON and its SHA-256),
-- number, POL revisions and their content hashes, sources, commit evidence and, for a rendered kind,
-- its renditions' bytes: bytes, hash, number and commit evidence in one transaction. It then moves
-- once from draft to issued or superseded, or from issued to superseded (SM-FUL-DOCUMENT): a kind has
-- at most one draft and one issued version at a time, and nothing else of a version ever changes. An
-- issue records the versions it issued, the differences it accepted with their reasons and the
-- reconciliation's SHA-256. A signed or stamped copy of one language of an issued version is an EVD
-- file whose SHA-256 is never one of the version's generated files; it is recorded once.
--
-- A documents release (document_release.record, S5-07's commercial release) is recorded on the
-- issued bill of lading record of the shipment's document set: its consumption names the set, the
-- B/L version, its number and content SHA-256 (new nullable columns; releases consumed before this
-- migration keep none), and the release trigger refuses a documents consumption without an issued
-- B/L version of that shipment.
--
-- References to PTY, POL and EVD are plain IDs and hashes: no foreign key crosses a schema (DB2).

CREATE TABLE brant_ful.shipment_booking (
    tenant_id             text        NOT NULL,
    company_id            text        NOT NULL,
    shipment_id           text        NOT NULL,
    version               bigint      NOT NULL CHECK (version > 0),
    service_party_id      text        NOT NULL CHECK (service_party_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    service_role_id       text        NOT NULL CHECK (service_role_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    service_role_revision bigint      NOT NULL CHECK (service_role_revision > 0),
    service_kind          text        NOT NULL CHECK (service_kind IN ('forwarder', 'carrier')),
    booking_number        text        NOT NULL CHECK (char_length(booking_number) BETWEEN 1 AND 64),
    vessel                text        NOT NULL CHECK (char_length(vessel) BETWEEN 1 AND 100),
    voyage                text        NOT NULL CHECK (char_length(voyage) BETWEEN 1 AND 32),
    etd                   date        NOT NULL,
    cut_off               timestamptz NOT NULL,
    container_type        text        NOT NULL CHECK (container_type ~ '^[A-Z0-9]{2,10}$'),
    container_count       integer     NOT NULL CHECK (container_count BETWEEN 1 AND 999),
    port_of_loading       text        NOT NULL CHECK (port_of_loading ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
    port_of_discharge     text        NOT NULL CHECK (port_of_discharge ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
    evidence_ids          text[]      NOT NULL CHECK (cardinality(evidence_ids) BETWEEN 1 AND 20),
    reason                text        CHECK (char_length(reason) BETWEEN 1 AND 1000),
    recorded_by_kind      text        NOT NULL CHECK (recorded_by_kind IN ('human', 'ai')),
    recorded_by_id        text        NOT NULL,
    record_epoch          bigint      NOT NULL CHECK (record_epoch > 0),
    recorded_at           timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, shipment_id, version),
    FOREIGN KEY (tenant_id, company_id, shipment_id) REFERENCES brant_ful.shipment (tenant_id, company_id, shipment_id),
    -- a correction states its reason
    CHECK (version = 1 OR reason IS NOT NULL),
    CHECK (cut_off < (etd + 1)::timestamp AT TIME ZONE 'UTC')
);

-- A booking version is only inserted, as the next version of a shipment that is not cancelled.
CREATE FUNCTION brant_ful.guard_shipment_booking() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_ful.shipment_booking: booking versions never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_ful.shipment s WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id
            AND s.shipment_id = NEW.shipment_id AND s.status IN ('prepared', 'confirmed')) THEN
        RAISE EXCEPTION 'brant_ful.shipment_booking: shipment % is cancelled or unknown', NEW.shipment_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.version <> 1 + coalesce((SELECT max(b.version) FROM brant_ful.shipment_booking b WHERE b.tenant_id = NEW.tenant_id
            AND b.company_id = NEW.company_id AND b.shipment_id = NEW.shipment_id), 0) THEN
        RAISE EXCEPTION 'brant_ful.shipment_booking: version % of shipment % is not the next', NEW.version, NEW.shipment_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER shipment_booking_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.shipment_booking
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_shipment_booking();

CREATE TABLE brant_ful.shipment_document_set (
    tenant_id       text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id      text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    document_set_id text        NOT NULL CHECK (document_set_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    shipment_id     text        NOT NULL,
    order_id        text        NOT NULL,
    order_revision  bigint      NOT NULL CHECK (order_revision > 0),
    order_hash      text        NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    row_version     bigint      NOT NULL CHECK (row_version > 0),
    document_count  bigint      NOT NULL CHECK (document_count > 0),
    issue_count     bigint      NOT NULL CHECK (issue_count >= 0),
    owner_actor_id  text        NOT NULL,
    owner_team_id   text,
    created_by_kind text        NOT NULL CHECK (created_by_kind IN ('human', 'ai')),
    created_by_id   text        NOT NULL,
    create_epoch    bigint      NOT NULL CHECK (create_epoch > 0),
    created_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, document_set_id),
    -- one set per shipment
    UNIQUE (tenant_id, company_id, shipment_id),
    FOREIGN KEY (tenant_id, company_id, shipment_id) REFERENCES brant_ful.shipment (tenant_id, company_id, shipment_id),
    CHECK (owner_actor_id = created_by_id),
    CHECK (row_version = document_count + issue_count)
);

-- A set is inserted with its first version at row version 1 and then moves only by one more version
-- or one more issue, one at a time; it is never deleted.
CREATE FUNCTION brant_ful.guard_shipment_document_set() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_moving text[] := ARRAY['row_version', 'document_count', 'issue_count'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_ful.shipment_document_set: sets are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.row_version <> 1 OR NEW.document_count <> 1 OR NEW.issue_count <> 0 THEN
            RAISE EXCEPTION 'brant_ful.shipment_document_set: a set is inserted with its first version at row version 1'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.row_version = OLD.row_version + 1 AND (to_jsonb(NEW) - v_moving) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_moving)
            AND ((NEW.document_count = OLD.document_count + 1 AND NEW.issue_count = OLD.issue_count)
              OR (NEW.issue_count = OLD.issue_count + 1 AND NEW.document_count = OLD.document_count)) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_ful.shipment_document_set: set % changes only by one version or one issue at a time', OLD.document_set_id
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER shipment_document_set_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.shipment_document_set
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_shipment_document_set();

CREATE TABLE brant_ful.shipment_document (
    tenant_id             text        NOT NULL,
    company_id            text        NOT NULL,
    document_set_id       text        NOT NULL,
    document_kind         text        NOT NULL CHECK (document_kind IN ('commercial_invoice', 'packing_list', 'bill_of_lading',
                                          'certificate_of_origin')),
    version               bigint      NOT NULL CHECK (version > 0),
    status                text        NOT NULL CHECK (status IN ('draft', 'issued', 'superseded')),
    set_version           bigint      NOT NULL CHECK (set_version > 0),
    number                text        NOT NULL CHECK (char_length(number) BETWEEN 1 AND 64),
    sequence_kind         text        CHECK (sequence_kind = 'number_sequence'),
    sequence_id           text,
    sequence_revision     bigint      CHECK (sequence_revision > 0),
    sequence_hash         text        CHECK (sequence_hash ~ '^[0-9a-f]{64}$'),
    seller_kind           text        CHECK (seller_kind = 'company_profile'),
    seller_id             text,
    seller_revision       bigint      CHECK (seller_revision > 0),
    seller_hash           text        CHECK (seller_hash ~ '^[0-9a-f]{64}$'),
    renderer_version      text        CHECK (renderer_version = 'shipment-text/0.1.0'),
    content               text        NOT NULL,
    content_sha256        text        NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    file_id               text,
    file_sha256           text        CHECK (file_sha256 ~ '^[0-9a-f]{64}$'),
    file_mime_type        text,
    sources               text        NOT NULL,
    evidence_ids          text[]      NOT NULL CHECK (cardinality(evidence_ids) <= 20),
    idempotency_key       text        NOT NULL,
    prepared_by_kind      text        NOT NULL CHECK (prepared_by_kind IN ('human', 'ai')),
    prepared_by_id        text        NOT NULL,
    prepare_epoch         bigint      NOT NULL CHECK (prepare_epoch > 0),
    prepared_at           timestamptz NOT NULL,
    -- the version's commit evidence as canonical JSON ([{"name": ..., "value": ...}, ...]) and its SHA-256
    commit_entries        text        NOT NULL,
    commit_entries_sha256 text        NOT NULL CHECK (commit_entries_sha256 ~ '^[0-9a-f]{64}$'),
    issue_seq             bigint      CHECK (issue_seq > 0),
    issued_at             timestamptz,
    superseded_at         timestamptz,
    PRIMARY KEY (tenant_id, company_id, document_set_id, document_kind, version),
    FOREIGN KEY (tenant_id, company_id, document_set_id) REFERENCES brant_ful.shipment_document_set (tenant_id, company_id, document_set_id),
    CHECK (content_sha256 = encode(sha256(convert_to(content, 'UTF8')), 'hex')),
    CHECK (commit_entries_sha256 = encode(sha256(convert_to(commit_entries, 'UTF8')), 'hex')),
    -- a rendered kind names its POL revisions and renderer; a record of an external document its EVD file
    CHECK (CASE WHEN document_kind IN ('commercial_invoice', 'packing_list')
                THEN sequence_id IS NOT NULL AND sequence_revision IS NOT NULL AND sequence_hash IS NOT NULL AND seller_id IS NOT NULL
                     AND seller_revision IS NOT NULL AND seller_hash IS NOT NULL AND renderer_version IS NOT NULL AND file_id IS NULL
                ELSE file_id IS NOT NULL AND file_sha256 IS NOT NULL AND file_mime_type IS NOT NULL AND sequence_id IS NULL
                     AND seller_id IS NULL AND renderer_version IS NULL END),
    CHECK ((status = 'issued') = (issue_seq IS NOT NULL AND superseded_at IS NULL) OR status = 'superseded'),
    CHECK ((status = 'draft') = (issue_seq IS NULL AND issued_at IS NULL AND superseded_at IS NULL) OR status <> 'draft'),
    CHECK ((issue_seq IS NULL) = (issued_at IS NULL)),
    CHECK (status <> 'superseded' OR superseded_at IS NOT NULL)
);

-- A kind has at most one draft and one issued version at a time (SM-FUL-DOCUMENT).
CREATE UNIQUE INDEX shipment_document_one_draft ON brant_ful.shipment_document (tenant_id, company_id, document_set_id, document_kind)
    WHERE status = 'draft';
CREATE UNIQUE INDEX shipment_document_one_issued ON brant_ful.shipment_document (tenant_id, company_id, document_set_id, document_kind)
    WHERE status = 'issued';

-- A version is inserted as a draft, the next of its kind; it then moves once: draft to issued
-- (with its issue) or to superseded, issued to superseded. Nothing else of it changes; nothing is
-- deleted (SM-FUL-DOCUMENT; FUL-INV-004 "签发版本不可变").
CREATE FUNCTION brant_ful.guard_shipment_document() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_moving text[] := ARRAY['status', 'issue_seq', 'issued_at', 'superseded_at'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_ful.shipment_document: versions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'draft' OR NEW.version <> 1 + coalesce((SELECT max(d.version) FROM brant_ful.shipment_document d
                WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND d.document_set_id = NEW.document_set_id
                AND d.document_kind = NEW.document_kind), 0) THEN
            RAISE EXCEPTION 'brant_ful.shipment_document: version % of % is not inserted as the next draft', NEW.version, NEW.document_kind
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - v_moving) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_moving) AND (
            (OLD.status = 'draft' AND NEW.status = 'issued' AND NEW.issue_seq IS NOT NULL AND NEW.issued_at IS NOT NULL
                AND NEW.superseded_at IS NULL)
            OR (OLD.status = 'draft' AND NEW.status = 'superseded' AND NEW.issue_seq IS NULL AND NEW.issued_at IS NULL
                AND NEW.superseded_at IS NOT NULL)
            OR (OLD.status = 'issued' AND NEW.status = 'superseded' AND NEW.issue_seq IS NOT DISTINCT FROM OLD.issue_seq
                AND NEW.issued_at IS NOT DISTINCT FROM OLD.issued_at AND NEW.superseded_at IS NOT NULL)) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_ful.shipment_document: version % of % moves only draft -> issued or superseded, issued -> superseded; an issued version never changes',
        OLD.version, OLD.document_kind USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER shipment_document_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.shipment_document
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_shipment_document();

CREATE TABLE brant_ful.shipment_document_rendition (
    tenant_id         text    NOT NULL,
    company_id        text    NOT NULL,
    document_set_id   text    NOT NULL,
    document_kind     text    NOT NULL CHECK (document_kind IN ('commercial_invoice', 'packing_list')),
    version           bigint  NOT NULL,
    language          text    NOT NULL CHECK (language IN ('en', 'zh-CN')),
    template_kind     text    NOT NULL CHECK (template_kind = 'shipment_document_template'),
    template_id       text    NOT NULL,
    template_revision bigint  NOT NULL CHECK (template_revision > 0),
    template_hash     text    NOT NULL CHECK (template_hash ~ '^[0-9a-f]{64}$'),
    file_name         text    NOT NULL CHECK (char_length(file_name) BETWEEN 1 AND 200),
    mime_type         text    NOT NULL CHECK (mime_type = 'text/plain'),
    content           bytea   NOT NULL,
    content_sha256    text    NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    size_bytes        integer NOT NULL CHECK (size_bytes BETWEEN 1 AND 1048576),
    PRIMARY KEY (tenant_id, company_id, document_set_id, document_kind, version, language),
    FOREIGN KEY (tenant_id, company_id, document_set_id, document_kind, version)
        REFERENCES brant_ful.shipment_document (tenant_id, company_id, document_set_id, document_kind, version),
    CHECK (size_bytes = octet_length(content)),
    CHECK (content_sha256 = encode(sha256(content), 'hex'))
);

CREATE TABLE brant_ful.shipment_document_issue (
    tenant_id             text        NOT NULL,
    company_id            text        NOT NULL,
    document_set_id       text        NOT NULL,
    seq                   bigint      NOT NULL CHECK (seq > 0),
    set_version           bigint      NOT NULL CHECK (set_version > 1),
    documents             text        NOT NULL,
    acknowledgements      text        NOT NULL,
    differences           text        NOT NULL,
    reconciliation_sha256 text        NOT NULL CHECK (reconciliation_sha256 ~ '^[0-9a-f]{64}$'),
    idempotency_key       text        NOT NULL,
    issued_by_kind        text        NOT NULL CHECK (issued_by_kind = 'human'),
    issued_by_id          text        NOT NULL,
    issue_epoch           bigint      NOT NULL CHECK (issue_epoch > 0),
    issued_at             timestamptz NOT NULL,
    commit_entries        text        NOT NULL,
    commit_entries_sha256 text        NOT NULL CHECK (commit_entries_sha256 ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (tenant_id, company_id, document_set_id, seq),
    UNIQUE (tenant_id, company_id, document_set_id, set_version),
    FOREIGN KEY (tenant_id, company_id, document_set_id) REFERENCES brant_ful.shipment_document_set (tenant_id, company_id, document_set_id),
    CHECK (reconciliation_sha256 = encode(sha256(convert_to(differences, 'UTF8')), 'hex')),
    CHECK (commit_entries_sha256 = encode(sha256(convert_to(commit_entries, 'UTF8')), 'hex'))
);

CREATE TABLE brant_ful.shipment_document_signature (
    tenant_id        text        NOT NULL,
    company_id       text        NOT NULL,
    document_set_id  text        NOT NULL,
    document_kind    text        NOT NULL,
    version          bigint      NOT NULL,
    language         text        NOT NULL,
    file_id          text        NOT NULL CHECK (file_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    file_sha256      text        NOT NULL CHECK (file_sha256 ~ '^[0-9a-f]{64}$'),
    file_mime_type   text        NOT NULL,
    document_sha256  text        NOT NULL CHECK (document_sha256 ~ '^[0-9a-f]{64}$'),
    evidence_ids     text[]      NOT NULL CHECK (cardinality(evidence_ids) <= 20),
    idempotency_key  text        NOT NULL,
    recorded_by_kind text        NOT NULL CHECK (recorded_by_kind = 'human'),
    recorded_by_id   text        NOT NULL,
    record_epoch     bigint      NOT NULL CHECK (record_epoch > 0),
    recorded_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, document_set_id, document_kind, version, language),
    FOREIGN KEY (tenant_id, company_id, document_set_id, document_kind, version, language)
        REFERENCES brant_ful.shipment_document_rendition (tenant_id, company_id, document_set_id, document_kind, version, language),
    -- 草稿不能冒充已签署: the generated bytes are never the signed copy
    CHECK (file_sha256 <> document_sha256)
);

-- Renditions, issues and signed copies are only inserted: renditions with their draft, signed copies
-- of an issued version whose generated files none of them is.
CREATE FUNCTION brant_ful.guard_shipment_document_part() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_ful.%: rows never change and are never deleted', TG_TABLE_NAME
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION brant_ful.guard_shipment_document_rendition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_ful.shipment_document_rendition: rows never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_ful.shipment_document d WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id
            AND d.document_set_id = NEW.document_set_id AND d.document_kind = NEW.document_kind AND d.version = NEW.version
            AND d.status = 'draft') THEN
        RAISE EXCEPTION 'brant_ful.shipment_document_rendition: version % of % is not a draft', NEW.version, NEW.document_kind
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION brant_ful.guard_shipment_document_signature() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_ful.shipment_document_signature: rows never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_ful.shipment_document d WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id
            AND d.document_set_id = NEW.document_set_id AND d.document_kind = NEW.document_kind AND d.version = NEW.version
            AND d.status = 'issued') THEN
        RAISE EXCEPTION 'brant_ful.shipment_document_signature: version % of % is not issued', NEW.version, NEW.document_kind
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM brant_ful.shipment_document_rendition r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
            AND r.document_set_id = NEW.document_set_id AND r.document_kind = NEW.document_kind AND r.version = NEW.version
            AND r.content_sha256 = NEW.file_sha256) THEN
        RAISE EXCEPTION 'brant_ful.shipment_document_signature: a generated file of version % of % is not a signed copy', NEW.version,
            NEW.document_kind USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER shipment_document_rendition_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.shipment_document_rendition
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_shipment_document_rendition();
CREATE TRIGGER shipment_document_issue_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.shipment_document_issue
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_shipment_document_part();
CREATE TRIGGER shipment_document_signature_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.shipment_document_signature
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_shipment_document_signature();

-- At commit: a set counts its versions and issues, numbered from 1; a rendered version has one or two
-- renditions.
CREATE FUNCTION brant_ful.check_shipment_document_set() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_set      record;
    v_docs     bigint;
    v_issues   bigint;
    v_last     bigint;
    v_unrender bigint;
BEGIN
    SELECT s.* INTO v_set FROM brant_ful.shipment_document_set s
    WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id AND s.document_set_id = NEW.document_set_id;
    SELECT count(*) INTO v_docs FROM brant_ful.shipment_document d
    WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND d.document_set_id = NEW.document_set_id;
    SELECT count(*), coalesce(max(i.seq), 0) INTO v_issues, v_last FROM brant_ful.shipment_document_issue i
    WHERE i.tenant_id = NEW.tenant_id AND i.company_id = NEW.company_id AND i.document_set_id = NEW.document_set_id;
    SELECT count(*) INTO v_unrender FROM brant_ful.shipment_document d
    WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND d.document_set_id = NEW.document_set_id
        AND d.document_kind IN ('commercial_invoice', 'packing_list')
        AND (SELECT count(*) FROM brant_ful.shipment_document_rendition r WHERE r.tenant_id = d.tenant_id AND r.company_id = d.company_id
             AND r.document_set_id = d.document_set_id AND r.document_kind = d.document_kind AND r.version = d.version) NOT BETWEEN 1 AND 2;
    IF v_docs <> v_set.document_count OR v_issues <> v_set.issue_count OR v_last <> v_set.issue_count OR v_unrender <> 0 THEN
        RAISE EXCEPTION 'brant_ful.shipment_document_set: set % counts % versions and % issues, the rows are % and % (% without renditions)',
            v_set.document_set_id, v_set.document_count, v_set.issue_count, v_docs, v_issues, v_unrender
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER shipment_document_set_at_commit AFTER INSERT OR UPDATE ON brant_ful.shipment_document_set
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_shipment_document_set();
CREATE CONSTRAINT TRIGGER shipment_document_at_commit AFTER INSERT ON brant_ful.shipment_document
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_shipment_document_set();

ALTER TABLE brant_ful.shipment_booking ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.shipment_booking FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_booking_scope ON brant_ful.shipment_booking
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.shipment_document_set ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.shipment_document_set FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_document_set_scope ON brant_ful.shipment_document_set
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.shipment_document ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.shipment_document FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_document_scope ON brant_ful.shipment_document
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.shipment_document_rendition ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.shipment_document_rendition FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_document_rendition_scope ON brant_ful.shipment_document_rendition
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.shipment_document_issue ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.shipment_document_issue FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_document_issue_scope ON brant_ful.shipment_document_issue
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.shipment_document_signature ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.shipment_document_signature FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_document_signature_scope ON brant_ful.shipment_document_signature
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

-- The documents release binds the issued B/L record (S5-07 x S5-08).
ALTER TABLE brant_ful.commercial_release
    ADD COLUMN consumed_document_set_id text,
    ADD COLUMN consumed_bl_version      bigint CHECK (consumed_bl_version > 0),
    ADD COLUMN consumed_bl_number       text CHECK (char_length(consumed_bl_number) BETWEEN 1 AND 64),
    ADD COLUMN consumed_bl_sha256       text CHECK (consumed_bl_sha256 ~ '^[0-9a-f]{64}$'),
    ADD CONSTRAINT commercial_release_bill_of_lading CHECK (
        num_nulls(consumed_document_set_id, consumed_bl_version, consumed_bl_number, consumed_bl_sha256) IN (0, 4)
        AND (consumed_bl_version IS NULL OR document_release_kind IS NOT NULL));

-- S5-07's release guard with the B/L columns among what a consumption writes; a documents
-- consumption names an issued B/L version of the shipment's document set.
CREATE OR REPLACE FUNCTION brant_ful.guard_commercial_release() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_evaluate text[] := ARRAY['status', 'row_version', 'evaluations'];
    v_except   text[] := ARRAY['status', 'row_version', 'exceptions'];
    v_consume  text[] := ARRAY['status', 'row_version', 'consumed_command', 'consumed_by_kind', 'consumed_by_id', 'consume_epoch',
                               'consumed_at', 'consume_evidence_ids', 'consume_entries', 'consume_entries_sha256', 'document_release_kind',
                               'consumed_document_set_id', 'consumed_bl_version', 'consumed_bl_number', 'consumed_bl_sha256'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_ful.commercial_release: releases are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status NOT IN ('blocked', 'granted') OR NEW.row_version <> 1 OR NEW.evaluations <> 1 OR NEW.exceptions <> 0 THEN
            RAISE EXCEPTION 'brant_ful.commercial_release: a release is inserted by its first evaluation'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status IN ('blocked', 'granted') AND NEW.row_version = OLD.row_version + 1 THEN
        IF NEW.status IN ('blocked', 'granted') AND NEW.evaluations = OLD.evaluations + 1
                AND (to_jsonb(NEW) - v_evaluate) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_evaluate) THEN
            RETURN NEW;
        END IF;
        IF OLD.status = 'blocked' AND NEW.status = 'granted' AND NEW.exceptions = OLD.exceptions + 1
                AND (to_jsonb(NEW) - v_except) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_except) THEN
            RETURN NEW;
        END IF;
        IF OLD.status = 'granted' AND NEW.status = 'consumed'
                AND (to_jsonb(NEW) - v_consume) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_consume) THEN
            IF NEW.scope = 'documents' AND NOT EXISTS (SELECT 1 FROM brant_ful.shipment_document d
                    JOIN brant_ful.shipment_document_set s ON s.tenant_id = d.tenant_id AND s.company_id = d.company_id
                        AND s.document_set_id = d.document_set_id
                    WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND d.document_set_id = NEW.consumed_document_set_id
                        AND s.shipment_id = NEW.shipment_id AND d.document_kind = 'bill_of_lading' AND d.version = NEW.consumed_bl_version
                        AND d.status = 'issued' AND d.number = NEW.consumed_bl_number AND d.content_sha256 = NEW.consumed_bl_sha256) THEN
                RAISE EXCEPTION 'brant_ful.commercial_release: the documents of shipment % are released on no issued bill of lading record',
                    NEW.shipment_id USING ERRCODE = 'integrity_constraint_violation';
            END IF;
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'brant_ful.commercial_release: release % changes only by an evaluation, an exception or its consumption (SM-FUL-RELEASE)',
        OLD.release_id USING ERRCODE = 'integrity_constraint_violation';
END
$$;
