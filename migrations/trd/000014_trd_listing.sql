-- TRD (S7-06 read models): the order of quote.list and order.list. A list returns the quotes or
-- the orders of one company the caller sees, most recently changed first, a page at a time; the
-- page boundary is the pair (updated_at, id). No column changes: both tables set updated_at on
-- every write.

CREATE INDEX quote_root_listing
    ON brant_trd.quote_root (tenant_id, company_id, updated_at DESC, quote_id DESC);

CREATE INDEX sales_order_listing
    ON brant_trd.sales_order (tenant_id, company_id, updated_at DESC, order_id DESC);
