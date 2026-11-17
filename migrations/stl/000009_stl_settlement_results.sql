-- STL: fund reversal, charge payments and the realised FX difference, payable adjustments, opening credit notes, cost
-- components and the result of a cancelled order (S5R unit R8, WBS S5R-08; simulated sign-off E0 of 2026-11-13:
-- S5R-D19, S5R-D20 (STL half), S5R-D22 (opening credit notes, fund.reverse), S5R-D24, S5R-D42, S5R-D46(2); tactical
-- model SM-STL-FUND, SM-STL-APPLICATION, SM-STL-ORDER-RESULT, STL-INV-002, STL-INV-008, STL-INV-010, STL-INV-011,
-- STL-INV-013, STL-INV-014; Context Map R31, R55). Python V0.50 has no STL.
--
-- S5R-D22. fund.reverse reverses any fund record, an opening prepayment included: a person other than its recorder,
-- while nothing stands applied from it (FUND_RECORD_HAS_APPLICATIONS) and nothing drawn on it stands (refunds,
-- returned remittances and conversions are reversed first). The record stays, reversed (terminal), with its reversal;
-- its bank reference may be recorded again on the account; an opening prepayment's item is reversed with it (its
-- source document may be recorded again). A credit note may be requested on an opening receivable: it binds no order.
--
-- S5R-D19. An outgoing payment may be applied to a confirmed charge (target kind charge) of the charge's source party:
-- the charge's paid state is derived from those applications, and the order result counts them in net cash. A receipt
-- in the order's currency applied to an order analysed in another currency may record, at the application, the POL
-- settlement rate in force from the order's currency into the analysis currency: what of the receipt is not converted
-- yet is valued at it (unrealised); what its fx_conversion records converted at their bank rates (realised). Moves keep
-- the rate.
--
-- S5R-D20. A payable adjustment (price allowance or under-invoicing, referencing a CNX purchase invoice) is an entity of a
-- purchase statement: requested by one person, confirmed or rejected once by another; a confirmed one reduces what is
-- open on the payable like a credit on the purchase side (STL-INV-013). Request and decision move the statement's
-- version, like a credit note; a statement is not reversed while an adjustment waits for its decision.
--
-- S5R-D24. A charge states its cost component (pre_carriage, export_clearance, main_carriage, insurance, other) and
-- whether it is recharged to the buyer; charges recorded before this migration are 'other' and not recharged.
--
-- S5R-D42. A closing snapshot records whether it closed the result of a cancelled order.
--
-- Merge note: this migration replaces brant_stl.guard_fund_record() and brant_stl.check_fund_balance() from 000008's
-- text (keeping the payer verifications in the version), and restores in check_fund_balance 000005's sum of what each
-- application takes from the fund (its source amount when it converts), which 000008 had dropped; it replaces
-- brant_stl.guard_application() from 000008's text (keeping the payer basis), check_application_target() and
-- check_settlement_root() from 000005's text, guard_credit_note() from 000005's text and guard_opening_balance() from
-- 000007's text. A later replacement starts from these.

-- S5R-D22: fund reversal ------------------------------------------------------------------------------------------

ALTER TABLE brant_stl.fund_record DROP CONSTRAINT fund_record_status_check;
ALTER TABLE brant_stl.fund_record ADD CONSTRAINT fund_record_status_check CHECK (status IN ('recorded', 'difference_resolved', 'reversed'));
-- 000008's version check: a fund record other than a receipt is recorded at version 1 or reversed at version 2;
-- check_fund_balance counts a receipt's version exactly.
ALTER TABLE brant_stl.fund_record DROP CONSTRAINT fund_record_version_check7;
ALTER TABLE brant_stl.fund_record ADD CONSTRAINT fund_record_version_check7 CHECK (kind = 'receipt' OR (status = 'recorded' AND version = 1)
    OR (status = 'reversed' AND version = 2));
-- A bank reference is recorded once on an account among the fund records that stand: a reversed record frees it.
ALTER TABLE brant_stl.fund_record DROP CONSTRAINT fund_record_tenant_id_company_id_bank_account_id_bank_refer_key;
CREATE UNIQUE INDEX fund_record_bank_reference ON brant_stl.fund_record (tenant_id, company_id, bank_account_id, bank_reference)
    WHERE status <> 'reversed';

CREATE TABLE brant_stl.fund_reversal (
    tenant_id        text        NOT NULL,
    company_id       text        NOT NULL,
    fund_record_id   text        NOT NULL,
    reason           text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    evidence_ids     text[]      NOT NULL CHECK (cardinality(evidence_ids) <= 20),
    reversed_by_kind text        NOT NULL CHECK (reversed_by_kind = 'human'),
    reversed_by_id   text        NOT NULL,
    reverse_epoch    bigint      NOT NULL CHECK (reverse_epoch > 0),
    reversed_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, fund_record_id),
    FOREIGN KEY (tenant_id, company_id, fund_record_id) REFERENCES brant_stl.fund_record (tenant_id, company_id, fund_record_id)
);

-- A reversal is only appended, once, by someone other than the fund record's recorder (independence row: fund.reverse
-- independentOf 资金登记人); it never changes and is never deleted.
CREATE FUNCTION brant_stl.guard_fund_reversal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_stl.fund_reversal: fund reversals never change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_stl.fund_record f WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id
            AND f.fund_record_id = NEW.fund_record_id AND f.recorded_by_id <> NEW.reversed_by_id AND f.recorded_at <= NEW.reversed_at) THEN
        RAISE EXCEPTION 'brant_stl.fund_reversal: % has no independent reversal by %', NEW.fund_record_id, NEW.reversed_by_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER fund_reversal_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.fund_reversal
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_fund_reversal();
CREATE CONSTRAINT TRIGGER fund_reversal_balance AFTER INSERT ON brant_stl.fund_reversal
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_entry_balance();

