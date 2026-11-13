//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5R unit R5 (WBS S5R-05) on PostgreSQL 16: decision S5R-D18 — the frozen quantity a revocation
// records from FUL's ReleaseConsumption v1 (Context Map R79 FUL→QLT), its release again after a
// re-inspection, and approved concessions (tactical model QLT-INV-001, QLT-INV-002, SM-QLT-RELEASE,
// SM-QLT-INSPECTION; BT21; UL QLT-Disposition, QLT-QualityRelease). FUL and QLT are the real
// modules of fulWorld, QLT wired to FUL's SQL function through qlt.FULReleaseConsumption.

// consumed runs FUL's ReleaseConsumption v1 as QLT does, as the application role in company's scope.
func (w *fulWorld) consumed(company, releaseID string) ([]string, error) {
	db, err := bentopg.NewDB(pgtest.AppPool(w.t))
	if err != nil {
		return nil, err
	}
	var out []string
	err = bento.ScopeTransactor{DB: db, Scope: w.scope(company)}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		rows, err := exec.Query(ctx, fulapi.ReleaseConsumptionSQL, fulapi.ReleaseConsumptionArgs(releaseID)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := fulapi.ScanReleaseConsumption(rows.Scan)
			if err != nil {
				return err
			}
			out = append(out, strings.Join([]string{c.UnitID, strconv.FormatInt(c.UnitRevision, 10), c.Quantity, strconv.FormatInt(c.Shipments, 10)}, " "))
		}
		return rows.Err()
	})
	return out, err
}

func (w *fulWorld) qltStanding(commitment string) qltapi.SubjectView {
	w.t.Helper()
	v, err := w.qlt.GetSubject(w.qc, qltapi.SubjectRef{Kind: qltapi.SubjectCommitmentLine, ID: commitment, LineID: "L1"})
	w.must(err)
	return v
}

// ReleaseConsumption v1 as the database publishes it (R79): a SECURITY DEFINER function with a fixed
// search_path that only the application role may execute, refusing a NULL release; it answers what
// the confirmed shipments shipped under a release — prepared ones do not count — in the caller's
// company only.
func TestReleaseConsumptionFunction(t *testing.T) {
	w := newFulWorld(t)
	var definer, publicExec, appExec bool
	var config []string
	w.must(w.execScan("company.A", `SELECT p.prosecdef, p.proconfig, has_function_privilege('public', p.oid, 'EXECUTE'),
		has_function_privilege('brant_app', p.oid, 'EXECUTE') FROM pg_proc p WHERE p.oid = 'brant_ful.release_consumption_v1(text)'::regprocedure`,
		&definer, &config, &publicExec, &appExec))
	if !definer || publicExec || !appExec || !slices.Contains(config, "search_path=pg_catalog, pg_temp") {
		t.Fatalf("definer %v config %v public %v app %v", definer, config, publicExec, appExec)
	}
	if err := w.exec("company.A", "SELECT * FROM brant_ful.release_consumption_v1(NULL)"); sqlCode(err) != "22004" {
		t.Fatalf("NULL: %s", sqlCode(err))
	}

	rel := w.released("commitment.1", "100")
	d := w.delivered("commitment.1", "100")
	if got, err := w.consumed("company.A", rel); err != nil || len(got) != 0 {
		t.Fatalf("nothing shipped yet %q %v", got, err)
	}
	for _, s := range []struct{ id, quantity string }{{"shipment.1", "60"}, {"shipment.2", "30"}} {
		_, err := w.prepare(w.merch, s.id, s.quantity, d, rel)
		w.must(err)
	}
	if got, err := w.consumed("company.A", rel); err != nil || len(got) != 0 {
		t.Fatalf("prepared shipments do not count %q %v", got, err)
	}
	w.granted("shipment.1")
	_, err := w.confirm(w.merch, "shipment.1", 1)
	w.must(err)
	unit := w.line.UnitRef.ID + " " + strconv.FormatInt(w.line.UnitRef.Revision, 10)
	if got, err := w.consumed("company.A", rel); err != nil || !slices.Equal(got, []string{unit + " 60 1"}) {
		t.Fatalf("after the confirmation %q %v", got, err)
	}
	if got, err := w.consumed("company.B", rel); err != nil || len(got) != 0 {
		t.Fatalf("another company %q %v", got, err)
	}
}

