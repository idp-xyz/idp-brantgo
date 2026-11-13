-- PUR: purchase commitment changes and cancellation (WBS S5-19; tactical model PUR-AGG-PurchaseCommitment,
-- SM-PUR-COMMITMENT, SM-PUR-COMMITMENT-CHANGE, PUR-INV-011, PUR-INV-002, PUR-INV-008; Python V0.50 has the
-- design only: docs/T04_A46_PURCHASE_DRAFT_REPORT_V0.1.md, A47 #6 and #7).
--
-- An issued commitment is never rewritten (A47 #6). What it buys afterwards moves by revisions: revision 1
-- is the commitment as issued, its draft's lines; every later revision is a row of
-- purchase_commitment_revision with one line per draft line:
--
--   change  a commitment change (purchase_commitment_change) prepared by a purchaser, decided by a human
--           independent of its preparer and of the commitment's requester, and applied: the proposed
--           revision's document (the draft's request prepared again on the facts of its preparation,
--           with the quantities, sources and supplier dates it states) becomes the commitment's;
--           the commitment is issued again until the supplier confirms that revision;
--   cancel  the commitment cancelled with a reason and the supplier's evidence: each line keeps
--           exactly what was verified delivered of it (PUR-INV-008: verified deliveries are never
--           erased), the rest of its allocation is released (PUR-INV-011).
--
-- purchase_commitment.revision names the revision in force. The DemandAllocation entries of a
-- committed draft are those of its commitment's revision in force (allocation_entry.commitment_revision;
-- revision 1 entries are the draft's own): a change or a cancellation releases the entries of the
-- revision it replaces and takes those of the revision it makes, under the same root locks as a draft
-- (PUR-INV-002). A deferred check holds the effective entries, the revision in force and the verified
-- deliveries together at commit.
--
-- DownstreamCommitments v1 keeps its columns: a cancelled commitment and its draft are no longer open
-- (only the allocation kept for the verified deliveries shows), a commitment shows the order revision of
-- its revision in force, and an issued or confirmed commitment whose every line is verified delivered in
-- full is in state fulfilled (a short receipt is a change to the verified quantity).

-- The commitment: the revision in force and the cancellation.
ALTER TABLE brant_pur.purchase_commitment
    ADD COLUMN revision          bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    ADD COLUMN cancelled_by_kind text   CHECK (cancelled_by_kind = 'human'),
    ADD COLUMN cancelled_by_id   text,
    ADD COLUMN cancel_epoch      bigint CHECK (cancel_epoch > 0),
    ADD COLUMN cancelled_at      timestamptz,
    ADD COLUMN cancel_reason     text   CHECK (char_length(cancel_reason) BETWEEN 1 AND 1000),
    -- The supplier's evidence of the cancellation ([{"locator": ..., "source_id": ...}, ...]).
    ADD COLUMN cancel_evidence   text   CHECK (left(cancel_evidence, 2) = '[{');
ALTER TABLE brant_pur.purchase_commitment DROP CONSTRAINT purchase_commitment_status;
ALTER TABLE brant_pur.purchase_commitment ADD CONSTRAINT purchase_commitment_status
    CHECK (status IN ('requested', 'approved', 'rejected', 'revoked', 'issued', 'confirmed', 'cancelled'));
ALTER TABLE brant_pur.purchase_commitment ADD CONSTRAINT purchase_commitment_cancellation
    CHECK ((cancelled_at IS NULL) = (cancelled_by_id IS NULL) AND (cancelled_by_id IS NULL) = (cancelled_by_kind IS NULL)
        AND (cancelled_by_id IS NULL) = (cancel_epoch IS NULL) AND (cancelled_by_id IS NULL) = (cancel_reason IS NULL)
        AND (cancelled_by_id IS NULL) = (cancel_evidence IS NULL) AND (status = 'cancelled') = (cancelled_at IS NOT NULL)
        AND cancelled_at >= issued_at);
ALTER TABLE brant_pur.purchase_commitment ADD CONSTRAINT purchase_commitment_revised_after_issue
    CHECK (revision = 1 OR issued_at IS NOT NULL);

-- A commitment change: the full next revision of an issued (or confirmed) commitment, bound to the
-- revision it changes, with its reason and the EVD evidence (the supplier's), along
-- SM-PUR-COMMITMENT-CHANGE: draft → approved | rejected (decide) → applied (apply). Its request and its
-- document are stored as the canonical JSON bytes their SHA-256 cover.
CREATE TABLE brant_pur.purchase_commitment_change (
    tenant_id          text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id         text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    change_id          text        NOT NULL CHECK (change_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    commitment_id      text        NOT NULL,
    status             text        NOT NULL CONSTRAINT purchase_commitment_change_status
        CHECK (status IN ('draft', 'approved', 'rejected', 'applied')),
    row_version        bigint      NOT NULL CHECK (row_version BETWEEN 1 AND 3),
    base_revision      bigint      NOT NULL CHECK (base_revision > 0),
    draft_id           text        NOT NULL,
    order_id           text        NOT NULL,
    order_revision     bigint      NOT NULL CHECK (order_revision > 0),
    order_hash         text        NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    supplier_id        text        NOT NULL,
    currency           text        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    supply_valid_until timestamptz NOT NULL,
    line_count         integer     NOT NULL CHECK (line_count BETWEEN 1 AND 200),
    request            text        NOT NULL CHECK (left(request, 1) = '{'),
    request_hash       text        NOT NULL,
    document           text        NOT NULL CHECK (left(document, 1) = '{'),
    change_hash        text        NOT NULL,
    reason             text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    evidence           text        NOT NULL CHECK (left(evidence, 2) = '[{'),
    prepared_by_kind   text        NOT NULL CHECK (prepared_by_kind IN ('human', 'ai')),
    prepared_by_id     text        NOT NULL,
    prepare_epoch      bigint      NOT NULL CHECK (prepare_epoch > 0),
    prepared_at        timestamptz NOT NULL,
    decided_by_kind    text        CHECK (decided_by_kind = 'human'),
    decided_by_id      text,
    decide_epoch       bigint      CHECK (decide_epoch > 0),
    decided_at         timestamptz,
    decision           text        CHECK (decision IN ('approve', 'reject')),
    decision_reason    text        CHECK (char_length(decision_reason) BETWEEN 1 AND 1000),
    -- The deviations the approval acknowledged one by one ([{"kind": ..., "order_line_id": ..., "reason": ...}]).
    acknowledgements   text        CHECK (left(acknowledgements, 1) = '['),
    applied_by_kind    text        CHECK (applied_by_kind = 'human'),
    applied_by_id      text,
    apply_epoch        bigint      CHECK (apply_epoch > 0),
    applied_at         timestamptz,
    applied_revision   bigint,
    updated_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, change_id),
    FOREIGN KEY (tenant_id, company_id, commitment_id) REFERENCES brant_pur.purchase_commitment (tenant_id, company_id, commitment_id),
    CHECK (request_hash = encode(sha256(convert_to(request, 'UTF8')), 'hex')),
    CHECK (change_hash = encode(sha256(convert_to(document, 'UTF8')), 'hex')),
    CHECK ((decided_at IS NULL) = (decided_by_id IS NULL) AND (decided_by_id IS NULL) = (decided_by_kind IS NULL)
        AND (decided_by_id IS NULL) = (decide_epoch IS NULL) AND (decided_by_id IS NULL) = (decision IS NULL)),
    CHECK ((applied_at IS NULL) = (applied_by_id IS NULL) AND (applied_by_id IS NULL) = (applied_by_kind IS NULL)
        AND (applied_by_id IS NULL) = (apply_epoch IS NULL) AND (applied_by_id IS NULL) = (applied_revision IS NULL)),
    -- SM-PUR-COMMITMENT-CHANGE: what each state carries.
    CHECK (CASE status
        WHEN 'draft' THEN row_version = 1 AND decided_at IS NULL AND applied_at IS NULL
        WHEN 'rejected' THEN row_version = 2 AND decision = 'reject' AND decision_reason IS NOT NULL AND acknowledgements IS NULL
            AND applied_at IS NULL
        WHEN 'approved' THEN row_version = 2 AND decision = 'approve' AND acknowledgements IS NOT NULL AND applied_at IS NULL
        ELSE row_version = 3 AND decision = 'approve' AND acknowledgements IS NOT NULL AND applied_at IS NOT NULL END),
    -- PUR-INV-011: decided by a human other than its preparer (the commitment's requester is checked by
    -- the trigger), after it was prepared; applied after the decision as the next revision.
    CHECK (decided_by_id IS DISTINCT FROM prepared_by_id),
    CHECK (decided_at >= prepared_at),
    CHECK (applied_at >= decided_at),
    CHECK (applied_revision = base_revision + 1)
);

CREATE INDEX purchase_commitment_change_commitment ON brant_pur.purchase_commitment_change (tenant_id, company_id, commitment_id);

-- A line of a change: what the proposed revision buys of one line of the commitment's draft — the
-- allocated quantity, the overbuy, their sum, and the supplier's date stated for it (NULL: the supply
-- condition's). A line the revision no longer buys has zero quantities.
CREATE TABLE brant_pur.purchase_commitment_change_line (
    tenant_id          text          NOT NULL,
    company_id         text          NOT NULL,
    change_id          text          NOT NULL,
    order_line_id      text          NOT NULL,
    line_no            integer       NOT NULL CHECK (line_no BETWEEN 0 AND 199),
    allocated_quantity numeric(18,6) NOT NULL CHECK (allocated_quantity >= 0),
    overbuy_quantity   numeric(18,6) NOT NULL CHECK (overbuy_quantity >= 0),
    purchase_quantity  numeric(18,6) NOT NULL,
    supplier_date      date,
    PRIMARY KEY (tenant_id, company_id, change_id, order_line_id),
    UNIQUE (tenant_id, company_id, change_id, line_no),
    FOREIGN KEY (tenant_id, company_id, change_id) REFERENCES brant_pur.purchase_commitment_change (tenant_id, company_id, change_id),
    CHECK (purchase_quantity = allocated_quantity + overbuy_quantity)
);

-- A revision of a commitment after revision 1, made by applying a change or by the cancellation. It
-- binds the order at the revision and hash its document was prepared on (a cancellation keeps the
-- binding of the revision it ends) and, for a change, the change's document as it was sealed.
CREATE TABLE brant_pur.purchase_commitment_revision (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    commitment_id   text        NOT NULL,
    revision        bigint      NOT NULL CHECK (revision > 1),
    kind            text        NOT NULL CHECK (kind IN ('change', 'cancel')),
    change_id       text,
    order_revision  bigint      NOT NULL CHECK (order_revision > 0),
    order_hash      text        NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    document        text        CHECK (left(document, 1) = '{'),
    document_hash   text,
    line_count      integer     NOT NULL CHECK (line_count BETWEEN 1 AND 200),
    created_by_kind text        NOT NULL CHECK (created_by_kind = 'human'),
    created_by_id   text        NOT NULL,
    created_epoch   bigint      NOT NULL CHECK (created_epoch > 0),
    created_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, commitment_id, revision),
    FOREIGN KEY (tenant_id, company_id, commitment_id) REFERENCES brant_pur.purchase_commitment (tenant_id, company_id, commitment_id),
    FOREIGN KEY (tenant_id, company_id, change_id) REFERENCES brant_pur.purchase_commitment_change (tenant_id, company_id, change_id),
    CHECK ((kind = 'change') = (change_id IS NOT NULL) AND (kind = 'change') = (document IS NOT NULL)
        AND (document IS NULL) = (document_hash IS NULL)),
    CHECK (document_hash = encode(sha256(convert_to(document, 'UTF8')), 'hex'))
);

CREATE UNIQUE INDEX purchase_commitment_revision_change ON brant_pur.purchase_commitment_revision (tenant_id, company_id, change_id)
    WHERE change_id IS NOT NULL;

CREATE TABLE brant_pur.purchase_commitment_revision_line (
    tenant_id          text          NOT NULL,
    company_id         text          NOT NULL,
    commitment_id      text          NOT NULL,
    revision           bigint        NOT NULL,
    order_line_id      text          NOT NULL,
    allocated_quantity numeric(18,6) NOT NULL CHECK (allocated_quantity >= 0),
    overbuy_quantity   numeric(18,6) NOT NULL CHECK (overbuy_quantity >= 0),
    purchase_quantity  numeric(18,6) NOT NULL,
    supplier_date      date,
    PRIMARY KEY (tenant_id, company_id, commitment_id, revision, order_line_id),
    FOREIGN KEY (tenant_id, company_id, commitment_id, revision)
        REFERENCES brant_pur.purchase_commitment_revision (tenant_id, company_id, commitment_id, revision),
    CHECK (purchase_quantity = allocated_quantity + overbuy_quantity)
);

-- What a commitment buys of each line of its draft at the revision in force: the draft's line at
-- revision 1, the revision's line after.
CREATE FUNCTION brant_pur.commitment_lines(p_tenant text, p_company text, p_commitment_id text)
    RETURNS TABLE (order_line_id text, allocated numeric, overbuy numeric, purchased numeric)
    LANGUAGE sql STABLE SET search_path = pg_catalog, pg_temp
AS $$
    SELECT dl.order_line_id, coalesce(rl.allocated_quantity, dl.quantity), coalesce(rl.overbuy_quantity, dl.overbuy),
        coalesce(rl.purchase_quantity, dl.quantity + dl.overbuy)
    FROM brant_pur.purchase_commitment c
    JOIN brant_pur.purchase_draft_line dl ON dl.tenant_id = c.tenant_id AND dl.company_id = c.company_id AND dl.draft_id = c.draft_id
    LEFT JOIN brant_pur.purchase_commitment_revision_line rl ON rl.tenant_id = c.tenant_id AND rl.company_id = c.company_id
        AND rl.commitment_id = c.commitment_id AND rl.revision = c.revision AND rl.order_line_id = dl.order_line_id
    WHERE c.tenant_id = p_tenant AND c.company_id = p_company AND c.commitment_id = p_commitment_id
$$;

-- What the verified versions of a commitment's deliveries add up to for one of its lines.
CREATE FUNCTION brant_pur.commitment_line_verified(p_tenant text, p_company text, p_commitment_id text, p_order_line_id text)
    RETURNS numeric LANGUAGE sql STABLE SET search_path = pg_catalog, pg_temp
AS $$
    SELECT coalesce(sum(l.verified_quantity), 0) FROM brant_pur.supplier_delivery_line l
    JOIN brant_pur.supplier_delivery_version v ON v.tenant_id = l.tenant_id AND v.company_id = l.company_id AND v.delivery_id = l.delivery_id
        AND v.version = l.version
    WHERE l.tenant_id = p_tenant AND l.company_id = p_company AND l.commitment_id = p_commitment_id AND l.order_line_id = p_order_line_id
        AND v.status = 'verified'
$$;

REVOKE ALL ON FUNCTION brant_pur.commitment_lines(text, text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION brant_pur.commitment_line_verified(text, text, text, text) FROM PUBLIC;

-- A commitment is requested for a prepared draft whose commitment was never issued, and binds it
-- exactly; then it changes only along SM-PUR-COMMITMENT, its version moving by one, each step writing
-- only its own columns: the decision (by a human other than the draft's preparer), the revocation,
-- the issue (of a draft not voided), the supplier's confirmations of the revision in force (each
-- inserted first), the next revision a change applied or the cancellation made (each inserted
-- first) with the cancellation's columns. Nothing is deleted.
CREATE OR REPLACE FUNCTION brant_pur.guard_purchase_commitment() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_decision text[] := ARRAY['decided_by_kind', 'decided_by_id', 'decide_epoch', 'decided_at', 'decision', 'valid_until',
        'decision_reason', 'acknowledgements'];
    v_revoke   text[] := ARRAY['revoked_by_kind', 'revoked_by_id', 'revoke_epoch', 'revoked_at', 'revoke_reason'];
    v_issue    text[] := ARRAY['issued_by_kind', 'issued_by_id', 'issue_epoch', 'issued_at', 'po_number', 'sequence_id',
        'sequence_revision', 'sent_at', 'issue_evidence', 'issue_file_id', 'issue_file_sha256'];
    v_cancel   text[] := ARRAY['cancelled_by_kind', 'cancelled_by_id', 'cancel_epoch', 'cancelled_at', 'cancel_reason', 'cancel_evidence'];
    v_moving   text[] := ARRAY['status', 'row_version', 'updated_at', 'revision'];
    v_changed  text[];
    v_kind     text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment: commitments are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'requested' OR NEW.revision <> 1 OR NOT EXISTS (SELECT 1 FROM brant_pur.purchase_draft d WHERE d.tenant_id = NEW.tenant_id
                AND d.company_id = NEW.company_id AND d.draft_id = NEW.draft_id AND d.status = 'prepared' AND d.draft_hash = NEW.draft_hash
                AND d.order_id = NEW.order_id AND d.order_revision = NEW.order_revision AND d.order_hash = NEW.order_hash
                AND d.supplier_id = NEW.supplier_id AND d.currency = NEW.currency AND d.supply_valid_until = NEW.supply_valid_until)
            OR EXISTS (SELECT 1 FROM brant_pur.purchase_commitment c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
                AND c.draft_id = NEW.draft_id AND c.status = 'cancelled') THEN
            RAISE EXCEPTION 'brant_pur.purchase_commitment: a commitment is requested for a prepared draft never committed and binds it exactly'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.revision = OLD.revision + 1 THEN
        SELECT r.kind INTO v_kind FROM brant_pur.purchase_commitment_revision r WHERE r.tenant_id = NEW.tenant_id
            AND r.company_id = NEW.company_id AND r.commitment_id = NEW.commitment_id AND r.revision = NEW.revision;
    END IF;
    v_changed := CASE
        WHEN NEW.revision <> OLD.revision THEN CASE
            WHEN OLD.status IN ('issued', 'confirmed') AND NEW.status = 'issued' AND v_kind = 'change' THEN ARRAY[]::text[]
            WHEN OLD.status IN ('issued', 'confirmed') AND NEW.status = 'cancelled' AND v_kind = 'cancel' THEN v_cancel END
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
            OR (OLD.status = 'issued' AND NEW.revision = OLD.revision AND NOT EXISTS (SELECT 1 FROM brant_pur.supplier_confirmation s
                WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id AND s.commitment_id = NEW.commitment_id
                    AND s.commitment_version = NEW.row_version AND s.commitment_revision = NEW.revision
                    AND (s.result = 'as_issued') = (NEW.status = 'confirmed'))) THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment: commitment % (%) changes only along SM-PUR-COMMITMENT (PUR-INV-009, PUR-INV-010, PUR-INV-011)',
            OLD.commitment_id, OLD.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

-- A supplier confirmation answers the revision in force of an issued commitment.
ALTER TABLE brant_pur.supplier_confirmation ADD COLUMN commitment_revision bigint NOT NULL DEFAULT 1 CHECK (commitment_revision > 0);

CREATE OR REPLACE FUNCTION brant_pur.guard_supplier_confirmation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_pur.supplier_confirmation: confirmations never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
            AND c.commitment_id = NEW.commitment_id AND c.status = 'issued' AND NEW.commitment_version = c.row_version + 1
            AND NEW.commitment_revision = c.revision AND NEW.received_at >= c.sent_at) THEN
        RAISE EXCEPTION 'brant_pur.supplier_confirmation: a confirmation answers the revision in force of an issued commitment, received after it was sent'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

-- A change is prepared for an issued or confirmed commitment at its revision in force and binds its
-- draft, order, supplier and currency; then it changes only along SM-PUR-COMMITMENT-CHANGE, each step
-- writing its own columns: the decision (by a human other than its preparer and the commitment's
-- requester, of a change still bound to the revision in force) and the application (as the revision
-- it made, which the commitment is now at). Nothing is deleted.
CREATE FUNCTION brant_pur.guard_purchase_commitment_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_decision text[] := ARRAY['status', 'row_version', 'updated_at', 'decided_by_kind', 'decided_by_id', 'decide_epoch', 'decided_at',
        'decision', 'decision_reason', 'acknowledgements'];
    v_apply    text[] := ARRAY['status', 'row_version', 'updated_at', 'applied_by_kind', 'applied_by_id', 'apply_epoch', 'applied_at',
        'applied_revision'];
    v_changed  text[];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment_change: changes are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'draft' OR NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment c WHERE c.tenant_id = NEW.tenant_id
                AND c.company_id = NEW.company_id AND c.commitment_id = NEW.commitment_id AND c.status IN ('issued', 'confirmed')
                AND c.revision = NEW.base_revision AND c.draft_id = NEW.draft_id AND c.order_id = NEW.order_id
                AND c.supplier_id = NEW.supplier_id AND c.currency = NEW.currency) THEN
            RAISE EXCEPTION 'brant_pur.purchase_commitment_change: a change is prepared for the revision in force of an issued commitment'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    v_changed := CASE
        WHEN OLD.status = 'draft' AND NEW.status IN ('approved', 'rejected') THEN v_decision
        WHEN OLD.status = 'approved' AND NEW.status = 'applied' THEN v_apply
    END;
    IF v_changed IS NULL OR NEW.row_version <> OLD.row_version + 1
            OR (to_jsonb(NEW) - v_changed) IS DISTINCT FROM (to_jsonb(OLD) - v_changed)
            OR (NEW.status IN ('approved', 'rejected') AND EXISTS (SELECT 1 FROM brant_pur.purchase_commitment c
                WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id AND c.commitment_id = NEW.commitment_id
                    AND c.requested_by_id = NEW.decided_by_id))
            OR (NEW.status = 'approved' AND NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment c
                WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id AND c.commitment_id = NEW.commitment_id
                    AND c.status IN ('issued', 'confirmed') AND c.revision = NEW.base_revision))
            OR (NEW.status = 'applied' AND NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment c
                JOIN brant_pur.purchase_commitment_revision r ON r.tenant_id = c.tenant_id AND r.company_id = c.company_id
                    AND r.commitment_id = c.commitment_id AND r.revision = c.revision
                WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id AND c.commitment_id = NEW.commitment_id
                    AND c.revision = NEW.applied_revision AND r.change_id = NEW.change_id)) THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment_change: change % (%) changes only along SM-PUR-COMMITMENT-CHANGE (PUR-INV-011)',
            OLD.change_id, OLD.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER purchase_commitment_change_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pur.purchase_commitment_change
    FOR EACH ROW EXECUTE FUNCTION brant_pur.guard_purchase_commitment_change();

