package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	pin "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/purchaseinvoice"
	tr "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/taxrefund"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Purchase invoices and tax refund cases are company-wide (no data scope, like licences): every
// holder of cnx.read in the company reads them.

func factsView(f pin.Facts) api.FactsView {
	out := api.FactsView{InvoiceFacts: api.InvoiceFacts{Kind: f.Kind, Colour: f.Colour, Number: f.Number, IssuedOn: f.IssuedOn,
		Supplier: api.InvoiceParty{PartyID: f.Supplier.PartyID, Name: f.Supplier.Name, TaxID: f.Supplier.TaxID},
		Buyer:    api.InvoiceParty{Name: f.Buyer.Name, TaxID: f.Buyer.TaxID}, Currency: api.CurrencyRef{ID: f.Currency.ConfigID, Revision: f.Currency.Revision},
		Lines: []api.InvoiceLine{}, NetTotal: f.NetTotal, TaxTotal: f.TaxTotal, GrossTotal: f.GrossTotal},
		CurrencyCode: f.Currency.Code, MinorUnits: f.Currency.MinorUnits}
	for _, l := range f.Lines {
		out.Lines = append(out.Lines, api.InvoiceLine{LineNo: l.LineNo, Description: l.Description, Quantity: l.Quantity, Unit: l.Unit,
			NetAmount: l.NetAmount, TaxRate: l.TaxRate, TaxAmount: l.TaxAmount})
	}
	if r := f.RedOf; r != nil {
		out.RedOf = &api.InvoiceRef{Kind: r.Kind, Number: r.Number}
	}
	return out
}

// GetInvoice returns a purchase invoice (purchase_invoice.get, cnx.read): the candidate and the
// verified facts with their sources and differences (CN-AT18), the matches and their state, and per
// verified line and purpose the tax evidence allocated and available.
func (s Service) GetInvoice(ctx context.Context, invoiceID string) (api.InvoiceView, error) {
	var v api.InvoiceView
	if !kernel.ValidIdentifier(invoiceID) {
		return v, kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
	}
	err := s.Ledger.Read(ctx, api.CommandGetInvoice, invoiceID, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		inv, err := postgres.LoadInvoice(ctx, exec, sc, invoiceID, postgres.NoLock)
		if err != nil {
			return err
		}
		if inv == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.InvoiceView{PurchaseInvoiceID: inv.ID, Status: inv.Status, RowVersion: inv.RowVersion,
			File: api.LicenseFileView{FileID: inv.FileID, ContentSHA256: inv.FileSHA256}, Candidate: factsView(inv.Candidate),
			ImportedBy: inv.ImportedBy, ImportedAt: inv.ImportedAt, Decision: inv.Decision, Reason: inv.Reason, DecidedBy: inv.DecidedBy,
			DecidedAt: inv.DecidedAt, Differences: []api.DifferenceView{}, Matches: []api.MatchView{}, Balances: []api.EvidenceBalanceView{},
			ReversesInvoiceID: inv.RedOfID, ReversedByID: inv.ReversedBy, AuthorityEpoch: inv.ImportEpoch, CheckedAt: kernel.Instant(s.Clock.Now())}
		if inv.Verified == nil {
			return nil
		}
		verified := factsView(*inv.Verified)
		v.Verified, v.AuthorityEpoch = &verified, inv.DecideEpoch
		candidateSource := api.SourceView{Step: "import", Actor: inv.ImportedBy, FileID: inv.FileID, At: inv.ImportedAt}
		verifiedSource := api.SourceView{Step: "verify", Actor: inv.DecidedBy, At: inv.DecidedAt}
		for _, d := range pin.Differences(inv.Candidate, *inv.Verified) {
			v.Differences = append(v.Differences, api.DifferenceView{Field: d.Field, Candidate: d.Candidate, Verified: d.Verified,
				CandidateSource: candidateSource, VerifiedSource: verifiedSource})
		}
		matches, err := postgres.Matches(ctx, exec, sc, inv.ID)
		if err != nil {
			return err
		}
		colour := inv.Verified.Colour
		var reds []*pin.Invoice
		var taken []pin.Match
		if colour == pin.Blue { // S5R-D20: what red invoices reversed of it and took back of its matches
			if reds, err = postgres.RedInvoices(ctx, exec, sc, inv.ID); err != nil {
				return err
			}
			if taken, err = postgres.TakeBacks(ctx, exec, sc, inv.ID); err != nil {
				return err
			}
		}
		reversed, takenBack := pin.Reversals(*inv.Verified, reds, taken)
		v.ReversedByIDs = []string{}
		for _, r := range reds {
			v.ReversedByIDs = append(v.ReversedByIDs, r.ID)
		}
		switch {
		case inv.Status == pin.Reversed:
			v.Reversal = api.ReversalFull
		case len(reds) > 0:
			v.Reversal = api.ReversalPartial
		}
		for _, m := range matches {
			mv := api.MatchView{LineNo: m.LineNo, CommitmentID: m.Ref.CommitmentID, OrderLineID: m.Ref.OrderLineID,
				Quantity: m.Quantity.String(), Unit: m.Unit, SupplierID: m.SupplierID, PONumber: m.PONumber, PurchaseQuantity: m.PurchaseQuantity.String(),
				DeliveredQuantity: m.DeliveredQuantity.String(), Status: m.Status(inv.Status, colour, takenBack[m.Key()]), MatchedAt: m.MatchedAt}
			if colour == pin.Blue {
				mv.TakenBack = takenBack[m.Key()].String()
			}
			v.Matches = append(v.Matches, mv)
		}
		if colour != pin.Blue {
			return nil
		}
		usage, err := postgres.InvoiceUsage(ctx, exec, sc, inv.ID)
		if err != nil {
			return err
		}
		consumed, err := postgres.Consumptions(ctx, exec, sc, inv.ID) // S5 C10: opening consumption
		if err != nil {
			return err
		}
		for _, l := range inv.Verified.Lines {
			q, _ := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
			for _, p := range []string{tr.ExportRefund, tr.ExportExemption} {
				var used kernel.Decimal
				for _, u := range usage {
					if u.LineNo == l.LineNo && u.Purpose == p {
						used = u.Used
					}
				}
				b := api.EvidenceBalanceView{LineNo: l.LineNo, Unit: l.Unit, Purpose: p, Quantity: q.String(), Reversed: reversed[l.LineNo].Quantity.String(),
					Allocated: used.String()}
				for _, x := range consumed {
					if x.LineNo == l.LineNo && x.Purpose == p {
						b.OpeningConsumed, used = x.Consumed.String(), used.Add(x.Consumed)
					}
				}
				b.Available = q.Sub(reversed[l.LineNo].Quantity).Sub(used).String()
				v.Balances = append(v.Balances, b)
			}
		}
		return nil
	})
	return v, err
}

