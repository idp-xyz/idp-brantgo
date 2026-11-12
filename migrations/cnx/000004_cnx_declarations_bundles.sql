-- CNX: customs declarations, document bundles and external reconciliation (tactical model
-- CNX-AGG-DeclarationCase with DeclarationVersion, DeclarationItemMapping, DeclarationSubmission and
-- DeclarationReceipt; CNX-AGG-DocumentBundle with RequirementCheck and ExternalReconciliation;
-- CNX-INV-003, CNX-INV-006, CNX-INV-008; SM-CNX-DECLARATION; baseline CN-R05, CN-R07, CN-R11,
-- CN-R13; WBS S5-05a; S5 decisions E-1 (manual paths), D-3, D-17). Python V0.50 has no CNX.
--
-- A declaration case is the root, one per FUL export lot and attempt (a new attempt only after the
-- last one was rejected or withdrawn). Its versions (the declared data, a new one per change of a
-- draft and per amendment receipt), confirmations, manual submissions and receipts are immutable
-- entries; the root keeps its binding and moves by one row version with each entry, along
-- SM-CNX-DECLARATION as S5 reads it: a receipt moves the case forward along the path received →
-- accepted → released (missing receipts may arrive later and are then stale), released → amended →
-- amended, released or amended → withdrawn; unknown follows a submission only; rejected and withdrawn
-- are final. The external sequence of the results never goes back (DECLARATION_RECEIPT_STALE), the
-- declaration number is stated once, and the export date (出口日期, the date customs states, which a
-- tax refund case binds its export_tax_policy revision at) is set by a release, may be restated by an
-- amendment and is cleared by a withdrawal.
--
-- A document bundle is the root, one per export lot: every check appends the whole requirement
-- checklist (kind, version, status missing/present/verified, owner, due date, references), and every
-- external reconciliation is appended to it (external and internal value, their exact difference,
-- responsibility, adjustment). An export licence release may now rest on a recorded declaration
-- receipt (result_source 'declaration_receipt', receipt_id), as S5 B5 announced.
--
-- References to FUL, TRD, CAT, POL, EVD and STL are plain IDs and hashes: no foreign key crosses a
-- schema (DB2).

