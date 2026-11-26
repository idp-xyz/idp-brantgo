-- PTY (S7-06 read models): the order of party.list and contact.list. A list returns the parties or
-- the contacts of one company the caller sees, most recently changed first, a page at a time; the
-- page boundary is the pair (updated_at, id). No column changes: both tables set updated_at on
-- every write. A search by name is a case-insensitive scan of the company's rows in this order.

CREATE INDEX party_listing
    ON brant_pty.party (tenant_id, company_id, updated_at DESC, party_id DESC);

CREATE INDEX contact_listing
    ON brant_pty.contact (tenant_id, company_id, updated_at DESC, contact_id DESC);

-- contact.list by party: one party's contacts.
CREATE INDEX contact_of_party
    ON brant_pty.contact (tenant_id, company_id, party_id);