// BT21 with S5R-D18 on the real FUL (QLT-INV-001, QLT-INV-002): 100 released, FUL confirms 60 and
// prepares 40; our QC revokes the release, which records 60 shipped and 40 frozen from FUL's
// ReleaseConsumption v1; the prepared 40 cannot ship. Without a re-inspection the frozen 40 is not
// releasable (QUALITY_RELEASE_EXCEEDS_CONFORMING); a re-inspection after the revocation supersedes
// the inspection the release was judged on and restates the result for all 100 — the shipped 60
// among them — and the frozen 40 is released again and ships on the new release.
func TestFrozenQuantityReleasedAgainAfterReinspection(t *testing.T) {
	w := newFulWorld(t)
	rel := w.released("commitment.1", "100")
	d := w.delivered("commitment.1", "100")
	_, err := w.prepare(w.merch, "shipment.1", "60", d, rel)
	w.must(err)
	w.granted("shipment.1")
	_, err = w.confirm(w.merch, "shipment.1", 1)
	w.must(err)
	_, err = w.prepare(w.merch, "shipment.2", "40", d, rel)
	w.must(err)
	w.granted("shipment.2")

	r, err := w.qlt.RevokeRelease(w.qc, qltapi.RevokeRelease{IdempotencyKey: w.key(), ReleaseID: rel, Reason: "复检发现色差，撤回放行"})
	if err != nil || r.Status != qltapi.StatusRevoked || r.ShippedQuantity != "60" || r.FrozenQuantity != "40" {
		t.Fatalf("revocation %+v %v", r, err)
	}
	_, err = w.confirm(w.merch, "shipment.2", 1)
	w.want(err, fulapi.CodeQualityReleaseRequired)
	if s := w.qltStanding("commitment.1"); s.Conforming != "100" || s.Released != "100" || s.Frozen != "40" || s.Releasable != "0" {
		t.Fatalf("frozen %+v", s)
	}
	subject := qltapi.SubjectRef{Kind: qltapi.SubjectCommitmentLine, ID: "commitment.1", LineID: "L1"}
	grant := func(id, quantity string) (qltapi.ReleaseReceipt, error) {
		return w.qlt.GrantRelease(w.qc, qltapi.GrantRelease{IdempotencyKey: w.key(), ReleaseID: id, Subject: subject, Quantity: quantity,
			ValidUntil: kernel.FormatTimestamp(time.Now().Add(30 * 24 * time.Hour))})
	}
	_, err = grant("rel.again", "40")
	w.want(err, qltapi.CodeQualityReleaseExceedsConforming)

	_, err = w.qlt.RecordInspection(w.qc, qltapi.RecordInspection{IdempotencyKey: w.key(), InspectionID: "insp.again", Subject: subject,
		InspectedQuantity: "100", ConformingQuantity: "100", NonconformingQuantity: "0", Sampling: qltapi.Sampling{Method: qltapi.SamplingFull},
		Standard: qltapi.StandardRef{SourceID: "standard"}, Inspector: qltapi.Inspector{Kind: qltapi.InspectorInternal}, ReportFileID: "report",
		InspectedAt: kernel.FormatTimestamp(time.Now().Add(-time.Second)), SupersedesInspectionID: "insp.commitment.1", Reason: "撤回后复检冻结货物"})
	w.must(err)
	if s := w.qltStanding("commitment.1"); s.Released != "60" || s.Frozen != "0" || s.Releasable != "40" {
		t.Fatalf("after the re-inspection %+v", s)
	}
	g, err := grant("rel.again", "40")
	if err != nil || g.Status != qltapi.StatusGranted || g.Concession {
		t.Fatalf("the frozen 40 again %+v %v", g, err)
	}
	_, err = w.prepare(w.merch, "shipment.3", "40", d, "rel.again")
	w.must(err)
	w.granted("shipment.3")
	_, err = w.confirm(w.merch, "shipment.3", 1)
	w.must(err)
	unit := w.line.UnitRef.ID + " " + strconv.FormatInt(w.line.UnitRef.Revision, 10)
	if got, err := w.consumed("company.A", "rel.again"); err != nil || !slices.Equal(got, []string{unit + " 40 1"}) {
		t.Fatalf("shipped on the new release %q %v", got, err)
	}
	if got, err := w.consumed("company.A", rel); err != nil || !slices.Equal(got, []string{unit + " 60 1"}) {
		t.Fatalf("shipped on the revoked release %q %v", got, err)
	}
}

