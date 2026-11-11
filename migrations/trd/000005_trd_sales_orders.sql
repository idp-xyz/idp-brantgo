-- TRD: sales orders in draft and their activation, the order review, ActiveOrderRef v1 for PUR, and
-- the owner's decision of 2026-11-11 on accepted revisions (WBS S3-07, the order half of S3-13;
-- tactical model TRD-AGG-SalesOrder, SM-TRD-ORDER, SM-TRD-ORDER-REVIEW; TRD-INV-010, 013, 014,
-- 016, 017, 021; ADR-BR-004, ADR-BR-013; Context Map R19; Python t03_storage prepare_order and
-- activate_order, t03_order_snapshot, t03_order_qualification and t04_order_review).
--
-- order.prepare converts an accepted, formed quote revision into a sales order draft in one
-- transaction of the quote root: it takes the root's conversion slot (full_v1, kept for good) and
-- creates the order, whose commercial snapshot copies the sealed commercial document of the
-- revision and binds the acceptance; the snapshot's SHA-256 is the order hash (the CHECK verifies it
-- on every write) and neither ever changes. A draft is reviewed (order.review.request, then
-- order.review.decide by a human independent of the preparer and the requester, acknowledging each
-- upstream risk found; order.approval.revoke), activated (order.activate: the commit evidence, the
-- review it used and the status in one transaction), or cancelled or closed unfulfilled with a
-- closure row. Every order write locks the quote root FOR UPDATE first (the conflict key of quotes,
-- acceptances and orders, ADR-BR-002), then the order FOR UPDATE, and moves the order's row version.
-- Nothing here is deleted; reviews are decided once and revoked once; commit evidence and closures
-- are only inserted.
--
-- brant_trd.guard_order_active_v1 publishes ActiveOrderRef v1 (Context Map R19, guardLock): whether
-- an order revision is active at its order hash and its activating approval is not revoked, holding
-- the order row FOR SHARE until the caller's transaction ends.
--
-- The owner's decision of 2026-11-11: a revision with a live (valid or disputed) acceptance — a
-- formed contract — is neither superseded, withdrawn nor cancelled until a human invalidates the
-- acceptance; acceptance.invalidate therefore also ends a disputed acceptance (valid → disputed →
-- invalidated), and an AI agent may take the conversion slot through order.prepare (人工或 AI).

-- An AI agent prepares orders (command catalog: order.prepare 人工或 AI), and preparing takes the slot.
ALTER TABLE brant_trd.quote_conversion_slot DROP CONSTRAINT quote_conversion_slot_taken_by_kind_check;
ALTER TABLE brant_trd.quote_conversion_slot ADD CONSTRAINT quote_conversion_slot_taken_by_kind_check CHECK (taken_by_kind IN ('human', 'ai'));

-- A disputed acceptance is invalidated once more (version 3); nothing returns to valid.
ALTER TABLE brant_trd.acceptance DROP CONSTRAINT acceptance_effectivity_version_check1;
ALTER TABLE brant_trd.acceptance ADD CONSTRAINT acceptance_effectivity_version_check1 CHECK (effectivity_version <= 3);
ALTER TABLE brant_trd.acceptance ADD CONSTRAINT acceptance_disputed_version CHECK (effectivity <> 'disputed' OR effectivity_version = 2);
ALTER TABLE brant_trd.acceptance_effectivity DROP CONSTRAINT acceptance_effectivity_effectivity_version_check;
ALTER TABLE brant_trd.acceptance_effectivity ADD CONSTRAINT acceptance_effectivity_effectivity_version_check
    CHECK (effectivity_version BETWEEN 1 AND 3);
ALTER TABLE brant_trd.acceptance_effectivity ADD CONSTRAINT acceptance_effectivity_third_invalidates
    CHECK (effectivity_version <> 3 OR effectivity = 'invalidated');

