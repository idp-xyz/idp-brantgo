-- CNX channel receipts (S6-05, S6-D05). The connector port is internal/foundation/interop. A late or
-- duplicate external receipt does not advance a declaration (DECLARATION_RECEIPT_STALE, CNX-INV-003);
-- it is stored here so the external version chain is kept (baseline 7.2, CN-AT06). An authoritative
-- receipt is the one that did advance the case. Channel attempts are one row per filing; after
-- not_received the row stays and the next filing is a new row.
--
-- Tax refund cases gain the connector states unknown and not_received beside SM-CNX-TAXCASE.
-- Existing manual transitions are unchanged. not_received releases evidence the way rejected does,
-- so the lot's next case can be allocated. unknown still holds the evidence.

-- Declarations and tax filings share the attempt row. The subject is the declaration or the tax case.
CREATE TABLE brant_cnx.channel_attempt (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    attempt_id      text        NOT NULL CHECK (attempt_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    channel         text        NOT NULL CHECK (channel IN ('customs_declaration', 'tax_refund')),
    subject_id      text        NOT NULL CHECK (subject_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    attempt_no      integer     NOT NULL CHECK (attempt_no > 0),
    operation_id    text        NOT NULL CHECK (operation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    state           text        NOT NULL CHECK (state IN ('planned', 'submitted', 'succeeded', 'failed', 'unknown', 'not_received')),
    outcome         text        CHECK (outcome IS NULL OR char_length(outcome) BETWEEN 1 AND 32),
    reason          text        NOT NULL DEFAULT '' CHECK (char_length(reason) <= 1000),
    row_version     bigint      NOT NULL CHECK (row_version > 0),
    actor_kind      text        NOT NULL CHECK (actor_kind IN ('human', 'ai', 'system')),
    actor_id        text        NOT NULL,
    authority_epoch bigint      NOT NULL CHECK (authority_epoch > 0),
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, attempt_id),
    UNIQUE (tenant_id, company_id, operation_id),
    UNIQUE (tenant_id, company_id, channel, subject_id, attempt_no),
    CHECK ((state IN ('succeeded', 'failed')) = (outcome IS NOT NULL)),
    CHECK (updated_at >= created_at)
);

-- An attempt opens planned. It moves along the connector states and stops at succeeded, failed or
-- not_received: that row is not overwritten by a later filing.
CREATE FUNCTION brant_cnx.guard_channel_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cnx.channel_attempt: attempts are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.state <> 'planned' OR NEW.outcome IS NOT NULL OR NEW.row_version <> 1 OR NEW.updated_at <> NEW.created_at
                OR NEW.attempt_no <> 1 + (SELECT count(*) FROM brant_cnx.channel_attempt a WHERE a.tenant_id = NEW.tenant_id
                    AND a.company_id = NEW.company_id AND a.channel = NEW.channel AND a.subject_id = NEW.subject_id) THEN
            RAISE EXCEPTION 'brant_cnx.channel_attempt: an attempt opens planned as the subject''s next attempt'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - ARRAY['state', 'outcome', 'reason', 'row_version', 'updated_at'])
            IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['state', 'outcome', 'reason', 'row_version', 'updated_at'])
            OR NEW.row_version <> OLD.row_version + 1 OR NEW.updated_at < OLD.updated_at
            OR NOT ((OLD.state, NEW.state) IN (
                ('planned', 'submitted'), ('planned', 'unknown'), ('planned', 'succeeded'), ('planned', 'failed'),
                ('submitted', 'unknown'), ('submitted', 'succeeded'), ('submitted', 'failed'),
                ('unknown', 'succeeded'), ('unknown', 'failed'), ('unknown', 'not_received'))) THEN
        RAISE EXCEPTION 'brant_cnx.channel_attempt: attempt % keeps its operation and follows the connector', OLD.attempt_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER channel_attempt_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.channel_attempt
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_channel_attempt();

