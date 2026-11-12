-- STL: fund records, applications, order accounts and PaymentConditionGuard v1 (tactical model
-- STL-AGG-FundRecord, STL-AGG-Application, STL-AGG-OrderAccount; SM-STL-FUND, SM-STL-APPLICATION;
-- STL-INV-002, STL-INV-003, STL-INV-006; Context Map R45 PaymentConditionGuard v1, R38 FundRecordRef
-- v1; WBS S5-04a and the STL half of S5-07; S5 plan D-3, D-4, D-5, D-16). Python V0.50 has no STL.
--
-- A fund record is a verified movement of money on one of the company's bank accounts (POL
-- bank_account), recorded by a person with bank evidence: a receipt, a non-goods receipt, a payment,
-- a refund or a returned remittance (both against an earlier fund record), or an FX conversion (a
-- transfer into another currency from an earlier receipt, never a receipt itself). The customer's
-- remitted amount, the bank's original-currency credit and the stated bank charges are kept apart;
-- the difference between remitted and received stays unexplained until an independent person
-- resolves part or all of it as a bank charge or a discount borne by us (only appended).
--
-- An order account is one root per order: the payment stages of the order revision it was opened at
-- (TRD OrderCommercialSnapshot v1, R32). An application allocates part of a receipt's available
-- balance to an order stage; unapplying it writes a new record and keeps the fund fact. Balances
-- are never stored (S5 D-3): available = received + resolved − applied − refunded or returned;
-- open = stage amount − applied. application.apply and application.unapply lock the fund record and
-- the order account FOR UPDATE in ID order, so two applications on one balance commit one after the
-- other and the second sees the first (STL-INV-002). Deferred checks below hold the balances at
-- every commit whatever the application does.
--
-- brant_stl.guard_payment_condition_v1 publishes PaymentConditionGuard v1 (R45, guardLock): FUL calls
-- it in its own transaction; it locks the order account FOR SHARE, so an application or an unapply
-- of the order either committed before and the verdict says so, or waits until FUL commits.
--
-- References to TRD (orders), PTY (counterparties), EVD (evidence) and POL (currency, bank_account)
-- are plain IDs and revisions: no foreign key crosses a schema (DB2). Nothing is deleted.