CREATE OR REPLACE FUNCTION brant_trd.guard_acceptance() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_trd.acceptance: acceptances are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.effectivity <> 'valid' OR NOT EXISTS (SELECT 1 FROM brant_trd.customer_response p
                JOIN brant_trd.quote_revision r ON r.tenant_id = p.tenant_id AND r.company_id = p.company_id AND r.quote_id = p.quote_id
                    AND r.revision = p.revision
                WHERE p.tenant_id = NEW.tenant_id AND p.company_id = NEW.company_id AND p.response_id = NEW.response_id
                    AND p.classification = 'full_unconditional' AND p.quote_id = NEW.quote_id AND p.revision = NEW.revision
                    AND p.business_hash = NEW.business_hash AND p.number = NEW.number AND p.expires_at = NEW.expires_at
                    AND p.source_id = NEW.message_source_id AND p.locator = NEW.message_locator
                    AND r.status = 'active' AND r.business_hash = NEW.business_hash
                    AND CASE WHEN coalesce(p.received_at_basis, '') IN ('channel_server', 'import_record')
                        THEN NEW.arrival_at = p.received_at AND NEW.arrival_basis = p.received_at_basis
                        ELSE NEW.arrival_basis = 'verification' END)
                OR EXISTS (SELECT 1 FROM brant_trd.quote_risk_hold h WHERE h.tenant_id = NEW.tenant_id AND h.company_id = NEW.company_id
                    AND h.quote_id = NEW.quote_id AND h.revision = NEW.revision) THEN
            RAISE EXCEPTION 'brant_trd.acceptance: an acceptance is created valid from a full_unconditional response of the active, unheld revision'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NOT ((OLD.effectivity = 'valid' AND NEW.effectivity IN ('invalidated', 'disputed'))
                OR (OLD.effectivity = 'disputed' AND NEW.effectivity = 'invalidated'))
            OR NEW.effectivity_version <> OLD.effectivity_version + 1
            OR (to_jsonb(OLD) - 'effectivity' - 'effectivity_version') IS DISTINCT FROM (to_jsonb(NEW) - 'effectivity' - 'effectivity_version')
            OR NOT EXISTS (SELECT 1 FROM brant_trd.acceptance_effectivity f WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id
                AND f.acceptance_id = NEW.acceptance_id AND f.effectivity_version = NEW.effectivity_version
                AND f.effectivity = NEW.effectivity) THEN
        RAISE EXCEPTION 'brant_trd.acceptance: acceptance % is %; a valid one is invalidated or disputed, a disputed one invalidated, once each, with its ledger entry, and nothing else changes',
            OLD.acceptance_id, OLD.effectivity USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

-- Version 1 records the verification; version 2 the invalidation or dispute of the valid acceptance;
-- version 3 the invalidation of the disputed one.
CREATE OR REPLACE FUNCTION brant_trd.guard_acceptance_effectivity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.acceptance_effectivity: the effectivity ledger never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_trd.acceptance a WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id
            AND a.acceptance_id = NEW.acceptance_id
            AND CASE NEW.effectivity_version
                WHEN 1 THEN a.effectivity = 'valid' AND a.effectivity_version = 1 AND a.verified_by_id = NEW.changed_by_id
                    AND a.verify_epoch = NEW.change_epoch AND a.verified_at = NEW.changed_at
                WHEN 2 THEN a.effectivity = 'valid' AND a.effectivity_version = 1
                ELSE a.effectivity = 'disputed' AND a.effectivity_version = 2 END) THEN
        RAISE EXCEPTION 'brant_trd.acceptance_effectivity: an entry records the verification or the next change of the acceptance'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

-- The owner's decision of 2026-11-11 (ACCEPTANCE_ALREADY_EXISTS): a revision a live acceptance
-- accepts is neither superseded, withdrawn nor cancelled. The commands check it under the quote
-- root's FOR UPDATE lock, which acceptance.invalidate takes first too; here the live acceptances are
-- locked FOR SHARE as well, so an invalidation in progress is waited for and judged as it commits.
CREATE FUNCTION brant_trd.guard_contracted_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status IN ('superseded', 'withdrawn', 'cancelled') AND NEW.status IS DISTINCT FROM OLD.status THEN
        PERFORM 1 FROM brant_trd.acceptance a
        WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id AND a.quote_id = NEW.quote_id AND a.revision = NEW.revision
            AND a.effectivity IN ('valid', 'disputed')
        FOR SHARE;
        IF FOUND THEN
            RAISE EXCEPTION 'brant_trd.quote_revision: ACCEPTANCE_ALREADY_EXISTS: revision % has a live acceptance (a formed contract); it becomes % only after the acceptance is invalidated',
                NEW.revision, NEW.status USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER quote_revision_contract_guard BEFORE UPDATE OF status ON brant_trd.quote_revision
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_contracted_revision();