CREATE TABLE brant_cnx.declaration_case (
    tenant_id          text        NOT NULL,
    company_id         text        NOT NULL,
    declaration_id     text        NOT NULL CHECK (declaration_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    export_lot_id      text        NOT NULL CHECK (export_lot_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    attempt            integer     NOT NULL CHECK (attempt > 0),
    shipment_id        text        NOT NULL,
    order_id           text        NOT NULL,
    order_revision     bigint      NOT NULL CHECK (order_revision > 0),
    order_hash         text        NOT NULL,
    code_table_version text        NOT NULL CHECK (code_table_version ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status             text        NOT NULL CHECK (status IN ('draft', 'confirmed', 'submitted', 'unknown', 'received', 'accepted', 'released',
                                                              'rejected', 'amended', 'withdrawn')),
    current_version    integer     NOT NULL CHECK (current_version > 0),
    mapping            text        NOT NULL CHECK (mapping IN ('verified', 'unverified')),
    declaration_number text        CHECK (char_length(declaration_number) BETWEEN 1 AND 64),
    exported_at        timestamptz,
    last_sequence      bigint      CHECK (last_sequence > 0),
    submissions        integer     NOT NULL CHECK (submissions >= 0),
    receipts           integer     NOT NULL CHECK (receipts >= 0),
    row_version        bigint      NOT NULL CHECK (row_version > 0),
    opened_by_kind     text        NOT NULL CHECK (opened_by_kind IN ('human', 'ai')),
    opened_by_id       text        NOT NULL,
    authority_epoch    bigint      NOT NULL CHECK (authority_epoch > 0),
    opened_at          timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, declaration_id),
    UNIQUE (tenant_id, company_id, export_lot_id, attempt),
    CHECK (exported_at IS NULL OR status IN ('released', 'amended')),
    CHECK (updated_at >= opened_at)
);

CREATE INDEX declaration_case_shipment ON brant_cnx.declaration_case (tenant_id, company_id, shipment_id);

-- A case opens as a draft at version 1 with its first version, as its lot's next attempt once every
-- earlier one was rejected or withdrawn; it keeps its binding and moves by one row version per
-- command along SM-CNX-DECLARATION (see above); a draft's version moves by one per change.
CREATE FUNCTION brant_cnx.guard_declaration_case() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cnx.declaration_case: cases are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'draft' OR NEW.row_version <> 1 OR NEW.current_version <> 1 OR NEW.submissions <> 0 OR NEW.receipts <> 0
                OR NEW.declaration_number IS NOT NULL OR NEW.exported_at IS NOT NULL OR NEW.last_sequence IS NOT NULL
                OR NEW.updated_at <> NEW.opened_at
                OR NEW.attempt <> 1 + (SELECT count(*) FROM brant_cnx.declaration_case c WHERE c.tenant_id = NEW.tenant_id
                    AND c.company_id = NEW.company_id AND c.export_lot_id = NEW.export_lot_id)
                OR EXISTS (SELECT 1 FROM brant_cnx.declaration_case c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
                    AND c.export_lot_id = NEW.export_lot_id AND c.status NOT IN ('rejected', 'withdrawn')) THEN
            RAISE EXCEPTION 'brant_cnx.declaration_case: a case opens as a draft at version 1 as its lot''s next attempt'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - ARRAY['status', 'current_version', 'mapping', 'declaration_number', 'exported_at', 'last_sequence', 'submissions',
                'receipts', 'row_version', 'updated_at'])
            IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['status', 'current_version', 'mapping', 'declaration_number', 'exported_at', 'last_sequence',
                'submissions', 'receipts', 'row_version', 'updated_at'])
            OR NEW.row_version <> OLD.row_version + 1 OR NEW.updated_at < OLD.updated_at
            OR NEW.current_version NOT IN (OLD.current_version, OLD.current_version + 1)
            OR (NEW.current_version <> OLD.current_version AND NOT (OLD.status = 'draft' OR NEW.status = 'amended'))
            OR NEW.submissions NOT IN (OLD.submissions, OLD.submissions + 1) OR NEW.receipts NOT IN (OLD.receipts, OLD.receipts + 1)
            OR (OLD.declaration_number IS NOT NULL AND NEW.declaration_number IS DISTINCT FROM OLD.declaration_number)
            OR (OLD.last_sequence IS NOT NULL AND (NEW.last_sequence IS NULL OR NEW.last_sequence < OLD.last_sequence))
            OR NOT ((OLD.status = 'draft' AND NEW.status IN ('draft', 'confirmed') AND NEW.receipts = OLD.receipts AND NEW.submissions = OLD.submissions)
                OR (OLD.status = 'confirmed' AND NEW.status = 'submitted' AND NEW.submissions = OLD.submissions + 1)
                OR (NEW.receipts = OLD.receipts + 1 AND (OLD.status, NEW.status) IN (('submitted', 'unknown'), ('submitted', 'received'),
                    ('submitted', 'accepted'), ('submitted', 'released'), ('submitted', 'rejected'), ('unknown', 'received'), ('unknown', 'accepted'),
                    ('unknown', 'released'), ('unknown', 'rejected'), ('received', 'accepted'), ('received', 'released'), ('received', 'rejected'),
                    ('accepted', 'released'), ('accepted', 'rejected'), ('released', 'amended'), ('released', 'withdrawn'), ('amended', 'amended'),
                    ('amended', 'withdrawn')))) THEN
        RAISE EXCEPTION 'brant_cnx.declaration_case: case % keeps its binding and follows SM-CNX-DECLARATION', OLD.declaration_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER declaration_case_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.declaration_case
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_declaration_case();

