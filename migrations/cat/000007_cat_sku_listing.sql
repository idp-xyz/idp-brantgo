-- CAT (S7-06 read models): the order of sku.list. A list returns the SKUs of one company the
-- caller sees, most recently changed first, a page at a time; the page boundary is the pair
-- (updated_at, sku_id). No column changes: updated_at is set by every write since 000001. A
-- search by name is a case-insensitive scan of the company's rows in this order.

CREATE INDEX sku_listing
    ON brant_cat.sku (tenant_id, company_id, updated_at DESC, sku_id DESC);
