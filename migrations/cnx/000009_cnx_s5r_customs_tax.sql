-- CNX: S5R customs, certificates, purchase invoices and tax refunds (S5R unit R10, WBS S5R-10; simulated sign-off E0 of
-- 2026-11-13: S5R-D12, D13, D14, D20, D21 (CNX half), D23, D36). Tactical model SM-CNX-DECLARATION, SM-CNX-INVOICE,
-- SM-CNX-TAXCASE, CNX-INV-003, -004, -005, -007, -008; Context Map R50 (ConfigAt v1), R55 (PurchaseInvoiceRef v1), R77
-- (ServiceRoleRef v1 of a customs broker). No row changes; applied files are not edited, replaced functions are
-- restated whole.
--
-- Declarations (S5R-D13, D36). A declaration whose result is unknown may be recorded not_received when the verification
-- (the status customs' own query shows, or the broker's written confirmation) finds customs never received it: a final
-- state, with a reason and at least one evidence source, after which the lot's next attempt may be prepared, as after a
-- rejection or a withdrawal. A broker's submission names the PTY service role (customs_broker) it rests on.
--
-- Certificates (S5R-D12). A new kind preferential_certificate_of_origin records an externally issued preferential
-- certificate of origin with its agreement code and its form: a certificate issued by customs or CCPIT, or the origin
-- declaration of an approved exporter (RCEP). Brant judges no origin rule.
--
-- Purchase invoices (S5R-D20). A red invoice may reverse part of a blue one: its lines name the blue lines they reverse
-- (by line number), and what verified red invoices reversed of a blue line is no longer evidence. Several red invoices
-- may reverse one blue invoice; the blue one is reversed (its state) only when nothing of it remains, and its version
-- moves with every reversal. A red invoice's matches take back what a blue match matched to a purchase commitment line.
-- An allocation is under review while what counting allocations and opening consumption take of its invoice line
-- exceeds what the line evidences now; a replacement may take the same line.
--
-- Tax refund cases (S5R-D14, D20, D23). A taxable export opens no case. A case converts to an exemption
-- (converted_to_exemption, final) from expected or planned; a filed case (submitted, approved, received) records the
-- external rollback of a purchase invoice's evidence (evidence_rolled_back, same state) before a red invoice may
-- reverse that invoice. An expected case may rebind the export_tax_policy revision ConfigAt v1 answers for its export
-- date (tax_refund_case.rebind_policy): the binding of an expected case may change, every rebinding is recorded.

-- Declarations -------------------------------------------------------------------------------------------------------

ALTER TABLE brant_cnx.declaration_case DROP CONSTRAINT declaration_case_status_check;
ALTER TABLE brant_cnx.declaration_case ADD CONSTRAINT declaration_case_status_check
    CHECK (status IN ('draft', 'confirmed', 'submitted', 'unknown', 'received', 'accepted', 'released', 'rejected', 'amended', 'withdrawn',
        'not_received'));

-- 000004's case guard, with not_received: unknown → not_received is final, and the lot's next attempt opens after it.
CREATE OR REPLACE FUNCTION brant_cnx.guard_declaration_case() RETURNS trigger LANGUAGE plpgsql AS $$
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
                    AND c.export_lot_id = NEW.export_lot_id AND c.status NOT IN ('rejected', 'withdrawn', 'not_received')) THEN
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
                    ('unknown', 'released'), ('unknown', 'rejected'), ('unknown', 'not_received'), ('received', 'accepted'), ('received', 'released'),
                    ('received', 'rejected'), ('accepted', 'released'), ('accepted', 'rejected'), ('released', 'amended'), ('released', 'withdrawn'),
                    ('amended', 'amended'), ('amended', 'withdrawn')))) THEN
        RAISE EXCEPTION 'brant_cnx.declaration_case: case % keeps its binding and follows SM-CNX-DECLARATION', OLD.declaration_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