-- 000008's trigger, with reversal: a fund record is inserted recorded at version 1 against a source that stands (of the
-- right kind, currency, direction and counterparty; a conversion with the bank's arithmetic); afterwards a receipt that
-- is not reversed moves by one version per resolution of its difference (recorded or difference_resolved →
-- difference_resolved) or per verification of its payer (the state stays), and any fund record that is not reversed is
-- reversed once (→ reversed, version + 1); nothing else changes and it is never deleted.
CREATE OR REPLACE FUNCTION brant_stl.guard_fund_record() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_moving text[] := ARRAY['status', 'version', 'updated_at'];
    v_src    record;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_stl.fund_record: fund records are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'recorded' OR NEW.version <> 1 THEN
            RAISE EXCEPTION 'brant_stl.fund_record: a fund record is inserted recorded' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.source_fund_record_id IS NOT NULL THEN
            SELECT s.kind, s.direction, s.currency, s.counterparty_id, s.status INTO v_src FROM brant_stl.fund_record s
            WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id AND s.fund_record_id = NEW.source_fund_record_id;
            IF NOT FOUND OR v_src.status = 'reversed' OR NOT (CASE NEW.kind
                    WHEN 'refund' THEN v_src.kind IN ('receipt', 'non_goods') AND v_src.currency = NEW.currency
                        AND v_src.counterparty_id = NEW.counterparty_id
                    WHEN 'returned_remittance' THEN v_src.kind IN ('receipt', 'non_goods', 'payment') AND v_src.currency = NEW.currency
                        AND v_src.counterparty_id = NEW.counterparty_id AND v_src.direction <> NEW.direction
                    ELSE v_src.kind IN ('receipt', 'non_goods') AND v_src.currency = NEW.sold_currency END) THEN
                RAISE EXCEPTION 'brant_stl.fund_record: % % does not fit its source %', NEW.kind, NEW.fund_record_id, NEW.source_fund_record_id
                    USING ERRCODE = 'integrity_constraint_violation';
            END IF;
        END IF;
        IF NEW.kind = 'fx_conversion' AND pg_catalog.round(NEW.sold_minor / pg_catalog.power(10::numeric, NEW.sold_minor_units) * NEW.bank_rate,
                NEW.minor_units) * pg_catalog.power(10::numeric, NEW.minor_units) <> NEW.received_minor + NEW.fee_minor THEN
            RAISE EXCEPTION 'brant_stl.fund_record: conversion % does not add up', NEW.fund_record_id USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status <> 'reversed' AND NEW.version = OLD.version + 1
            AND (to_jsonb(NEW) - v_moving) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_moving)
            AND (NEW.status = 'reversed' OR (OLD.kind = 'receipt'
                AND (NEW.status = 'difference_resolved' OR (OLD.status = 'recorded' AND NEW.status = 'recorded')))) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.fund_record: fund record % moves only along SM-STL-FUND', OLD.fund_record_id
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;

-- 000008's balance check, with reversal and 000005's fund side: the version counts the resolutions, the verifications
-- and the reversal; a receipt that stands is difference_resolved exactly when it has a resolution; a reversed record
-- has its reversal, nothing applied from it and nothing drawn on it that stands, and an opening item of it is reversed
-- too; what applications take from the fund is their source amount when they convert; reversed draws give back what
-- they drew.
CREATE OR REPLACE FUNCTION brant_stl.check_fund_balance(p_tenant text, p_company text, p_fund text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    v_f        record;
    v_resolved numeric;
    v_count    bigint;
    v_verified bigint;
    v_reversed boolean;
    v_applied  numeric;
    v_drawn    numeric;
    v_sold     numeric;
BEGIN
    SELECT f.kind, f.status, f.version, f.received_minor, f.remitted_minor INTO v_f FROM brant_stl.fund_record f
    WHERE f.tenant_id = p_tenant AND f.company_id = p_company AND f.fund_record_id = p_fund;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    SELECT coalesce(sum(r.amount_minor), 0), count(*) INTO v_resolved, v_count FROM brant_stl.fund_difference_resolution r
    WHERE r.tenant_id = p_tenant AND r.company_id = p_company AND r.fund_record_id = p_fund;
    SELECT count(*) INTO v_verified FROM brant_stl.fund_payer_verification v
    WHERE v.tenant_id = p_tenant AND v.company_id = p_company AND v.fund_record_id = p_fund;
    v_reversed := EXISTS (SELECT 1 FROM brant_stl.fund_reversal x WHERE x.tenant_id = p_tenant AND x.company_id = p_company
        AND x.fund_record_id = p_fund);
    SELECT coalesce(sum(coalesce(a.source_amount_minor, a.amount_minor)), 0) INTO v_applied FROM brant_stl.application a
    WHERE a.tenant_id = p_tenant AND a.company_id = p_company AND a.fund_record_id = p_fund AND a.status = 'applied';
    SELECT coalesce(sum(o.received_minor) FILTER (WHERE o.kind IN ('refund', 'returned_remittance')), 0),
           coalesce(sum(o.sold_minor) FILTER (WHERE o.kind = 'fx_conversion'), 0) INTO v_drawn, v_sold
    FROM brant_stl.fund_record o WHERE o.tenant_id = p_tenant AND o.company_id = p_company AND o.source_fund_record_id = p_fund
        AND o.status <> 'reversed';
    IF v_f.version <> 1 + v_count + v_verified + (CASE WHEN v_reversed THEN 1 ELSE 0 END)
            OR (v_f.status = 'reversed') <> v_reversed
            OR (v_f.status <> 'reversed' AND (v_f.status = 'difference_resolved') <> (v_count > 0))
            OR v_resolved > v_f.remitted_minor - v_f.received_minor
            OR v_applied + v_drawn > v_f.received_minor + v_resolved OR v_drawn + v_sold > v_f.received_minor
            OR (v_reversed AND (v_applied > 0 OR v_drawn + v_sold > 0))
            OR (v_reversed AND EXISTS (SELECT 1 FROM brant_stl.opening_balance ob WHERE ob.tenant_id = p_tenant AND ob.company_id = p_company
                AND ob.fund_record_id = p_fund AND ob.status <> 'reversed')) THEN
        RAISE EXCEPTION 'brant_stl.fund_record: the balance of % does not hold (STL-INV-002, STL-INV-006, STL-INV-014)', p_fund
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM brant_stl.application a
            JOIN brant_stl.order_account_stage s ON s.tenant_id = a.tenant_id AND s.company_id = a.company_id AND s.order_id = a.order_id
                AND s.order_revision = a.order_revision AND s.stage_id = a.stage_id
            WHERE a.tenant_id = p_tenant AND a.company_id = p_company AND a.fund_record_id = p_fund
                AND s.amount_minor < (SELECT coalesce(sum(b.amount_minor), 0) FROM brant_stl.application b WHERE b.tenant_id = s.tenant_id
                    AND b.company_id = s.company_id AND b.order_id = s.order_id AND b.order_revision = s.order_revision
                    AND b.stage_id = s.stage_id AND b.status = 'applied')) THEN
        RAISE EXCEPTION 'brant_stl.application: a stage of % is applied beyond its amount (STL-INV-002)', p_fund
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM brant_stl.application a
            LEFT JOIN brant_stl.application_unapply u ON u.tenant_id = a.tenant_id AND u.company_id = a.company_id
                AND u.application_id = a.application_id
            WHERE a.tenant_id = p_tenant AND a.company_id = p_company AND a.fund_record_id = p_fund
                AND (a.status = 'unapplied') IS DISTINCT FROM (u.application_id IS NOT NULL)) THEN
        RAISE EXCEPTION 'brant_stl.application: an application of % is unapplied without its record', p_fund
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
END
$$;