CREATE TABLE brant_cnx.declaration_version (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    declaration_id  text        NOT NULL,
    version_no      integer     NOT NULL CHECK (version_no > 0),
    kind            text        NOT NULL CHECK (kind IN ('prepared', 'amended')),
    mapping         text        NOT NULL CHECK (mapping IN ('verified', 'unverified')),
    document        text        NOT NULL CHECK (left(document, 1) = '{'),
    document_sha256 text        NOT NULL CHECK (document_sha256 ~ '^[0-9a-f]{64}$'),
    receipt_id      text,
    created_by_kind text        NOT NULL CHECK (created_by_kind IN ('human', 'ai')),
    created_by_id   text        NOT NULL,
    authority_epoch bigint      NOT NULL CHECK (authority_epoch > 0),
    created_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, declaration_id, version_no),
    FOREIGN KEY (tenant_id, company_id, declaration_id) REFERENCES brant_cnx.declaration_case (tenant_id, company_id, declaration_id),
    CHECK ((kind = 'amended') = (receipt_id IS NOT NULL)),
    CHECK (kind = 'prepared' OR created_by_kind = 'human')
);

CREATE TABLE brant_cnx.declaration_confirmation (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    declaration_id    text        NOT NULL,
    version_no        integer     NOT NULL,
    evidence          text        NOT NULL CHECK (left(evidence, 1) = '['),
    reason            text        CHECK (char_length(reason) BETWEEN 1 AND 1000),
    confirmed_by_kind text        NOT NULL CHECK (confirmed_by_kind = 'human'),
    confirmed_by_id   text        NOT NULL,
    authority_epoch   bigint      NOT NULL CHECK (authority_epoch > 0),
    confirmed_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, declaration_id, version_no),
    FOREIGN KEY (tenant_id, company_id, declaration_id, version_no)
        REFERENCES brant_cnx.declaration_version (tenant_id, company_id, declaration_id, version_no)
);

CREATE TABLE brant_cnx.declaration_submission (
    tenant_id           text        NOT NULL,
    company_id          text        NOT NULL,
    declaration_id      text        NOT NULL,
    submission_no       integer     NOT NULL CHECK (submission_no > 0),
    version_no          integer     NOT NULL,
    operation_reference text        NOT NULL CHECK (operation_reference ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    submitter_kind      text        NOT NULL CHECK (submitter_kind IN ('broker', 'own_clerk')),
    submitter_party_id  text,
    submitter_name      text        NOT NULL CHECK (char_length(submitter_name) BETWEEN 1 AND 200),
    submitted_at        timestamptz NOT NULL,
    evidence            text        NOT NULL CHECK (left(evidence, 1) = '['),
    note                text        CHECK (char_length(note) BETWEEN 1 AND 1000),
    recorded_by_kind    text        NOT NULL CHECK (recorded_by_kind = 'human'),
    recorded_by_id      text        NOT NULL,
    authority_epoch     bigint      NOT NULL CHECK (authority_epoch > 0),
    recorded_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, declaration_id, submission_no),
    UNIQUE (tenant_id, company_id, operation_reference),
    FOREIGN KEY (tenant_id, company_id, declaration_id, version_no)
        REFERENCES brant_cnx.declaration_confirmation (tenant_id, company_id, declaration_id, version_no),
    CHECK ((submitter_kind = 'broker') = (submitter_party_id IS NOT NULL)),
    CHECK (submitted_at <= recorded_at)
);

