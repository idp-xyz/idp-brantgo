-- TRD: the active sales order's amendments, revisions and ends (WBS S3-15; tactical model
-- TRD-AGG-SalesOrder OrderRevision, OrderAmendment, AmendmentImpact; SM-TRD-ORDER after activation,
-- SM-TRD-ORDER-AMENDMENT; TRD-INV-016, 018, 019; UL-INV-21, UL-INV-27; Context Map R19). Python has
-- none of this; the design is the only source.
--
-- An order's current revision is sales_order.order_revision and order_hash: revision 1 is the
-- snapshot order.prepare stored (snapshot_hash keeps its own hash), every later one a
-- brant.order-revision/0.1.0 document in sales_order_revision, written by order.amendment.apply in
-- one transaction with the amendment's applied state, the commit evidence and the order's move to
-- that revision. OrderGuard v1 (brant_trd.guard_order_active_v1) compares the caller's binding with
-- the current revision, so an amendment makes every downstream binding of the old revision
-- REFERENCE_STALE, and a cancellation or close makes it ORDER_NOT_ACTIVE; the function is unchanged.
--
-- An amendment is prepared a draft against the order's current revision (prepared again while a
-- draft, every preparer recorded), submitted with its impact assessment, decided once by a human
-- independent of every preparer and the submitter, then applied while the order is still at the
-- revision it binds (of two amendments of one revision the first applied wins: the other's base is
-- no longer current), or withdrawn while a draft or pending. An active order ends once: cancelled,
-- or closed (its remainder given up, or fulfilled), with the lines' ordered, shipped and given-up
-- quantities and where the shipments and downstream commitments it judged came from. Every order
-- command locks the quote root FOR UPDATE, then the order FOR UPDATE (amendments are written under
-- that lock). Nothing here is deleted.

-- The current revision moves; the snapshot is revision 1 and keeps its hash.
ALTER TABLE brant_trd.sales_order ADD COLUMN snapshot_hash text;
-- The backfill covers every tenant's orders: FORCE row level security binds the owner too, so it is
-- lifted for this statement only (the migration runs in one transaction).
ALTER TABLE brant_trd.sales_order DISABLE TRIGGER sales_order_guard;
ALTER TABLE brant_trd.sales_order NO FORCE ROW LEVEL SECURITY;
UPDATE brant_trd.sales_order SET snapshot_hash = order_hash;
ALTER TABLE brant_trd.sales_order FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.sales_order ENABLE TRIGGER sales_order_guard;
ALTER TABLE brant_trd.sales_order ALTER COLUMN snapshot_hash SET NOT NULL;
ALTER TABLE brant_trd.sales_order ADD CONSTRAINT sales_order_snapshot_hash
    CHECK (snapshot_hash = encode(sha256(convert_to(snapshot, 'UTF8')), 'hex'));
ALTER TABLE brant_trd.sales_order DROP CONSTRAINT sales_order_order_revision_check;
ALTER TABLE brant_trd.sales_order ADD CONSTRAINT sales_order_order_revision_check CHECK (order_revision > 0);
-- 000005 named the order hash check sales_order_check (it reads the snapshot too).
ALTER TABLE brant_trd.sales_order DROP CONSTRAINT sales_order_check;
ALTER TABLE brant_trd.sales_order ADD CONSTRAINT sales_order_order_hash_check CHECK (order_hash ~ '^[0-9a-f]{64}$');
ALTER TABLE brant_trd.sales_order ADD CONSTRAINT sales_order_first_revision CHECK (order_revision > 1 OR order_hash = snapshot_hash);

