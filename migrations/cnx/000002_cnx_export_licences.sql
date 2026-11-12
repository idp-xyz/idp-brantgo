-- CNX: export licences and their usage ledger (tactical model CNX-AGG-ExportLicense with its
-- LicenseAllocation entries, CNX-INV-002; MD-44; UL CNX-ExportLicense, CNX-LicenseAllocation; WBS
-- S5-14; S5 decisions D-3, E-4: every licence in S5 is synthetic). Python V0.50 has no CNX.
--
-- A licence is the root: registered by a human, its terms never change; its row version moves by
-- one with every allocation and release, under the FOR UPDATE lock each takes (trigger: only
-- row_version and updated_at move; nothing is deleted). Allocations and releases are immutable
-- entries of it, and the licence's usage is derived from them: an allocation's outstanding quantity
-- and value are its own less its releases'. The entry triggers lock the licence FOR UPDATE too and
-- refuse an entry that would take Σ outstanding quantity or value above the licensed ones, an
-- allocation of a target that still holds one (a target's allocations are numbered by attempt; a
-- new attempt only after every earlier one was released in full), or a release of more than is
-- outstanding. A release is human and rests on an external result (an amended or withdrawn
-- declaration receipt); until CNX records declaration receipts (S5 C3a) the result is stated by the
-- human with its EVD evidence (result_source 'stated').
--
-- brant_cnx.guard_export_license_v1 publishes ExportLicenseGuard v1 (Context Map R56 CNX->FUL,
-- guardLock): whether an allocation with quantity outstanding on a licence valid at the instant
-- covers a shipment, holding the licence row and then the allocation row FOR SHARE until the
-- caller's transaction ends. CNX's reads derive the same verdict with brant_cnx.export_license_state
-- without locks.
--
-- References to FUL, CAT, POL and EVD are plain IDs and hashes: no foreign key crosses a schema
-- (DB2).

CREATE TABLE brant_cnx.export_license (
    tenant_id            text        NOT NULL,
    company_id           text        NOT NULL,
    license_id           text        NOT NULL CHECK (license_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    license_number       text        NOT NULL CHECK (char_length(license_number) BETWEEN 1 AND 64),
    issuer               text        NOT NULL CHECK (char_length(issuer) BETWEEN 1 AND 200),
    holder               text        NOT NULL CHECK (char_length(holder) BETWEEN 1 AND 200),
    goods                text        NOT NULL CHECK (left(goods, 1) = '{'),
    unit                 text        NOT NULL CHECK (unit ~ '^[A-Z0-9-]{1,32}$'),
    licensed_quantity    numeric     NOT NULL CHECK (licensed_quantity > 0),
    currency_id          text        NOT NULL,
    currency_revision    bigint      NOT NULL CHECK (currency_revision > 0),
    currency_code        text        NOT NULL CHECK (currency_code ~ '^[A-Z]{3}$'),
    currency_minor_units integer     NOT NULL CHECK (currency_minor_units BETWEEN 0 AND 4),
    licensed_value_minor numeric     NOT NULL CHECK (licensed_value_minor > 0 AND licensed_value_minor = trunc(licensed_value_minor)),
    destination          text        NOT NULL CHECK (destination ~ '^[A-Z]{2}$'),
    end_user_party_id    text        NOT NULL CHECK (end_user_party_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    end_user_name        text        NOT NULL CHECK (char_length(end_user_name) BETWEEN 1 AND 200),
    valid_from           timestamptz NOT NULL,
    valid_until          timestamptz NOT NULL,
    file_id              text        NOT NULL,
    file_sha256          text        NOT NULL CHECK (file_sha256 ~ '^[0-9a-f]{64}$'),
    status               text        NOT NULL CHECK (status = 'registered'),
    row_version          bigint      NOT NULL CHECK (row_version > 0),
    registered_by_kind   text        NOT NULL CHECK (registered_by_kind = 'human'),
    registered_by_id     text        NOT NULL,
    authority_epoch      bigint      NOT NULL CHECK (authority_epoch > 0),
    registered_at        timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, license_id),
    UNIQUE (tenant_id, company_id, issuer, license_number),
    CHECK (valid_until > valid_from),
    CHECK (updated_at >= registered_at)
);

-- A licence is registered at version 1; afterwards only its version (by one) and updated_at move.
CREATE FUNCTION brant_cnx.guard_export_license() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cnx.export_license: licences are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.row_version <> 1 OR NEW.updated_at <> NEW.registered_at THEN
            RAISE EXCEPTION 'brant_cnx.export_license: a licence is registered at version 1' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - ARRAY['row_version', 'updated_at']) IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['row_version', 'updated_at'])
            OR NEW.row_version <> OLD.row_version + 1 OR NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'brant_cnx.export_license: licence % keeps its terms and moves by one version', OLD.license_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER export_license_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.export_license
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_export_license();

