-- CNX: opening refund material and the impact of an STL credit note (tactical model CNX-AGG-TaxRefundCase value
-- object OpeningPosition, SM-CNX-TAXCASE, CNX-INV-004; CNX-AGG-DocumentBundle ExternalReconciliation; Context Map
-- MD-51, R38; baseline 11.2 期初、防重、切换, CN-R04, CN-R08, CN-R09, CN-E01; CN-AT16 CNX half, BT17, CN-AT11 CNX
-- half; WBS S5-20; S5 plan D-3). Python V0.50 has no CNX.
--
-- tax_refund_case.record_opening (human only, tax specialist) registers the refund material the books held at a
-- cutoff instant, from a source document with EVD evidence, as one item:
--
--   invoice_balance  a verified blue purchase invoice in CNX with, per line and purpose, the quantity refund
--                    declarations finished before the cutoff consumed. That consumption is an external result
--                    already reached: it counts for ever (CN-R09 "不仅凭本地取消状态恢复用途额度"; MD-51 forbids
--                    restoring declared usage from a local estimate), next to the allocations of the cases.
--   refund_case      a refund case of an export declared before the cutoff (not a FUL export lot: its reference
--                    is the customs declaration number and its export date the 出口日期 the tax specialist states
--                    from the declaration's evidence), opened directly in the state its declaration had at the
--                    cutoff (expected, planned, submitted or approved) with that stage's facts, the export_tax_policy
--                    revision in force at its export date, and the invoice lines its declaration uses as ordinary
--                    allocations: they count until an external rejection of the case releases them, like any case's.
--
-- An item is recorded once per kind and source document (OPENING_BALANCE_DUPLICATE), an invoice's opening balance
-- once, and an export's case only while its earlier attempts were rejected: no usage is created twice. Every
-- balance stays derived from immutable entries (D-3): per purpose, Σ counting allocations and Σ opening
-- consumption of an invoice line never exceed its quantity (the triggers lock the invoice FOR UPDATE).
--
-- external_reconciliation.resolve gains the subject credit_note: an STL credit note (CreditNoteRef v1, R38 STL→CNX)
-- confirmed on a sales statement whose shipment lines are lines of the bundle's export lot; the reconciliation
-- keeps the credit note's version and what STL stated of it. It records the impact only: no declaration, refund
-- case or allocation changes (CN-AT11 "贷项不自动完成退税调整", CN-E01).
--
-- References to STL, FUL and EVD are plain IDs: no foreign key crosses a schema (DB2). Nothing is deleted.

-- Opening items ----------------------------------------------------------------------------------------------------

ALTER TABLE brant_cnx.tax_refund_case ADD COLUMN opening boolean NOT NULL DEFAULT false;

CREATE TABLE brant_cnx.tax_opening (
    tenant_id           text        NOT NULL,
    company_id          text        NOT NULL,
    opening_id          text        NOT NULL CHECK (opening_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    kind                text        NOT NULL CHECK (kind IN ('invoice_balance', 'refund_case')),
    cutoff_at           timestamptz NOT NULL,
    source_document     text        NOT NULL CHECK (source_document ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,63}$'),
    purchase_invoice_id text,
    case_id             text,
    export_reference    text        CHECK (export_reference ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    exported_at         timestamptz,
    order_reference     text        CHECK (order_reference ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,63}$'),
    position            text        CHECK (position IN ('expected', 'planned', 'submitted', 'approved')),
    evidence            text        NOT NULL CHECK (left(evidence, 2) = '[{'),
    recorded_by_kind    text        NOT NULL CHECK (recorded_by_kind = 'human'),
    recorded_by_id      text        NOT NULL,
    authority_epoch     bigint      NOT NULL CHECK (authority_epoch > 0),
    recorded_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, opening_id),
    FOREIGN KEY (tenant_id, company_id, purchase_invoice_id) REFERENCES brant_cnx.purchase_invoice (tenant_id, company_id, purchase_invoice_id),
    FOREIGN KEY (tenant_id, company_id, case_id) REFERENCES brant_cnx.tax_refund_case (tenant_id, company_id, case_id),
    CHECK ((kind = 'invoice_balance') = (purchase_invoice_id IS NOT NULL) AND (kind = 'refund_case') = (case_id IS NOT NULL)
        AND (case_id IS NULL) = (export_reference IS NULL) AND (case_id IS NULL) = (exported_at IS NULL) AND (case_id IS NULL) = (position IS NULL)
        AND (case_id IS NOT NULL OR order_reference IS NULL)),
    CHECK (cutoff_at <= recorded_at AND (exported_at IS NULL OR exported_at <= cutoff_at))
);

-- MD-51 "不重复生成用途占用": a source document stands once per kind whatever the cutoff, an invoice has one opening
-- balance, a case one opening.
CREATE UNIQUE INDEX tax_opening_source ON brant_cnx.tax_opening (tenant_id, company_id, kind, source_document);
CREATE UNIQUE INDEX tax_opening_invoice ON brant_cnx.tax_opening (tenant_id, company_id, purchase_invoice_id) WHERE purchase_invoice_id IS NOT NULL;
CREATE UNIQUE INDEX tax_opening_case ON brant_cnx.tax_opening (tenant_id, company_id, case_id) WHERE case_id IS NOT NULL;
CREATE INDEX tax_opening_cutoff ON brant_cnx.tax_opening (tenant_id, company_id, cutoff_at);

-- An opening item is only inserted: an invoice balance of a verified blue invoice; a refund case with the
-- opening case its command opened at the same instant, of its export reference and date, in the stated position
-- at version 1. It never changes and is never deleted.
CREATE FUNCTION brant_cnx.guard_tax_opening() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cnx.tax_opening: opening items never change and are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.kind = 'invoice_balance' AND NOT EXISTS (SELECT 1 FROM brant_cnx.purchase_invoice i WHERE i.tenant_id = NEW.tenant_id
            AND i.company_id = NEW.company_id AND i.purchase_invoice_id = NEW.purchase_invoice_id AND i.status = 'verified' AND i.colour = 'blue') THEN
        RAISE EXCEPTION 'brant_cnx.tax_opening: invoice % is not a verified blue invoice', NEW.purchase_invoice_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.kind = 'refund_case' AND NOT EXISTS (SELECT 1 FROM brant_cnx.tax_refund_case c WHERE c.tenant_id = NEW.tenant_id
            AND c.company_id = NEW.company_id AND c.case_id = NEW.case_id AND c.opening AND c.row_version = 1 AND c.status = NEW.position
            AND c.export_lot_id = NEW.export_reference AND c.exported_at = NEW.exported_at AND c.opened_by_id = NEW.recorded_by_id
            AND c.opened_at = NEW.recorded_at) THEN
        RAISE EXCEPTION 'brant_cnx.tax_opening: % names no opening case its command opened', NEW.opening_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER tax_opening_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.tax_opening
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_tax_opening();

-- What declarations finished before the cutoff consumed of an invoice line for a purpose (the invoice_balance
-- item's lines); a line and purpose once per item.
CREATE TABLE brant_cnx.tax_evidence_opening (
    tenant_id           text    NOT NULL,
    company_id          text    NOT NULL,
    opening_id          text    NOT NULL,
    purchase_invoice_id text    NOT NULL,
    line_no             integer NOT NULL,
    purpose             text    NOT NULL CHECK (purpose IN ('export_refund', 'export_exemption')),
    consumed            numeric NOT NULL CHECK (consumed >= 0),
    unit                text    NOT NULL,
    PRIMARY KEY (tenant_id, company_id, opening_id, line_no, purpose),
    FOREIGN KEY (tenant_id, company_id, opening_id) REFERENCES brant_cnx.tax_opening (tenant_id, company_id, opening_id),
    FOREIGN KEY (tenant_id, company_id, purchase_invoice_id, line_no)
        REFERENCES brant_cnx.purchase_invoice_line (tenant_id, company_id, purchase_invoice_id, line_no)
);

CREATE INDEX tax_evidence_opening_line ON brant_cnx.tax_evidence_opening (tenant_id, company_id, purchase_invoice_id, line_no);

-- An opening consumption is only inserted, under its invoice's FOR UPDATE lock, for its item's invoice, in the
-- invoice line's unit, and never takes Σ counting allocations and opening consumption of the line, per purpose,
-- above its quantity (CNX-INV-004).
CREATE FUNCTION brant_cnx.guard_tax_evidence_opening() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_line numeric;
    v_used numeric;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cnx.tax_evidence_opening: opening consumption never changes and is never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    PERFORM 1 FROM brant_cnx.purchase_invoice i WHERE i.tenant_id = NEW.tenant_id AND i.company_id = NEW.company_id
        AND i.purchase_invoice_id = NEW.purchase_invoice_id AND i.status = 'verified' AND i.colour = 'blue' FOR UPDATE;
    IF NOT FOUND OR NOT EXISTS (SELECT 1 FROM brant_cnx.tax_opening o WHERE o.tenant_id = NEW.tenant_id AND o.company_id = NEW.company_id
            AND o.opening_id = NEW.opening_id AND o.kind = 'invoice_balance' AND o.purchase_invoice_id = NEW.purchase_invoice_id) THEN
        RAISE EXCEPTION 'brant_cnx.tax_evidence_opening: % is no opening balance of verified blue invoice %', NEW.opening_id, NEW.purchase_invoice_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT l.quantity INTO v_line FROM brant_cnx.purchase_invoice_line l WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id
        AND l.purchase_invoice_id = NEW.purchase_invoice_id AND l.line_no = NEW.line_no AND l.unit = NEW.unit;
    SELECT coalesce(sum(a.quantity), 0) INTO v_used FROM brant_cnx.tax_evidence_allocation a WHERE a.tenant_id = NEW.tenant_id
        AND a.company_id = NEW.company_id AND a.purchase_invoice_id = NEW.purchase_invoice_id AND a.line_no = NEW.line_no
        AND a.purpose = NEW.purpose AND brant_cnx.tax_evidence_counts(a.tenant_id, a.company_id, a.allocation_id, a.case_id);
    v_used := v_used + (SELECT coalesce(sum(o.consumed), 0) FROM brant_cnx.tax_evidence_opening o WHERE o.tenant_id = NEW.tenant_id
        AND o.company_id = NEW.company_id AND o.purchase_invoice_id = NEW.purchase_invoice_id AND o.line_no = NEW.line_no AND o.purpose = NEW.purpose);
    IF v_line IS NULL OR v_used + NEW.consumed > v_line THEN
        RAISE EXCEPTION 'brant_cnx.tax_evidence_opening: invoice % line % would be exceeded (CNX-INV-004)', NEW.purchase_invoice_id, NEW.line_no
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER tax_evidence_opening_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.tax_evidence_opening
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_tax_evidence_opening();

-- 000003's case guard, with opening cases: an opening case opens at version 1 in the state its declaration had at
-- the cutoff (expected, planned, submitted or approved; SM-CNX-TAXCASE from there on); every other case opens
-- expected. Nothing else changes.
CREATE OR REPLACE FUNCTION brant_cnx.guard_tax_refund_case() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cnx.tax_refund_case: cases are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NOT (NEW.status = 'expected' OR (NEW.opening AND NEW.status IN ('planned', 'submitted', 'approved')))
                OR NEW.row_version <> 1 OR NEW.updated_at <> NEW.opened_at
                OR NEW.attempt <> 1 + (SELECT count(*) FROM brant_cnx.tax_refund_case c WHERE c.tenant_id = NEW.tenant_id
                    AND c.company_id = NEW.company_id AND c.export_lot_id = NEW.export_lot_id)
                OR EXISTS (SELECT 1 FROM brant_cnx.tax_refund_case c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
                    AND c.export_lot_id = NEW.export_lot_id AND c.status <> 'rejected') THEN
            RAISE EXCEPTION 'brant_cnx.tax_refund_case: a case opens expected (an opening case in its position) at version 1 as its lot''s next attempt'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - ARRAY['status', 'row_version', 'updated_at']) IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['status', 'row_version', 'updated_at'])
            OR NEW.row_version <> OLD.row_version + 1 OR NEW.updated_at < OLD.updated_at
            OR NOT (NEW.status = OLD.status AND OLD.status IN ('expected', 'planned')
                OR (OLD.status, NEW.status) IN (('expected', 'planned'), ('planned', 'submitted'), ('submitted', 'approved'),
                    ('submitted', 'rejected'), ('approved', 'received'))) THEN
        RAISE EXCEPTION 'brant_cnx.tax_refund_case: case % keeps its binding and follows SM-CNX-TAXCASE', OLD.case_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

-- 000003's allocation guard, with opening material: an opening case takes its allocations when it is opened (in
-- any position: what its declaration used at the cutoff), every other case while expected or planned; and the
-- invoice line's balance counts the opening consumption of the line as well. Nothing else changes.
CREATE OR REPLACE FUNCTION brant_cnx.guard_tax_evidence_allocation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_line numeric;
    v_used numeric;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cnx.tax_evidence_allocation: allocations never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    PERFORM 1 FROM brant_cnx.tax_refund_case c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
        AND c.case_id = NEW.case_id AND (c.status IN ('expected', 'planned') OR (c.opening AND c.row_version = 1 AND c.opened_at = NEW.allocated_at))
        FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'brant_cnx.tax_evidence_allocation: case % takes no allocation', NEW.case_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    PERFORM 1 FROM brant_cnx.purchase_invoice i WHERE i.tenant_id = NEW.tenant_id AND i.company_id = NEW.company_id
        AND i.purchase_invoice_id = NEW.purchase_invoice_id AND i.status = 'verified' AND i.colour = 'blue' FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'brant_cnx.tax_evidence_allocation: invoice % is not a verified blue invoice', NEW.purchase_invoice_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.replaces IS NOT NULL AND NOT EXISTS (SELECT 1 FROM brant_cnx.tax_evidence_allocation r
            JOIN brant_cnx.purchase_invoice i ON i.tenant_id = r.tenant_id AND i.company_id = r.company_id
                AND i.purchase_invoice_id = r.purchase_invoice_id AND i.status = 'reversed'
            WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.allocation_id = NEW.replaces
                AND r.case_id = NEW.case_id AND r.export_line_id = NEW.export_line_id) THEN
        RAISE EXCEPTION 'brant_cnx.tax_evidence_allocation: % replaces no allocation under review', NEW.allocation_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT l.quantity INTO v_line FROM brant_cnx.purchase_invoice_line l WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id
        AND l.purchase_invoice_id = NEW.purchase_invoice_id AND l.line_no = NEW.line_no AND l.unit = NEW.unit;
    SELECT coalesce(sum(a.quantity), 0) INTO v_used FROM brant_cnx.tax_evidence_allocation a WHERE a.tenant_id = NEW.tenant_id
        AND a.company_id = NEW.company_id AND a.purchase_invoice_id = NEW.purchase_invoice_id AND a.line_no = NEW.line_no
        AND a.purpose = NEW.purpose AND brant_cnx.tax_evidence_counts(a.tenant_id, a.company_id, a.allocation_id, a.case_id);
    v_used := v_used + (SELECT coalesce(sum(o.consumed), 0) FROM brant_cnx.tax_evidence_opening o WHERE o.tenant_id = NEW.tenant_id
        AND o.company_id = NEW.company_id AND o.purchase_invoice_id = NEW.purchase_invoice_id AND o.line_no = NEW.line_no AND o.purpose = NEW.purpose);
    IF v_line IS NULL OR v_used + NEW.quantity > v_line THEN
        RAISE EXCEPTION 'brant_cnx.tax_evidence_allocation: invoice % line % would be exceeded (CNX-INV-004)', NEW.purchase_invoice_id, NEW.line_no
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT coalesce(sum(a.quantity), 0) INTO v_used FROM brant_cnx.tax_evidence_allocation a WHERE a.tenant_id = NEW.tenant_id
        AND a.company_id = NEW.company_id AND a.case_id = NEW.case_id AND a.export_line_id = NEW.export_line_id AND a.purpose = NEW.purpose
        AND a.allocation_id IS DISTINCT FROM NEW.replaces
        AND brant_cnx.tax_evidence_counts(a.tenant_id, a.company_id, a.allocation_id, a.case_id);
    IF v_used + NEW.quantity > NEW.export_quantity THEN
        RAISE EXCEPTION 'brant_cnx.tax_evidence_allocation: export line % would be exceeded (CNX-INV-004)', NEW.export_line_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

