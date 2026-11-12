package httpapi

import (
	"net/http"

	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CNX is what the transport needs from the China export context.
type CNX interface {
	cnxapi.Commands
	cnxapi.Queries
	cnxapi.FollowQueries
	cnxapi.LicenseCommands
	cnxapi.LicenseQueries
	cnxapi.InvoiceCommands
	cnxapi.InvoiceQueries
	cnxapi.TaxCaseCommands
	cnxapi.TaxCaseQueries
	cnxapi.DeclarationCommands
	cnxapi.DeclarationQueries
	cnxapi.BundleCommands
	cnxapi.BundleQueries
	cnxapi.CertificateCommands
	cnxapi.CertificateQueries
	cnxapi.Reconciler
	cnxapi.Auditor
}

func cnxCommands(c CNX) *commandSet {
	// CNX's commands name their objects (check_id, order_id, license_id, allocation_id, release_id),
	// so no member is renamed to id; the members follow the reflective mapping (S3-D149).
	return &commandSet{bc: "cnx", contract: cnxapi.Contract, auditor: c, audit: c.Audit, reconcile: reconciler(c.Reconcile),
		commands: map[string]commandFunc{
			cnxapi.CommandEvaluate:        reflected("", "", receiptOf[cnxapi.Receipt](c.Evaluate)),
			cnxapi.CommandDecide:          reflected("", "", receiptOf[cnxapi.Receipt](c.Decide)),
			cnxapi.CommandReopen:          reflected("", "", receiptOf[cnxapi.Receipt](c.Reopen)),
			cnxapi.CommandRegisterLicense: reflected("", "", receiptOf[cnxapi.Receipt](c.RegisterLicense)),
			cnxapi.CommandAllocateLicense: reflected("", "", receiptOf[cnxapi.Receipt](c.AllocateLicense)),
			cnxapi.CommandReleaseLicense:  reflected("", "", receiptOf[cnxapi.Receipt](c.ReleaseLicense)),
			// Purchase invoices and tax refund cases name purchase_invoice_id, case_id, export_lot_id.
			cnxapi.CommandImportInvoice:       reflected("", "", receiptOf[cnxapi.Receipt](c.ImportInvoice)),
			cnxapi.CommandVerifyInvoice:       reflected("", "", receiptOf[cnxapi.Receipt](c.VerifyInvoice)),
			cnxapi.CommandAllocateTaxEvidence: reflected("", "", receiptOf[cnxapi.Receipt](c.AllocateTaxEvidence)),
			cnxapi.CommandRecordTaxResult:     reflected("", "", receiptOf[cnxapi.Receipt](c.RecordTaxResult)),
			// Declarations and document bundles name declaration_id, export_lot_id, bundle_id.
			cnxapi.CommandPrepareDeclaration:    reflected("", "", receiptOf[cnxapi.Receipt](c.PrepareDeclaration)),
			cnxapi.CommandConfirmDeclaration:    reflected("", "", receiptOf[cnxapi.Receipt](c.ConfirmDeclaration)),
			cnxapi.CommandRecordSubmission:      reflected("", "", receiptOf[cnxapi.Receipt](c.RecordSubmission)),
			cnxapi.CommandRecordReceipt:         reflected("", "", receiptOf[cnxapi.Receipt](c.RecordReceipt)),
			cnxapi.CommandCheckBundle:           reflected("", "", receiptOf[cnxapi.Receipt](c.CheckBundle)),
			cnxapi.CommandResolveReconciliation: reflected("", "", receiptOf[cnxapi.Receipt](c.ResolveReconciliation)),
			// Certificate results name certificate_id (S5 C5).
			cnxapi.CommandRegisterCertificate: reflected("", "", receiptOf[cnxapi.Receipt](c.RegisterCertificate)),
		}}
}

// cnxReads are CNX's read routes in the OpenAPI document.
var cnxReads = []readRoute{
	{"/api/brant/v1/cnx/trade_compliance_checks/{id}", "cnx.trade_compliance.get", "cnx",
		"A trade compliance check of an order revision: its state now as TradeComplianceGuard v1 decides it, every evaluation with its facts, findings and gaps, every decision and reopening", []string{"id"}, false},
	{"/api/brant/v1/cnx/orders/{id}/trade_compliance", "cnx.trade_compliance.order_checks", "cnx",
		"The trade compliance checks of a sales order's revisions, newest first, each with its state now", []string{"id"}, false},
	{"/api/brant/v1/cnx/regulations/{id}/trade_compliance", "cnx.trade_compliance.by_regulation", "cnx",
		"The clear trade compliance conclusions that follow a screening_regulation, at ?revision= only (a positive integer) when given, each with the programme list versions it used and its state now", []string{"id"}, false},
	{"/api/brant/v1/cnx/programme_lists/{id}/trade_compliance", "cnx.trade_compliance.by_list", "cnx",
		"The clear trade compliance conclusions whose evaluation used a country programme list, at ?version= only when given, each with its state now (pending, list_updated when the list has a newer version)", []string{"id"}, false},
	{"/api/brant/v1/cnx/export_licenses", "cnx.export_license.list", "cnx",
		"The company's export licences, newest first, each with its state now and its usage (licensed, allocated, available quantity and value)", nil, false},
	{"/api/brant/v1/cnx/export_licenses/{id}", "cnx.export_license.get", "cnx",
		"An export licence: its terms, its state now, its usage derived from its allocations and every allocation with its releases and the external results they rest on", []string{"id"}, false},
	{"/api/brant/v1/cnx/shipments/{id}/export_license", "cnx.export_license.shipment", "cnx",
		"What ExportLicenseGuard v1 decides now for a shipment, read without locks: covered, status, reason, the allocation and what remains outstanding", []string{"id"}, false},
	{"/api/brant/v1/cnx/purchase_invoices", "cnx.purchase_invoice.list", "cnx",
		"The company's purchase invoices, newest first, each with its state, kind, colour, number and totals", nil, false},
	{"/api/brant/v1/cnx/purchase_invoices/{id}", "cnx.purchase_invoice.get", "cnx",
		"A purchase invoice: its original, the candidate and who produced it, the verified facts and who verified them, their differences with both sources, its matches to purchase commitment lines with their state, and per line and purpose the tax evidence allocated and available", []string{"id"}, false},
	{"/api/brant/v1/cnx/tax_refund_cases", "cnx.tax_refund_case.list", "cnx",
		"The company's tax refund cases, newest first, each with its export lot, state and the export tax policy revision it binds", nil, false},
	{"/api/brant/v1/cnx/tax_refund_cases/{id}", "cnx.tax_refund_case.get", "cnx",
		"A tax refund case: the export lot, the export tax policy revision in force at its export date, every evidence allocation with its state, every recorded stage and the expected, planned, approved and received amounts side by side", []string{"id"}, false},
	{"/api/brant/v1/cnx/declaration_cases", "cnx.declaration_case.list", "cnx",
		"The company's customs declaration cases, newest first, each with its export lot, attempt, state, current version and mapping, declaration number and export date", nil, false},
	{"/api/brant/v1/cnx/declaration_cases/{id}", "cnx.declaration_case.get", "cnx",
		"A customs declaration case of an export lot: every version of the declared items with their profiles, conversions, legal quantities and mapping, every confirmation, manual submission and external result with its original text, mapping version and external sequence; customs release is not departure or delivery", []string{"id"}, false},
	{"/api/brant/v1/cnx/document_bundles", "cnx.document_bundle.list", "cnx",
		"The company's document bundles, newest first, each with its export lot and state", nil, false},
	{"/api/brant/v1/cnx/document_bundles/{id}", "cnx.document_bundle.get", "cnx",
		"A document bundle of an export lot: the current requirement checklist (kind, version, missing, present or verified, owner, due date, attachments, linked declaration and fund records as commercial allocations, certificate results with the lot lines they cover), every check and every external reconciliation", []string{"id"}, false},
	// Certificate results (S5 C5).
	{"/api/brant/v1/cnx/certificates", "cnx.certificate.list", "cnx",
		"The company's official certificate results, newest first, each as its current revision states it (kind, number, issuer, market, object, validity) with its state now", nil, false},
	{"/api/brant/v1/cnx/certificates/{id}", "cnx.certificate.get", "cnx",
		"An official certificate result: every revision as registered (issuer, number, object with the lines CNX read of it, market, validity, file, verification basis and evidence, verifier, correction reason), its current revision and its state now", []string{"id"}, false},
	{"/api/brant/v1/cnx/certificates/{id}/applicability", "cnx.certificate.applicability", "cnx",
		"What CertificateResultRef v1 answers now for the certificate's current revision on the shipment or export lot ?target_kind=shipment|export_lot&target_id=: applicable or the code and reason (kind, market_unknown, market, not_yet_valid, expired, goods), the destination and the lines it covers", []string{"id"}, false},
}

// cnxRoutes registers CNX's reads.
func (s *server) cnxRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/brant/v1/cnx/trade_compliance_checks/{id}", s.authenticated(read(s, s.cfg.CNX.GetCheck, okView[cnxapi.CheckView])))
	mux.Handle("GET /api/brant/v1/cnx/orders/{id}/trade_compliance", s.authenticated(s.orderComplianceChecks))
	mux.Handle("GET /api/brant/v1/cnx/regulations/{id}/trade_compliance", s.authenticated(s.regulationComplianceChecks))
	mux.Handle("GET /api/brant/v1/cnx/programme_lists/{id}/trade_compliance", s.authenticated(s.listComplianceChecks))
	mux.Handle("GET /api/brant/v1/cnx/export_licenses", s.authenticated(s.exportLicenses))
	mux.Handle("GET /api/brant/v1/cnx/export_licenses/{id}", s.authenticated(read(s, s.cfg.CNX.GetLicense, okView[cnxapi.LicenseView])))
	mux.Handle("GET /api/brant/v1/cnx/shipments/{id}/export_license",
		s.authenticated(read(s, s.cfg.CNX.ShipmentLicense, okView[cnxapi.ShipmentLicenseView])))
	mux.Handle("GET /api/brant/v1/cnx/purchase_invoices", s.authenticated(s.purchaseInvoices))
	mux.Handle("GET /api/brant/v1/cnx/purchase_invoices/{id}", s.authenticated(read(s, s.cfg.CNX.GetInvoice, okView[cnxapi.InvoiceView])))
	mux.Handle("GET /api/brant/v1/cnx/tax_refund_cases", s.authenticated(s.taxRefundCases))
	mux.Handle("GET /api/brant/v1/cnx/tax_refund_cases/{id}", s.authenticated(read(s, s.cfg.CNX.GetTaxCase, okView[cnxapi.TaxCaseView])))
	s.cnxDeclarationRoutes(mux)
	s.cnxCertificateRoutes(mux)
}

