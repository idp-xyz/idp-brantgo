-- STL: charges, charge allocation and the order operating result (tactical model STL-AGG-Charge with its entity
-- ChargeAllocation, STL-AGG-OrderResult with its entity ResultSnapshot; SM-STL-CHARGE, SM-STL-ORDER-RESULT;
-- STL-INV-010, STL-INV-011; Context Map MD-49; UL STL-OrderResult; baseline 4.2, 4.5, 4.6 S-A, 4.13; BT18, BT24;
-- WBS S5-17; S5 plan D-3). Python V0.50 has no STL.
--
-- A charge states its source (a supplier bill, a logistics offer or an internal charge), the orders or shipments
-- it benefits, its POL fee category, currency and conversion basis, its amount and its state: estimated, pending
-- (the bill or charge basis received) or confirmed (an authorised person checked the bill). An estimate keeps its
-- estimated amount when the bill replaces it, so the forecast reads what was expected and the confirmed result
-- what was billed: estimated, pending and confirmed charges are recorded apart and a charge counts once. A charge
-- is allocated to orders or shipments by amount, quantity or weight on weights with a data basis, all non-negative
-- with a positive sum, by the largest remainder in minor units, ties by line ID, a negative adjustment on its
-- absolute value then signed (STL-INV-010). Allocations are only appended; the charge names the one that counts.
--
-- An order result is the order's root for its operating result (order_result_id = order_id): reviewed (human or
-- AI), then settled (closed) by an authorised person when every close condition holds (STL-INV-011), then opened
-- again for a late charge or adjustment. Each close appends a snapshot that never changes (MD-49: a live
-- recalculation never overwrites it); a reopening is recorded with its reason, and the next close appends the next
-- snapshot (BT24). Reviews are kept too. A row at version 0 is the lock anchor a charge command creates for an
-- order without a result yet. Figures are derived from STL's own records and its upstreams' facts when read; only
-- the snapshots store them.

-- Charges --------------------------------------------------------------------------------------------------------

