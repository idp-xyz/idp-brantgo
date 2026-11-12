-- FUL: shipments with their lines, parties and packing records, the order fulfilment root, and
-- ShipmentFacts v1 (tactical model FUL-AGG-Shipment, FUL-AGG-OrderFulfilment, SM-FUL-SHIPMENT,
-- FUL-INV-001, FUL-INV-003; Context Map R26 QLT->FUL, R27 TRD->FUL, R44 PTY->FUL, R59 PUR->FUL,
-- R70 FUL->TRD, R74 EVD->FUL; WBS S5-03a; owner decisions D-1, D-3, D-10, D-19).
--
-- An order fulfilment is the root of one sales order in FUL: one row per order, created by the
-- order's first shipment and never changed. shipment.prepare and shipment.confirm lock it FOR UPDATE
-- after the shipment, so the confirmations of an order commit one at a time; what an order shipped
-- is derived from the lines of its confirmed shipments and stored nowhere else (D-3).
--
-- A shipment is prepared once with its lines and the parties its confirmation screens, binding one
-- sales order at an order revision and hash. Each line ships a quantity of an order line — never
-- more than the line's quantity at that revision — from one line of a PUR supplier delivery at its
-- version, under one QLT quality release. A prepared shipment takes packing records (a plan or an
-- actual packing; each replaces the one before of its kind and is kept), and ends confirmed or
-- cancelled, once (SM-FUL-SHIPMENT). The confirmation writes, with the state, the guard evidence of
-- the confirming transaction as canonical JSON with its SHA-256, and the extension points it found
-- not wired. At the confirmation the confirmed quantity of every order line, this shipment
-- included, is at most the line's quantity (FUL-INV-001, zero tolerance): the trigger recomputes it
-- from the rows as the confirming command, holding the order's fulfilment root, sees them.
--
-- brant_ful.order_shipped_v1 publishes ShipmentFacts v1 to TRD (R70): a lock-free read TRD makes in
-- its own transaction after it locked the order row, which shipment.confirm holds FOR SHARE through
-- TRD's OrderGuard v1 until it commits.
--
-- References to TRD, PUR, QLT, PTY and EVD are plain IDs and hashes: no foreign key crosses a schema
-- (DB2). action_result and audit_event are FUL's command ledger (internal/foundation/action).

CREATE SCHEMA brant_ful;

CREATE TABLE brant_ful.order_fulfilment (
    tenant_id      text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id     text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_id       text        NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    opened_by_kind text        NOT NULL CHECK (opened_by_kind IN ('human', 'ai')),
    opened_by_id   text        NOT NULL,
    opened_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, order_id)
);

-- A fulfilment root is only inserted. Commands lock it FOR UPDATE and change nothing.
CREATE FUNCTION brant_ful.guard_order_fulfilment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'brant_ful.order_fulfilment: a fulfilment root never changes and is never deleted'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER order_fulfilment_guard BEFORE UPDATE OR DELETE ON brant_ful.order_fulfilment
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_order_fulfilment();

