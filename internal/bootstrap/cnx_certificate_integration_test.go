//go:build integration

package bootstrap_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CNX's official certificate results and CertificateResultRef v1 (S5 C5, WBS S5-10 CNX half; tactical
// model CNX-AGG-CertificateResult, CNX-INV-007; MD-40; Context Map R56; baseline CN-R14, CN-AT14
// certificate half; S5 decisions E-1 manual paths, D-8, D-17). Python V0.50 has no CNX: the vectors
// are synthetic and cite the design.

// certWorld is the declaration world (TRD's real active order of 120 PCS of sku.1 at 8.00 USD, real FUL
// shipments and export lots, the order revision's trade compliance evaluation stating destination DE,
// CNX on its real FUL adapter) with the documentation clerk's certificate file and verification
// source.
type certWorld struct {
	*declWorld
	doc    context.Context
	coFile string
	page   string
}

func newCertWorld(t *testing.T) *certWorld {
	t.Helper()
	w := &certWorld{declWorld: newDeclWorld(t)}
	w.doc = w.cx.doc1
	_, err := w.file(w.doc, "co.file", "co.txt", evdapi.MimeText, "合成原产地证扫描件（测试数据）", "")
	w.must(err)
	w.coFile, w.page = "co.file", w.source(w.doc, "co.verify", "合成签发机构证书核验页：证书真实有效（测试数据）")
	return w
}

// certificate is a certificate of origin number of 合成贸促会 for market, applying to object, valid
// from from until until, on the world's file and verification page.
func (w *certWorld) certificate(number, market string, object cnxapi.CertificateObject, from, until time.Time) cnxapi.RegisterCertificate {
	return cnxapi.RegisterCertificate{IdempotencyKey: w.key(), CertificateKind: cnxapi.CertificateOfOrigin, Number: number, Issuer: "合成贸促会（测试）",
		IssuedOn: from.Format(time.DateOnly), Object: object, Market: market, ValidFrom: kernel.FormatTimestamp(from),
		ValidUntil: kernel.FormatTimestamp(until), FileID: w.coFile, Basis: "原件与签发机构核验页逐项核对一致（合成）", EvidenceIDs: []string{w.page}}
}

func (w *certWorld) register(cmd cnxapi.RegisterCertificate) cnxapi.CertificateReceipt {
	w.t.Helper()
	r, err := w.cnx.RegisterCertificate(w.doc, cmd)
	w.must(err)
	return r
}

func specs(sku string, revision int64) cnxapi.CertificateObject {
	return cnxapi.CertificateObject{Kind: cnxapi.ObjectSkuSpec, SkuSpecs: []cnxapi.SpecRef{{SkuID: sku, SpecRevision: revision}}}
}

func onShipment(id string, version int64, certificate string) cnxapi.CertificateUse {
	return cnxapi.CertificateUse{Ref: cnxapi.CertificateRef{CertificateID: certificate, Version: version}, Kind: cnxapi.CertificateOfOrigin,
		Target: cnxapi.ExportTarget{Kind: cnxapi.TargetShipment, ID: id}, At: time.Now()}
}