// purchaseInvoices answers GET /api/brant/v1/cnx/purchase_invoices.
func (s *server) purchaseInvoices(w http.ResponseWriter, r *http.Request) {
	v, err := s.cfg.CNX.Invoices(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if v == nil {
		v = []cnxapi.InvoiceSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "purchase_invoices": wire(v)})
}

// taxRefundCases answers GET /api/brant/v1/cnx/tax_refund_cases.
func (s *server) taxRefundCases(w http.ResponseWriter, r *http.Request) {
	v, err := s.cfg.CNX.TaxCases(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if v == nil {
		v = []cnxapi.TaxCaseSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tax_refund_cases": wire(v)})
}

// orderComplianceChecks answers GET /api/brant/v1/cnx/orders/<id>/trade_compliance.
func (s *server) orderComplianceChecks(w http.ResponseWriter, r *http.Request) {
	v, err := s.cfg.CNX.OrderChecks(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if v == nil {
		v = []cnxapi.CheckSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "order_id": r.PathValue("id"), "checks": wire(v)})
}

// regulationComplianceChecks answers GET /api/brant/v1/cnx/regulations/<id>/trade_compliance[?revision=N].
func (s *server) regulationComplianceChecks(w http.ResponseWriter, r *http.Request) {
	q, err := queryInts(r, "revision")
	var v []cnxapi.ClearCheck
	if err == nil {
		v, err = s.cfg.CNX.ChecksByRegulation(r.Context(), r.PathValue("id"), q["revision"])
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "regulation_id": r.PathValue("id"), "checks": wire(clearChecks(v))})
}

// listComplianceChecks answers GET /api/brant/v1/cnx/programme_lists/<code>/trade_compliance[?version=v].
func (s *server) listComplianceChecks(w http.ResponseWriter, r *http.Request) {
	var v []cnxapi.ClearCheck
	var err error
	q := r.URL.Query()
	for k := range q {
		if k != "version" || len(q[k]) != 1 {
			err = kernel.Reject(kernel.CodeSchemaInvalid, "/"+k, "unknown or repeated query parameter")
		}
	}
	if err == nil {
		v, err = s.cfg.CNX.ChecksByList(r.Context(), r.PathValue("id"), q.Get("version"))
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "list_code": r.PathValue("id"), "checks": wire(clearChecks(v))})
}

func clearChecks(v []cnxapi.ClearCheck) []cnxapi.ClearCheck {
	if v == nil {
		return []cnxapi.ClearCheck{}
	}
	return v
}

// exportLicenses answers GET /api/brant/v1/cnx/export_licenses.
func (s *server) exportLicenses(w http.ResponseWriter, r *http.Request) {
	v, err := s.cfg.CNX.Licenses(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if v == nil {
		v = []cnxapi.LicenseSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "licenses": wire(v)})
}
