-- STL: credit notes, settlement reversal and cross-currency applications (tactical model
-- STL-AGG-SettlementStatement entities CreditNote and SettlementReversal, STL-AGG-Application;
-- SM-STL-CREDIT-NOTE, SM-STL-SETTLEMENT; STL-INV-007, STL-INV-008, STL-INV-009; UL-INV-22, UL STL-CreditNote;
-- Context Map R33 (POL fx_rate of purpose settlement, ConfigGuard); MD-10; baseline 4.2, 4.3, 4.5, 4.7 S-B;
-- WBS S5-18; S5 plan D-3, D-4). Python V0.50 has no STL.
--
-- A credit note is part of the sales statement it names (a price allowance, a return or a claim): requested by a
-- sales clerk, then confirmed or rejected by a credit manager who did not request it. Requesting and deciding it
-- move the statement root by one version, as settlement.reverse does: the root counts every change of its
-- entities. A confirmed credit note is a source of applications to that statement's receivable — never more than
-- it amounts to, never more than is open — and changes nothing else: no shipped quantity, no refund, no tax-refund
-- material (STL-INV-008).
--
-- settlement.reverse moves a statement confirmed → reversed (terminal) when nothing stands applied on it, by a
-- person other than its confirmer, and its lines settled → reversed, which frees their sources for a new statement
-- (the source index counts settled lines only). The statement, its lines and the reversal record stay
-- (STL-INV-009, baseline 4.2).
--
-- An application's source is a fund record or a confirmed credit note. A fund in another currency than the
-- target's is converted at a POL fx_rate of purpose settlement for exactly that direction (STL-INV-007, MD-10):
-- the application keeps the source amount (what the fund gives), the target amount (what the target receives:
-- source × rate, HALF_UP at the target's minor units — amount_minor, in the target's currency as before), the rate
-- revision and value, and, for an order's target, the order's analysis rate and the business settlement FX
-- difference against it in the analysis currency (baseline 4.5; never a receipt or revenue). The fund side counts
-- source amounts, the target side target amounts. Balances are never stored (D-3).

-- Credit notes ---------------------------------------------------------------------------------------------------

CREATE TABLE brant_stl.credit_note (
    tenant_id         text          NOT NULL,
    company_id        text          NOT NULL,
    credit_note_id    text          NOT NULL CHECK (credit_note_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    statement_id      text          NOT NULL,
    order_id          text          NOT NULL,
    kind              text          NOT NULL CHECK (kind IN ('price_allowance', 'return', 'claim')),
    status            text          NOT NULL CHECK (status IN ('requested', 'confirmed', 'rejected')),
    version           bigint        NOT NULL CHECK (version BETWEEN 1 AND 2),
    currency          text          NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units       smallint      NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    amount_minor      numeric(32,0) NOT NULL CHECK (amount_minor > 0),
    basis             text          NOT NULL CHECK (char_length(basis) BETWEEN 1 AND 1000),
    evidence_ids      text[]        NOT NULL CHECK (cardinality(evidence_ids) BETWEEN 1 AND 20),
    owner_actor_id    text          NOT NULL,
    owner_team_id     text,
    requested_by_kind text          NOT NULL CHECK (requested_by_kind IN ('human', 'ai')),
    requested_by_id   text          NOT NULL,
    request_epoch     bigint        NOT NULL CHECK (request_epoch > 0),
    requested_at      timestamptz   NOT NULL,
    decided_by_kind   text          CHECK (decided_by_kind = 'human'),
    decided_by_id     text,
    decide_epoch      bigint        CHECK (decide_epoch > 0),
    decided_at        timestamptz,
    decision_reason   text          CHECK (char_length(decision_reason) BETWEEN 1 AND 1000),
    updated_at        timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, credit_note_id),
    FOREIGN KEY (tenant_id, company_id, statement_id) REFERENCES brant_stl.settlement_statement (tenant_id, company_id, statement_id),
    CHECK ((status = 'requested') = (version = 1)),
    CHECK ((status = 'requested') = (decided_by_id IS NULL) AND (decided_by_id IS NULL) = (decided_by_kind IS NULL)
        AND (decided_by_id IS NULL) = (decide_epoch IS NULL) AND (decided_by_id IS NULL) = (decided_at IS NULL)
        AND (decided_by_id IS NULL) = (decision_reason IS NULL)),
    CHECK (decided_by_id IS NULL OR decided_by_id <> requested_by_id),
    CHECK (owner_actor_id = requested_by_id),
    CHECK (updated_at >= requested_at AND (decided_at IS NULL OR decided_at >= requested_at))
);