// CN-AT14 certificate half (单证缺件或证书不适用：列缺件、适用对象、责任人及期限；已上传文件不直接满足要求),
// CNX-INV-007, MD-40, CN-R14, D-17, on a real FUL shipment and export lot:
//   - an AI agent holding certificate.register is refused (PERMISSION_DENIED, audited): only a human
//     registers, and the registering human is the verifier;
//   - the documentation clerk registers a certificate of origin of the shipment for DE, one of another
//     specification revision, one for the US and an expired one; the issuer's number is unique; an
//     object FUL does not show and a file the clerk cannot read are refused;
//   - CertificateResultRef v1 answers for the shipment (destination DE from the order revision's trade
//     compliance evaluation): applicable; CERTIFICATE_NOT_APPLICABLE for another specification
//     (goods), another market (market), expired (expired) and another kind (kind); REFERENCE_INVALID
//     for a certificate CNX lacks;
//   - history is kept: the US certificate is corrected to DE as revision 2 (a stale version, another
//     kind or a correction stating nothing new are refused); revision 2 applies, revision 1 is
//     REFERENCE_STALE and still reads as registered; CertificateRegistered is published for every
//     revision;
//   - the export lot's bundle lists the certificate of origin missing with its owner and due date; it
//     cannot be verified on an attachment (DOCUMENT_REQUIREMENT_UNVERIFIED) nor on a certificate of
//     another specification, market or an expired one (CERTIFICATE_NOT_APPLICABLE) or an unknown one
//     (REFERENCE_INVALID); on the shipment's certificate it is verified and the bundle records the
//     revision and the lot line it covers.
func TestCNAT14CertificateResultsApplyOnlyToTheirObjectMarketAndValidity(t *testing.T) {
	w := newCertWorld(t)
	w.shipment("shipment.C", "60", "commitment.1")
	now := time.Now().UTC()
	from, until := now.Add(-time.Hour), now.Add(30*24*time.Hour)
	ai := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("ai", "agent.cert"), DataScope: "company",
		Permissions: []string{cnxapi.CommandRegisterCertificate, "cnx.read", "evd.read", "ful.read"}})
	_, err := w.cnx.RegisterCertificate(ai, w.certificate("SYN-CO-AI", "DE", specs("sku.1", w.spec), from, until))
	w.want(err, kernel.CodePermissionDenied)
	auditor := w.actor("auditor.cert", "cnx.audit")
	trail, err := w.cnx.Audit(auditor)
	w.must(err)
	denied := 0
	for _, e := range trail {
		if e.Command == cnxapi.CommandRegisterCertificate && e.Code == kernel.CodePermissionDenied && e.Actor.ID == "agent.cert" {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("audited AI refusals %d", denied)
	}

	shipment := cnxapi.CertificateObject{Kind: cnxapi.ObjectShipment, ID: "shipment.C"}
	good := w.register(w.certificate("SYN-CO-1", "DE", shipment, from, until))
	if good.Status != cnxapi.CertificateRegistered || good.RowVersion != 1 || good.Revision != 1 || len(good.ContentSHA256) != 64 ||
		!strings.HasPrefix(good.CertificateID, "crt-") {
		t.Fatalf("registered %+v", good)
	}
	otherSpec := w.register(w.certificate("SYN-CO-2", "DE", specs("sku.1", w.spec+1), from, until))
	us := w.register(w.certificate("SYN-CO-3", "US", specs("sku.1", w.spec), from, until))
	expired := w.register(w.certificate("SYN-CO-4", "DE", specs("sku.1", w.spec), now.Add(-60*24*time.Hour), now.Add(-30*24*time.Hour)))
	for _, x := range []struct {
		cmd        cnxapi.RegisterCertificate
		code, path string
	}{
		{w.certificate("SYN-CO-1", "FR", specs("sku.1", w.spec), from, until), cnxapi.CodeBusinessKeyConflict, "/args/number"},
		{w.certificate("SYN-CO-5", "DE", cnxapi.CertificateObject{Kind: cnxapi.ObjectShipment, ID: "shipment.none"}, from, until),
			kernel.CodeReferenceInvalid, "/args/object/id"},
		{func() cnxapi.RegisterCertificate {
			c := w.certificate("SYN-CO-6", "DE", shipment, from, until)
			c.FileID = "file.none"
			return c
		}(), kernel.CodeReferenceInvalid, "/args/file_id"},
		{func() cnxapi.RegisterCertificate {
			c := w.certificate("SYN-CO-7", "DE", shipment, from, until)
			c.EvidenceIDs = []string{"source.none"}
			return c
		}(), kernel.CodeReferenceInvalid, "/args/evidence_ids/0"},
	} {
		_, err := w.cnx.RegisterCertificate(w.doc, x.cmd)
		if code(err) != x.code || path(err) != x.path {
			t.Errorf("%s: want %s %s, got %s %s", x.cmd.Number, x.code, x.path, code(err), path(err))
		}
	}

	inspection := onShipment("shipment.C", 0, good.CertificateID)
	inspection.Kind = cnxapi.CertificateInspection
	results, err := w.cnx.ResolveCertificates(w.doc, []cnxapi.CertificateUse{onShipment("shipment.C", 0, good.CertificateID),
		onShipment("shipment.C", 0, otherSpec.CertificateID), onShipment("shipment.C", 0, us.CertificateID),
		onShipment("shipment.C", 0, expired.CertificateID), inspection, onShipment("shipment.C", 0, "crt-none")})
	w.must(err)
	for i, want := range []struct{ code, reason string }{
		{"", cnxapi.CertificateApplicable}, {cnxapi.CodeCertificateNotApplicable, cnxapi.CertificateGoods},
		{cnxapi.CodeCertificateNotApplicable, cnxapi.CertificateMarket}, {cnxapi.CodeCertificateNotApplicable, cnxapi.CertificateExpired},
		{cnxapi.CodeCertificateNotApplicable, cnxapi.CertificateKindMismatch}, {kernel.CodeReferenceInvalid, cnxapi.CertificateUnknown},
	} {
		if r := results[i]; r.Code != want.code || r.Reason != want.reason {
			t.Errorf("use %d: want %s %s, got %s %s", i, want.code, want.reason, r.Code, r.Reason)
		}
	}
	if r := results[0]; !r.Applicable || r.Destination != "DE" || len(r.Covered) != 1 || r.Covered[0] != "S1" || r.Revision != 1 ||
		r.ContentSHA256 != good.ContentSHA256 || r.VerifiedBy.ID != "doc.1" || r.Check("/x") != nil {
		t.Fatalf("applicable %+v", r)
	}
	if r := results[1]; len(r.Uncovered) != 1 || r.Uncovered[0] != "S1" || r.Check("/x") == nil {
		t.Fatalf("another specification %+v", r)
	}

	fixed := w.certificate("SYN-CO-3", "DE", specs("sku.1", w.spec), from, until)
	fixed.CertificateID, fixed.ExpectedVersion, fixed.Reason = us.CertificateID, 1, "签发机构更正进口国（合成）"
	for _, x := range []struct {
		mod        func(*cnxapi.RegisterCertificate)
		code, path string
	}{
		{func(c *cnxapi.RegisterCertificate) { c.ExpectedVersion = 2 }, kernel.CodeVersionConflict, "/args/expected_version"},
		{func(c *cnxapi.RegisterCertificate) { c.CertificateKind = cnxapi.CertificateInspection }, kernel.CodeInvalidKnownValue, "/args/certificate_kind"},
		{func(c *cnxapi.RegisterCertificate) { c.Market = "US" }, kernel.CodeInvalidKnownValue, "/args/certificate_id"},
		{func(c *cnxapi.RegisterCertificate) { c.CertificateID = "crt-none" }, kernel.CodeNotFound, "/args/certificate_id"},
	} {
		c := fixed
		c.IdempotencyKey = w.key()
		x.mod(&c)
		_, err := w.cnx.RegisterCertificate(w.doc, c)
		if code(err) != x.code || path(err) != x.path {
			t.Errorf("correction: want %s %s, got %s %s", x.code, x.path, code(err), path(err))
		}
	}
	corrected := w.register(fixed)
	if corrected.CertificateID != us.CertificateID || corrected.RowVersion != 2 || corrected.Revision != 2 || corrected.ContentSHA256 == us.ContentSHA256 {
		t.Fatalf("corrected %+v", corrected)
	}
	results, err = w.cnx.ResolveCertificates(w.doc, []cnxapi.CertificateUse{onShipment("shipment.C", 0, us.CertificateID),
		onShipment("shipment.C", 1, us.CertificateID), onShipment("shipment.C", 2, us.CertificateID), onShipment("shipment.C", 3, us.CertificateID)})
	w.must(err)
	if results[0].Code != "" || results[0].Revision != 2 || results[1].Code != cnxapi.CodeReferenceStale || results[1].Reason != cnxapi.CertificateSuperseded ||
		results[1].Market != "US" || results[1].CurrentRevision != 2 || results[2].Code != "" || results[3].Code != kernel.CodeReferenceInvalid {
		t.Fatalf("after the correction %+v", results)
	}
	v, err := w.cnx.GetCertificate(w.doc, us.CertificateID)
	w.must(err)
	if len(v.Revisions) != 2 || v.CurrentRevision != 2 || v.RowVersion != 2 || v.State != cnxapi.LicenseValid || v.Revisions[0].Market != "US" ||
		v.Revisions[0].Reason != "" || v.Revisions[0].DocumentSHA256 != us.ContentSHA256 || v.Revisions[1].Market != "DE" ||
		v.Revisions[1].Supersedes != 1 || v.Revisions[1].Reason == "" || v.Revisions[1].VerifiedBy.ID != "doc.1" || v.Revisions[1].Basis == "" ||
		len(v.Revisions[1].Evidence) != 1 || v.Revisions[1].File.SourceID != "co.file" {
		t.Fatalf("history %+v", v)
	}
	if g, err := w.cnx.GetCertificate(w.doc, good.CertificateID); err != nil || g.Revisions[0].Object.ID != "shipment.C" ||
		len(g.Revisions[0].Object.Lines) != 1 || g.Revisions[0].Object.Lines[0].SkuID != "sku.1" || g.Revisions[0].Object.Lines[0].Quantity != "60" ||
		g.Revisions[0].Object.OrderID != w.order.ObjectID {
		t.Fatalf("shipment certificate %+v %v", g, err)
	}
	list, err := w.cnx.Certificates(w.doc)
	w.must(err)
	states := map[string]string{}
	for _, s := range list {
		states[s.CertificateID] = s.State
	}
	if len(list) != 4 || states[expired.CertificateID] != cnxapi.LicenseExpired || states[good.CertificateID] != cnxapi.LicenseValid {
		t.Fatalf("list %+v", list)
	}
	var registered []event
	for _, e := range w.events("company.A", cnxapi.Source) {
		if e.Type == cnxapi.EventCertificateRegistered {
			registered = append(registered, e)
		}
	}
	if len(registered) != 5 || registered[0].Payload["certificate_id"] != good.CertificateID || registered[0].Payload["object_id"] != "shipment.C" ||
		registered[4].Payload["revision"] != float64(2) || registered[4].Payload["supersedes"] != float64(1) || registered[4].Payload["market"] != "DE" {
		t.Fatalf("events %+v", registered)
	}

	// The bundle of the shipment's export lot (CN-AT14 certificate half).
	w.granted("shipment.C")
	_, err = w.confirm(w.merch, "shipment.C", 1)
	w.must(err)
	_, err = w.registerLot(w.merch, "lot.C", "shipment.C", lotLine("E1", "S1", "60"))
	w.must(err)
	missing := bundleRequirement("co", cnxapi.RequirementCertificateOfOrigin, cnxapi.RequirementMissing)
	missing.Owner = cnxapi.RequirementOwner{Kind: cnxapi.OwnerRole, ID: "documentation"}
	b, err := w.cnx.CheckBundle(w.doc, cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: "lot.C", Requirements: []cnxapi.Requirement{missing}})
	if err != nil || b.Status != cnxapi.BundleIncomplete || len(b.Missing) != 1 || b.Missing[0] != "co" {
		t.Fatalf("missing certificate %+v %v", b, err)
	}
	verified := func(ids ...string) []cnxapi.Requirement {
		r := bundleRequirement("co", cnxapi.RequirementCertificateOfOrigin, cnxapi.RequirementVerified, w.page)
		r.CertificateIDs = ids
		return []cnxapi.Requirement{r}
	}
	for _, x := range []struct {
		ids        []string
		code, path string
	}{
		{nil, cnxapi.CodeDocumentRequirementUnverified, "/args/requirements/0/status"},
		{[]string{otherSpec.CertificateID}, cnxapi.CodeCertificateNotApplicable, "/args/requirements/0/certificate_ids/0"},
		{[]string{good.CertificateID, expired.CertificateID}, cnxapi.CodeCertificateNotApplicable, "/args/requirements/0/certificate_ids/1"},
		{[]string{"crt-none"}, kernel.CodeReferenceInvalid, "/args/requirements/0/certificate_ids/0"},
	} {
		_, err := w.cnx.CheckBundle(w.doc, cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: "lot.C", ExpectedVersion: 1, Requirements: verified(x.ids...)})
		if code(err) != x.code || path(err) != x.path {
			t.Errorf("%v: want %s %s, got %s %s", x.ids, x.code, x.path, code(err), path(err))
		}
	}
	done, err := w.cnx.CheckBundle(w.doc, cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: "lot.C", ExpectedVersion: 1,
		Requirements: verified(good.CertificateID), ExpectComplete: true})
	if err != nil || done.Status != cnxapi.BundleComplete || done.RowVersion != 2 {
		t.Fatalf("verified on the certificate %+v %v", done, err)
	}
	bv, err := w.cnx.GetBundle(w.doc, done.BundleID)
	w.must(err)
	if c := bv.Requirements[0].Certificates; len(c) != 1 || c[0].CertificateID != good.CertificateID || c[0].Revision != 1 || c[0].Number != "SYN-CO-1" ||
		c[0].Market != "DE" || c[0].State != cnxapi.LicenseValid || len(c[0].Covered) != 1 || c[0].Covered[0] != "E1" ||
		c[0].DocumentSHA256 != good.ContentSHA256 || len(bv.Checks[0].Requirements[0].Certificates) != 0 {
		t.Fatalf("bundle %+v", bv.Requirements)
	}
}