CREATE TABLE brant_cnx.export_license_allocation (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    allocation_id     text        NOT NULL CHECK (allocation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    license_id        text        NOT NULL,
    target_kind       text        NOT NULL CHECK (target_kind IN ('shipment', 'export_lot')),
    target_id         text        NOT NULL CHECK (target_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    target_version    bigint      NOT NULL CHECK (target_version >= 0),
    attempt           integer     NOT NULL CHECK (attempt > 0),
    order_id          text        NOT NULL,
    order_revision    bigint      NOT NULL CHECK (order_revision >= 0),
    destination       text        NOT NULL CHECK (destination ~ '^[A-Z]{2}$'),
    end_user_party_id text        NOT NULL,
    lines             text        NOT NULL CHECK (left(lines, 1) = '['),
    lines_hash        text        NOT NULL CHECK (lines_hash ~ '^[0-9a-f]{64}$'),
    quantity          numeric     NOT NULL CHECK (quantity > 0),
    value_minor       numeric     NOT NULL CHECK (value_minor >= 0 AND value_minor = trunc(value_minor)),
    currency_code     text        NOT NULL,
    allocated_by_kind text        NOT NULL CHECK (allocated_by_kind IN ('human', 'ai', 'system')),
    allocated_by_id   text        NOT NULL,
    authority_epoch   bigint      NOT NULL CHECK (authority_epoch > 0),
    allocated_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, allocation_id),
    UNIQUE (tenant_id, company_id, license_id, allocation_id),
    UNIQUE (tenant_id, company_id, target_kind, target_id, attempt),
    FOREIGN KEY (tenant_id, company_id, license_id) REFERENCES brant_cnx.export_license (tenant_id, company_id, license_id)
);

CREATE INDEX export_license_allocation_license ON brant_cnx.export_license_allocation (tenant_id, company_id, license_id);

CREATE TABLE brant_cnx.export_license_release (
    tenant_id          text        NOT NULL,
    company_id         text        NOT NULL,
    release_id         text        NOT NULL CHECK (release_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    license_id         text        NOT NULL,
    allocation_id      text        NOT NULL,
    kind               text        NOT NULL CHECK (kind IN ('amended', 'withdrawn')),
    result_source      text        NOT NULL CHECK (result_source = 'stated'),
    declaration_number text        NOT NULL CHECK (char_length(declaration_number) BETWEEN 1 AND 64),
    receipt_text       text        NOT NULL CHECK (char_length(receipt_text) BETWEEN 1 AND 4000),
    received_at        timestamptz NOT NULL,
    evidence           text        NOT NULL CHECK (left(evidence, 2) = '[{'),
    quantity           numeric     NOT NULL CHECK (quantity > 0),
    value_minor        numeric     NOT NULL CHECK (value_minor >= 0 AND value_minor = trunc(value_minor)),
    reason             text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    released_by_kind   text        NOT NULL CHECK (released_by_kind = 'human'),
    released_by_id     text        NOT NULL,
    authority_epoch    bigint      NOT NULL CHECK (authority_epoch > 0),
    released_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, release_id),
    FOREIGN KEY (tenant_id, company_id, license_id, allocation_id)
        REFERENCES brant_cnx.export_license_allocation (tenant_id, company_id, license_id, allocation_id)
);

CREATE INDEX export_license_release_allocation ON brant_cnx.export_license_release (tenant_id, company_id, allocation_id);

-- The outstanding quantity and value of a licence's allocations (Σ allocations − Σ releases).
CREATE FUNCTION brant_cnx.export_license_outstanding(p_tenant text, p_company text, p_license text, OUT quantity numeric, OUT value_minor numeric)
    LANGUAGE sql STABLE SECURITY INVOKER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT coalesce((SELECT sum(a.quantity) FROM brant_cnx.export_license_allocation a WHERE a.tenant_id = p_tenant
            AND a.company_id = p_company AND a.license_id = p_license), 0)
        - coalesce((SELECT sum(r.quantity) FROM brant_cnx.export_license_release r WHERE r.tenant_id = p_tenant
            AND r.company_id = p_company AND r.license_id = p_license), 0),
        coalesce((SELECT sum(a.value_minor) FROM brant_cnx.export_license_allocation a WHERE a.tenant_id = p_tenant
            AND a.company_id = p_company AND a.license_id = p_license), 0)
        - coalesce((SELECT sum(r.value_minor) FROM brant_cnx.export_license_release r WHERE r.tenant_id = p_tenant
            AND r.company_id = p_company AND r.license_id = p_license), 0)
$$;
REVOKE ALL ON FUNCTION brant_cnx.export_license_outstanding(text, text, text) FROM PUBLIC;

-- An allocation is only inserted, under its licence's FOR UPDATE lock, in the licence's currency,
-- as its target's next attempt after every earlier one was released in full, and never takes Σ
-- outstanding above the licensed quantity or value (CNX-INV-002).
CREATE FUNCTION brant_cnx.guard_export_license_allocation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_license record;
    v_used    record;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cnx.export_license_allocation: allocations never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT l.licensed_quantity, l.licensed_value_minor, l.currency_code INTO v_license FROM brant_cnx.export_license l
    WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id AND l.license_id = NEW.license_id FOR UPDATE;
    IF NOT FOUND OR v_license.currency_code <> NEW.currency_code THEN
        RAISE EXCEPTION 'brant_cnx.export_license_allocation: an allocation is in its licence''s currency'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.attempt <> 1 + (SELECT count(*) FROM brant_cnx.export_license_allocation a WHERE a.tenant_id = NEW.tenant_id
                AND a.company_id = NEW.company_id AND a.target_kind = NEW.target_kind AND a.target_id = NEW.target_id)
            OR EXISTS (SELECT 1 FROM brant_cnx.export_license_allocation a WHERE a.tenant_id = NEW.tenant_id
                AND a.company_id = NEW.company_id AND a.target_kind = NEW.target_kind AND a.target_id = NEW.target_id
                AND a.quantity > coalesce((SELECT sum(r.quantity) FROM brant_cnx.export_license_release r WHERE r.tenant_id = a.tenant_id
                    AND r.company_id = a.company_id AND r.allocation_id = a.allocation_id), 0)) THEN
        RAISE EXCEPTION 'brant_cnx.export_license_allocation: target % % holds an allocation', NEW.target_kind, NEW.target_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT * INTO v_used FROM brant_cnx.export_license_outstanding(NEW.tenant_id, NEW.company_id, NEW.license_id);
    IF v_used.quantity + NEW.quantity > v_license.licensed_quantity OR v_used.value_minor + NEW.value_minor > v_license.licensed_value_minor THEN
        RAISE EXCEPTION 'brant_cnx.export_license_allocation: licence % would be exceeded (CNX-INV-002)', NEW.license_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER export_license_allocation_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.export_license_allocation
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_export_license_allocation();

-- A release is only inserted, under its licence's FOR UPDATE lock, and never releases more of its
-- allocation's quantity or value than is outstanding.
CREATE FUNCTION brant_cnx.guard_export_license_release() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_allocation record;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cnx.export_license_release: releases never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    PERFORM 1 FROM brant_cnx.export_license l WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id
        AND l.license_id = NEW.license_id FOR UPDATE;
    SELECT a.quantity - coalesce(sum(r.quantity), 0) AS quantity, a.value_minor - coalesce(sum(r.value_minor), 0) AS value_minor
    INTO v_allocation
    FROM brant_cnx.export_license_allocation a
    LEFT JOIN brant_cnx.export_license_release r ON r.tenant_id = a.tenant_id AND r.company_id = a.company_id AND r.allocation_id = a.allocation_id
    WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id AND a.allocation_id = NEW.allocation_id
    GROUP BY a.quantity, a.value_minor;
    IF NOT FOUND OR NEW.quantity > v_allocation.quantity OR NEW.value_minor > v_allocation.value_minor THEN
        RAISE EXCEPTION 'brant_cnx.export_license_release: allocation % has less outstanding', NEW.allocation_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER export_license_release_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.export_license_release
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_export_license_release();

ALTER TABLE brant_cnx.export_license ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.export_license FORCE ROW LEVEL SECURITY;
CREATE POLICY export_license_scope ON brant_cnx.export_license
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.export_license_allocation ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.export_license_allocation FORCE ROW LEVEL SECURITY;
CREATE POLICY export_license_allocation_scope ON brant_cnx.export_license_allocation
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.export_license_release ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.export_license_release FORCE ROW LEVEL SECURITY;
CREATE POLICY export_license_release_scope ON brant_cnx.export_license_release
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

-- The export licence verdict for a shipment at p_at in the caller's scope: its latest allocation
-- (highest attempt) and that allocation's licence; with p_lock the licence row, then the allocation
-- row, stay locked FOR SHARE until the caller's transaction ends (release and allocate lock the
-- licence FOR UPDATE and wait). A shipment without an allocation locks nothing. The checks run from
-- the most to the least lasting: no allocation, released in full, the licence's validity.
CREATE FUNCTION brant_cnx.export_license_state(p_shipment_id text, p_at timestamptz, p_lock boolean)
    RETURNS TABLE (covered boolean, status text, reason text, license_id text, allocation_id text, quantity text, unit text,
                   value_minor text, currency text, lines_hash text, valid_until timestamptz, code text)
    LANGUAGE plpgsql VOLATILE SECURITY INVOKER SET search_path = pg_catalog, pg_temp
