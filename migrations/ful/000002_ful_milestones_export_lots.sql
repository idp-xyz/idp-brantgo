-- FUL: the price of every shipment line, milestone plans with their actuals, and export lots
-- (tactical model FUL-AGG-MilestonePlan, FUL-AGG-ExportLot; UL FUL-MilestonePlan, FUL-ExportLot;
-- Context Map R30 FUL->STL, R36 FUL->CNX, R74 EVD->FUL; CN-R07, CN-AT02; SAP reference GTC-08, CN-05;
-- WBS S5-03b; owner decision D-3).
--
-- A shipment line's price is the order line's at the order revision the shipment binds, as TRD
-- sealed it: shipment.prepare writes it with the line, since TRD shows an order only at its current
-- revision. A shipment prepared from this migration on has a price for every line (at commit).
--
-- A milestone plan follows one object (a sales order or a shipment; one plan per object) through
-- nodes of a closed set of types. Its nodes are written per revision and kept; actuals are only
-- inserted, each at an evidence level — a supplier's claim, an internal verification (a person, with
-- evidence) or an accepted fact (a person, with evidence) — and never after the instant they were
-- recorded. A node that has actuals stays in every later revision with its type. Customs release,
-- actual departure, delivery and customer acceptance are distinct node types (CN-R07).
--
-- An export lot is what of one prepared or confirmed shipment goes into one export: lines naming
-- shipment lines with a quantity no larger than the line's and its amounts at the line's price. A
-- shipment line belongs to at most one lot; a lot is never changed or deleted. How a lot stands
-- against its shipment's confirmation is derived when it is read.
--
-- References to TRD, PTY and EVD are plain IDs: no foreign key crosses a schema (DB2).