-- not_received, like unknown, states no external result; it gives a reason and rests on evidence.
ALTER TABLE brant_cnx.declaration_receipt DROP CONSTRAINT declaration_receipt_outcome_check;
ALTER TABLE brant_cnx.declaration_receipt ADD CONSTRAINT declaration_receipt_outcome_check
    CHECK (outcome IN ('unknown', 'received', 'accepted', 'released', 'rejected', 'amended', 'withdrawn', 'not_received'));
ALTER TABLE brant_cnx.declaration_receipt DROP CONSTRAINT declaration_receipt_check;
ALTER TABLE brant_cnx.declaration_receipt ADD CONSTRAINT declaration_receipt_external_check
    CHECK ((outcome IN ('unknown', 'not_received')) = (external_sequence IS NULL));
ALTER TABLE brant_cnx.declaration_receipt DROP CONSTRAINT declaration_receipt_check2;
ALTER TABLE brant_cnx.declaration_receipt ADD CONSTRAINT declaration_receipt_reason_required
    CHECK (outcome NOT IN ('unknown', 'not_received') OR reason IS NOT NULL);
ALTER TABLE brant_cnx.declaration_receipt ADD CONSTRAINT declaration_receipt_not_received_evidence
    CHECK (outcome <> 'not_received' OR evidence <> '[]');

-- A broker's submission names the verified PTY service role it rests on (S5R-D36, R77); submissions recorded before
-- name none.
ALTER TABLE brant_cnx.declaration_submission ADD COLUMN service_role_id text CHECK (service_role_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    ADD COLUMN service_role_revision bigint CHECK (service_role_revision > 0),
    ADD COLUMN service_role_kind text CHECK (service_role_kind IN ('customs_broker', 'broker')),
    ADD CONSTRAINT declaration_submission_service_role
        CHECK ((service_role_id IS NULL) = (service_role_revision IS NULL) AND (service_role_id IS NULL) = (service_role_kind IS NULL)
            AND (service_role_id IS NULL OR submitter_kind = 'broker'));

-- Certificates ------------------------------------------------------------------------------------------------------

ALTER TABLE brant_cnx.certificate_result DROP CONSTRAINT certificate_result_kind_check;
ALTER TABLE brant_cnx.certificate_result ADD CONSTRAINT certificate_result_kind_check
    CHECK (kind IN ('certificate_of_origin', 'license_copy', 'inspection_certificate', 'preferential_certificate_of_origin'));
ALTER TABLE brant_cnx.certificate_revision DROP CONSTRAINT certificate_revision_kind_check;
ALTER TABLE brant_cnx.certificate_revision ADD CONSTRAINT certificate_revision_kind_check
    CHECK (kind IN ('certificate_of_origin', 'license_copy', 'inspection_certificate', 'preferential_certificate_of_origin'));
-- A preferential certificate states its agreement and form; a certificate (not an origin declaration) its issuing body.
ALTER TABLE brant_cnx.certificate_revision ADD COLUMN agreement_code text CHECK (agreement_code ~ '^[A-Z][A-Z0-9-]{1,31}$'),
    ADD COLUMN origin_form text CHECK (origin_form IN ('certificate', 'origin_declaration')),
    ADD COLUMN issuing_body text CHECK (issuing_body IN ('customs', 'ccpit')),
    ADD CONSTRAINT certificate_revision_preferential
        CHECK ((kind = 'preferential_certificate_of_origin') = (agreement_code IS NOT NULL) AND (agreement_code IS NULL) = (origin_form IS NULL)
            AND (origin_form IS NOT NULL OR issuing_body IS NULL) AND (origin_form IS NULL OR (origin_form = 'certificate') = (issuing_body IS NOT NULL)));

-- Purchase invoices -------------------------------------------------------------------------------------------------

-- Several red invoices may reverse one blue invoice (partial reversals; S5R-D20).
DROP INDEX brant_cnx.purchase_invoice_red_of;
CREATE INDEX purchase_invoice_red_of ON brant_cnx.purchase_invoice (tenant_id, company_id, red_of_invoice_id) WHERE red_of_invoice_id IS NOT NULL;

-- 000003's invoice guard, with partial reversals: a verified blue invoice moves by one version, verified, with each red
-- invoice that reverses part of it, and to reversed with the one that reverses what remained.
CREATE OR REPLACE FUNCTION brant_cnx.guard_purchase_invoice() RETURNS trigger LANGUAGE plpgsql AS $$
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
                OR (OLD.status = 'verified' AND NEW.status IN ('verified', 'reversed') AND OLD.colour = 'blue'))
            OR (OLD.status <> 'imported' AND (SELECT bool_or(to_jsonb(NEW) -> f IS DISTINCT FROM to_jsonb(OLD) -> f) FROM unnest(v_decided) f)) THEN
        RAISE EXCEPTION 'brant_cnx.purchase_invoice: invoice % keeps its import, is decided once and reversed by red invoices', OLD.purchase_invoice_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