CREATE TABLE brant_stl.fund_record (
    tenant_id             text          NOT NULL,
    company_id            text          NOT NULL,
    fund_record_id        text          NOT NULL CHECK (fund_record_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    kind                  text          NOT NULL CHECK (kind IN ('receipt', 'non_goods', 'payment', 'refund', 'returned_remittance',
                                            'fx_conversion')),
    direction             text          NOT NULL CHECK (direction IN ('in', 'out')),
    status                text          NOT NULL CHECK (status IN ('recorded', 'difference_resolved')),
    version               bigint        NOT NULL CHECK (version >= 1),
    counterparty_id       text          CHECK (counterparty_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    currency              text          NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units           smallint      NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    currency_revision     bigint        NOT NULL CHECK (currency_revision > 0),
    bank_account_id       text          NOT NULL CHECK (bank_account_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    bank_account_revision bigint        NOT NULL CHECK (bank_account_revision > 0),
    received_minor        numeric(32,0) NOT NULL CHECK (received_minor > 0),
    remitted_minor        numeric(32,0) NOT NULL,
    bank_charges_minor    numeric(32,0) NOT NULL CHECK (bank_charges_minor >= 0),
    value_date            timestamptz   NOT NULL,
    bank_reference        text          NOT NULL CHECK (bank_reference ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,63}$'),
    evidence_ids          text[]        NOT NULL CHECK (cardinality(evidence_ids) BETWEEN 1 AND 20),
    source_fund_record_id text,
    sold_currency         text          CHECK (sold_currency ~ '^[A-Z]{3}$'),
    sold_minor_units      smallint      CHECK (sold_minor_units BETWEEN 0 AND 4),
    sold_minor            numeric(32,0) CHECK (sold_minor > 0),
    bank_rate             numeric       CHECK (bank_rate > 0),
    fee_minor             numeric(32,0) CHECK (fee_minor >= 0),
    owner_actor_id        text          NOT NULL,
    owner_team_id         text,
    recorded_by_kind      text          NOT NULL CHECK (recorded_by_kind = 'human'),
    recorded_by_id        text          NOT NULL,
    record_epoch          bigint        NOT NULL CHECK (record_epoch > 0),
    recorded_at           timestamptz   NOT NULL,
    updated_at            timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, fund_record_id),
    UNIQUE (tenant_id, company_id, bank_account_id, bank_reference),
    FOREIGN KEY (tenant_id, company_id, source_fund_record_id) REFERENCES brant_stl.fund_record (tenant_id, company_id, fund_record_id),
    CHECK (remitted_minor >= received_minor AND bank_charges_minor <= remitted_minor - received_minor),
    CHECK (kind = 'receipt' OR (remitted_minor = received_minor AND bank_charges_minor = 0)),
    CHECK ((source_fund_record_id IS NOT NULL) = (kind IN ('refund', 'returned_remittance', 'fx_conversion'))),
    CHECK ((counterparty_id IS NULL) = (kind = 'fx_conversion')),
    CHECK ((kind = 'fx_conversion') = (sold_minor IS NOT NULL) AND (sold_minor IS NULL) = (sold_currency IS NULL)
        AND (sold_minor IS NULL) = (sold_minor_units IS NULL) AND (sold_minor IS NULL) = (bank_rate IS NULL)
        AND (sold_minor IS NULL) = (fee_minor IS NULL)),
    CHECK (sold_currency IS NULL OR sold_currency <> currency),
    CHECK (CASE WHEN kind IN ('receipt', 'non_goods', 'fx_conversion') THEN direction = 'in'
        WHEN kind IN ('payment', 'refund') THEN direction = 'out' ELSE true END),
    CHECK ((status = 'recorded') = (version = 1) AND (status = 'recorded' OR kind = 'receipt')),
    CHECK (owner_actor_id = recorded_by_id),
    CHECK (updated_at >= recorded_at)
);

CREATE INDEX fund_record_source ON brant_stl.fund_record (tenant_id, company_id, source_fund_record_id) WHERE source_fund_record_id IS NOT NULL;

-- A fund record is inserted recorded at version 1, against a source of the right kind, currency,
-- direction and counterparty, and for a conversion with the bank's arithmetic (received + fee =
-- sold × rate, HALF_UP to the target's minor units); afterwards only a receipt's state and version
-- move, by one per resolution of its difference (SM-STL-FUND); it is never deleted.
CREATE FUNCTION brant_stl.guard_fund_record() RETURNS trigger LANGUAGE plpgsql AS $$
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
            SELECT s.kind, s.direction, s.currency, s.counterparty_id INTO v_src FROM brant_stl.fund_record s
            WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id AND s.fund_record_id = NEW.source_fund_record_id;
            IF NOT FOUND OR NOT (CASE NEW.kind
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
    IF OLD.kind = 'receipt' AND NEW.status = 'difference_resolved' AND NEW.version = OLD.version + 1
            AND (to_jsonb(NEW) - v_moving) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_moving) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.fund_record: fund record % moves only along SM-STL-FUND', OLD.fund_record_id
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER fund_record_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.fund_record
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_fund_record();

CREATE TABLE brant_stl.fund_difference_resolution (
    tenant_id        text          NOT NULL,
    company_id       text          NOT NULL,
    fund_record_id   text          NOT NULL,
    seq              integer       NOT NULL CHECK (seq >= 1),
    resolution       text          NOT NULL CHECK (resolution IN ('bank_charge', 'discount')),
    amount_minor     numeric(32,0) NOT NULL CHECK (amount_minor > 0),
    reason           text          NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    evidence_ids     text[]        NOT NULL CHECK (cardinality(evidence_ids) BETWEEN 1 AND 20),
    resolved_by_kind text          NOT NULL CHECK (resolved_by_kind = 'human'),
    resolved_by_id   text          NOT NULL,
    resolve_epoch    bigint        NOT NULL CHECK (resolve_epoch > 0),
    resolved_at      timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, fund_record_id, seq),
    FOREIGN KEY (tenant_id, company_id, fund_record_id) REFERENCES brant_stl.fund_record (tenant_id, company_id, fund_record_id)
);

-- A resolution is only appended, to a receipt, by someone other than its recorder (independence
-- row: fund.resolve_difference independentOf 资金登记人); it never changes and is never deleted.
CREATE FUNCTION brant_stl.guard_fund_difference_resolution() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_stl.fund_difference_resolution: resolutions never change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_stl.fund_record f WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id
            AND f.fund_record_id = NEW.fund_record_id AND f.kind = 'receipt' AND f.recorded_by_id <> NEW.resolved_by_id
            AND f.recorded_at <= NEW.resolved_at) THEN
        RAISE EXCEPTION 'brant_stl.fund_difference_resolution: % has no independent resolution by %', NEW.fund_record_id, NEW.resolved_by_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER fund_difference_resolution_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.fund_difference_resolution
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_fund_difference_resolution();