CREATE TABLE brant_cnx.declaration_receipt (
    tenant_id          text        NOT NULL,
    company_id         text        NOT NULL,
    receipt_id         text        NOT NULL CHECK (receipt_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    declaration_id     text        NOT NULL,
    receipt_no         integer     NOT NULL CHECK (receipt_no > 0),
    outcome            text        NOT NULL CHECK (outcome IN ('unknown', 'received', 'accepted', 'released', 'rejected', 'amended', 'withdrawn')),
    from_status        text        NOT NULL,
    external_sequence  bigint      CHECK (external_sequence > 0),
    external_status    text        CHECK (char_length(external_status) BETWEEN 1 AND 64),
    original_text      text        CHECK (char_length(original_text) BETWEEN 1 AND 4000),
    mapping_version    text        CHECK (mapping_version ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    received_at        timestamptz,
    declaration_number text        CHECK (char_length(declaration_number) BETWEEN 1 AND 64),
    exported_at        timestamptz,
    version_no         integer,
    evidence           text        NOT NULL CHECK (left(evidence, 1) = '['),
    reason             text        CHECK (char_length(reason) BETWEEN 1 AND 1000),
    recorded_by_kind   text        NOT NULL CHECK (recorded_by_kind = 'human'),
    recorded_by_id     text        NOT NULL,
    authority_epoch    bigint      NOT NULL CHECK (authority_epoch > 0),
    recorded_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, receipt_id),
    UNIQUE (tenant_id, company_id, declaration_id, receipt_no),
    FOREIGN KEY (tenant_id, company_id, declaration_id) REFERENCES brant_cnx.declaration_case (tenant_id, company_id, declaration_id),
    -- unknown is the absence of a result: no external sequence, text, mapping or number, and a reason.
    CHECK ((outcome = 'unknown') = (external_sequence IS NULL)),
    CHECK ((external_sequence IS NULL) = (external_status IS NULL) AND (external_sequence IS NULL) = (original_text IS NULL)
        AND (external_sequence IS NULL) = (mapping_version IS NULL) AND (external_sequence IS NULL) = (received_at IS NULL)
        AND (external_sequence IS NULL) = (declaration_number IS NULL)),
    CHECK (outcome <> 'unknown' OR reason IS NOT NULL),
    CHECK (outcome <> 'released' OR exported_at IS NOT NULL),
    CHECK (exported_at IS NULL OR outcome IN ('released', 'amended')),
    CHECK (exported_at IS NULL OR (exported_at <= received_at AND exported_at <= recorded_at)),
    CHECK (version_no IS NULL OR outcome = 'amended'),
    CHECK (received_at IS NULL OR received_at <= recorded_at)
);

CREATE INDEX declaration_receipt_case ON brant_cnx.declaration_receipt (tenant_id, company_id, declaration_id);

-- Versions, confirmations, submissions and receipts are only inserted, numbered without gaps, under
-- the case's FOR UPDATE lock; a confirmation confirms the case's current version, a submission
-- submits the confirmed one.
CREATE FUNCTION brant_cnx.guard_declaration_entry() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_case record;
    v_next bigint;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION '%: declaration entries never change and are never deleted', TG_TABLE_NAME USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT c.* INTO v_case FROM brant_cnx.declaration_case c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
        AND c.declaration_id = NEW.declaration_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION '%: no declaration %', TG_TABLE_NAME, NEW.declaration_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    -- Each table's number is read in its own branch: PL/pgSQL resolves a record's field only where
    -- the statement runs.
    IF TG_TABLE_NAME = 'declaration_version' THEN
        SELECT 1 + count(*) INTO v_next FROM brant_cnx.declaration_version v WHERE v.tenant_id = NEW.tenant_id AND v.company_id = NEW.company_id
            AND v.declaration_id = NEW.declaration_id;
        IF to_jsonb(NEW) ->> 'version_no' <> v_next::text THEN
            RAISE EXCEPTION 'declaration_version: versions of % are numbered without gaps', NEW.declaration_id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSIF TG_TABLE_NAME = 'declaration_confirmation' THEN
        IF to_jsonb(NEW) ->> 'version_no' <> v_case.current_version::text THEN
            RAISE EXCEPTION 'declaration_confirmation: % confirms its current version only', NEW.declaration_id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSIF TG_TABLE_NAME = 'declaration_submission' THEN
        SELECT 1 + count(*) INTO v_next FROM brant_cnx.declaration_submission s WHERE s.tenant_id = NEW.tenant_id
            AND s.company_id = NEW.company_id AND s.declaration_id = NEW.declaration_id;
        IF to_jsonb(NEW) ->> 'submission_no' <> v_next::text THEN
            RAISE EXCEPTION 'declaration_submission: submissions of % are numbered without gaps', NEW.declaration_id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSE
        SELECT 1 + count(*) INTO v_next FROM brant_cnx.declaration_receipt r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
            AND r.declaration_id = NEW.declaration_id;
        IF to_jsonb(NEW) ->> 'receipt_no' <> v_next::text THEN
            RAISE EXCEPTION 'declaration_receipt: receipts of % are numbered without gaps', NEW.declaration_id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER declaration_version_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.declaration_version
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_declaration_entry();
CREATE TRIGGER declaration_confirmation_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.declaration_confirmation
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_declaration_entry();
CREATE TRIGGER declaration_submission_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.declaration_submission
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_declaration_entry();
CREATE TRIGGER declaration_receipt_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.declaration_receipt
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_declaration_entry();

-- An amendment version names the amendment receipt it records.
ALTER TABLE brant_cnx.declaration_version ADD FOREIGN KEY (tenant_id, company_id, receipt_id)
    REFERENCES brant_cnx.declaration_receipt (tenant_id, company_id, receipt_id) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE brant_cnx.document_bundle (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    bundle_id       text        NOT NULL CHECK (bundle_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    export_lot_id   text        NOT NULL CHECK (export_lot_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_id        text        NOT NULL,
    status          text        NOT NULL CHECK (status IN ('incomplete', 'complete')),
    checks          integer     NOT NULL CHECK (checks > 0),
    reconciliations integer     NOT NULL CHECK (reconciliations >= 0),
    row_version     bigint      NOT NULL CHECK (row_version > 0),
    opened_by_kind  text        NOT NULL CHECK (opened_by_kind IN ('human', 'ai')),
    opened_by_id    text        NOT NULL,
    authority_epoch bigint      NOT NULL CHECK (authority_epoch > 0),
    opened_at       timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, bundle_id),
    UNIQUE (tenant_id, company_id, export_lot_id),
    CHECK (row_version = checks + reconciliations),
    CHECK (updated_at >= opened_at)
);

-- A bundle opens with its first check at version 1; it keeps its binding and moves by one row
-- version with each check (which states its status) or reconciliation.
CREATE FUNCTION brant_cnx.guard_document_bundle() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cnx.document_bundle: bundles are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.row_version <> 1 OR NEW.checks <> 1 OR NEW.reconciliations <> 0 OR NEW.updated_at <> NEW.opened_at THEN
            RAISE EXCEPTION 'brant_cnx.document_bundle: a bundle opens with its first check' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - ARRAY['status', 'checks', 'reconciliations', 'row_version', 'updated_at'])
            IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['status', 'checks', 'reconciliations', 'row_version', 'updated_at'])
            OR NEW.row_version <> OLD.row_version + 1 OR NEW.updated_at < OLD.updated_at
            OR NOT ((NEW.checks = OLD.checks + 1 AND NEW.reconciliations = OLD.reconciliations)
                OR (NEW.reconciliations = OLD.reconciliations + 1 AND NEW.checks = OLD.checks AND NEW.status = OLD.status)) THEN
        RAISE EXCEPTION 'brant_cnx.document_bundle: bundle % keeps its binding and moves by one check or reconciliation', OLD.bundle_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER document_bundle_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.document_bundle
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_document_bundle();