CREATE TABLE brant_ful.shipment (
    tenant_id            text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id           text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    shipment_id          text        NOT NULL CHECK (shipment_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status               text        NOT NULL CHECK (status IN ('prepared', 'confirmed', 'cancelled')),
    row_version          bigint      NOT NULL CHECK (row_version > 0),
    route                text        NOT NULL CHECK (route = 'supplier_direct'),
    order_id             text        NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_revision       bigint      NOT NULL CHECK (order_revision > 0),
    order_hash           text        NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    customer_id          text        NOT NULL CHECK (customer_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    line_count           integer     NOT NULL CHECK (line_count BETWEEN 1 AND 200),
    party_count          integer     NOT NULL CHECK (party_count BETWEEN 1 AND 100),
    packing_records      bigint      NOT NULL CHECK (packing_records >= 0),
    request_hash         text        NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    owner_actor_id       text        NOT NULL,
    owner_team_id        text,
    prepared_by_kind     text        NOT NULL CHECK (prepared_by_kind IN ('human', 'ai')),
    prepared_by_id       text        NOT NULL,
    prepare_epoch        bigint      NOT NULL CHECK (prepare_epoch > 0),
    prepared_at          timestamptz NOT NULL,
    confirmed_by_kind    text,
    confirmed_by_id      text,
    confirm_epoch        bigint,
    confirmed_at         timestamptz,
    confirm_evidence_ids text[],
    -- The guard evidence of the confirming transaction as canonical JSON
    -- ([{"name": ..., "value": ...}, ...]) and its SHA-256; the extension points found not wired.
    guard_entries        text,
    guard_entries_sha256 text,
    unchecked_guards     text[],
    cancelled_by_kind    text,
    cancelled_by_id      text,
    cancel_epoch         bigint,
    cancelled_at         timestamptz,
    cancel_reason        text,
    PRIMARY KEY (tenant_id, company_id, shipment_id),
    FOREIGN KEY (tenant_id, company_id, order_id) REFERENCES brant_ful.order_fulfilment (tenant_id, company_id, order_id),
    CHECK (owner_actor_id = prepared_by_id),
    CHECK (num_nulls(confirmed_by_kind, confirmed_by_id, confirm_epoch, confirmed_at, confirm_evidence_ids, guard_entries,
                     guard_entries_sha256, unchecked_guards) IN (0, 8)),
    CHECK (confirmed_at IS NULL OR (confirmed_by_kind = 'human' AND confirm_epoch > 0 AND confirmed_at >= prepared_at
        AND cardinality(confirm_evidence_ids) <= 20 AND left(guard_entries, 1) = '['
        AND guard_entries_sha256 = encode(sha256(convert_to(guard_entries, 'UTF8')), 'hex'))),
    CHECK (num_nulls(cancelled_by_kind, cancelled_by_id, cancel_epoch, cancelled_at, cancel_reason) IN (0, 5)),
    CHECK (cancelled_at IS NULL OR (cancelled_by_kind IN ('human', 'ai') AND cancel_epoch > 0 AND cancelled_at >= prepared_at
        AND char_length(cancel_reason) BETWEEN 1 AND 1000)),
    CHECK (confirmed_at IS NULL OR cancelled_at IS NULL),
    CHECK (status = CASE WHEN confirmed_at IS NOT NULL THEN 'confirmed' WHEN cancelled_at IS NOT NULL THEN 'cancelled' ELSE 'prepared' END),
    CHECK (row_version = 1 + packing_records + (status <> 'prepared')::int)
);

CREATE INDEX shipment_order ON brant_ful.shipment (tenant_id, company_id, order_id);

CREATE TABLE brant_ful.shipment_line (
    tenant_id           text          NOT NULL,
    company_id          text          NOT NULL,
    shipment_id         text          NOT NULL,
    line_id             text          NOT NULL CHECK (line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    line_no             integer       NOT NULL CHECK (line_no > 0),
    order_line_id       text          NOT NULL CHECK (order_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    sku_id              text          NOT NULL CHECK (sku_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    spec_revision       bigint        NOT NULL CHECK (spec_revision > 0),
    unit_id             text          NOT NULL CHECK (unit_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    unit_revision       bigint        NOT NULL CHECK (unit_revision > 0),
    -- the order line's quantity at the order revision the shipment binds
    order_line_quantity numeric(18,6) NOT NULL CHECK (order_line_quantity > 0),
    quantity            numeric(18,6) NOT NULL CHECK (quantity > 0),
    delivery_id         text          NOT NULL CHECK (delivery_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    delivery_line_id    text          NOT NULL CHECK (delivery_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    delivery_version    bigint        NOT NULL CHECK (delivery_version > 0),
    commitment_id       text          NOT NULL CHECK (commitment_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    supplier_id         text          NOT NULL CHECK (supplier_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    quality_release_id  text          NOT NULL CHECK (quality_release_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    PRIMARY KEY (tenant_id, company_id, shipment_id, line_id),
    UNIQUE (tenant_id, company_id, shipment_id, line_no),
    FOREIGN KEY (tenant_id, company_id, shipment_id) REFERENCES brant_ful.shipment (tenant_id, company_id, shipment_id),
    CHECK (quantity <= order_line_quantity)
);

CREATE INDEX shipment_line_release ON brant_ful.shipment_line (tenant_id, company_id, quality_release_id);
CREATE INDEX shipment_line_delivery ON brant_ful.shipment_line (tenant_id, company_id, delivery_id, delivery_line_id);

-- The parties a confirmation screens (R44): the order's customer and the party of every address
-- role the order binds, as TRD sealed them.
CREATE TABLE brant_ful.shipment_party (
    tenant_id      text    NOT NULL,
    company_id     text    NOT NULL,
    shipment_id    text    NOT NULL,
    party_no       integer NOT NULL CHECK (party_no > 0),
    role           text    NOT NULL CHECK (role ~ '^[a-z][a-z0-9_]{0,63}$'),
    party_id       text    NOT NULL CHECK (party_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    party_revision bigint  NOT NULL CHECK (party_revision > 0),
    address_id     text    CHECK (address_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    PRIMARY KEY (tenant_id, company_id, shipment_id, party_no),
    FOREIGN KEY (tenant_id, company_id, shipment_id) REFERENCES brant_ful.shipment (tenant_id, company_id, shipment_id),
    CHECK ((role = 'customer') = (address_id IS NULL))
);

-- A packing record of a prepared shipment: the plan or the actual packing, numbered from 1 within
-- its kind; the record with the highest number of a kind is the current one.
CREATE TABLE brant_ful.shipment_packing (
    tenant_id        text        NOT NULL,
    company_id       text        NOT NULL,
    shipment_id      text        NOT NULL,
    kind             text        NOT NULL CHECK (kind IN ('plan', 'actual')),
    seq              bigint      NOT NULL CHECK (seq > 0),
    shipment_version bigint      NOT NULL CHECK (shipment_version > 1),
    package_count    integer     NOT NULL CHECK (package_count BETWEEN 1 AND 500),
    carton_count     bigint      NOT NULL CHECK (carton_count > 0),
    evidence_ids     text[]      NOT NULL CHECK (cardinality(evidence_ids) <= 20),
    recorded_by_kind text        NOT NULL CHECK (recorded_by_kind IN ('human', 'ai')),
    recorded_by_id   text        NOT NULL,
    record_epoch     bigint      NOT NULL CHECK (record_epoch > 0),
    recorded_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, shipment_id, kind, seq),
    UNIQUE (tenant_id, company_id, shipment_id, shipment_version),
    FOREIGN KEY (tenant_id, company_id, shipment_id) REFERENCES brant_ful.shipment (tenant_id, company_id, shipment_id)
);

-- A package is a group of equal cartons numbered carton_from to carton_from + cartons - 1; weights
-- and volume are per carton. An actual packing states them all.
CREATE TABLE brant_ful.shipment_package (
    tenant_id       text          NOT NULL,
    company_id      text          NOT NULL,
    shipment_id     text          NOT NULL,
    kind            text          NOT NULL,
    seq             bigint        NOT NULL,
    package_no      integer       NOT NULL CHECK (package_no > 0),
    cartons         bigint        NOT NULL CHECK (cartons BETWEEN 1 AND 999999),
    carton_from     bigint        CHECK (carton_from BETWEEN 1 AND 999999999),
    gross_weight_kg numeric(18,6) CHECK (gross_weight_kg > 0),
    net_weight_kg   numeric(18,6) CHECK (net_weight_kg > 0),
    volume_cbm      numeric(18,6) CHECK (volume_cbm > 0),
    PRIMARY KEY (tenant_id, company_id, shipment_id, kind, seq, package_no),
    FOREIGN KEY (tenant_id, company_id, shipment_id, kind, seq)
        REFERENCES brant_ful.shipment_packing (tenant_id, company_id, shipment_id, kind, seq),
    CHECK (kind <> 'actual' OR num_nulls(carton_from, gross_weight_kg, net_weight_kg, volume_cbm) = 0),
    CHECK (net_weight_kg IS NULL OR gross_weight_kg IS NULL OR net_weight_kg <= gross_weight_kg)
);

CREATE TABLE brant_ful.shipment_package_content (
    tenant_id           text          NOT NULL,
    company_id          text          NOT NULL,
    shipment_id         text          NOT NULL,
    kind                text          NOT NULL,
    seq                 bigint        NOT NULL,
    package_no          integer       NOT NULL,
    line_id             text          NOT NULL,
    quantity_per_carton numeric(18,6) NOT NULL CHECK (quantity_per_carton > 0),
    PRIMARY KEY (tenant_id, company_id, shipment_id, kind, seq, package_no, line_id),
    FOREIGN KEY (tenant_id, company_id, shipment_id, kind, seq, package_no)
        REFERENCES brant_ful.shipment_package (tenant_id, company_id, shipment_id, kind, seq, package_no),
    FOREIGN KEY (tenant_id, company_id, shipment_id, line_id)
        REFERENCES brant_ful.shipment_line (tenant_id, company_id, shipment_id, line_id)
);

-- A shipment is inserted prepared at row version 1. A prepared shipment changes only by a packing
-- record (its version and its count of packing records move by one), by its confirmation or by its
-- cancellation, once; a confirmed or cancelled shipment never changes; nothing else about a
-- shipment changes and it is never deleted (SM-FUL-SHIPMENT). At the confirmation every order line
-- stays within its quantity over the order's confirmed shipments (FUL-INV-001).
CREATE FUNCTION brant_ful.guard_shipment() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_packing text[] := ARRAY['row_version', 'packing_records'];
    v_confirm text[] := ARRAY['status', 'row_version', 'confirmed_by_kind', 'confirmed_by_id', 'confirm_epoch', 'confirmed_at',
                              'confirm_evidence_ids', 'guard_entries', 'guard_entries_sha256', 'unchecked_guards'];
    v_cancel  text[] := ARRAY['status', 'row_version', 'cancelled_by_kind', 'cancelled_by_id', 'cancel_epoch', 'cancelled_at',
                              'cancel_reason'];
    v_line    text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_ful.shipment: shipments are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'prepared' OR NEW.row_version <> 1 THEN
            RAISE EXCEPTION 'brant_ful.shipment: a shipment is inserted prepared at row version 1'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'prepared' AND NEW.row_version = OLD.row_version + 1 THEN
        IF NEW.status = 'prepared' AND NEW.packing_records = OLD.packing_records + 1
                AND (to_jsonb(NEW) - v_packing) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_packing) THEN
            RETURN NEW;
        END IF;
        IF NEW.status = 'cancelled' AND (to_jsonb(NEW) - v_cancel) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_cancel) THEN
            RETURN NEW;
        END IF;
        IF NEW.status = 'confirmed' AND (to_jsonb(NEW) - v_confirm) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_confirm) THEN
            SELECT l.order_line_id INTO v_line
            FROM (SELECT m.order_line_id, sum(m.quantity) AS shipping, min(m.order_line_quantity) AS ordered
                  FROM brant_ful.shipment_line m
                  WHERE m.tenant_id = NEW.tenant_id AND m.company_id = NEW.company_id AND m.shipment_id = NEW.shipment_id
                  GROUP BY m.order_line_id) l
            WHERE l.shipping + coalesce((SELECT sum(o.quantity) FROM brant_ful.shipment_line o
                    JOIN brant_ful.shipment s ON s.tenant_id = o.tenant_id AND s.company_id = o.company_id AND s.shipment_id = o.shipment_id
                    WHERE o.tenant_id = NEW.tenant_id AND o.company_id = NEW.company_id AND s.order_id = NEW.order_id
                        AND s.status = 'confirmed' AND o.order_line_id = l.order_line_id), 0) > l.ordered
            ORDER BY l.order_line_id COLLATE "C"
            LIMIT 1;
            IF FOUND THEN
                RAISE EXCEPTION 'brant_ful.shipment: confirming shipment % ships more of order line % than the line orders (FUL-INV-001)',
                    NEW.shipment_id, v_line USING ERRCODE = 'integrity_constraint_violation';
            END IF;
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'brant_ful.shipment: shipment % changes only by a packing record, its confirmation or its cancellation, once (SM-FUL-SHIPMENT)',
        OLD.shipment_id USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER shipment_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.shipment
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_shipment();

-- Lines, parties, packing records, packages and their contents are only inserted: lines and
-- parties with their shipment, packing records while the shipment is prepared.
CREATE FUNCTION brant_ful.guard_shipment_part() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_ful.%: rows never change and are never deleted', TG_TABLE_NAME
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_ful.shipment s WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id
            AND s.shipment_id = NEW.shipment_id AND s.status = 'prepared') THEN
        RAISE EXCEPTION 'brant_ful.%: shipment % is not prepared', TG_TABLE_NAME, NEW.shipment_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER shipment_line_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.shipment_line
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_shipment_part();
CREATE TRIGGER shipment_party_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.shipment_party
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_shipment_part();
CREATE TRIGGER shipment_packing_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.shipment_packing
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_shipment_part();
CREATE TRIGGER shipment_package_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.shipment_package
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_shipment_part();
CREATE TRIGGER shipment_package_content_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.shipment_package_content
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_shipment_part();

-- At commit: a shipment has exactly the lines and parties it counts, numbered from 1, and as many
-- packing records as it counts, numbered from 1 within their kind, each written at the row version
-- it gave the shipment.
CREATE FUNCTION brant_ful.check_shipment() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_ship    record;
    v_lines   bigint;
    v_last    bigint;
    v_parties bigint;
    v_plast   bigint;
    v_records bigint;
    v_bad     bigint;
BEGIN
    SELECT s.* INTO v_ship FROM brant_ful.shipment s
    WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id AND s.shipment_id = NEW.shipment_id;
    SELECT count(*), coalesce(max(l.line_no), 0) INTO v_lines, v_last FROM brant_ful.shipment_line l
    WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id AND l.shipment_id = NEW.shipment_id;
    SELECT count(*), coalesce(max(p.party_no), 0) INTO v_parties, v_plast FROM brant_ful.shipment_party p
    WHERE p.tenant_id = NEW.tenant_id AND p.company_id = NEW.company_id AND p.shipment_id = NEW.shipment_id;
    IF v_lines <> v_ship.line_count OR v_last <> v_ship.line_count OR v_parties <> v_ship.party_count OR v_plast <> v_ship.party_count THEN
        RAISE EXCEPTION 'brant_ful.shipment: shipment % counts % lines and % parties, the rows are % and %', v_ship.shipment_id,
            v_ship.line_count, v_ship.party_count, v_lines, v_parties USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT count(*), count(*) FILTER (WHERE k.seq <> k.expected OR k.shipment_version < 2 OR k.shipment_version > v_ship.packing_records + 1)
        INTO v_records, v_bad
    FROM (SELECT r.seq, r.shipment_version, row_number() OVER (PARTITION BY r.kind ORDER BY r.seq) AS expected
          FROM brant_ful.shipment_packing r
          WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.shipment_id = NEW.shipment_id) k;
    IF v_records <> v_ship.packing_records OR v_bad <> 0 THEN
        RAISE EXCEPTION 'brant_ful.shipment: shipment % counts % packing records, the rows are % (% out of order)', v_ship.shipment_id,
            v_ship.packing_records, v_records, v_bad USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER shipment_at_commit AFTER INSERT OR UPDATE ON brant_ful.shipment
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_shipment();
CREATE CONSTRAINT TRIGGER shipment_line_at_commit AFTER INSERT ON brant_ful.shipment_line
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_shipment();
CREATE CONSTRAINT TRIGGER shipment_party_at_commit AFTER INSERT ON brant_ful.shipment_party
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_shipment();
CREATE CONSTRAINT TRIGGER shipment_packing_counted_at_commit AFTER INSERT ON brant_ful.shipment_packing
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_shipment();

-- At commit: a packing record has exactly the packages it counts, numbered from 1, each with
-- contents; its cartons are the sum of its packages'; and its packages account for exactly the
-- quantity of every line of the shipment (BT11: 箱至订单数量一致).
CREATE FUNCTION brant_ful.check_shipment_packing() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_rec      record;
    v_packages bigint;
    v_last     bigint;
    v_cartons  numeric;
    v_empty    bigint;
    v_line     text;
BEGIN
    SELECT r.* INTO v_rec FROM brant_ful.shipment_packing r
    WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.shipment_id = NEW.shipment_id AND r.kind = NEW.kind
        AND r.seq = NEW.seq;
    SELECT count(*), coalesce(max(p.package_no), 0), coalesce(sum(p.cartons), 0),
           count(*) FILTER (WHERE NOT EXISTS (SELECT 1 FROM brant_ful.shipment_package_content c WHERE c.tenant_id = p.tenant_id
               AND c.company_id = p.company_id AND c.shipment_id = p.shipment_id AND c.kind = p.kind AND c.seq = p.seq
               AND c.package_no = p.package_no))
        INTO v_packages, v_last, v_cartons, v_empty
    FROM brant_ful.shipment_package p
    WHERE p.tenant_id = NEW.tenant_id AND p.company_id = NEW.company_id AND p.shipment_id = NEW.shipment_id AND p.kind = NEW.kind
        AND p.seq = NEW.seq;
    IF v_packages <> v_rec.package_count OR v_last <> v_rec.package_count OR v_cartons <> v_rec.carton_count OR v_empty <> 0 THEN
        RAISE EXCEPTION 'brant_ful.shipment_packing: record % % of shipment % counts % packages and % cartons, the rows are % and % (% empty)',
            v_rec.kind, v_rec.seq, v_rec.shipment_id, v_rec.package_count, v_rec.carton_count, v_packages, v_cartons, v_empty
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT l.line_id INTO v_line FROM brant_ful.shipment_line l
    WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id AND l.shipment_id = NEW.shipment_id
        AND l.quantity <> coalesce((SELECT sum(p.cartons * c.quantity_per_carton) FROM brant_ful.shipment_package_content c
            JOIN brant_ful.shipment_package p ON p.tenant_id = c.tenant_id AND p.company_id = c.company_id AND p.shipment_id = c.shipment_id
                AND p.kind = c.kind AND p.seq = c.seq AND p.package_no = c.package_no
            WHERE c.tenant_id = l.tenant_id AND c.company_id = l.company_id AND c.shipment_id = l.shipment_id AND c.kind = NEW.kind
                AND c.seq = NEW.seq AND c.line_id = l.line_id), 0)
    ORDER BY l.line_no
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'brant_ful.shipment_packing: record % % of shipment % does not pack the quantity of line % (BT11)', v_rec.kind,
            v_rec.seq, v_rec.shipment_id, v_line USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER shipment_packing_at_commit AFTER INSERT ON brant_ful.shipment_packing
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_shipment_packing();
CREATE CONSTRAINT TRIGGER shipment_package_at_commit AFTER INSERT ON brant_ful.shipment_package
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_shipment_packing();
CREATE CONSTRAINT TRIGGER shipment_package_content_at_commit AFTER INSERT ON brant_ful.shipment_package_content
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_shipment_packing();

-- ShipmentFacts v1 (Context Map R70 FUL->TRD "ShipmentFacts v1", OHS（事务内无锁读取）, guardLock false;
-- owner decision D-1). TRD calls it with its own executor inside its own transaction, after it
-- locked the order row FOR UPDATE, and gets one row per order line and unit that has a confirmed
-- shipment line in the caller's scope: the sum of the confirmed shipment lines of the order line
-- over every order revision (QuantityFormat text) and the number of confirmed shipments it came
-- from; no row for an order nothing shipped of. It takes no lock: shipment.confirm, the only command
-- that adds to the answer, holds the order row FOR SHARE through TRD's OrderGuard v1 until it
-- commits, so it either committed before TRD got its lock, and is counted, or waits for TRD. The
-- function is VOLATILE so that its query reads a snapshot taken when it runs, after the caller's
-- lock. It applies no data scope (own, team): the caller decided whether its actor sees the order.
-- SECURITY DEFINER runs it as the schema owner, which FORCE ROW LEVEL SECURITY still confines to
-- app.tenant_id and app.company_id; the predicates repeat it. search_path is fixed and every name
-- is qualified. brant_app gets EXECUTE in deploy/postgres/10_app_grants.sql; nobody else does.
CREATE FUNCTION brant_ful.order_shipped_v1(p_order_id text)
    RETURNS TABLE (order_line_id text, unit_id text, unit_revision bigint, shipped_quantity text, shipments bigint)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
BEGIN
    IF p_order_id IS NULL THEN
        RAISE EXCEPTION 'brant_ful.order_shipped_v1: order_id is required' USING ERRCODE = 'null_value_not_allowed';
    END IF;
    RETURN QUERY
    SELECT l.order_line_id, l.unit_id, l.unit_revision, pg_catalog.trim_scale(pg_catalog.sum(l.quantity))::text,
           pg_catalog.count(DISTINCT l.shipment_id)
    FROM brant_ful.shipment s
    JOIN brant_ful.shipment_line l ON l.tenant_id = s.tenant_id AND l.company_id = s.company_id AND l.shipment_id = s.shipment_id
    WHERE s.tenant_id = v_tenant AND s.company_id = v_company AND s.order_id = p_order_id AND s.status = 'confirmed'
    GROUP BY l.order_line_id, l.unit_id, l.unit_revision
    ORDER BY l.order_line_id COLLATE "C", l.unit_id COLLATE "C", l.unit_revision;
END
$$;

REVOKE ALL ON FUNCTION brant_ful.order_shipped_v1(text) FROM PUBLIC;

CREATE TABLE brant_ful.action_result (
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

CREATE TABLE brant_ful.audit_event (
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

ALTER TABLE brant_ful.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_ful.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_ful.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.order_fulfilment ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.order_fulfilment FORCE ROW LEVEL SECURITY;
CREATE POLICY order_fulfilment_scope ON brant_ful.order_fulfilment
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.shipment ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.shipment FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_scope ON brant_ful.shipment
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.shipment_line ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.shipment_line FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_line_scope ON brant_ful.shipment_line
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.shipment_party ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.shipment_party FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_party_scope ON brant_ful.shipment_party
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.shipment_packing ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.shipment_packing FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_packing_scope ON brant_ful.shipment_packing
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.shipment_package ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.shipment_package FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_package_scope ON brant_ful.shipment_package
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.shipment_package_content ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.shipment_package_content FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_package_content_scope ON brant_ful.shipment_package_content
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