CREATE TABLE brant_stl.charge (
    tenant_id             text          NOT NULL,
    company_id            text          NOT NULL,
    charge_id             text          NOT NULL CHECK (charge_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status                text          NOT NULL CHECK (status IN ('estimated', 'pending', 'confirmed')),
    version               bigint        NOT NULL CHECK (version >= 1),
    source_kind           text          NOT NULL CHECK (source_kind IN ('supplier_bill', 'logistics_offer', 'internal')),
    source_party_id       text,
    source_reference      text          CHECK (char_length(source_reference) BETWEEN 1 AND 128),
    estimate_reference    text          CHECK (char_length(estimate_reference) BETWEEN 1 AND 128),
    fee_category_id       text          NOT NULL,
    fee_category_revision bigint        NOT NULL CHECK (fee_category_revision > 0),
    currency              text          NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units           smallint      NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    currency_revision     bigint        NOT NULL CHECK (currency_revision > 0),
    estimate_minor        numeric(32,0) CHECK (estimate_minor <> 0),
    amount_minor          numeric(32,0) NOT NULL CHECK (amount_minor <> 0),
    conversion_kind       text          NOT NULL CHECK (conversion_kind IN ('same_currency', 'order_analysis', 'fx_rate')),
    fx_rate_id            text,
    fx_rate_revision      bigint        CHECK (fx_rate_revision > 0),
    fx_rate_hash          text          CHECK (fx_rate_hash ~ '^[0-9a-f]{64}$'),
    fx_rate               numeric(30,12) CHECK (fx_rate > 0),
    fx_target_currency    text          CHECK (fx_target_currency ~ '^[A-Z]{3}$'),
    occurred_on           date          NOT NULL,
    analysis_on           date          NOT NULL,
    basis                 text          NOT NULL CHECK (char_length(basis) BETWEEN 1 AND 1000),
    evidence_ids          text[]        NOT NULL CHECK (cardinality(evidence_ids) <= 20),
    owner_actor_id        text          NOT NULL,
    owner_team_id         text,
    recorded_by_kind      text          NOT NULL CHECK (recorded_by_kind IN ('human', 'ai')),
    recorded_by_id        text          NOT NULL,
    record_epoch          bigint        NOT NULL CHECK (record_epoch > 0),
    recorded_at           timestamptz   NOT NULL,
    billed_by_kind        text          CHECK (billed_by_kind IN ('human', 'ai')),
    billed_by_id          text,
    bill_epoch            bigint        CHECK (bill_epoch > 0),
    billed_at             timestamptz,
    confirmed_by_kind     text          CHECK (confirmed_by_kind = 'human'),
    confirmed_by_id       text,
    confirm_epoch         bigint        CHECK (confirm_epoch > 0),
    confirmed_at          timestamptz,
    confirm_evidence_ids  text[]        CHECK (cardinality(confirm_evidence_ids) BETWEEN 1 AND 20),
    confirm_reason        text          CHECK (char_length(confirm_reason) BETWEEN 1 AND 1000),
    allocation_seq        integer       NOT NULL CHECK (allocation_seq >= 0),
    updated_at            timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, charge_id),
    CHECK (source_kind = 'internal' OR source_party_id IS NOT NULL),
    CHECK ((billed_by_id IS NULL) = (billed_by_kind IS NULL) AND (billed_by_id IS NULL) = (bill_epoch IS NULL)
        AND (billed_by_id IS NULL) = (billed_at IS NULL)),
    CHECK ((status = 'confirmed') = (confirmed_by_id IS NOT NULL) AND (confirmed_by_id IS NULL) = (confirmed_by_kind IS NULL)
        AND (confirmed_by_id IS NULL) = (confirm_epoch IS NULL) AND (confirmed_by_id IS NULL) = (confirmed_at IS NULL)
        AND (confirmed_by_id IS NULL) = (confirm_evidence_ids IS NULL)),
    CHECK (status <> 'estimated' OR billed_by_id IS NULL),
    CHECK ((status <> 'estimated' AND billed_by_id IS NULL) OR estimate_minor IS NOT NULL),
    CHECK (estimate_reference IS NULL OR billed_by_id IS NOT NULL),
    CHECK ((conversion_kind = 'fx_rate') = (fx_rate_id IS NOT NULL) AND (fx_rate_id IS NULL) = (fx_rate_revision IS NULL)
        AND (fx_rate_id IS NULL) = (fx_rate_hash IS NULL) AND (fx_rate_id IS NULL) = (fx_rate IS NULL)
        AND (fx_rate_id IS NULL) = (fx_target_currency IS NULL)),
    CHECK (owner_actor_id = recorded_by_id),
    CHECK (analysis_on >= occurred_on - 366 AND analysis_on <= occurred_on + 366),
    CHECK (updated_at >= recorded_at)
);

-- A bill or offer is recorded once: one charge per source party and reference ("金额型费用单据按来源及分配关系防重").
CREATE UNIQUE INDEX charge_source ON brant_stl.charge (tenant_id, company_id, source_kind, source_party_id, source_reference)
    WHERE source_reference IS NOT NULL;