CREATE TABLE brant_cnx.document_bundle_check (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    bundle_id       text        NOT NULL,
    check_no        integer     NOT NULL CHECK (check_no > 0),
    document        text        NOT NULL CHECK (left(document, 1) = '{'),
    document_sha256 text        NOT NULL CHECK (document_sha256 ~ '^[0-9a-f]{64}$'),
    complete        boolean     NOT NULL,
    missing         integer     NOT NULL CHECK (missing >= 0),
    present         integer     NOT NULL CHECK (present >= 0),
    verified        integer     NOT NULL CHECK (verified >= 0),
    checked_by_kind text        NOT NULL CHECK (checked_by_kind IN ('human', 'ai')),
    checked_by_id   text        NOT NULL,
    authority_epoch bigint      NOT NULL CHECK (authority_epoch > 0),
    checked_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, bundle_id, check_no),
    FOREIGN KEY (tenant_id, company_id, bundle_id) REFERENCES brant_cnx.document_bundle (tenant_id, company_id, bundle_id),
    -- presence is not validity: a bundle is complete only when every requirement is verified, and
    -- only a human verifies (CNX-INV-008).
    CHECK (complete = (missing = 0 AND present = 0 AND verified > 0)),
    CHECK (verified = 0 OR checked_by_kind = 'human')
);