CREATE TABLE brant_trd.sales_order_amendment (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    order_id          text        NOT NULL,
    amendment_id      text        NOT NULL CHECK (amendment_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    base_revision     bigint      NOT NULL CHECK (base_revision > 0),
    base_hash         text        NOT NULL CHECK (base_hash ~ '^[0-9a-f]{64}$'),
    -- The amendment's content (brant.order-amendment/0.1.0, canonical JSON) and its hash.
    content           text        NOT NULL CHECK (left(content, 1) = '{'),
    amendment_hash    text        NOT NULL CHECK (amendment_hash = encode(sha256(convert_to(content, 'UTF8')), 'hex')),
    status            text        NOT NULL CHECK (status IN ('draft', 'pending_approval', 'approved', 'applied', 'rejected', 'withdrawn')),
    version           bigint      NOT NULL CHECK (version > 0),
    -- Every preparation, in order: [{"at", "by_id", "by_kind", "epoch"}, ...].
    preparations      text        NOT NULL CHECK (left(preparations, 1) = '['),
    submitted_by_kind text        CHECK (submitted_by_kind = 'human'),
    submitted_by_id   text,
    submit_epoch      bigint      CHECK (submit_epoch > 0),
    submitted_at      timestamptz,
    decided_by_kind   text        CHECK (decided_by_kind = 'human'),
    decided_by_id     text,
    decide_epoch      bigint      CHECK (decide_epoch > 0),
    decided_at        timestamptz,
    decision          text        CHECK (decision IN ('approve', 'reject')),
    decision_reason   text        CHECK (char_length(decision_reason) BETWEEN 1 AND 2000),
    decision_evidence text        CHECK (left(decision_evidence, 1) = '['),
    applied_by_kind   text        CHECK (applied_by_kind = 'human'),
    applied_by_id     text,
    apply_epoch       bigint      CHECK (apply_epoch > 0),
    applied_at        timestamptz,
    applied_revision  bigint      CHECK (applied_revision > 1),
    withdrawn_by_kind text        CHECK (withdrawn_by_kind = 'human'),
    withdrawn_by_id   text,
    withdraw_epoch    bigint      CHECK (withdraw_epoch > 0),
    withdrawn_at      timestamptz,
    withdraw_reason   text        CHECK (char_length(withdraw_reason) BETWEEN 1 AND 2000),
    withdraw_evidence text        CHECK (left(withdraw_evidence, 1) = '['),
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, order_id, amendment_id),
    FOREIGN KEY (tenant_id, company_id, order_id) REFERENCES brant_trd.sales_order (tenant_id, company_id, order_id),
    CHECK ((submitted_at IS NULL) = (submitted_by_id IS NULL) AND (submitted_by_id IS NULL) = (submitted_by_kind IS NULL)
        AND (submitted_by_id IS NULL) = (submit_epoch IS NULL)),
    CHECK (status <> 'draft' OR submitted_at IS NULL),
    CHECK (status NOT IN ('pending_approval', 'approved', 'applied', 'rejected') OR submitted_at IS NOT NULL),
    CHECK ((decided_at IS NULL) = (decided_by_id IS NULL) AND (decided_by_id IS NULL) = (decided_by_kind IS NULL)
        AND (decided_by_id IS NULL) = (decide_epoch IS NULL) AND (decided_by_id IS NULL) = (decision IS NULL)
        AND (decided_by_id IS NULL) = (decision_evidence IS NULL)),
    CHECK ((status IN ('approved', 'applied', 'rejected')) = (decided_at IS NOT NULL)),
    CHECK ((status IN ('approved', 'applied')) = (decision IS NOT DISTINCT FROM 'approve')),
    CHECK (status <> 'rejected' OR decision_reason IS NOT NULL),
    CHECK (decided_by_id IS NULL OR decided_by_id <> submitted_by_id),
    CHECK ((applied_at IS NULL) = (applied_by_id IS NULL) AND (applied_by_id IS NULL) = (applied_by_kind IS NULL)
        AND (applied_by_id IS NULL) = (apply_epoch IS NULL) AND (applied_by_id IS NULL) = (applied_revision IS NULL)),
    CHECK ((status = 'applied') = (applied_at IS NOT NULL)),
    CHECK ((withdrawn_at IS NULL) = (withdrawn_by_id IS NULL) AND (withdrawn_by_id IS NULL) = (withdrawn_by_kind IS NULL)
        AND (withdrawn_by_id IS NULL) = (withdraw_epoch IS NULL) AND (withdrawn_by_id IS NULL) = (withdraw_reason IS NULL)
        AND (withdrawn_by_id IS NULL) = (withdraw_evidence IS NULL)),
    CHECK ((status = 'withdrawn') = (withdrawn_at IS NOT NULL))
);

