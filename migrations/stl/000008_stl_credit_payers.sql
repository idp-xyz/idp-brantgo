-- STL: credit reservations that follow an order revision, and third-party payers (S5R unit R7, WBS S5R-07;
-- simulated sign-off E0 of 2026-11-13: S5R-D08, S5R-D21; tactical model SM-STL-CREDIT-RESERVATION, SM-STL-FUND,
-- STL-INV-004, STL-INV-006; Context Map R43). Python V0.50 has no STL.
--
-- S5R-D08. When an order amendment takes effect (TRD's OrderAmended), STL's credit consumer leaves the new revision
-- with exactly one consumed reservation of the new revision's credit portion: the new revision's own reservation
-- consumed (reserved before apply, D-9, or a stray one made although apply asked none), or — when the event names
-- none — the earlier revision's consumed reservation carried over: a new reservation, basis carried_over, that the
-- system inserts consumed for the new revision at its credit portion, no larger than the reservation it carries
-- (carried_from), so no limit check applies. The earlier revision's reservation is then released with the cause
-- superseded. Every release from now on states its cause: abandoned (a person or agent gave up a reservation of an
-- order not activated), expired (it had expired), order_ended (the order's end, PL-13) or superseded; releases
-- recorded before this migration keep only their reason text (cause NULL).
--
-- S5R-D21. A receipt whose payer is not the customer may be applied to the customer's order stages and receivables
-- when the payer is one of the permitted payers of the customer's PTY trading profile (STL reads it before its
-- transaction, TradingProfileRef v1, and records the profile revision on the application), or when a credit manager
-- other than the recorder verified the third-party payment for that customer with evidence (fund.verify_payer: one
-- verification per fund record and customer, appended; the fund record's version moves by one, its state stays).
-- Every application records its payer basis: customer, permitted_payer or verified_payer; the applications a sales
-- statement moves keep theirs. Nothing is deleted.
--
-- Merge note (S5R R8): this migration replaces brant_stl.guard_fund_record() and brant_stl.check_fund_balance() from
-- 000002's text and brant_stl.guard_application() from 000007's text; a later replacement must keep the payer
-- verifications in the fund record's version (version = 1 + resolutions + verifications) and the payer basis.

-- S5R-D08: carried-over reservations and release causes ---------------------------------------------------------

ALTER TABLE brant_stl.credit_reservation ADD COLUMN carried_from text, ADD COLUMN release_cause text
    CHECK (release_cause IN ('abandoned', 'expired', 'order_ended', 'superseded'));
ALTER TABLE brant_stl.credit_reservation ADD CONSTRAINT credit_reservation_carried_from_fkey FOREIGN KEY (tenant_id, company_id, carried_from)
    REFERENCES brant_stl.credit_reservation (tenant_id, company_id, reservation_id);
-- A reservation is carried over once.
CREATE UNIQUE INDEX credit_reservation_carried ON brant_stl.credit_reservation (tenant_id, company_id, carried_from)
    WHERE carried_from IS NOT NULL;

ALTER TABLE brant_stl.credit_reservation DROP CONSTRAINT credit_reservation_basis_check;
ALTER TABLE brant_stl.credit_reservation ADD CONSTRAINT credit_reservation_basis_check
    CHECK (basis IN ('within_limit', 'credit_decision', 'documents_secured', 'carried_over'));
ALTER TABLE brant_stl.credit_reservation ADD CONSTRAINT credit_reservation_carried_check CHECK ((basis = 'carried_over') = (carried_from IS NOT NULL));
-- 000001's amounts by basis (credit_reservation_check3), with carried_over: what the reservation it carries held,
-- no more (the trigger), at least a portion or a secured amount, the exposure as its basis's.
ALTER TABLE brant_stl.credit_reservation DROP CONSTRAINT credit_reservation_check3;
ALTER TABLE brant_stl.credit_reservation ADD CONSTRAINT credit_reservation_amounts_check CHECK (CASE basis
    WHEN 'documents_secured' THEN amount_minor = 0 AND secured_minor > 0 AND exposure_minor = 0 AND fx_id IS NULL
    WHEN 'carried_over' THEN (amount_minor > 0 OR secured_minor > 0) AND (amount_minor > 0 OR exposure_minor = 0)
        AND (fx_id IS NOT NULL OR (currency = limit_currency AND exposure_minor = amount_minor))
    ELSE amount_minor > 0 AND (fx_id IS NOT NULL OR (currency = limit_currency AND exposure_minor = amount_minor)) END);
-- 000001's expiry after the reservation (check4): a carried-over reservation keeps the expiry of the one it carries,
-- which may lie before it was written; consumed, it never expires.
ALTER TABLE brant_stl.credit_reservation DROP CONSTRAINT credit_reservation_check4;
ALTER TABLE brant_stl.credit_reservation ADD CONSTRAINT credit_reservation_expiry_check CHECK (carried_from IS NOT NULL OR expires_at > reserved_at);
-- 000001's owner (check5): a carried-over reservation keeps the owner of the one it carries (the trigger).
ALTER TABLE brant_stl.credit_reservation DROP CONSTRAINT credit_reservation_check5;
ALTER TABLE brant_stl.credit_reservation ADD CONSTRAINT credit_reservation_owner_check CHECK (carried_from IS NOT NULL OR owner_actor_id = reserved_by_id);
-- 000001's consumption (check7): by the system, at an activation within the reservation's validity; a carried-over
-- reservation is consumed by the system when it is written, at the instant the amendment took effect.
ALTER TABLE brant_stl.credit_reservation DROP CONSTRAINT credit_reservation_check7;
ALTER TABLE brant_stl.credit_reservation ADD CONSTRAINT credit_reservation_consumed_check CHECK (consumed_by_kind IS NULL
    OR (consumed_by_kind = 'system' AND consume_epoch > 0 AND consumed_at >= activated_at
        AND (CASE WHEN carried_from IS NULL THEN activated_at >= reserved_at AND activated_at < expires_at
            ELSE reserved_by_kind = 'system' AND consumed_at = reserved_at END)));
-- 000001's states and versions (check10): a carried-over reservation starts consumed at version 1 and is released
-- at version 2.
ALTER TABLE brant_stl.credit_reservation DROP CONSTRAINT credit_reservation_check10;
ALTER TABLE brant_stl.credit_reservation ADD CONSTRAINT credit_reservation_state_check CHECK (CASE status
    WHEN 'reserved' THEN carried_from IS NULL AND version = 1 AND consumed_at IS NULL AND released_at IS NULL
    WHEN 'consumed' THEN version = CASE WHEN carried_from IS NULL THEN 2 ELSE 1 END AND consumed_at IS NOT NULL AND released_at IS NULL
    ELSE released_at IS NOT NULL AND version = CASE WHEN carried_from IS NULL THEN 1 ELSE 0 END + CASE WHEN consumed_at IS NULL THEN 1 ELSE 2 END
        AND (consumed_at IS NULL OR (released_by_kind = 'system' AND released_at >= consumed_at)) END);
-- A cause only with a release; the order's end and supersession are the system's.
ALTER TABLE brant_stl.credit_reservation ADD CONSTRAINT credit_reservation_cause_check CHECK ((released_at IS NOT NULL OR release_cause IS NULL)
    AND (release_cause IS NULL OR release_cause NOT IN ('order_ended', 'superseded') OR released_by_kind = 'system'));

-- 000001's trigger, with carried-over reservations and causes: a reservation is inserted reserved at version 1 (under
-- a released decision of its binding if any), or carried over: inserted consumed at version 1 by the system, basis
-- carried_over, for a later revision of the order of the reservation it carries while that one still holds credit,
-- with its customer, owner, currencies, policy, rate and expiry, no larger a portion or exposure. Then reserved → consumed → released or reserved → released, a
-- carried-over one consumed → released, the version by one; a release from now on states its cause; nothing else;
-- never deleted.
CREATE OR REPLACE FUNCTION brant_stl.guard_credit_reservation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_moving text[] := ARRAY['status', 'version', 'consumed_by_kind', 'consumed_by_id', 'consume_epoch', 'consumed_at', 'activated_at',
        'released_by_kind', 'released_by_id', 'release_epoch', 'released_at', 'release_reason', 'release_cause'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_stl.credit_reservation: reservations are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.carried_from IS NOT NULL THEN
            IF NEW.status <> 'consumed' OR NEW.version <> 1 OR NEW.basis <> 'carried_over' OR NEW.reserved_by_kind <> 'system'
                    OR NEW.release_cause IS NOT NULL OR NOT EXISTS (SELECT 1 FROM brant_stl.credit_reservation f
                    WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id AND f.reservation_id = NEW.carried_from
                        AND f.party_id = NEW.party_id AND f.order_id = NEW.order_id AND f.order_revision < NEW.order_revision
                        AND f.status IN ('reserved', 'consumed') AND f.currency = NEW.currency AND f.minor_units = NEW.minor_units
                        AND f.limit_currency = NEW.limit_currency AND f.limit_minor_units = NEW.limit_minor_units
                        AND f.amount_minor >= NEW.amount_minor AND f.exposure_minor >= NEW.exposure_minor
                        AND (f.fx_id, f.fx_revision, f.fx_hash) IS NOT DISTINCT FROM (NEW.fx_id, NEW.fx_revision, NEW.fx_hash)
                        AND (f.policy_id, f.policy_revision, f.policy_hash) = (NEW.policy_id, NEW.policy_revision, NEW.policy_hash)
                        AND f.owner_actor_id = NEW.owner_actor_id AND f.owner_team_id IS NOT DISTINCT FROM NEW.owner_team_id
                        AND f.expires_at = NEW.expires_at) THEN
                RAISE EXCEPTION 'brant_stl.credit_reservation: % carries over no reservation of its order that holds as much credit (S5R-D08)',
                    NEW.reservation_id USING ERRCODE = 'integrity_constraint_violation';
            END IF;
            RETURN NEW;
        END IF;
        IF NEW.status <> 'reserved' OR NEW.version <> 1 OR NEW.release_cause IS NOT NULL OR (NEW.decision_id IS NOT NULL AND NOT EXISTS (
                SELECT 1 FROM brant_stl.credit_decision d
                WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND d.decision_id = NEW.decision_id AND d.status = 'released'
                    AND d.party_id = NEW.party_id AND d.order_id = NEW.order_id AND d.order_revision = NEW.order_revision
                    AND d.order_hash = NEW.order_hash AND d.portion_minor >= NEW.amount_minor AND d.expires_at > NEW.reserved_at)) THEN
            RAISE EXCEPTION 'brant_stl.credit_reservation: a reservation is inserted reserved, under a released decision of its binding if any'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.version = OLD.version + 1 AND ((OLD.status = 'reserved' AND NEW.status IN ('consumed', 'released'))
            OR (OLD.status = 'consumed' AND NEW.status = 'released'))
            AND (NEW.status <> 'released' OR NEW.release_cause IS NOT NULL)
            AND (to_jsonb(NEW) - v_moving) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_moving)
            AND (OLD.consumed_at IS NULL OR (NEW.consumed_by_kind, NEW.consumed_by_id, NEW.consume_epoch, NEW.consumed_at, NEW.activated_at)
                IS NOT DISTINCT FROM (OLD.consumed_by_kind, OLD.consumed_by_id, OLD.consume_epoch, OLD.consumed_at, OLD.activated_at)) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.credit_reservation: reservation % is %; it moves along SM-STL-CREDIT-RESERVATION only', OLD.reservation_id, OLD.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;

-- S5R-D21: payer verifications ------------------------------------------------------------------------------------

CREATE TABLE brant_stl.fund_payer_verification (
    tenant_id        text        NOT NULL,
    company_id       text        NOT NULL,
    fund_record_id   text        NOT NULL,
    seq              integer     NOT NULL CHECK (seq >= 1),
    payer_id         text        NOT NULL,
    customer_id      text        NOT NULL CHECK (customer_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    evidence_ids     text[]      NOT NULL CHECK (cardinality(evidence_ids) BETWEEN 1 AND 20),
    reason           text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    verified_by_kind text        NOT NULL CHECK (verified_by_kind = 'human'),
    verified_by_id   text        NOT NULL,
    verify_epoch     bigint      NOT NULL CHECK (verify_epoch > 0),
    verified_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, fund_record_id, seq),
    UNIQUE (tenant_id, company_id, fund_record_id, customer_id),
    FOREIGN KEY (tenant_id, company_id, fund_record_id) REFERENCES brant_stl.fund_record (tenant_id, company_id, fund_record_id),
    CHECK (payer_id <> customer_id)
);

-- A verification is only appended, to a receipt of the payer it names, for another party, by someone other than
-- the receipt's recorder (independence row: fund.verify_payer independentOf 资金登记人); it never changes and is
-- never deleted.
CREATE FUNCTION brant_stl.guard_fund_payer_verification() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_stl.fund_payer_verification: payer verifications never change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_stl.fund_record f WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id
            AND f.fund_record_id = NEW.fund_record_id AND f.kind = 'receipt' AND f.counterparty_id = NEW.payer_id
            AND f.recorded_by_id <> NEW.verified_by_id AND f.recorded_at <= NEW.verified_at) THEN
        RAISE EXCEPTION 'brant_stl.fund_payer_verification: % has no independent verification of its payer by %', NEW.fund_record_id,
            NEW.verified_by_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER fund_payer_verification_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.fund_payer_verification
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_fund_payer_verification();
CREATE CONSTRAINT TRIGGER fund_payer_verification_balance AFTER INSERT ON brant_stl.fund_payer_verification
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_entry_balance();