-- What verified red invoices reversed of a line of a blue invoice (quantity, in the line's unit).
CREATE FUNCTION brant_cnx.purchase_invoice_line_reversed(p_tenant text, p_company text, p_invoice text, p_line integer) RETURNS numeric
    LANGUAGE sql STABLE SECURITY INVOKER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT coalesce(sum(l.quantity), 0) FROM brant_cnx.purchase_invoice r
        JOIN brant_cnx.purchase_invoice_line l ON l.tenant_id = r.tenant_id AND l.company_id = r.company_id
            AND l.purchase_invoice_id = r.purchase_invoice_id
        WHERE r.tenant_id = p_tenant AND r.company_id = p_company AND r.red_of_invoice_id = p_invoice AND r.status = 'verified'
            AND l.line_no = p_line
$$;
REVOKE ALL ON FUNCTION brant_cnx.purchase_invoice_line_reversed(text, text, text, integer) FROM PUBLIC;

-- A red invoice's match takes back part of a match of the blue invoice it reverses: the same line, commitment line and
-- unit, and never more than that match matched, with every earlier take-back (under the blue invoice's lock).
CREATE FUNCTION brant_cnx.guard_purchase_invoice_reversal_match() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_blue text;
    v_matched numeric;
    v_taken numeric;
BEGIN
    SELECT i.red_of_invoice_id INTO v_blue FROM brant_cnx.purchase_invoice i WHERE i.tenant_id = NEW.tenant_id AND i.company_id = NEW.company_id
        AND i.purchase_invoice_id = NEW.purchase_invoice_id AND i.colour = 'red';
    IF NOT FOUND THEN
        RETURN NEW;
    END IF;
    PERFORM 1 FROM brant_cnx.purchase_invoice b WHERE b.tenant_id = NEW.tenant_id AND b.company_id = NEW.company_id
        AND b.purchase_invoice_id = v_blue FOR UPDATE;
    SELECT m.quantity INTO v_matched FROM brant_cnx.purchase_invoice_match m WHERE m.tenant_id = NEW.tenant_id AND m.company_id = NEW.company_id
        AND m.purchase_invoice_id = v_blue AND m.line_no = NEW.line_no AND m.commitment_id = NEW.commitment_id
        AND m.order_line_id = NEW.order_line_id AND m.unit = NEW.unit;
    SELECT coalesce(sum(m.quantity), 0) INTO v_taken FROM brant_cnx.purchase_invoice_match m
        JOIN brant_cnx.purchase_invoice r ON r.tenant_id = m.tenant_id AND r.company_id = m.company_id AND r.purchase_invoice_id = m.purchase_invoice_id
        WHERE m.tenant_id = NEW.tenant_id AND m.company_id = NEW.company_id AND r.red_of_invoice_id = v_blue AND r.status = 'verified'
            AND m.line_no = NEW.line_no AND m.commitment_id = NEW.commitment_id AND m.order_line_id = NEW.order_line_id;
    IF v_matched IS NULL OR v_taken + NEW.quantity > v_matched THEN
        RAISE EXCEPTION 'brant_cnx.purchase_invoice_match: red invoice % takes back more than blue invoice % matched to % / %',
            NEW.purchase_invoice_id, v_blue, NEW.commitment_id, NEW.order_line_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER purchase_invoice_match_reversal_guard BEFORE INSERT ON brant_cnx.purchase_invoice_match
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_purchase_invoice_reversal_match();

-- Whether counting allocations and the opening consumption take more of an invoice line, for a purpose, than the line
-- evidences now (its quantity less what red invoices reversed): their allocations are then under review.
CREATE FUNCTION brant_cnx.tax_evidence_overused(p_tenant text, p_company text, p_invoice text, p_line integer, p_purpose text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY INVOKER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT (SELECT coalesce(sum(a.quantity), 0) FROM brant_cnx.tax_evidence_allocation a WHERE a.tenant_id = p_tenant AND a.company_id = p_company
                AND a.purchase_invoice_id = p_invoice AND a.line_no = p_line AND a.purpose = p_purpose
                AND brant_cnx.tax_evidence_counts(a.tenant_id, a.company_id, a.allocation_id, a.case_id))
        + (SELECT coalesce(sum(o.consumed), 0) FROM brant_cnx.tax_evidence_opening o WHERE o.tenant_id = p_tenant AND o.company_id = p_company
                AND o.purchase_invoice_id = p_invoice AND o.line_no = p_line AND o.purpose = p_purpose)
        > (SELECT l.quantity - brant_cnx.purchase_invoice_line_reversed(p_tenant, p_company, p_invoice, p_line) FROM brant_cnx.purchase_invoice_line l
                WHERE l.tenant_id = p_tenant AND l.company_id = p_company AND l.purchase_invoice_id = p_invoice AND l.line_no = p_line)
$$;
REVOKE ALL ON FUNCTION brant_cnx.tax_evidence_overused(text, text, text, integer, text) FROM PUBLIC;

-- Tax refund cases --------------------------------------------------------------------------------------------------

ALTER TABLE brant_cnx.tax_refund_case DROP CONSTRAINT tax_refund_case_status_check;
ALTER TABLE brant_cnx.tax_refund_case ADD CONSTRAINT tax_refund_case_status_check
    CHECK (status IN ('expected', 'planned', 'submitted', 'approved', 'received', 'rejected', 'converted_to_exemption'));

-- 000006's case guard, with S5R: a case never binds a taxable revision (a taxable export opens no case, S5R-D14); an
-- expected case may rebind another revision of its policy (S5R-D23); a filed case records rollbacks in its state (S5R-D20);
-- expected and planned convert to an exemption (S5R-D14).
CREATE OR REPLACE FUNCTION brant_cnx.guard_tax_refund_case() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_binding text[] := ARRAY['policy_revision', 'policy_hash', 'refund_method', 'policy_valid_from', 'policy_valid_until'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cnx.tax_refund_case: cases are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.refund_method = 'taxable' AND (TG_OP = 'INSERT' OR NEW.policy_revision <> OLD.policy_revision) THEN
        RAISE EXCEPTION 'brant_cnx.tax_refund_case: a taxable export opens no tax refund case (S5R-D14)' USING ERRCODE = 'integrity_constraint_violation';
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
    IF (to_jsonb(NEW) - ARRAY['status', 'row_version', 'updated_at'] - v_binding)
                IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['status', 'row_version', 'updated_at'] - v_binding)
            OR ((SELECT bool_or(to_jsonb(NEW) -> f IS DISTINCT FROM to_jsonb(OLD) -> f) FROM unnest(v_binding) f)
                AND NOT (OLD.status = 'expected' AND NEW.status = 'expected' AND NEW.policy_revision <> OLD.policy_revision))
            OR NEW.row_version <> OLD.row_version + 1 OR NEW.updated_at < OLD.updated_at
            OR NOT (NEW.status = OLD.status AND OLD.status IN ('expected', 'planned', 'submitted', 'approved', 'received')
                OR (OLD.status, NEW.status) IN (('expected', 'planned'), ('planned', 'submitted'), ('submitted', 'approved'),
                    ('submitted', 'rejected'), ('approved', 'received'), ('expected', 'converted_to_exemption'),
                    ('planned', 'converted_to_exemption'))) THEN
        RAISE EXCEPTION 'brant_cnx.tax_refund_case: case % keeps its binding (an expected case rebinds) and follows SM-CNX-TAXCASE', OLD.case_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

-- Every rebinding of an expected case (tax_refund_case.rebind_policy, S5R-D23): from which revision to which, why, by
-- whom, under which authority epoch, and the case's version it made. Only inserted, with the case binding it.
CREATE TABLE brant_cnx.tax_policy_rebinding (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    rebinding_id    text        NOT NULL CHECK (rebinding_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    case_id         text        NOT NULL,
    case_version    bigint      NOT NULL CHECK (case_version > 1),
    policy_id       text        NOT NULL,
    from_revision   bigint      NOT NULL CHECK (from_revision > 0),
    from_hash       text        NOT NULL CHECK (from_hash ~ '^[0-9a-f]{64}$'),
    from_method     text        NOT NULL,
    to_revision     bigint      NOT NULL CHECK (to_revision > 0),
    to_hash         text        NOT NULL CHECK (to_hash ~ '^[0-9a-f]{64}$'),
    to_method       text        NOT NULL CHECK (to_method <> 'taxable'),
    reason          text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    rebound_by_kind text        NOT NULL CHECK (rebound_by_kind = 'human'),
    rebound_by_id   text        NOT NULL,
    authority_epoch bigint      NOT NULL CHECK (authority_epoch > 0),
    rebound_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, rebinding_id),
    UNIQUE (tenant_id, company_id, case_id, case_version),
    FOREIGN KEY (tenant_id, company_id, case_id) REFERENCES brant_cnx.tax_refund_case (tenant_id, company_id, case_id),
    CHECK (to_revision <> from_revision)
);

CREATE FUNCTION brant_cnx.guard_tax_policy_rebinding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cnx.tax_policy_rebinding: rebindings never change and are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    PERFORM 1 FROM brant_cnx.tax_refund_case c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id AND c.case_id = NEW.case_id
        AND c.status = 'expected' AND c.row_version = NEW.case_version AND c.policy_id = NEW.policy_id AND c.policy_revision = NEW.to_revision
        AND c.policy_hash = NEW.to_hash AND c.refund_method = NEW.to_method FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'brant_cnx.tax_policy_rebinding: case % does not bind revision % at version %', NEW.case_id, NEW.to_revision, NEW.case_version
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER tax_policy_rebinding_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.tax_policy_rebinding
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_tax_policy_rebinding();

-- At commit, every change of a case's binding is a recorded rebinding of that version (S5R-D23).
CREATE FUNCTION brant_cnx.check_tax_refund_case_rebinding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.policy_revision <> OLD.policy_revision AND NOT EXISTS (SELECT 1 FROM brant_cnx.tax_policy_rebinding r WHERE r.tenant_id = NEW.tenant_id
            AND r.company_id = NEW.company_id AND r.case_id = NEW.case_id AND r.case_version = NEW.row_version AND r.from_revision = OLD.policy_revision
            AND r.to_revision = NEW.policy_revision) THEN
        RAISE EXCEPTION 'brant_cnx.tax_refund_case: case % rebinds its policy only with a recorded rebinding', NEW.case_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER tax_refund_case_rebinding_recorded AFTER UPDATE ON brant_cnx.tax_refund_case
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_cnx.check_tax_refund_case_rebinding();

-- Results: the conversion to an exemption (evidence of the missed deadline and a reason) and the external rollback of a
-- purchase invoice's evidence (the invoice, the external confirmation and evidence; once per case and invoice).
ALTER TABLE brant_cnx.tax_refund_result DROP CONSTRAINT tax_refund_result_stage_check;
ALTER TABLE brant_cnx.tax_refund_result ADD CONSTRAINT tax_refund_result_stage_check
    CHECK (stage IN ('expected', 'planned', 'submitted', 'approved', 'received', 'rejected', 'converted_to_exemption', 'evidence_rolled_back'));
ALTER TABLE brant_cnx.tax_refund_result DROP CONSTRAINT tax_refund_result_check5;
ALTER TABLE brant_cnx.tax_refund_result ADD CONSTRAINT tax_refund_result_result_text_required
    CHECK ((stage IN ('approved', 'rejected', 'evidence_rolled_back')) = (result_text IS NOT NULL));
ALTER TABLE brant_cnx.tax_refund_result ADD COLUMN purchase_invoice_id text,
    ADD FOREIGN KEY (tenant_id, company_id, purchase_invoice_id) REFERENCES brant_cnx.purchase_invoice (tenant_id, company_id, purchase_invoice_id),
    ADD CONSTRAINT tax_refund_result_rolled_back CHECK ((stage = 'evidence_rolled_back') = (purchase_invoice_id IS NOT NULL)),
    ADD CONSTRAINT tax_refund_result_evidence_required CHECK (stage NOT IN ('converted_to_exemption', 'evidence_rolled_back') OR evidence <> '[]'),
    ADD CONSTRAINT tax_refund_result_conversion_reason CHECK (stage <> 'converted_to_exemption' OR reason IS NOT NULL);
CREATE UNIQUE INDEX tax_refund_result_rollback ON brant_cnx.tax_refund_result (tenant_id, company_id, case_id, purchase_invoice_id)
    WHERE stage = 'evidence_rolled_back';

-- 000006's allocation guard, with partial reversals (S5R-D20): the invoice line evidences its quantity less what red
-- invoices reversed; a replacement replaces an allocation of the case and export line under review (its invoice reversed
-- or its line over-used) and may take the same invoice line, whose balance then leaves the replaced allocation out.
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
                AND i.purchase_invoice_id = r.purchase_invoice_id
            WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.allocation_id = NEW.replaces
                AND r.case_id = NEW.case_id AND r.export_line_id = NEW.export_line_id
                AND (i.status = 'reversed' OR brant_cnx.tax_evidence_overused(r.tenant_id, r.company_id, r.purchase_invoice_id, r.line_no, r.purpose))) THEN
        RAISE EXCEPTION 'brant_cnx.tax_evidence_allocation: % replaces no allocation under review', NEW.allocation_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT l.quantity - brant_cnx.purchase_invoice_line_reversed(l.tenant_id, l.company_id, l.purchase_invoice_id, l.line_no) INTO v_line
        FROM brant_cnx.purchase_invoice_line l WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id
        AND l.purchase_invoice_id = NEW.purchase_invoice_id AND l.line_no = NEW.line_no AND l.unit = NEW.unit;
    SELECT coalesce(sum(a.quantity), 0) INTO v_used FROM brant_cnx.tax_evidence_allocation a WHERE a.tenant_id = NEW.tenant_id
        AND a.company_id = NEW.company_id AND a.purchase_invoice_id = NEW.purchase_invoice_id AND a.line_no = NEW.line_no
        AND a.purpose = NEW.purpose AND a.allocation_id IS DISTINCT FROM NEW.replaces
        AND brant_cnx.tax_evidence_counts(a.tenant_id, a.company_id, a.allocation_id, a.case_id);
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

-- 000006's opening consumption guard, with partial reversals: the line evidences its quantity less what red invoices
-- reversed. Nothing else changes.
CREATE OR REPLACE FUNCTION brant_cnx.guard_tax_evidence_opening() RETURNS trigger LANGUAGE plpgsql AS $$
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
    SELECT l.quantity - brant_cnx.purchase_invoice_line_reversed(l.tenant_id, l.company_id, l.purchase_invoice_id, l.line_no) INTO v_line
        FROM brant_cnx.purchase_invoice_line l WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id
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

ALTER TABLE brant_cnx.tax_policy_rebinding ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.tax_policy_rebinding FORCE ROW LEVEL SECURITY;
CREATE POLICY tax_policy_rebinding_scope ON brant_cnx.tax_policy_rebinding
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
