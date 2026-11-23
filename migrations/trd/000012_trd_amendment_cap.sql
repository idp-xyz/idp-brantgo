-- TRD 000012: an amendment that grows a multi_order line appends the increase (S6-07).
--
-- quote_order_consumption stays append-only. The prepare still writes one row per line at
-- order_revision 1. A later increase of that line writes another row, at the sales-order
-- revision the amendment produces, holding only the extra quantity. A decrease does not insert
-- a row, and cancelling an order still does not. business_revision remains the quote revision
-- the order was drawn from; order_revision is the sales-order revision.

ALTER TABLE brant_trd.quote_order_consumption
    ADD COLUMN order_revision bigint NOT NULL DEFAULT 1 CHECK (order_revision > 0);

ALTER TABLE brant_trd.quote_order_consumption
    ALTER COLUMN order_revision DROP DEFAULT;

ALTER TABLE brant_trd.quote_order_consumption
    DROP CONSTRAINT quote_order_consumption_pkey;

ALTER TABLE brant_trd.quote_order_consumption
    ADD PRIMARY KEY (tenant_id, company_id, order_id, line_id, order_revision);
