-- CNX: purchase invoices and tax refund cases (tactical model CNX-AGG-PurchaseInvoice with its
-- InvoiceLine and InvoiceMatch entities, SM-CNX-INVOICE, CNX-INV-005; CNX-AGG-TaxRefundCase with its
-- TaxEvidenceAllocation entries, SM-CNX-TAXCASE, CNX-INV-004; baseline 22.1, 22.2, CN-R04, CN-R06,
-- CN-R08, CN-R09; WBS S5-05b; S5 decisions E-1, D-3, D-17). Python V0.50 has no CNX.
--
-- A purchase invoice is a root: imported with its original (an EVD file) and the candidate read
-- from it, then decided once by a human (verified with the verified facts, or rejected), and a
-- verified blue invoice is reversed when a red invoice naming it is verified. The candidate, the
-- file and the import never change (trigger); (kind, number) is unique among invoices that are not
-- rejected. Its verified lines and its matches to PUR commitment lines are inserted with the
-- verification and never change. A match of a reversed invoice is under review: derived, nothing
-- is deleted (CN-R06).
--
-- A tax refund case is the root of one FUL export lot's refund material: it binds the
-- export_tax_policy revision in force at the lot's export date and moves through SM-CNX-TAXCASE
-- (trigger). Allocations and results are immutable entries. The allocation trigger locks the
-- invoice FOR UPDATE, as the command does, and refuses an allocation of an invoice that is not a
-- verified blue one or that would take, per purpose, Σ counting allocations of an invoice line above
-- its quantity or of an export line within its case above its quantity (CNX-INV-004). An
-- allocation counts unless a later one replaced it or its case was rejected (an external result):
-- no local cancellation exists (CN-R09).
--
-- References to FUL, PUR, STL, POL and EVD are plain IDs and hashes: no foreign key crosses a
-- schema (DB2).

CREATE TABLE brant_cnx.purchase_invoice (
    tenant_id              text        NOT NULL,
    company_id             text        NOT NULL,
    purchase_invoice_id    text        NOT NULL CHECK (purchase_invoice_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    invoice_kind           text        NOT NULL CHECK (invoice_kind IN ('special_vat', 'ordinary_vat', 'digital_special', 'digital_ordinary')),
    invoice_number         text        NOT NULL CHECK (invoice_number ~ '^[A-Za-z0-9][A-Za-z0-9-]{0,63}$'),
    colour                 text        NOT NULL CHECK (colour IN ('blue', 'red')),
    status                 text        NOT NULL CHECK (status IN ('imported', 'verified', 'rejected', 'reversed')),
    row_version            bigint      NOT NULL CHECK (row_version > 0),
    file_id                text        NOT NULL,
    file_sha256            text        NOT NULL CHECK (file_sha256 ~ '^[0-9a-f]{64}$'),
    candidate              text        NOT NULL CHECK (left(candidate, 1) = '{'),
    imported_by_kind       text        NOT NULL CHECK (imported_by_kind IN ('human', 'ai')),
    imported_by_id         text        NOT NULL,
    import_epoch           bigint      NOT NULL CHECK (import_epoch > 0),
    imported_at            timestamptz NOT NULL,
    verified               text        CHECK (left(verified, 1) = '{'),
    decision               text        CHECK (decision IN ('verify', 'reject')),
    decision_reason        text        CHECK (char_length(decision_reason) BETWEEN 1 AND 1000),
    decided_by_kind        text        CHECK (decided_by_kind = 'human'),
    decided_by_id          text,
    decide_epoch           bigint      CHECK (decide_epoch > 0),
    decided_at             timestamptz,
    red_of_invoice_id      text,
    reversed_by_invoice_id text,
    updated_at             timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, purchase_invoice_id),
    CHECK ((status = 'imported') = (decision IS NULL)),
    CHECK ((decision IS NULL) = (decided_by_id IS NULL) AND (decision IS NULL) = (decided_at IS NULL)),
    CHECK ((decision = 'verify') = (verified IS NOT NULL)),
    CHECK ((status = 'reversed') = (reversed_by_invoice_id IS NOT NULL)),
    CHECK (red_of_invoice_id IS NULL OR (colour = 'red' AND decision = 'verify')),
    CHECK (updated_at >= imported_at)
);

