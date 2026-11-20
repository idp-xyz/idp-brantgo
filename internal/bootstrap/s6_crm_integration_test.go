//go:build integration

package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	crmapi "go.idp.xyz/idp-brant/internal/contexts/crm/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CRM on (S6-D04, R40): lead, opportunity, follow-up, then an RFQ that cites the opportunity.
// An unknown or inactive opportunity is REFERENCE_INVALID. An AI token is refused at the
// transport. Another company and another tenant do not see the lead (RLS).
func TestS6CRMOnBridge(t *testing.T) {
	w := newWorld(t)
	if w.c.CRM == nil {
		t.Fatal("CRM was not constructed")
	}
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTP(t, handler)
	sales := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "sales.1"},
		Permissions: []string{crmapi.CommandCaptureLead, crmapi.CommandAdvanceOpportunity, crmapi.CommandRecordFollowUp,
			"crm.read", inqapi.CommandSaveRfq, "inq.read"},
		DataScope: "company", Team: "trade"})
	cli := &client{t: t, base: srv, token: is.token(w.tenant, "company.A", "sales.1", "human")}

	lead := postOK(t, cli, "crm", crmapi.Contract, crmapi.CommandCaptureLead, map[string]any{
		"lead_id": "lead.1", "expected_version": 0, "source_channel": "fair", "company_name": "Northwind",
		"contact_name": "Ada", "summary": "asked about shirts"})
	if lead["status"] != crmapi.StatusCaptured || lead["row_version"] != float64(1) {
		t.Fatalf("lead %v", lead)
	}
	created := postOK(t, cli, "crm", crmapi.Contract, crmapi.CommandAdvanceOpportunity, map[string]any{
		"opportunity_id": "opp.1", "expected_version": 0, "title": "Spring shirts", "lead_id": "lead.1",
		"customer_id": "customer.1", "currency": "USD", "expected_amount_minor": 96000})
	if created["status"] != crmapi.StageQualified || created["row_version"] != float64(1) {
		t.Fatalf("create %v", created)
	}
	advanced := postOK(t, cli, "crm", crmapi.Contract, crmapi.CommandAdvanceOpportunity, map[string]any{
		"opportunity_id": "opp.1", "expected_version": 1, "stage": "proposal"})
	if advanced["status"] != crmapi.StageProposal || advanced["row_version"] != float64(2) {
		t.Fatalf("advance %v", advanced)
	}
	followed := postOK(t, cli, "crm", crmapi.Contract, crmapi.CommandRecordFollowUp, map[string]any{
		"opportunity_id": "opp.1", "expected_version": 2, "follow_up_id": "fu.1", "summary": "sent the line sheet",
		"next_step": "call"})
	if followed["status"] != crmapi.StageProposal || followed["row_version"] != float64(3) {
		t.Fatalf("follow-up %v", followed)
	}
	view, err := w.c.CRM.GetOpportunity(sales, "opp.1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Stage != crmapi.StageProposal || view.LeadID != "lead.1" || view.CustomerID != "customer.1" ||
		view.Currency != "USD" || view.ExpectedAmountMinor != 96000 || len(view.FollowUps) != 1 || view.FollowUps[0].FollowUpID != "fu.1" {
		t.Fatalf("opportunity view %+v", view)
	}
	if _, err := w.c.CRM.GetLead(sales, "lead.1"); err != nil {
		t.Fatal(err)
	}

	// Create and the stage advance each emit OpportunityAdvanced. The follow-up does not: it is
	// not a transition and not a BusinessTask (SM-CRM-OPPORTUNITY).
	ev := w.events("company.A", crmapi.Source)
	if len(ev) != 2 || ev[0].Type != crmapi.EventOpportunityAdvanced || ev[1].Type != crmapi.EventOpportunityAdvanced {
		t.Fatalf("events %+v", ev)
	}
	if ev[0].Payload["opportunity_id"] != "opp.1" || ev[0].Payload["stage"] != crmapi.StageQualified || ev[0].Payload["previous_stage"] != "" {
		t.Fatalf("create event %+v", ev[0].Payload)
	}
	if ev[1].Payload["stage"] != crmapi.StageProposal || ev[1].Payload["previous_stage"] != crmapi.StageQualified {
		t.Fatalf("advance event %+v", ev[1].Payload)
	}

	postOK(t, cli, "inq", inqapi.Contract, inqapi.CommandSaveRfq, map[string]any{
		"id": "rfq.1", "expected_version": 0, "opportunity_id": "opp.1"})
	status, rfq := cli.do(http.MethodGet, "/api/brant/v1/inq/rfqs/rfq.1", nil)
	if status != http.StatusOK || rfq["opportunity_id"] != "opp.1" {
		t.Fatalf("rfq get %d %v", status, rfq)
	}
	// Omitting opportunity_id is every save that does not cite CRM. CRM being on does not change it.
	postOK(t, cli, "inq", inqapi.Contract, inqapi.CommandSaveRfq, map[string]any{
		"id": "rfq.plain", "expected_version": 0})
	status, plain := cli.do(http.MethodGet, "/api/brant/v1/inq/rfqs/rfq.plain", nil)
	if status != http.StatusOK || plain["opportunity_id"] != nil {
		t.Fatalf("plain rfq %d %v", status, plain)
	}
	// A stored customer that differs from the RFQ's customer is not a source for that RFQ (R40).
	// The check is before INQ's transaction, so the customer does not have to exist in PTY.
	postCode(t, cli, "inq", inqapi.Contract, inqapi.CommandSaveRfq, map[string]any{
		"id": "rfq.other", "expected_version": 0, "opportunity_id": "opp.1", "customer_id": "customer.other"},
		http.StatusUnprocessableEntity, kernel.CodeReferenceInvalid, "/args/opportunity_id")
	postCode(t, cli, "inq", inqapi.Contract, inqapi.CommandSaveRfq, map[string]any{
		"id": "rfq.missing", "expected_version": 0, "opportunity_id": "opp.missing"},
		http.StatusUnprocessableEntity, kernel.CodeReferenceInvalid, "/args/opportunity_id")

	postOK(t, cli, "crm", crmapi.Contract, crmapi.CommandAdvanceOpportunity, map[string]any{
		"opportunity_id": "opp.1", "expected_version": 3, "stage": "abandoned"})
	ev = w.events("company.A", crmapi.Source)
	if len(ev) != 3 || ev[2].Payload["stage"] != crmapi.StageAbandoned || ev[2].Payload["previous_stage"] != crmapi.StageProposal {
		t.Fatalf("abandon event %+v", ev)
	}
	postCode(t, cli, "inq", inqapi.Contract, inqapi.CommandSaveRfq, map[string]any{
		"id": "rfq.dead", "expected_version": 0, "opportunity_id": "opp.1"},
		http.StatusUnprocessableEntity, kernel.CodeReferenceInvalid, "/args/opportunity_id")

	ai := &client{t: t, base: srv, token: is.token(w.tenant, "company.A", "agent.q01", "ai")}
	postCode(t, ai, "crm", crmapi.Contract, crmapi.CommandCaptureLead, map[string]any{"lead_id": "lead.ai"},
		http.StatusForbidden, kernel.CodePermissionDenied, "")

	ctxB := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "sales.b"},
		Permissions: []string{"crm.read"}, DataScope: "company"})
	if _, err := w.c.CRM.GetLead(ctxB, "lead.1"); rejectionCode(err) != kernel.CodeNotFound {
		t.Fatalf("company B sees the lead: %v", err)
	}
	if _, err := w.c.CRM.GetOpportunity(ctxB, "opp.1"); rejectionCode(err) != kernel.CodeNotFound {
		t.Fatalf("company B sees the opportunity: %v", err)
	}
	other := kernel.Scope{Tenant: kernel.TenantID(pgtest.UniqueID(t, "tenant.")), Company: kernel.CompanyID("company.A")}
	ctxT := w.provision(bootstrap.Grant{Scope: other, Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "sales.t"},
		Permissions: []string{"crm.read"}, DataScope: "company"})
	if _, err := w.c.CRM.GetLead(ctxT, "lead.1"); rejectionCode(err) != kernel.CodeNotFound {
		t.Fatalf("other tenant sees the lead: %v", err)
	}
	if _, err := w.c.CRM.GetOpportunity(ctxT, "opp.1"); rejectionCode(err) != kernel.CodeNotFound {
		t.Fatalf("other tenant sees the opportunity: %v", err)
	}
}