// S5 C5 on FUL's real shipment document set (FUL-INV-004 原产地证引用; R56 CertificateResultRef v1;
// CNX-INV-007): the documentation clerk's certificate of origin record that names a certificate CNX
// lacks is REFERENCE_INVALID, one for another market CERTIFICATE_NOT_APPLICABLE, one with another
// number INVALID_KNOWN_VALUE — refused at the prepare, not left as a difference to acknowledge at
// issue — and after the certificate's correction its first revision is REFERENCE_STALE; the record
// naming the current revision is prepared as a draft and binds CNX's answer among its sources. A
// refused prepare creates no set.
func TestCertificateOfOriginRecordOnAShipmentDocumentSet(t *testing.T) {
	w := newCertWorld(t)
	w.shipment("shipment.D", "50", "commitment.1")
	now := time.Now().UTC()
	from, until := now.Add(-time.Hour), now.Add(30*24*time.Hour)
	good := w.register(w.certificate("SYN-CO-D1", "DE", cnxapi.CertificateObject{Kind: cnxapi.ObjectShipment, ID: "shipment.D"}, from, until))
	us := w.register(w.certificate("SYN-CO-D2", "US", specs("sku.1", w.spec), from, until))
	co := func(number, id string, version int64) fulapi.PrepareShipmentDocument {
		return fulapi.PrepareShipmentDocument{IdempotencyKey: w.key(), DocumentSetID: "docs.D", ShipmentID: "shipment.D",
			DocumentKind: fulapi.DocumentCertificateOfOrigin, Certificate: &fulapi.CertificateOfOrigin{Number: number, FileID: w.coFile,
				Issuer: "合成贸促会（测试）", IssuedOn: from.Format(time.DateOnly), CertificateRef: &fulapi.CertificateRef{CertificateID: id, Version: version}}}
	}
	fixed := w.certificate("SYN-CO-D1", "DE", cnxapi.CertificateObject{Kind: cnxapi.ObjectShipment, ID: "shipment.D"}, from, until.Add(24*time.Hour))
	fixed.CertificateID, fixed.ExpectedVersion, fixed.Reason = good.CertificateID, 1, "签发机构延长有效期（合成）"
	corrected := w.register(fixed)
	for _, x := range []struct {
		cmd        fulapi.PrepareShipmentDocument
		code, path string
	}{
		{co("SYN-CO-X", "crt-none", 1), kernel.CodeReferenceInvalid, "/args/certificate/certificate_ref"},
		{co("SYN-CO-D2", us.CertificateID, 1), fulapi.CodeCertificateNotApplicable, "/args/certificate/certificate_ref"},
		{co("SYN-CO-D1", good.CertificateID, 1), fulapi.CodeReferenceStale, "/args/certificate/certificate_ref"},
		{co("SYN-CO-OTHER", good.CertificateID, 2), kernel.CodeInvalidKnownValue, "/args/certificate/number"},
	} {
		_, err := w.ful.PrepareShipmentDocument(w.doc, x.cmd)
		if code(err) != x.code || path(err) != x.path {
			t.Errorf("%s: want %s %s, got %s %s", x.cmd.Certificate.Number, x.code, x.path, code(err), path(err))
		}
	}
	if _, err := w.ful.GetDocumentSet(w.doc, "docs.D"); code(err) != kernel.CodeNotFound {
		t.Fatalf("a refused prepare created the set: %v", err)
	}
	r, err := w.ful.PrepareShipmentDocument(w.doc, co("SYN-CO-D1", good.CertificateID, 2))
	if err != nil || r.Status != "draft" || r.Number != "SYN-CO-D1" || r.Version != 1 {
		t.Fatalf("prepared %+v %v", r, err)
	}
	v, err := w.ful.GetDocumentSet(w.doc, "docs.D")
	w.must(err)
	var bound string
	for _, s := range v.Documents[0].Sources {
		if s.Name == cnxapi.CertificateResultRefName {
			bound = s.Value
		}
	}
	if len(v.Documents) != 1 || v.Documents[0].DocumentKind != fulapi.DocumentCertificateOfOrigin ||
		bound != good.CertificateID+"/v2/"+corrected.ContentSHA256+"/applicable/DE/"+kernel.FormatTimestamp(until.Add(24*time.Hour)) {
		t.Fatalf("set %+v (bound %q)", v.Documents, bound)
	}
}

