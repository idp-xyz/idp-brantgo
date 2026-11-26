-- INQ (S7-06 read models): the order of rfq.list. A list returns the RFQs of one company the
-- caller sees, most recently changed first, a page at a time; the page boundary is the pair
-- (updated_at, rfq_id). No column changes: updated_at is set by every write since 000001.

CREATE INDEX customer_rfq_listing
    ON brant_inq.customer_rfq (tenant_id, company_id, updated_at DESC, rfq_id DESC);