-- The orders and shipments a charge benefits, fixed when it is recorded.
CREATE TABLE brant_stl.charge_beneficiary (
    tenant_id   text     NOT NULL,
    company_id  text     NOT NULL,
    charge_id   text     NOT NULL,
    seq         smallint NOT NULL CHECK (seq BETWEEN 1 AND 50),
    kind        text     NOT NULL CHECK (kind IN ('order', 'shipment')),
    order_id    text     NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    shipment_id text     CHECK (shipment_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    PRIMARY KEY (tenant_id, company_id, charge_id, seq),
    UNIQUE NULLS NOT DISTINCT (tenant_id, company_id, charge_id, order_id, shipment_id),
    FOREIGN KEY (tenant_id, company_id, charge_id) REFERENCES brant_stl.charge (tenant_id, company_id, charge_id),
    CHECK ((kind = 'shipment') = (shipment_id IS NOT NULL))
);

CREATE INDEX charge_beneficiary_order ON brant_stl.charge_beneficiary (tenant_id, company_id, order_id);

-- An allocation of the charge's amount at the charge version it moved the charge to.
CREATE TABLE brant_stl.charge_allocation (
    tenant_id         text          NOT NULL,
    company_id        text          NOT NULL,
    charge_id         text          NOT NULL,
    allocation_seq    integer       NOT NULL CHECK (allocation_seq >= 1),
    charge_version    bigint        NOT NULL CHECK (charge_version >= 2),
    basis             text          NOT NULL CHECK (basis IN ('amount', 'quantity', 'weight')),
    data_basis        text          NOT NULL CHECK (char_length(data_basis) BETWEEN 1 AND 1000),
    evidence_ids      text[]        NOT NULL CHECK (cardinality(evidence_ids) <= 20),
    currency          text          NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units       smallint      NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    amount_minor      numeric(32,0) NOT NULL CHECK (amount_minor <> 0),
    allocated_by_kind text          NOT NULL CHECK (allocated_by_kind IN ('human', 'ai')),
    allocated_by_id   text          NOT NULL,
    allocate_epoch    bigint        NOT NULL CHECK (allocate_epoch > 0),
    allocated_at      timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, charge_id, allocation_seq),
    UNIQUE (tenant_id, company_id, charge_id, charge_version),
    FOREIGN KEY (tenant_id, company_id, charge_id) REFERENCES brant_stl.charge (tenant_id, company_id, charge_id)
);

CREATE TABLE brant_stl.charge_allocation_line (
    tenant_id      text          NOT NULL,
    company_id     text          NOT NULL,
    charge_id      text          NOT NULL,
    allocation_seq integer       NOT NULL,
    line_id        text          NOT NULL CHECK (line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    seq            smallint      NOT NULL CHECK (seq BETWEEN 1 AND 200),
    target_kind    text          NOT NULL CHECK (target_kind IN ('order', 'shipment')),
    order_id       text          NOT NULL,
    shipment_id    text,
    weight         numeric(24,6) NOT NULL CHECK (weight >= 0),
    amount_minor   numeric(32,0) NOT NULL,
    PRIMARY KEY (tenant_id, company_id, charge_id, allocation_seq, line_id),
    UNIQUE (tenant_id, company_id, charge_id, allocation_seq, seq),
    UNIQUE NULLS NOT DISTINCT (tenant_id, company_id, charge_id, allocation_seq, order_id, shipment_id),
    FOREIGN KEY (tenant_id, company_id, charge_id, allocation_seq)
        REFERENCES brant_stl.charge_allocation (tenant_id, company_id, charge_id, allocation_seq),
    CHECK ((target_kind = 'shipment') = (shipment_id IS NOT NULL))
);

CREATE INDEX charge_allocation_line_order ON brant_stl.charge_allocation_line (tenant_id, company_id, order_id);

-- A charge is inserted estimated or pending at version 1 without an allocation. It then moves one version at a
-- time (SM-STL-CHARGE): estimated → pending when its bill or basis is recorded (amount, source, dates, basis and
-- evidence may change; the estimate is kept); estimated or pending → confirmed by a person with the bill as
-- evidence (nothing else changes); an allocation makes the newest allocation the charge's (nothing else changes).
-- What is fixed when recorded never changes; a charge is never deleted.
CREATE FUNCTION brant_stl.guard_charge() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_billing    text[] := ARRAY['status', 'version', 'source_kind', 'source_party_id', 'source_reference', 'estimate_reference',
        'estimate_minor', 'amount_minor', 'occurred_on', 'analysis_on', 'basis', 'evidence_ids', 'billed_by_kind', 'billed_by_id',
        'bill_epoch', 'billed_at', 'allocation_seq', 'updated_at'];
    v_confirming text[] := ARRAY['status', 'version', 'confirmed_by_kind', 'confirmed_by_id', 'confirm_epoch', 'confirmed_at',
        'confirm_evidence_ids', 'confirm_reason', 'updated_at'];
    v_allocating text[] := ARRAY['version', 'allocation_seq', 'updated_at'];
BEGIN
    IF TG_OP = 'INSERT' AND NEW.status IN ('estimated', 'pending') AND NEW.version = 1 AND NEW.allocation_seq = 0
            AND NEW.billed_by_id IS NULL AND (NEW.status = 'estimated') = (NEW.estimate_minor IS NOT NULL)
            AND (NEW.estimate_minor IS NULL OR NEW.estimate_minor = NEW.amount_minor) THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.version = OLD.version + 1 THEN
        IF OLD.status = 'estimated' AND NEW.status = 'pending' AND NEW.billed_by_id IS NOT NULL
                AND NEW.estimate_minor IS NOT DISTINCT FROM OLD.estimate_minor
                AND (NEW.allocation_seq = OLD.allocation_seq OR NEW.allocation_seq = 0)
                AND (NEW.allocation_seq = 0 OR NEW.amount_minor = OLD.amount_minor)
                AND (to_jsonb(NEW) - v_billing) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_billing) THEN
            RETURN NEW;
        END IF;
        IF OLD.status IN ('estimated', 'pending') AND NEW.status = 'confirmed'
                AND (to_jsonb(NEW) - v_confirming) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_confirming) THEN
            RETURN NEW;
        END IF;
        IF NEW.status = OLD.status AND NEW.allocation_seq > OLD.allocation_seq
                AND (to_jsonb(NEW) - v_allocating) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_allocating) THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'brant_stl.charge: a charge is recorded, billed, confirmed and allocated only along SM-STL-CHARGE'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER charge_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.charge
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_charge();

