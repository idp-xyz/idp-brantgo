package api

import (
	"strings"
	"testing"
)

func booking() RecordBooking {
	return RecordBooking{IdempotencyKey: "k", ShipmentID: "shipment.1", ServiceRole: ServiceRoleRef{PartyID: "forwarder.1", ServiceRoleID: "sea",
		Revision: 1}, BookingNumber: "SHA2611001", Vessel: "SYNTHETIC STAR", Voyage: "026E", ETD: "2026-12-20", CutOff: "2026-12-18T10:00:00.000000Z",
		ContainerType: "40HQ", ContainerCount: 1, PortOfLoading: "CNSHA", PortOfDischarge: "DEHAM", EvidenceIDs: []string{"evidence"}}
}

// The closed contract of booking.record (UL FUL-BookingRecord; SAP reference O2C-11: 船期、截关、箱型、
// 承运人确认).
func TestRecordBookingContract(t *testing.T) {
	for _, c := range []struct {
		edit func(*RecordBooking)
		want string
	}{
		{func(*RecordBooking) {}, "OK"},
		{func(c *RecordBooking) { c.ExpectedVersion = -1 }, "SCHEMA_INVALID /args/expected_version"},
		{func(c *RecordBooking) { c.ServiceRole.PartyID = "" }, "SCHEMA_INVALID /args/service_role/party_id"},
		{func(c *RecordBooking) { c.ServiceRole.ServiceRoleID = "a b" }, "SCHEMA_INVALID /args/service_role/service_role_id"},
		{func(c *RecordBooking) { c.ServiceRole.Revision = 0 }, "SCHEMA_INVALID /args/service_role/revision"},
		{func(c *RecordBooking) { c.BookingNumber = " " }, "SCHEMA_INVALID /args/booking_number"},
		{func(c *RecordBooking) { c.Vessel = "A\tB" }, "SCHEMA_INVALID /args/vessel"},
		{func(c *RecordBooking) { c.Voyage = strings.Repeat("9", MaxVoyage+1) }, "SCHEMA_INVALID /args/voyage"},
		{func(c *RecordBooking) { c.ContainerType = "40hq" }, "SCHEMA_INVALID /args/container_type"},
		{func(c *RecordBooking) { c.ContainerCount = 0 }, "SCHEMA_INVALID /args/container_count"},
		{func(c *RecordBooking) { c.PortOfLoading = "Shanghai" }, "SCHEMA_INVALID /args/port_of_loading"},
		{func(c *RecordBooking) { c.PortOfDischarge = "DE1AM" }, "SCHEMA_INVALID /args/port_of_discharge"},
		{func(c *RecordBooking) { c.ETD = "2026-02-30" }, "SCHEMA_INVALID /args/etd"},
		{func(c *RecordBooking) { c.CutOff = "2026-12-18" }, "SCHEMA_INVALID /args/cut_off"},
		{func(c *RecordBooking) { c.CutOff = "2026-12-21T00:00:00.000000Z" }, "INVALID_KNOWN_VALUE /args/cut_off"},
		{func(c *RecordBooking) { c.CutOff = "2026-12-20T23:59:59.000000Z" }, "OK"},
		{func(c *RecordBooking) { c.EvidenceIDs = nil }, "SCHEMA_INVALID /args/evidence_ids"},
		{func(c *RecordBooking) { c.EvidenceIDs = []string{"e", "e"} }, "SCHEMA_INVALID /args/evidence_ids/1"},
		{func(c *RecordBooking) { c.ExpectedVersion = 1 }, "SCHEMA_INVALID /args/reason"},
		{func(c *RecordBooking) { c.ExpectedVersion, c.Reason = 1, "甩柜改配" }, "OK"},
		{func(c *RecordBooking) { c.Reason = " " }, "SCHEMA_INVALID /args/reason"},
	} {
		cmd := booking()
		c.edit(&cmd)
		if got := outcome(cmd.Validate()); got != c.want {
			t.Errorf("%s, want %s", got, c.want)
		}
	}
	if !Location("CNSHA") || Location("cnsha") || Location("CN1HA") || !Location("DE2AM") || !ContainerType("22G1") || ContainerType("X") ||
		!ContainerNumber("COSU1234565") || ContainerNumber("COS1234565X") || !Block("a\nb", 3) || Block("a\tb", 3) || Line("a\nb", 3) {
		t.Fatal("forms")
	}
}

func cfg(kind, id string) *ConfigRef { return &ConfigRef{Kind: kind, ID: id, Revision: 1} }

