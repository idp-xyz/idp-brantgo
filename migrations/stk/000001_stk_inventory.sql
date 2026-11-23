-- STK: company enablement, stock balances, append-only movements, FIFO cost layers,
-- reservations and batch lineage (STK-AGG-StockLedger, STK-AGG-Reservation, STK-AGG-BatchLineage;
-- STK-INV-001..004; Context Map R29 StockAvailability v1, R41 CostLayerRef v1; S6-D02; WBS S6-01).
--
-- The company switch defaults to off: a company with no row, or with enabled false, has no stock
-- commands and StockAvailability v1 answers module_disabled. stk.module.configure is the only
-- writer of company_setting.
--
-- A balance is (tenant, company, sku, location, batch, custody). on_hand and reserved stay
-- non-negative and reserved never exceeds on_hand. An issue of unreserved stock requires
-- on_hand - reserved. An issue that consumes a reservation decreases on_hand and reserved by the
-- same quantity, so the hold is not counted twice.
--
-- stock_movement, cost_allocation and batch_lineage are append-only. A shipment issue is unique on
-- (tenant, company, ful_shipment_line, shipment_id, shipment_line_id), so the same line cannot be
-- issued twice. cost_layer.remaining_qty only falls. FIFO order is (received_at, layer_id).
-- The one rounding point is round(unit_cost_minor * quantity / price_basis_qty, 0), HALF_UP,
-- which matches kernel.Money.MulHalfUp for a non-negative amount.
--
-- brant_stk.stock_availability_issue_v1 is StockAvailability v1 (R29, guardLock). FUL calls it
-- with FUL's own executor inside shipment.confirm on route stock, after FUL's roots and STL and
-- before TRD. It locks the balance FOR UPDATE, then the reservation, then the cost layers in FIFO
-- order. It locks no FUL row. A status other than ok writes nothing.
--
-- References to PUR, FUL and MFG are plain ids: no foreign key crosses a schema.
-- action_result and audit_event are STK's command ledger (internal/foundation/action).

CREATE SCHEMA brant_stk;

CREATE TABLE brant_stk.company_setting (
    tenant_id       text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id      text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    enabled         boolean     NOT NULL DEFAULT false,
    row_version     bigint      NOT NULL CHECK (row_version > 0),
    updated_by_kind text        NOT NULL CHECK (updated_by_kind IN ('human', 'ai', 'system')),
    updated_by_id   text        NOT NULL CHECK (updated_by_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    update_epoch    bigint      NOT NULL CHECK (update_epoch > 0),
    updated_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id)
);