// The composed process over HTTP with OIDC access tokens (S5 C5): the documentation clerk registers a
// certificate of origin of a real shipment through CNX's command route and corrects it; an AI agent is
// refused (403); a schema refusal names its member; the reads answer the certificate with both
// revisions, the list and CertificateResultRef v1's verdict for the shipment; reconcile reports a
// command never sent.
func TestCertificatesOverHTTP(t *testing.T) {
	w := newCertWorld(t)
	w.shipment("shipment.H", "40", "commitment.1")
	w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("ai", "agent.http"), DataScope: "company",
		Permissions: []string{cnxapi.CommandRegisterCertificate, "cnx.read", "evd.read", "ful.read"}})
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	doc := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "doc.1", "human"), n: 800}
	ai := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "agent.http", "ai"), n: 900}
	now := time.Now().UTC()
	args := map[string]any{"expected_version": 0, "certificate_kind": "certificate_of_origin", "number": "SYN-CO-H", "issuer": "合成贸促会（测试）",
		"issued_on": now.Format(time.DateOnly), "object": map[string]any{"kind": "shipment", "id": "shipment.H"}, "market": "DE",
		"valid_from": kernel.FormatTimestamp(now.Add(-time.Hour)), "valid_until": kernel.FormatTimestamp(now.Add(30 * 24 * time.Hour)),
		"file_id": w.coFile, "basis": "原件核验（合成）", "evidence_ids": []any{w.page}}
	if status, out := ai.try("cnx", cnxapi.Contract, cnxapi.CommandRegisterCertificate, args); status != 403 || out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("AI register %d %v", status, out)
	}
	bad := map[string]any{}
	for k, v := range args {
		bad[k] = v
	}
	bad["market"] = "XX"
	if status, out := doc.try("cnx", cnxapi.Contract, cnxapi.CommandRegisterCertificate, bad); status != 422 || out["code"] != kernel.CodeSchemaInvalid ||
		errPath(out) != "/args/market" {
		t.Fatalf("schema %d %v", status, out)
	}
	registered := doc.command("cnx", cnxapi.Contract, cnxapi.CommandRegisterCertificate, args)
	id, _ := registered["object_id"].(string)
	if registered["status"] != "registered" || registered["row_version"] != float64(1) || id == "" {
		t.Fatalf("register %v", registered)
	}
	correction := map[string]any{}
	for k, v := range args {
		correction[k] = v
	}
	correction["certificate_id"], correction["expected_version"], correction["reason"] = id, 1, "更正签发日期（合成）"
	correction["issued_on"] = now.Add(-24 * time.Hour).Format(time.DateOnly)
	if c := doc.command("cnx", cnxapi.Contract, cnxapi.CommandRegisterCertificate, correction); c["row_version"] != float64(2) {
		t.Fatalf("correction %v", c)
	}
	status, view := doc.do("GET", "/api/brant/v1/cnx/certificates/"+id, nil)
	revisions, _ := view["revisions"].([]any)
	if status != 200 || view["current_revision"] != float64(2) || view["state"] != "valid" || len(revisions) != 2 ||
		revisions[1].(map[string]any)["supersedes"] != float64(1) {
		t.Fatalf("certificate %d %v", status, view)
	}
	status, list := doc.do("GET", "/api/brant/v1/cnx/certificates", nil)
	if status != 200 || len(list["certificates"].([]any)) != 1 {
		t.Fatalf("list %d %v", status, list)
	}
	status, verdict := doc.do("GET", "/api/brant/v1/cnx/certificates/"+id+"/applicability?target_kind=shipment&target_id=shipment.H", nil)
	if status != 200 || verdict["applicable"] != true || verdict["destination"] != "DE" || verdict["revision"] != float64(2) {
		t.Fatalf("applicability %d %v", status, verdict)
	}
	status, rec := doc.do("POST", "/api/brant/v1/cnx/commands/"+cnxapi.CommandRegisterCertificate+"/reconcile", map[string]any{
		"contract": cnxapi.Contract, "command": cnxapi.CommandRegisterCertificate, "idempotency_key": "never.sent", "args": args})
	if status != 200 || rec["state"] != "not_committed" {
		t.Fatalf("reconcile %d %v", status, rec)
	}
}