CREATE TABLE brant_cnx.external_reconciliation (
    tenant_id               text        NOT NULL,
    company_id              text        NOT NULL,
    reconciliation_id       text        NOT NULL CHECK (reconciliation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    bundle_id               text        NOT NULL,
    reconciliation_no       integer     NOT NULL CHECK (reconciliation_no > 0),
    subject_kind            text        NOT NULL CHECK (subject_kind IN ('export_lot', 'declaration', 'tax_refund_case', 'fund_record', 'document')),
    subject_id              text        NOT NULL,
    measure                 text        NOT NULL CHECK (measure IN ('amount', 'quantity')),
    unit                    text        NOT NULL CHECK (unit ~ '^[A-Z0-9-]{1,32}$'),
    external_value          numeric     NOT NULL,
    internal_value          numeric     NOT NULL,
    difference              numeric     NOT NULL,
    external_source         text        NOT NULL CHECK (char_length(external_source) BETWEEN 1 AND 1000),
    internal_source         text        NOT NULL CHECK (char_length(internal_source) BETWEEN 1 AND 1000),
    responsibility_kind     text        NOT NULL CHECK (responsibility_kind IN ('none', 'internal', 'customer', 'supplier', 'broker', 'carrier',
                                                                               'authority', 'bank', 'other')),
    responsibility_party_id text,
    responsibility_note     text        NOT NULL CHECK (char_length(responsibility_note) BETWEEN 1 AND 1000),
    adjustment              text        NOT NULL CHECK (adjustment IN ('none', 'approved')),
    adjustment_note         text        CHECK (char_length(adjustment_note) BETWEEN 1 AND 1000),
    evidence                text        NOT NULL CHECK (left(evidence, 2) = '[{'),
    reason                  text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    resolved_by_kind        text        NOT NULL CHECK (resolved_by_kind = 'human'),
    resolved_by_id          text        NOT NULL,
    authority_epoch         bigint      NOT NULL CHECK (authority_epoch > 0),
    resolved_at             timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, reconciliation_id),
    UNIQUE (tenant_id, company_id, bundle_id, reconciliation_no),
    FOREIGN KEY (tenant_id, company_id, bundle_id) REFERENCES brant_cnx.document_bundle (tenant_id, company_id, bundle_id),
    CHECK (difference = external_value - internal_value),
    CHECK ((adjustment = 'approved') = (adjustment_note IS NOT NULL)),
    CHECK (difference <> 0 OR adjustment = 'none')
);

