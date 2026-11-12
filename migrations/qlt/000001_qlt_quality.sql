-- QLT: inspections with their dispositions, quality releases with their revocations, and
-- QualityReleaseGuard v1 (tactical model QLT-AGG-Inspection, QLT-AGG-QualityRelease,
-- SM-QLT-INSPECTION, SM-QLT-RELEASE, QLT-INV-001, QLT-INV-002; Context Map R24 PUR->QLT, R26
-- QLT->FUL, R72 EVD->QLT, R73 PTY->QLT; WBS S5-02; owner decision D-15, finding F-06).
--
-- An inspection is the result of an inspection of a subject — a line of a PUR purchase commitment —
-- in the subject's unit, as PUR said the subject was when it was recorded (revision, hash, supplier,
-- order line, specification, unit, quantity). It is recorded once and never edited: it changes
-- only by its disposition (recorded -> disposed, the whole nonconforming quantity disposed in
-- disposition_item rows) and by being superseded, once, by the re-inspection or correction that
-- cites it (recorded or disposed -> superseded). A subject's inspections are numbered from 1
-- (subject_seq, unique per subject): a command that records one locks the subject's existing
-- inspections FOR UPDATE, and two commands recording a subject's first inspection at once collide on
-- the number, so the second runs again and sees the first. At commit, the valid inspections of a
-- subject cover at most its quantity (a re-inspection supersedes, it never adds: BT08).
--
-- A quality release is a human decision that a quantity of a subject at its specification serves
-- shipments in [valid_from, valid_until). It is granted once, never edited, and only revoked, once.
-- Its quantity is at most the conforming quantity of the subject's valid inspections at the
-- specification less every quantity released at it before, revoked releases included
-- (QLT-INV-001); the insert trigger recomputes both from the rows as the granting command, holding
-- the subject's inspections FOR UPDATE, sees them. Expiry is derived from valid_until at an
-- instant, never written.
--
-- brant_qlt.guard_quality_release_v1 publishes QualityReleaseRef v1 to FUL (R26, guardLock): it
-- locks the one release row FOR SHARE in FUL's transaction, which quality_release.revoke locks FOR
-- UPDATE (QLT-INV-002). QLT locks no row of another context.
--
-- References to PUR, EVD and PTY are plain IDs and hashes: no foreign key crosses a schema (DB2).
-- action_result and audit_event are QLT's command ledger (internal/foundation/action).

CREATE SCHEMA brant_qlt;