// BC-AT24: CRM off. Startup succeeds, CRM routes answer 400/403/404, an RFQ that cites an
// opportunity is DEPENDENCY_NOT_SUPPORTED, and the trade chain through settlement still runs.
func TestBCAT24CRMOff(t *testing.T) {
	c := newBRC001S5With(t, bootstrap.PlatformConfig{DisableCRM: true})
	if c.w.c.CRM != nil {
		t.Fatal("CRM was constructed")
	}
	c.refused(brcSales, "crm", crmapi.Contract, crmapi.CommandCaptureLead, map[string]any{
		"lead_id": "lead.1", "expected_version": 0, "source_channel": "fair", "company_name": "Northwind",
		"contact_name": "Ada", "summary": "asked about shirts"}, http.StatusBadRequest, kernel.CodeUnsupportedContract)
	ai := &client{t: t, base: c.base, token: c.is.token(c.w.tenant, "company.A", "agent.q01", "ai")}
	postCode(t, ai, "crm", crmapi.Contract, crmapi.CommandCaptureLead, map[string]any{"lead_id": "lead.ai"},
		http.StatusForbidden, kernel.CodePermissionDenied, "")
	c.read(brcSales, "/api/brant/v1/crm/leads/lead.1", http.StatusNotFound)
	c.read(brcSales, "/api/brant/v1/crm/audit", http.StatusNotFound)
	status, out := c.post(brcSales, "inq", inqapi.Contract, inqapi.CommandSaveRfq, map[string]any{
		"id": "rfq.crm", "expected_version": 0, "opportunity_id": "opp.1"})
	if status != http.StatusUnprocessableEntity || out["code"] != inqapi.CodeDependencyNotSupported {
		t.Fatalf("opportunity with CRM off: %d %v", status, out)
	}
	errs, _ := out["field_errors"].([]any)
	first, _ := errs[0].(map[string]any)
	if first["path"] != "/args/opportunity_id" {
		t.Fatalf("path %v", out["field_errors"])
	}
	c.run()
}