-- 000002's state and version (fund_record_check7): a receipt's version now also counts its payer verifications, so a
-- recorded receipt may be past version 1; only receipts move. check_fund_balance counts the version exactly.
ALTER TABLE brant_stl.fund_record DROP CONSTRAINT fund_record_check7;
ALTER TABLE brant_stl.fund_record ADD CONSTRAINT fund_record_version_check7 CHECK (kind = 'receipt' OR (status = 'recorded' AND version = 1));

-- 000002's trigger, with payer verifications: a fund record is inserted recorded at version 1 (against a source of
-- the right kind, currency, direction and counterparty; a conversion with the bank's arithmetic); afterwards only a
-- receipt's state and version move, by one per resolution of its difference (recorded or difference_resolved →
-- difference_resolved) or per verification of its payer (the state stays); it is never deleted.
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
    IF OLD.kind = 'receipt' AND (NEW.status = 'difference_resolved' OR (OLD.status = 'recorded' AND NEW.status = 'recorded'))
            AND NEW.version = OLD.version + 1 AND (to_jsonb(NEW) - v_moving) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_moving) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.fund_record: fund record % moves only along SM-STL-FUND', OLD.fund_record_id
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;

-- 000002's balance check, with payer verifications: the version counts the resolutions and the verifications, a
-- receipt is difference_resolved exactly when it has a resolution, and everything else as before.
CREATE OR REPLACE FUNCTION brant_stl.check_fund_balance(p_tenant text, p_company text, p_fund text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    v_f        record;
    v_resolved numeric;
    v_count    bigint;
    v_verified bigint;
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
    SELECT coalesce(sum(a.amount_minor), 0) INTO v_applied FROM brant_stl.application a
    WHERE a.tenant_id = p_tenant AND a.company_id = p_company AND a.fund_record_id = p_fund AND a.status = 'applied';
    SELECT coalesce(sum(o.received_minor) FILTER (WHERE o.kind IN ('refund', 'returned_remittance')), 0),
           coalesce(sum(o.sold_minor) FILTER (WHERE o.kind = 'fx_conversion'), 0) INTO v_drawn, v_sold
    FROM brant_stl.fund_record o WHERE o.tenant_id = p_tenant AND o.company_id = p_company AND o.source_fund_record_id = p_fund;
    IF v_f.version <> 1 + v_count + v_verified OR (v_f.status = 'difference_resolved') <> (v_count > 0)
            OR v_resolved > v_f.remitted_minor - v_f.received_minor
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

-- S5R-D21: the payer basis of an application ----------------------------------------------------------------------

-- customer: the fund's counterparty is the target's customer (as before); permitted_payer: it is a permitted payer of
-- the customer's PTY trading profile, whose revision STL read before its transaction (payer_profile_revision; R43 has
-- no guard lock); verified_payer: a payer verification of the fund for the customer exists. Credit notes and payables
-- keep the customer basis. Applications recorded before this migration are the customer's.
ALTER TABLE brant_stl.application ADD COLUMN payer_basis text NOT NULL DEFAULT 'customer'
    CHECK (payer_basis IN ('customer', 'permitted_payer', 'verified_payer')),
    ADD COLUMN payer_profile_revision bigint CHECK (payer_profile_revision > 0);