-- A change's lines are written with it, one per line of the commitment's draft, and never change.
CREATE FUNCTION brant_pur.guard_purchase_commitment_change_line() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment_change_line: change lines never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment_change x
            JOIN brant_pur.purchase_draft_line dl ON dl.tenant_id = x.tenant_id AND dl.company_id = x.company_id AND dl.draft_id = x.draft_id
            WHERE x.tenant_id = NEW.tenant_id AND x.company_id = NEW.company_id AND x.change_id = NEW.change_id AND x.status = 'draft'
                AND NEW.line_no < x.line_count AND dl.order_line_id = NEW.order_line_id) THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment_change_line: change % does not take line %', NEW.change_id, NEW.order_line_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER purchase_commitment_change_line_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pur.purchase_commitment_change_line
    FOR EACH ROW EXECUTE FUNCTION brant_pur.guard_purchase_commitment_change_line();

-- At commit a change has a line for every line of its draft, and buys something.
CREATE FUNCTION brant_pur.check_purchase_commitment_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment_change x
            JOIN brant_pur.purchase_draft d ON d.tenant_id = x.tenant_id AND d.company_id = x.company_id AND d.draft_id = x.draft_id
            WHERE x.tenant_id = NEW.tenant_id AND x.company_id = NEW.company_id AND x.change_id = NEW.change_id
                AND x.line_count = d.line_count
                AND x.line_count = (SELECT count(*) FROM brant_pur.purchase_commitment_change_line l WHERE l.tenant_id = x.tenant_id
                    AND l.company_id = x.company_id AND l.change_id = x.change_id)
                AND EXISTS (SELECT 1 FROM brant_pur.purchase_commitment_change_line l WHERE l.tenant_id = x.tenant_id
                    AND l.company_id = x.company_id AND l.change_id = x.change_id AND l.purchase_quantity > 0)) THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment_change: change % is not whole at commit', NEW.change_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER purchase_commitment_change_whole AFTER INSERT ON brant_pur.purchase_commitment_change
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_pur.check_purchase_commitment_change();

