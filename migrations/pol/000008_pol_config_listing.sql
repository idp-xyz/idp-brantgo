-- POL (S7-06 read models): the order of config.list. The list returns the configurations of one
-- kind of one company, most recently changed first, a page at a time; the page boundary is the
-- pair (updated_at, config_id). No column changes: the root sets updated_at on every write.

CREATE INDEX config_root_listing
    ON brant_pol.config_root (tenant_id, company_id, kind, updated_at DESC, config_id DESC);