-- The impact of a credit note -------------------------------------------------------------------------------------

ALTER TABLE brant_cnx.external_reconciliation DROP CONSTRAINT external_reconciliation_subject_kind_check;
ALTER TABLE brant_cnx.external_reconciliation ADD CONSTRAINT external_reconciliation_subject_kind_check
    CHECK (subject_kind IN ('export_lot', 'declaration', 'tax_refund_case', 'fund_record', 'document', 'credit_note'));
ALTER TABLE brant_cnx.external_reconciliation ADD COLUMN subject_version bigint CHECK (subject_version > 0),
    ADD COLUMN subject_detail text CHECK (left(subject_detail, 1) = '{');
-- A credit note is reconciled as an amount, with the version STL showed and what STL stated of it.
ALTER TABLE brant_cnx.external_reconciliation ADD CONSTRAINT external_reconciliation_credit_note
    CHECK ((subject_kind = 'credit_note') = (subject_version IS NOT NULL) AND (subject_version IS NULL) = (subject_detail IS NULL)
        AND (subject_kind <> 'credit_note' OR measure = 'amount'));

ALTER TABLE brant_cnx.tax_opening ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.tax_opening FORCE ROW LEVEL SECURITY;
CREATE POLICY tax_opening_scope ON brant_cnx.tax_opening
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.tax_evidence_opening ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.tax_evidence_opening FORCE ROW LEVEL SECURITY;
CREATE POLICY tax_evidence_opening_scope ON brant_cnx.tax_evidence_opening
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