-- A revision is inserted as the next one of an issued or confirmed commitment: a change's when the
-- change is approved, bound to the revision in force and carries the same document; a cancellation's
-- with the binding of the revision it ends. It never changes and is never deleted.
CREATE FUNCTION brant_pur.guard_purchase_commitment_revision() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_c              record;
    v_order_revision bigint;
    v_order_hash     text;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment_revision: revisions never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT c.status, c.revision, c.order_revision, c.order_hash, d.line_count INTO v_c FROM brant_pur.purchase_commitment c
    JOIN brant_pur.purchase_draft d ON d.tenant_id = c.tenant_id AND d.company_id = c.company_id AND d.draft_id = c.draft_id
    WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id AND c.commitment_id = NEW.commitment_id;
    IF FOUND THEN
        v_order_revision := v_c.order_revision;
        v_order_hash := v_c.order_hash;
        SELECT r.order_revision, r.order_hash INTO v_order_revision, v_order_hash FROM brant_pur.purchase_commitment_revision r
        WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.commitment_id = NEW.commitment_id
            AND r.revision = v_c.revision;
        IF NOT FOUND THEN
            v_order_revision := v_c.order_revision;
            v_order_hash := v_c.order_hash;
        END IF;
    END IF;
    IF v_c IS NULL OR v_c.status NOT IN ('issued', 'confirmed') OR NEW.revision <> v_c.revision + 1 OR NEW.line_count <> v_c.line_count
            OR (NEW.kind = 'change' AND NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment_change x WHERE x.tenant_id = NEW.tenant_id
                AND x.company_id = NEW.company_id AND x.change_id = NEW.change_id AND x.commitment_id = NEW.commitment_id
                AND x.status = 'approved' AND x.base_revision = v_c.revision AND x.document = NEW.document
                AND x.order_revision = NEW.order_revision AND x.order_hash = NEW.order_hash))
            OR (NEW.kind = 'cancel' AND (NEW.order_revision <> v_order_revision OR NEW.order_hash <> v_order_hash)) THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment_revision: revision % of commitment % is not its next one', NEW.revision, NEW.commitment_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER purchase_commitment_revision_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pur.purchase_commitment_revision
    FOR EACH ROW EXECUTE FUNCTION brant_pur.guard_purchase_commitment_revision();