// S5R-D18 (QLT-INV-001, SM-QLT-INSPECTION, UL QLT-Disposition): an inspection finds 20 of 120
// nonconforming; the disposition concedes 15 on the customer's consent and scraps 5 — without
// evidence of the consent it is refused (SCHEMA_INVALID /args/evidence_ids). The conceded 15 become
// releasable next to the conforming 100: the release that takes them carries the concession flag
// and quantity, in its read, in QualityReleaseRef v1 and in its event. A re-inspection that
// supersedes the inspection restates the result and its own disposition decides again: the
// superseded concession no longer counts.
func TestApprovedConcessionIsReleasableAndMarked(t *testing.T) {
	x := newQltWorld(t)
	x.subjects.put(commitmentLine("commitment.1", "120"))
	line := subjectOf("commitment.1")
	_, err := x.inspect(x.qc, "insp.1", line, "120", "100")
	x.must(err)
	x.source(x.qc, "consent", "合成客户邮件：同意 15 件轻微色差让步接收。")
	dispose := qltapi.DecideDisposition{IdempotencyKey: x.key(), InspectionID: "insp.1", ExpectedVersion: 1, Reason: "客户同意让步接收 15 件，报废 5 件",
		Items: []qltapi.DispositionItem{{Action: qltapi.DispositionConcession, Quantity: "15"}, {Action: qltapi.DispositionScrap, Quantity: "5"}}}
	_, err = x.q.DecideDisposition(x.qc, dispose)
	if code(err) != kernel.CodeSchemaInvalid || path(err) != "/args/evidence_ids" {
		t.Fatalf("a concession without the customer's consent: %s at %s", code(err), path(err))
	}
	dispose.IdempotencyKey, dispose.EvidenceIDs = x.key(), []string{"consent"}
	_, err = x.q.DecideDisposition(x.qc, dispose)
	x.must(err)
	if s := x.standing("commitment.1"); s.Conforming != "100" || s.Conceded != "15" || s.Releasable != "115" || s.Inspections[0].Conceded != "15" {
		t.Fatalf("the conceded 15 count %+v", s)
	}
	first, err := x.grant(x.qc, "rel.1", line, "100")
	if err != nil || first.Concession || first.ConcessionQuantity != "0" {
		t.Fatalf("conforming goods first %+v %v", first, err)
	}
	_, err = x.grant(x.qc, "rel.2", line, "15.000001")
	x.want(err, qltapi.CodeQualityReleaseExceedsConforming)
	second, err := x.grant(x.qc, "rel.2", line, "15")
	if err != nil || !second.Concession || second.ConcessionQuantity != "15" {
		t.Fatalf("the conceded 15 %+v %v", second, err)
	}
	v, err := x.q.GetRelease(x.qc, "rel.2")
	x.must(err)
	if !v.Concession || v.ConcessionQuantity != "15" || v.Conceded != "15" || v.Conforming != "100" || v.ReleasedBefore != "100" {
		t.Fatalf("release %+v", v)
	}
	resolved, err := x.q.ResolveReleases(x.qc, []string{"rel.1", "rel.2"}, time.Now())
	x.must(err)
	if r := resolved[qltapi.QualityReleaseRef{ReleaseID: "rel.2"}]; !r.Concession || r.ConcessionQuantity != "15" || r.Code != "" ||
		resolved[qltapi.QualityReleaseRef{ReleaseID: "rel.1"}].Concession {
		t.Fatalf("QualityReleaseRef v1 %+v", resolved)
	}
	events := x.qltEvents(qltapi.EventQualityReleased)
	if len(events) != 2 || events[0].Payload["concession"] != false || events[1].Payload["concession"] != true ||
		events[1].Payload["concession_quantity"] != "15" {
		t.Fatalf("events %v", events)
	}
	if s := x.standing("commitment.1"); s.Released != "115" || s.Releasable != "0" || !s.Releases[1].Concession {
		t.Fatalf("all released %+v", s)
	}
	// A re-inspection supersedes insp.1 and restates 120 inspected, 110 conforming; the superseded
	// concession no longer counts: 110 less the 115 released leaves nothing.
	_, err = x.inspect(x.qc, "insp.2", line, "120", "110", supersedes("insp.1"))
	x.must(err)
	if s := x.standing("commitment.1"); s.Conforming != "110" || s.Conceded != "0" || s.Releasable != "0" {
		t.Fatalf("after the re-inspection %+v", s)
	}
}
