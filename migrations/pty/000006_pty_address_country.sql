-- PTY: the country of a party's address (S5 A3b; owner decision D-8 of 2026-11-12; tactical model
-- PTY-AGG-Party value object AddressCountry; Context Map R77 PTY->CNX "PartyRef v1、AddressRoleRef v1
-- （含国家代码）", whose risk is "以自由文本地址推定所在国").
--
-- country is the ISO 3166-1 alpha-2 code of the country an address is in, stated with the draft and
-- kept with every confirmed revision; CNX's trade compliance check takes the destination and the
-- parties' countries from it. PTY admits only officially assigned codes (kernel.ValidCountryCode);
-- the column checks the shape. NULL is unknown: it is never derived from the address text.
--
-- The columns are added without a default, so no existing row is rewritten: addresses confirmed
-- before this migration keep no country in their revisions, which never change (PTY-INV-001;
-- party_revision_address keeps its immutability trigger), and a country arrives only with the next
-- confirmed revision. A draft's addresses are replaced whole as before (guard_party_address).
-- AddressRoleGuard v1 is unchanged: a revision's address row, country included, never changes, so a
-- downstream that read the country before its transaction and holds the row FOR SHARE through the
-- guard decides on the same country. brant_app's table grants cover the new columns.

ALTER TABLE brant_pty.party_address ADD COLUMN country text CHECK (country ~ '^[A-Z]{2}$');
ALTER TABLE brant_pty.party_revision_address ADD COLUMN country text CHECK (country ~ '^[A-Z]{2}$');