-- A revision's lines are written with it, one per line of the commitment's draft, a change's equal to
-- the change's lines; they never change.
CREATE FUNCTION brant_pur.guard_purchase_commitment_revision_line() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment_revision_line: revision lines never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment_revision r
            JOIN brant_pur.purchase_commitment c ON c.tenant_id = r.tenant_id AND c.company_id = r.company_id AND c.commitment_id = r.commitment_id
            JOIN brant_pur.purchase_draft_line dl ON dl.tenant_id = c.tenant_id AND dl.company_id = c.company_id AND dl.draft_id = c.draft_id
            WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.commitment_id = NEW.commitment_id
                AND r.revision = NEW.revision AND dl.order_line_id = NEW.order_line_id
                AND (r.kind = 'cancel' OR EXISTS (SELECT 1 FROM brant_pur.purchase_commitment_change_line x WHERE x.tenant_id = r.tenant_id
                    AND x.company_id = r.company_id AND x.change_id = r.change_id AND x.order_line_id = NEW.order_line_id
                    AND x.allocated_quantity = NEW.allocated_quantity AND x.overbuy_quantity = NEW.overbuy_quantity
                    AND x.supplier_date IS NOT DISTINCT FROM NEW.supplier_date))) THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment_revision_line: revision % of commitment % does not take line %', NEW.revision,
            NEW.commitment_id, NEW.order_line_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER purchase_commitment_revision_line_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pur.purchase_commitment_revision_line
    FOR EACH ROW EXECUTE FUNCTION brant_pur.guard_purchase_commitment_revision_line();