-- Checks and reconciliations are only inserted, numbered without gaps, under the bundle's FOR
-- UPDATE lock.
CREATE FUNCTION brant_cnx.guard_document_bundle_entry() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_next bigint;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION '%: bundle entries never change and are never deleted', TG_TABLE_NAME USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    PERFORM 1 FROM brant_cnx.document_bundle b WHERE b.tenant_id = NEW.tenant_id AND b.company_id = NEW.company_id AND b.bundle_id = NEW.bundle_id
        FOR UPDATE;
    IF TG_TABLE_NAME = 'document_bundle_check' THEN
        SELECT 1 + count(*) INTO v_next FROM brant_cnx.document_bundle_check c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
            AND c.bundle_id = NEW.bundle_id;
        IF to_jsonb(NEW) ->> 'check_no' <> v_next::text THEN
            RAISE EXCEPTION 'document_bundle_check: checks of % are numbered without gaps', NEW.bundle_id USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSE
        SELECT 1 + count(*) INTO v_next FROM brant_cnx.external_reconciliation r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
            AND r.bundle_id = NEW.bundle_id;
        IF to_jsonb(NEW) ->> 'reconciliation_no' <> v_next::text THEN
            RAISE EXCEPTION 'external_reconciliation: reconciliations of % are numbered without gaps', NEW.bundle_id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER document_bundle_check_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.document_bundle_check
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_document_bundle_entry();
CREATE TRIGGER external_reconciliation_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.external_reconciliation
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_document_bundle_entry();

-- An export licence release on a recorded declaration receipt (S5 B5's announced widening): the
-- receipt is an amended or withdrawn one of the same kind as the release.
ALTER TABLE brant_cnx.export_license_release DROP CONSTRAINT export_license_release_result_source_check;
ALTER TABLE brant_cnx.export_license_release ADD COLUMN receipt_id text;
ALTER TABLE brant_cnx.export_license_release ADD CONSTRAINT export_license_release_result_source_check
    CHECK (result_source IN ('stated', 'declaration_receipt'));
ALTER TABLE brant_cnx.export_license_release ADD CONSTRAINT export_license_release_receipt_check
    CHECK ((result_source = 'declaration_receipt') = (receipt_id IS NOT NULL));
ALTER TABLE brant_cnx.export_license_release ADD FOREIGN KEY (tenant_id, company_id, receipt_id)
    REFERENCES brant_cnx.declaration_receipt (tenant_id, company_id, receipt_id);

CREATE FUNCTION brant_cnx.guard_export_license_release_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.receipt_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM brant_cnx.declaration_receipt r WHERE r.tenant_id = NEW.tenant_id
            AND r.company_id = NEW.company_id AND r.receipt_id = NEW.receipt_id AND r.outcome = NEW.kind) THEN
        RAISE EXCEPTION 'brant_cnx.export_license_release: receipt % is no % receipt', NEW.receipt_id, NEW.kind
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER export_license_release_receipt_guard BEFORE INSERT ON brant_cnx.export_license_release
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_export_license_release_receipt();

ALTER TABLE brant_cnx.declaration_case ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.declaration_case FORCE ROW LEVEL SECURITY;
CREATE POLICY declaration_case_scope ON brant_cnx.declaration_case
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.declaration_version ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.declaration_version FORCE ROW LEVEL SECURITY;
CREATE POLICY declaration_version_scope ON brant_cnx.declaration_version
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.declaration_confirmation ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.declaration_confirmation FORCE ROW LEVEL SECURITY;
CREATE POLICY declaration_confirmation_scope ON brant_cnx.declaration_confirmation
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.declaration_submission ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.declaration_submission FORCE ROW LEVEL SECURITY;
CREATE POLICY declaration_submission_scope ON brant_cnx.declaration_submission
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.declaration_receipt ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.declaration_receipt FORCE ROW LEVEL SECURITY;
CREATE POLICY declaration_receipt_scope ON brant_cnx.declaration_receipt
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.document_bundle ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.document_bundle FORCE ROW LEVEL SECURITY;
CREATE POLICY document_bundle_scope ON brant_cnx.document_bundle
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.document_bundle_check ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.document_bundle_check FORCE ROW LEVEL SECURITY;
CREATE POLICY document_bundle_check_scope ON brant_cnx.document_bundle_check
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.external_reconciliation ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.external_reconciliation FORCE ROW LEVEL SECURITY;
CREATE POLICY external_reconciliation_scope ON brant_cnx.external_reconciliation
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