-- Beneficiaries, allocations and their lines are only inserted; they never change and are never deleted.
CREATE FUNCTION brant_stl.guard_charge_entry() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION '%: charge beneficiaries and allocations are only appended (STL-INV-010)', TG_TABLE_NAME
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER charge_beneficiary_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.charge_beneficiary
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_charge_entry();
CREATE TRIGGER charge_allocation_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.charge_allocation
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_charge_entry();
CREATE TRIGGER charge_allocation_line_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.charge_allocation_line
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_charge_entry();

-- At commit, for every charge a command touched (STL-INV-010): it has 1 to 50 beneficiaries; its version counts
-- its changes (1 + billed + confirmed + allocations); every allocation is in the charge's currency, its lines add
-- up to its amount exactly, carry its sign or nothing (no line absorbs rounding against it), weigh at least one
-- positive and target an order among the beneficiaries' orders; and the allocation the charge names is one of its
-- own at its current amount.
CREATE FUNCTION brant_stl.check_charge(p_tenant text, p_company text, p_charge text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    v_c     record;
    v_n     integer;
    v_a     record;
    v_sum   numeric;
    v_w     numeric;
    v_wrong integer;
BEGIN
    SELECT c.* INTO v_c FROM brant_stl.charge c WHERE c.tenant_id = p_tenant AND c.company_id = p_company AND c.charge_id = p_charge;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    SELECT count(*) INTO v_n FROM brant_stl.charge_beneficiary b
    WHERE b.tenant_id = p_tenant AND b.company_id = p_company AND b.charge_id = p_charge;
    IF v_n < 1 THEN
        RAISE EXCEPTION 'brant_stl.charge: % benefits no order or shipment', p_charge USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT count(*) INTO v_n FROM brant_stl.charge_allocation a
    WHERE a.tenant_id = p_tenant AND a.company_id = p_company AND a.charge_id = p_charge;
    IF v_c.version <> 1 + (CASE WHEN v_c.billed_by_id IS NULL THEN 0 ELSE 1 END) + (CASE WHEN v_c.status = 'confirmed' THEN 1 ELSE 0 END) + v_n
            OR v_c.allocation_seq > v_n THEN
        RAISE EXCEPTION 'brant_stl.charge: % does not count the changes of its aggregate', p_charge
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    FOR v_a IN SELECT a.* FROM brant_stl.charge_allocation a
            WHERE a.tenant_id = p_tenant AND a.company_id = p_company AND a.charge_id = p_charge LOOP
        SELECT coalesce(sum(l.amount_minor), 0), coalesce(sum(l.weight), 0),
               count(*) FILTER (WHERE sign(l.amount_minor) = -sign(v_a.amount_minor)
                   OR NOT EXISTS (SELECT 1 FROM brant_stl.charge_beneficiary b WHERE b.tenant_id = p_tenant AND b.company_id = p_company
                       AND b.charge_id = p_charge AND b.order_id = l.order_id))
          INTO v_sum, v_w, v_wrong
        FROM brant_stl.charge_allocation_line l
        WHERE l.tenant_id = p_tenant AND l.company_id = p_company AND l.charge_id = p_charge AND l.allocation_seq = v_a.allocation_seq;
        IF v_sum <> v_a.amount_minor OR v_w <= 0 OR v_wrong > 0 OR v_a.currency <> v_c.currency OR v_a.minor_units <> v_c.minor_units
                OR v_a.allocation_seq > v_n THEN
            RAISE EXCEPTION 'brant_stl.charge_allocation: allocation % of % does not split its amount over its weights (STL-INV-010)',
                v_a.allocation_seq, p_charge USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END LOOP;
    IF v_c.allocation_seq > 0 AND NOT EXISTS (SELECT 1 FROM brant_stl.charge_allocation a WHERE a.tenant_id = p_tenant
            AND a.company_id = p_company AND a.charge_id = p_charge AND a.allocation_seq = v_c.allocation_seq
            AND a.amount_minor = v_c.amount_minor) THEN
        RAISE EXCEPTION 'brant_stl.charge: % names an allocation of another amount', p_charge USING ERRCODE = 'integrity_constraint_violation';
    END IF;
END
$$;

CREATE FUNCTION brant_stl.check_charge_entry() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM brant_stl.check_charge(NEW.tenant_id, NEW.company_id, NEW.charge_id);
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER charge_root AFTER INSERT OR UPDATE ON brant_stl.charge
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_charge_entry();
CREATE CONSTRAINT TRIGGER charge_allocation_root AFTER INSERT ON brant_stl.charge_allocation
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_charge_entry();
CREATE CONSTRAINT TRIGGER charge_allocation_line_root AFTER INSERT ON brant_stl.charge_allocation_line
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_charge_entry();

-- Order results ----------------------------------------------------------------------------------------------------

CREATE TABLE brant_stl.order_result (
    tenant_id      text        NOT NULL,
    company_id     text        NOT NULL,
    order_id       text        NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status         text        NOT NULL CHECK (status IN ('open', 'reviewed', 'settled')),
    version        bigint      NOT NULL CHECK (version >= 0),
    owner_actor_id text        NOT NULL,
    owner_team_id  text,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, order_id),
    CHECK (version > 0 OR status = 'open'),
    CHECK (updated_at >= created_at)
);