func invoice() PrepareShipmentDocument {
	return PrepareShipmentDocument{IdempotencyKey: "k", DocumentSetID: "docs.1", ShipmentID: "shipment.1", DocumentKind: DocumentCommercialInvoice,
		Templates:   []ConfigRef{*cfg(KindShipmentDocumentTemplate, "ci.en"), *cfg(KindShipmentDocumentTemplate, "ci.zh")},
		SequenceRef: cfg(KindNumberSequence, "ci.seq"), SellerRef: cfg(KindCompanyProfile, "company.A"),
		Particulars: &Particulars{Marks: "N/M", Descriptions: []Description{{SkuID: "sku.1", Language: LanguageEn, Text: "Shirt"},
			{SkuID: "sku.1", Language: LanguageZhCN, Text: "衬衫"}}}}
}

func lading() *BillOfLading {
	return &BillOfLading{Number: "COSU0001", FileID: "file.bl", Vessel: "SYNTHETIC STAR", Voyage: "026E", PortOfLoading: "CNSHA",
		PortOfDischarge: "DEHAM", ShippedOnBoard: "2026-12-20", Consignee: DocumentParty{Name: "Buyer", Address: "Hamburg"}, Packages: 5,
		GrossWeightKg: "52.5", ContainerNumbers: []string{"COSU1234565"}}
}