-- At commit a revision is whole and keeps what was verified delivered (PUR-INV-008, PUR-INV-011): it
-- has a line for every line of the draft, the commitment is at it, no line buys less than its
-- verified deliveries, and a cancellation keeps of each line exactly what was verified of it.
CREATE FUNCTION brant_pur.check_purchase_commitment_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
            AND c.commitment_id = NEW.commitment_id AND c.revision >= NEW.revision)
        OR NEW.line_count <> (SELECT count(*) FROM brant_pur.purchase_commitment_revision_line l WHERE l.tenant_id = NEW.tenant_id
            AND l.company_id = NEW.company_id AND l.commitment_id = NEW.commitment_id AND l.revision = NEW.revision)
        OR EXISTS (SELECT 1 FROM brant_pur.purchase_commitment_revision_line l WHERE l.tenant_id = NEW.tenant_id
            AND l.company_id = NEW.company_id AND l.commitment_id = NEW.commitment_id AND l.revision = NEW.revision
            AND (l.purchase_quantity < brant_pur.commitment_line_verified(l.tenant_id, l.company_id, l.commitment_id, l.order_line_id)
                OR (NEW.kind = 'cancel'
                    AND l.purchase_quantity <> brant_pur.commitment_line_verified(l.tenant_id, l.company_id, l.commitment_id, l.order_line_id)))) THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment_revision: revision % of commitment % does not keep its verified deliveries (PUR-INV-011)',
            NEW.revision, NEW.commitment_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER purchase_commitment_revision_whole AFTER INSERT ON brant_pur.purchase_commitment_revision
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_pur.check_purchase_commitment_revision();