-- 000007's opening item, with fund reversal: an opening prepayment's item is reversed with its fund record, as an
-- opening receivable's or payable's item is with its statement (S5R-D22).
ALTER TABLE brant_stl.opening_balance DROP CONSTRAINT opening_balance_check4;
CREATE OR REPLACE FUNCTION brant_stl.guard_opening_balance() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_ok boolean;
BEGIN
    IF TG_OP = 'INSERT' THEN
        v_ok := NEW.status = 'recorded' AND NEW.updated_at = NEW.recorded_at;
        IF v_ok AND NEW.statement_id IS NOT NULL THEN
            v_ok := EXISTS (SELECT 1 FROM brant_stl.settlement_statement s WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id
                AND s.statement_id = NEW.statement_id AND s.opening AND s.status = 'confirmed' AND s.version = 1
                AND s.kind = CASE NEW.kind WHEN 'receivable' THEN 'sales' ELSE 'purchase' END AND s.counterparty_id = NEW.counterparty_id
                AND s.currency = NEW.currency AND s.minor_units = NEW.minor_units AND s.amount_minor = NEW.amount_minor
                AND s.confirmed_by_id = NEW.recorded_by_id AND s.confirmed_at = NEW.recorded_at);
        ELSIF v_ok THEN
            v_ok := EXISTS (SELECT 1 FROM brant_stl.fund_record f WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id
                AND f.fund_record_id = NEW.fund_record_id AND f.kind = CASE NEW.kind WHEN 'customer_prepayment' THEN 'receipt' ELSE 'payment' END
                AND f.status = 'recorded' AND f.counterparty_id = NEW.counterparty_id AND f.currency = NEW.currency
                AND f.minor_units = NEW.minor_units AND f.currency_revision = NEW.currency_revision AND f.received_minor = NEW.amount_minor
                AND f.value_date <= NEW.cutoff_at AND f.recorded_by_id = NEW.recorded_by_id AND f.recorded_at = NEW.recorded_at)
                AND NOT EXISTS (SELECT 1 FROM brant_stl.application a WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id
                    AND a.fund_record_id = NEW.fund_record_id);
        END IF;
        IF NOT v_ok THEN
            RAISE EXCEPTION 'brant_stl.opening_balance: % is recorded with the opening statement or fund record its command wrote', NEW.opening_id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.status = 'recorded' AND NEW.status = 'reversed'
            AND (to_jsonb(NEW) - ARRAY['status', 'updated_at']) IS NOT DISTINCT FROM (to_jsonb(OLD) - ARRAY['status', 'updated_at'])
            AND NEW.updated_at >= OLD.updated_at
            AND (EXISTS (SELECT 1 FROM brant_stl.settlement_statement s WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id
                    AND s.statement_id = NEW.statement_id AND s.status = 'reversed')
                OR EXISTS (SELECT 1 FROM brant_stl.fund_record f WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id
                    AND f.fund_record_id = NEW.fund_record_id AND f.status = 'reversed')) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.opening_balance: an opening item is recorded once and reversed only with its statement or fund record'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;

-- S5R-D22: credit notes on opening receivables -------------------------------------------------------------------