CREATE UNIQUE INDEX purchase_invoice_key ON brant_cnx.purchase_invoice (tenant_id, company_id, invoice_kind, invoice_number)
    WHERE status <> 'rejected';
CREATE UNIQUE INDEX purchase_invoice_red_of ON brant_cnx.purchase_invoice (tenant_id, company_id, red_of_invoice_id)
    WHERE red_of_invoice_id IS NOT NULL;

-- An invoice is imported at version 1; it is decided once (imported -> verified | rejected) and a
-- verified blue invoice is reversed once (verified -> reversed); each moves the version by one. Its
-- import never changes and nothing is deleted.
CREATE FUNCTION brant_cnx.guard_purchase_invoice() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_fixed text[] := ARRAY['tenant_id', 'company_id', 'purchase_invoice_id', 'file_id', 'file_sha256', 'candidate', 'imported_by_kind',
        'imported_by_id', 'import_epoch', 'imported_at'];
    v_decided text[] := ARRAY['invoice_kind', 'invoice_number', 'colour', 'verified', 'decision', 'decision_reason', 'decided_by_kind',
        'decided_by_id', 'decide_epoch', 'decided_at', 'red_of_invoice_id'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cnx.purchase_invoice: invoices are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'imported' OR NEW.row_version <> 1 OR NEW.updated_at <> NEW.imported_at THEN
            RAISE EXCEPTION 'brant_cnx.purchase_invoice: an invoice is imported at version 1' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (SELECT bool_or(to_jsonb(NEW) -> f IS DISTINCT FROM to_jsonb(OLD) -> f) FROM unnest(v_fixed) f)
            OR NEW.row_version <> OLD.row_version + 1 OR NEW.updated_at < OLD.updated_at
            OR NOT ((OLD.status = 'imported' AND NEW.status IN ('verified', 'rejected'))
                OR (OLD.status = 'verified' AND NEW.status = 'reversed' AND OLD.colour = 'blue'))
            OR (OLD.status <> 'imported' AND (SELECT bool_or(to_jsonb(NEW) -> f IS DISTINCT FROM to_jsonb(OLD) -> f) FROM unnest(v_decided) f)) THEN
        RAISE EXCEPTION 'brant_cnx.purchase_invoice: invoice % keeps its import, is decided once and reversed once', OLD.purchase_invoice_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER purchase_invoice_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.purchase_invoice
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_purchase_invoice();

CREATE TABLE brant_cnx.purchase_invoice_line (
    tenant_id           text    NOT NULL,
    company_id          text    NOT NULL,
    purchase_invoice_id text    NOT NULL,
    line_no             integer NOT NULL CHECK (line_no BETWEEN 1 AND 100),
    unit                text    NOT NULL CHECK (unit ~ '^[A-Z0-9-]{1,32}$'),
    quantity            numeric NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (tenant_id, company_id, purchase_invoice_id, line_no),
    FOREIGN KEY (tenant_id, company_id, purchase_invoice_id) REFERENCES brant_cnx.purchase_invoice (tenant_id, company_id, purchase_invoice_id)
);

CREATE TABLE brant_cnx.purchase_invoice_match (
    tenant_id           text        NOT NULL,
    company_id          text        NOT NULL,
    purchase_invoice_id text        NOT NULL,
    line_no             integer     NOT NULL,
    commitment_id       text        NOT NULL CHECK (commitment_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_line_id       text        NOT NULL CHECK (order_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    quantity            numeric     NOT NULL CHECK (quantity > 0),
    unit                text        NOT NULL,
    supplier_id         text        NOT NULL,
    po_number           text        NOT NULL,
    purchase_quantity   numeric     NOT NULL CHECK (purchase_quantity > 0),
    delivered_quantity  numeric     NOT NULL CHECK (delivered_quantity >= 0),
    matched_at          timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, purchase_invoice_id, line_no, commitment_id, order_line_id),
    FOREIGN KEY (tenant_id, company_id, purchase_invoice_id, line_no)
        REFERENCES brant_cnx.purchase_invoice_line (tenant_id, company_id, purchase_invoice_id, line_no)
);

CREATE INDEX purchase_invoice_match_line ON brant_cnx.purchase_invoice_match (tenant_id, company_id, commitment_id, order_line_id);

-- Lines and matches belong to a verification: inserted while their invoice is verified, never
-- changed or deleted.
CREATE FUNCTION brant_cnx.guard_purchase_invoice_entry() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION '%: entries never change and are never deleted', TG_TABLE_NAME USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    PERFORM 1 FROM brant_cnx.purchase_invoice i WHERE i.tenant_id = NEW.tenant_id AND i.company_id = NEW.company_id
        AND i.purchase_invoice_id = NEW.purchase_invoice_id AND i.status = 'verified';
    IF NOT FOUND THEN
        RAISE EXCEPTION '%: invoice % is not verified', TG_TABLE_NAME, NEW.purchase_invoice_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER purchase_invoice_line_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.purchase_invoice_line
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_purchase_invoice_entry();
CREATE TRIGGER purchase_invoice_match_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.purchase_invoice_match
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_purchase_invoice_entry();

CREATE TABLE brant_cnx.tax_refund_case (
    tenant_id          text        NOT NULL,
    company_id         text        NOT NULL,
    case_id            text        NOT NULL CHECK (case_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    export_lot_id      text        NOT NULL CHECK (export_lot_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    attempt            integer     NOT NULL CHECK (attempt > 0),
    order_id           text        NOT NULL,
    exported_at        timestamptz NOT NULL,
    policy_id          text        NOT NULL,
    policy_revision    bigint      NOT NULL CHECK (policy_revision > 0),
    policy_hash        text        NOT NULL CHECK (policy_hash ~ '^[0-9a-f]{64}$'),
    refund_method      text        NOT NULL CHECK (refund_method IN ('exempt_credit_refund', 'exempt_refund', 'not_applicable')),
    policy_valid_from  timestamptz NOT NULL,
    policy_valid_until timestamptz,
    status             text        NOT NULL CHECK (status IN ('expected', 'planned', 'submitted', 'approved', 'received', 'rejected')),
    row_version        bigint      NOT NULL CHECK (row_version > 0),
    opened_by_kind     text        NOT NULL CHECK (opened_by_kind IN ('human', 'ai')),
    opened_by_id       text        NOT NULL,
    authority_epoch    bigint      NOT NULL CHECK (authority_epoch > 0),
    opened_at          timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, case_id),
    UNIQUE (tenant_id, company_id, export_lot_id, attempt),
    CHECK (exported_at <= opened_at),
    CHECK (updated_at >= opened_at)
);

-- A case opens expected at version 1, as its lot's next attempt once every earlier one was
-- rejected; it keeps its binding and moves by one version with each allocation and result, along
-- SM-CNX-TAXCASE.
CREATE FUNCTION brant_cnx.guard_tax_refund_case() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cnx.tax_refund_case: cases are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'expected' OR NEW.row_version <> 1 OR NEW.updated_at <> NEW.opened_at
                OR NEW.attempt <> 1 + (SELECT count(*) FROM brant_cnx.tax_refund_case c WHERE c.tenant_id = NEW.tenant_id
                    AND c.company_id = NEW.company_id AND c.export_lot_id = NEW.export_lot_id)
                OR EXISTS (SELECT 1 FROM brant_cnx.tax_refund_case c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
                    AND c.export_lot_id = NEW.export_lot_id AND c.status <> 'rejected') THEN
            RAISE EXCEPTION 'brant_cnx.tax_refund_case: a case opens expected at version 1 as its lot''s next attempt'
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
CREATE TRIGGER tax_refund_case_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.tax_refund_case
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_tax_refund_case();

CREATE TABLE brant_cnx.tax_evidence_allocation (
    tenant_id           text        NOT NULL,
    company_id          text        NOT NULL,
    allocation_id       text        NOT NULL CHECK (allocation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    case_id             text        NOT NULL,
    export_line_id      text        NOT NULL,
    shipment_id         text        NOT NULL,
    order_line_id       text        NOT NULL,
    sku_id              text        NOT NULL,
    spec_revision       bigint      NOT NULL CHECK (spec_revision >= 0),
    export_quantity     numeric     NOT NULL CHECK (export_quantity > 0),
    export_unit         text        NOT NULL,
    purchase_invoice_id text        NOT NULL,
    line_no             integer     NOT NULL,
    purpose             text        NOT NULL CHECK (purpose IN ('export_refund', 'export_exemption')),
    quantity            numeric     NOT NULL CHECK (quantity > 0),
    unit                text        NOT NULL,
    replaces            text,
    allocated_by_kind   text        NOT NULL CHECK (allocated_by_kind IN ('human', 'ai')),
    allocated_by_id     text        NOT NULL,
    authority_epoch     bigint      NOT NULL CHECK (authority_epoch > 0),
    allocated_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, allocation_id),
    UNIQUE (tenant_id, company_id, replaces),
    FOREIGN KEY (tenant_id, company_id, case_id) REFERENCES brant_cnx.tax_refund_case (tenant_id, company_id, case_id),
    FOREIGN KEY (tenant_id, company_id, purchase_invoice_id, line_no)
        REFERENCES brant_cnx.purchase_invoice_line (tenant_id, company_id, purchase_invoice_id, line_no),
    FOREIGN KEY (tenant_id, company_id, replaces) REFERENCES brant_cnx.tax_evidence_allocation (tenant_id, company_id, allocation_id),
    CHECK (unit = export_unit)
);

CREATE INDEX tax_evidence_allocation_case ON brant_cnx.tax_evidence_allocation (tenant_id, company_id, case_id);
CREATE INDEX tax_evidence_allocation_line ON brant_cnx.tax_evidence_allocation (tenant_id, company_id, purchase_invoice_id, line_no);

-- Whether an allocation counts: not replaced by a later one and its case not rejected.
CREATE FUNCTION brant_cnx.tax_evidence_counts(p_tenant text, p_company text, p_allocation text, p_case text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY INVOKER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT NOT EXISTS (SELECT 1 FROM brant_cnx.tax_evidence_allocation r WHERE r.tenant_id = p_tenant AND r.company_id = p_company
            AND r.replaces = p_allocation)
        AND EXISTS (SELECT 1 FROM brant_cnx.tax_refund_case c WHERE c.tenant_id = p_tenant AND c.company_id = p_company
            AND c.case_id = p_case AND c.status <> 'rejected')
$$;
REVOKE ALL ON FUNCTION brant_cnx.tax_evidence_counts(text, text, text, text) FROM PUBLIC;

-- An allocation is only inserted, under its case's and its invoice's FOR UPDATE locks, while its
-- case is expected or planned, from a verified blue invoice, replacing only an allocation of its
-- case and export line whose invoice was reversed, and never takes Σ counting allocations of its
-- invoice line or of its export line in its case, per purpose, above their quantities (CNX-INV-004).
CREATE FUNCTION brant_cnx.guard_tax_evidence_allocation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_line numeric;
    v_used numeric;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cnx.tax_evidence_allocation: allocations never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    PERFORM 1 FROM brant_cnx.tax_refund_case c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
        AND c.case_id = NEW.case_id AND c.status IN ('expected', 'planned') FOR UPDATE;
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
CREATE TRIGGER tax_evidence_allocation_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.tax_evidence_allocation
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_tax_evidence_allocation();

CREATE TABLE brant_cnx.tax_refund_result (
    tenant_id          text        NOT NULL,
    company_id         text        NOT NULL,
    result_id          text        NOT NULL CHECK (result_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    case_id            text        NOT NULL,
    stage              text        NOT NULL CHECK (stage IN ('expected', 'planned', 'submitted', 'approved', 'received', 'rejected')),
    from_status        text        NOT NULL,
    currency_id        text,
    currency_revision  bigint,
    currency_code      text        CHECK (currency_code ~ '^[A-Z]{3}$'),
    minor_units        integer     CHECK (minor_units BETWEEN 0 AND 4),
    amount_minor       numeric     CHECK (amount_minor >= 0 AND amount_minor = trunc(amount_minor)),
    offset_minor       numeric     CHECK (offset_minor >= 0 AND offset_minor = trunc(offset_minor)),
    tax_period         text        CHECK (tax_period ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'),
    reviewer           text,
    review_evidence    text        NOT NULL CHECK (left(review_evidence, 1) = '['),
    external_reference text,
    result_text        text        CHECK (char_length(result_text) BETWEEN 1 AND 4000),
    fund_record_id     text,
    evidence           text        NOT NULL CHECK (left(evidence, 1) = '['),
    reason             text,
    recorded_by_kind   text        NOT NULL CHECK (recorded_by_kind = 'human'),
    recorded_by_id     text        NOT NULL,
    authority_epoch    bigint      NOT NULL CHECK (authority_epoch > 0),
    recorded_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, result_id),
    FOREIGN KEY (tenant_id, company_id, case_id) REFERENCES brant_cnx.tax_refund_case (tenant_id, company_id, case_id),
    CHECK ((currency_code IS NULL) = (amount_minor IS NULL)),
    CHECK ((stage IN ('expected', 'planned', 'approved', 'received')) = (amount_minor IS NOT NULL)),
    CHECK (offset_minor IS NULL OR stage = 'approved'),
    CHECK ((stage = 'planned') = (tax_period IS NOT NULL AND reviewer IS NOT NULL)),
    CHECK ((stage = 'submitted') = (external_reference IS NOT NULL)),
    CHECK ((stage IN ('approved', 'rejected')) = (result_text IS NOT NULL)),
    CHECK ((stage = 'received') = (fund_record_id IS NOT NULL))
);

CREATE INDEX tax_refund_result_case ON brant_cnx.tax_refund_result (tenant_id, company_id, case_id);

CREATE FUNCTION brant_cnx.guard_tax_refund_result() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cnx.tax_refund_result: results never change and are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER tax_refund_result_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.tax_refund_result
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_tax_refund_result();

ALTER TABLE brant_cnx.purchase_invoice ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.purchase_invoice FORCE ROW LEVEL SECURITY;
CREATE POLICY purchase_invoice_scope ON brant_cnx.purchase_invoice
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.purchase_invoice_line ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.purchase_invoice_line FORCE ROW LEVEL SECURITY;
CREATE POLICY purchase_invoice_line_scope ON brant_cnx.purchase_invoice_line
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.purchase_invoice_match ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.purchase_invoice_match FORCE ROW LEVEL SECURITY;
CREATE POLICY purchase_invoice_match_scope ON brant_cnx.purchase_invoice_match
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.tax_refund_case ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.tax_refund_case FORCE ROW LEVEL SECURITY;
CREATE POLICY tax_refund_case_scope ON brant_cnx.tax_refund_case
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.tax_evidence_allocation ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.tax_evidence_allocation FORCE ROW LEVEL SECURITY;
CREATE POLICY tax_evidence_allocation_scope ON brant_cnx.tax_evidence_allocation
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.tax_refund_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.tax_refund_result FORCE ROW LEVEL SECURITY;
CREATE POLICY tax_refund_result_scope ON brant_cnx.tax_refund_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