func newHTTP(t *testing.T, handler http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func postOK(t *testing.T, c *client, bc, contract, command string, args map[string]any) map[string]any {
	t.Helper()
	status, out := postCmd(t, c, bc, contract, command, args)
	if status != http.StatusOK || out["ok"] != true {
		t.Fatalf("%s %s: %d %v", bc, command, status, out)
	}
	return out
}

func postCode(t *testing.T, c *client, bc, contract, command string, args map[string]any, wantStatus int, wantCode, wantPath string) {
	t.Helper()
	status, out := postCmd(t, c, bc, contract, command, args)
	if status != wantStatus || out["code"] != wantCode {
		t.Fatalf("%s %s: want %d %s, got %d %v", bc, command, wantStatus, wantCode, status, out)
	}
	if wantPath == "" {
		return
	}
	errs, _ := out["field_errors"].([]any)
	first, _ := errs[0].(map[string]any)
	if first["path"] != wantPath {
		t.Fatalf("path %v, want %s", out["field_errors"], wantPath)
	}
}

func postCmd(t *testing.T, c *client, bc, contract, command string, args map[string]any) (int, map[string]any) {
	t.Helper()
	c.n++
	key := "crm." + command + "." + crmKey(c.n)
	return c.do(http.MethodPost, "/api/brant/v1/"+bc+"/commands/"+command, map[string]any{
		"contract": contract, "command": command, "idempotency_key": key, "args": args})
}

func crmKey(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func rejectionCode(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code
	}
	if err == nil {
		return ""
	}
	return err.Error()
}