CREATE INDEX credit_note_statement ON brant_stl.credit_note (tenant_id, company_id, statement_id);
CREATE INDEX credit_note_order ON brant_stl.credit_note (tenant_id, company_id, order_id);

-- A credit note is inserted requested at version 1 against a confirmed sales statement of its order, in the
-- statement's currency; it is decided once, requested → confirmed (the statement still confirmed) or rejected, by
-- a person other than the requester (SM-STL-CREDIT-NOTE; the CHECK above); nothing else changes; it is never
-- deleted.
CREATE FUNCTION brant_stl.guard_credit_note() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_deciding text[] := ARRAY['status', 'version', 'decided_by_kind', 'decided_by_id', 'decide_epoch', 'decided_at', 'decision_reason',
        'updated_at'];
BEGIN
    IF TG_OP = 'INSERT' AND NEW.status = 'requested' AND NEW.version = 1 AND EXISTS (SELECT 1 FROM brant_stl.settlement_statement s
            WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id AND s.statement_id = NEW.statement_id AND s.kind = 'sales'
                AND s.status = 'confirmed' AND s.order_id = NEW.order_id AND s.currency = NEW.currency AND s.minor_units = NEW.minor_units) THEN
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
CREATE TRIGGER credit_note_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.credit_note
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_credit_note();

-- Settlement reversal -------------------------------------------------------------------------------------------

CREATE TABLE brant_stl.settlement_reversal (
    tenant_id        text        NOT NULL,
    company_id       text        NOT NULL,
    statement_id     text        NOT NULL,
    reason           text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    evidence_ids     text[]      NOT NULL CHECK (cardinality(evidence_ids) <= 20),
    reversed_by_kind text        NOT NULL CHECK (reversed_by_kind = 'human'),
    reversed_by_id   text        NOT NULL,
    reverse_epoch    bigint      NOT NULL CHECK (reverse_epoch > 0),
    reversed_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, statement_id),
    FOREIGN KEY (tenant_id, company_id, statement_id) REFERENCES brant_stl.settlement_statement (tenant_id, company_id, statement_id)
);

-- A reversal is only appended, once per statement; it never changes and is never deleted.
CREATE FUNCTION brant_stl.guard_settlement_reversal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_stl.settlement_reversal: reversals never change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER settlement_reversal_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.settlement_reversal
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_settlement_reversal();

-- The statement root moves by one version per change of the aggregate: a credit note requested or decided keeps
-- it confirmed, settlement.reverse makes it reversed (SM-STL-SETTLEMENT, terminal). Its own facts never change.
ALTER TABLE brant_stl.settlement_statement DROP CONSTRAINT settlement_statement_check2;
ALTER TABLE brant_stl.settlement_statement ADD CONSTRAINT settlement_statement_version_status CHECK (status = 'confirmed' OR version >= 2);

CREATE OR REPLACE FUNCTION brant_stl.guard_settlement_statement() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_moving text[] := ARRAY['status', 'version', 'updated_at'];
BEGIN
    IF TG_OP = 'INSERT' AND NEW.status = 'confirmed' AND NEW.version = 1 THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.status = 'confirmed' AND NEW.status IN ('confirmed', 'reversed') AND NEW.version = OLD.version + 1
            AND (to_jsonb(NEW) - v_moving) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_moving) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.settlement_statement: a statement is confirmed once and moves only along SM-STL-SETTLEMENT'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;

-- A line moves settled → reversed with its reversed statement (STL-INV-009), nothing else; its source is then free.
CREATE OR REPLACE FUNCTION brant_stl.guard_settlement_line() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' AND NEW.status = 'settled' AND EXISTS (SELECT 1 FROM brant_stl.settlement_statement s
            WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id AND s.statement_id = NEW.statement_id AND s.status = 'confirmed'
                AND (s.kind = 'sales') = (NEW.source_kind = 'shipment_line') AND (s.kind = 'purchase' OR s.order_id = NEW.order_id)
                AND (s.kind = 'sales' OR s.counterparty_id = NEW.supplier_id)) THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.status = 'settled' AND NEW.status = 'reversed'
            AND (to_jsonb(NEW) - 'status'::text) IS NOT DISTINCT FROM (to_jsonb(OLD) - 'status'::text)
            AND EXISTS (SELECT 1 FROM brant_stl.settlement_statement s WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id
                AND s.statement_id = NEW.statement_id AND s.status = 'reversed') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.settlement_line: lines are inserted with their confirmed statement and reversed only with it'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;

