-- SUP (S7-06 read models): the order and the filters of supplier_rfq.list and supplier_offer.list.
-- A list returns the supplier RFQs or the supplier offers of one company the caller sees, most
-- recently changed first, a page at a time; the page boundary is the pair (updated_at, id). No
-- column changes: both roots set updated_at on every write.

CREATE INDEX supplier_rfq_listing
    ON brant_sup.supplier_rfq (tenant_id, company_id, updated_at DESC, supplier_rfq_id DESC);

CREATE INDEX condition_root_listing
    ON brant_sup.condition_root (tenant_id, company_id, kind, updated_at DESC, condition_id DESC);

-- The lists are narrowed by the customer RFQ a supplier RFQ or an offer revision names, which is a
-- member of its stored content. The queries use exactly these expressions
-- (adapters/postgres/lists.go: requestRfq, offerRfq).
CREATE INDEX supplier_rfq_of_rfq
    ON brant_sup.supplier_rfq (tenant_id, company_id, ((data #>> '{rfq,rfq_id}')));

CREATE INDEX condition_revision_of_rfq
    ON brant_sup.condition_revision (tenant_id, company_id, kind, ((data #>> '{rfq,rfq_id}')));