-- Allocation entries by commitment revision (PUR-INV-002): a committed draft's effective entries are
-- those of its commitment's revision in force, one per line it allocates.
ALTER TABLE brant_pur.allocation_entry ADD COLUMN commitment_revision bigint NOT NULL DEFAULT 1 CHECK (commitment_revision > 0);
ALTER TABLE brant_pur.allocation_entry DROP CONSTRAINT allocation_entry_pkey;
ALTER TABLE brant_pur.allocation_entry ADD PRIMARY KEY (tenant_id, company_id, order_id, order_line_id, draft_id, commitment_revision);
CREATE UNIQUE INDEX allocation_entry_effective ON brant_pur.allocation_entry (tenant_id, company_id, order_id, order_line_id, draft_id)
    WHERE released_version IS NULL;

-- An entry is taken once and released once: at revision 1 the effective claim of its draft line, at
-- a later revision the allocated quantity of that revision's line; nothing else changes and nothing
-- is deleted.
CREATE OR REPLACE FUNCTION brant_pur.guard_allocation_entry() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pur.allocation_entry: allocation entries are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.released_version IS NOT NULL
                OR NOT EXISTS (SELECT 1 FROM brant_pur.purchase_draft_line l WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id
                    AND l.draft_id = NEW.draft_id AND l.order_line_id = NEW.order_line_id AND l.order_id = NEW.order_id
                    AND (NEW.commitment_revision > 1 OR l.quantity = NEW.quantity))
                OR (NEW.commitment_revision > 1 AND NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment_revision_line rl
                    JOIN brant_pur.purchase_commitment c ON c.tenant_id = rl.tenant_id AND c.company_id = rl.company_id
                        AND c.commitment_id = rl.commitment_id
                    WHERE rl.tenant_id = NEW.tenant_id AND rl.company_id = NEW.company_id AND c.draft_id = NEW.draft_id
                        AND rl.revision = NEW.commitment_revision AND rl.order_line_id = NEW.order_line_id
                        AND rl.allocated_quantity = NEW.quantity)) THEN
            RAISE EXCEPTION 'brant_pur.allocation_entry: an entry is the effective claim of its draft line at a commitment revision'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.released_version IS NULL AND NEW.released_version IS NOT NULL
            AND (to_jsonb(NEW) - ARRAY['released_version', 'released_at']) IS NOT DISTINCT FROM
                (to_jsonb(OLD) - ARRAY['released_version', 'released_at']) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_pur.allocation_entry: an entry is only released, once' USING ERRCODE = 'integrity_constraint_violation';