CREATE TABLE brant_stl.order_result_review (
    tenant_id        text        NOT NULL,
    company_id       text        NOT NULL,
    order_id         text        NOT NULL,
    seq              integer     NOT NULL CHECK (seq >= 1),
    order_revision   bigint      NOT NULL CHECK (order_revision > 0),
    summary          jsonb       NOT NULL,
    summary_sha256   text        NOT NULL CHECK (summary_sha256 ~ '^[0-9a-f]{64}$'),
    note             text        CHECK (char_length(note) BETWEEN 1 AND 1000),
    reviewed_by_kind text        NOT NULL CHECK (reviewed_by_kind IN ('human', 'ai')),
    reviewed_by_id   text        NOT NULL,
    review_epoch     bigint      NOT NULL CHECK (review_epoch > 0),
    reviewed_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, order_id, seq),
    FOREIGN KEY (tenant_id, company_id, order_id) REFERENCES brant_stl.order_result (tenant_id, company_id, order_id)
);

-- A closing snapshot: the figures and the close conditions as they stood when the result was settled, with the
-- SHA-256 of the canonical content. It never changes (MD-49).
CREATE TABLE brant_stl.order_result_snapshot (
    tenant_id              text          NOT NULL,
    company_id             text          NOT NULL,
    order_id               text          NOT NULL,
    seq                    integer       NOT NULL CHECK (seq >= 1),
    order_revision         bigint        NOT NULL CHECK (order_revision > 0),
    order_hash             text          NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    currency               text          NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units            smallint      NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    net_sales_minor        numeric(32,0) NOT NULL,
    result_minor           numeric(32,0) NOT NULL,
    content                jsonb         NOT NULL,
    content_sha256         text          NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    reason                 text          CHECK (char_length(reason) BETWEEN 1 AND 1000),
    closed_by_kind         text          NOT NULL CHECK (closed_by_kind = 'human'),
    closed_by_id           text          NOT NULL,
    close_epoch            bigint        NOT NULL CHECK (close_epoch > 0),
    closed_at              timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, order_id, seq),
    FOREIGN KEY (tenant_id, company_id, order_id) REFERENCES brant_stl.order_result (tenant_id, company_id, order_id)
);

