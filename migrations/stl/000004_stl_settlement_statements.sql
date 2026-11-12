-- STL: settlement statements, business receivables and payables, and applications to them (tactical
-- model STL-AGG-SettlementStatement, STL-AGG-Application; SM-STL-SETTLEMENT, SM-STL-APPLICATION;
-- STL-INV-001, STL-INV-002, STL-INV-003; UL-INV-22, UL-INV-23; Context Map R30 ShipmentLineRef v1,
-- R31 PurchaseSettlementSource v1; baseline 4.2, 4.3, 4.6 S-A; WBS S5-04b; S5 plan D-3, D-4).
-- Python V0.50 has no STL.
--
-- settlement.confirm (human only) confirms a statement: a sales statement settles confirmed FUL
-- shipment lines of one order and forms a business receivable; a purchase statement settles PUR's
-- verified supplier delivery lines of one supplier and forms a business payable. A source line is
-- settled once while its statement line stands (partial unique index; settlement.reverse, unit C8,
-- moves a statement and its lines to reversed and so restores the source). A receivable falls due in
-- installments that follow the order's payment stages: the order's settled amount fills the stages
-- of the order account's revision in plan order, and each installment is due by its stage's trigger
-- and days (after the goods left, after the statement); a payable is one installment.
--
-- Applications now also target a receivable installment or a payable (target_kind receivable,
-- payable). When a sales statement is confirmed the order's prepayments move to the receivable (S5
-- D-4): a standing stage application of a stage the statement settles is marked moved and replaced by
-- an application of the same fund and stage to the receivable's installment — and, when only part of
-- it fits, by a remainder application on the stage. A moved application counts nowhere; the
-- replacements count for their fund and, because a receivable application keeps its stage, for that
-- stage, so PaymentConditionGuard v1 and the order account read the same as before the move, and
-- what is paid on a receivable after shipment still counts for a document-controlled stage (D-5).
-- Balances are never stored (D-3): open = installment − Σ standing applications on it.
--
-- PaymentConditionGuard v1 is replaced once more (same signature, 000003's body): a stage counts at
-- most its due in the scope's applied total (defect found by unit C1).
--
-- References to FUL (shipments) and PUR (deliveries, commitments) are plain IDs and versions; no
-- foreign key crosses a schema (DB2). Nothing is deleted.

CREATE TABLE brant_stl.settlement_statement (
    tenant_id       text          NOT NULL,
    company_id      text          NOT NULL,
    statement_id    text          NOT NULL CHECK (statement_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    kind            text          NOT NULL CHECK (kind IN ('sales', 'purchase')),
    status          text          NOT NULL CHECK (status IN ('confirmed', 'reversed')),
    version         bigint        NOT NULL CHECK (version >= 1),
    counterparty_id text          NOT NULL CHECK (counterparty_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_id        text,
    order_revision  bigint        CHECK (order_revision > 0),
    order_hash      text          CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    shipped_at      timestamptz,
    currency        text          NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units     smallint      NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    net_minor       numeric(32,0) NOT NULL CHECK (net_minor >= 0),
    tax_minor       numeric(32,0) NOT NULL CHECK (tax_minor >= 0),
    amount_minor    numeric(32,0) NOT NULL CHECK (amount_minor > 0),
    evidence_ids    text[]        NOT NULL CHECK (cardinality(evidence_ids) <= 20),
    owner_actor_id  text          NOT NULL,
    owner_team_id   text,
    confirmed_by_kind text        NOT NULL CHECK (confirmed_by_kind = 'human'),
    confirmed_by_id text          NOT NULL,
    confirm_epoch   bigint        NOT NULL CHECK (confirm_epoch > 0),
    confirmed_at    timestamptz   NOT NULL,
    updated_at      timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, statement_id),
    FOREIGN KEY (tenant_id, company_id, order_id) REFERENCES brant_stl.order_account (tenant_id, company_id, order_id),
    CHECK ((kind = 'sales') = (order_id IS NOT NULL) AND (order_id IS NULL) = (order_revision IS NULL)
        AND (order_id IS NULL) = (order_hash IS NULL) AND (order_id IS NULL) = (shipped_at IS NULL)),
    CHECK (amount_minor = net_minor + tax_minor),
    CHECK ((status = 'confirmed') = (version = 1)),
    CHECK (owner_actor_id = confirmed_by_id),
    CHECK (updated_at >= confirmed_at)
);

CREATE INDEX settlement_statement_order ON brant_stl.settlement_statement (tenant_id, company_id, order_id) WHERE order_id IS NOT NULL;
CREATE INDEX settlement_statement_party ON brant_stl.settlement_statement (tenant_id, company_id, counterparty_id, kind);

-- A statement is inserted confirmed at version 1 and does not change here (SM-STL-SETTLEMENT: its
-- reversal is unit C8's settlement.reverse, which replaces this function); applications lock it FOR
-- UPDATE without changing it (S5 D-3). It is never deleted.
CREATE FUNCTION brant_stl.guard_settlement_statement() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' AND NEW.status = 'confirmed' AND NEW.version = 1 THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.settlement_statement: a statement is confirmed once and moves only along SM-STL-SETTLEMENT'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER settlement_statement_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.settlement_statement
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_settlement_statement();

-- A statement line is one settled source line: a confirmed FUL shipment line (sales: shipment ID,
-- line ID, the shipment's row version) or a verified PUR supplier delivery line (purchase: delivery
-- ID, the order line its commitment line buys, the verified version), with the order binding, order
-- line, SKU specification and unit it is of, the quantity settled, the basis it is valued from (the
-- order line's or commitment line's quantity and amounts) and its amounts.
CREATE TABLE brant_stl.settlement_line (
    tenant_id       text          NOT NULL,
    company_id      text          NOT NULL,
    statement_id    text          NOT NULL,
    seq             integer       NOT NULL CHECK (seq >= 1),
    source_kind     text          NOT NULL CHECK (source_kind IN ('shipment_line', 'supplier_delivery_line')),
    source_id       text          NOT NULL CHECK (source_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    source_line_id  text          NOT NULL CHECK (source_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    source_version  bigint        NOT NULL CHECK (source_version > 0),
    order_id        text          NOT NULL,
    order_revision  bigint        NOT NULL CHECK (order_revision > 0),
    order_hash      text          NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    order_line_id   text          NOT NULL,
    commitment_id   text,
    supplier_id     text,
    sku_id          text          NOT NULL,
    spec_revision   bigint        NOT NULL CHECK (spec_revision > 0),
    unit_id         text          NOT NULL,
    unit_revision   bigint        NOT NULL CHECK (unit_revision > 0),
    quantity        numeric(24,6) NOT NULL CHECK (quantity > 0),
    basis_quantity  numeric(24,6) NOT NULL CHECK (basis_quantity > 0),
    unit_price      text,
    basis_net_minor numeric(32,0) NOT NULL CHECK (basis_net_minor >= 0),
    basis_tax_minor numeric(32,0) NOT NULL CHECK (basis_tax_minor >= 0),
    net_minor       numeric(32,0) NOT NULL CHECK (net_minor >= 0),
    tax_minor       numeric(32,0) NOT NULL CHECK (tax_minor >= 0),
    amount_minor    numeric(32,0) NOT NULL CHECK (amount_minor >= 0),
    occurred_at     timestamptz   NOT NULL,
    status          text          NOT NULL CHECK (status IN ('settled', 'reversed')),
    PRIMARY KEY (tenant_id, company_id, statement_id, seq),
    FOREIGN KEY (tenant_id, company_id, statement_id) REFERENCES brant_stl.settlement_statement (tenant_id, company_id, statement_id),
    CHECK (quantity <= basis_quantity),
    CHECK (amount_minor = net_minor + tax_minor),
    CHECK (source_kind <> 'supplier_delivery_line' OR (commitment_id IS NOT NULL AND supplier_id IS NOT NULL))
);

-- STL-INV-001 (UL-INV-23): a source line is settled in at most one standing statement line; of two
-- statements settling it at once, the second waits for the first and fails.
CREATE UNIQUE INDEX settlement_line_source ON brant_stl.settlement_line (tenant_id, company_id, source_kind, source_id, source_line_id)
    WHERE status = 'settled';
CREATE INDEX settlement_line_order ON brant_stl.settlement_line (tenant_id, company_id, order_id);

-- Lines are inserted settled with their statement and do not change here (C8's reversal replaces
-- this function to move them to reversed with their statement); they are never deleted.
CREATE FUNCTION brant_stl.guard_settlement_line() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' AND NEW.status = 'settled' AND EXISTS (SELECT 1 FROM brant_stl.settlement_statement s
            WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id AND s.statement_id = NEW.statement_id AND s.status = 'confirmed'
                AND (s.kind = 'sales') = (NEW.source_kind = 'shipment_line') AND (s.kind = 'purchase' OR s.order_id = NEW.order_id)
                AND (s.kind = 'sales' OR s.counterparty_id = NEW.supplier_id)) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.settlement_line: lines are inserted with their confirmed statement and never change here'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER settlement_line_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.settlement_line
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_settlement_line();

-- An installment is part of a statement's amount falling due at due_at: for a receivable, the part of
-- one payment stage of the order account's revision (stage, trigger, days, release control), or the
-- part beyond the payment plan (no stage); for a payable, the whole amount (no stage).
CREATE TABLE brant_stl.settlement_installment (
    tenant_id       text          NOT NULL,
    company_id      text          NOT NULL,
    statement_id    text          NOT NULL,
    seq             integer       NOT NULL CHECK (seq >= 1),
    order_id        text,
    order_revision  bigint,
    stage_id        text,
    stage_trigger   text          CHECK (stage_trigger IN ('order_confirmed', 'before_shipment', 'after_shipment', 'after_invoice')),
    days            integer       CHECK (days >= 0),
    release_control text          CHECK (release_control IN ('', 'documents')),
    amount_minor    numeric(32,0) NOT NULL CHECK (amount_minor > 0),
    due_at          timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, statement_id, seq),
    UNIQUE (tenant_id, company_id, statement_id, stage_id),
    FOREIGN KEY (tenant_id, company_id, statement_id) REFERENCES brant_stl.settlement_statement (tenant_id, company_id, statement_id),
    FOREIGN KEY (tenant_id, company_id, order_id, order_revision, stage_id)
        REFERENCES brant_stl.order_account_stage (tenant_id, company_id, order_id, order_revision, stage_id),
    CHECK ((stage_id IS NULL) = (stage_trigger IS NULL) AND (stage_id IS NULL) = (days IS NULL) AND (stage_id IS NULL) = (release_control IS NULL)
        AND (stage_id IS NULL) = (order_revision IS NULL)),
    CHECK (stage_id IS NULL OR order_id IS NOT NULL)
);

-- Installments are inserted with their statement and never change.
CREATE FUNCTION brant_stl.guard_settlement_installment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' AND EXISTS (SELECT 1 FROM brant_stl.settlement_statement s WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id
            AND s.statement_id = NEW.statement_id AND s.status = 'confirmed' AND s.order_id IS NOT DISTINCT FROM NEW.order_id
            AND (NEW.order_revision IS NULL OR NEW.order_revision = s.order_revision)) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.settlement_installment: installments are inserted with their statement and never change'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER settlement_installment_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.settlement_installment
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_settlement_installment();

-- At commit: a statement's lines and installments each add up to its amounts (net, tax, total), and
-- a receivable's installments of a stage, over every confirmed statement of the order, stay within
-- that stage's amount (the plan is filled once).
CREATE FUNCTION brant_stl.check_settlement_statement() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_lines record;
    v_due   numeric;
BEGIN
    SELECT count(*) AS n, coalesce(sum(l.net_minor), 0) AS net, coalesce(sum(l.tax_minor), 0) AS tax, coalesce(sum(l.amount_minor), 0) AS amount
    INTO v_lines FROM brant_stl.settlement_line l
    WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id AND l.statement_id = NEW.statement_id;
    SELECT coalesce(sum(i.amount_minor), 0) INTO v_due FROM brant_stl.settlement_installment i
    WHERE i.tenant_id = NEW.tenant_id AND i.company_id = NEW.company_id AND i.statement_id = NEW.statement_id;
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
CREATE CONSTRAINT TRIGGER settlement_statement_total AFTER INSERT ON brant_stl.settlement_statement
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_settlement_statement();

-- Applications: target kinds receivable (an installment of a confirmed sales statement, keeping the
-- order, the account revision and the stage it is due for) and payable (the installment of a
-- confirmed purchase statement); state moved (a stage application replaced at a statement's
-- confirmation); moved_from names the application a replacement or remainder comes from.
ALTER TABLE brant_stl.application DROP CONSTRAINT application_target_kind_check;
ALTER TABLE brant_stl.application ADD CONSTRAINT application_target_kind_check CHECK (target_kind IN ('order_stage', 'receivable', 'payable'));
ALTER TABLE brant_stl.application DROP CONSTRAINT application_status_check;
ALTER TABLE brant_stl.application ADD CONSTRAINT application_status_check CHECK (status IN ('applied', 'unapplied', 'moved'));
ALTER TABLE brant_stl.application ADD COLUMN statement_id text, ADD COLUMN installment_seq integer, ADD COLUMN moved_from text;
ALTER TABLE brant_stl.application ADD CONSTRAINT application_installment_fkey FOREIGN KEY (tenant_id, company_id, statement_id, installment_seq)
    REFERENCES brant_stl.settlement_installment (tenant_id, company_id, statement_id, seq);
ALTER TABLE brant_stl.application ADD CONSTRAINT application_moved_from_fkey FOREIGN KEY (tenant_id, company_id, moved_from)
    REFERENCES brant_stl.application (tenant_id, company_id, application_id);
ALTER TABLE brant_stl.application ADD CONSTRAINT application_target_check CHECK (CASE target_kind
    WHEN 'order_stage' THEN statement_id IS NULL AND installment_seq IS NULL
    WHEN 'receivable' THEN statement_id IS NOT NULL AND installment_seq IS NOT NULL AND order_id IS NOT NULL AND order_revision IS NOT NULL
    ELSE statement_id IS NOT NULL AND installment_seq IS NOT NULL AND order_id IS NULL AND order_revision IS NULL AND stage_id IS NULL END);
ALTER TABLE brant_stl.application ADD CONSTRAINT application_moved_check CHECK (status <> 'moved' OR target_kind = 'order_stage');

CREATE INDEX application_statement ON brant_stl.application (tenant_id, company_id, statement_id, installment_seq) WHERE statement_id IS NOT NULL;
CREATE INDEX application_moved_from ON brant_stl.application (tenant_id, company_id, moved_from) WHERE moved_from IS NOT NULL;

-- An application is inserted applied at version 1 (STL-INV-002, STL-INV-006): to an order stage, from
-- a receipt of the order's customer in the order's currency at the account's revision (a remainder
-- of a moved application keeps the revision it was made at); to a receivable, from a receipt of the
-- statement's customer in its currency, for the statement's order, account revision and the
-- installment's stage; to a payable, from a payment to the statement's supplier in its currency. A
-- replacement or remainder names a moved application of the same fund and stage. Afterwards it moves
-- once (SM-STL-APPLICATION): applied → unapplied, or — a stage application at a statement's
-- confirmation — applied → moved. It is never deleted.
CREATE OR REPLACE FUNCTION brant_stl.guard_application() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_moving text[] := ARRAY['status', 'version', 'updated_at'];
    v_ok     boolean;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_stl.application: applications are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        v_ok := NEW.status = 'applied' AND NEW.version = 1;
        IF v_ok AND NEW.target_kind = 'order_stage' THEN
            v_ok := EXISTS (SELECT 1 FROM brant_stl.fund_record f
                JOIN brant_stl.order_account a ON a.tenant_id = f.tenant_id AND a.company_id = f.company_id AND a.order_id = NEW.order_id
                    AND (a.order_revision = NEW.order_revision OR (NEW.moved_from IS NOT NULL AND a.order_revision >= NEW.order_revision))
                WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id AND f.fund_record_id = NEW.fund_record_id
                    AND f.kind = 'receipt' AND f.currency = NEW.currency AND f.minor_units = NEW.minor_units AND a.currency = NEW.currency
                    AND f.counterparty_id = a.party_id AND f.recorded_at <= NEW.applied_at);
        ELSIF v_ok AND NEW.target_kind = 'receivable' THEN
            v_ok := EXISTS (SELECT 1 FROM brant_stl.fund_record f
                JOIN brant_stl.settlement_statement s ON s.tenant_id = f.tenant_id AND s.company_id = f.company_id AND s.statement_id = NEW.statement_id
                JOIN brant_stl.settlement_installment i ON i.tenant_id = s.tenant_id AND i.company_id = s.company_id AND i.statement_id = s.statement_id
                    AND i.seq = NEW.installment_seq
                WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id AND f.fund_record_id = NEW.fund_record_id
                    AND f.kind = 'receipt' AND f.currency = NEW.currency AND f.minor_units = NEW.minor_units AND s.kind = 'sales'
                    AND s.status = 'confirmed' AND s.currency = NEW.currency AND f.counterparty_id = s.counterparty_id AND s.order_id = NEW.order_id
                    AND s.order_revision = NEW.order_revision AND i.stage_id IS NOT DISTINCT FROM NEW.stage_id AND f.recorded_at <= NEW.applied_at);
        ELSIF v_ok THEN
            v_ok := EXISTS (SELECT 1 FROM brant_stl.fund_record f
                JOIN brant_stl.settlement_statement s ON s.tenant_id = f.tenant_id AND s.company_id = f.company_id AND s.statement_id = NEW.statement_id
                WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id AND f.fund_record_id = NEW.fund_record_id
                    AND f.kind = 'payment' AND f.currency = NEW.currency AND f.minor_units = NEW.minor_units AND s.kind = 'purchase'
                    AND s.status = 'confirmed' AND s.currency = NEW.currency AND f.counterparty_id = s.counterparty_id
                    AND f.recorded_at <= NEW.applied_at);
        END IF;
        IF v_ok AND NEW.moved_from IS NOT NULL THEN
            v_ok := NEW.target_kind IN ('order_stage', 'receivable') AND EXISTS (SELECT 1 FROM brant_stl.application m
                WHERE m.tenant_id = NEW.tenant_id AND m.company_id = NEW.company_id AND m.application_id = NEW.moved_from AND m.status = 'moved'
                    AND m.target_kind = 'order_stage' AND m.fund_record_id = NEW.fund_record_id AND m.order_id = NEW.order_id
                    AND m.stage_id = NEW.stage_id AND m.applied_at <= NEW.applied_at
                    AND (NEW.target_kind = 'receivable' OR m.order_revision = NEW.order_revision));
        END IF;
        IF NOT v_ok THEN
            RAISE EXCEPTION 'brant_stl.application: % applies no fund of the target''s party in its currency', NEW.application_id
                USING ERRCODE = 'integrity_constraint_violation';
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

-- At commit, for every application a command wrote: what stands on a receivable installment or a
-- payable stays within its amount (STL-INV-002), and a moved application is replaced exactly — its
-- replacements and remainder add up to its amount.
CREATE FUNCTION brant_stl.check_application_target() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.statement_id IS NOT NULL AND (SELECT i.amount_minor FROM brant_stl.settlement_installment i WHERE i.tenant_id = NEW.tenant_id
            AND i.company_id = NEW.company_id AND i.statement_id = NEW.statement_id AND i.seq = NEW.installment_seq)
        < (SELECT coalesce(sum(b.amount_minor), 0) FROM brant_stl.application b WHERE b.tenant_id = NEW.tenant_id AND b.company_id = NEW.company_id
            AND b.statement_id = NEW.statement_id AND b.installment_seq = NEW.installment_seq AND b.status = 'applied') THEN
        RAISE EXCEPTION 'brant_stl.application: % applies installment % of % beyond its amount (STL-INV-002)', NEW.application_id,
            NEW.installment_seq, NEW.statement_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status = 'moved' AND NEW.amount_minor IS DISTINCT FROM (SELECT sum(r.amount_minor) FROM brant_stl.application r
            WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.moved_from = NEW.application_id) THEN
        RAISE EXCEPTION 'brant_stl.application: moved application % is not replaced exactly', NEW.application_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER application_target AFTER INSERT OR UPDATE ON brant_stl.application
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_application_target();

-- PaymentConditionGuard v1 (R45), same signature and result as in 000002 and 000003, built on
-- 000003's text (C4b-STL: applications made at the account's revision or an earlier one count for
-- the stage of the same ID — receivable applications included, since they keep their stage). Fix
-- found by unit C1: a stage counts at most its due in applied_minor. A payment plan review can shrink
-- a stage below what stands on it; counted in full, the over-application hid another stage's
-- shortfall in the scope's totals, and FUL's exception rule (one unpaid stage ⇔ scope shortfall =
-- stage shortfall) misjudged it.
CREATE OR REPLACE FUNCTION brant_stl.guard_payment_condition_v1(p_order_id text, p_order_revision bigint, p_scope text, p_at timestamptz)
    RETURNS TABLE (status text, reason text, order_revision bigint, order_hash text, currency text, required_minor numeric,
                   applied_minor numeric, stage_id text, stage_trigger text, stage_due_minor numeric, stage_applied_minor numeric,
                   shortfall_minor numeric, checked_at timestamptz, code text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant   text := pg_catalog.current_setting('app.tenant_id', true);
    v_company  text := pg_catalog.current_setting('app.company_id', true);
    v_a        record;
    v_s        record;
    v_required numeric := 0;
    v_applied  numeric := 0;
    v_first    record;
    v_found    boolean := false;
BEGIN
    IF p_order_id IS NULL OR p_order_revision IS NULL OR p_scope IS NULL OR p_at IS NULL THEN
        RAISE EXCEPTION 'brant_stl.guard_payment_condition_v1: order_id, order_revision, scope and at are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    IF p_scope NOT IN ('shipment', 'documents') THEN
        RAISE EXCEPTION 'brant_stl.guard_payment_condition_v1: scope is shipment or documents' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    SELECT a.order_revision, a.order_hash, a.currency INTO v_a FROM brant_stl.order_account a
    WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.order_id = p_order_id
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT 'blocked'::text, 'no_account'::text, NULL::bigint, NULL::text, NULL::text, NULL::numeric, NULL::numeric, NULL::text,
            NULL::text, NULL::numeric, NULL::numeric, NULL::numeric, p_at, 'COMMERCIAL_RELEASE_BLOCKED'::text;
        RETURN;
    END IF;
    IF v_a.order_revision <> p_order_revision THEN
        RETURN QUERY SELECT 'blocked'::text, 'revision_mismatch'::text, v_a.order_revision, v_a.order_hash, v_a.currency, NULL::numeric,
            NULL::numeric, NULL::text, NULL::text, NULL::numeric, NULL::numeric, NULL::numeric, p_at, 'COMMERCIAL_RELEASE_BLOCKED'::text;
        RETURN;
    END IF;
    FOR v_s IN
        SELECT s.stage_id, s.stage_trigger, s.amount_minor,
            (SELECT coalesce(sum(x.amount_minor), 0) FROM brant_stl.application x WHERE x.tenant_id = s.tenant_id AND x.company_id = s.company_id
                AND x.order_id = s.order_id AND x.order_revision <= s.order_revision AND x.stage_id = s.stage_id AND x.status = 'applied') AS applied
        FROM brant_stl.order_account_stage s
        WHERE s.tenant_id = v_tenant AND s.company_id = v_company AND s.order_id = p_order_id AND s.order_revision = v_a.order_revision
            AND (s.stage_trigger IN ('order_confirmed', 'before_shipment')
                OR (p_scope = 'documents' AND s.stage_trigger IN ('after_shipment', 'after_invoice') AND s.release_control = 'documents'))
        ORDER BY s.seq
    LOOP
        v_required := v_required + v_s.amount_minor;
        v_applied := v_applied + least(v_s.applied, v_s.amount_minor);
        IF NOT v_found AND v_s.applied < v_s.amount_minor THEN
            v_first := v_s;
            v_found := true;
        END IF;
    END LOOP;
    IF v_found THEN
        RETURN QUERY SELECT 'blocked'::text, 'stage_unpaid'::text, v_a.order_revision, v_a.order_hash, v_a.currency, v_required, v_applied,
            v_first.stage_id::text, v_first.stage_trigger::text, v_first.amount_minor::numeric, v_first.applied::numeric,
            (v_first.amount_minor - v_first.applied)::numeric, p_at, 'COMMERCIAL_RELEASE_BLOCKED'::text;
        RETURN;
    END IF;
    RETURN QUERY SELECT 'satisfied'::text, ''::text, v_a.order_revision, v_a.order_hash, v_a.currency, v_required, v_applied, NULL::text,
        NULL::text, NULL::numeric, NULL::numeric, NULL::numeric, p_at, NULL::text;
END
$$;

REVOKE ALL ON FUNCTION brant_stl.guard_payment_condition_v1(text, bigint, text, timestamptz) FROM PUBLIC;

ALTER TABLE brant_stl.settlement_statement ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.settlement_statement FORCE ROW LEVEL SECURITY;
CREATE POLICY settlement_statement_scope ON brant_stl.settlement_statement
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.settlement_line ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.settlement_line FORCE ROW LEVEL SECURITY;
CREATE POLICY settlement_line_scope ON brant_stl.settlement_line
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.settlement_installment ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.settlement_installment FORCE ROW LEVEL SECURITY;
CREATE POLICY settlement_installment_scope ON brant_stl.settlement_installment
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