-- A credit note binds the order of its statement: none for an opening receivable.
ALTER TABLE brant_stl.credit_note ALTER COLUMN order_id DROP NOT NULL;

-- 000005's trigger, the order of an opening statement being none.
CREATE OR REPLACE FUNCTION brant_stl.guard_credit_note() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_deciding text[] := ARRAY['status', 'version', 'decided_by_kind', 'decided_by_id', 'decide_epoch', 'decided_at', 'decision_reason',
        'updated_at'];
BEGIN
    IF TG_OP = 'INSERT' AND NEW.status = 'requested' AND NEW.version = 1 AND EXISTS (SELECT 1 FROM brant_stl.settlement_statement s
            WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id AND s.statement_id = NEW.statement_id AND s.kind = 'sales'
                AND s.status = 'confirmed' AND s.order_id IS NOT DISTINCT FROM NEW.order_id AND s.currency = NEW.currency
                AND s.minor_units = NEW.minor_units) THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.status = 'requested' AND NEW.status IN ('confirmed', 'rejected') AND NEW.version = OLD.version + 1
            AND (to_jsonb(NEW) - v_deciding) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_deciding)
            AND (NEW.status = 'rejected' OR EXISTS (SELECT 1 FROM brant_stl.settlement_statement s WHERE s.tenant_id = NEW.tenant_id
                AND s.company_id = NEW.company_id AND s.statement_id = NEW.statement_id AND s.status = 'confirmed')) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.credit_note: a credit note is requested on a confirmed sales statement and decided once (SM-STL-CREDIT-NOTE)'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;

-- S5R-D24: cost components ---------------------------------------------------------------------------------------

ALTER TABLE brant_stl.charge ADD COLUMN cost_component text NOT NULL DEFAULT 'other'
    CHECK (cost_component IN ('pre_carriage', 'export_clearance', 'main_carriage', 'insurance', 'other')),
    ADD COLUMN recharged boolean NOT NULL DEFAULT false;
ALTER TABLE brant_stl.charge ALTER COLUMN cost_component DROP DEFAULT;

-- S5R-D20: payable adjustments -----------------------------------------------------------------------------------

CREATE TABLE brant_stl.payable_adjustment (
    tenant_id           text          NOT NULL,
    company_id          text          NOT NULL,
    adjustment_id       text          NOT NULL CHECK (adjustment_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    statement_id        text          NOT NULL,
    kind                text          NOT NULL CHECK (kind IN ('price_allowance', 'under_invoiced')),
    status              text          NOT NULL CHECK (status IN ('requested', 'confirmed', 'rejected')),
    version             bigint        NOT NULL CHECK (version BETWEEN 1 AND 2),
    currency            text          NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units         smallint      NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    amount_minor        numeric(32,0) NOT NULL CHECK (amount_minor > 0),
    purchase_invoice_id text          NOT NULL CHECK (purchase_invoice_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    basis               text          NOT NULL CHECK (char_length(basis) BETWEEN 1 AND 1000),
    evidence_ids        text[]        NOT NULL CHECK (cardinality(evidence_ids) BETWEEN 1 AND 20),
    owner_actor_id      text          NOT NULL,
    owner_team_id       text,
    requested_by_kind   text          NOT NULL CHECK (requested_by_kind = 'human'),
    requested_by_id     text          NOT NULL,
    request_epoch       bigint        NOT NULL CHECK (request_epoch > 0),
    requested_at        timestamptz   NOT NULL,
    decided_by_kind     text          CHECK (decided_by_kind = 'human'),
    decided_by_id       text,
    decide_epoch        bigint        CHECK (decide_epoch > 0),
    decided_at          timestamptz,
    decision_reason     text          CHECK (char_length(decision_reason) BETWEEN 1 AND 1000),
    updated_at          timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, adjustment_id),
    FOREIGN KEY (tenant_id, company_id, statement_id) REFERENCES brant_stl.settlement_statement (tenant_id, company_id, statement_id),
    CHECK ((status = 'requested') = (version = 1)),
    CHECK ((status = 'requested') = (decided_by_id IS NULL) AND (decided_by_id IS NULL) = (decided_by_kind IS NULL)
        AND (decided_by_id IS NULL) = (decide_epoch IS NULL) AND (decided_by_id IS NULL) = (decided_at IS NULL)
        AND (decided_by_id IS NULL) = (decision_reason IS NULL)),
    CHECK (decided_by_id IS NULL OR decided_by_id <> requested_by_id),
    CHECK (owner_actor_id = requested_by_id),
    CHECK (updated_at >= requested_at AND (decided_at IS NULL OR decided_at >= requested_at))
);

CREATE INDEX payable_adjustment_statement ON brant_stl.payable_adjustment (tenant_id, company_id, statement_id);

-- An adjustment is requested on a confirmed purchase statement in its currency and decided once, confirmed only while
-- the statement stands (SM of the entity: requested → confirmed | rejected); it is never deleted.
CREATE FUNCTION brant_stl.guard_payable_adjustment() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_deciding text[] := ARRAY['status', 'version', 'decided_by_kind', 'decided_by_id', 'decide_epoch', 'decided_at', 'decision_reason',
        'updated_at'];
BEGIN
    IF TG_OP = 'INSERT' AND NEW.status = 'requested' AND NEW.version = 1 AND EXISTS (SELECT 1 FROM brant_stl.settlement_statement s
            WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id AND s.statement_id = NEW.statement_id AND s.kind = 'purchase'
                AND s.status = 'confirmed' AND s.currency = NEW.currency AND s.minor_units = NEW.minor_units) THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.status = 'requested' AND NEW.status IN ('confirmed', 'rejected') AND NEW.version = OLD.version + 1
            AND (to_jsonb(NEW) - v_deciding) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_deciding)
            AND (NEW.status = 'rejected' OR EXISTS (SELECT 1 FROM brant_stl.settlement_statement s WHERE s.tenant_id = NEW.tenant_id
                AND s.company_id = NEW.company_id AND s.statement_id = NEW.statement_id AND s.status = 'confirmed')) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.payable_adjustment: an adjustment is requested on a confirmed purchase statement and decided once (STL-INV-013)'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER payable_adjustment_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.payable_adjustment
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_payable_adjustment();
CREATE CONSTRAINT TRIGGER payable_adjustment_root AFTER INSERT OR UPDATE ON brant_stl.payable_adjustment
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_settlement_root_entry();