// Certificate results are append-only and confined to their company (tactical "只追加；更正形成新版本";
// FORCE RLS), and concurrent registrations are decided by the database: the application role cannot
// change or delete a revision or a certificate; even the owner cannot rewrite a revision, renumber one,
// move a root without its revision or change its kind; company.B sees nothing. Two corrections of one
// certificate at once: one is revision 2, the other VERSION_CONFLICT; two registrations of one issuer's
// number at once: one is registered, the other BUSINESS_KEY_CONFLICT.
func TestCertificateRecordsAreAppendOnlyAndConcurrencySafe(t *testing.T) {
	w := newCertWorld(t)
	now := time.Now().UTC()
	from, until := now.Add(-time.Hour), now.Add(30*24*time.Hour)
	first := w.register(w.certificate("SYN-CO-R1", "DE", specs("sku.1", w.spec), from, until))
	for round := 0; round < 3; round++ {
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range errs {
			c := w.certificate("SYN-CO-R1", "DE", specs("sku.1", w.spec), from, until.Add(time.Duration(round*2+i+1)*time.Hour))
			c.CertificateID, c.ExpectedVersion, c.Reason = first.CertificateID, int64(round+1), "并发更正"
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = w.cnx.RegisterCertificate(w.doc, c)
			}()
		}
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) || code(errs[0])+code(errs[1]) != "OK"+kernel.CodeVersionConflict &&
			code(errs[0])+code(errs[1]) != kernel.CodeVersionConflict+"OK" {
			t.Fatalf("round %d: %v %v", round, errs[0], errs[1])
		}
		var wg2 sync.WaitGroup
		for i := range errs {
			c := w.certificate("SYN-CO-RACE-"+string(rune('A'+round)), "DE", specs("sku.1", w.spec), from, until)
			wg2.Add(1)
			go func() {
				defer wg2.Done()
				_, errs[i] = w.cnx.RegisterCertificate(w.doc, c)
			}()
		}
		wg2.Wait()
		if got := code(errs[0]) + " " + code(errs[1]); got != "OK "+cnxapi.CodeBusinessKeyConflict && got != cnxapi.CodeBusinessKeyConflict+" OK" {
			t.Fatalf("round %d registrations: %s", round, got)
		}
	}
	v, err := w.cnx.GetCertificate(w.doc, first.CertificateID)
	w.must(err)
	if len(v.Revisions) != 4 || v.RowVersion != 4 {
		t.Fatalf("revisions %+v", v)
	}
	for _, sql := range []string{`UPDATE brant_cnx.certificate_revision SET market = 'FR'`, `DELETE FROM brant_cnx.certificate_revision`,
		`DELETE FROM brant_cnx.certificate_result`, `UPDATE brant_cnx.certificate_result SET kind = 'license_copy'`} {
		if err := w.exec("company.A", sql); err == nil {
			t.Errorf("brant_app must not run %s", sql)
		}
	}
	owner := func(sql string) error {
		tx, err := pgtest.MigratorPool(w.t).Begin(context.Background())
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true), set_config('app.company_id', 'company.A', true)`,
			w.tenant); err != nil {
			return err
		}
		if _, err = tx.Exec(context.Background(), sql); err != nil {
			return err
		}
		return tx.Commit(context.Background())
	}
	for _, x := range []struct{ sql, want string }{
		{`UPDATE brant_cnx.certificate_revision SET market = 'FR'`, "never change"},
		{`DELETE FROM brant_cnx.certificate_revision`, "never change"},
		{`DELETE FROM brant_cnx.certificate_result`, "never deleted"},
		{`UPDATE brant_cnx.certificate_result SET kind = 'license_copy', current_revision = current_revision + 1, row_version = row_version + 1`,
			"keeps its identity and kind"},
		{`UPDATE brant_cnx.certificate_result SET current_revision = current_revision + 1, row_version = row_version + 1
			WHERE certificate_id = '` + first.CertificateID + `'`, "was never registered"},
		{`INSERT INTO brant_cnx.certificate_revision SELECT tenant_id, company_id, certificate_id, revision + 10, kind, issuer, number, issued_on,
			object_kind, object_id, market, valid_from, valid_until, file_id, file_sha256, license_id, document, document_sha256, 'forged',
			verified_by_kind, verified_by_id, authority_epoch, registered_at FROM brant_cnx.certificate_revision
			WHERE certificate_id = '` + first.CertificateID + `' AND revision = 4`, "is the next one"},
	} {
		if err := owner(x.sql); err == nil || !strings.Contains(err.Error(), x.want) {
			t.Errorf("must refuse %s: %v", x.sql, err)
		}
	}
	for _, table := range []string{"brant_cnx.certificate_result", "brant_cnx.certificate_revision"} {
		if n := w.count("company.B", table); n != 0 {
			t.Errorf("company.B sees %d rows of %s", n, table)
		}
		if n := w.count("company.A", table); n == 0 {
			t.Errorf("company.A sees no %s", table)
		}
	}
}
