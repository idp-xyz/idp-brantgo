-- CNX: a refund case binds an export_tax_policy revision of any statutory refund method (S5R-D14,
-- simulated sign-off E0 of 2026-11-13; UL POL-ExportTaxPolicy; S5R unit R2).
--
-- POL's export_tax_policy states one of the four VAT treatments of exports the law defines:
-- exempt_credit_refund (免抵退), exempt_refund (免退), exempt (免税) and taxable (征税); not_applicable
-- stays on the revisions published before S5R. A case keeps the method of the revision it binds, so
-- the column admits all five. Evidence for export_refund is allocated only under exempt_credit_refund
-- or exempt_refund (CNX's rule); that a taxable export opens no case at all is S5R R10's. No row
-- changes.

ALTER TABLE brant_cnx.tax_refund_case DROP CONSTRAINT tax_refund_case_refund_method_check;
ALTER TABLE brant_cnx.tax_refund_case ADD CONSTRAINT tax_refund_case_refund_method_check
    CHECK (refund_method IN ('exempt_credit_refund', 'exempt_refund', 'exempt', 'taxable', 'not_applicable'));