CREATE TABLE brant_stl.order_result_reopening (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    order_id          text        NOT NULL,
    seq               integer     NOT NULL CHECK (seq >= 1),
    reason            text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    evidence_ids      text[]      NOT NULL CHECK (cardinality(evidence_ids) <= 20),
    reopened_by_kind  text        NOT NULL CHECK (reopened_by_kind = 'human'),
    reopened_by_id    text        NOT NULL,
    reopen_epoch      bigint      NOT NULL CHECK (reopen_epoch > 0),
    reopened_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, order_id, seq),
    FOREIGN KEY (tenant_id, company_id, order_id, seq) REFERENCES brant_stl.order_result_snapshot (tenant_id, company_id, order_id, seq)
);

-- An order result is inserted open at version 0 (the lock anchor); it moves one version at a time along
-- SM-STL-ORDER-RESULT: open → reviewed, reviewed → settled, settled → open. Its owner never changes; it is never
-- deleted.
CREATE FUNCTION brant_stl.guard_order_result() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' AND NEW.status = 'open' AND NEW.version = 0 THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.version = OLD.version + 1 AND NEW.owner_actor_id = OLD.owner_actor_id
            AND NEW.owner_team_id IS NOT DISTINCT FROM OLD.owner_team_id AND NEW.created_at = OLD.created_at
            AND ((OLD.status = 'open' AND NEW.status = 'reviewed') OR (OLD.status = 'reviewed' AND NEW.status = 'settled')
                OR (OLD.status = 'settled' AND NEW.status = 'open')) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.order_result: an order result moves only along SM-STL-ORDER-RESULT'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER order_result_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.order_result
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_order_result();

-- Reviews, snapshots and reopenings are only appended: a closing snapshot is never overwritten (MD-49, BT24).
CREATE FUNCTION brant_stl.guard_order_result_entry() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION '%: order result reviews, closing snapshots and reopenings never change (MD-49)', TG_TABLE_NAME
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER order_result_review_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.order_result_review
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_order_result_entry();
CREATE TRIGGER order_result_snapshot_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.order_result_snapshot
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_order_result_entry();
CREATE TRIGGER order_result_reopening_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.order_result_reopening
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_order_result_entry();