-- Every external receipt the channel ingested, in arrival order. authoritative is true only when the
-- receipt advanced the declaration. A stale or illegal one keeps refusal_code and does not.
CREATE TABLE brant_cnx.channel_receipt (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    receipt_id        text        NOT NULL CHECK (receipt_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    channel           text        NOT NULL CHECK (channel IN ('customs_declaration', 'tax_refund')),
    subject_id        text        NOT NULL CHECK (subject_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    operation_id      text        NOT NULL DEFAULT '' CHECK (operation_id = '' OR operation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    ingress_no        integer     NOT NULL CHECK (ingress_no > 0),
    external_sequence bigint      CHECK (external_sequence > 0),
    outcome           text        NOT NULL CHECK (char_length(outcome) BETWEEN 1 AND 32),
    status_code       text        NOT NULL DEFAULT '' CHECK (char_length(status_code) <= 64),
    original_text     text        NOT NULL DEFAULT '' CHECK (char_length(original_text) <= 4000),
    mapping_version   text        NOT NULL DEFAULT '' CHECK (mapping_version = '' OR mapping_version ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    authoritative     boolean     NOT NULL,
    refusal_code      text        NOT NULL DEFAULT '' CHECK (char_length(refusal_code) <= 64),
    recorded_at       timestamptz NOT NULL,
    actor_kind        text        NOT NULL CHECK (actor_kind = 'human'),
    actor_id          text        NOT NULL,
    authority_epoch   bigint      NOT NULL CHECK (authority_epoch > 0),
    PRIMARY KEY (tenant_id, company_id, receipt_id),
    UNIQUE (tenant_id, company_id, channel, subject_id, ingress_no),
    CHECK (authoritative = (refusal_code = ''))
);

CREATE INDEX channel_receipt_subject ON brant_cnx.channel_receipt (tenant_id, company_id, channel, subject_id, ingress_no);

CREATE FUNCTION brant_cnx.guard_channel_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cnx.channel_receipt: receipts never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.ingress_no <> 1 + (SELECT count(*) FROM brant_cnx.channel_receipt r WHERE r.tenant_id = NEW.tenant_id
            AND r.company_id = NEW.company_id AND r.channel = NEW.channel AND r.subject_id = NEW.subject_id) THEN
        RAISE EXCEPTION 'brant_cnx.channel_receipt: receipts of % are numbered without gaps', NEW.subject_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER channel_receipt_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.channel_receipt
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_channel_receipt();

ALTER TABLE brant_cnx.channel_attempt ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.channel_attempt FORCE ROW LEVEL SECURITY;
CREATE POLICY channel_attempt_scope ON brant_cnx.channel_attempt
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.channel_receipt ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.channel_receipt FORCE ROW LEVEL SECURITY;
CREATE POLICY channel_receipt_scope ON brant_cnx.channel_receipt
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

-- Tax refund cases (S6-D05). unknown and not_received sit beside the manual stages. A person still
-- records expected, planned, submitted, approved, received, rejected, converted_to_exemption and
-- evidence_rolled_back as before.
ALTER TABLE brant_cnx.tax_refund_case DROP CONSTRAINT tax_refund_case_status_check;
ALTER TABLE brant_cnx.tax_refund_case ADD CONSTRAINT tax_refund_case_status_check
    CHECK (status IN ('expected', 'planned', 'submitted', 'approved', 'received', 'rejected', 'converted_to_exemption',
        'unknown', 'not_received'));

-- 000009's case guard, plus the connector: submitted → unknown, and unknown → approved, rejected or
-- not_received. A not_received case, like a rejected one, lets the lot's next attempt open.
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
                    AND c.export_lot_id = NEW.export_lot_id AND c.status NOT IN ('rejected', 'not_received')) THEN
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
                    ('submitted', 'rejected'), ('submitted', 'unknown'), ('approved', 'received'),
                    ('unknown', 'approved'), ('unknown', 'rejected'), ('unknown', 'not_received'),
                    ('expected', 'converted_to_exemption'), ('planned', 'converted_to_exemption'))) THEN
        RAISE EXCEPTION 'brant_cnx.tax_refund_case: case % keeps its binding (an expected case rebinds) and follows SM-CNX-TAXCASE', OLD.case_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

-- not_received returns the evidence, as a rejection does. unknown does not: the filing may still stand.
CREATE OR REPLACE FUNCTION brant_cnx.tax_evidence_counts(p_tenant text, p_company text, p_allocation text, p_case text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY INVOKER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT NOT EXISTS (SELECT 1 FROM brant_cnx.tax_evidence_allocation r WHERE r.tenant_id = p_tenant AND r.company_id = p_company
            AND r.replaces = p_allocation)
        AND EXISTS (SELECT 1 FROM brant_cnx.tax_refund_case c WHERE c.tenant_id = p_tenant AND c.company_id = p_company
            AND c.case_id = p_case AND c.status NOT IN ('rejected', 'not_received'))
$$;

ALTER TABLE brant_cnx.tax_refund_result DROP CONSTRAINT tax_refund_result_stage_check;
ALTER TABLE brant_cnx.tax_refund_result ADD CONSTRAINT tax_refund_result_stage_check
    CHECK (stage IN ('expected', 'planned', 'submitted', 'approved', 'received', 'rejected', 'converted_to_exemption',
        'evidence_rolled_back', 'unknown', 'not_received'));
ALTER TABLE brant_cnx.tax_refund_result ADD CONSTRAINT tax_refund_result_channel_reason
    CHECK (stage NOT IN ('unknown', 'not_received') OR reason IS NOT NULL);
ALTER TABLE brant_cnx.tax_refund_result ADD CONSTRAINT tax_refund_result_not_received_evidence
    CHECK (stage <> 'not_received' OR evidence <> '[]');