CREATE TABLE brant_trd.sales_order (
    tenant_id           text        NOT NULL,
    company_id          text        NOT NULL,
    order_id            text        NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    quote_id            text        NOT NULL,
    slot                text        NOT NULL CHECK (slot = 'full_v1'),
    revision            bigint      NOT NULL CHECK (revision > 0),
    business_hash       text        NOT NULL CHECK (business_hash ~ '^[0-9a-f]{64}$'),
    number              text        NOT NULL,
    document_sha256     text        NOT NULL CHECK (document_sha256 ~ '^[0-9a-f]{64}$'),
    acceptance_id       text        NOT NULL,
    acceptance_version  bigint      NOT NULL CHECK (acceptance_version = 1),
    arrival_at          timestamptz NOT NULL,
    arrival_basis       text        NOT NULL CHECK (arrival_basis IN ('channel_server', 'import_record', 'verification')),
    expires_at          timestamptz NOT NULL,
    order_revision      bigint      NOT NULL CHECK (order_revision = 1),
    snapshot            text        NOT NULL CHECK (left(snapshot, 1) = '{'),
    order_hash          text        NOT NULL CHECK (order_hash = encode(sha256(convert_to(snapshot, 'UTF8')), 'hex')),
    status              text        NOT NULL CHECK (status IN ('draft', 'active', 'cancelled', 'closed_unfulfilled', 'closed')),
    row_version         bigint      NOT NULL CHECK (row_version > 0),
    prepared_by_kind    text        NOT NULL CHECK (prepared_by_kind IN ('human', 'ai')),
    prepared_by_id      text        NOT NULL,
    prepare_epoch       bigint      NOT NULL CHECK (prepare_epoch > 0),
    prepared_at         timestamptz NOT NULL,
    activated_review_id text,
    updated_at          timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, order_id),
    -- One order per root and slot (TRD-INV-010: (company, root_quote_id, slot=full_v1) 唯一).
    UNIQUE (tenant_id, company_id, quote_id, slot),
    FOREIGN KEY (tenant_id, company_id, quote_id, slot) REFERENCES brant_trd.quote_conversion_slot (tenant_id, company_id, quote_id, slot),
    FOREIGN KEY (tenant_id, company_id, quote_id, revision) REFERENCES brant_trd.quote_document (tenant_id, company_id, quote_id, revision),
    FOREIGN KEY (tenant_id, company_id, acceptance_id) REFERENCES brant_trd.acceptance (tenant_id, company_id, acceptance_id),
    -- The contract formed (ADR-BR-013): the acceptance arrived before the quote expired. No clock is
    -- compared with expires_at.
    CHECK (arrival_at < expires_at),
    CHECK (status <> 'active' OR activated_review_id IS NOT NULL),
    CHECK (status <> 'draft' OR activated_review_id IS NULL)
);