CREATE TABLE brant_stl.order_account (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    order_id          text        NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_revision    bigint      NOT NULL CHECK (order_revision > 0),
    order_hash        text        NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    party_id          text        NOT NULL,
    currency          text        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units       smallint    NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    currency_revision bigint      NOT NULL CHECK (currency_revision > 0),
    status            text        NOT NULL CHECK (status = 'open'),
    version           bigint      NOT NULL CHECK (version >= 1),
    owner_actor_id    text        NOT NULL,
    owner_team_id     text,
    opened_by_kind    text        NOT NULL CHECK (opened_by_kind IN ('human', 'system')),
    opened_by_id      text        NOT NULL,
    open_epoch        bigint      NOT NULL CHECK (open_epoch > 0),
    opened_at         timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, order_id),
    CHECK (owner_actor_id = opened_by_id),
    CHECK (updated_at >= opened_at)
);

-- An order account is inserted open at version 1 and does not change here: moving it to a later
-- order revision is the payment plan review (WF-06, SM-STL-PAYMENT-PLAN-REVIEW, unit C4b), which
-- replaces this function. Applications lock it FOR UPDATE without changing it (S5 D-3).
CREATE FUNCTION brant_stl.guard_order_account() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' AND NEW.status = 'open' AND NEW.version = 1 THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.order_account: an order account is opened once and moves only by its payment plan review'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER order_account_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.order_account
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_order_account();

CREATE TABLE brant_stl.order_account_stage (
    tenant_id       text          NOT NULL,
    company_id      text          NOT NULL,
    order_id        text          NOT NULL,
    order_revision  bigint        NOT NULL CHECK (order_revision > 0),
    stage_id        text          NOT NULL CHECK (stage_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    seq             integer       NOT NULL CHECK (seq >= 1),
    stage_trigger   text          NOT NULL CHECK (stage_trigger IN ('order_confirmed', 'before_shipment', 'after_shipment', 'after_invoice')),
    days            integer       NOT NULL CHECK (days >= 0),
    percent         text          NOT NULL,
    amount_minor    numeric(32,0) NOT NULL CHECK (amount_minor >= 0),
    release_control text          NOT NULL CHECK (release_control IN ('', 'documents')),
    PRIMARY KEY (tenant_id, company_id, order_id, order_revision, stage_id),
    UNIQUE (tenant_id, company_id, order_id, order_revision, seq),
    FOREIGN KEY (tenant_id, company_id, order_id) REFERENCES brant_stl.order_account (tenant_id, company_id, order_id)
);

-- Stages are inserted with their account's revision and never change.
CREATE FUNCTION brant_stl.guard_order_account_stage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' AND EXISTS (SELECT 1 FROM brant_stl.order_account a WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id
            AND a.order_id = NEW.order_id AND a.order_revision = NEW.order_revision) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.order_account_stage: stages are inserted with their account revision and never change'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER order_account_stage_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.order_account_stage
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_order_account_stage();

-- At commit: an account has the stages of its revision.
CREATE FUNCTION brant_stl.check_order_account_stages() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM brant_stl.order_account_stage s WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id
            AND s.order_id = NEW.order_id AND s.order_revision = NEW.order_revision) THEN
        RAISE EXCEPTION 'brant_stl.order_account: % has no payment stages', NEW.order_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER order_account_stages AFTER INSERT ON brant_stl.order_account
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_order_account_stages();

