-- STL: opening balances (tactical model STL-AGG-SettlementStatement entity OpeningBalance, value object Cutoff;
-- STL-INV-012; Context Map MD-50, R75 期初来源; baseline 11.2 期初、防重、切换; BT17, CN-AT16 STL half; WBS S5-20;
-- S5 plan D-3, D-4). Python V0.50 has no STL.
--
-- opening_balance.record (human only, treasury) registers what the books held at a cutoff instant, from a source
-- document with EVD evidence, as one item: an open receivable or payable of a party (an opening settlement
-- statement — a sales or purchase statement marked opening, without lines: no shipment or delivery source, and
-- without a Brant order; the legacy order is a stated reference) or an unapplied prepayment received from a
-- customer or paid to a supplier (an opening fund record: a receipt or a payment on the company's bank account,
-- with the bank's reference, which no later fund.record can record again). The item keeps the source document's
-- amount and the amount open (unapplied) at the cutoff; what the document's amount exceeds it by was settled
-- before the cutoff. Applications against opening statements and opening fund records work as for any other.
--
-- A source document stands once per kind (STL-INV-012 "同一截止与来源只登记一次，上线后不重复生成同一账款或现金"): a
-- second record of it — at the same cutoff or another — is OPENING_BALANCE_DUPLICATE (partial unique index, the
-- command checks first). Reversing an opening statement (settlement.reverse, unit C8) moves its opening item to
-- reversed, so a corrected item of the same document can be recorded; an opening fund record is never reversed.
--
-- An opening receivable counts in its customer's credit exposure as an open receivable (D-4; it forms no order's
-- credit stage) and its installments' due instants feed the overdue tolerance. Nothing is deleted.

-- Opening statements ---------------------------------------------------------------------------------------------

ALTER TABLE brant_stl.settlement_statement ADD COLUMN opening boolean NOT NULL DEFAULT false;
ALTER TABLE brant_stl.settlement_statement DROP CONSTRAINT settlement_statement_check;
-- 000004's binding rule, with opening statements: an opening statement binds no order, no revision and no
-- shipment (its order is a stated reference of the opening item).
ALTER TABLE brant_stl.settlement_statement ADD CONSTRAINT settlement_statement_binding CHECK (CASE WHEN opening
    THEN order_id IS NULL AND order_revision IS NULL AND order_hash IS NULL AND shipped_at IS NULL AND tax_minor = 0
    ELSE (kind = 'sales') = (order_id IS NOT NULL) AND (order_id IS NULL) = (order_revision IS NULL)
        AND (order_id IS NULL) = (order_hash IS NULL) AND (order_id IS NULL) = (shipped_at IS NULL) END);