-- At commit, for every statement whose aggregate a command changed: its version counts its changes (1 + the
-- versions of its credit notes + 1 when reversed); it is reversed exactly when its reversal exists; a reversed
-- statement was reversed by someone other than its confirmer, has no settled line, nothing standing applied on
-- it and no credit note waiting for a decision (STL-INV-009).
CREATE FUNCTION brant_stl.check_settlement_root(p_tenant text, p_company text, p_statement text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    v_s     record;
    v_notes numeric;
    v_by    text;
BEGIN
    SELECT s.status, s.version, s.confirmed_by_id INTO v_s FROM brant_stl.settlement_statement s
    WHERE s.tenant_id = p_tenant AND s.company_id = p_company AND s.statement_id = p_statement;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    SELECT coalesce(sum(c.version), 0) INTO v_notes FROM brant_stl.credit_note c
    WHERE c.tenant_id = p_tenant AND c.company_id = p_company AND c.statement_id = p_statement;
    SELECT r.reversed_by_id INTO v_by FROM brant_stl.settlement_reversal r
    WHERE r.tenant_id = p_tenant AND r.company_id = p_company AND r.statement_id = p_statement;
    IF v_s.version <> 1 + v_notes + (CASE WHEN v_s.status = 'reversed' THEN 1 ELSE 0 END)
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
                AND c.statement_id = p_statement AND c.status = 'requested')) THEN
        RAISE EXCEPTION 'brant_stl.settlement_statement: % is reversed with applications, settled lines or pending credit notes, or by its confirmer (STL-INV-009)',
            p_statement USING ERRCODE = 'integrity_constraint_violation';
    END IF;
END
$$;

CREATE FUNCTION brant_stl.check_settlement_root_entry() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM brant_stl.check_settlement_root(NEW.tenant_id, NEW.company_id, NEW.statement_id);
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER settlement_statement_root AFTER UPDATE ON brant_stl.settlement_statement
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_settlement_root_entry();
CREATE CONSTRAINT TRIGGER credit_note_root AFTER INSERT OR UPDATE ON brant_stl.credit_note
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_settlement_root_entry();
CREATE CONSTRAINT TRIGGER settlement_reversal_root AFTER INSERT ON brant_stl.settlement_reversal
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_settlement_root_entry();

-- Applications: sources and conversions ----------------------------------------------------------------------------