CREATE TABLE brant_ful.shipment_line_price (
    tenant_id         text           NOT NULL,
    company_id        text           NOT NULL,
    shipment_id       text           NOT NULL,
    line_id           text           NOT NULL,
    currency_id       text           NOT NULL CHECK (currency_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    currency_revision bigint         NOT NULL CHECK (currency_revision > 0),
    unit_price        numeric(20,8)  NOT NULL CHECK (unit_price >= 0),
    -- the order line's amounts in minor units for its whole quantity at the bound revision
    net_minor         numeric(24,0)  NOT NULL CHECK (net_minor >= 0),
    tax_minor         numeric(24,0)  NOT NULL CHECK (tax_minor >= 0),
    payable_minor     numeric(24,0)  NOT NULL CHECK (payable_minor >= 0),
    PRIMARY KEY (tenant_id, company_id, shipment_id, line_id),
    FOREIGN KEY (tenant_id, company_id, shipment_id, line_id) REFERENCES brant_ful.shipment_line (tenant_id, company_id, shipment_id, line_id)
);

-- Prices are only inserted, while their shipment is prepared (the shipment parts' guard).
CREATE TRIGGER shipment_line_price_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.shipment_line_price
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_shipment_part();

-- At commit: a shipment inserted from this migration on has a price for every line, in one currency.
CREATE FUNCTION brant_ful.check_shipment_prices() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_prices     bigint;
    v_currencies bigint;
BEGIN
    SELECT count(*), count(DISTINCT (p.currency_id, p.currency_revision)) INTO v_prices, v_currencies
    FROM brant_ful.shipment_line_price p
    WHERE p.tenant_id = NEW.tenant_id AND p.company_id = NEW.company_id AND p.shipment_id = NEW.shipment_id;
    IF v_prices <> NEW.line_count OR v_currencies <> 1 THEN
        RAISE EXCEPTION 'brant_ful.shipment: shipment % has % lines and % prices in % currencies', NEW.shipment_id, NEW.line_count, v_prices,
            v_currencies USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER shipment_priced_at_commit AFTER INSERT ON brant_ful.shipment
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_shipment_prices();

CREATE TABLE brant_ful.milestone_plan (
    tenant_id        text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id       text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    plan_id          text        NOT NULL CHECK (plan_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status           text        NOT NULL CHECK (status = 'active'),
    row_version      bigint      NOT NULL CHECK (row_version > 0),
    plan_revision    bigint      NOT NULL CHECK (plan_revision > 0),
    node_count       integer     NOT NULL CHECK (node_count BETWEEN 1 AND 100),
    actual_count     bigint      NOT NULL CHECK (actual_count >= 0),
    subject_kind     text        NOT NULL CHECK (subject_kind IN ('order', 'shipment')),
    subject_id       text        NOT NULL CHECK (subject_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    request_hash     text        NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    owner_actor_id   text        NOT NULL,
    owner_team_id    text,
    created_by_kind  text        NOT NULL CHECK (created_by_kind IN ('human', 'ai')),
    created_by_id    text        NOT NULL,
    create_epoch     bigint      NOT NULL CHECK (create_epoch > 0),
    created_at       timestamptz NOT NULL,
    revised_by_kind  text        NOT NULL CHECK (revised_by_kind IN ('human', 'ai')),
    revised_by_id    text        NOT NULL,
    revise_epoch     bigint      NOT NULL CHECK (revise_epoch > 0),
    revised_at       timestamptz NOT NULL,
    revision_reason  text        CHECK (char_length(revision_reason) BETWEEN 1 AND 1000),
    PRIMARY KEY (tenant_id, company_id, plan_id),
    -- one plan per object
    UNIQUE (tenant_id, company_id, subject_kind, subject_id),
    CHECK (owner_actor_id = created_by_id),
    CHECK (revised_at >= created_at),
    CHECK (row_version = plan_revision + actual_count)
);

-- A node of a plan revision. Every revision writes all its nodes; earlier revisions are kept.
CREATE TABLE brant_ful.milestone_node (
    tenant_id        text    NOT NULL,
    company_id       text    NOT NULL,
    plan_id          text    NOT NULL,
    plan_revision    bigint  NOT NULL CHECK (plan_revision > 0),
    node_id          text    NOT NULL CHECK (node_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    node_no          integer NOT NULL CHECK (node_no BETWEEN 1 AND 100),
    node_type        text    NOT NULL CHECK (node_type IN ('material_ready', 'production_start', 'production_complete', 'inspection',
                                 'packing_complete', 'supplier_handover', 'booking_confirmed', 'cargo_loaded', 'customs_release',
                                 'actual_departure', 'arrival', 'delivery', 'customer_acceptance')),
    planned_on       date    NOT NULL,
    responsible_kind text    CHECK (responsible_kind IN ('actor', 'party')),
    responsible_id   text    CHECK (responsible_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    predecessors     text[]  NOT NULL CHECK (cardinality(predecessors) <= 20 AND NOT (node_id = ANY (predecessors))),
    PRIMARY KEY (tenant_id, company_id, plan_id, plan_revision, node_id),
    UNIQUE (tenant_id, company_id, plan_id, plan_revision, node_no),
    FOREIGN KEY (tenant_id, company_id, plan_id) REFERENCES brant_ful.milestone_plan (tenant_id, company_id, plan_id),
    CHECK ((responsible_kind IS NULL) = (responsible_id IS NULL))
);

-- An actual: what happened at a node of the revision it was recorded under, at an evidence level.
CREATE TABLE brant_ful.milestone_actual (
    tenant_id        text        NOT NULL,
    company_id       text        NOT NULL,
    plan_id          text        NOT NULL,
    seq              bigint      NOT NULL CHECK (seq > 0),
    plan_revision    bigint      NOT NULL,
    node_id          text        NOT NULL,
    node_type        text        NOT NULL,
    evidence_level   text        NOT NULL CHECK (evidence_level IN ('supplier_claim', 'internal_verified', 'accepted')),
    occurred_at      timestamptz NOT NULL,
    evidence_ids     text[]      NOT NULL CHECK (cardinality(evidence_ids) <= 20),
    note             text        CHECK (char_length(note) BETWEEN 1 AND 1000),
    plan_version     bigint      NOT NULL CHECK (plan_version > 1),
    recorded_by_kind text        NOT NULL CHECK (recorded_by_kind IN ('human', 'ai')),
    recorded_by_id   text        NOT NULL,
    record_epoch     bigint      NOT NULL CHECK (record_epoch > 0),
    recorded_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, plan_id, seq),
    UNIQUE (tenant_id, company_id, plan_id, plan_version),
    FOREIGN KEY (tenant_id, company_id, plan_id, plan_revision, node_id)
        REFERENCES brant_ful.milestone_node (tenant_id, company_id, plan_id, plan_revision, node_id),
    -- 预计进度不是实际: an actual is never recorded ahead of what happened.
    CHECK (occurred_at <= recorded_at),
    -- An internal verification or an accepted fact is a person's, on evidence.
    CHECK (evidence_level = 'supplier_claim' OR (recorded_by_kind = 'human' AND cardinality(evidence_ids) >= 1))
);

CREATE INDEX milestone_actual_node ON brant_ful.milestone_actual (tenant_id, company_id, plan_id, node_id);

-- A plan is inserted at row version 1, revision 1, without actuals. It then changes only by a
-- revision (its version and revision move by one, with who revised it) or by an actual (its version
-- and count of actuals move by one); its object, owner and creation never change; it is never
-- deleted.
CREATE FUNCTION brant_ful.guard_milestone_plan() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_revise text[] := ARRAY['row_version', 'plan_revision', 'node_count', 'revised_by_kind', 'revised_by_id', 'revise_epoch', 'revised_at',
                             'revision_reason'];
    v_actual text[] := ARRAY['row_version', 'actual_count'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_ful.milestone_plan: plans are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.row_version <> 1 OR NEW.plan_revision <> 1 OR NEW.actual_count <> 0 OR NEW.revised_at <> NEW.created_at THEN
            RAISE EXCEPTION 'brant_ful.milestone_plan: a plan is inserted at row version 1, revision 1, without actuals'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.row_version = OLD.row_version + 1 THEN
        IF NEW.plan_revision = OLD.plan_revision + 1 AND (to_jsonb(NEW) - v_revise) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_revise) THEN
            RETURN NEW;
        END IF;
        IF NEW.actual_count = OLD.actual_count + 1 AND (to_jsonb(NEW) - v_actual) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_actual) THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'brant_ful.milestone_plan: plan % changes only by a revision or an actual, one at a time', OLD.plan_id
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER milestone_plan_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.milestone_plan
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_milestone_plan();

-- Nodes and actuals are only inserted; an actual carries the type of the node it names.
CREATE FUNCTION brant_ful.guard_milestone_part() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_ful.%: rows never change and are never deleted', TG_TABLE_NAME
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_TABLE_NAME = 'milestone_actual' AND NOT EXISTS (SELECT 1 FROM brant_ful.milestone_node n WHERE n.tenant_id = NEW.tenant_id
            AND n.company_id = NEW.company_id AND n.plan_id = NEW.plan_id AND n.plan_revision = NEW.plan_revision AND n.node_id = NEW.node_id
            AND n.node_type = NEW.node_type) THEN
        RAISE EXCEPTION 'brant_ful.milestone_actual: node % of plan % is not of type %', NEW.node_id, NEW.plan_id, NEW.node_type
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER milestone_node_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.milestone_node
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_milestone_part();
CREATE TRIGGER milestone_actual_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.milestone_actual
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_milestone_part();

-- At commit: the plan's current revision has exactly the nodes it counts, numbered from 1, and every
-- revision before it is complete too; the plan has exactly the actuals it counts, numbered from 1,
-- recorded under a revision it had, at the row version each gave it; and every node that has actuals
-- is in the current revision with the type they were recorded under.
CREATE FUNCTION brant_ful.check_milestone_plan() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_plan    record;
    v_nodes   bigint;
    v_last    bigint;
    v_revs    bigint;
    v_actuals bigint;
    v_bad     bigint;
    v_lost    text;
BEGIN
    SELECT p.* INTO v_plan FROM brant_ful.milestone_plan p
    WHERE p.tenant_id = NEW.tenant_id AND p.company_id = NEW.company_id AND p.plan_id = NEW.plan_id;
    SELECT count(*), coalesce(max(n.node_no), 0) INTO v_nodes, v_last FROM brant_ful.milestone_node n
    WHERE n.tenant_id = NEW.tenant_id AND n.company_id = NEW.company_id AND n.plan_id = NEW.plan_id AND n.plan_revision = v_plan.plan_revision;
    SELECT count(DISTINCT n.plan_revision), count(*) FILTER (WHERE n.plan_revision > v_plan.plan_revision) INTO v_revs, v_bad
    FROM brant_ful.milestone_node n
    WHERE n.tenant_id = NEW.tenant_id AND n.company_id = NEW.company_id AND n.plan_id = NEW.plan_id;
    IF v_nodes <> v_plan.node_count OR v_last <> v_plan.node_count OR v_revs <> v_plan.plan_revision OR v_bad <> 0 THEN
        RAISE EXCEPTION 'brant_ful.milestone_plan: plan % revision % counts % nodes, the rows are % (% revisions)', v_plan.plan_id,
            v_plan.plan_revision, v_plan.node_count, v_nodes, v_revs USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT count(*), count(*) FILTER (WHERE k.seq <> k.expected OR k.plan_revision > v_plan.plan_revision OR k.plan_version > v_plan.row_version)
        INTO v_actuals, v_bad
    FROM (SELECT a.seq, a.plan_revision, a.plan_version, row_number() OVER (ORDER BY a.seq) AS expected
          FROM brant_ful.milestone_actual a
          WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id AND a.plan_id = NEW.plan_id) k;
    IF v_actuals <> v_plan.actual_count OR v_bad <> 0 THEN
        RAISE EXCEPTION 'brant_ful.milestone_plan: plan % counts % actuals, the rows are % (% out of order)', v_plan.plan_id,
            v_plan.actual_count, v_actuals, v_bad USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT a.node_id INTO v_lost FROM brant_ful.milestone_actual a
    WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id AND a.plan_id = NEW.plan_id
        AND NOT EXISTS (SELECT 1 FROM brant_ful.milestone_node n WHERE n.tenant_id = a.tenant_id AND n.company_id = a.company_id
            AND n.plan_id = a.plan_id AND n.plan_revision = v_plan.plan_revision AND n.node_id = a.node_id AND n.node_type = a.node_type)
    ORDER BY a.seq
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'brant_ful.milestone_plan: node % of plan % has actuals and left the plan or changed its type', v_lost, v_plan.plan_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER milestone_plan_at_commit AFTER INSERT OR UPDATE ON brant_ful.milestone_plan
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_milestone_plan();
CREATE CONSTRAINT TRIGGER milestone_node_at_commit AFTER INSERT ON brant_ful.milestone_node
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_milestone_plan();
CREATE CONSTRAINT TRIGGER milestone_actual_at_commit AFTER INSERT ON brant_ful.milestone_actual
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_milestone_plan();

CREATE TABLE brant_ful.export_lot (
    tenant_id          text          NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id         text          NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    export_lot_id      text          NOT NULL CHECK (export_lot_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status             text          NOT NULL CHECK (status = 'registered'),
    row_version        bigint        NOT NULL CHECK (row_version = 1),
    shipment_id        text          NOT NULL,
    -- the shipment's state and row version when the lot was registered
    shipment_status    text          NOT NULL CHECK (shipment_status IN ('prepared', 'confirmed')),
    shipment_version   bigint        NOT NULL CHECK (shipment_version > 0),
    order_id           text          NOT NULL,
    order_revision     bigint        NOT NULL CHECK (order_revision > 0),
    order_hash         text          NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    customer_id        text          NOT NULL,
    currency_id        text          NOT NULL CHECK (currency_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    currency_revision  bigint        NOT NULL CHECK (currency_revision > 0),
    line_count         integer       NOT NULL CHECK (line_count BETWEEN 1 AND 200),
    net_minor          numeric(24,0) NOT NULL CHECK (net_minor >= 0),
    tax_minor          numeric(24,0) NOT NULL CHECK (tax_minor >= 0),
    payable_minor      numeric(24,0) NOT NULL CHECK (payable_minor = net_minor + tax_minor),
    request_hash       text          NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    owner_actor_id     text          NOT NULL,
    owner_team_id      text,
    registered_by_kind text          NOT NULL CHECK (registered_by_kind IN ('human', 'ai')),
    registered_by_id   text          NOT NULL,
    register_epoch     bigint        NOT NULL CHECK (register_epoch > 0),
    registered_at      timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, export_lot_id),
    UNIQUE (tenant_id, company_id, export_lot_id, shipment_id),
    FOREIGN KEY (tenant_id, company_id, shipment_id) REFERENCES brant_ful.shipment (tenant_id, company_id, shipment_id),
    CHECK (owner_actor_id = registered_by_id)
);

CREATE INDEX export_lot_shipment ON brant_ful.export_lot (tenant_id, company_id, shipment_id);

CREATE TABLE brant_ful.export_lot_line (
    tenant_id        text          NOT NULL,
    company_id       text          NOT NULL,
    export_lot_id    text          NOT NULL,
    line_id          text          NOT NULL CHECK (line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    line_no          integer       NOT NULL CHECK (line_no BETWEEN 1 AND 200),
    shipment_id      text          NOT NULL,
    shipment_line_id text          NOT NULL,
    quantity         numeric(18,6) NOT NULL CHECK (quantity > 0),
    net_minor        numeric(24,0) NOT NULL CHECK (net_minor >= 0),
    tax_minor        numeric(24,0) NOT NULL CHECK (tax_minor >= 0),
    payable_minor    numeric(24,0) NOT NULL CHECK (payable_minor = net_minor + tax_minor),
    PRIMARY KEY (tenant_id, company_id, export_lot_id, line_id),
    UNIQUE (tenant_id, company_id, export_lot_id, line_no),
    -- a shipment line goes into one export lot: never a second stock
    UNIQUE (tenant_id, company_id, shipment_id, shipment_line_id),
    FOREIGN KEY (tenant_id, company_id, export_lot_id, shipment_id)
        REFERENCES brant_ful.export_lot (tenant_id, company_id, export_lot_id, shipment_id),
    FOREIGN KEY (tenant_id, company_id, shipment_id, shipment_line_id)
        REFERENCES brant_ful.shipment_line (tenant_id, company_id, shipment_id, line_id)
);

-- A lot and its lines are only inserted. A lot is registered of a shipment in the state and at the
-- row version it records, never of a cancelled one; a line takes no more than its shipment line
-- ships.
CREATE FUNCTION brant_ful.guard_export_lot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_ful.%: export lots never change and are never deleted', TG_TABLE_NAME
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_TABLE_NAME = 'export_lot' THEN
        IF NOT EXISTS (SELECT 1 FROM brant_ful.shipment s WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id
                AND s.shipment_id = NEW.shipment_id AND s.status = NEW.shipment_status AND s.row_version = NEW.shipment_version
                AND s.order_id = NEW.order_id AND s.order_revision = NEW.order_revision AND s.order_hash = NEW.order_hash) THEN
            RAISE EXCEPTION 'brant_ful.export_lot: shipment % is not % at version %', NEW.shipment_id, NEW.shipment_status, NEW.shipment_version
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSE
        IF NOT EXISTS (SELECT 1 FROM brant_ful.shipment_line l WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id
                AND l.shipment_id = NEW.shipment_id AND l.line_id = NEW.shipment_line_id AND l.quantity >= NEW.quantity) THEN
            RAISE EXCEPTION 'brant_ful.export_lot_line: line % of lot % takes more than shipment line % ships', NEW.line_id, NEW.export_lot_id,
                NEW.shipment_line_id USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER export_lot_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.export_lot
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_export_lot();
CREATE TRIGGER export_lot_line_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.export_lot_line
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_export_lot();

-- At commit: a lot has exactly the lines it counts, numbered from 1, and its amounts are their sums.
CREATE FUNCTION brant_ful.check_export_lot() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_lot   record;
    v_lines bigint;
    v_last  bigint;
    v_net   numeric;
    v_tax   numeric;
BEGIN
    SELECT x.* INTO v_lot FROM brant_ful.export_lot x
    WHERE x.tenant_id = NEW.tenant_id AND x.company_id = NEW.company_id AND x.export_lot_id = NEW.export_lot_id;
    SELECT count(*), coalesce(max(l.line_no), 0), coalesce(sum(l.net_minor), 0), coalesce(sum(l.tax_minor), 0)
        INTO v_lines, v_last, v_net, v_tax
    FROM brant_ful.export_lot_line l
    WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id AND l.export_lot_id = NEW.export_lot_id;
    IF v_lines <> v_lot.line_count OR v_last <> v_lot.line_count OR v_net <> v_lot.net_minor OR v_tax <> v_lot.tax_minor THEN
        RAISE EXCEPTION 'brant_ful.export_lot: lot % counts % lines, the rows are %, or its amounts are not their sums', v_lot.export_lot_id,
            v_lot.line_count, v_lines USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER export_lot_at_commit AFTER INSERT ON brant_ful.export_lot
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_export_lot();
CREATE CONSTRAINT TRIGGER export_lot_line_at_commit AFTER INSERT ON brant_ful.export_lot_line
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_export_lot();

ALTER TABLE brant_ful.shipment_line_price ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.shipment_line_price FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_line_price_scope ON brant_ful.shipment_line_price
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.milestone_plan ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.milestone_plan FORCE ROW LEVEL SECURITY;
CREATE POLICY milestone_plan_scope ON brant_ful.milestone_plan
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.milestone_node ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.milestone_node FORCE ROW LEVEL SECURITY;
CREATE POLICY milestone_node_scope ON brant_ful.milestone_node
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.milestone_actual ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.milestone_actual FORCE ROW LEVEL SECURITY;
CREATE POLICY milestone_actual_scope ON brant_ful.milestone_actual
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.export_lot ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.export_lot FORCE ROW LEVEL SECURITY;
CREATE POLICY export_lot_scope ON brant_ful.export_lot
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.export_lot_line ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.export_lot_line FORCE ROW LEVEL SECURITY;
CREATE POLICY export_lot_line_scope ON brant_ful.export_lot_line
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