AS $$
#variable_conflict use_column
DECLARE
    v_tenant     text := pg_catalog.current_setting('app.tenant_id', true);
    v_company    text := pg_catalog.current_setting('app.company_id', true);
    v_found      record;
    v_license    record;
    v_allocation record;
    v_quantity   numeric;
    v_value      numeric;
    v_code       text := 'EXPORT_LICENSE_REQUIRED';
BEGIN
    IF p_shipment_id IS NULL OR p_at IS NULL OR p_lock IS NULL THEN
        RAISE EXCEPTION 'brant_cnx.export_license_state: shipment, instant and lock mode are required' USING ERRCODE = 'null_value_not_allowed';
    END IF;
    SELECT a.allocation_id, a.license_id INTO v_found FROM brant_cnx.export_license_allocation a
    WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.target_kind = 'shipment' AND a.target_id = p_shipment_id
    ORDER BY a.attempt DESC LIMIT 1;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, 'required'::text, 'not_allocated'::text, ''::text, ''::text, NULL::text, NULL::text, NULL::text,
            NULL::text, NULL::text, NULL::timestamptz, v_code;
        RETURN;
    END IF;
    IF p_lock THEN
        SELECT l.unit, l.currency_code, l.valid_from, l.valid_until INTO v_license FROM brant_cnx.export_license l
        WHERE l.tenant_id = v_tenant AND l.company_id = v_company AND l.license_id = v_found.license_id FOR SHARE;
        SELECT a.quantity, a.value_minor, a.lines_hash INTO v_allocation FROM brant_cnx.export_license_allocation a
        WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.allocation_id = v_found.allocation_id FOR SHARE;
    ELSE
        SELECT l.unit, l.currency_code, l.valid_from, l.valid_until INTO v_license FROM brant_cnx.export_license l
        WHERE l.tenant_id = v_tenant AND l.company_id = v_company AND l.license_id = v_found.license_id;
        SELECT a.quantity, a.value_minor, a.lines_hash INTO v_allocation FROM brant_cnx.export_license_allocation a
        WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.allocation_id = v_found.allocation_id;
    END IF;
    SELECT v_allocation.quantity - coalesce(sum(r.quantity), 0), v_allocation.value_minor - coalesce(sum(r.value_minor), 0)
    INTO v_quantity, v_value
    FROM brant_cnx.export_license_release r
    WHERE r.tenant_id = v_tenant AND r.company_id = v_company AND r.allocation_id = v_found.allocation_id;
    IF v_quantity <= 0 THEN
        RETURN QUERY SELECT false, 'required'::text, 'released'::text, v_found.license_id, v_found.allocation_id, trim_scale(v_quantity)::text,
            v_license.unit, trim_scale(v_value)::text, v_license.currency_code, v_allocation.lines_hash, v_license.valid_until, v_code;
        RETURN;
    END IF;
    IF NOT (p_at >= v_license.valid_from AND p_at < v_license.valid_until) THEN
        RETURN QUERY SELECT false, 'expired'::text, 'expired'::text, v_found.license_id, v_found.allocation_id, trim_scale(v_quantity)::text,
            v_license.unit, trim_scale(v_value)::text, v_license.currency_code, v_allocation.lines_hash, v_license.valid_until, v_code;
        RETURN;
    END IF;
    RETURN QUERY SELECT true, 'allocated'::text, 'allocated'::text, v_found.license_id, v_found.allocation_id, trim_scale(v_quantity)::text,
        v_license.unit, trim_scale(v_value)::text, v_license.currency_code, v_allocation.lines_hash, v_license.valid_until, NULL::text;
END
$$;
REVOKE ALL ON FUNCTION brant_cnx.export_license_state(text, timestamptz, boolean) FROM PUBLIC;

-- ExportLicenseGuard v1: the verdict with the FOR SHARE locks, for FUL's transaction (R56).
CREATE FUNCTION brant_cnx.guard_export_license_v1(p_shipment_id text, p_at timestamptz)
    RETURNS TABLE (covered boolean, status text, reason text, license_id text, allocation_id text, quantity text, unit text,
                   value_minor text, currency text, lines_hash text, valid_until timestamptz, code text)
    LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT s.covered, s.status, s.reason, s.license_id, s.allocation_id, s.quantity, s.unit, s.value_minor, s.currency, s.lines_hash,
        s.valid_until, s.code
    FROM brant_cnx.export_license_state(p_shipment_id, p_at, true) s
$$;
REVOKE ALL ON FUNCTION brant_cnx.guard_export_license_v1(text, timestamptz) FROM PUBLIC;