CREATE TABLE brant_trd.sales_order_revision (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    order_id          text        NOT NULL,
    order_revision    bigint      NOT NULL CHECK (order_revision > 1),
    -- The revision's document (brant.order-revision/0.1.0, canonical JSON); its SHA-256 is the order
    -- hash.
    document          text        NOT NULL CHECK (left(document, 1) = '{'),
    order_hash        text        NOT NULL CHECK (order_hash = encode(sha256(convert_to(document, 'UTF8')), 'hex')),
    previous_revision bigint      NOT NULL,
    previous_hash     text        NOT NULL CHECK (previous_hash ~ '^[0-9a-f]{64}$'),
    amendment_id      text        NOT NULL,
    amendment_hash    text        NOT NULL CHECK (amendment_hash ~ '^[0-9a-f]{64}$'),
    row_version       bigint      NOT NULL CHECK (row_version > 1),
    created_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, order_id, order_revision),
    UNIQUE (tenant_id, company_id, order_id, amendment_id),
    FOREIGN KEY (tenant_id, company_id, order_id) REFERENCES brant_trd.sales_order (tenant_id, company_id, order_id),
    FOREIGN KEY (tenant_id, company_id, order_id, amendment_id)
        REFERENCES brant_trd.sales_order_amendment (tenant_id, company_id, order_id, amendment_id),
    CHECK (previous_revision = order_revision - 1)
);

CREATE TABLE brant_trd.sales_order_amendment_commit (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    order_id          text        NOT NULL,
    order_revision    bigint      NOT NULL CHECK (order_revision > 1),
    command           text        NOT NULL CHECK (command = 'order.amendment.apply'),
    idempotency_key   text        NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    amendment_id      text        NOT NULL,
    amendment_hash    text        NOT NULL CHECK (amendment_hash ~ '^[0-9a-f]{64}$'),
    from_order_hash   text        NOT NULL CHECK (from_order_hash ~ '^[0-9a-f]{64}$'),
    order_hash        text        NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    authority_epoch   bigint      NOT NULL CHECK (authority_epoch > 0),
    control_epoch     bigint      NOT NULL CHECK (control_epoch >= 0),
    row_version       bigint      NOT NULL CHECK (row_version > 1),
    committed_by_kind text        NOT NULL CHECK (committed_by_kind = 'human'),
    committed_by_id   text        NOT NULL,
    committed_at      timestamptz NOT NULL,
    -- The evidence entries as canonical JSON ([{"name": ..., "value": ...}, ...]) and their SHA-256.
    entries           text        NOT NULL CHECK (left(entries, 1) = '['),
    entries_sha256    text        NOT NULL CHECK (entries_sha256 = encode(sha256(convert_to(entries, 'UTF8')), 'hex')),
    PRIMARY KEY (tenant_id, company_id, order_id, order_revision),
    FOREIGN KEY (tenant_id, company_id, order_id, order_revision)
        REFERENCES brant_trd.sales_order_revision (tenant_id, company_id, order_id, order_revision)
);

CREATE TABLE brant_trd.sales_order_end (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    order_id          text        NOT NULL,
    command           text        NOT NULL CHECK (command IN ('order.cancel', 'order.close_remainder', 'order.close')),
    status            text        NOT NULL CHECK (status IN ('cancelled', 'closed')),
    order_revision    bigint      NOT NULL CHECK (order_revision > 0),
    order_hash        text        NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    -- Each line's ordered, shipped and given-up quantity: [{"line_id", "ordered", "remainder", "shipped", "unit"}, ...].
    lines             text        NOT NULL CHECK (left(lines, 1) = '['),
    fulfilment_source text        NOT NULL CHECK (char_length(fulfilment_source) BETWEEN 1 AND 200),
    downstream_source text        NOT NULL CHECK (char_length(downstream_source) BETWEEN 1 AND 200),
    ended_by_kind     text        NOT NULL CHECK (ended_by_kind = 'human'),
    ended_by_id       text        NOT NULL,
    end_epoch         bigint      NOT NULL CHECK (end_epoch > 0),
    ended_at          timestamptz NOT NULL,
    reason            text        CHECK (char_length(reason) BETWEEN 1 AND 2000),
    evidence          text        NOT NULL CHECK (left(evidence, 1) = '['),
    row_version       bigint      NOT NULL CHECK (row_version > 1),
    PRIMARY KEY (tenant_id, company_id, order_id),
    FOREIGN KEY (tenant_id, company_id, order_id) REFERENCES brant_trd.sales_order (tenant_id, company_id, order_id),
    CHECK ((command = 'order.cancel') = (status = 'cancelled')),
    CHECK (command = 'order.close' OR reason IS NOT NULL)
);