END
$$;

-- Whether a draft's claims are whole: exactly its line_count lines; while it is prepared, effective
-- entries exactly for the lines its revision in force allocates (revision 1: every line, at the
-- draft's quantities; a later revision of its issued, confirmed or cancelled commitment: the lines
-- that revision allocates something of), all of that revision; once voided, none effective.
CREATE FUNCTION brant_pur.draft_claims_whole(p_tenant text, p_company text, p_draft_id text) RETURNS boolean
    LANGUAGE plpgsql STABLE SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_draft     record;
    v_lines     bigint;
    v_revision  bigint;
    v_expected  bigint;
    v_effective bigint;
    v_other     bigint;
BEGIN
    SELECT d.status, d.line_count INTO v_draft FROM brant_pur.purchase_draft d
    WHERE d.tenant_id = p_tenant AND d.company_id = p_company AND d.draft_id = p_draft_id;
    IF NOT FOUND THEN
        RETURN false;
    END IF;
    SELECT count(*) INTO v_lines FROM brant_pur.purchase_draft_line l
    WHERE l.tenant_id = p_tenant AND l.company_id = p_company AND l.draft_id = p_draft_id;
    SELECT coalesce(max(c.revision), 1) INTO v_revision FROM brant_pur.purchase_commitment c
    WHERE c.tenant_id = p_tenant AND c.company_id = p_company AND c.draft_id = p_draft_id AND c.status IN ('issued', 'confirmed', 'cancelled');
    IF v_revision = 1 THEN
        v_expected := v_lines;
    ELSE
        SELECT count(*) INTO v_expected FROM brant_pur.purchase_commitment_revision_line rl
        JOIN brant_pur.purchase_commitment c ON c.tenant_id = rl.tenant_id AND c.company_id = rl.company_id AND c.commitment_id = rl.commitment_id
        WHERE rl.tenant_id = p_tenant AND rl.company_id = p_company AND c.draft_id = p_draft_id
            AND c.status IN ('issued', 'confirmed', 'cancelled') AND rl.revision = v_revision AND rl.allocated_quantity > 0;
    END IF;
    SELECT count(*) FILTER (WHERE e.released_version IS NULL),
        count(*) FILTER (WHERE e.released_version IS NULL AND e.commitment_revision <> v_revision)
        INTO v_effective, v_other FROM brant_pur.allocation_entry e
    WHERE e.tenant_id = p_tenant AND e.company_id = p_company AND e.draft_id = p_draft_id;
    RETURN v_lines = v_draft.line_count AND v_other = 0
        AND CASE WHEN v_draft.status = 'prepared' THEN v_effective = v_expected ELSE v_effective = 0 END;
END
$$;
REVOKE ALL ON FUNCTION brant_pur.draft_claims_whole(text, text, text) FROM PUBLIC;

CREATE OR REPLACE FUNCTION brant_pur.check_draft_claims() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT brant_pur.draft_claims_whole(NEW.tenant_id, NEW.company_id, NEW.draft_id) THEN
        RAISE EXCEPTION 'brant_pur.purchase_draft: the claims of draft % are not whole at commit', NEW.draft_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;

-- A new revision moves the claims of its draft too.
CREATE FUNCTION brant_pur.check_revision_claims() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_draft text;
BEGIN
    SELECT c.draft_id INTO v_draft FROM brant_pur.purchase_commitment c
    WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id AND c.commitment_id = NEW.commitment_id;
    IF NOT brant_pur.draft_claims_whole(NEW.tenant_id, NEW.company_id, v_draft) THEN
        RAISE EXCEPTION 'brant_pur.purchase_commitment_revision: the claims of draft % are not those of revision % at commit', v_draft,
            NEW.revision USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER purchase_commitment_revision_claims AFTER INSERT ON brant_pur.purchase_commitment_revision
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_pur.check_revision_claims();