// The closed contract of shipment_document.prepare (FUL-AGG-ShipmentDocumentSet; R49; INT-05): what
// each kind states and nothing else.
func TestPrepareShipmentDocumentContract(t *testing.T) {
	pl := Package{Cartons: 4, CartonFrom: 1, Contents: []PackageContent{{LineID: "S1", QuantityPerCarton: "10"}}, GrossWeightKg: "10",
		NetWeightKg: "9", VolumeCbm: "0.1"}
	for _, c := range []struct {
		edit func(*PrepareShipmentDocument)
		want string
	}{
		{func(*PrepareShipmentDocument) {}, "OK"},
		{func(c *PrepareShipmentDocument) { c.DocumentSetID = "" }, "SCHEMA_INVALID /args/document_set_id"},
		{func(c *PrepareShipmentDocument) { c.ShipmentID = "a b" }, "SCHEMA_INVALID /args/shipment_id"},
		{func(c *PrepareShipmentDocument) { c.ExpectedVersion = -1 }, "SCHEMA_INVALID /args/expected_version"},
		{func(c *PrepareShipmentDocument) { c.DocumentKind = "insurance_policy" }, "SCHEMA_INVALID /args/document_kind"},
		{func(c *PrepareShipmentDocument) { c.Templates = nil }, "SCHEMA_INVALID /args/templates"},
		{func(c *PrepareShipmentDocument) { c.Templates = append(c.Templates, c.Templates[0]) }, "SCHEMA_INVALID /args/templates"},
		{func(c *PrepareShipmentDocument) { c.Templates[1] = c.Templates[0] }, "SCHEMA_INVALID /args/templates/1"},
		{func(c *PrepareShipmentDocument) { c.Templates[0].Kind = "order_document_template" }, "SCHEMA_INVALID /args/templates/0/kind"},
		{func(c *PrepareShipmentDocument) { c.SequenceRef = nil }, "SCHEMA_INVALID /args/sequence_ref"},
		{func(c *PrepareShipmentDocument) { c.SequenceRef.Revision = 0 }, "SCHEMA_INVALID /args/sequence_ref/revision"},
		{func(c *PrepareShipmentDocument) { c.SellerRef = cfg("currency", "USD") }, "SCHEMA_INVALID /args/seller_ref/kind"},
		{func(c *PrepareShipmentDocument) { c.Particulars = nil }, "SCHEMA_INVALID /args/particulars"},
		{func(c *PrepareShipmentDocument) { c.Particulars.Descriptions = nil }, "SCHEMA_INVALID /args/particulars/descriptions"},
		{func(c *PrepareShipmentDocument) { c.Particulars.Descriptions[1].Language = LanguageEn },
			"DUPLICATE_LINE_ID /args/particulars/descriptions/1/sku_id"},
		{func(c *PrepareShipmentDocument) { c.Particulars.Descriptions[1].Language = "de" }, "SCHEMA_INVALID /args/particulars/descriptions/1/language"},
		{func(c *PrepareShipmentDocument) { c.Particulars.Descriptions[0].Text = "" }, "SCHEMA_INVALID /args/particulars/descriptions/0/text"},
		{func(c *PrepareShipmentDocument) { c.Particulars.PortOfLoading = "SHA" }, "SCHEMA_INVALID /args/particulars/port_of_loading"},
		{func(c *PrepareShipmentDocument) { c.Particulars.Consignee = &DocumentParty{Name: "Buyer"} }, "SCHEMA_INVALID /args/particulars/consignee/address"},
		{func(c *PrepareShipmentDocument) { c.Particulars.Notify = &DocumentParty{Address: "x"} }, "SCHEMA_INVALID /args/particulars/notify/name"},
		{func(c *PrepareShipmentDocument) { c.Particulars.Marks = "\t" }, "SCHEMA_INVALID /args/particulars/marks"},
		{func(c *PrepareShipmentDocument) { c.Packages = []Package{pl} }, "SCHEMA_INVALID /args/packages"},
		{func(c *PrepareShipmentDocument) { c.DocumentKind = DocumentPackingList; c.Packages = []Package{pl} }, "OK"},
		{func(c *PrepareShipmentDocument) {
			c.DocumentKind, c.Packages = DocumentPackingList, []Package{{Cartons: 1, Contents: pl.Contents}}
		},
			"SCHEMA_INVALID /args/packages/0/carton_from"},
		{func(c *PrepareShipmentDocument) { c.BillOfLading = lading() }, "SCHEMA_INVALID /args/bill_of_lading"},
		{func(c *PrepareShipmentDocument) { c.Certificate = &CertificateOfOrigin{} }, "SCHEMA_INVALID /args/certificate"},
		{func(c *PrepareShipmentDocument) { c.EvidenceIDs = []string{""} }, "SCHEMA_INVALID /args/evidence_ids/0"},
	} {
		cmd := invoice()
		c.edit(&cmd)
		if got := outcome(cmd.Validate()); got != c.want {
			t.Errorf("%s, want %s", got, c.want)
		}
	}
	record := func(edit func(*PrepareShipmentDocument)) string {
		c := PrepareShipmentDocument{IdempotencyKey: "k", DocumentSetID: "docs.1", ShipmentID: "shipment.1", DocumentKind: DocumentBillOfLading,
			BillOfLading: lading()}
		edit(&c)
		return outcome(c.Validate())
	}
	for _, c := range []struct {
		edit func(*PrepareShipmentDocument)
		want string
	}{
		{func(*PrepareShipmentDocument) {}, "OK"},
		{func(c *PrepareShipmentDocument) { c.BillOfLading = nil }, "SCHEMA_INVALID /args/bill_of_lading"},
		{func(c *PrepareShipmentDocument) { c.Templates = invoice().Templates }, "SCHEMA_INVALID /args/templates"},
		{func(c *PrepareShipmentDocument) { c.SequenceRef = invoice().SequenceRef }, "SCHEMA_INVALID /args/sequence_ref"},
		{func(c *PrepareShipmentDocument) { c.SellerRef = invoice().SellerRef }, "SCHEMA_INVALID /args/seller_ref"},
		{func(c *PrepareShipmentDocument) { c.Particulars = invoice().Particulars }, "SCHEMA_INVALID /args/particulars"},
		{func(c *PrepareShipmentDocument) { c.Certificate = &CertificateOfOrigin{} }, "SCHEMA_INVALID /args/certificate"},
		{func(c *PrepareShipmentDocument) { c.BillOfLading.Number = "" }, "SCHEMA_INVALID /args/bill_of_lading/number"},
		{func(c *PrepareShipmentDocument) { c.BillOfLading.FileID = "" }, "SCHEMA_INVALID /args/bill_of_lading/file_id"},
		{func(c *PrepareShipmentDocument) { c.BillOfLading.ShippedOnBoard = "x" }, "SCHEMA_INVALID /args/bill_of_lading/shipped_on_board"},
		{func(c *PrepareShipmentDocument) { c.BillOfLading.Packages = 0 }, "SCHEMA_INVALID /args/bill_of_lading/packages"},
		{func(c *PrepareShipmentDocument) { c.BillOfLading.GrossWeightKg = "0" }, "SCHEMA_INVALID /args/bill_of_lading/gross_weight_kg"},
		{func(c *PrepareShipmentDocument) { c.BillOfLading.VolumeCbm = "-1" }, "SCHEMA_INVALID /args/bill_of_lading/volume_cbm"},
		{func(c *PrepareShipmentDocument) {
			c.BillOfLading.ContainerNumbers = []string{"COSU1234565", "COSU1234565"}
		},
			"SCHEMA_INVALID /args/bill_of_lading/container_numbers/1"},
		{func(c *PrepareShipmentDocument) { c.BillOfLading.Consignee.Name = "" }, "SCHEMA_INVALID /args/bill_of_lading/consignee/name"},
		{func(c *PrepareShipmentDocument) {
			c.DocumentKind, c.BillOfLading = DocumentCertificateOfOrigin, nil
			c.Certificate = &CertificateOfOrigin{Number: "CO1", FileID: "file.co", Issuer: "CCPIT", IssuedOn: "2026-12-19",
				CertificateRef: &CertificateRef{CertificateID: "cert.1", Version: 1}}
		}, "OK"},
		{func(c *PrepareShipmentDocument) {
			c.DocumentKind, c.BillOfLading = DocumentCertificateOfOrigin, nil
			c.Certificate = &CertificateOfOrigin{Number: "CO1", FileID: "file.co", Issuer: "CCPIT", IssuedOn: "2026-12-19",
				CertificateRef: &CertificateRef{CertificateID: "cert.1"}}
		}, "SCHEMA_INVALID /args/certificate/certificate_ref"},
	} {
		if got := record(c.edit); got != c.want {
			t.Errorf("%s, want %s", got, c.want)
		}
	}
}