// Invoices returns the company's purchase invoices, newest first (purchase_invoice.list, cnx.read).
func (s Service) Invoices(ctx context.Context) ([]api.InvoiceSummary, error) {
	out := []api.InvoiceSummary{}
	err := s.Ledger.Read(ctx, api.CommandInvoices, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		invs, err := postgres.Invoices(ctx, exec, scope(ctx))
		if err != nil {
			return err
		}
		for _, i := range invs {
			f := i.Facts()
			out = append(out, api.InvoiceSummary{PurchaseInvoiceID: i.ID, Status: i.Status, Kind: f.Kind, Colour: f.Colour, Number: f.Number,
				IssuedOn: f.IssuedOn, SupplierName: f.Supplier.Name, GrossTotal: f.GrossTotal, CurrencyCode: f.Currency.Code, RowVersion: i.RowVersion,
				ImportedBy: i.ImportedBy, ImportedAt: i.ImportedAt})
		}
		return nil
	})
	return out, err
}

func minorText(m *kernel.Money) string {
	if m == nil {
		return ""
	}
	return m.MinorString()
}

func taxEvidenceViews(e []tr.Evidence) []api.EvidenceView {
	out := []api.EvidenceView{}
	for _, x := range e {
		out = append(out, api.EvidenceView{SourceID: x.SourceID, ContentSHA256: x.ContentSHA256})
	}
	return out
}

