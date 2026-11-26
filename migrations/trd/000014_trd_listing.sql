-- TRD (S7-06 read models): the order of quote.list, order.list and quote.response.list, and the
-- filters of quote.list and quote.rfq_coverage. A list returns the quotes, the orders or the
-- customer responses of one company the caller sees, a page at a time. Quotes and orders are
-- ordered by their last change and the page boundary is the pair (updated_at, id); both tables
-- set updated_at on every write. A customer response never changes, so its list is ordered by
-- (recorded_at, response_id). No column changes.

CREATE INDEX quote_root_listing
    ON brant_trd.quote_root (tenant_id, company_id, updated_at DESC, quote_id DESC);

CREATE INDEX sales_order_listing
    ON brant_trd.sales_order (tenant_id, company_id, updated_at DESC, order_id DESC);

CREATE INDEX customer_response_listing
    ON brant_trd.customer_response (tenant_id, company_id, recorded_at DESC, response_id DESC);

-- quote.list by customer or RFQ and quote.rfq_coverage compare the customer and the RFQ a
-- revision cites, which are members of its stored JSON. The queries use exactly these
-- expressions (adapters/postgres/lists.go: revisionCustomer, revisionRfq).
CREATE INDEX quote_revision_customer
    ON brant_trd.quote_revision (tenant_id, company_id, ((data::jsonb #>> '{customer_ref,party_id}')));

CREATE INDEX quote_revision_rfq
    ON brant_trd.quote_revision (tenant_id, company_id, ((data::jsonb #>> '{rfq_ref,rfq_id}')));