ALTER TABLE brant_stl.application ADD CONSTRAINT application_payer_check CHECK ((payer_basis = 'permitted_payer') = (payer_profile_revision IS NOT NULL)
    AND (payer_basis = 'customer' OR (fund_record_id IS NOT NULL AND target_kind IN ('order_stage', 'receivable'))));

-- Whether fund p_fund, received from p_payer, may be applied to customer p_customer's target on p_basis.
CREATE FUNCTION brant_stl.payer_admitted(p_tenant text, p_company text, p_fund text, p_basis text, p_payer text, p_customer text)
    RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT CASE p_basis
        WHEN 'customer' THEN p_payer = p_customer
        WHEN 'permitted_payer' THEN p_payer <> p_customer
        WHEN 'verified_payer' THEN p_payer <> p_customer AND EXISTS (SELECT 1 FROM brant_stl.fund_payer_verification v
            WHERE v.tenant_id = p_tenant AND v.company_id = p_company AND v.fund_record_id = p_fund AND v.customer_id = p_customer
                AND v.payer_id = p_payer)
        ELSE false END
$$;

-- 000007's trigger, with the payer basis: an order stage or a receivable takes a receipt of the customer, or of a
-- permitted or verified payer of the customer (S5R-D21); a moved application keeps the basis of the one it comes from.
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
                    AND brant_stl.payer_admitted(f.tenant_id, f.company_id, f.fund_record_id, NEW.payer_basis, f.counterparty_id, a.party_id)
                    AND f.recorded_at <= NEW.applied_at);
        ELSIF v_ok AND NEW.target_kind = 'receivable' THEN
            v_ok := EXISTS (SELECT 1 FROM brant_stl.fund_record f
                JOIN brant_stl.settlement_statement s ON s.tenant_id = f.tenant_id AND s.company_id = f.company_id AND s.statement_id = NEW.statement_id
                JOIN brant_stl.settlement_installment i ON i.tenant_id = s.tenant_id AND i.company_id = s.company_id AND i.statement_id = s.statement_id
                    AND i.seq = NEW.installment_seq
                WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id AND f.fund_record_id = NEW.fund_record_id
                    AND f.kind = 'receipt' AND f.currency = v_cur AND f.minor_units = v_units AND s.kind = 'sales'
                    AND s.status = 'confirmed' AND s.currency = NEW.currency
                    AND brant_stl.payer_admitted(f.tenant_id, f.company_id, f.fund_record_id, NEW.payer_basis, f.counterparty_id, s.counterparty_id)
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
                    AND m.payer_basis = NEW.payer_basis AND m.payer_profile_revision IS NOT DISTINCT FROM NEW.payer_profile_revision
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

ALTER TABLE brant_stl.fund_payer_verification ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.fund_payer_verification FORCE ROW LEVEL SECURITY;
CREATE POLICY fund_payer_verification_scope ON brant_stl.fund_payer_verification
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