-- An amendment is inserted a draft at version 1 for an active order at the revision it binds. Then
-- its version moves by one with each change: prepared again while a draft (content, base and hash
-- may change, one preparation is appended); submitted (draft → pending_approval) while the order is
-- at its base; decided once (pending_approval → approved or rejected) by a human who neither prepared
-- nor submitted it; applied (approved → applied) with the revision it produced; withdrawn (draft or
-- pending_approval → withdrawn). Nothing else of it changes; never deleted.
CREATE FUNCTION brant_trd.guard_sales_order_amendment() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    prepared  constant text[] := ARRAY['base_revision', 'base_hash', 'content', 'amendment_hash', 'version', 'preparations', 'updated_at'];
    submitted constant text[] := ARRAY['status', 'version', 'submitted_by_kind', 'submitted_by_id', 'submit_epoch', 'submitted_at', 'updated_at'];
    decided   constant text[] := ARRAY['status', 'version', 'decided_by_kind', 'decided_by_id', 'decide_epoch', 'decided_at', 'decision',
        'decision_reason', 'decision_evidence', 'updated_at'];
    applied   constant text[] := ARRAY['status', 'version', 'applied_by_kind', 'applied_by_id', 'apply_epoch', 'applied_at', 'applied_revision',
        'updated_at'];
    withdrawn constant text[] := ARRAY['status', 'version', 'withdrawn_by_kind', 'withdrawn_by_id', 'withdraw_epoch', 'withdrawn_at',
        'withdraw_reason', 'withdraw_evidence', 'updated_at'];
    v_at_base boolean;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_trd.sales_order_amendment: amendments are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    v_at_base := EXISTS (SELECT 1 FROM brant_trd.sales_order o WHERE o.tenant_id = NEW.tenant_id AND o.company_id = NEW.company_id
        AND o.order_id = NEW.order_id AND o.status = 'active' AND o.order_revision = NEW.base_revision AND o.order_hash = NEW.base_hash);
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'draft' OR NEW.version <> 1 OR jsonb_array_length(NEW.preparations::jsonb) <> 1 OR NOT v_at_base THEN
            RAISE EXCEPTION 'brant_trd.sales_order_amendment: an amendment is prepared a draft against the active order''s current revision'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.version = OLD.version + 1 AND (
            (OLD.status = 'draft' AND NEW.status = 'draft' AND v_at_base AND (to_jsonb(OLD) - prepared) = (to_jsonb(NEW) - prepared)
                AND jsonb_array_length(NEW.preparations::jsonb) = jsonb_array_length(OLD.preparations::jsonb) + 1
                AND (NEW.preparations::jsonb - (jsonb_array_length(NEW.preparations::jsonb) - 1)) = OLD.preparations::jsonb)
            OR (OLD.status = 'draft' AND NEW.status = 'pending_approval' AND v_at_base AND (to_jsonb(OLD) - submitted) = (to_jsonb(NEW) - submitted))
            OR (OLD.status = 'pending_approval' AND NEW.status IN ('approved', 'rejected') AND (to_jsonb(OLD) - decided) = (to_jsonb(NEW) - decided)
                AND (NEW.status = 'rejected' OR v_at_base)
                AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(NEW.preparations::jsonb) p WHERE p->>'by_id' = NEW.decided_by_id))
            OR (OLD.status = 'approved' AND NEW.status = 'applied' AND (to_jsonb(OLD) - applied) = (to_jsonb(NEW) - applied)
                AND EXISTS (SELECT 1 FROM brant_trd.sales_order_revision r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
                    AND r.order_id = NEW.order_id AND r.amendment_id = NEW.amendment_id AND r.order_revision = NEW.applied_revision
                    AND r.amendment_hash = NEW.amendment_hash AND r.previous_revision = NEW.base_revision AND r.previous_hash = NEW.base_hash))
            OR (OLD.status IN ('draft', 'pending_approval') AND NEW.status = 'withdrawn'
                AND (to_jsonb(OLD) - withdrawn) = (to_jsonb(NEW) - withdrawn))) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_trd.sales_order_amendment: amendment % is %; SM-TRD-ORDER-AMENDMENT does not move it to % that way',
        OLD.amendment_id, OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER sales_order_amendment_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.sales_order_amendment
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_sales_order_amendment();

