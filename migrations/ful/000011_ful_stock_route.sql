-- FUL stock route (S6-D02, Context Map R29 STK→FUL, guardLock true; FUL-INV-001).
--
-- shipment.prepare may now name route stock. A stock line names a balance (location, batch,
-- custody) and, optionally, a reservation. It has no supplier delivery, commitment, supplier or
-- quality release. A supplier_direct line stays exactly as 000001 stored it. shipment.confirm of a
-- stock shipment calls brant_stk.stock_availability_issue_v1 inside FUL's transaction; this file
-- only records the line the call is built from. Applied files 000001–000010 are not edited.

ALTER TABLE brant_ful.shipment DROP CONSTRAINT shipment_route_check;
ALTER TABLE brant_ful.shipment ADD CONSTRAINT shipment_route_check
    CHECK (route IN ('supplier_direct', 'stock'));

ALTER TABLE brant_ful.shipment_line ALTER COLUMN delivery_id DROP NOT NULL;
ALTER TABLE brant_ful.shipment_line ALTER COLUMN delivery_line_id DROP NOT NULL;
ALTER TABLE brant_ful.shipment_line ALTER COLUMN delivery_version DROP NOT NULL;
ALTER TABLE brant_ful.shipment_line ALTER COLUMN commitment_id DROP NOT NULL;
ALTER TABLE brant_ful.shipment_line ALTER COLUMN supplier_id DROP NOT NULL;
ALTER TABLE brant_ful.shipment_line ALTER COLUMN quality_release_id DROP NOT NULL;

ALTER TABLE brant_ful.shipment_line
    ADD COLUMN location_id text CHECK (location_id IS NULL OR location_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    ADD COLUMN batch_id text CHECK (batch_id IS NULL OR batch_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    ADD COLUMN custody_kind text CHECK (custody_kind IS NULL OR custody_kind IN
        ('own_warehouse', 'external_factory_material', 'external_factory_wip', 'in_transit')),
    ADD COLUMN reservation_id text CHECK (reservation_id IS NULL OR reservation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$');

ALTER TABLE brant_ful.shipment_line ADD CONSTRAINT shipment_line_route_shape_check CHECK (
    (
        location_id IS NULL AND batch_id IS NULL AND custody_kind IS NULL AND reservation_id IS NULL
        AND delivery_id IS NOT NULL AND delivery_line_id IS NOT NULL AND delivery_version IS NOT NULL
        AND commitment_id IS NOT NULL AND supplier_id IS NOT NULL AND quality_release_id IS NOT NULL
    )
    OR
    (
        location_id IS NOT NULL AND batch_id IS NOT NULL AND custody_kind IS NOT NULL
        AND delivery_id IS NULL AND delivery_line_id IS NULL AND delivery_version IS NULL
        AND commitment_id IS NULL AND supplier_id IS NULL AND quality_release_id IS NULL
    )
);

-- The line shape must be the parent shipment's route. Lines are inserted after the shipment row
-- in the same transaction, so the shipment is visible here.
CREATE FUNCTION brant_ful.guard_stock_line() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE
    v_route text;
BEGIN
    SELECT s.route INTO v_route FROM brant_ful.shipment s
        WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id AND s.shipment_id = NEW.shipment_id;
    IF v_route = 'stock' AND NEW.location_id IS NULL THEN
        RAISE EXCEPTION 'brant_ful.shipment_line: stock shipment % line % is not a stock line', NEW.shipment_id, NEW.line_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF v_route = 'supplier_direct' AND NEW.location_id IS NOT NULL THEN
        RAISE EXCEPTION 'brant_ful.shipment_line: supplier direct shipment % line % is not a delivery line', NEW.shipment_id, NEW.line_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER shipment_line_stock BEFORE INSERT ON brant_ful.shipment_line
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_stock_line();
