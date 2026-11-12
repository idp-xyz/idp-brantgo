-- FUL: the insurance policy record of a shipment document set (tactical model
-- FUL-AGG-ShipmentDocumentSet entity InsurancePolicyRecord, value object InsuredAmount; FUL-INV-005;
-- simulated walkthrough SIM-02 step 8, finding F-03; Context Map R44 PTY->FUL, R74 EVD->FUL, R78
-- CST->FUL; S5 decision D-13; WBS S5-16).
--
-- A policy is an external document recorded in the set like a bill of lading: versions of kind
-- insurance_policy in brant_ful.shipment_document, with what the policy states as the version's
-- content (canonical JSON and its SHA-256: insurer, policy number, clauses, date, currency, insured
-- amount and premium in integer minor units, the contract percentage, and the cover and premium
-- comparison FUL found when it was recorded) and its EVD file. A version is inserted as a draft and
-- moves draft -> issued or superseded, issued -> superseded with the set (SM-FUL-DOCUMENT, 000004's
-- triggers); nothing of it changes otherwise. Its number is the policy number. The insured amount is
-- above zero, the premium zero or more, and the contract percentage at least 110 (Incoterms 2020
-- CIF/CIP A5); whether the policy covers the CIF or CIP invoice is FUL's domain rule, decided when
-- the policy is recorded and again when the set is issued.
--
-- No table is added: the grants and row level security of 000004 cover the policy records.

ALTER TABLE brant_ful.shipment_document DROP CONSTRAINT shipment_document_document_kind_check;
ALTER TABLE brant_ful.shipment_document ADD CONSTRAINT shipment_document_document_kind_check
    CHECK (document_kind IN ('commercial_invoice', 'packing_list', 'bill_of_lading', 'certificate_of_origin', 'insurance_policy'));

ALTER TABLE brant_ful.shipment_document ADD CONSTRAINT shipment_document_insurance_policy CHECK (document_kind <> 'insurance_policy' OR (
    (content::jsonb -> 'insurance_policy' ->> 'policy_number') = number
    AND (content::jsonb -> 'insurance_policy' -> 'currency' ->> 'code') ~ '^[A-Z]{3}$'
    AND (content::jsonb -> 'insurance_policy' ->> 'insured_minor') ~ '^[1-9][0-9]{0,23}$'
    AND (content::jsonb -> 'insurance_policy' ->> 'premium_minor') ~ '^(0|[1-9][0-9]{0,23})$'
    AND (content::jsonb -> 'insurance_policy' ->> 'coverage_percent') ~ '^[0-9]{1,4}(\.[0-9]{1,2})?$'
    AND (content::jsonb -> 'insurance_policy' ->> 'coverage_percent')::numeric >= 110
    AND jsonb_typeof(content::jsonb -> 'insurance_policy' -> 'clauses') = 'array'
    AND jsonb_array_length(content::jsonb -> 'insurance_policy' -> 'clauses') BETWEEN 1 AND 20));