-- PUR-INV-010 (TD-AT06): a draft whose commitment was issued — whether it is still issued, confirmed
-- or cancelled since — is committed and is never voided. The rest of the draft's guard is migration
-- 000001's.
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
                AND c.draft_id = NEW.draft_id AND c.status IN ('issued', 'confirmed', 'cancelled')) THEN
            RAISE EXCEPTION 'brant_pur.purchase_draft: draft % formed a purchase commitment and is never voided (PUR-INV-010)', OLD.draft_id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_pur.purchase_draft: draft % is never modified or replaced; it is only voided, once (PUR-INV-007)', OLD.draft_id
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;

-- PUR-INV-008 at commit, against what the commitment buys at its revision in force (migration
-- 000003's rule otherwise).
CREATE OR REPLACE FUNCTION brant_pur.check_supplier_delivery_within_commitment() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_line text;
BEGIN
    SELECT l.order_line_id INTO v_line
    FROM brant_pur.supplier_delivery_line l
    LEFT JOIN brant_pur.commitment_lines(l.tenant_id, l.company_id, l.commitment_id) q ON q.order_line_id = l.order_line_id
    WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id AND l.delivery_id = NEW.delivery_id AND l.version = NEW.version
        AND (q.purchased IS NULL
            OR brant_pur.commitment_line_verified(l.tenant_id, l.company_id, l.commitment_id, l.order_line_id) > q.purchased)
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'brant_pur.supplier_delivery_line: the verified deliveries of commitment % line % exceed what it buys (PUR-INV-008)',
            NEW.commitment_id, v_line USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;

-- DownstreamCommitments v1 (Context Map R71), migration 000002's columns and contract, after changes and
-- cancellations: a commitment requested or approved for a draft not voided, or issued or confirmed —
-- state fulfilled when every line is verified delivered in full at its revision in force —, with the
-- order revision of that revision; a prepared draft whose commitment was never issued (a cancelled
-- commitment's draft is not open: what was verified of it keeps its allocation); an order line with an
-- effective allocation. It takes no locks: every PUR command that adds to it (a draft's preparation, a
-- commitment's request, approval and issue, a change's application) holds the order row FOR SHARE
-- through OrderGuard v1 (S5 decision D-1).
CREATE OR REPLACE FUNCTION brant_pur.order_commitments_v1(p_order_id text)
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
            coalesce(r.order_revision, c.order_revision) AS order_revision,
            CASE WHEN c.status IN ('issued', 'confirmed') AND NOT EXISTS (SELECT 1 FROM brant_pur.commitment_lines(c.tenant_id, c.company_id,
                    c.commitment_id) q WHERE brant_pur.commitment_line_verified(c.tenant_id, c.company_id, c.commitment_id, q.order_line_id)
                    < q.purchased)
                THEN 'fulfilled' ELSE c.status END AS state,
            NULL::text AS quantity, NULL::text AS disposition
        FROM brant_pur.purchase_commitment c
        JOIN brant_pur.purchase_draft d ON d.tenant_id = c.tenant_id AND d.company_id = c.company_id AND d.draft_id = c.draft_id
        LEFT JOIN brant_pur.purchase_commitment_revision r ON r.tenant_id = c.tenant_id AND r.company_id = c.company_id
            AND r.commitment_id = c.commitment_id AND r.revision = c.revision
        WHERE c.tenant_id = v_tenant AND c.company_id = v_company AND c.order_id = p_order_id
            AND (c.status IN ('issued', 'confirmed') OR (c.status IN ('requested', 'approved') AND d.status = 'prepared'))
        UNION ALL
        SELECT 'purchase_draft'::text, d.draft_id, d.draft_id, NULL::text, d.order_revision, d.status, NULL::text, NULL::text
        FROM brant_pur.purchase_draft d
        WHERE d.tenant_id = v_tenant AND d.company_id = v_company AND d.order_id = p_order_id AND d.status = 'prepared'
            AND NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment c WHERE c.tenant_id = d.tenant_id AND c.company_id = d.company_id
                AND c.draft_id = d.draft_id AND c.status IN ('issued', 'confirmed', 'cancelled'))
        UNION ALL
        SELECT 'demand_allocation'::text, a.order_line_id, NULL::text, a.order_line_id, a.order_revision, 'allocated'::text,
            pg_catalog.trim_scale(a.allocated)::text, NULL::text
        FROM brant_pur.demand_allocation a
        WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.order_id = p_order_id AND a.allocated > 0
    ) x
    ORDER BY x.kind, x.object_id;
END
$$;

ALTER TABLE brant_pur.purchase_commitment_change ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.purchase_commitment_change FORCE ROW LEVEL SECURITY;
CREATE POLICY purchase_commitment_change_scope ON brant_pur.purchase_commitment_change
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pur.purchase_commitment_change_line ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.purchase_commitment_change_line FORCE ROW LEVEL SECURITY;
CREATE POLICY purchase_commitment_change_line_scope ON brant_pur.purchase_commitment_change_line
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pur.purchase_commitment_revision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.purchase_commitment_revision FORCE ROW LEVEL SECURITY;
CREATE POLICY purchase_commitment_revision_scope ON brant_pur.purchase_commitment_revision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pur.purchase_commitment_revision_line ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.purchase_commitment_revision_line FORCE ROW LEVEL SECURITY;
CREATE POLICY purchase_commitment_revision_line_scope ON brant_pur.purchase_commitment_revision_line
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
