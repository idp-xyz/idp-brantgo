-- PUR: supplier deliveries on the direct delivery route (WBS S5-15; tactical model
-- PUR-AGG-SupplierDelivery, SM-PUR-SUPPLIER-DELIVERY, PUR-INV-008; UL-INV-32; fact authority MD-45;
-- S5 decisions D-3 and D-10; Python V0.50 has nothing for it).
--
-- A supplier delivery is what a supplier handed over against the lines of one issued purchase
-- commitment: quantities, the handover place (UN/LOCODE), the time and the evidence. What is
-- recorded is a claim — the supplier's own report, entered by a human or an AI — and never counts.
-- A human other than the recorder verifies it line by line (verified ≤ claimed) or rejects it; only
-- the verified quantities are a fact, the source of shipments (FUL) and of purchase settlement
-- (STL). A delivery has versions: a correction is a new version, recorded as a claim again, and
-- when it is verified the version verified before is superseded. Nothing is edited or deleted.
--
--   supplier_delivery          the root: its commitment, how many versions it has (latest_version)
--                              and which one is verified now (verified_version); every command
--                              moves row_version by one;
--   supplier_delivery_version  one claim: recorded → verified | rejected, verified → superseded;
--   supplier_delivery_line     a line of a version: one line of the commitment (the order line its
--                              draft buys, which is the delivery line's ID in every version), the
--                              claimed quantity and, once verified, the verified one.
--
-- PUR-INV-008: for every commitment line the verified quantities of the versions verified now never
-- exceed what the commitment buys (the draft line's allocated quantity plus its approved overbuy;
-- no tolerance is configured in S5). The sum is derived from the immutable verified lines; a
-- delivery command locks the delivery root FOR UPDATE and then its commitment root FOR UPDATE, on
-- which the verifications of one commitment serialize (S5 decision D-3: the commitment is locked,
-- not written), and a deferred constraint trigger checks the sum again at commit.
--
-- brant_pur.guard_supplier_delivery_v1 publishes SupplierDeliveryGuard v1 to FUL (Context Map R59,
-- guardLock): it locks the one delivery root row FOR SHARE in FUL's transaction, which every
-- delivery command locks FOR UPDATE, so a correction and a shipment confirmation have one order
-- (FUL-INV-003). PUR locks no row of another context.
--
-- References to EVD are plain IDs and hashes: no foreign key crosses a schema (DB2).

CREATE TABLE brant_pur.supplier_delivery (
    tenant_id        text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id       text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    delivery_id      text        NOT NULL CHECK (delivery_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    row_version      bigint      NOT NULL CHECK (row_version > 0),
    commitment_id    text        NOT NULL,
    order_id         text        NOT NULL,
    supplier_id      text        NOT NULL,
    latest_version   bigint      NOT NULL CHECK (latest_version > 0),
    verified_version bigint      CHECK (verified_version > 0),
    owner_actor_id   text        NOT NULL,
    owner_team_id    text,
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, delivery_id),
    FOREIGN KEY (tenant_id, company_id, commitment_id) REFERENCES brant_pur.purchase_commitment (tenant_id, company_id, commitment_id),
    CHECK (verified_version <= latest_version),
    CHECK (row_version >= latest_version),
    CHECK (updated_at >= created_at)
);

CREATE INDEX supplier_delivery_commitment ON brant_pur.supplier_delivery (tenant_id, company_id, commitment_id);

-- A delivery is recorded against an issued (or confirmed) commitment, whose order, supplier and
-- owner it takes: who sees the commitment sees its deliveries. Afterwards only its version counters
-- move, the row version by one per command. It is never deleted.
CREATE FUNCTION brant_pur.guard_supplier_delivery() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_moving text[] := ARRAY['row_version', 'latest_version', 'verified_version', 'updated_at'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pur.supplier_delivery: deliveries are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.row_version <> 1 OR NEW.latest_version <> 1 OR NEW.verified_version IS NOT NULL
                OR NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
                    AND c.commitment_id = NEW.commitment_id AND c.status IN ('issued', 'confirmed') AND c.order_id = NEW.order_id
                    AND c.supplier_id = NEW.supplier_id AND c.owner_actor_id = NEW.owner_actor_id
                    AND c.owner_team_id IS NOT DISTINCT FROM NEW.owner_team_id) THEN
            RAISE EXCEPTION 'brant_pur.supplier_delivery: a delivery is recorded against an issued commitment and binds it exactly'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.row_version <> OLD.row_version + 1 OR NEW.latest_version NOT IN (OLD.latest_version, OLD.latest_version + 1)
            OR (to_jsonb(NEW) - v_moving) IS DISTINCT FROM (to_jsonb(OLD) - v_moving) THEN
        RAISE EXCEPTION 'brant_pur.supplier_delivery: delivery % only moves its versions, one command at a time', OLD.delivery_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER supplier_delivery_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pur.supplier_delivery
    FOR EACH ROW EXECUTE FUNCTION brant_pur.guard_supplier_delivery();

CREATE TABLE brant_pur.supplier_delivery_version (
    tenant_id             text        NOT NULL,
    company_id            text        NOT NULL,
    delivery_id           text        NOT NULL,
    version               bigint      NOT NULL CHECK (version > 0),
    status                text        NOT NULL CONSTRAINT supplier_delivery_version_status
        CHECK (status IN ('recorded', 'verified', 'rejected', 'superseded')),
    commitment_id         text        NOT NULL,
    -- UN/LOCODE of the place the goods were handed over at (forwarder, yard, consignee).
    handover_place        text        NOT NULL CHECK (handover_place ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
    handed_over_at        timestamptz NOT NULL,
    supplier_reference    text        CHECK (char_length(supplier_reference) BETWEEN 1 AND 64),
    correction_reason     text        CHECK (char_length(correction_reason) BETWEEN 1 AND 1000),
    line_count            integer     NOT NULL CHECK (line_count BETWEEN 1 AND 200),
    -- The EVD sources of the claim ([{"locator": ..., "source_id": ...}, ...]) and its EVD business
    -- files with the SHA-256 they had when recorded ([{"file_id": ..., "sha256": ...}, ...]).
    evidence              text        NOT NULL CHECK (left(evidence, 2) = '[{'),
    files                 text        NOT NULL CHECK (left(files, 1) = '['),
    request_hash          text        NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    recorded_by_kind      text        NOT NULL CHECK (recorded_by_kind IN ('human', 'ai')),
    recorded_by_id        text        NOT NULL,
    record_epoch          bigint      NOT NULL CHECK (record_epoch > 0),
    recorded_at           timestamptz NOT NULL,
    decided_by_kind       text        CHECK (decided_by_kind = 'human'),
    decided_by_id         text,
    decide_epoch          bigint      CHECK (decide_epoch > 0),
    decided_at            timestamptz,
    decision              text        CHECK (decision IN ('verify', 'reject')),
    decision_reason       text        CHECK (char_length(decision_reason) BETWEEN 1 AND 1000),
    -- The EVD sources the verifier judged the claim on, besides the claim's own.
    decision_evidence     text        CHECK (left(decision_evidence, 1) = '['),
    superseded_by_version bigint,
    superseded_at         timestamptz,
    PRIMARY KEY (tenant_id, company_id, delivery_id, version),
    FOREIGN KEY (tenant_id, company_id, delivery_id) REFERENCES brant_pur.supplier_delivery (tenant_id, company_id, delivery_id),
    -- A first version is the delivery as first claimed; every later one is a correction with its reason.
    CHECK ((version = 1) = (correction_reason IS NULL)),
    CHECK (handed_over_at <= recorded_at),
    CHECK ((decided_at IS NULL) = (decided_by_id IS NULL) AND (decided_by_id IS NULL) = (decided_by_kind IS NULL)
        AND (decided_by_id IS NULL) = (decide_epoch IS NULL) AND (decided_by_id IS NULL) = (decision IS NULL)
        AND (decided_by_id IS NULL) = (decision_evidence IS NULL) AND (decided_by_id IS NOT NULL OR decision_reason IS NULL)),
    CHECK ((superseded_at IS NULL) = (superseded_by_version IS NULL)),
    -- SM-PUR-SUPPLIER-DELIVERY: what each state carries.
    CHECK (CASE status
        WHEN 'recorded' THEN decided_at IS NULL AND superseded_at IS NULL
        WHEN 'verified' THEN decision = 'verify' AND superseded_at IS NULL
        WHEN 'rejected' THEN decision = 'reject' AND decision_reason IS NOT NULL AND superseded_at IS NULL
        ELSE decision = 'verify' AND superseded_by_version > version AND superseded_at >= decided_at END),
    -- The verifier is a human other than the recorder of the version (role templates, independence of
    -- supplier_delivery.verify from 交付的登记人), deciding after the record.
    CHECK (decided_by_id IS DISTINCT FROM recorded_by_id),
    CHECK (decided_at >= recorded_at)
);

-- One claim awaits its verification at a time, and one version is the verified one.
CREATE UNIQUE INDEX supplier_delivery_version_pending ON brant_pur.supplier_delivery_version (tenant_id, company_id, delivery_id)
    WHERE status = 'recorded';
CREATE UNIQUE INDEX supplier_delivery_version_verified ON brant_pur.supplier_delivery_version (tenant_id, company_id, delivery_id)
    WHERE status = 'verified';

-- A version is recorded as the delivery's next one, a claim against its issued commitment; then it
-- changes only along SM-PUR-SUPPLIER-DELIVERY, each step writing only its own columns: the decision
-- (verified once every line states its verified quantity, and — unless it supersedes the version
-- verified before, which a correction may bring down to nothing — at least one above zero; rejected
-- with no line verified), and the supersession by the pending correction that is being verified.
-- Nothing is deleted.
CREATE FUNCTION brant_pur.guard_supplier_delivery_version() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_decision  text[] := ARRAY['status', 'decided_by_kind', 'decided_by_id', 'decide_epoch', 'decided_at', 'decision', 'decision_reason',
        'decision_evidence'];
    v_supersede text[] := ARRAY['status', 'superseded_by_version', 'superseded_at'];
    v_changed   text[];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pur.supplier_delivery_version: delivery versions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'recorded'
                OR NOT EXISTS (SELECT 1 FROM brant_pur.supplier_delivery d
                    JOIN brant_pur.purchase_commitment c ON c.tenant_id = d.tenant_id AND c.company_id = d.company_id AND c.commitment_id = d.commitment_id
                    WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND d.delivery_id = NEW.delivery_id
                        AND d.commitment_id = NEW.commitment_id AND d.latest_version = NEW.version AND c.status IN ('issued', 'confirmed'))
                OR NEW.version <> 1 + (SELECT count(*) FROM brant_pur.supplier_delivery_version v WHERE v.tenant_id = NEW.tenant_id
                    AND v.company_id = NEW.company_id AND v.delivery_id = NEW.delivery_id) THEN
            RAISE EXCEPTION 'brant_pur.supplier_delivery_version: a version is recorded as the next claim of a delivery against an issued commitment'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    v_changed := CASE
        WHEN OLD.status = 'recorded' AND NEW.status IN ('verified', 'rejected') THEN v_decision
        WHEN OLD.status = 'verified' AND NEW.status = 'superseded' THEN v_supersede
    END;
    IF v_changed IS NULL OR (to_jsonb(NEW) - v_changed) IS DISTINCT FROM (to_jsonb(OLD) - v_changed)
            OR (NEW.status = 'verified' AND (EXISTS (SELECT 1 FROM brant_pur.supplier_delivery_line l WHERE l.tenant_id = NEW.tenant_id
                    AND l.company_id = NEW.company_id AND l.delivery_id = NEW.delivery_id AND l.version = NEW.version AND l.verified_quantity IS NULL)
                OR (NOT EXISTS (SELECT 1 FROM brant_pur.supplier_delivery_line l WHERE l.tenant_id = NEW.tenant_id
                        AND l.company_id = NEW.company_id AND l.delivery_id = NEW.delivery_id AND l.version = NEW.version AND l.verified_quantity > 0)
                    AND NOT EXISTS (SELECT 1 FROM brant_pur.supplier_delivery_version s WHERE s.tenant_id = NEW.tenant_id
                        AND s.company_id = NEW.company_id AND s.delivery_id = NEW.delivery_id AND s.superseded_by_version = NEW.version))))
            OR (NEW.status = 'rejected' AND EXISTS (SELECT 1 FROM brant_pur.supplier_delivery_line l WHERE l.tenant_id = NEW.tenant_id
                    AND l.company_id = NEW.company_id AND l.delivery_id = NEW.delivery_id AND l.version = NEW.version AND l.verified_quantity IS NOT NULL))
            OR (NEW.status = 'superseded' AND NOT EXISTS (SELECT 1 FROM brant_pur.supplier_delivery_version v WHERE v.tenant_id = NEW.tenant_id
                    AND v.company_id = NEW.company_id AND v.delivery_id = NEW.delivery_id AND v.version = NEW.superseded_by_version
                    AND v.status = 'recorded')) THEN
        RAISE EXCEPTION 'brant_pur.supplier_delivery_version: version % of delivery % (%) changes only along SM-PUR-SUPPLIER-DELIVERY (PUR-INV-008)',
            OLD.version, OLD.delivery_id, OLD.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER supplier_delivery_version_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pur.supplier_delivery_version
    FOR EACH ROW EXECUTE FUNCTION brant_pur.guard_supplier_delivery_version();

CREATE TABLE brant_pur.supplier_delivery_line (
    tenant_id         text          NOT NULL,
    company_id        text          NOT NULL,
    delivery_id       text          NOT NULL,
    version           bigint        NOT NULL,
    -- The delivery line is its commitment line: the order line the commitment's draft buys. It
    -- names the same goods in every version of the delivery, with the SKU specification revision and
    -- the unit that line has in the draft.
    order_line_id     text          NOT NULL CHECK (order_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    line_no           integer       NOT NULL CHECK (line_no BETWEEN 0 AND 199),
    commitment_id     text          NOT NULL,
    order_id          text          NOT NULL,
    sku_id            text          NOT NULL,
    spec_revision     bigint        NOT NULL CHECK (spec_revision > 0),
    unit_id           text          NOT NULL,
    unit_revision     bigint        NOT NULL CHECK (unit_revision > 0),
    claimed_quantity  numeric(18,6) NOT NULL CHECK (claimed_quantity > 0),
    verified_quantity numeric(18,6) CHECK (verified_quantity >= 0),
    -- One version states a commitment line once.
    PRIMARY KEY (tenant_id, company_id, delivery_id, version, order_line_id),
    UNIQUE (tenant_id, company_id, delivery_id, version, line_no),
    FOREIGN KEY (tenant_id, company_id, delivery_id, version)
        REFERENCES brant_pur.supplier_delivery_version (tenant_id, company_id, delivery_id, version),
    -- Only what the evidence shows is verified, never more than was claimed.
    CHECK (verified_quantity <= claimed_quantity)
);

CREATE INDEX supplier_delivery_line_commitment ON brant_pur.supplier_delivery_line (tenant_id, company_id, commitment_id, order_line_id);

-- A line is written with its version, against a line of the version's commitment, and states the
-- same SKU specification and unit in every version of the delivery; afterwards only its verified
-- quantity is set, once, while the version awaits its decision. Nothing is deleted.
CREATE FUNCTION brant_pur.guard_supplier_delivery_line() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_verify text[] := ARRAY['verified_quantity'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pur.supplier_delivery_line: delivery lines are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.verified_quantity IS NOT NULL
                OR NOT EXISTS (SELECT 1 FROM brant_pur.supplier_delivery_version v WHERE v.tenant_id = NEW.tenant_id AND v.company_id = NEW.company_id
                    AND v.delivery_id = NEW.delivery_id AND v.version = NEW.version AND v.status = 'recorded' AND v.commitment_id = NEW.commitment_id
                    AND NEW.line_no < v.line_count
                    AND (SELECT count(*) FROM brant_pur.supplier_delivery_line l WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id
                        AND l.delivery_id = NEW.delivery_id AND l.version = NEW.version) < v.line_count)
                OR NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment c
                    JOIN brant_pur.purchase_draft_line dl ON dl.tenant_id = c.tenant_id AND dl.company_id = c.company_id AND dl.draft_id = c.draft_id
                    WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id AND c.commitment_id = NEW.commitment_id
                        AND c.order_id = NEW.order_id AND dl.order_line_id = NEW.order_line_id)
                OR EXISTS (SELECT 1 FROM brant_pur.supplier_delivery_line l WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id
                    AND l.delivery_id = NEW.delivery_id AND l.order_line_id = NEW.order_line_id AND l.version <> NEW.version
                    AND (l.sku_id, l.spec_revision, l.unit_id, l.unit_revision)
                        IS DISTINCT FROM (NEW.sku_id, NEW.spec_revision, NEW.unit_id, NEW.unit_revision)) THEN
            RAISE EXCEPTION 'brant_pur.supplier_delivery_line: delivery % version % does not take line %', NEW.delivery_id, NEW.version,
                NEW.order_line_id USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.verified_quantity IS NOT NULL OR NEW.verified_quantity IS NULL
            OR (to_jsonb(NEW) - v_verify) IS DISTINCT FROM (to_jsonb(OLD) - v_verify)
            OR NOT EXISTS (SELECT 1 FROM brant_pur.supplier_delivery_version v WHERE v.tenant_id = NEW.tenant_id AND v.company_id = NEW.company_id
                AND v.delivery_id = NEW.delivery_id AND v.version = NEW.version AND v.status = 'recorded') THEN
        RAISE EXCEPTION 'brant_pur.supplier_delivery_line: line % of delivery % version % is verified once, while its version awaits the decision',
            OLD.order_line_id, OLD.delivery_id, OLD.version USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER supplier_delivery_line_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pur.supplier_delivery_line
    FOR EACH ROW EXECUTE FUNCTION brant_pur.guard_supplier_delivery_line();

-- At commit a delivery is whole: its root counts its versions and points at the one verified now,
-- every version has exactly its lines, a claim that is not decided has no verified quantity, and a
-- superseded version was superseded by a version that is verified (or superseded in its turn).
CREATE FUNCTION brant_pur.check_supplier_delivery() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM brant_pur.supplier_delivery d WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id
            AND d.delivery_id = NEW.delivery_id
            AND d.latest_version = (SELECT count(*) FROM brant_pur.supplier_delivery_version v WHERE v.tenant_id = d.tenant_id
                AND v.company_id = d.company_id AND v.delivery_id = d.delivery_id)
            AND d.latest_version = (SELECT max(v.version) FROM brant_pur.supplier_delivery_version v WHERE v.tenant_id = d.tenant_id
                AND v.company_id = d.company_id AND v.delivery_id = d.delivery_id)
            AND d.verified_version IS NOT DISTINCT FROM (SELECT v.version FROM brant_pur.supplier_delivery_version v
                WHERE v.tenant_id = d.tenant_id AND v.company_id = d.company_id AND v.delivery_id = d.delivery_id AND v.status = 'verified'))
        OR EXISTS (SELECT 1 FROM brant_pur.supplier_delivery_version v WHERE v.tenant_id = NEW.tenant_id AND v.company_id = NEW.company_id
            AND v.delivery_id = NEW.delivery_id
            AND (v.line_count <> (SELECT count(*) FROM brant_pur.supplier_delivery_line l WHERE l.tenant_id = v.tenant_id
                    AND l.company_id = v.company_id AND l.delivery_id = v.delivery_id AND l.version = v.version)
                OR (v.status = 'recorded' AND EXISTS (SELECT 1 FROM brant_pur.supplier_delivery_line l WHERE l.tenant_id = v.tenant_id
                    AND l.company_id = v.company_id AND l.delivery_id = v.delivery_id AND l.version = v.version AND l.verified_quantity IS NOT NULL))
                OR (v.status = 'superseded' AND NOT EXISTS (SELECT 1 FROM brant_pur.supplier_delivery_version s WHERE s.tenant_id = v.tenant_id
                    AND s.company_id = v.company_id AND s.delivery_id = v.delivery_id AND s.version = v.superseded_by_version
                    AND s.status IN ('verified', 'superseded'))))) THEN
        RAISE EXCEPTION 'brant_pur.supplier_delivery: delivery % is not whole at commit', NEW.delivery_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER supplier_delivery_whole AFTER INSERT OR UPDATE ON brant_pur.supplier_delivery
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_pur.check_supplier_delivery();
CREATE CONSTRAINT TRIGGER supplier_delivery_version_whole AFTER INSERT OR UPDATE ON brant_pur.supplier_delivery_version
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_pur.check_supplier_delivery();

-- PUR-INV-008 at commit: for every commitment line a version verified in this transaction states,
-- the verified quantities of the versions verified now stay within what the commitment buys (the
-- draft line's allocated quantity plus its overbuy; zero tolerance in S5). A later migration that
-- lets a commitment change its quantities (S5-19) replaces this function.
CREATE FUNCTION brant_pur.check_supplier_delivery_within_commitment() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_line text;
BEGIN
    SELECT x.order_line_id INTO v_line FROM (
        SELECT l.order_line_id,
            (SELECT coalesce(sum(o.verified_quantity), 0) FROM brant_pur.supplier_delivery_line o
                JOIN brant_pur.supplier_delivery_version ov ON ov.tenant_id = o.tenant_id AND ov.company_id = o.company_id
                    AND ov.delivery_id = o.delivery_id AND ov.version = o.version
                WHERE o.tenant_id = l.tenant_id AND o.company_id = l.company_id AND o.commitment_id = l.commitment_id
                    AND o.order_line_id = l.order_line_id AND ov.status = 'verified') AS verified,
            (SELECT dl.quantity + dl.overbuy FROM brant_pur.purchase_commitment c
                JOIN brant_pur.purchase_draft_line dl ON dl.tenant_id = c.tenant_id AND dl.company_id = c.company_id AND dl.draft_id = c.draft_id
                WHERE c.tenant_id = l.tenant_id AND c.company_id = l.company_id AND c.commitment_id = l.commitment_id
                    AND dl.order_line_id = l.order_line_id) AS purchased
        FROM brant_pur.supplier_delivery_line l
        WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id AND l.delivery_id = NEW.delivery_id AND l.version = NEW.version
    ) x WHERE x.purchased IS NULL OR x.verified > x.purchased LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'brant_pur.supplier_delivery_line: the verified deliveries of commitment % line % exceed what it buys (PUR-INV-008)',
            NEW.commitment_id, v_line USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER supplier_delivery_within_commitment AFTER UPDATE ON brant_pur.supplier_delivery_version
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.status = 'verified')
    EXECUTE FUNCTION brant_pur.check_supplier_delivery_within_commitment();

-- SupplierDeliveryGuard v1 (Context Map R59 PUR→FUL, guardLock; FUL-INV-003; ADR-BR-004 option B,
-- ADR-BR-011): FUL calls it with its own executor inside its own transaction, after it locked its
-- own roots, for each delivery line a shipment line is sourced from, bound as (delivery, line,
-- version); the line is named by the order line its commitment line buys. The function locks the
-- delivery root row FOR SHARE until the caller's transaction ends; supplier_delivery.record and
-- supplier_delivery.verify lock that row FOR UPDATE and change it, so a correction either committed
-- before the guard read the row — and the guard answers REFERENCE_STALE for the superseded version
-- — or waits until the caller's transaction ends; in READ COMMITTED a guard that waited for a
-- delivery command reads the delivery as that command left it. It always returns one row in the
-- caller's scope (app.tenant_id and app.company_id):
--
--   known false  no such delivery, version or line (every other column NULL but code);
--   code NULL    the version is the verified one: verified_quantity is what FUL may ship of the
--                line in total, in the line's unit;
--   code SUPPLIER_DELIVERY_UNVERIFIED  unknown, or a claim recorded and not verified, or rejected:
--                a claim never counts;
--   code REFERENCE_STALE               a version a correction superseded; verified_version is the
--                version verified now.
--
-- FUL keeps its own count of what it shipped of a delivery line, across versions
-- (SUPPLIER_DELIVERY_INSUFFICIENT is FUL's). The guard applies no data scope: the caller checks
-- visibility when it resolves the delivery before its transaction. SECURITY DEFINER runs it as the
-- schema owner, which FORCE ROW LEVEL SECURITY still confines to the caller's scope; the predicates
-- repeat it. search_path is fixed and every name is qualified. brant_app gets EXECUTE in
-- deploy/postgres/10_app_grants.sql; nobody else does.
CREATE FUNCTION brant_pur.guard_supplier_delivery_v1(p_delivery_id text, p_line_id text, p_version bigint)
    RETURNS TABLE (known boolean, status text, verified_version bigint, commitment_id text, order_id text, order_line_id text,
                   supplier_id text, sku_id text, spec_revision bigint, unit_id text, unit_revision bigint, verified_quantity text, code text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
    v_root    record;
    v_line    record;
BEGIN
    IF p_delivery_id IS NULL OR p_line_id IS NULL OR p_version IS NULL THEN
        RAISE EXCEPTION 'brant_pur.guard_supplier_delivery_v1: delivery_id, line_id and version are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    SELECT d.verified_version, d.commitment_id, d.order_id, d.supplier_id INTO v_root
    FROM brant_pur.supplier_delivery d
    WHERE d.tenant_id = v_tenant AND d.company_id = v_company AND d.delivery_id = p_delivery_id
    FOR SHARE;
    IF FOUND THEN
        SELECT v.status, l.order_line_id, l.sku_id, l.spec_revision, l.unit_id, l.unit_revision, l.verified_quantity INTO v_line
        FROM brant_pur.supplier_delivery_version v
        JOIN brant_pur.supplier_delivery_line l ON l.tenant_id = v.tenant_id AND l.company_id = v.company_id AND l.delivery_id = v.delivery_id
            AND l.version = v.version
        WHERE v.tenant_id = v_tenant AND v.company_id = v_company AND v.delivery_id = p_delivery_id AND v.version = p_version
            AND l.order_line_id = p_line_id;
    END IF;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, NULL::bigint, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::bigint, NULL::text,
            NULL::bigint, NULL::text, 'SUPPLIER_DELIVERY_UNVERIFIED'::text;
        RETURN;
    END IF;
    RETURN QUERY SELECT true, v_line.status, v_root.verified_version, v_root.commitment_id, v_root.order_id, v_line.order_line_id,
        v_root.supplier_id, v_line.sku_id, v_line.spec_revision, v_line.unit_id, v_line.unit_revision,
        CASE WHEN v_line.status = 'verified' THEN pg_catalog.trim_scale(v_line.verified_quantity)::text END,
        CASE v_line.status WHEN 'verified' THEN NULL WHEN 'superseded' THEN 'REFERENCE_STALE' ELSE 'SUPPLIER_DELIVERY_UNVERIFIED' END::text;
END
$$;

REVOKE ALL ON FUNCTION brant_pur.guard_supplier_delivery_v1(text, text, bigint) FROM PUBLIC;

ALTER TABLE brant_pur.supplier_delivery ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.supplier_delivery FORCE ROW LEVEL SECURITY;
CREATE POLICY supplier_delivery_scope ON brant_pur.supplier_delivery
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pur.supplier_delivery_version ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.supplier_delivery_version FORCE ROW LEVEL SECURITY;
CREATE POLICY supplier_delivery_version_scope ON brant_pur.supplier_delivery_version
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pur.supplier_delivery_line ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.supplier_delivery_line FORCE ROW LEVEL SECURITY;
CREATE POLICY supplier_delivery_line_scope ON brant_pur.supplier_delivery_line
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
