package api

import (
	"errors"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func outcome(err error) string {
	if err == nil {
		return "OK"
	}
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	return err.Error()
}

var subject = SubjectRef{Kind: SubjectCommitmentLine, ID: "commitment.1", LineID: "L1"}

func record() RecordInspection {
	return RecordInspection{IdempotencyKey: "k", InspectionID: "insp.1", Subject: subject, InspectedQuantity: "120",
		ConformingQuantity: "100", NonconformingQuantity: "20", Sampling: Sampling{Method: SamplingSample, SampleSize: "32", Plan: "AQL 2.5 GII"},
		Standard: StandardRef{SourceID: "standard"}, Inspector: Inspector{Kind: InspectorAgency, Agency: &AgencyRef{PartyID: "agency",
			ServiceRoleID: "final", Revision: 1}}, ReportFileID: "report", InspectedAt: "2026-11-12T08:00:00Z", EvidenceIDs: []string{"ev"}}
}

// The closed contract of inspection.record (SM-QLT-INSPECTION, F-06, BT08).
func TestRecordInspectionContract(t *testing.T) {
	for _, c := range []struct {
		edit func(*RecordInspection)
		want string
	}{
		{func(*RecordInspection) {}, "OK"},
		{func(c *RecordInspection) {
			c.Inspector = Inspector{Kind: InspectorInternal}
			c.Sampling = Sampling{Method: SamplingFull}
		}, "OK"},
		{func(c *RecordInspection) { c.SupersedesInspectionID, c.Reason = "insp.0", "复检" }, "OK"},
		{func(c *RecordInspection) { c.IdempotencyKey = "" }, "SCHEMA_INVALID /idempotency_key"},
		{func(c *RecordInspection) { c.InspectionID = "a b" }, "SCHEMA_INVALID /args/inspection_id"},
		{func(c *RecordInspection) { c.Subject.Kind = "supplier_delivery_line" }, "SCHEMA_INVALID /args/subject/kind"},
		{func(c *RecordInspection) { c.Subject.ID = "" }, "SCHEMA_INVALID /args/subject/id"},
		{func(c *RecordInspection) { c.Subject.LineID = "" }, "SCHEMA_INVALID /args/subject/line_id"},
		{func(c *RecordInspection) { c.InspectedQuantity = "0" }, "SCHEMA_INVALID /args/inspected_quantity"},
		{func(c *RecordInspection) { c.InspectedQuantity = "1.0000001" }, "SCHEMA_INVALID /args/inspected_quantity"},
		{func(c *RecordInspection) { c.ConformingQuantity = "-1" }, "SCHEMA_INVALID /args/conforming_quantity"},
		{func(c *RecordInspection) { c.NonconformingQuantity = "" }, "SCHEMA_INVALID /args/nonconforming_quantity"},
		{func(c *RecordInspection) { c.ConformingQuantity = "101" }, "INVALID_KNOWN_VALUE /args/conforming_quantity"},
		{func(c *RecordInspection) { c.Sampling.Method = "skip" }, "SCHEMA_INVALID /args/sampling/method"},
		{func(c *RecordInspection) { c.Sampling.SampleSize = "" }, "SCHEMA_INVALID /args/sampling/sample_size"},
		{func(c *RecordInspection) { c.Sampling.SampleSize = "121" }, "INVALID_KNOWN_VALUE /args/sampling/sample_size"},
		{func(c *RecordInspection) { c.Sampling = Sampling{Method: SamplingFull, SampleSize: "1"} }, "SCHEMA_INVALID /args/sampling/sample_size"},
		{func(c *RecordInspection) { c.Sampling.Plan = " " }, "SCHEMA_INVALID /args/sampling/plan"},
		{func(c *RecordInspection) { c.Standard.SourceID = "" }, "SCHEMA_INVALID /args/standard/source_id"},
		{func(c *RecordInspection) { c.Inspector.Kind = "supplier" }, "SCHEMA_INVALID /args/inspector/kind"},
		{func(c *RecordInspection) { c.Inspector.Agency = nil }, "SCHEMA_INVALID /args/inspector/agency"},
		{func(c *RecordInspection) { c.Inspector.Kind = InspectorInternal }, "SCHEMA_INVALID /args/inspector/agency"},
		{func(c *RecordInspection) { c.Inspector.Agency.Revision = 0 }, "SCHEMA_INVALID /args/inspector/agency/revision"},
		{func(c *RecordInspection) { c.Inspector.Agency.PartyID = "" }, "SCHEMA_INVALID /args/inspector/agency/party_id"},
		{func(c *RecordInspection) { c.ReportFileID = "" }, "SCHEMA_INVALID /args/report_file_id"},
		{func(c *RecordInspection) { c.InspectedAt = "2026-11-12" }, "SCHEMA_INVALID /args/inspected_at"},
		{func(c *RecordInspection) { c.Reason = "复检" }, "SCHEMA_INVALID /args/reason"},
		{func(c *RecordInspection) { c.SupersedesInspectionID = "insp.0" }, "SCHEMA_INVALID /args/reason"},
		{func(c *RecordInspection) { c.SupersedesInspectionID, c.Reason = "insp.1", "复检" }, "SCHEMA_INVALID /args/supersedes_inspection_id"},
		{func(c *RecordInspection) { c.EvidenceIDs = []string{"ok", "not ok"} }, "SCHEMA_INVALID /args/evidence_ids/1"},
		{func(c *RecordInspection) { c.EvidenceIDs = make([]string, MaxEvidence+1) }, "SCHEMA_INVALID /args/evidence_ids"},
	} {
		cmd := record()
		c.edit(&cmd)
		if got := outcome(cmd.Validate()); got != c.want {
			t.Errorf("%+v: %s, want %s", cmd, got, c.want)
		}
	}
}

func TestDispositionContract(t *testing.T) {
	ok := DecideDisposition{IdempotencyKey: "k", InspectionID: "insp.1", ExpectedVersion: 1, Reason: "返工后复检",
		Items: []DispositionItem{{Action: DispositionRework, Quantity: "15"}, {Action: DispositionScrap, Quantity: "5"}}}
	for _, c := range []struct {
		edit func(*DecideDisposition)
		want string
	}{
		{func(*DecideDisposition) {}, "OK"},
		{func(c *DecideDisposition) { c.ExpectedVersion = 0 }, "SCHEMA_INVALID /args/expected_version"},
		{func(c *DecideDisposition) { c.Items = nil }, "SCHEMA_INVALID /args/items"},
		{func(c *DecideDisposition) { c.Items = make([]DispositionItem, MaxDispositions+1) }, "SCHEMA_INVALID /args/items"},
		{func(c *DecideDisposition) { c.Items[1].Action = "resell" }, "SCHEMA_INVALID /args/items/1/action"},
		{func(c *DecideDisposition) { c.Items[0].Quantity = "0" }, "SCHEMA_INVALID /args/items/0/quantity"},
		{func(c *DecideDisposition) { c.Items[1].Action = DispositionRework }, "DUPLICATE_LINE_ID /args/items/1/action"},
		{func(c *DecideDisposition) { c.Reason = "" }, "SCHEMA_INVALID /args/reason"},
		// S5R-D18: a concession cites the customer's consent as evidence.
		{func(c *DecideDisposition) { c.Items[0].Action = DispositionConcession }, "SCHEMA_INVALID /args/evidence_ids"},
		{func(c *DecideDisposition) {
			c.Items[0].Action, c.EvidenceIDs = DispositionConcession, []string{"consent"}
		}, "OK"},
	} {
		cmd := ok
		cmd.Items = append([]DispositionItem{}, ok.Items...)
		c.edit(&cmd)
		if got := outcome(cmd.Validate()); got != c.want {
			t.Errorf("%+v: %s, want %s", cmd, got, c.want)
		}
	}
}

func TestReleaseContracts(t *testing.T) {
	ok := GrantRelease{IdempotencyKey: "k", ReleaseID: "rel.1", Subject: subject, Quantity: "120", ValidFrom: "2026-11-12T00:00:00Z",
		ValidUntil: "2026-12-12T00:00:00Z"}
	for _, c := range []struct {
		edit func(*GrantRelease)
		want string
	}{
		{func(*GrantRelease) {}, "OK"},
		{func(c *GrantRelease) { c.ValidFrom = "" }, "OK"},
		{func(c *GrantRelease) { c.ReleaseID = "" }, "SCHEMA_INVALID /args/release_id"},
		{func(c *GrantRelease) { c.Subject.Kind = "" }, "SCHEMA_INVALID /args/subject/kind"},
		{func(c *GrantRelease) { c.Quantity = "0" }, "SCHEMA_INVALID /args/quantity"},
		{func(c *GrantRelease) { c.ValidUntil = "" }, "SCHEMA_INVALID /args/valid_until"},
		{func(c *GrantRelease) { c.ValidFrom = "tomorrow" }, "SCHEMA_INVALID /args/valid_from"},
		{func(c *GrantRelease) { c.ValidFrom = c.ValidUntil }, "INVALID_KNOWN_VALUE /args/valid_until"},
	} {
		cmd := ok
		c.edit(&cmd)
		if got := outcome(cmd.Validate()); got != c.want {
			t.Errorf("%+v: %s, want %s", cmd, got, c.want)
		}
	}
	revoke := RevokeRelease{IdempotencyKey: "k", ReleaseID: "rel.1", Reason: "复检发现色差"}
	if err := revoke.Validate(); err != nil {
		t.Fatal(err)
	}
	revoke.Reason = "\x07"
	if got := outcome(revoke.Validate()); got != "SCHEMA_INVALID /args/reason" {
		t.Fatal(got)
	}
}

// The codes QLT declares and their catalog categories.
func TestCodes(t *testing.T) {
	for code, want := range map[string]kernel.Category{CodeQualityReleaseExceedsConforming: kernel.CategoryInvalid,
		CodeQualityReleaseRequired: kernel.CategoryConflict, CodeReferenceStale: kernel.CategoryConflict,
		CodeBusinessKeyConflict: kernel.CategoryConflict, CodeDependencyNotSupported: kernel.CategoryInvalid,
		CodeInspectionQuantityExceeded: kernel.CategoryInvalid} {
		if c, ok := Category(code); !ok || c != want {
			t.Errorf("%s: %s", code, c)
		}
	}
	if len(Codes()) != 6 {
		t.Fatal(Codes())
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an undeclared code panics")
		}
	}()
	_ = Reject("SURPRISE", "/", "")
}