CREATE TABLE brant_trd.sales_order_review (
    tenant_id          text        NOT NULL,
    company_id         text        NOT NULL,
    order_id           text        NOT NULL,
    review_id          text        NOT NULL CHECK (review_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_revision     bigint      NOT NULL CHECK (order_revision > 0),
    order_hash         text        NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    status             text        NOT NULL CHECK (status IN ('open', 'approved', 'rejected', 'revoked')),
    requested_by_kind  text        NOT NULL CHECK (requested_by_kind IN ('human', 'ai')),
    requested_by_id    text        NOT NULL,
    request_epoch      bigint      NOT NULL CHECK (request_epoch > 0),
    requested_at       timestamptz NOT NULL,
    decided_by_kind    text        CHECK (decided_by_kind = 'human'),
    decided_by_id      text,
    decide_epoch       bigint      CHECK (decide_epoch > 0),
    decided_at         timestamptz,
    decision_reason    text        CHECK (char_length(decision_reason) BETWEEN 1 AND 2000),
    decision_evidence  text        CHECK (left(decision_evidence, 1) = '['),
    expires_at         timestamptz,
    -- The upstream risks the approval acknowledged one by one, as canonical JSON
    -- ([{"code": ..., "detail": ..., "path": ...}, ...]).
    acknowledged_risks text        CHECK (left(acknowledged_risks, 1) = '['),
    revoked_by_kind    text        CHECK (revoked_by_kind = 'human'),
    revoked_by_id      text,
    revoke_epoch       bigint      CHECK (revoke_epoch > 0),
    revoked_at         timestamptz,
    revoke_reason      text        CHECK (char_length(revoke_reason) BETWEEN 1 AND 2000),
    revoke_evidence    text        CHECK (left(revoke_evidence, 1) = '['),
    PRIMARY KEY (tenant_id, company_id, order_id, review_id),
    FOREIGN KEY (tenant_id, company_id, order_id) REFERENCES brant_trd.sales_order (tenant_id, company_id, order_id),
    CHECK ((status = 'open') = (decided_at IS NULL)),
    CHECK ((decided_at IS NULL) = (decided_by_id IS NULL) AND (decided_by_id IS NULL) = (decided_by_kind IS NULL)
        AND (decided_by_id IS NULL) = (decide_epoch IS NULL) AND (decided_by_id IS NULL) = (decision_evidence IS NULL)),
    -- An approval has its validity, set by the reviewer at the decision and unrelated to the quote's
    -- (SM-TRD-ORDER-REVIEW), and its acknowledged risks; a rejection a reason.
    CHECK ((status IN ('approved', 'revoked')) = (expires_at IS NOT NULL)),
    CHECK ((status IN ('approved', 'revoked')) = (acknowledged_risks IS NOT NULL)),
    CHECK (expires_at > decided_at),
    CHECK (status <> 'rejected' OR decision_reason IS NOT NULL),
    CHECK ((status = 'revoked') = (revoked_at IS NOT NULL)),
    CHECK ((revoked_at IS NULL) = (revoked_by_id IS NULL) AND (revoked_by_id IS NULL) = (revoked_by_kind IS NULL)
        AND (revoked_by_id IS NULL) = (revoke_epoch IS NULL) AND (revoked_by_id IS NULL) = (revoke_reason IS NULL)
        AND (revoked_by_id IS NULL) = (revoke_evidence IS NULL)),
    -- The reviewer is not the requester (TRD-INV-013; the preparer is checked by the trigger).
    CHECK (decided_by_id IS DISTINCT FROM requested_by_id)
);

-- At most one open review per order.
CREATE UNIQUE INDEX sales_order_review_open ON brant_trd.sales_order_review (tenant_id, company_id, order_id) WHERE status = 'open';

CREATE TABLE brant_trd.sales_order_commit_evidence (
    tenant_id          text        NOT NULL,
    company_id         text        NOT NULL,
    order_id           text        NOT NULL,
    order_revision     bigint      NOT NULL CHECK (order_revision > 0),
    command            text        NOT NULL CHECK (command = 'order.activate'),
    idempotency_key    text        NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_hash         text        NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    review_id          text        NOT NULL,
    quote_id           text        NOT NULL,
    revision           bigint      NOT NULL,
    business_hash      text        NOT NULL CHECK (business_hash ~ '^[0-9a-f]{64}$'),
    approval_id        text        NOT NULL,
    acceptance_id      text        NOT NULL,
    acceptance_version bigint      NOT NULL CHECK (acceptance_version = 1),
    arrival_at         timestamptz NOT NULL,
    expires_at         timestamptz NOT NULL,
    cost_snapshot_id   text        NOT NULL,
    authority_epoch    bigint      NOT NULL CHECK (authority_epoch > 0),
    control_epoch      bigint      NOT NULL CHECK (control_epoch >= 0),
    row_version        bigint      NOT NULL CHECK (row_version > 1),
    committed_by_kind  text        NOT NULL CHECK (committed_by_kind = 'human'),
    committed_by_id    text        NOT NULL,
    committed_at       timestamptz NOT NULL,
    -- The evidence entries as canonical JSON ([{"name": ..., "value": ...}, ...]) and their SHA-256.
    entries            text        NOT NULL CHECK (left(entries, 1) = '['),
    entries_sha256     text        NOT NULL CHECK (entries_sha256 = encode(sha256(convert_to(entries, 'UTF8')), 'hex')),
    PRIMARY KEY (tenant_id, company_id, order_id, order_revision),
    FOREIGN KEY (tenant_id, company_id, order_id) REFERENCES brant_trd.sales_order (tenant_id, company_id, order_id),
    FOREIGN KEY (tenant_id, company_id, order_id, review_id) REFERENCES brant_trd.sales_order_review (tenant_id, company_id, order_id, review_id),
    FOREIGN KEY (tenant_id, company_id, quote_id, approval_id) REFERENCES brant_trd.quote_approval (tenant_id, company_id, quote_id, approval_id),
    FOREIGN KEY (tenant_id, company_id, acceptance_id) REFERENCES brant_trd.acceptance (tenant_id, company_id, acceptance_id),
    CHECK (arrival_at < expires_at)
);

CREATE TABLE brant_trd.sales_order_closure (
    tenant_id      text        NOT NULL,
    company_id     text        NOT NULL,
    order_id       text        NOT NULL,
    status         text        NOT NULL CHECK (status IN ('cancelled', 'closed_unfulfilled')),
    closed_by_kind text        NOT NULL CHECK (closed_by_kind = 'human'),
    closed_by_id   text        NOT NULL,
    close_epoch    bigint      NOT NULL CHECK (close_epoch > 0),
    closed_at      timestamptz NOT NULL,
    reason         text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 2000),
    evidence       text        NOT NULL CHECK (left(evidence, 1) = '['),
    row_version    bigint      NOT NULL CHECK (row_version > 1),
    PRIMARY KEY (tenant_id, company_id, order_id),
    FOREIGN KEY (tenant_id, company_id, order_id) REFERENCES brant_trd.sales_order (tenant_id, company_id, order_id)
);