CREATE TABLE brant_qlt.inspection (
    tenant_id                   text          NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id                  text          NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    inspection_id               text          NOT NULL CHECK (inspection_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status                      text          NOT NULL CHECK (status IN ('recorded', 'disposed', 'superseded')),
    row_version                 bigint        NOT NULL CHECK (row_version BETWEEN 1 AND 3),
    subject_kind                text          NOT NULL CHECK (subject_kind = 'purchase_commitment_line'),
    subject_id                  text          NOT NULL CHECK (subject_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    subject_line_id             text          NOT NULL CHECK (subject_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    subject_seq                 bigint        NOT NULL CHECK (subject_seq > 0),
    subject_revision            bigint        NOT NULL CHECK (subject_revision > 0),
    subject_hash                text          NOT NULL CHECK (subject_hash ~ '^[0-9a-f]{64}$'),
    supplier_id                 text          NOT NULL CHECK (supplier_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_id                    text          NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_revision              bigint        NOT NULL CHECK (order_revision > 0),
    order_hash                  text          NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    order_line_id               text          NOT NULL CHECK (order_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    sku_id                      text          NOT NULL CHECK (sku_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    spec_revision               bigint        NOT NULL CHECK (spec_revision > 0),
    unit_id                     text          NOT NULL CHECK (unit_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    unit_revision               bigint        NOT NULL CHECK (unit_revision > 0),
    subject_quantity            numeric(18,6) NOT NULL CHECK (subject_quantity > 0),
    inspected                   numeric(18,6) NOT NULL CHECK (inspected > 0),
    conforming                  numeric(18,6) NOT NULL CHECK (conforming >= 0),
    nonconforming               numeric(18,6) NOT NULL CHECK (nonconforming >= 0),
    sampling_method             text          NOT NULL CHECK (sampling_method IN ('full', 'sample')),
    sample_size                 numeric(18,6),
    sampling_plan               text          CHECK (char_length(sampling_plan) BETWEEN 1 AND 200),
    standard_source_id          text          NOT NULL CHECK (standard_source_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    standard_sha256             text          NOT NULL CHECK (standard_sha256 ~ '^[0-9a-f]{64}$'),
    inspector_kind              text          NOT NULL CHECK (inspector_kind IN ('internal', 'external_agency')),
    inspector_actor_id          text,
    agency_party_id             text          CHECK (agency_party_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    agency_service_role_id      text          CHECK (agency_service_role_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    agency_role_revision        bigint        CHECK (agency_role_revision > 0),
    report_file_id              text          NOT NULL CHECK (report_file_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    report_sha256               text          NOT NULL CHECK (report_sha256 ~ '^[0-9a-f]{64}$'),
    report_mime_type            text          NOT NULL CHECK (char_length(report_mime_type) BETWEEN 1 AND 200),
    inspected_at                timestamptz   NOT NULL,
    supersedes_inspection_id    text,
    reason                      text          CHECK (char_length(reason) BETWEEN 1 AND 1000),
    evidence_ids                text[]        NOT NULL CHECK (cardinality(evidence_ids) <= 20),
    request_hash                text          NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    owner_actor_id              text          NOT NULL,
    owner_team_id               text,
    recorded_by_kind            text          NOT NULL CHECK (recorded_by_kind IN ('human', 'ai')),
    recorded_by_id              text          NOT NULL,
    record_epoch                bigint        NOT NULL CHECK (record_epoch > 0),
    recorded_at                 timestamptz   NOT NULL,
    disposed_by_kind            text,
    disposed_by_id              text,
    dispose_epoch               bigint,
    disposed_at                 timestamptz,
    disposition_reason          text,
    disposition_evidence_ids    text[],
    superseded_by_inspection_id text,
    superseded_at               timestamptz,
    PRIMARY KEY (tenant_id, company_id, inspection_id),
    UNIQUE (tenant_id, company_id, subject_kind, subject_id, subject_line_id, subject_seq),
    FOREIGN KEY (tenant_id, company_id, supersedes_inspection_id) REFERENCES brant_qlt.inspection (tenant_id, company_id, inspection_id),
    CHECK (conforming + nonconforming = inspected),
    CHECK (inspected <= subject_quantity),
    CHECK ((sampling_method = 'full') = (sample_size IS NULL)),
    CHECK (sample_size IS NULL OR (sample_size > 0 AND sample_size <= inspected)),
    CHECK (CASE inspector_kind
        WHEN 'internal' THEN inspector_actor_id IS NOT NULL AND inspector_actor_id = recorded_by_id AND recorded_by_kind = 'human'
            AND agency_party_id IS NULL AND agency_service_role_id IS NULL AND agency_role_revision IS NULL
        ELSE inspector_actor_id IS NULL AND agency_party_id IS NOT NULL AND agency_service_role_id IS NOT NULL
            AND agency_role_revision IS NOT NULL END),
    CHECK (inspected_at <= recorded_at),
    CHECK ((supersedes_inspection_id IS NULL) = (reason IS NULL)),
    CHECK (supersedes_inspection_id IS DISTINCT FROM inspection_id),
    CHECK (owner_actor_id = recorded_by_id),
    CHECK (num_nulls(disposed_by_kind, disposed_by_id, dispose_epoch, disposed_at, disposition_reason, disposition_evidence_ids) IN (0, 6)),
    CHECK (disposed_at IS NULL OR (disposed_by_kind = 'human' AND dispose_epoch > 0 AND disposed_at >= recorded_at
        AND char_length(disposition_reason) BETWEEN 1 AND 1000 AND cardinality(disposition_evidence_ids) <= 20 AND nonconforming > 0)),
    CHECK ((superseded_at IS NULL) = (superseded_by_inspection_id IS NULL)),
    CHECK (superseded_at IS NULL OR superseded_at >= recorded_at),
    CHECK (status = CASE WHEN superseded_at IS NOT NULL THEN 'superseded' WHEN disposed_at IS NOT NULL THEN 'disposed' ELSE 'recorded' END),
    CHECK (row_version = 1 + (disposed_at IS NOT NULL)::int + (superseded_at IS NOT NULL)::int)
);

-- An inspection is superseded by at most one other.
CREATE UNIQUE INDEX inspection_supersedes ON brant_qlt.inspection (tenant_id, company_id, supersedes_inspection_id)
    WHERE supersedes_inspection_id IS NOT NULL;

-- An inspection is inserted recorded; one that supersedes another names a valid inspection of the
-- same subject. It changes only by its disposition (recorded -> disposed) and by its supersession
-- (to superseded, by the inspection that names it), once each, and nothing else about it changes;
-- it is never deleted.
CREATE FUNCTION brant_qlt.guard_inspection() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_dispose   text[] := ARRAY['status', 'row_version', 'disposed_by_kind', 'disposed_by_id', 'dispose_epoch', 'disposed_at',
                                'disposition_reason', 'disposition_evidence_ids'];
    v_supersede text[] := ARRAY['status', 'row_version', 'superseded_by_inspection_id', 'superseded_at'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_qlt.inspection: inspections are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'recorded' OR (NEW.supersedes_inspection_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM brant_qlt.inspection o
                WHERE o.tenant_id = NEW.tenant_id AND o.company_id = NEW.company_id AND o.inspection_id = NEW.supersedes_inspection_id
                    AND o.status <> 'superseded' AND (o.subject_kind, o.subject_id, o.subject_line_id)
                        = (NEW.subject_kind, NEW.subject_id, NEW.subject_line_id))) THEN
            RAISE EXCEPTION 'brant_qlt.inspection: an inspection is recorded first and supersedes only a valid inspection of its subject'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'recorded' AND NEW.status = 'disposed' AND (to_jsonb(NEW) - v_dispose) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_dispose) THEN
        RETURN NEW;
    END IF;
    IF OLD.status IN ('recorded', 'disposed') AND NEW.status = 'superseded'
            AND (to_jsonb(NEW) - v_supersede) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_supersede)
            AND EXISTS (SELECT 1 FROM brant_qlt.inspection n WHERE n.tenant_id = NEW.tenant_id AND n.company_id = NEW.company_id
                AND n.inspection_id = NEW.superseded_by_inspection_id AND n.supersedes_inspection_id = NEW.inspection_id) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_qlt.inspection: inspection % is never modified; it is disposed and superseded once each (SM-QLT-INSPECTION)',
        OLD.inspection_id USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER inspection_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_qlt.inspection
    FOR EACH ROW EXECUTE FUNCTION brant_qlt.guard_inspection();

CREATE TABLE brant_qlt.disposition_item (
    tenant_id     text          NOT NULL,
    company_id    text          NOT NULL,
    inspection_id text          NOT NULL,
    action        text          NOT NULL CHECK (action IN ('rework', 'concession', 'return', 'scrap')),
    quantity      numeric(18,6) NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (tenant_id, company_id, inspection_id, action),
    FOREIGN KEY (tenant_id, company_id, inspection_id) REFERENCES brant_qlt.inspection (tenant_id, company_id, inspection_id)
);

-- An item is written with the disposition of its inspection and never changes.
CREATE FUNCTION brant_qlt.guard_disposition_item() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_qlt.disposition_item: disposition items never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_qlt.inspection i WHERE i.tenant_id = NEW.tenant_id AND i.company_id = NEW.company_id
            AND i.inspection_id = NEW.inspection_id AND i.status = 'disposed') THEN
        RAISE EXCEPTION 'brant_qlt.disposition_item: inspection % is not being disposed', NEW.inspection_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER disposition_item_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_qlt.disposition_item
    FOR EACH ROW EXECUTE FUNCTION brant_qlt.guard_disposition_item();

-- At commit: a disposed inspection disposes exactly its nonconforming quantity, an undisposed one
-- nothing; an inspection that supersedes another is the one that superseded it; the valid
-- inspections of a subject cover at most the subject's quantity.
CREATE FUNCTION brant_qlt.check_inspection() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_insp     record;
    v_items    numeric;
    v_count    bigint;
    v_covered  numeric;
BEGIN
    SELECT i.* INTO v_insp FROM brant_qlt.inspection i
    WHERE i.tenant_id = NEW.tenant_id AND i.company_id = NEW.company_id AND i.inspection_id = NEW.inspection_id;
    SELECT coalesce(sum(d.quantity), 0), count(*) INTO v_items, v_count FROM brant_qlt.disposition_item d
    WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND d.inspection_id = NEW.inspection_id;
    IF (v_insp.disposed_at IS NULL AND v_count <> 0) OR (v_insp.disposed_at IS NOT NULL AND v_items <> v_insp.nonconforming) THEN
        RAISE EXCEPTION 'brant_qlt.disposition_item: inspection % disposes % of its nonconforming %', v_insp.inspection_id, v_items,
            v_insp.nonconforming USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF v_insp.supersedes_inspection_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM brant_qlt.inspection o
            WHERE o.tenant_id = v_insp.tenant_id AND o.company_id = v_insp.company_id AND o.inspection_id = v_insp.supersedes_inspection_id
                AND o.status = 'superseded' AND o.superseded_by_inspection_id = v_insp.inspection_id) THEN
        RAISE EXCEPTION 'brant_qlt.inspection: inspection % supersedes % without superseding it', v_insp.inspection_id,
            v_insp.supersedes_inspection_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT coalesce(sum(i.inspected), 0) INTO v_covered FROM brant_qlt.inspection i
    WHERE i.tenant_id = v_insp.tenant_id AND i.company_id = v_insp.company_id AND i.subject_kind = v_insp.subject_kind
        AND i.subject_id = v_insp.subject_id AND i.subject_line_id = v_insp.subject_line_id AND i.status <> 'superseded';
    IF v_insp.status <> 'superseded' AND v_covered > v_insp.subject_quantity THEN
        RAISE EXCEPTION 'brant_qlt.inspection: the valid inspections of line % of % cover %, the subject %', v_insp.subject_line_id,
            v_insp.subject_id, v_covered, v_insp.subject_quantity USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER inspection_at_commit AFTER INSERT OR UPDATE ON brant_qlt.inspection
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_qlt.check_inspection();
CREATE CONSTRAINT TRIGGER disposition_item_at_commit AFTER INSERT ON brant_qlt.disposition_item
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_qlt.check_inspection();

CREATE TABLE brant_qlt.quality_release (
    tenant_id           text          NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id          text          NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    release_id          text          NOT NULL CHECK (release_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status              text          NOT NULL CHECK (status IN ('granted', 'revoked')),
    row_version         bigint        NOT NULL CHECK (row_version IN (1, 2)),
    subject_kind        text          NOT NULL CHECK (subject_kind = 'purchase_commitment_line'),
    subject_id          text          NOT NULL CHECK (subject_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    subject_line_id     text          NOT NULL CHECK (subject_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    subject_revision    bigint        NOT NULL CHECK (subject_revision > 0),
    subject_hash        text          NOT NULL CHECK (subject_hash ~ '^[0-9a-f]{64}$'),
    supplier_id         text          NOT NULL CHECK (supplier_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_id            text          NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_revision      bigint        NOT NULL CHECK (order_revision > 0),
    order_hash          text          NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    order_line_id       text          NOT NULL CHECK (order_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    sku_id              text          NOT NULL CHECK (sku_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    spec_revision       bigint        NOT NULL CHECK (spec_revision > 0),
    unit_id             text          NOT NULL CHECK (unit_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    unit_revision       bigint        NOT NULL CHECK (unit_revision > 0),
    subject_quantity    numeric(18,6) NOT NULL CHECK (subject_quantity > 0),
    quantity            numeric(18,6) NOT NULL CHECK (quantity > 0),
    conforming          numeric(18,6) NOT NULL CHECK (conforming >= 0),
    released_before     numeric(18,6) NOT NULL CHECK (released_before >= 0),
    inspection_ids      text[]        NOT NULL CHECK (cardinality(inspection_ids) >= 1),
    valid_from          timestamptz   NOT NULL,
    valid_until         timestamptz   NOT NULL,
    evidence_ids        text[]        NOT NULL CHECK (cardinality(evidence_ids) <= 20),
    request_hash        text          NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    owner_actor_id      text          NOT NULL,
    owner_team_id       text,
    granted_by_kind     text          NOT NULL CHECK (granted_by_kind = 'human'),
    granted_by_id       text          NOT NULL,
    grant_epoch         bigint        NOT NULL CHECK (grant_epoch > 0),
    granted_at          timestamptz   NOT NULL,
    revoked_by_kind     text,
    revoked_by_id       text,
    revoke_epoch        bigint,
    revoked_at          timestamptz,
    revoke_reason       text,
    revoke_evidence_ids text[],
    PRIMARY KEY (tenant_id, company_id, release_id),
    CHECK (quantity <= conforming - released_before),
    CHECK (valid_from < valid_until),
    CHECK (valid_until > granted_at),
    CHECK (owner_actor_id = granted_by_id),
    CHECK (CASE WHEN status = 'granted'
        THEN row_version = 1 AND num_nulls(revoked_by_kind, revoked_by_id, revoke_epoch, revoked_at, revoke_reason, revoke_evidence_ids) = 6
        ELSE row_version = 2 AND revoked_by_kind = 'human' AND revoked_by_id IS NOT NULL AND revoke_epoch > 0 AND revoked_at >= granted_at
             AND char_length(revoke_reason) BETWEEN 1 AND 1000 AND cardinality(revoke_evidence_ids) <= 20 END)
);

CREATE INDEX quality_release_subject ON brant_qlt.quality_release (tenant_id, company_id, subject_kind, subject_id, subject_line_id);

-- A release is inserted granted, on exactly the conforming quantity of its subject's valid
-- inspections at its specification and the quantity released at it before (revoked releases
-- included), as the rows stand in the granting transaction (QLT-INV-001); it changes only by its
-- revocation, once, and nothing else about it changes; it is never deleted.
CREATE FUNCTION brant_qlt.guard_quality_release() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_revoke     text[] := ARRAY['status', 'row_version', 'revoked_by_kind', 'revoked_by_id', 'revoke_epoch', 'revoked_at', 'revoke_reason',
                                 'revoke_evidence_ids'];
    v_conforming numeric;
    v_ids        text[];
    v_released   numeric;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_qlt.quality_release: releases are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        SELECT coalesce(sum(i.conforming), 0), coalesce(array_agg(i.inspection_id ORDER BY i.inspection_id COLLATE "C"), '{}')
            INTO v_conforming, v_ids FROM brant_qlt.inspection i
        WHERE i.tenant_id = NEW.tenant_id AND i.company_id = NEW.company_id AND i.subject_kind = NEW.subject_kind AND i.subject_id = NEW.subject_id
            AND i.subject_line_id = NEW.subject_line_id AND i.status <> 'superseded' AND i.sku_id = NEW.sku_id
            AND i.spec_revision = NEW.spec_revision AND i.unit_id = NEW.unit_id AND i.unit_revision = NEW.unit_revision;
        SELECT coalesce(sum(r.quantity), 0) INTO v_released FROM brant_qlt.quality_release r
        WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.subject_kind = NEW.subject_kind AND r.subject_id = NEW.subject_id
            AND r.subject_line_id = NEW.subject_line_id AND r.sku_id = NEW.sku_id AND r.spec_revision = NEW.spec_revision
            AND r.unit_id = NEW.unit_id AND r.unit_revision = NEW.unit_revision;
        IF NEW.status <> 'granted' OR NEW.conforming <> v_conforming OR NEW.released_before <> v_released OR NEW.inspection_ids <> v_ids THEN
            RAISE EXCEPTION 'brant_qlt.quality_release: release % is judged on conforming % and released % of inspections %, the rows say %, % and % (QLT-INV-001)',
                NEW.release_id, NEW.conforming, NEW.released_before, NEW.inspection_ids, v_conforming, v_released, v_ids
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'granted' AND NEW.status = 'revoked' AND (to_jsonb(NEW) - v_revoke) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_revoke) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_qlt.quality_release: release % is never modified; it is only revoked, once (SM-QLT-RELEASE)', OLD.release_id
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER quality_release_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_qlt.quality_release
    FOR EACH ROW EXECUTE FUNCTION brant_qlt.guard_quality_release();

-- QualityReleaseGuard v1 (Context Map R26 QLT->FUL "QualityReleaseRef v1", guardLock; ADR-BR-004
-- option B, ADR-BR-011 lock order; ADR-BR-014; QLT-INV-002). FUL calls it with its own executor
-- inside its own transaction, after it locked its own roots FOR UPDATE, and gets one row: known is
-- false, with code QUALITY_RELEASE_REQUIRED and every other column NULL, for a release that does not
-- exist in the caller's scope; otherwise the release's status, scope (subject, supplier, order line,
-- specification, unit), released quantity and validity, and code NULL when it is granted and
-- p_at lies in [valid_from, valid_until), QUALITY_RELEASE_REQUIRED when it is revoked or p_at lies
-- outside its validity. It locks the release row FOR SHARE: quality_release.revoke locks it FOR
-- UPDATE, so a revocation either committed before the guard read it, and the guard refuses, or waits
-- until FUL's transaction ends; in READ COMMITTED a guard that waited re-reads the row as the
-- revocation left it. The data scope (own, team) is ReleaseLookups' to check before the transaction;
-- the guard confines the row to app.tenant_id and app.company_id. SECURITY DEFINER runs it as the
-- schema owner, which FORCE ROW LEVEL SECURITY still confines to that scope; the predicates repeat
-- it. search_path is fixed and every name is qualified. brant_app gets EXECUTE in
-- deploy/postgres/10_app_grants.sql; nobody else does.
CREATE FUNCTION brant_qlt.guard_quality_release_v1(p_release_id text, p_at timestamptz)
    RETURNS TABLE (known boolean, status text, subject_kind text, subject_id text, subject_line_id text, supplier_id text, order_id text,
                   order_line_id text, sku_id text, spec_revision bigint, unit_id text, unit_revision bigint, released_quantity text,
                   valid_from timestamptz, valid_until timestamptz, code text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
    v_rel     record;
BEGIN
    IF p_release_id IS NULL OR p_at IS NULL THEN
        RAISE EXCEPTION 'brant_qlt.guard_quality_release_v1: release_id and at are required' USING ERRCODE = 'null_value_not_allowed';
    END IF;
    SELECT r.status, r.subject_kind, r.subject_id, r.subject_line_id, r.supplier_id, r.order_id, r.order_line_id, r.sku_id, r.spec_revision,
           r.unit_id, r.unit_revision, r.quantity, r.valid_from, r.valid_until INTO v_rel
    FROM brant_qlt.quality_release r
    WHERE r.tenant_id = v_tenant AND r.company_id = v_company AND r.release_id = p_release_id
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::bigint,
            NULL::text, NULL::bigint, NULL::text, NULL::timestamptz, NULL::timestamptz, 'QUALITY_RELEASE_REQUIRED'::text;
        RETURN;
    END IF;
    RETURN QUERY SELECT true, v_rel.status, v_rel.subject_kind, v_rel.subject_id, v_rel.subject_line_id, v_rel.supplier_id, v_rel.order_id,
        v_rel.order_line_id, v_rel.sku_id, v_rel.spec_revision, v_rel.unit_id, v_rel.unit_revision, pg_catalog.trim_scale(v_rel.quantity)::text,
        v_rel.valid_from, v_rel.valid_until,
        CASE
            WHEN v_rel.status <> 'granted' OR p_at < v_rel.valid_from OR p_at >= v_rel.valid_until THEN 'QUALITY_RELEASE_REQUIRED'
        END::text;
END
$$;

REVOKE ALL ON FUNCTION brant_qlt.guard_quality_release_v1(text, timestamptz) FROM PUBLIC;

CREATE TABLE brant_qlt.action_result (
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

CREATE TABLE brant_qlt.audit_event (
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

ALTER TABLE brant_qlt.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_qlt.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_qlt.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_qlt.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_qlt.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_qlt.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_qlt.inspection ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_qlt.inspection FORCE ROW LEVEL SECURITY;
CREATE POLICY inspection_scope ON brant_qlt.inspection
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_qlt.disposition_item ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_qlt.disposition_item FORCE ROW LEVEL SECURITY;
CREATE POLICY disposition_item_scope ON brant_qlt.disposition_item
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_qlt.quality_release ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_qlt.quality_release FORCE ROW LEVEL SECURITY;
CREATE POLICY quality_release_scope ON brant_qlt.quality_release
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
