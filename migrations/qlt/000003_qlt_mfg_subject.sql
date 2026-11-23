-- QLT: InspectionSubject v1 accepts an MFG subcontract operation (Context Map R25, S6-D02).
--
-- subject_kind was only purchase_commitment_line. A subcontract operation is a different subject.
-- A factory progress claim is not a quality release and does not become qualified stock: this
-- migration only widens the kind. The columns of an inspection stay a snapshot of the subject
-- MFG published when the inspection was recorded.

ALTER TABLE brant_qlt.inspection DROP CONSTRAINT inspection_subject_kind_check;
ALTER TABLE brant_qlt.inspection ADD CONSTRAINT inspection_subject_kind_check
    CHECK (subject_kind IN ('purchase_commitment_line', 'mfg_subcontract_operation'));

ALTER TABLE brant_qlt.quality_release DROP CONSTRAINT quality_release_subject_kind_check;
ALTER TABLE brant_qlt.quality_release ADD CONSTRAINT quality_release_subject_kind_check
    CHECK (subject_kind IN ('purchase_commitment_line', 'mfg_subcontract_operation'));