CREATE TABLE brant_stl.opening_balance (
    tenant_id           text          NOT NULL,
    company_id          text          NOT NULL,
    opening_id          text          NOT NULL CHECK (opening_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    kind                text          NOT NULL CHECK (kind IN ('receivable', 'payable', 'customer_prepayment', 'supplier_prepayment')),
    status              text          NOT NULL CHECK (status IN ('recorded', 'reversed')),
    cutoff_at           timestamptz   NOT NULL,
    source_document     text          NOT NULL CHECK (source_document ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,63}$'),
    source_issued_at    timestamptz   NOT NULL,
    source_amount_minor numeric(32,0) NOT NULL,
    amount_minor        numeric(32,0) NOT NULL CHECK (amount_minor > 0),
    currency            text          NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units         smallint      NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    currency_revision   bigint        NOT NULL CHECK (currency_revision > 0),
    counterparty_id     text          NOT NULL CHECK (counterparty_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_reference     text          CHECK (order_reference ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,63}$'),
    statement_id        text,
    fund_record_id      text,
    evidence_ids        text[]        NOT NULL CHECK (cardinality(evidence_ids) BETWEEN 1 AND 20),
    owner_actor_id      text          NOT NULL,
    owner_team_id       text,
    recorded_by_kind    text          NOT NULL CHECK (recorded_by_kind = 'human'),
    recorded_by_id      text          NOT NULL,
    record_epoch        bigint        NOT NULL CHECK (record_epoch > 0),
    recorded_at         timestamptz   NOT NULL,
    updated_at          timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, opening_id),
    FOREIGN KEY (tenant_id, company_id, statement_id) REFERENCES brant_stl.settlement_statement (tenant_id, company_id, statement_id),
    FOREIGN KEY (tenant_id, company_id, fund_record_id) REFERENCES brant_stl.fund_record (tenant_id, company_id, fund_record_id),
    CHECK ((kind IN ('receivable', 'payable')) = (statement_id IS NOT NULL) AND (statement_id IS NULL) <> (fund_record_id IS NULL)),
    CHECK (opening_id = coalesce(statement_id, fund_record_id)),
    CHECK (source_amount_minor >= amount_minor),
    CHECK (source_issued_at <= cutoff_at AND cutoff_at <= recorded_at),
    CHECK (status = 'recorded' OR statement_id IS NOT NULL),
    CHECK (owner_actor_id = recorded_by_id),
    CHECK (updated_at >= recorded_at)
);

-- STL-INV-012: a source document stands once per kind, whatever the cutoff (BT17: no second receivable, payable
-- or cash for an item recorded before); of two records at once, the second waits for the first and fails.
CREATE UNIQUE INDEX opening_balance_source ON brant_stl.opening_balance (tenant_id, company_id, kind, source_document)
    WHERE status = 'recorded';
CREATE INDEX opening_balance_cutoff ON brant_stl.opening_balance (tenant_id, company_id, cutoff_at);
CREATE UNIQUE INDEX opening_balance_statement ON brant_stl.opening_balance (tenant_id, company_id, statement_id) WHERE statement_id IS NOT NULL;
CREATE UNIQUE INDEX opening_balance_fund ON brant_stl.opening_balance (tenant_id, company_id, fund_record_id) WHERE fund_record_id IS NOT NULL;

-- An opening item is inserted recorded with the statement or fund record the same command wrote (same recorder,
-- same instant): an opening statement of the item's kind, party, currency and amount, confirmed at version 1; or
-- a receipt from the customer (a customer prepayment) or a payment to the supplier (a supplier prepayment) in its
-- currency revision, received the amount, its value date not after the cutoff, nothing applied yet. Afterwards it
-- only moves recorded → reversed when its statement is reversed (settlement.reverse). It is never deleted.
CREATE FUNCTION brant_stl.guard_opening_balance() RETURNS trigger LANGUAGE plpgsql AS $$
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
            AND EXISTS (SELECT 1 FROM brant_stl.settlement_statement s WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id
                AND s.statement_id = NEW.statement_id AND s.status = 'reversed') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.opening_balance: an opening item is recorded once and reversed only with its statement'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER opening_balance_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.opening_balance
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_opening_balance();

-- 000004's statement total, with opening statements: an opening statement has no line, its installments add up to
-- its amount (net = amount, tax 0: an open balance is not split), and its opening item states the same kind,
-- party, currency and amount. Other statements as before.
CREATE OR REPLACE FUNCTION brant_stl.check_settlement_statement() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_lines record;
    v_due   numeric;
BEGIN
    SELECT count(*) AS n, coalesce(sum(l.net_minor), 0) AS net, coalesce(sum(l.tax_minor), 0) AS tax, coalesce(sum(l.amount_minor), 0) AS amount
    INTO v_lines FROM brant_stl.settlement_line l
    WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id AND l.statement_id = NEW.statement_id;
    SELECT coalesce(sum(i.amount_minor), 0) INTO v_due FROM brant_stl.settlement_installment i
    WHERE i.tenant_id = NEW.tenant_id AND i.company_id = NEW.company_id AND i.statement_id = NEW.statement_id;
    IF NEW.opening THEN
        IF v_lines.n <> 0 OR v_due <> NEW.amount_minor OR NEW.net_minor <> NEW.amount_minor OR NOT EXISTS (SELECT 1 FROM brant_stl.opening_balance o
                WHERE o.tenant_id = NEW.tenant_id AND o.company_id = NEW.company_id AND o.statement_id = NEW.statement_id
                    AND o.kind = CASE NEW.kind WHEN 'sales' THEN 'receivable' ELSE 'payable' END AND o.counterparty_id = NEW.counterparty_id
                    AND o.currency = NEW.currency AND o.amount_minor = NEW.amount_minor) THEN
            RAISE EXCEPTION 'brant_stl.settlement_statement: opening statement % does not add up to its opening item and installments',
                NEW.statement_id USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NULL;
    END IF;
    IF v_lines.n = 0 OR v_lines.net <> NEW.net_minor OR v_lines.tax <> NEW.tax_minor OR v_lines.amount <> NEW.amount_minor
            OR v_due <> NEW.amount_minor THEN
        RAISE EXCEPTION 'brant_stl.settlement_statement: % does not add up to its lines and installments', NEW.statement_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM brant_stl.settlement_installment i
            JOIN brant_stl.order_account_stage s ON s.tenant_id = i.tenant_id AND s.company_id = i.company_id AND s.order_id = i.order_id
                AND s.order_revision = i.order_revision AND s.stage_id = i.stage_id
            WHERE i.tenant_id = NEW.tenant_id AND i.company_id = NEW.company_id AND i.statement_id = NEW.statement_id
                AND s.amount_minor < (SELECT coalesce(sum(o.amount_minor), 0) FROM brant_stl.settlement_installment o
                    JOIN brant_stl.settlement_statement t ON t.tenant_id = o.tenant_id AND t.company_id = o.company_id AND t.statement_id = o.statement_id
                    WHERE o.tenant_id = i.tenant_id AND o.company_id = i.company_id AND o.order_id = i.order_id AND o.stage_id = i.stage_id
                        AND t.status = 'confirmed')) THEN
        RAISE EXCEPTION 'brant_stl.settlement_statement: % settles a payment stage beyond its amount', NEW.statement_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;

