-- PUR: purchase commitments (WBS S5-01; tactical model PUR-AGG-PurchaseCommitment, SM-PUR-COMMITMENT,
-- PUR-INV-009 and PUR-INV-010; S5 decision D-14; Python V0.50 has the design only:
-- docs/T04_A46_PURCHASE_DRAFT_REPORT_V0.1.md, A47 #1–#8).
--
-- A purchase commitment binds one purchase draft at its draft hash, and through it the order at its
-- revision and hash, the supplier, the currency and the end of the draft's supply validity. It is
-- requested (human or AI), decided once by a human independent of the draft's preparer and of the
-- requester (approved with a validity not after the supply's, every deviation acknowledged with a
-- reason, or rejected with a reason), revoked before issue, issued once with POL's purchase order
-- number and the evidence of sending, and confirmed by the supplier as issued. A supplier
-- confirmation is a separate fact; one that deviates is recorded and leaves the commitment issued.
-- Changes and cancellation after issue are a later migration (S5-19), which extends the status set.
--
-- A draft has at most one live commitment (requested, approved, issued or confirmed). A draft with
-- an issued commitment is committed and is never voided (PUR-INV-010, TD-AT06); the draft's row
-- keeps commitment_state not_committed, the state it was prepared in, and readers derive the
-- commitment state from this table. The commitment never takes allocation entries of its own: the
-- draft's entries stay effective while the draft is not voided (PUR-INV-002).
--
-- brant_pur.order_commitments_v1 publishes DownstreamCommitments v1 (Context Map R71): what is open
-- against an order in PUR, read without locks inside TRD's own transaction after TRD locked the
-- order (S5 decision D-1). Every PUR command that adds to it calls OrderGuard v1, which holds the
-- order row FOR SHARE, so TRD's read after its order lock is consistent.

CREATE TABLE brant_pur.purchase_commitment (
    tenant_id          text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id         text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    commitment_id      text        NOT NULL CHECK (commitment_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status             text        NOT NULL CONSTRAINT purchase_commitment_status
        CHECK (status IN ('requested', 'approved', 'rejected', 'revoked', 'issued', 'confirmed')),
    row_version        bigint      NOT NULL CHECK (row_version > 0),
    draft_id           text        NOT NULL,
    draft_hash         text        NOT NULL CHECK (draft_hash ~ '^[0-9a-f]{64}$'),
    order_id           text        NOT NULL,
    order_revision     bigint      NOT NULL CHECK (order_revision > 0),
    order_hash         text        NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    supplier_id        text        NOT NULL,
    currency           text        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    supply_valid_until timestamptz NOT NULL,
    owner_actor_id     text        NOT NULL,
    owner_team_id      text,
    requested_by_kind  text        NOT NULL CHECK (requested_by_kind IN ('human', 'ai')),
    requested_by_id    text        NOT NULL,
    request_epoch      bigint      NOT NULL CHECK (request_epoch > 0),
    requested_at       timestamptz NOT NULL,
    decided_by_kind    text        CHECK (decided_by_kind = 'human'),
    decided_by_id      text,
    decide_epoch       bigint      CHECK (decide_epoch > 0),
    decided_at         timestamptz,
    decision           text        CHECK (decision IN ('approve', 'reject')),
    valid_until        timestamptz,
    decision_reason    text        CHECK (char_length(decision_reason) BETWEEN 1 AND 1000),
    -- The deviations the approval acknowledged one by one, as canonical JSON
    -- ([{"kind": ..., "order_line_id": ..., "reason": ...}, ...]).
    acknowledgements   text        CHECK (left(acknowledgements, 1) = '['),
    revoked_by_kind    text        CHECK (revoked_by_kind = 'human'),
    revoked_by_id      text,
    revoke_epoch       bigint      CHECK (revoke_epoch > 0),
    revoked_at         timestamptz,
    revoke_reason      text        CHECK (char_length(revoke_reason) BETWEEN 1 AND 1000),
    issued_by_kind     text        CHECK (issued_by_kind = 'human'),
    issued_by_id       text,
    issue_epoch        bigint      CHECK (issue_epoch > 0),
    issued_at          timestamptz,
    po_number          text        CHECK (char_length(po_number) BETWEEN 1 AND 64),
    sequence_id        text,
    sequence_revision  bigint      CHECK (sequence_revision > 0),
    sent_at            timestamptz,
    -- The EVD sources that show the commitment was sent ([{"locator": ..., "source_id": ...}, ...]).
    issue_evidence     text        CHECK (left(issue_evidence, 1) = '['),
    issue_file_id      text,
    issue_file_sha256  text        CHECK (issue_file_sha256 ~ '^[0-9a-f]{64}$'),
    updated_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, commitment_id),
    FOREIGN KEY (tenant_id, company_id, draft_id) REFERENCES brant_pur.purchase_draft (tenant_id, company_id, draft_id),
    CHECK (owner_actor_id = requested_by_id),
    CHECK ((decided_at IS NULL) = (decided_by_id IS NULL) AND (decided_by_id IS NULL) = (decided_by_kind IS NULL)
        AND (decided_by_id IS NULL) = (decide_epoch IS NULL) AND (decided_by_id IS NULL) = (decision IS NULL)),
    CHECK ((revoked_at IS NULL) = (revoked_by_id IS NULL) AND (revoked_by_id IS NULL) = (revoked_by_kind IS NULL)
        AND (revoked_by_id IS NULL) = (revoke_epoch IS NULL) AND (revoked_by_id IS NULL) = (revoke_reason IS NULL)),
    CHECK ((issued_at IS NULL) = (issued_by_id IS NULL) AND (issued_by_id IS NULL) = (issued_by_kind IS NULL)
        AND (issued_by_id IS NULL) = (issue_epoch IS NULL) AND (issued_by_id IS NULL) = (po_number IS NULL)
        AND (issued_by_id IS NULL) = (sequence_id IS NULL) AND (issued_by_id IS NULL) = (sequence_revision IS NULL)
        AND (issued_by_id IS NULL) = (sent_at IS NULL) AND (issued_by_id IS NULL) = (issue_evidence IS NULL)
        AND (issue_file_id IS NULL) = (issue_file_sha256 IS NULL) AND (issued_by_id IS NOT NULL OR issue_file_id IS NULL)),
    -- SM-PUR-COMMITMENT: what each state carries.
    CHECK (CASE status
        WHEN 'requested' THEN row_version = 1 AND decided_at IS NULL AND revoked_at IS NULL AND issued_at IS NULL
        WHEN 'rejected' THEN decision = 'reject' AND decision_reason IS NOT NULL AND valid_until IS NULL AND acknowledgements IS NULL
            AND revoked_at IS NULL AND issued_at IS NULL
        WHEN 'approved' THEN decision = 'approve' AND valid_until IS NOT NULL AND acknowledgements IS NOT NULL AND revoked_at IS NULL
            AND issued_at IS NULL
        WHEN 'revoked' THEN decision = 'approve' AND valid_until IS NOT NULL AND revoked_at IS NOT NULL AND issued_at IS NULL
        ELSE decision = 'approve' AND valid_until IS NOT NULL AND revoked_at IS NULL AND issued_at IS NOT NULL END),
    -- PUR-INV-009: the decider is a human other than the requester (the preparer is checked by the
    -- trigger), deciding after the request; an approval is valid after its decision and not after the
    -- draft's supply validity (A47 #4).
    CHECK (decided_by_id IS DISTINCT FROM requested_by_id),
    CHECK (decided_at >= requested_at),
    CHECK (valid_until > decided_at AND valid_until <= supply_valid_until),
    -- PUR-INV-010: revoked before issue, issued after the approval, sent after the approval and not
    -- after it was recorded.
    CHECK (revoked_at >= decided_at),
    CHECK (issued_at >= decided_at AND issued_at < valid_until AND sent_at >= decided_at AND sent_at <= issued_at)
);

CREATE INDEX purchase_commitment_order ON brant_pur.purchase_commitment (tenant_id, company_id, order_id);
-- One live commitment per draft.
CREATE UNIQUE INDEX purchase_commitment_live ON brant_pur.purchase_commitment (tenant_id, company_id, draft_id)
    WHERE status IN ('requested', 'approved', 'issued', 'confirmed');
-- A purchase order number is the commitment's alone.
CREATE UNIQUE INDEX purchase_commitment_po_number ON brant_pur.purchase_commitment (tenant_id, company_id, po_number)
    WHERE po_number IS NOT NULL;

-- A commitment is requested for a prepared draft and binds it exactly; then it changes only along
-- SM-PUR-COMMITMENT, its version moving by one, each step writing only its own columns: the decision
-- (by a human other than the draft's preparer), the revocation, the issue (of a draft not voided),
-- and the supplier's confirmations, each of which is inserted first. Nothing is deleted.
CREATE FUNCTION brant_pur.guard_purchase_commitment() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_fixed    text[] := ARRAY['tenant_id', 'company_id', 'commitment_id', 'draft_id', 'draft_hash', 'order_id', 'order_revision',
        'order_hash', 'supplier_id', 'currency', 'supply_valid_until', 'owner_actor_id', 'owner_team_id', 'requested_by_kind',
        'requested_by_id', 'request_epoch', 'requested_at'];
    v_decision text[] := ARRAY['decided_by_kind', 'decided_by_id', 'decide_epoch', 'decided_at', 'decision', 'valid_until',
        'decision_reason', 'acknowledgements'];
    v_revoke   text[] := ARRAY['revoked_by_kind', 'revoked_by_id', 'revoke_epoch', 'revoked_at', 'revoke_reason'];
    v_issue    text[] := ARRAY['issued_by_kind', 'issued_by_id', 'issue_epoch', 'issued_at', 'po_number', 'sequence_id',
        'sequence_revision', 'sent_at', 'issue_evidence', 'issue_file_id', 'issue_file_sha256'];
    v_moving   text[] := ARRAY['status', 'row_version', 'updated_at'];
    v_changed  text[];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment: commitments are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'requested' OR NOT EXISTS (SELECT 1 FROM brant_pur.purchase_draft d WHERE d.tenant_id = NEW.tenant_id
                AND d.company_id = NEW.company_id AND d.draft_id = NEW.draft_id AND d.status = 'prepared' AND d.draft_hash = NEW.draft_hash
                AND d.order_id = NEW.order_id AND d.order_revision = NEW.order_revision AND d.order_hash = NEW.order_hash
                AND d.supplier_id = NEW.supplier_id AND d.currency = NEW.currency AND d.supply_valid_until = NEW.supply_valid_until) THEN
            RAISE EXCEPTION 'brant_pur.purchase_commitment: a commitment is requested for a prepared draft and binds it exactly'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    v_changed := CASE
        WHEN OLD.status = 'requested' AND NEW.status IN ('approved', 'rejected') THEN v_decision
        WHEN OLD.status = 'approved' AND NEW.status = 'revoked' THEN v_revoke
        WHEN OLD.status = 'approved' AND NEW.status = 'issued' THEN v_issue
        WHEN OLD.status = 'issued' AND NEW.status IN ('issued', 'confirmed') THEN ARRAY[]::text[]
    END;
    IF v_changed IS NULL OR NEW.row_version <> OLD.row_version + 1
            OR (to_jsonb(NEW) - (v_changed || v_moving)) IS DISTINCT FROM (to_jsonb(OLD) - (v_changed || v_moving))
            OR (NEW.status IN ('approved', 'rejected') AND EXISTS (SELECT 1 FROM brant_pur.purchase_draft d
                WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND d.draft_id = NEW.draft_id
                    AND d.prepared_by_id = NEW.decided_by_id))
            OR (NEW.status = 'issued' AND OLD.status = 'approved' AND NOT EXISTS (SELECT 1 FROM brant_pur.purchase_draft d
                WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND d.draft_id = NEW.draft_id AND d.status = 'prepared'))
            OR (OLD.status = 'issued' AND NOT EXISTS (SELECT 1 FROM brant_pur.supplier_confirmation s
                WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id AND s.commitment_id = NEW.commitment_id
                    AND s.commitment_version = NEW.row_version AND (s.result = 'as_issued') = (NEW.status = 'confirmed'))) THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment: commitment % (%) changes only along SM-PUR-COMMITMENT (PUR-INV-009, PUR-INV-010)',
            OLD.commitment_id, OLD.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER purchase_commitment_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pur.purchase_commitment
    FOR EACH ROW EXECUTE FUNCTION brant_pur.guard_purchase_commitment();

-- What the supplier answered to an issued commitment (A47 #7): the result (as issued, or with
-- deviations, which leaves the commitment issued and open for a change), when it was received, the
-- EVD sources showing it and the supplier's own reference. Each is recorded at the commitment version
-- it moves the commitment to, and never changes.
CREATE TABLE brant_pur.supplier_confirmation (
    tenant_id          text        NOT NULL,
    company_id         text        NOT NULL,
    commitment_id      text        NOT NULL,
    commitment_version bigint      NOT NULL CHECK (commitment_version > 1),
    result             text        NOT NULL CHECK (result IN ('as_issued', 'with_deviations')),
    received_at        timestamptz NOT NULL,
    deviation          text        CHECK (char_length(deviation) BETWEEN 1 AND 1000),
    supplier_reference text        CHECK (char_length(supplier_reference) BETWEEN 1 AND 64),
    evidence           text        NOT NULL CHECK (left(evidence, 1) = '['),
    recorded_by_kind   text        NOT NULL CHECK (recorded_by_kind IN ('human', 'ai')),
    recorded_by_id     text        NOT NULL,
    record_epoch       bigint      NOT NULL CHECK (record_epoch > 0),
    recorded_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, commitment_id, commitment_version),
    FOREIGN KEY (tenant_id, company_id, commitment_id) REFERENCES brant_pur.purchase_commitment (tenant_id, company_id, commitment_id),
    CHECK ((result = 'with_deviations') = (deviation IS NOT NULL)),
    CHECK (received_at <= recorded_at)
);

CREATE FUNCTION brant_pur.guard_supplier_confirmation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_pur.supplier_confirmation: confirmations never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
            AND c.commitment_id = NEW.commitment_id AND c.status = 'issued' AND NEW.commitment_version = c.row_version + 1
            AND NEW.received_at >= c.sent_at) THEN
        RAISE EXCEPTION 'brant_pur.supplier_confirmation: a confirmation answers an issued commitment, received after it was sent'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER supplier_confirmation_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pur.supplier_confirmation
    FOR EACH ROW EXECUTE FUNCTION brant_pur.guard_supplier_confirmation();