CREATE TABLE brant_stl.application (
    tenant_id      text          NOT NULL,
    company_id     text          NOT NULL,
    application_id text          NOT NULL CHECK (application_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    fund_record_id text          NOT NULL,
    target_kind    text          NOT NULL CHECK (target_kind IN ('order_stage')),
    order_id       text,
    order_revision bigint,
    stage_id       text,
    currency       text          NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units    smallint      NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    amount_minor   numeric(32,0) NOT NULL CHECK (amount_minor > 0),
    status         text          NOT NULL CHECK (status IN ('applied', 'unapplied')),
    version        bigint        NOT NULL CHECK (version BETWEEN 1 AND 2),
    owner_actor_id text          NOT NULL,
    owner_team_id  text,
    applied_by_kind text         NOT NULL CHECK (applied_by_kind = 'human'),
    applied_by_id  text          NOT NULL,
    apply_epoch    bigint        NOT NULL CHECK (apply_epoch > 0),
    applied_at     timestamptz   NOT NULL,
    updated_at     timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, application_id),
    FOREIGN KEY (tenant_id, company_id, fund_record_id) REFERENCES brant_stl.fund_record (tenant_id, company_id, fund_record_id),
    FOREIGN KEY (tenant_id, company_id, order_id, order_revision, stage_id)
        REFERENCES brant_stl.order_account_stage (tenant_id, company_id, order_id, order_revision, stage_id),
    CHECK (target_kind <> 'order_stage' OR (order_id IS NOT NULL AND order_revision IS NOT NULL AND stage_id IS NOT NULL)),
    CHECK ((status = 'applied') = (version = 1)),
    CHECK (owner_actor_id = applied_by_id),
    CHECK (updated_at >= applied_at)
);

CREATE INDEX application_fund ON brant_stl.application (tenant_id, company_id, fund_record_id);
CREATE INDEX application_order ON brant_stl.application (tenant_id, company_id, order_id, stage_id);

-- An application is inserted applied at version 1, from a receipt of the order's customer in the
-- order's currency (STL-INV-002, STL-INV-006); it moves once, applied → unapplied (SM-STL-APPLICATION),
-- nothing else; it is never deleted.
CREATE FUNCTION brant_stl.guard_application() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_moving text[] := ARRAY['status', 'version', 'updated_at'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_stl.application: applications are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'applied' OR NEW.version <> 1 OR NOT EXISTS (SELECT 1 FROM brant_stl.fund_record f
                JOIN brant_stl.order_account a ON a.tenant_id = f.tenant_id AND a.company_id = f.company_id AND a.order_id = NEW.order_id
                    AND a.order_revision = NEW.order_revision
                WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id AND f.fund_record_id = NEW.fund_record_id
                    AND f.kind = 'receipt' AND f.currency = NEW.currency AND f.minor_units = NEW.minor_units AND a.currency = NEW.currency
                    AND f.counterparty_id = a.party_id AND f.recorded_at <= NEW.applied_at) THEN
            RAISE EXCEPTION 'brant_stl.application: % applies no receipt of the order''s customer in its currency', NEW.application_id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'applied' AND NEW.status = 'unapplied' AND NEW.version = OLD.version + 1
            AND (to_jsonb(NEW) - v_moving) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_moving) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.application: application % moves only along SM-STL-APPLICATION', OLD.application_id
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER application_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.application
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_application();

CREATE TABLE brant_stl.application_unapply (
    tenant_id          text        NOT NULL,
    company_id         text        NOT NULL,
    application_id     text        NOT NULL,
    reason             text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    unapplied_by_kind  text        NOT NULL CHECK (unapplied_by_kind = 'human'),
    unapplied_by_id    text        NOT NULL,
    unapply_epoch      bigint      NOT NULL CHECK (unapply_epoch > 0),
    unapplied_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, application_id),
    FOREIGN KEY (tenant_id, company_id, application_id) REFERENCES brant_stl.application (tenant_id, company_id, application_id)
);