// QualityReleaseGuard v1 as FUL reads it (R26, QLT-INV-002).
func TestQualityReleaseGuardVerdict(t *testing.T) {
	ref := QualityReleaseRef{ReleaseID: "rel.1"}
	at := time.Date(2026, 11, 12, 8, 0, 0, 123456789, time.FixedZone("CST", 8*3600))
	if args := QualityReleaseGuardArgs(ref, at); len(args) != 2 || args[0] != "rel.1" || !args[1].(time.Time).Equal(kernel.Instant(at)) ||
		args[1].(time.Time).Location() != time.UTC {
		t.Fatalf("args %v", args)
	}
	str := func(s string) *string { return &s }
	num := func(n int64) *int64 { return &n }
	from, until := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	row := func(known bool, status, code *string) func(dest ...any) error {
		return func(dest ...any) error {
			*dest[0].(*bool) = known
			if !known {
				*dest[15].(**string) = code
				return nil
			}
			vals := []*string{status, str(SubjectCommitmentLine), str("commitment.1"), str("L1"), str("supplier.1"), str("order.1"), str("OL1"),
				str("sku.1")}
			for i, v := range vals {
				*dest[1+i].(**string) = v
			}
			*dest[9].(**int64) = num(2)
			*dest[10].(**string) = str("pcs")
			*dest[11].(**int64) = num(1)
			*dest[12].(**string) = str("120")
			*dest[13].(**time.Time) = &from
			*dest[14].(**time.Time) = &until
			*dest[15].(**string) = code
			return nil
		}
	}
	v, err := ScanQualityReleaseGuard(ref, row(true, str(StatusGranted), nil))
	want := ReleaseScope{Subject: subject, SupplierID: "supplier.1", OrderID: "order.1", OrderLineID: "OL1", SkuID: "sku.1", SpecRevision: 2,
		UnitID: "pcs", UnitRevision: 1}
	if err != nil || !v.Usable() || v.Check("/x") != nil || v.Scope != want || v.Quantity != "120" || !v.ValidFrom.Equal(from) ||
		!v.ValidUntil.Equal(until) || v.Ref != ref {
		t.Fatalf("granted %+v, %v", v, err)
	}
	for _, c := range []struct {
		v      func(dest ...any) error
		reason string
	}{
		{row(true, str(StatusRevoked), str(CodeQualityReleaseRequired)), "The quality release was revoked"},
		{row(true, str(StatusGranted), str(CodeQualityReleaseRequired)), "The quality release is not valid at this instant"},
		{row(false, nil, str(CodeQualityReleaseRequired)), "The quality release is unknown"},
	} {
		v, err := ScanQualityReleaseGuard(ref, c.v)
		rej, ok := kernel.AsRejection(v.Check("/args/lines/0/quality_release_id"))
		if err != nil || v.Usable() || !ok || rej.Code != CodeQualityReleaseRequired || rej.Category != kernel.CategoryConflict ||
			rej.Fields[0].Path != "/args/lines/0/quality_release_id" || rej.Fields[0].Reason != c.reason {
			t.Errorf("%+v: %v", v, rej)
		}
	}
	boom := errors.New("scan")
	if _, err = ScanQualityReleaseGuard(ref, func(...any) error { return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	r := ResolvedRelease{Code: CodeQualityReleaseRequired, State: StateExpired}
	if rej, ok := kernel.AsRejection(r.Check("/r")); !ok || rej.Code != CodeQualityReleaseRequired || (ResolvedRelease{}).Check("/r") != nil {
		t.Fatal("resolved check")
	}
	if outcome(ValidateReleaseIDs([]string{"ok", ""})) != "SCHEMA_INVALID /release_ids/1" || ValidateReleaseIDs(make([]string, MaxLookup+1)) == nil {
		t.Fatal("lookup ids")
	}
}