-- A revision is inserted for the active order at the revision it amends, by an approved amendment of
-- that revision with that hash. Never changed or deleted.
CREATE FUNCTION brant_trd.guard_sales_order_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.sales_order_revision: order revisions never change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_trd.sales_order o
            JOIN brant_trd.sales_order_amendment a ON a.tenant_id = o.tenant_id AND a.company_id = o.company_id AND a.order_id = o.order_id
            WHERE o.tenant_id = NEW.tenant_id AND o.company_id = NEW.company_id AND o.order_id = NEW.order_id AND o.status = 'active'
                AND o.order_revision = NEW.previous_revision AND o.order_hash = NEW.previous_hash
                AND a.amendment_id = NEW.amendment_id AND a.status = 'approved' AND a.amendment_hash = NEW.amendment_hash
                AND a.base_revision = NEW.previous_revision AND a.base_hash = NEW.previous_hash) THEN
        RAISE EXCEPTION 'brant_trd.sales_order_revision: a revision follows the active order''s current one by an approved amendment of it'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER sales_order_revision_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.sales_order_revision
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_sales_order_revision();

-- The commit evidence of an apply binds the revision it produced. Never changed or deleted.
CREATE FUNCTION brant_trd.guard_sales_order_amendment_commit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.sales_order_amendment_commit: commit evidence never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_trd.sales_order_revision r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
            AND r.order_id = NEW.order_id AND r.order_revision = NEW.order_revision AND r.order_hash = NEW.order_hash
            AND r.previous_hash = NEW.from_order_hash AND r.amendment_id = NEW.amendment_id AND r.amendment_hash = NEW.amendment_hash
            AND r.row_version = NEW.row_version) THEN
        RAISE EXCEPTION 'brant_trd.sales_order_amendment_commit: commit evidence binds the revision the amendment produced'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER sales_order_amendment_commit_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.sales_order_amendment_commit
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_sales_order_amendment_commit();

-- An active order ends once, at its current revision. Never changed or deleted.
CREATE FUNCTION brant_trd.guard_sales_order_end() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.sales_order_end: the end of an order never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_trd.sales_order o WHERE o.tenant_id = NEW.tenant_id AND o.company_id = NEW.company_id
            AND o.order_id = NEW.order_id AND o.status = 'active' AND o.order_revision = NEW.order_revision AND o.order_hash = NEW.order_hash) THEN
        RAISE EXCEPTION 'brant_trd.sales_order_end: an active order ends at its current revision' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER sales_order_end_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.sales_order_end
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_sales_order_end();