CREATE TABLE brant_stk.stock_balance (
    tenant_id      text          NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id     text          NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    sku_id         text          NOT NULL CHECK (sku_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    location_id    text          NOT NULL CHECK (location_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    batch_id       text          NOT NULL CHECK (batch_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    custody_kind   text          NOT NULL CHECK (custody_kind IN ('own_warehouse', 'external_factory_material', 'external_factory_wip', 'in_transit')),
    on_hand        numeric(18,6) NOT NULL CHECK (on_hand >= 0),
    reserved       numeric(18,6) NOT NULL CHECK (reserved >= 0),
    unit_id        text          NOT NULL CHECK (unit_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    unit_revision  bigint        NOT NULL CHECK (unit_revision > 0),
    currency       text          NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units    smallint      NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    row_version    bigint        NOT NULL CHECK (row_version > 0),
    owner_actor_id text          NOT NULL CHECK (owner_actor_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    owner_team_id  text          CHECK (owner_team_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    PRIMARY KEY (tenant_id, company_id, sku_id, location_id, batch_id, custody_kind),
    CHECK (reserved <= on_hand)
);

CREATE TABLE brant_stk.stock_movement (
    tenant_id       text          NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id      text          NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    movement_id     text          NOT NULL CHECK (movement_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    kind            text          NOT NULL CHECK (kind IN ('receive', 'issue', 'adjust_in', 'adjust_out', 'handover_transfer', 'handover_consume', 'handover_return', 'handover_scrap', 'shipment_issue')),
    sku_id          text          NOT NULL CHECK (sku_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    location_id     text          NOT NULL CHECK (location_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    batch_id        text          NOT NULL CHECK (batch_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    custody_kind    text          NOT NULL CHECK (custody_kind IN ('own_warehouse', 'external_factory_material', 'external_factory_wip', 'in_transit')),
    quantity        numeric(18,6) NOT NULL CHECK (quantity > 0),
    unit_id         text          NOT NULL CHECK (unit_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    unit_revision   bigint        NOT NULL CHECK (unit_revision > 0),
    source_kind     text          CHECK (source_kind ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    source_id       text          CHECK (source_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    source_line_id  text          CHECK (source_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    reason          text          CHECK (char_length(reason) BETWEEN 1 AND 1000),
    at              timestamptz   NOT NULL,
    actor_kind      text          NOT NULL CHECK (actor_kind IN ('human', 'ai', 'system')),
    actor_id        text          NOT NULL CHECK (actor_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    authority_epoch bigint        NOT NULL CHECK (authority_epoch > 0),
    owner_actor_id  text          NOT NULL CHECK (owner_actor_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    owner_team_id   text          CHECK (owner_team_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    PRIMARY KEY (tenant_id, company_id, movement_id),
    CHECK ((source_kind IS NULL) = (source_id IS NULL AND source_line_id IS NULL) OR source_kind IS NOT NULL),
    CHECK (source_kind IS DISTINCT FROM 'ful_shipment_line' OR (kind = 'shipment_issue' AND source_id IS NOT NULL AND source_line_id IS NOT NULL)),
    CHECK ((source_id IS NULL) = (source_line_id IS NULL))
);

-- One shipment line is issued at most once (STK-INV-001).
CREATE UNIQUE INDEX stock_movement_shipment_line ON brant_stk.stock_movement (tenant_id, company_id, source_id, source_line_id)
    WHERE source_kind = 'ful_shipment_line';

CREATE FUNCTION brant_stk.forbid_movement_change() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'brant_stk.stock_movement is append-only' USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER stock_movement_append_only BEFORE UPDATE OR DELETE ON brant_stk.stock_movement
    FOR EACH ROW EXECUTE FUNCTION brant_stk.forbid_movement_change();

CREATE TABLE brant_stk.cost_layer (
    tenant_id       text          NOT NULL,
    company_id      text          NOT NULL,
    layer_id        text          NOT NULL CHECK (layer_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    movement_id     text          NOT NULL,
    sku_id          text          NOT NULL,
    location_id     text          NOT NULL,
    batch_id        text          NOT NULL,
    custody_kind    text          NOT NULL CHECK (custody_kind IN ('own_warehouse', 'external_factory_material', 'external_factory_wip', 'in_transit')),
    currency        text          NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units     smallint      NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    unit_cost_minor bigint        NOT NULL CHECK (unit_cost_minor >= 0),
    price_basis_qty numeric(18,6) NOT NULL CHECK (price_basis_qty > 0),
    original_qty    numeric(18,6) NOT NULL CHECK (original_qty > 0),
    remaining_qty   numeric(18,6) NOT NULL CHECK (remaining_qty >= 0),
    received_at     timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, layer_id),
    FOREIGN KEY (tenant_id, company_id, movement_id) REFERENCES brant_stk.stock_movement (tenant_id, company_id, movement_id),
    CHECK (remaining_qty <= original_qty)
);

CREATE INDEX cost_layer_fifo ON brant_stk.cost_layer (tenant_id, company_id, sku_id, location_id, batch_id, custody_kind, received_at, layer_id)
    WHERE remaining_qty > 0;

-- Only remaining_qty changes, and only downwards.
CREATE FUNCTION brant_stk.guard_cost_layer() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_stk.cost_layer is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'UPDATE' AND ((to_jsonb(NEW) - 'remaining_qty') IS DISTINCT FROM (to_jsonb(OLD) - 'remaining_qty')
            OR NEW.remaining_qty > OLD.remaining_qty) THEN
        RAISE EXCEPTION 'brant_stk.cost_layer % only loses remaining quantity', OLD.layer_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER cost_layer_guard BEFORE UPDATE OR DELETE ON brant_stk.cost_layer
    FOR EACH ROW EXECUTE FUNCTION brant_stk.guard_cost_layer();

CREATE TABLE brant_stk.cost_allocation (
    tenant_id         text          NOT NULL,
    company_id        text          NOT NULL,
    allocation_id     text          NOT NULL CHECK (allocation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    issue_movement_id text          NOT NULL,
    layer_id          text          NOT NULL,
    quantity          numeric(18,6) NOT NULL CHECK (quantity > 0),
    amount_minor      bigint        NOT NULL CHECK (amount_minor >= 0),
    currency          text          NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    PRIMARY KEY (tenant_id, company_id, allocation_id),
    UNIQUE (tenant_id, company_id, issue_movement_id, layer_id),
    FOREIGN KEY (tenant_id, company_id, issue_movement_id) REFERENCES brant_stk.stock_movement (tenant_id, company_id, movement_id),
    FOREIGN KEY (tenant_id, company_id, layer_id) REFERENCES brant_stk.cost_layer (tenant_id, company_id, layer_id)
);

CREATE FUNCTION brant_stk.forbid_allocation_change() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'brant_stk.cost_allocation is append-only' USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER cost_allocation_append_only BEFORE UPDATE OR DELETE ON brant_stk.cost_allocation
    FOR EACH ROW EXECUTE FUNCTION brant_stk.forbid_allocation_change();

CREATE TABLE brant_stk.stock_reservation (
    tenant_id      text          NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id     text          NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    reservation_id text          NOT NULL CHECK (reservation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    sku_id         text          NOT NULL CHECK (sku_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    location_id    text          NOT NULL CHECK (location_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    batch_id       text          NOT NULL CHECK (batch_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    custody_kind   text          NOT NULL CHECK (custody_kind IN ('own_warehouse', 'external_factory_material', 'external_factory_wip', 'in_transit')),
    order_id       text          CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_line_id  text          CHECK (order_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    quantity       numeric(18,6) NOT NULL CHECK (quantity > 0),
    remaining_qty  numeric(18,6) NOT NULL CHECK (remaining_qty >= 0),
    status         text          NOT NULL CHECK (status IN ('active', 'consumed', 'released')),
    unit_id        text          NOT NULL CHECK (unit_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    unit_revision  bigint        NOT NULL CHECK (unit_revision > 0),
    row_version    bigint        NOT NULL CHECK (row_version > 0),
    owner_actor_id text          NOT NULL CHECK (owner_actor_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    owner_team_id  text          CHECK (owner_team_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    PRIMARY KEY (tenant_id, company_id, reservation_id),
    CHECK (remaining_qty <= quantity),
    CHECK ((order_id IS NULL) = (order_line_id IS NULL)),
    CHECK (status = 'active' OR remaining_qty = 0)
);

CREATE INDEX stock_reservation_balance ON brant_stk.stock_reservation (tenant_id, company_id, sku_id, location_id, batch_id, custody_kind)
    WHERE status = 'active';

CREATE TABLE brant_stk.batch_lineage (
    tenant_id      text          NOT NULL,
    company_id     text          NOT NULL,
    lineage_id     text          NOT NULL CHECK (lineage_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    movement_id    text          NOT NULL,
    input_batch_id text          NOT NULL CHECK (input_batch_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    output_kind    text          NOT NULL CHECK (output_kind IN ('batch', 'shipment_line')),
    output_batch_id text         CHECK (output_batch_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    output_id      text          CHECK (output_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    output_line_id text          CHECK (output_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    quantity       numeric(18,6) NOT NULL CHECK (quantity > 0),
    unit_id        text          NOT NULL,
    unit_revision  bigint        NOT NULL CHECK (unit_revision > 0),
    recorded_at    timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, lineage_id),
    FOREIGN KEY (tenant_id, company_id, movement_id) REFERENCES brant_stk.stock_movement (tenant_id, company_id, movement_id),
    CHECK ((output_kind = 'batch') = (output_batch_id IS NOT NULL AND output_id IS NULL AND output_line_id IS NULL)),
    CHECK ((output_kind = 'shipment_line') = (output_batch_id IS NULL AND output_id IS NOT NULL AND output_line_id IS NOT NULL))
);

CREATE FUNCTION brant_stk.forbid_lineage_change() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'brant_stk.batch_lineage is append-only' USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER batch_lineage_append_only BEFORE UPDATE OR DELETE ON brant_stk.batch_lineage
    FOR EACH ROW EXECUTE FUNCTION brant_stk.forbid_lineage_change();

-- StockAvailability v1. See the header. search_path is fixed; every name is qualified.
-- brant_app gets EXECUTE in deploy/postgres/10_app_grants.sql.
CREATE FUNCTION brant_stk.stock_availability_issue_v1(
    p_shipment_id text, p_shipment_line_id text, p_sku_id text, p_location_id text, p_batch_id text,
    p_custody_kind text, p_reservation_id text, p_quantity text, p_unit_id text, p_unit_revision bigint,
    p_actor_kind text, p_actor_id text, p_epoch bigint, p_at timestamptz)
    RETURNS TABLE (status text, movement_id text, total_cost_minor bigint, currency text, available_qty text, replayed boolean, code text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant    text := pg_catalog.current_setting('app.tenant_id', true);
    v_company   text := pg_catalog.current_setting('app.company_id', true);
    v_enabled   boolean;
    v_qty       numeric;
    v_on_hand   numeric;
    v_reserved  numeric;
    v_unit      text;
    v_unit_rev  bigint;
    v_available numeric;
    v_existing  text;
    v_total     bigint;
    v_currency  text;
    v_res_status text;
    v_res_sku   text;
    v_res_loc   text;
    v_res_batch text;
    v_res_custody text;
    v_res_remaining numeric;
    v_need      numeric;
    v_layer     record;
    v_take      numeric;
    v_amount    bigint;
    v_ids       text[];
    v_takes     numeric[];
    v_amounts   bigint[];
    v_movement  text;
    v_i         integer;
    v_hold      numeric := 0;
    ident       text := '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$';
    qty_form    text := '^(0|[1-9][0-9]{0,11})(\.[0-9]{1,6})?$';
BEGIN
    IF v_tenant IS NULL OR v_company IS NULL OR v_tenant !~ ident OR v_company !~ ident THEN
        RAISE EXCEPTION 'brant_stk.stock_availability_issue_v1: tenant and company scope are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    IF p_shipment_id !~ ident OR p_shipment_line_id !~ ident OR p_sku_id !~ ident OR p_location_id !~ ident
            OR p_batch_id !~ ident OR p_unit_id !~ ident OR p_actor_id !~ ident
            OR p_custody_kind NOT IN ('own_warehouse', 'external_factory_material', 'external_factory_wip', 'in_transit')
            OR p_actor_kind NOT IN ('human', 'ai', 'system')
            OR p_unit_revision IS NULL OR p_unit_revision <= 0 OR p_epoch IS NULL OR p_epoch <= 0 OR p_at IS NULL
            OR p_quantity !~ qty_form OR (p_reservation_id IS NOT NULL AND p_reservation_id <> '' AND p_reservation_id !~ ident) THEN
        RAISE EXCEPTION 'brant_stk.stock_availability_issue_v1: arguments are not a stock issue'
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    v_qty := p_quantity::pg_catalog.numeric;
    IF v_qty <= 0 THEN
        RAISE EXCEPTION 'brant_stk.stock_availability_issue_v1: quantity must be positive'
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

    SELECT s.enabled INTO v_enabled FROM brant_stk.company_setting s
        WHERE s.tenant_id = v_tenant AND s.company_id = v_company;
    IF NOT FOUND OR v_enabled IS NOT TRUE THEN
        RETURN QUERY SELECT 'module_disabled'::text, NULL::text, NULL::bigint, NULL::text, NULL::text, false, 'DEPENDENCY_NOT_SUPPORTED'::text;
        RETURN;
    END IF;

    SELECT b.on_hand, b.reserved, b.unit_id, b.unit_revision
        INTO v_on_hand, v_reserved, v_unit, v_unit_rev
    FROM brant_stk.stock_balance b
    WHERE b.tenant_id = v_tenant AND b.company_id = v_company AND b.sku_id = p_sku_id AND b.location_id = p_location_id
        AND b.batch_id = p_batch_id AND b.custody_kind = p_custody_kind
    FOR UPDATE;

    SELECT m.movement_id INTO v_existing FROM brant_stk.stock_movement m
    WHERE m.tenant_id = v_tenant AND m.company_id = v_company AND m.source_kind = 'ful_shipment_line'
        AND m.source_id = p_shipment_id AND m.source_line_id = p_shipment_line_id;
    IF FOUND THEN
        SELECT coalesce(sum(a.amount_minor), 0), min(a.currency)
            INTO v_total, v_currency
        FROM brant_stk.cost_allocation a
        WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.issue_movement_id = v_existing;
        RETURN QUERY SELECT 'ok'::text, v_existing, v_total, v_currency, NULL::text, true, NULL::text;
        RETURN;
    END IF;

    IF NOT FOUND AND v_on_hand IS NULL THEN
        RETURN QUERY SELECT 'insufficient'::text, NULL::text, NULL::bigint, NULL::text, '0'::text, false, 'STOCK_INSUFFICIENT'::text;
        RETURN;
    END IF;
    -- The movement probe above uses FOUND. Re-read the balance flag from the variables.
    IF v_on_hand IS NULL THEN
        RETURN QUERY SELECT 'insufficient'::text, NULL::text, NULL::bigint, NULL::text, '0'::text, false, 'STOCK_INSUFFICIENT'::text;
        RETURN;
    END IF;
    IF v_unit IS DISTINCT FROM p_unit_id OR v_unit_rev IS DISTINCT FROM p_unit_revision THEN
        RETURN QUERY SELECT 'unit_mismatch'::text, NULL::text, NULL::bigint, NULL::text, NULL::text, false, 'INVALID_KNOWN_VALUE'::text;
        RETURN;
    END IF;

    v_available := v_on_hand - v_reserved;
    IF p_reservation_id IS NOT NULL AND p_reservation_id <> '' THEN
        SELECT r.status, r.sku_id, r.location_id, r.batch_id, r.custody_kind, r.remaining_qty
            INTO v_res_status, v_res_sku, v_res_loc, v_res_batch, v_res_custody, v_res_remaining
        FROM brant_stk.stock_reservation r
        WHERE r.tenant_id = v_tenant AND r.company_id = v_company AND r.reservation_id = p_reservation_id
        FOR UPDATE;
        IF NOT FOUND OR v_res_status IS DISTINCT FROM 'active'
                OR (v_res_sku, v_res_loc, v_res_batch, v_res_custody) IS DISTINCT FROM (p_sku_id, p_location_id, p_batch_id, p_custody_kind)
                OR v_res_remaining < v_qty OR v_reserved < v_qty OR v_on_hand < v_qty THEN
            RETURN QUERY SELECT 'reservation_conflict'::text, NULL::text, NULL::bigint, NULL::text, NULL::text, false, 'STOCK_RESERVATION_CONFLICT'::text;
            RETURN;
        END IF;
        v_hold := v_qty;
    ELSIF v_available < v_qty THEN
        RETURN QUERY SELECT 'insufficient'::text, NULL::text, NULL::bigint, NULL::text, pg_catalog.trim_scale(v_available)::text, false, 'STOCK_INSUFFICIENT'::text;
        RETURN;
    END IF;

    v_need := v_qty;
    v_total := 0;
    FOR v_layer IN
        SELECT c.layer_id, c.unit_cost_minor, c.price_basis_qty, c.remaining_qty, c.currency
        FROM brant_stk.cost_layer c
        WHERE c.tenant_id = v_tenant AND c.company_id = v_company AND c.sku_id = p_sku_id AND c.location_id = p_location_id
            AND c.batch_id = p_batch_id AND c.custody_kind = p_custody_kind AND c.remaining_qty > 0
        ORDER BY c.received_at, c.layer_id
        FOR UPDATE
    LOOP
        EXIT WHEN v_need = 0;
        IF v_currency IS NULL THEN
            v_currency := v_layer.currency;
        ELSIF v_currency IS DISTINCT FROM v_layer.currency THEN
            RETURN QUERY SELECT 'cost_short'::text, NULL::text, NULL::bigint, NULL::text, NULL::text, false, 'STOCK_INSUFFICIENT'::text;
            RETURN;
        END IF;
        v_take := v_layer.remaining_qty;
        IF v_take > v_need THEN
            v_take := v_need;
        END IF;
        v_amount := pg_catalog.round((v_layer.unit_cost_minor::pg_catalog.numeric * v_take) / v_layer.price_basis_qty, 0)::pg_catalog.int8;
        v_ids := pg_catalog.array_append(v_ids, v_layer.layer_id);
        v_takes := pg_catalog.array_append(v_takes, v_take);
        v_amounts := pg_catalog.array_append(v_amounts, v_amount);
        v_total := v_total + v_amount;
        v_need := v_need - v_take;
    END LOOP;
    IF v_need > 0 THEN
        RETURN QUERY SELECT 'cost_short'::text, NULL::text, NULL::bigint, NULL::text, NULL::text, false, 'STOCK_INSUFFICIENT'::text;
        RETURN;
    END IF;

    v_movement := 'm' || pg_catalog.md5(p_shipment_id || '|' || p_shipment_line_id);
    UPDATE brant_stk.stock_balance SET on_hand = on_hand - v_qty, reserved = reserved - v_hold, row_version = row_version + 1
    WHERE tenant_id = v_tenant AND company_id = v_company AND sku_id = p_sku_id AND location_id = p_location_id
        AND batch_id = p_batch_id AND custody_kind = p_custody_kind;
    IF v_hold > 0 THEN
        UPDATE brant_stk.stock_reservation AS r
        SET remaining_qty = r.remaining_qty - v_qty,
            status = CASE WHEN r.remaining_qty - v_qty = 0 THEN 'consumed' ELSE r.status END,
            row_version = r.row_version + 1
        WHERE r.tenant_id = v_tenant AND r.company_id = v_company AND r.reservation_id = p_reservation_id;
    END IF;
    FOR v_i IN 1 .. coalesce(pg_catalog.array_length(v_ids, 1), 0) LOOP
        UPDATE brant_stk.cost_layer SET remaining_qty = remaining_qty - v_takes[v_i]
        WHERE tenant_id = v_tenant AND company_id = v_company AND layer_id = v_ids[v_i];
    END LOOP;
    INSERT INTO brant_stk.stock_movement (tenant_id, company_id, movement_id, kind, sku_id, location_id, batch_id, custody_kind,
        quantity, unit_id, unit_revision, source_kind, source_id, source_line_id, at, actor_kind, actor_id, authority_epoch, owner_actor_id)
    VALUES (v_tenant, v_company, v_movement, 'shipment_issue', p_sku_id, p_location_id, p_batch_id, p_custody_kind,
        v_qty, p_unit_id, p_unit_revision, 'ful_shipment_line', p_shipment_id, p_shipment_line_id, p_at, p_actor_kind, p_actor_id, p_epoch, p_actor_id);
    FOR v_i IN 1 .. coalesce(pg_catalog.array_length(v_ids, 1), 0) LOOP
        INSERT INTO brant_stk.cost_allocation (tenant_id, company_id, allocation_id, issue_movement_id, layer_id, quantity, amount_minor, currency)
        VALUES (v_tenant, v_company, 'a' || pg_catalog.md5(v_movement || '|' || v_ids[v_i]), v_movement, v_ids[v_i], v_takes[v_i], v_amounts[v_i], v_currency);
    END LOOP;
    RETURN QUERY SELECT 'ok'::text, v_movement, v_total, v_currency, NULL::text, false, NULL::text;
    RETURN;
END
$$;

REVOKE ALL ON FUNCTION brant_stk.stock_availability_issue_v1(text, text, text, text, text, text, text, text, text, bigint, text, text, bigint, timestamptz) FROM PUBLIC;

CREATE TABLE brant_stk.action_result (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    command         text        NOT NULL CHECK (command ~ '^[a-z][a-z0-9_]*\.[a-z][a-z0-9_.]*$'),
    idempotency_key text        NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    request_hash    text        NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    action_id       text        NOT NULL,
    object_id       text        NOT NULL,
    row_version     bigint      NOT NULL CHECK (row_version > 0),
    status          text        NOT NULL,
    actor_kind      text        NOT NULL CHECK (actor_kind IN ('human', 'ai', 'system')),
    actor_id        text        NOT NULL,
    authority_epoch bigint      NOT NULL CHECK (authority_epoch > 0),
    correlation_id  text        NOT NULL,
    recorded_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, command, idempotency_key),
    UNIQUE (tenant_id, company_id, action_id)
);

CREATE TABLE brant_stk.audit_event (
    seq             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    event_id        text        NOT NULL UNIQUE,
    actor_kind      text        NOT NULL CHECK (actor_kind IN ('human', 'ai', 'system')),
    actor_id        text        NOT NULL,
    command         text        NOT NULL,
    object_id       text,
    outcome         text        NOT NULL CHECK (outcome IN ('succeeded', 'replayed', 'rejected', 'read')),
    code            text,
    correlation_id  text        NOT NULL,
    recorded_at     timestamptz NOT NULL,
    reason          text,
    authority_epoch bigint,
    CHECK ((outcome = 'rejected') = (code IS NOT NULL))
);

ALTER TABLE brant_stk.company_setting ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stk.company_setting FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_stk.stock_balance ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stk.stock_balance FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_stk.stock_movement ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stk.stock_movement FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_stk.cost_layer ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stk.cost_layer FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_stk.cost_allocation ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stk.cost_allocation FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_stk.stock_reservation ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stk.stock_reservation FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_stk.batch_lineage ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stk.batch_lineage FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_stk.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stk.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_stk.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stk.audit_event FORCE ROW LEVEL SECURITY;

CREATE POLICY company_setting_scope ON brant_stk.company_setting
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY stock_balance_scope ON brant_stk.stock_balance
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY stock_movement_scope ON brant_stk.stock_movement
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY cost_layer_scope ON brant_stk.cost_layer
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY cost_allocation_scope ON brant_stk.cost_allocation
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY stock_reservation_scope ON brant_stk.stock_reservation
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY batch_lineage_scope ON brant_stk.batch_lineage
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY action_result_scope ON brant_stk.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_stk.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