-- 000005's line guard, with opening statements: an opening statement takes no line (it settles no shipment or
-- delivery line; STL-INV-001's sources stay free).
CREATE OR REPLACE FUNCTION brant_stl.guard_settlement_line() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' AND NEW.status = 'settled' AND EXISTS (SELECT 1 FROM brant_stl.settlement_statement s
            WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id AND s.statement_id = NEW.statement_id AND s.status = 'confirmed'
                AND NOT s.opening
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

-- Applications to opening receivables ------------------------------------------------------------------------------

-- 000004's target rule, with opening receivables: an application to a receivable keeps its statement's order and
-- account revision — none for an opening statement, which has no stage either.
ALTER TABLE brant_stl.application DROP CONSTRAINT application_target_check;
ALTER TABLE brant_stl.application ADD CONSTRAINT application_target_check CHECK (CASE target_kind
    WHEN 'order_stage' THEN statement_id IS NULL AND installment_seq IS NULL
    WHEN 'receivable' THEN statement_id IS NOT NULL AND installment_seq IS NOT NULL AND (order_id IS NULL) = (order_revision IS NULL)
        AND (order_id IS NOT NULL OR stage_id IS NULL)
    ELSE statement_id IS NOT NULL AND installment_seq IS NOT NULL AND order_id IS NULL AND order_revision IS NULL AND stage_id IS NULL END);

-- 000005's guard, with opening receivables: a receivable application keeps its statement's order and revision,
-- none for an opening statement (IS NOT DISTINCT FROM; every other statement binds both). Nothing else changes.
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
                    AND s.status = 'confirmed' AND s.currency = NEW.currency AND f.counterparty_id = s.counterparty_id
                    AND s.order_id IS NOT DISTINCT FROM NEW.order_id AND s.order_revision IS NOT DISTINCT FROM NEW.order_revision
                    AND i.stage_id IS NOT DISTINCT FROM NEW.stage_id AND f.recorded_at <= NEW.applied_at);
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

ALTER TABLE brant_stl.opening_balance ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.opening_balance FORCE ROW LEVEL SECURITY;
CREATE POLICY opening_balance_scope ON brant_stl.opening_balance
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