ALTER TABLE brant_stl.application ALTER COLUMN fund_record_id DROP NOT NULL;
ALTER TABLE brant_stl.application
    ADD COLUMN credit_note_id       text,
    ADD COLUMN source_currency      text          CHECK (source_currency ~ '^[A-Z]{3}$'),
    ADD COLUMN source_minor_units   smallint      CHECK (source_minor_units BETWEEN 0 AND 4),
    ADD COLUMN source_amount_minor  numeric(32,0) CHECK (source_amount_minor > 0),
    ADD COLUMN fx_rate_id           text          CHECK (fx_rate_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    ADD COLUMN fx_rate_revision     bigint        CHECK (fx_rate_revision > 0),
    ADD COLUMN fx_rate_hash         text          CHECK (fx_rate_hash ~ '^[0-9a-f]{64}$'),
    ADD COLUMN fx_rate              numeric       CHECK (fx_rate > 0),
    ADD COLUMN analysis_currency    text          CHECK (analysis_currency ~ '^[A-Z]{3}$'),
    ADD COLUMN analysis_minor_units smallint      CHECK (analysis_minor_units BETWEEN 0 AND 4),
    ADD COLUMN analysis_rate        numeric       CHECK (analysis_rate > 0),
    ADD COLUMN fx_difference_minor  numeric(32,0);
ALTER TABLE brant_stl.application ADD CONSTRAINT application_credit_note_fkey FOREIGN KEY (tenant_id, company_id, credit_note_id)
    REFERENCES brant_stl.credit_note (tenant_id, company_id, credit_note_id);
-- One source: a fund record, or a confirmed credit note applied to its statement's receivable in its currency.
ALTER TABLE brant_stl.application ADD CONSTRAINT application_source_check CHECK ((fund_record_id IS NULL) <> (credit_note_id IS NULL)
    AND (credit_note_id IS NULL OR (target_kind = 'receivable' AND source_currency IS NULL AND moved_from IS NULL)));
-- A conversion is whole (source, rate) and in another currency; the analysis and the difference only with it.
ALTER TABLE brant_stl.application ADD CONSTRAINT application_conversion_check CHECK ((source_currency IS NULL) = (source_minor_units IS NULL)
    AND (source_currency IS NULL) = (source_amount_minor IS NULL) AND (source_currency IS NULL) = (fx_rate_id IS NULL)
    AND (source_currency IS NULL) = (fx_rate_revision IS NULL) AND (source_currency IS NULL) = (fx_rate_hash IS NULL)
    AND (source_currency IS NULL) = (fx_rate IS NULL) AND (source_currency IS NULL OR source_currency <> currency)
    AND (analysis_currency IS NULL) = (analysis_minor_units IS NULL) AND (analysis_currency IS NULL) = (analysis_rate IS NULL)
    AND (analysis_currency IS NULL OR (source_currency IS NOT NULL AND target_kind <> 'payable'))
    AND (fx_difference_minor IS NULL OR analysis_currency IS NOT NULL));

CREATE INDEX application_credit_note ON brant_stl.application (tenant_id, company_id, credit_note_id) WHERE credit_note_id IS NOT NULL;

-- 000004's guard with the sources: a fund in the target's currency or — converted — in source_currency, whose
-- original (not a replacement or remainder) adds up: target = source × rate HALF_UP at the target's minor units,
-- and the difference = the source (analysis currency = source currency) or the target (= target currency, rate 1)
-- less the target at the analysis rate, HALF_UP at the analysis currency's minor units; or a confirmed credit
-- note of the receivable's statement in its currency. A replacement or remainder keeps its moved application's
-- fund, stage, source currency and rate.
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
                    AND s.kind = 'sales' AND s.status = 'confirmed' AND s.order_id = NEW.order_id AND s.order_revision = NEW.order_revision
                    AND i.stage_id IS NOT DISTINCT FROM NEW.stage_id AND c.decided_at <= NEW.applied_at);
        ELSIF v_ok AND NEW.target_kind = 'order_stage' THEN
            v_ok := EXISTS (SELECT 1 FROM brant_stl.fund_record f
                JOIN brant_stl.order_account a ON a.tenant_id = f.tenant_id AND a.company_id = f.company_id AND a.order_id = NEW.order_id
                    AND (a.order_revision = NEW.order_revision OR (NEW.moved_from IS NOT NULL AND a.order_revision >= NEW.order_revision))
                WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id AND f.fund_record_id = NEW.fund_record_id
                    AND f.kind = 'receipt' AND f.currency = v_cur AND f.minor_units = v_units AND a.currency = NEW.currency
                    AND f.counterparty_id = a.party_id AND f.recorded_at <= NEW.applied_at);
        ELSIF v_ok AND NEW.target_kind = 'receivable' THEN
            v_ok := EXISTS (SELECT 1 FROM brant_stl.fund_record f
                JOIN brant_stl.settlement_statement s ON s.tenant_id = f.tenant_id AND s.company_id = f.company_id AND s.statement_id = NEW.statement_id
                JOIN brant_stl.settlement_installment i ON i.tenant_id = s.tenant_id AND i.company_id = s.company_id AND i.statement_id = s.statement_id
                    AND i.seq = NEW.installment_seq
                WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id AND f.fund_record_id = NEW.fund_record_id
                    AND f.kind = 'receipt' AND f.currency = v_cur AND f.minor_units = v_units AND s.kind = 'sales'
                    AND s.status = 'confirmed' AND s.currency = NEW.currency AND f.counterparty_id = s.counterparty_id AND s.order_id = NEW.order_id
                    AND s.order_revision = NEW.order_revision AND i.stage_id IS NOT DISTINCT FROM NEW.stage_id AND f.recorded_at <= NEW.applied_at);
        ELSIF v_ok THEN
            v_ok := EXISTS (SELECT 1 FROM brant_stl.fund_record f
                JOIN brant_stl.settlement_statement s ON s.tenant_id = f.tenant_id AND s.company_id = f.company_id AND s.statement_id = NEW.statement_id
                WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id AND f.fund_record_id = NEW.fund_record_id
                    AND f.kind = 'payment' AND f.currency = v_cur AND f.minor_units = v_units AND s.kind = 'purchase'
                    AND s.status = 'confirmed' AND s.currency = NEW.currency AND f.counterparty_id = s.counterparty_id
                    AND f.recorded_at <= NEW.applied_at);
        END IF;
        IF v_ok AND NEW.moved_from IS NOT NULL THEN
            v_ok := NEW.target_kind IN ('order_stage', 'receivable') AND EXISTS (SELECT 1 FROM brant_stl.application m
                WHERE m.tenant_id = NEW.tenant_id AND m.company_id = NEW.company_id AND m.application_id = NEW.moved_from AND m.status = 'moved'
                    AND m.target_kind = 'order_stage' AND m.fund_record_id = NEW.fund_record_id AND m.order_id = NEW.order_id
                    AND m.stage_id = NEW.stage_id AND m.applied_at <= NEW.applied_at
                    AND m.source_currency IS NOT DISTINCT FROM NEW.source_currency AND m.fx_rate_id IS NOT DISTINCT FROM NEW.fx_rate_id
                    AND m.fx_rate_revision IS NOT DISTINCT FROM NEW.fx_rate_revision AND m.fx_rate IS NOT DISTINCT FROM NEW.fx_rate
                    AND m.analysis_currency IS NOT DISTINCT FROM NEW.analysis_currency AND m.analysis_rate IS NOT DISTINCT FROM NEW.analysis_rate
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
            RAISE EXCEPTION 'brant_stl.application: % applies no fund or credit note of the target''s party in its currency, or its conversion does not add up',
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

-- 000002's fund balance, the fund side counting what each application takes from the fund: its source amount when
-- it converts, else its amount.
CREATE OR REPLACE FUNCTION brant_stl.check_fund_balance(p_tenant text, p_company text, p_fund text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    v_f        record;
    v_resolved numeric;
    v_count    bigint;
    v_applied  numeric;
    v_drawn    numeric;
    v_sold     numeric;
BEGIN
    SELECT f.kind, f.version, f.received_minor, f.remitted_minor INTO v_f FROM brant_stl.fund_record f
    WHERE f.tenant_id = p_tenant AND f.company_id = p_company AND f.fund_record_id = p_fund;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    SELECT coalesce(sum(r.amount_minor), 0), count(*) INTO v_resolved, v_count FROM brant_stl.fund_difference_resolution r
    WHERE r.tenant_id = p_tenant AND r.company_id = p_company AND r.fund_record_id = p_fund;
    SELECT coalesce(sum(coalesce(a.source_amount_minor, a.amount_minor)), 0) INTO v_applied FROM brant_stl.application a
    WHERE a.tenant_id = p_tenant AND a.company_id = p_company AND a.fund_record_id = p_fund AND a.status = 'applied';
    SELECT coalesce(sum(o.received_minor) FILTER (WHERE o.kind IN ('refund', 'returned_remittance')), 0),
           coalesce(sum(o.sold_minor) FILTER (WHERE o.kind = 'fx_conversion'), 0) INTO v_drawn, v_sold
    FROM brant_stl.fund_record o WHERE o.tenant_id = p_tenant AND o.company_id = p_company AND o.source_fund_record_id = p_fund;
    IF v_f.version <> 1 + v_count OR v_resolved > v_f.remitted_minor - v_f.received_minor
            OR v_applied + v_drawn > v_f.received_minor + v_resolved OR v_drawn + v_sold > v_f.received_minor THEN
        RAISE EXCEPTION 'brant_stl.fund_record: the balance of % does not hold (STL-INV-002, STL-INV-006)', p_fund
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

-- 000004's target check, extended: a moved application is replaced exactly in target amount, source amount and
-- difference; what stands applied from a credit note stays within its amount, and a credit note's application is
-- unapplied exactly when its unapply exists (STL-INV-002, STL-INV-008).
CREATE OR REPLACE FUNCTION brant_stl.check_application_target() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.statement_id IS NOT NULL AND (SELECT i.amount_minor FROM brant_stl.settlement_installment i WHERE i.tenant_id = NEW.tenant_id
            AND i.company_id = NEW.company_id AND i.statement_id = NEW.statement_id AND i.seq = NEW.installment_seq)
        < (SELECT coalesce(sum(b.amount_minor), 0) FROM brant_stl.application b WHERE b.tenant_id = NEW.tenant_id AND b.company_id = NEW.company_id
            AND b.statement_id = NEW.statement_id AND b.installment_seq = NEW.installment_seq AND b.status = 'applied') THEN
        RAISE EXCEPTION 'brant_stl.application: % applies installment % of % beyond its amount (STL-INV-002)', NEW.application_id,
            NEW.installment_seq, NEW.statement_id USING ERRCODE = 'integrity_constraint_violation';
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

ALTER TABLE brant_stl.credit_note ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.credit_note FORCE ROW LEVEL SECURITY;
CREATE POLICY credit_note_scope ON brant_stl.credit_note
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.settlement_reversal ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.settlement_reversal FORCE ROW LEVEL SECURITY;
CREATE POLICY settlement_reversal_scope ON brant_stl.settlement_reversal
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