-- The order itself: as before (000005), and now an active order also moves to the next revision with
-- the revision row and the commit evidence of the amendment that produced it, at that row version,
-- or ends with its end row at that row version. The current revision changes only that way.
CREATE OR REPLACE FUNCTION brant_trd.guard_sales_order() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    mutable constant text[] := ARRAY['status', 'row_version', 'activated_review_id', 'updated_at', 'order_revision', 'order_hash'];
    v_same  boolean;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_trd.sales_order: orders are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'draft' OR NEW.row_version <> 1 OR NEW.order_revision <> 1 OR NEW.order_hash <> NEW.snapshot_hash
                OR NOT EXISTS (SELECT 1 FROM brant_trd.quote_conversion_slot s WHERE s.tenant_id = NEW.tenant_id
                    AND s.company_id = NEW.company_id AND s.quote_id = NEW.quote_id AND s.slot = NEW.slot AND s.order_id = NEW.order_id
                    AND s.revision = NEW.revision AND s.business_hash = NEW.business_hash)
                OR NOT EXISTS (SELECT 1 FROM brant_trd.quote_document d WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id
                    AND d.quote_id = NEW.quote_id AND d.revision = NEW.revision AND d.business_hash = NEW.business_hash
                    AND d.number = NEW.number AND d.content_sha256 = NEW.document_sha256)
                OR NOT EXISTS (SELECT 1 FROM brant_trd.acceptance a WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id
                    AND a.acceptance_id = NEW.acceptance_id AND a.quote_id = NEW.quote_id AND a.revision = NEW.revision
                    AND a.business_hash = NEW.business_hash AND a.effectivity = 'valid' AND a.effectivity_version = NEW.acceptance_version
                    AND a.arrival_at = NEW.arrival_at AND a.arrival_basis = NEW.arrival_basis AND a.expires_at = NEW.expires_at) THEN
            RAISE EXCEPTION 'brant_trd.sales_order: an order is prepared a draft for its conversion slot, binding the formal document and a valid acceptance of the revision'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (to_jsonb(OLD) - mutable) IS DISTINCT FROM (to_jsonb(NEW) - mutable) OR NEW.row_version <> OLD.row_version + 1 THEN
        RAISE EXCEPTION 'brant_trd.sales_order: order % keeps its identity, binding and snapshot; its row version moves by one',
            OLD.order_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    v_same := NEW.order_revision = OLD.order_revision AND NEW.order_hash = OLD.order_hash;
    IF v_same AND OLD.status = 'draft' AND NEW.status = 'draft' AND NEW.activated_review_id IS NULL THEN
        RETURN NEW;
    END IF;
    IF v_same AND OLD.status = 'draft' AND NEW.status = 'active' AND EXISTS (SELECT 1 FROM brant_trd.sales_order_commit_evidence e
            JOIN brant_trd.sales_order_review r ON r.tenant_id = e.tenant_id AND r.company_id = e.company_id AND r.order_id = e.order_id
                AND r.review_id = e.review_id
            WHERE e.tenant_id = NEW.tenant_id AND e.company_id = NEW.company_id AND e.order_id = NEW.order_id
                AND e.order_revision = NEW.order_revision AND e.order_hash = NEW.order_hash AND e.review_id = NEW.activated_review_id
                AND e.row_version = NEW.row_version AND r.status = 'approved') THEN
        RETURN NEW;
    END IF;
    IF v_same AND OLD.status = 'draft' AND NEW.status IN ('cancelled', 'closed_unfulfilled') AND NEW.activated_review_id IS NULL
            AND EXISTS (SELECT 1 FROM brant_trd.sales_order_closure c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
                AND c.order_id = NEW.order_id AND c.status = NEW.status AND c.row_version = NEW.row_version) THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'active' AND NEW.status = 'active' AND NEW.activated_review_id = OLD.activated_review_id
            AND (v_same OR (NEW.order_revision = OLD.order_revision + 1
                AND EXISTS (SELECT 1 FROM brant_trd.sales_order_revision r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
                    AND r.order_id = NEW.order_id AND r.order_revision = NEW.order_revision AND r.order_hash = NEW.order_hash
                    AND r.previous_hash = OLD.order_hash AND r.row_version = NEW.row_version)
                AND EXISTS (SELECT 1 FROM brant_trd.sales_order_amendment_commit e WHERE e.tenant_id = NEW.tenant_id
                    AND e.company_id = NEW.company_id AND e.order_id = NEW.order_id AND e.order_revision = NEW.order_revision
                    AND e.row_version = NEW.row_version))) THEN
        RETURN NEW;
    END IF;
    IF v_same AND OLD.status = 'active' AND NEW.status IN ('cancelled', 'closed') AND NEW.activated_review_id = OLD.activated_review_id
            AND EXISTS (SELECT 1 FROM brant_trd.sales_order_end e WHERE e.tenant_id = NEW.tenant_id AND e.company_id = NEW.company_id
                AND e.order_id = NEW.order_id AND e.status = NEW.status AND e.row_version = NEW.row_version
                AND e.order_revision = NEW.order_revision AND e.order_hash = NEW.order_hash) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_trd.sales_order: order % is %; it does not become % here', OLD.order_id, OLD.status, NEW.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;

ALTER TABLE brant_trd.sales_order_amendment ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.sales_order_amendment FORCE ROW LEVEL SECURITY;
CREATE POLICY sales_order_amendment_scope ON brant_trd.sales_order_amendment
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_trd.sales_order_revision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.sales_order_revision FORCE ROW LEVEL SECURITY;
CREATE POLICY sales_order_revision_scope ON brant_trd.sales_order_revision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_trd.sales_order_amendment_commit ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.sales_order_amendment_commit FORCE ROW LEVEL SECURITY;
CREATE POLICY sales_order_amendment_commit_scope ON brant_trd.sales_order_amendment_commit
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_trd.sales_order_end ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.sales_order_end FORCE ROW LEVEL SECURITY;
CREATE POLICY sales_order_end_scope ON brant_trd.sales_order_end
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