-- At commit, for every order result a command touched: its version counts its reviews, closes and reopenings;
-- it is settled exactly when it has one snapshot more than reopenings (the newest close stands), open or reviewed
-- when every snapshot was reopened; reviewed only after a review that follows the last reopening; a closing
-- snapshot follows a review made since the previous reopening (STL-INV-011, BT24).
CREATE FUNCTION brant_stl.check_order_result(p_tenant text, p_company text, p_order text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    v_r         record;
    v_reviews   integer;
    v_snapshots integer;
    v_reopened  integer;
    v_last_open timestamptz;
    v_last_rev  timestamptz;
BEGIN
    SELECT r.status, r.version INTO v_r FROM brant_stl.order_result r
    WHERE r.tenant_id = p_tenant AND r.company_id = p_company AND r.order_id = p_order;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    SELECT count(*), max(reviewed_at) INTO v_reviews, v_last_rev FROM brant_stl.order_result_review
    WHERE tenant_id = p_tenant AND company_id = p_company AND order_id = p_order;
    SELECT count(*) INTO v_snapshots FROM brant_stl.order_result_snapshot
    WHERE tenant_id = p_tenant AND company_id = p_company AND order_id = p_order;
    SELECT count(*), max(reopened_at) INTO v_reopened, v_last_open FROM brant_stl.order_result_reopening
    WHERE tenant_id = p_tenant AND company_id = p_company AND order_id = p_order;
    IF v_r.version <> v_reviews + v_snapshots + v_reopened
            OR (v_r.status = 'settled') <> (v_snapshots = v_reopened + 1)
            OR (v_r.status <> 'settled' AND v_snapshots <> v_reopened)
            OR (v_r.status = 'reviewed' AND (v_last_rev IS NULL OR (v_last_open IS NOT NULL AND v_last_rev < v_last_open)))
            OR (v_r.status = 'settled' AND (v_last_rev IS NULL OR (v_last_open IS NOT NULL AND v_last_rev < v_last_open))) THEN
        RAISE EXCEPTION 'brant_stl.order_result: % does not follow its reviews, closes and reopenings (STL-INV-011)', p_order
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
END
$$;

CREATE FUNCTION brant_stl.check_order_result_entry() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM brant_stl.check_order_result(NEW.tenant_id, NEW.company_id, NEW.order_id);
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER order_result_root AFTER INSERT OR UPDATE ON brant_stl.order_result
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_order_result_entry();
CREATE CONSTRAINT TRIGGER order_result_review_root AFTER INSERT ON brant_stl.order_result_review
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_order_result_entry();
CREATE CONSTRAINT TRIGGER order_result_snapshot_root AFTER INSERT ON brant_stl.order_result_snapshot
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_order_result_entry();
CREATE CONSTRAINT TRIGGER order_result_reopening_root AFTER INSERT ON brant_stl.order_result_reopening
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_order_result_entry();

-- Row-level security: every table on app.tenant_id and app.company_id ---------------------------------------------

ALTER TABLE brant_stl.charge ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.charge FORCE ROW LEVEL SECURITY;
CREATE POLICY charge_scope ON brant_stl.charge
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.charge_beneficiary ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.charge_beneficiary FORCE ROW LEVEL SECURITY;
CREATE POLICY charge_beneficiary_scope ON brant_stl.charge_beneficiary
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.charge_allocation ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.charge_allocation FORCE ROW LEVEL SECURITY;
CREATE POLICY charge_allocation_scope ON brant_stl.charge_allocation
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.charge_allocation_line ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.charge_allocation_line FORCE ROW LEVEL SECURITY;
CREATE POLICY charge_allocation_line_scope ON brant_stl.charge_allocation_line
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.order_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.order_result FORCE ROW LEVEL SECURITY;
CREATE POLICY order_result_scope ON brant_stl.order_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.order_result_review ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.order_result_review FORCE ROW LEVEL SECURITY;
CREATE POLICY order_result_review_scope ON brant_stl.order_result_review
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.order_result_snapshot ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.order_result_snapshot FORCE ROW LEVEL SECURITY;
CREATE POLICY order_result_snapshot_scope ON brant_stl.order_result_snapshot
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.order_result_reopening ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.order_result_reopening FORCE ROW LEVEL SECURITY;
CREATE POLICY order_result_reopening_scope ON brant_stl.order_result_reopening
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