-- 000005's root check, with payable adjustments: the statement's version counts its credit notes' and its adjustments'
-- versions and its reversal; a payable's confirmed adjustments and standing payments stay within its amount; a reversed
-- statement has no adjustment waiting for its decision.
CREATE OR REPLACE FUNCTION brant_stl.check_settlement_root(p_tenant text, p_company text, p_statement text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    v_s           record;
    v_notes       numeric;
    v_adjustments numeric;
    v_by          text;
BEGIN
    SELECT s.status, s.version, s.confirmed_by_id, s.kind, s.amount_minor INTO v_s FROM brant_stl.settlement_statement s
    WHERE s.tenant_id = p_tenant AND s.company_id = p_company AND s.statement_id = p_statement;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    SELECT coalesce(sum(c.version), 0) INTO v_notes FROM brant_stl.credit_note c
    WHERE c.tenant_id = p_tenant AND c.company_id = p_company AND c.statement_id = p_statement;
    SELECT coalesce(sum(j.version), 0) INTO v_adjustments FROM brant_stl.payable_adjustment j
    WHERE j.tenant_id = p_tenant AND j.company_id = p_company AND j.statement_id = p_statement;
    SELECT r.reversed_by_id INTO v_by FROM brant_stl.settlement_reversal r
    WHERE r.tenant_id = p_tenant AND r.company_id = p_company AND r.statement_id = p_statement;
    IF v_s.version <> 1 + v_notes + v_adjustments + (CASE WHEN v_s.status = 'reversed' THEN 1 ELSE 0 END)
            OR (v_s.status = 'reversed') <> FOUND THEN
        RAISE EXCEPTION 'brant_stl.settlement_statement: % does not count the changes of its aggregate', p_statement
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF v_s.status = 'reversed' AND (v_by = v_s.confirmed_by_id
            OR EXISTS (SELECT 1 FROM brant_stl.settlement_line l WHERE l.tenant_id = p_tenant AND l.company_id = p_company
                AND l.statement_id = p_statement AND l.status = 'settled')
            OR EXISTS (SELECT 1 FROM brant_stl.application a WHERE a.tenant_id = p_tenant AND a.company_id = p_company
                AND a.statement_id = p_statement AND a.status = 'applied')
            OR EXISTS (SELECT 1 FROM brant_stl.credit_note c WHERE c.tenant_id = p_tenant AND c.company_id = p_company
                AND c.statement_id = p_statement AND c.status = 'requested')
            OR EXISTS (SELECT 1 FROM brant_stl.payable_adjustment j WHERE j.tenant_id = p_tenant AND j.company_id = p_company
                AND j.statement_id = p_statement AND j.status = 'requested')) THEN
        RAISE EXCEPTION 'brant_stl.settlement_statement: % is reversed with applications, settled lines or pending credit notes or adjustments, or by its confirmer (STL-INV-009)',
            p_statement USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF v_s.kind = 'purchase' AND v_s.status = 'confirmed' AND v_s.amount_minor < (SELECT coalesce(sum(a.amount_minor), 0)
                FROM brant_stl.application a WHERE a.tenant_id = p_tenant AND a.company_id = p_company AND a.statement_id = p_statement
                    AND a.status = 'applied')
            + (SELECT coalesce(sum(j.amount_minor), 0) FROM brant_stl.payable_adjustment j WHERE j.tenant_id = p_tenant
                AND j.company_id = p_company AND j.statement_id = p_statement AND j.status = 'confirmed') THEN
        RAISE EXCEPTION 'brant_stl.payable_adjustment: the payable % is paid and adjusted beyond its amount (STL-INV-013)', p_statement
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
END
$$;

-- S5R-D19: charge payments and the settlement rate of a receipt in the order's currency -----------------------------

ALTER TABLE brant_stl.application ADD COLUMN charge_id text,
    ADD COLUMN settlement_rate_id text CHECK (settlement_rate_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    ADD COLUMN settlement_rate_revision bigint CHECK (settlement_rate_revision > 0),
    ADD COLUMN settlement_rate_hash text CHECK (settlement_rate_hash ~ '^[0-9a-f]{64}$'),
    ADD COLUMN settlement_rate numeric CHECK (settlement_rate > 0),
    ADD COLUMN settlement_currency text CHECK (settlement_currency ~ '^[A-Z]{3}$'),
    ADD COLUMN settlement_minor_units smallint CHECK (settlement_minor_units BETWEEN 0 AND 4);
ALTER TABLE brant_stl.application ADD CONSTRAINT application_charge_fkey FOREIGN KEY (tenant_id, company_id, charge_id)
    REFERENCES brant_stl.charge (tenant_id, company_id, charge_id);
CREATE INDEX application_charge ON brant_stl.application (tenant_id, company_id, charge_id) WHERE charge_id IS NOT NULL;

ALTER TABLE brant_stl.application DROP CONSTRAINT application_target_kind_check;
ALTER TABLE brant_stl.application ADD CONSTRAINT application_target_kind_check CHECK (target_kind IN ('order_stage', 'receivable', 'payable',
    'charge'));
-- 000007's targets, with a charge: the charge alone.
ALTER TABLE brant_stl.application DROP CONSTRAINT application_target_check;
ALTER TABLE brant_stl.application ADD CONSTRAINT application_target_check CHECK (CASE target_kind
    WHEN 'order_stage' THEN statement_id IS NULL AND installment_seq IS NULL AND charge_id IS NULL
    WHEN 'receivable' THEN statement_id IS NOT NULL AND installment_seq IS NOT NULL AND (order_id IS NULL) = (order_revision IS NULL)
        AND (order_id IS NOT NULL OR stage_id IS NULL) AND charge_id IS NULL
    WHEN 'payable' THEN statement_id IS NOT NULL AND installment_seq IS NOT NULL AND order_id IS NULL AND order_revision IS NULL
        AND stage_id IS NULL AND charge_id IS NULL
    ELSE charge_id IS NOT NULL AND statement_id IS NULL AND installment_seq IS NULL AND order_id IS NULL AND order_revision IS NULL
        AND stage_id IS NULL END);
-- 000005's conversion: an order analysis only for an order's stage or receivable.
ALTER TABLE brant_stl.application DROP CONSTRAINT application_conversion_check;
ALTER TABLE brant_stl.application ADD CONSTRAINT application_conversion_check CHECK ((source_currency IS NULL) = (source_minor_units IS NULL)
    AND (source_currency IS NULL) = (source_amount_minor IS NULL) AND (source_currency IS NULL) = (fx_rate_id IS NULL)
    AND (source_currency IS NULL) = (fx_rate_revision IS NULL) AND (source_currency IS NULL) = (fx_rate_hash IS NULL)
    AND (source_currency IS NULL) = (fx_rate IS NULL) AND (source_currency IS NULL OR source_currency <> currency)
    AND (analysis_currency IS NULL) = (analysis_minor_units IS NULL) AND (analysis_currency IS NULL) = (analysis_rate IS NULL)
    AND (analysis_currency IS NULL OR (source_currency IS NOT NULL AND target_kind IN ('order_stage', 'receivable')))
    AND (fx_difference_minor IS NULL OR analysis_currency IS NOT NULL));
-- The settlement rate of a receipt in the order's currency: all its members or none, only for a fund in the target's
-- currency applied to an order's stage or receivable, into another currency.
ALTER TABLE brant_stl.application ADD CONSTRAINT application_settlement_basis_check CHECK ((settlement_rate_id IS NULL) = (settlement_rate_revision IS NULL)
    AND (settlement_rate_id IS NULL) = (settlement_rate_hash IS NULL) AND (settlement_rate_id IS NULL) = (settlement_rate IS NULL)
    AND (settlement_rate_id IS NULL) = (settlement_currency IS NULL) AND (settlement_rate_id IS NULL) = (settlement_minor_units IS NULL)
    AND (settlement_rate_id IS NULL OR (fund_record_id IS NOT NULL AND source_currency IS NULL AND order_id IS NOT NULL
        AND target_kind IN ('order_stage', 'receivable') AND settlement_currency <> currency)));

-- 000008's trigger, with reversed funds, charges and the settlement rate: no application takes a reversed fund; a charge
-- takes a payment of its source party, in its currency or converted, once it is confirmed; a moved application keeps the
-- settlement rate of the one it comes from; a credit note of an opening receivable binds no order.
CREATE OR REPLACE FUNCTION brant_stl.guard_application() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_moving text[] := ARRAY['status', 'version', 'updated_at'];
    v_ok     boolean;
    v_cur    text;
    v_units  smallint;
    v_value  numeric;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_stl.application: applications are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        v_cur := coalesce(NEW.source_currency, NEW.currency);
        v_units := coalesce(NEW.source_minor_units, NEW.minor_units);
        v_ok := NEW.status = 'applied' AND NEW.version = 1;
        IF v_ok AND NEW.credit_note_id IS NOT NULL THEN
            v_ok := EXISTS (SELECT 1 FROM brant_stl.credit_note c
                JOIN brant_stl.settlement_statement s ON s.tenant_id = c.tenant_id AND s.company_id = c.company_id AND s.statement_id = c.statement_id
                JOIN brant_stl.settlement_installment i ON i.tenant_id = s.tenant_id AND i.company_id = s.company_id AND i.statement_id = s.statement_id
                    AND i.seq = NEW.installment_seq
                WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id AND c.credit_note_id = NEW.credit_note_id
                    AND c.status = 'confirmed' AND c.statement_id = NEW.statement_id AND c.currency = NEW.currency AND c.minor_units = NEW.minor_units
                    AND s.kind = 'sales' AND s.status = 'confirmed' AND s.order_id IS NOT DISTINCT FROM NEW.order_id
                    AND s.order_revision IS NOT DISTINCT FROM NEW.order_revision
                    AND i.stage_id IS NOT DISTINCT FROM NEW.stage_id AND c.decided_at <= NEW.applied_at);
        ELSIF v_ok AND NEW.target_kind = 'order_stage' THEN
            v_ok := EXISTS (SELECT 1 FROM brant_stl.fund_record f
                JOIN brant_stl.order_account a ON a.tenant_id = f.tenant_id AND a.company_id = f.company_id AND a.order_id = NEW.order_id
                    AND (a.order_revision = NEW.order_revision OR (NEW.moved_from IS NOT NULL AND a.order_revision >= NEW.order_revision))
                WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id AND f.fund_record_id = NEW.fund_record_id
                    AND f.kind = 'receipt' AND f.status <> 'reversed' AND f.currency = v_cur AND f.minor_units = v_units AND a.currency = NEW.currency
                    AND brant_stl.payer_admitted(f.tenant_id, f.company_id, f.fund_record_id, NEW.payer_basis, f.counterparty_id, a.party_id)
                    AND f.recorded_at <= NEW.applied_at);
        ELSIF v_ok AND NEW.target_kind = 'receivable' THEN
            v_ok := EXISTS (SELECT 1 FROM brant_stl.fund_record f
                JOIN brant_stl.settlement_statement s ON s.tenant_id = f.tenant_id AND s.company_id = f.company_id AND s.statement_id = NEW.statement_id
                JOIN brant_stl.settlement_installment i ON i.tenant_id = s.tenant_id AND i.company_id = s.company_id AND i.statement_id = s.statement_id
                    AND i.seq = NEW.installment_seq
                WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id AND f.fund_record_id = NEW.fund_record_id
                    AND f.kind = 'receipt' AND f.status <> 'reversed' AND f.currency = v_cur AND f.minor_units = v_units AND s.kind = 'sales'
                    AND s.status = 'confirmed' AND s.currency = NEW.currency
                    AND brant_stl.payer_admitted(f.tenant_id, f.company_id, f.fund_record_id, NEW.payer_basis, f.counterparty_id, s.counterparty_id)
                    AND s.order_id IS NOT DISTINCT FROM NEW.order_id AND s.order_revision IS NOT DISTINCT FROM NEW.order_revision
                    AND i.stage_id IS NOT DISTINCT FROM NEW.stage_id AND f.recorded_at <= NEW.applied_at);
        ELSIF v_ok AND NEW.target_kind = 'payable' THEN
            v_ok := EXISTS (SELECT 1 FROM brant_stl.fund_record f
                JOIN brant_stl.settlement_statement s ON s.tenant_id = f.tenant_id AND s.company_id = f.company_id AND s.statement_id = NEW.statement_id
                WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id AND f.fund_record_id = NEW.fund_record_id
                    AND f.kind = 'payment' AND f.status <> 'reversed' AND f.currency = v_cur AND f.minor_units = v_units AND s.kind = 'purchase'
                    AND s.status = 'confirmed' AND s.currency = NEW.currency AND f.counterparty_id = s.counterparty_id
                    AND f.recorded_at <= NEW.applied_at);
        ELSIF v_ok THEN
            v_ok := EXISTS (SELECT 1 FROM brant_stl.fund_record f
                JOIN brant_stl.charge c ON c.tenant_id = f.tenant_id AND c.company_id = f.company_id AND c.charge_id = NEW.charge_id
                WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id AND f.fund_record_id = NEW.fund_record_id
                    AND f.kind = 'payment' AND f.status <> 'reversed' AND f.currency = v_cur AND f.minor_units = v_units
                    AND c.status = 'confirmed' AND c.amount_minor > 0 AND c.currency = NEW.currency AND c.minor_units = NEW.minor_units
                    AND c.source_party_id IS NOT NULL AND f.counterparty_id = c.source_party_id
                    AND f.recorded_at <= NEW.applied_at AND c.confirmed_at <= NEW.applied_at);
        END IF;
        IF v_ok AND NEW.moved_from IS NOT NULL THEN
            v_ok := NEW.target_kind IN ('order_stage', 'receivable') AND EXISTS (SELECT 1 FROM brant_stl.application m
                WHERE m.tenant_id = NEW.tenant_id AND m.company_id = NEW.company_id AND m.application_id = NEW.moved_from AND m.status = 'moved'
                    AND m.target_kind = 'order_stage' AND m.fund_record_id = NEW.fund_record_id AND m.order_id = NEW.order_id
                    AND m.stage_id = NEW.stage_id AND m.applied_at <= NEW.applied_at
                    AND m.source_currency IS NOT DISTINCT FROM NEW.source_currency AND m.fx_rate_id IS NOT DISTINCT FROM NEW.fx_rate_id
                    AND m.fx_rate_revision IS NOT DISTINCT FROM NEW.fx_rate_revision AND m.fx_rate IS NOT DISTINCT FROM NEW.fx_rate
                    AND m.analysis_currency IS NOT DISTINCT FROM NEW.analysis_currency AND m.analysis_rate IS NOT DISTINCT FROM NEW.analysis_rate
                    AND m.payer_basis = NEW.payer_basis AND m.payer_profile_revision IS NOT DISTINCT FROM NEW.payer_profile_revision
                    AND m.settlement_rate_id IS NOT DISTINCT FROM NEW.settlement_rate_id
                    AND m.settlement_rate_revision IS NOT DISTINCT FROM NEW.settlement_rate_revision
                    AND m.settlement_rate IS NOT DISTINCT FROM NEW.settlement_rate
                    AND m.settlement_currency IS NOT DISTINCT FROM NEW.settlement_currency
                    AND (NEW.target_kind = 'receivable' OR m.order_revision = NEW.order_revision));
        ELSIF v_ok AND NEW.source_currency IS NOT NULL THEN
            v_ok := pg_catalog.round(NEW.source_amount_minor / pg_catalog.power(10::numeric, NEW.source_minor_units) * NEW.fx_rate, NEW.minor_units)
                * pg_catalog.power(10::numeric, NEW.minor_units) = NEW.amount_minor;
            IF v_ok AND NEW.fx_difference_minor IS NOT NULL THEN
                v_value := pg_catalog.round(NEW.amount_minor / pg_catalog.power(10::numeric, NEW.minor_units) * NEW.analysis_rate,
                    NEW.analysis_minor_units) * pg_catalog.power(10::numeric, NEW.analysis_minor_units);
                v_ok := CASE
                    WHEN NEW.analysis_currency = NEW.source_currency THEN NEW.analysis_minor_units = NEW.source_minor_units
                        AND NEW.fx_difference_minor = NEW.source_amount_minor - v_value
                    WHEN NEW.analysis_currency = NEW.currency THEN NEW.analysis_rate = 1 AND NEW.fx_difference_minor = 0
                    ELSE false END;
            END IF;
        END IF;
        IF NOT v_ok THEN
            RAISE EXCEPTION 'brant_stl.application: % applies no fund or credit note of the target''s party (or a permitted or verified payer) in its currency, or its conversion does not add up',
                NEW.application_id USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'applied' AND (NEW.status = 'unapplied' OR (NEW.status = 'moved' AND OLD.target_kind = 'order_stage'))
            AND NEW.version = OLD.version + 1 AND (to_jsonb(NEW) - v_moving) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_moving) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.application: application % moves only along SM-STL-APPLICATION', OLD.application_id
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;

-- 000005's target check, with payables and charges: an installment takes no more than its amount — a payable's
-- confirmed adjustments counted with its payments (STL-INV-013) —, a charge no more payments than it amounts to, and the
-- rest as before.
CREATE OR REPLACE FUNCTION brant_stl.check_application_target() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.statement_id IS NOT NULL AND (SELECT i.amount_minor FROM brant_stl.settlement_installment i WHERE i.tenant_id = NEW.tenant_id
            AND i.company_id = NEW.company_id AND i.statement_id = NEW.statement_id AND i.seq = NEW.installment_seq)
        < (SELECT coalesce(sum(b.amount_minor), 0) FROM brant_stl.application b WHERE b.tenant_id = NEW.tenant_id AND b.company_id = NEW.company_id
            AND b.statement_id = NEW.statement_id AND b.installment_seq = NEW.installment_seq AND b.status = 'applied')
            + (CASE WHEN NEW.target_kind = 'payable' THEN (SELECT coalesce(sum(j.amount_minor), 0) FROM brant_stl.payable_adjustment j
                WHERE j.tenant_id = NEW.tenant_id AND j.company_id = NEW.company_id AND j.statement_id = NEW.statement_id
                    AND j.status = 'confirmed') ELSE 0 END) THEN
        RAISE EXCEPTION 'brant_stl.application: % applies installment % of % beyond its amount (STL-INV-002)', NEW.application_id,
            NEW.installment_seq, NEW.statement_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.charge_id IS NOT NULL AND (SELECT c.amount_minor FROM brant_stl.charge c WHERE c.tenant_id = NEW.tenant_id
            AND c.company_id = NEW.company_id AND c.charge_id = NEW.charge_id)
        < (SELECT coalesce(sum(b.amount_minor), 0) FROM brant_stl.application b WHERE b.tenant_id = NEW.tenant_id AND b.company_id = NEW.company_id
            AND b.charge_id = NEW.charge_id AND b.status = 'applied') THEN
        RAISE EXCEPTION 'brant_stl.application: % pays charge % beyond its amount (STL-INV-002, S5R-D19)', NEW.application_id, NEW.charge_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status = 'moved' AND EXISTS (SELECT 1 FROM (SELECT sum(r.amount_minor) AS amount, sum(r.source_amount_minor) AS source,
                sum(r.fx_difference_minor) AS difference FROM brant_stl.application r
            WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.moved_from = NEW.application_id) x
            WHERE x.amount IS DISTINCT FROM NEW.amount_minor OR x.source IS DISTINCT FROM NEW.source_amount_minor
                OR x.difference IS DISTINCT FROM NEW.fx_difference_minor) THEN
        RAISE EXCEPTION 'brant_stl.application: moved application % is not replaced exactly', NEW.application_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.credit_note_id IS NOT NULL AND ((SELECT c.amount_minor FROM brant_stl.credit_note c WHERE c.tenant_id = NEW.tenant_id
                AND c.company_id = NEW.company_id AND c.credit_note_id = NEW.credit_note_id)
            < (SELECT coalesce(sum(b.amount_minor), 0) FROM brant_stl.application b WHERE b.tenant_id = NEW.tenant_id
                AND b.company_id = NEW.company_id AND b.credit_note_id = NEW.credit_note_id AND b.status = 'applied')
            OR (NEW.status = 'unapplied') IS DISTINCT FROM EXISTS (SELECT 1 FROM brant_stl.application_unapply u WHERE u.tenant_id = NEW.tenant_id
                AND u.company_id = NEW.company_id AND u.application_id = NEW.application_id)) THEN
        RAISE EXCEPTION 'brant_stl.application: credit note % is applied beyond its amount, or % is unapplied without its record (STL-INV-008)',
            NEW.credit_note_id, NEW.application_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;

-- S5R-D42: the result of a cancelled order -----------------------------------------------------------------------

ALTER TABLE brant_stl.order_result_snapshot ADD COLUMN cancelled boolean NOT NULL DEFAULT false;

-- Row-level security ---------------------------------------------------------------------------------------------

ALTER TABLE brant_stl.fund_reversal ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.fund_reversal FORCE ROW LEVEL SECURITY;
CREATE POLICY fund_reversal_scope ON brant_stl.fund_reversal
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.payable_adjustment ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.payable_adjustment FORCE ROW LEVEL SECURITY;
CREATE POLICY payable_adjustment_scope ON brant_stl.payable_adjustment
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