// The closed contracts of shipment_document.issue and shipment_document.record_signed.
func TestIssueAndSignContracts(t *testing.T) {
	issue := func(edit func(*IssueShipmentDocuments)) string {
		c := IssueShipmentDocuments{IdempotencyKey: "k", DocumentSetID: "docs.1", ExpectedVersion: 2,
			Acknowledgements: []Acknowledgement{{Path: "/bill_of_lading/port_of_discharge", Reason: "改港"}}}
		edit(&c)
		return outcome(c.Validate())
	}
	for _, c := range []struct {
		edit func(*IssueShipmentDocuments)
		want string
	}{
		{func(*IssueShipmentDocuments) {}, "OK"},
		{func(c *IssueShipmentDocuments) { c.ExpectedVersion = 0 }, "SCHEMA_INVALID /args/expected_version"},
		{func(c *IssueShipmentDocuments) { c.Acknowledgements[0].Path = "bill_of_lading" }, "SCHEMA_INVALID /args/acknowledgements/0/path"},
		{func(c *IssueShipmentDocuments) { c.Acknowledgements[0].Reason = "" }, "SCHEMA_INVALID /args/acknowledgements/0/reason"},
		{func(c *IssueShipmentDocuments) {
			c.Acknowledgements = append(c.Acknowledgements, c.Acknowledgements[0])
		},
			"SCHEMA_INVALID /args/acknowledgements/1/path"},
		{func(c *IssueShipmentDocuments) { c.Acknowledgements = make([]Acknowledgement, MaxAcknowledgements+1) }, "SCHEMA_INVALID /args/acknowledgements"},
	} {
		if got := issue(c.edit); got != c.want {
			t.Errorf("issue: %s, want %s", got, c.want)
		}
	}
	sign := func(edit func(*RecordSignedShipmentDocument)) string {
		c := RecordSignedShipmentDocument{IdempotencyKey: "k", DocumentSetID: "docs.1", DocumentKind: DocumentCommercialInvoice, Version: 1,
			Language: LanguageZhCN, FileID: "file.stamped"}
		edit(&c)
		return outcome(c.Validate())
	}
	for _, c := range []struct {
		edit func(*RecordSignedShipmentDocument)
		want string
	}{
		{func(*RecordSignedShipmentDocument) {}, "OK"},
		{func(c *RecordSignedShipmentDocument) { c.DocumentKind = "x" }, "SCHEMA_INVALID /args/document_kind"},
		{func(c *RecordSignedShipmentDocument) { c.Version = 0 }, "SCHEMA_INVALID /args/version"},
		{func(c *RecordSignedShipmentDocument) { c.Language = "zh" }, "SCHEMA_INVALID /args/language"},
		{func(c *RecordSignedShipmentDocument) { c.FileID = "" }, "SCHEMA_INVALID /args/file_id"},
		{func(c *RecordSignedShipmentDocument) { c.EvidenceIDs = make([]string, MaxEvidence+1) }, "SCHEMA_INVALID /args/evidence_ids"},
	} {
		if got := sign(c.edit); got != c.want {
			t.Errorf("sign: %s, want %s", got, c.want)
		}
	}
	if !Rendered(DocumentPackingList) || Rendered(DocumentBillOfLading) || !DocumentKind(DocumentCertificateOfOrigin) || Language("de") {
		t.Fatal("kinds and languages")
	}
}