-- An unapply is only appended, once per application; it never changes and is never deleted.
CREATE FUNCTION brant_stl.guard_application_unapply() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_stl.application_unapply: unapplies never change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER application_unapply_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.application_unapply
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_application_unapply();

-- At commit, for every fund record a command touched: its version counts its resolutions, which do
-- not exceed its difference; what is applied and drawn on it stays within received + resolved, and
-- what left its account (refunds, returns, conversions) within received; every stage it was applied
-- to keeps applied ≤ amount; an application is unapplied exactly when its unapply exists.
CREATE FUNCTION brant_stl.check_fund_balance(p_tenant text, p_company text, p_fund text) RETURNS void LANGUAGE plpgsql AS $$
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
    SELECT coalesce(sum(a.amount_minor), 0) INTO v_applied FROM brant_stl.application a
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

CREATE FUNCTION brant_stl.check_fund_record_balance() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM brant_stl.check_fund_balance(NEW.tenant_id, NEW.company_id, NEW.fund_record_id);
    IF NEW.source_fund_record_id IS NOT NULL THEN
        PERFORM brant_stl.check_fund_balance(NEW.tenant_id, NEW.company_id, NEW.source_fund_record_id);
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER fund_record_balance AFTER INSERT OR UPDATE ON brant_stl.fund_record
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_fund_record_balance();

CREATE FUNCTION brant_stl.check_entry_balance() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_fund text;
BEGIN
    IF TG_TABLE_NAME = 'application_unapply' THEN
        SELECT a.fund_record_id INTO v_fund FROM brant_stl.application a WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id
            AND a.application_id = NEW.application_id;
    ELSE
        v_fund := NEW.fund_record_id;
    END IF;
    PERFORM brant_stl.check_fund_balance(NEW.tenant_id, NEW.company_id, v_fund);
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER fund_difference_resolution_balance AFTER INSERT ON brant_stl.fund_difference_resolution
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_entry_balance();
CREATE CONSTRAINT TRIGGER application_balance AFTER INSERT OR UPDATE ON brant_stl.application
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_entry_balance();
CREATE CONSTRAINT TRIGGER application_unapply_balance AFTER INSERT ON brant_stl.application_unapply
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_entry_balance();

-- PaymentConditionGuard v1 (R45): the order account locked FOR SHARE in the caller's transaction;
-- always one row in the caller's scope. See internal/contexts/stl/api/payment.go.
CREATE FUNCTION brant_stl.guard_payment_condition_v1(p_order_id text, p_order_revision bigint, p_scope text, p_at timestamptz)
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
                AND x.order_id = s.order_id AND x.order_revision = s.order_revision AND x.stage_id = s.stage_id AND x.status = 'applied') AS applied
        FROM brant_stl.order_account_stage s
        WHERE s.tenant_id = v_tenant AND s.company_id = v_company AND s.order_id = p_order_id AND s.order_revision = v_a.order_revision
            AND (s.stage_trigger IN ('order_confirmed', 'before_shipment')
                OR (p_scope = 'documents' AND s.stage_trigger IN ('after_shipment', 'after_invoice') AND s.release_control = 'documents'))
        ORDER BY s.seq
    LOOP
        v_required := v_required + v_s.amount_minor;
        v_applied := v_applied + v_s.applied;
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

ALTER TABLE brant_stl.fund_record ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.fund_record FORCE ROW LEVEL SECURITY;
CREATE POLICY fund_record_scope ON brant_stl.fund_record
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.fund_difference_resolution ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.fund_difference_resolution FORCE ROW LEVEL SECURITY;
CREATE POLICY fund_difference_resolution_scope ON brant_stl.fund_difference_resolution
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.order_account ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.order_account FORCE ROW LEVEL SECURITY;
CREATE POLICY order_account_scope ON brant_stl.order_account
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.order_account_stage ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.order_account_stage FORCE ROW LEVEL SECURITY;
CREATE POLICY order_account_stage_scope ON brant_stl.order_account_stage
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.application ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.application FORCE ROW LEVEL SECURITY;
CREATE POLICY application_scope ON brant_stl.application
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.application_unapply ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.application_unapply FORCE ROW LEVEL SECURITY;
CREATE POLICY application_unapply_scope ON brant_stl.application_unapply
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