-- An order is created a draft at row version 1 for the conversion slot taken for it — the revision
-- and business hash the slot was taken for — binding the revision's formal document and a valid
-- acceptance of the revision as it formed the contract. Its identity, binding and snapshot never
-- change; each command moves its row version by one; a draft becomes active with its commit
-- evidence and the review it names, cancelled or closed unfulfilled with its closure; an active
-- order stays active here (its amendments, cancellation and closes come with WBS S3-15). Never
-- deleted.
CREATE FUNCTION brant_trd.guard_sales_order() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    mutable constant text[] := ARRAY['status', 'row_version', 'activated_review_id', 'updated_at'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_trd.sales_order: orders are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'draft' OR NEW.row_version <> 1
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
    IF OLD.status = 'draft' AND NEW.status = 'draft' AND NEW.activated_review_id IS NULL THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'draft' AND NEW.status = 'active' AND EXISTS (SELECT 1 FROM brant_trd.sales_order_commit_evidence e
            JOIN brant_trd.sales_order_review r ON r.tenant_id = e.tenant_id AND r.company_id = e.company_id AND r.order_id = e.order_id
                AND r.review_id = e.review_id
            WHERE e.tenant_id = NEW.tenant_id AND e.company_id = NEW.company_id AND e.order_id = NEW.order_id
                AND e.order_revision = NEW.order_revision AND e.order_hash = NEW.order_hash AND e.review_id = NEW.activated_review_id
                AND e.row_version = NEW.row_version AND r.status = 'approved') THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'draft' AND NEW.status IN ('cancelled', 'closed_unfulfilled') AND NEW.activated_review_id IS NULL
            AND EXISTS (SELECT 1 FROM brant_trd.sales_order_closure c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
                AND c.order_id = NEW.order_id AND c.status = NEW.status AND c.row_version = NEW.row_version) THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'active' AND NEW.status = 'active' AND NEW.activated_review_id = OLD.activated_review_id THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_trd.sales_order: order % is %; it does not become % here', OLD.order_id, OLD.status, NEW.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER sales_order_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.sales_order
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_sales_order();

-- A review is requested open for a draft order at its revision and hash; it is decided once by a
-- human who neither prepared the order nor requested the review (approved, with its validity and the
-- risks it acknowledged, or rejected with a reason), and an approval is revoked once. Nothing else
-- of it changes; never deleted.
CREATE FUNCTION brant_trd.guard_sales_order_review() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    decided constant text[] := ARRAY['status', 'decided_by_kind', 'decided_by_id', 'decide_epoch', 'decided_at', 'decision_reason',
        'decision_evidence', 'expires_at', 'acknowledged_risks'];
    revoked constant text[] := ARRAY['status', 'revoked_by_kind', 'revoked_by_id', 'revoke_epoch', 'revoked_at', 'revoke_reason',
        'revoke_evidence'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_trd.sales_order_review: reviews are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'open' OR NOT EXISTS (SELECT 1 FROM brant_trd.sales_order o WHERE o.tenant_id = NEW.tenant_id
                AND o.company_id = NEW.company_id AND o.order_id = NEW.order_id AND o.status = 'draft'
                AND o.order_revision = NEW.order_revision AND o.order_hash = NEW.order_hash) THEN
            RAISE EXCEPTION 'brant_trd.sales_order_review: a review is requested open for a draft order at its revision and hash'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'open' AND NEW.status IN ('approved', 'rejected') AND (to_jsonb(OLD) - decided) = (to_jsonb(NEW) - decided)
            AND NOT EXISTS (SELECT 1 FROM brant_trd.sales_order o WHERE o.tenant_id = NEW.tenant_id AND o.company_id = NEW.company_id
                AND o.order_id = NEW.order_id AND o.prepared_by_id = NEW.decided_by_id) THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'approved' AND NEW.status = 'revoked' AND (to_jsonb(OLD) - revoked) = (to_jsonb(NEW) - revoked) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_trd.sales_order_review: review % is %; it is decided once by an independent human and an approval revoked once',
        OLD.review_id, OLD.status USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER sales_order_review_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.sales_order_review
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_sales_order_review();

-- The commit evidence of an activation binds the draft order at its revision and hash, an approved
-- review of exactly that revision and hash, and the valid acceptance the order binds. Never changed
-- or deleted.
CREATE FUNCTION brant_trd.guard_sales_order_commit_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.sales_order_commit_evidence: commit evidence never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_trd.sales_order o
            JOIN brant_trd.sales_order_review r ON r.tenant_id = o.tenant_id AND r.company_id = o.company_id AND r.order_id = o.order_id
            JOIN brant_trd.acceptance a ON a.tenant_id = o.tenant_id AND a.company_id = o.company_id AND a.acceptance_id = o.acceptance_id
            WHERE o.tenant_id = NEW.tenant_id AND o.company_id = NEW.company_id AND o.order_id = NEW.order_id AND o.status = 'draft'
                AND o.order_revision = NEW.order_revision AND o.order_hash = NEW.order_hash AND o.quote_id = NEW.quote_id
                AND o.revision = NEW.revision AND o.business_hash = NEW.business_hash AND o.acceptance_id = NEW.acceptance_id
                AND o.arrival_at = NEW.arrival_at AND o.expires_at = NEW.expires_at
                AND r.review_id = NEW.review_id AND r.status = 'approved' AND r.order_revision = NEW.order_revision
                AND r.order_hash = NEW.order_hash AND a.effectivity = 'valid' AND a.effectivity_version = NEW.acceptance_version) THEN
        RAISE EXCEPTION 'brant_trd.sales_order_commit_evidence: commit evidence binds the draft order, an approved review of it and its valid acceptance'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER sales_order_commit_evidence_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.sales_order_commit_evidence
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_sales_order_commit_evidence();

-- A cancellation or unfulfilled close ends a draft order; never changed or deleted.
CREATE FUNCTION brant_trd.guard_sales_order_closure() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.sales_order_closure: a closure never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_trd.sales_order o WHERE o.tenant_id = NEW.tenant_id AND o.company_id = NEW.company_id
            AND o.order_id = NEW.order_id AND o.status = 'draft') THEN
        RAISE EXCEPTION 'brant_trd.sales_order_closure: a cancellation or unfulfilled close ends a draft order'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER sales_order_closure_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.sales_order_closure
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_sales_order_closure();