// GetTaxCase returns a tax refund case (tax_refund_case.get, cnx.read): the export lot, the policy
// revision it binds, every allocation with its state, every result and the amounts side by side.
func (s Service) GetTaxCase(ctx context.Context, caseID string) (api.TaxCaseView, error) {
	var v api.TaxCaseView
	if !kernel.ValidIdentifier(caseID) {
		return v, kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
	}
	err := s.Ledger.Read(ctx, api.CommandGetTaxCase, caseID, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		c, err := postgres.LoadCase(ctx, exec, sc, caseID, postgres.NoLock)
		if err != nil {
			return err
		}
		if c == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		allocations, err := postgres.CaseAllocations(ctx, exec, sc, c)
		if err != nil {
			return err
		}
		results, err := postgres.TaxResults(ctx, exec, sc, c.ID)
		if err != nil {
			return err
		}
		v = api.TaxCaseView{CaseID: c.ID, ExportLotID: c.ExportLotID, Attempt: c.Attempt, OrderID: c.OrderID, ExportedAt: c.ExportedAt,
			Policy: policyView(c.Policy), Status: c.Status, RowVersion: c.RowVersion, Allocations: []api.TaxAllocationView{},
			Results: []api.TaxResultView{}, OpenedBy: c.OpenedBy, OpenedAt: c.OpenedAt, AuthorityEpoch: c.Epoch,
			CheckedAt: kernel.Instant(s.Clock.Now())}
		if v.PolicyAt, v.PolicySuperseded, err = policyAtView(ctx, exec, c); err != nil { // S5R-D23
			return err
		}
		rebindings, err := postgres.Rebindings(ctx, exec, sc, c.ID)
		if err != nil {
			return err
		}
		v.Rebindings = rebindingViews(rebindings)
		for _, a := range allocations {
			v.Allocations = append(v.Allocations, api.TaxAllocationView{AllocationID: a.ID, ExportLineID: a.ExportLineID, ShipmentID: a.ShipmentID,
				OrderLineID: a.OrderLineID, SkuID: a.SkuID, SpecRevision: a.SpecRevision, ExportQuantity: a.ExportQuantity.String(),
				ExportUnit: string(a.ExportUnit), PurchaseInvoiceID: a.InvoiceID, InvoiceKind: a.InvoiceKind, InvoiceNumber: a.InvoiceNumber,
				LineNo: a.LineNo, Purpose: a.Purpose, Quantity: a.Quantity.String(), Unit: string(a.Unit), Status: a.Status(), Replaces: a.Replaces,
				ReplacedBy: a.ReplacedBy, AllocatedBy: a.By, AllocatedAt: a.At, AuthorityEpoch: a.Epoch})
		}
		for _, r := range results {
			rv := api.TaxResultView{ResultID: r.ID, Stage: r.Stage, From: r.From, AmountMinor: minorText(r.Amount), OffsetMinor: minorText(r.Offset),
				TaxPeriod: r.TaxPeriod, Reviewer: r.Reviewer, ReviewEvidence: taxEvidenceViews(r.ReviewEvidence), ExternalReference: r.ExternalReference,
				ResultText: r.ResultText, FundRecordID: r.FundRecordID, PurchaseInvoiceID: r.PurchaseInvoiceID, Evidence: taxEvidenceViews(r.Evidence),
				Reason: r.Reason, RecordedBy: r.By,
				RecordedAt: r.At, AuthorityEpoch: r.Epoch}
			if r.Currency != nil {
				rv.CurrencyCode = r.Currency.Code
			}
			v.Results = append(v.Results, rv)
		}
		if v.Opening, err = openingRef(ctx, exec, c); err != nil {
			return err
		}
		a := tr.AmountsOf(results)
		v.Amounts = api.TaxAmountsView{CurrencyCode: a.Currency, ExpectedMinor: minorText(a.Expected), PlannedMinor: minorText(a.Planned),
			ApprovedMinor: minorText(a.Approved), OffsetMinor: minorText(a.Offset), ReceivedMinor: minorText(a.Received),
			ApprovedLessExpectedMinor: minorText(a.ApprovedLessExpected), ReceivedLessApprovedMinor: minorText(a.ReceivedLessApproved)}
		return nil
	})
	return v, err
}

// TaxCases returns the company's tax refund cases, newest first (tax_refund_case.list, cnx.read).
func (s Service) TaxCases(ctx context.Context) ([]api.TaxCaseSummary, error) {
	out := []api.TaxCaseSummary{}
	err := s.Ledger.Read(ctx, api.CommandTaxCases, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		cs, err := postgres.Cases(ctx, exec, scope(ctx))
		if err != nil {
			return err
		}
		for _, c := range cs {
			out = append(out, api.TaxCaseSummary{CaseID: c.ID, ExportLotID: c.ExportLotID, Attempt: c.Attempt, Status: c.Status,
				PolicyID: c.Policy.PolicyID, PolicyRevision: c.Policy.Revision, RefundMethod: c.Policy.RefundMethod, RowVersion: c.RowVersion,
				OpenedAt: c.OpenedAt})
		}
		return nil
	})
	return out, err
}