-- PUR-INV-010 (TD-AT06): a draft with an issued commitment is committed and is never voided. The rest
-- of the draft's guard is migration 000001's.
CREATE OR REPLACE FUNCTION brant_pur.guard_purchase_draft() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_void text[] := ARRAY['status', 'row_version', 'voided_by_kind', 'voided_by_id', 'void_epoch', 'voided_at', 'void_reason'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pur.purchase_draft: purchase drafts are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'prepared' THEN
            RAISE EXCEPTION 'brant_pur.purchase_draft: a draft is prepared first' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'prepared' AND NEW.status = 'voided' AND (to_jsonb(NEW) - v_void) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_void) THEN
        IF EXISTS (SELECT 1 FROM brant_pur.purchase_commitment c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
                AND c.draft_id = NEW.draft_id AND c.status IN ('issued', 'confirmed')) THEN
            RAISE EXCEPTION 'brant_pur.purchase_draft: draft % formed a purchase commitment and is never voided (PUR-INV-010)', OLD.draft_id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_pur.purchase_draft: draft % is never modified or replaced; it is only voided, once (PUR-INV-007)', OLD.draft_id
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;

-- DownstreamCommitments v1 (Context Map R71 PUR→TRD, guardLock false): what is open against an
-- order in PUR, one row per object, ordered by kind and object:
--
--   purchase_commitment  a commitment requested or approved for a draft not voided, or issued or
--                        confirmed (state: its status);
--   purchase_draft       a prepared draft without an issued commitment (state: prepared);
--   demand_allocation    an order line with an effective allocation (object_id and order_line_id:
--                        the line; quantity: the allocated quantity in the line's unit; state:
--                        allocated).
--
-- order_revision is the order revision the object binds; disposition is the approved disposition
-- of the object, NULL until an order-change review records one (S5-09). The function takes no locks:
-- TRD calls it with its own executor inside its own transaction after it locked the order row FOR
-- UPDATE, and every PUR command that adds a row first holds that order row FOR SHARE through
-- OrderGuard v1 (S5 decision D-1). It reads the caller's scope (app.tenant_id, app.company_id) and
-- applies no data scope.
CREATE FUNCTION brant_pur.order_commitments_v1(p_order_id text)
    RETURNS TABLE (kind text, object_id text, draft_id text, order_line_id text, order_revision bigint, state text, quantity text,
                   disposition text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
BEGIN
    IF p_order_id IS NULL THEN
        RAISE EXCEPTION 'brant_pur.order_commitments_v1: order_id is required' USING ERRCODE = 'null_value_not_allowed';
    END IF;
    RETURN QUERY
    SELECT x.kind, x.object_id, x.draft_id, x.order_line_id, x.order_revision, x.state, x.quantity, x.disposition FROM (
        SELECT 'purchase_commitment'::text AS kind, c.commitment_id AS object_id, c.draft_id, NULL::text AS order_line_id,
            c.order_revision, c.status AS state, NULL::text AS quantity, NULL::text AS disposition
        FROM brant_pur.purchase_commitment c
        JOIN brant_pur.purchase_draft d ON d.tenant_id = c.tenant_id AND d.company_id = c.company_id AND d.draft_id = c.draft_id
        WHERE c.tenant_id = v_tenant AND c.company_id = v_company AND c.order_id = p_order_id
            AND (c.status IN ('issued', 'confirmed') OR (c.status IN ('requested', 'approved') AND d.status = 'prepared'))
        UNION ALL
        SELECT 'purchase_draft'::text, d.draft_id, d.draft_id, NULL::text, d.order_revision, d.status, NULL::text, NULL::text
        FROM brant_pur.purchase_draft d
        WHERE d.tenant_id = v_tenant AND d.company_id = v_company AND d.order_id = p_order_id AND d.status = 'prepared'
            AND NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment c WHERE c.tenant_id = d.tenant_id AND c.company_id = d.company_id
                AND c.draft_id = d.draft_id AND c.status IN ('issued', 'confirmed'))
        UNION ALL
        SELECT 'demand_allocation'::text, a.order_line_id, NULL::text, a.order_line_id, a.order_revision, 'allocated'::text,
            pg_catalog.trim_scale(a.allocated)::text, NULL::text
        FROM brant_pur.demand_allocation a
        WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.order_id = p_order_id AND a.allocated > 0
    ) x
    ORDER BY x.kind, x.object_id;
END
$$;

REVOKE ALL ON FUNCTION brant_pur.order_commitments_v1(text) FROM PUBLIC;

ALTER TABLE brant_pur.purchase_commitment ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.purchase_commitment FORCE ROW LEVEL SECURITY;
CREATE POLICY purchase_commitment_scope ON brant_pur.purchase_commitment
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pur.supplier_confirmation ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.supplier_confirmation FORCE ROW LEVEL SECURITY;
CREATE POLICY supplier_confirmation_scope ON brant_pur.supplier_confirmation
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