ALTER TABLE brant_trd.sales_order ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.sales_order FORCE ROW LEVEL SECURITY;
CREATE POLICY sales_order_scope ON brant_trd.sales_order
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_trd.sales_order_review ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.sales_order_review FORCE ROW LEVEL SECURITY;
CREATE POLICY sales_order_review_scope ON brant_trd.sales_order_review
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_trd.sales_order_commit_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.sales_order_commit_evidence FORCE ROW LEVEL SECURITY;
CREATE POLICY sales_order_commit_evidence_scope ON brant_trd.sales_order_commit_evidence
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_trd.sales_order_closure ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.sales_order_closure FORCE ROW LEVEL SECURITY;
CREATE POLICY sales_order_closure_scope ON brant_trd.sales_order_closure
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

-- ActiveOrderRef v1 / OrderGuard (Context Map R19 TRD->PUR, guardLock; ADR-BR-004 option B,
-- ADR-BR-011: the caller locks its own root first). It locks the one order row FOR SHARE in the
-- caller's transaction. order.approval.revoke, the order's activation and every later order command
-- lock that row FOR UPDATE and move its row version, so each either committed before the guard read
-- it, and the verdict says so, or waits until the caller's transaction ends; in READ COMMITTED a
-- guard that waited reads the row as that command left it.
--
-- It always returns one row in the caller's scope (app.tenant_id, app.company_id). known is false,
-- with code REFERENCE_INVALID, for an order that does not exist there. Otherwise the columns are the
-- order's: its status, order revision and hash, the quote revision it converted, the review that
-- activated it and whether that approval was revoked; code is NULL when the order is active at
-- p_order_revision and p_order_hash with its activating approval in force, REFERENCE_STALE when it
-- is at another order revision or hash, ORDER_NOT_ACTIVE when it is not active, and
-- ORDER_APPROVAL_REVOKED when the approval that activated it was revoked (SM-TRD-ORDER-REVIEW: the
-- revocation stops new forward use without rewriting the activation). The data scope (own, team) is
-- the caller's to check before its transaction (TRD's ActiveOrderLookups). SECURITY DEFINER, fixed
-- search_path, qualified names; brant_app alone may execute it.
CREATE FUNCTION brant_trd.guard_order_active_v1(p_order_id text, p_order_revision bigint, p_order_hash text)
    RETURNS TABLE (known boolean, status text, order_revision bigint, order_hash text, quote_id text, revision bigint,
                   business_hash text, review_id text, approval_revoked boolean, code text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
    v_order   record;
    v_revoked boolean;
BEGIN
    IF p_order_id IS NULL OR p_order_revision IS NULL OR p_order_hash IS NULL THEN
        RAISE EXCEPTION 'brant_trd.guard_order_active_v1: order_id, order_revision and order_hash are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    SELECT o.status, o.order_revision, o.order_hash, o.quote_id, o.revision, o.business_hash, o.activated_review_id INTO v_order
    FROM brant_trd.sales_order o
    WHERE o.tenant_id = v_tenant AND o.company_id = v_company AND o.order_id = p_order_id
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, NULL::bigint, NULL::text, NULL::text, NULL::bigint, NULL::text, NULL::text, false,
            'REFERENCE_INVALID'::text;
        RETURN;
    END IF;
    v_revoked := EXISTS (SELECT 1 FROM brant_trd.sales_order_review r WHERE r.tenant_id = v_tenant AND r.company_id = v_company
        AND r.order_id = p_order_id AND r.review_id = v_order.activated_review_id AND r.status = 'revoked');
    RETURN QUERY SELECT true, v_order.status, v_order.order_revision, v_order.order_hash, v_order.quote_id, v_order.revision,
        v_order.business_hash, v_order.activated_review_id, v_revoked,
        CASE
            WHEN v_order.order_revision <> p_order_revision OR v_order.order_hash <> p_order_hash THEN 'REFERENCE_STALE'
            WHEN v_order.status <> 'active' THEN 'ORDER_NOT_ACTIVE'
            WHEN v_revoked THEN 'ORDER_APPROVAL_REVOKED'
        END::text;
END
$$;

REVOKE ALL ON FUNCTION brant_trd.guard_order_active_v1(text, bigint, text) FROM PUBLIC;
