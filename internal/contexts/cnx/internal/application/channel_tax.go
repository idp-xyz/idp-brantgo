package application

import (
	"context"
	"errors"

	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	tr "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/taxrefund"
	"go.idp.xyz/idp-brant/internal/foundation/interop"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// SubmitTaxChannel files a planned tax refund case. A timeout is unknown. received stays a manual
// record: the channel does not record a fund. An expected case is refused until it is planned.
func (s Service) SubmitTaxChannel(ctx context.Context, cmd api.SubmitTaxChannel) (api.ChannelAnswer, error) {
	if err := cmd.Validate(); err != nil {
		return api.ChannelAnswer{}, s.refuse(ctx, api.CommandSubmitTaxChannel, cmd.CaseID, err)
	}
	c, err := s.loadTaxCase(ctx, cmd.CaseID)
	if err != nil {
		return api.ChannelAnswer{}, err
	}
	if c == nil {
		return api.ChannelAnswer{}, s.refuse(ctx, api.CommandSubmitTaxChannel, cmd.CaseID, kernel.Reject(kernel.CodeNotFound, "/args/case_id", ""))
	}
	attempt, err := s.loadAttempt(ctx, interop.ChannelTaxRefund, c.ID, api.CommandGetTaxCase)
	if err != nil {
		return api.ChannelAnswer{}, err
	}
	switch c.Status {
	case tr.Planned:
		if err = tr.CheckVersion(c, cmd.ExpectedVersion); err != nil {
			return api.ChannelAnswer{}, s.refuse(ctx, api.CommandSubmitTaxChannel, c.ID, err)
		}
		attempt, err = s.openAttempt(ctx, cmd, interop.ChannelTaxRefund, c.ID, tr.Planned, c.RowVersion)
		if err != nil {
			return api.ChannelAnswer{}, err
		}
		return s.fileTax(ctx, cmd, c, attempt)
	case tr.Submitted:
		if attempt == nil || attempt.State != string(interop.Planned) && attempt.State != string(interop.Submitted) {
			return api.ChannelAnswer{}, s.refuse(ctx, api.CommandSubmitTaxChannel, c.ID,
				kernel.Reject(kernel.CodeInvalidState, "/args/case_id", "Record this case's result by hand"))
		}
		return s.recoverTax(ctx, cmd, c, attempt)
	case tr.Unknown:
		return s.taxUnknown(ctx, api.CommandSubmitTaxChannel, cmd.ExpectedVersion, cmd.IdempotencyKey, c, attempt, true)
	case tr.Approved, tr.Rejected, tr.NotReceived, tr.Received, tr.ConvertedToExemption:
		return s.finishTax(ctx, cmd.IdempotencyKey, c, attempt)
	case tr.Expected:
		return api.ChannelAnswer{}, s.refuse(ctx, api.CommandSubmitTaxChannel, c.ID,
			kernel.Reject(kernel.CodeInvalidState, "/args/case_id", "Plan the tax refund before the channel files it"))
	default:
		return api.ChannelAnswer{}, s.refuse(ctx, api.CommandSubmitTaxChannel, c.ID,
			kernel.Reject(kernel.CodeInvalidState, "/args/case_id", "The tax refund case is "+c.Status))
	}
}

// ReconcileTaxChannel looks an unknown filing up. NotFound, with evidence and a reason, is
// not_received and releases the evidence. An approval needs Currency. A rejection records the
// professional text.
func (s Service) ReconcileTaxChannel(ctx context.Context, cmd api.ReconcileTaxChannel) (api.ChannelAnswer, error) {
	if err := cmd.Validate(); err != nil {
		return api.ChannelAnswer{}, s.refuse(ctx, api.CommandReconcileTaxChannel, cmd.CaseID, err)
	}
	c, err := s.loadTaxCase(ctx, cmd.CaseID)
	if err != nil {
		return api.ChannelAnswer{}, err
	}
	if c == nil {
		return api.ChannelAnswer{}, s.refuse(ctx, api.CommandReconcileTaxChannel, cmd.CaseID, kernel.Reject(kernel.CodeNotFound, "/args/case_id", ""))
	}
	attempt, err := s.loadAttempt(ctx, interop.ChannelTaxRefund, c.ID, api.CommandGetTaxCase)
	if err != nil {
		return api.ChannelAnswer{}, err
	}
	switch c.Status {
	case tr.Unknown:
		if err = tr.CheckVersion(c, cmd.ExpectedVersion); err != nil {
			return api.ChannelAnswer{}, s.refuse(ctx, api.CommandReconcileTaxChannel, c.ID, err)
		}
		if attempt == nil {
			return api.ChannelAnswer{}, s.refuse(ctx, api.CommandReconcileTaxChannel, c.ID,
				kernel.Reject(kernel.CodeInvalidState, "/args/case_id", "The case has no channel attempt"))
		}
		return s.lookupTax(ctx, cmd, c, attempt)
	case tr.Approved, tr.Rejected, tr.NotReceived, tr.Received, tr.ConvertedToExemption:
		return s.finishTax(ctx, cmd.IdempotencyKey, c, attempt)
	default:
		return api.ChannelAnswer{}, s.refuse(ctx, api.CommandReconcileTaxChannel, c.ID,
			kernel.Reject(kernel.CodeInvalidState, "/args/case_id", "The tax refund case is "+c.Status+", not unknown"))
	}
}

func (s Service) fileTax(ctx context.Context, cmd api.SubmitTaxChannel, c *tr.Case, attempt *postgres.ChannelAttempt) (api.ChannelAnswer, error) {
	res, err := s.port().Submit(ctx, interop.Submission{ID: interop.OperationID(attempt.OperationID), Channel: interop.ChannelTaxRefund, Subject: c.ID})
	return s.afterTaxSend(ctx, api.CommandSubmitTaxChannel, cmd.IdempotencyKey, cmd.EvidenceIDs, cmd.Currency, c, attempt, res, err, false)
}

func (s Service) recoverTax(ctx context.Context, cmd api.SubmitTaxChannel, c *tr.Case, attempt *postgres.ChannelAttempt) (api.ChannelAnswer, error) {
	res, err := s.port().Lookup(ctx, interop.OperationID(attempt.OperationID))
	return s.afterTaxSend(ctx, api.CommandSubmitTaxChannel, cmd.IdempotencyKey, cmd.EvidenceIDs, cmd.Currency, c, attempt, res, err, true)
}

func (s Service) lookupTax(ctx context.Context, cmd api.ReconcileTaxChannel, c *tr.Case, attempt *postgres.ChannelAttempt) (api.ChannelAnswer, error) {
	res, err := s.port().Lookup(ctx, interop.OperationID(attempt.OperationID))
	if errors.Is(err, interop.ErrNotConfigured) {
		return api.ChannelAnswer{}, s.notConfigured(ctx, api.CommandReconcileTaxChannel, c.ID)
	}
	if errors.Is(err, interop.ErrUnavailable) {
		return api.ChannelAnswer{}, s.unavailable(ctx, api.CommandReconcileTaxChannel, c.ID,
			"The professional system could not be reached; the tax refund case stays unknown")
	}
	if errors.Is(err, interop.ErrNotFound) {
		if cmd.Reason == "" {
			return api.ChannelAnswer{}, s.refuse(ctx, api.CommandReconcileTaxChannel, c.ID, kernel.Reject(kernel.CodeSchemaInvalid, "/args/reason", ""))
		}
		return s.writeTax(ctx, cmd.IdempotencyKey, c, attempt, cmd.EvidenceIDs, "",
			api.RecordTaxResult{Stage: api.TaxNotReceived, EvidenceIDs: cmd.EvidenceIDs, Reason: cmd.Reason},
			string(interop.NotReceived), "")
	}
	if err == nil && taxAuthoritative(res) {
		return s.writeTaxResult(ctx, api.CommandReconcileTaxChannel, cmd.IdempotencyKey, cmd.EvidenceIDs, cmd.Currency, c, attempt, res)
	}
	if err != nil {
		return api.ChannelAnswer{}, err
	}
	return api.ChannelAnswer{}, s.refuse(ctx, api.CommandReconcileTaxChannel, c.ID,
		kernel.Reject(kernel.CodeInvalidKnownValue, "/channel", "The professional answer cannot be recorded"))
}

func (s Service) afterTaxSend(ctx context.Context, command, key string, evidence []string, currency *api.CurrencyRef, c *tr.Case, attempt *postgres.ChannelAttempt, res interop.Result, err error, recovery bool) (api.ChannelAnswer, error) {
	if errors.Is(err, interop.ErrNotConfigured) {
		return api.ChannelAnswer{}, s.notConfigured(ctx, command, c.ID)
	}
	if errors.Is(err, interop.ErrUnavailable) {
		return api.ChannelAnswer{}, s.unavailable(ctx, command, c.ID, "The professional system could not be reached; nothing was recorded as a success")
	}
	if err == nil && taxAuthoritative(res) {
		return s.writeTaxResult(ctx, command, key, evidence, currency, c, attempt, res)
	}
	if errors.Is(err, interop.ErrUnknown) || errors.Is(err, interop.ErrNotFound) || err == nil && !recovery {
		return s.writeTax(ctx, key, c, attempt, evidence, attempt.OperationID,
			api.RecordTaxResult{Stage: api.TaxUnknown, EvidenceIDs: evidence, Reason: channelTimeoutReason},
			string(interop.Unknown), "")
	}
	if err != nil {
		return api.ChannelAnswer{}, err
	}
	return api.ChannelAnswer{}, s.refuse(ctx, command, c.ID,
		kernel.Reject(kernel.CodeInvalidKnownValue, "/channel", "The professional answer cannot be recorded"))
}

func (s Service) writeTaxResult(ctx context.Context, command, key string, evidence []string, currency *api.CurrencyRef, c *tr.Case, attempt *postgres.ChannelAttempt, res interop.Result) (api.ChannelAnswer, error) {
	stage := api.RecordTaxResult{EvidenceIDs: evidence, ResultText: res.OriginalText}
	state := string(interop.Failed)
	outcome := res.Outcome
	if res.Outcome == api.TaxApproved {
		amount, cur, err := s.approvedAmount(ctx, currency, res)
		if err != nil {
			return api.ChannelAnswer{}, s.refuse(ctx, command, c.ID, err)
		}
		stage.Stage, stage.Amount, stage.Currency = api.TaxApproved, amount, cur
		state = string(interop.Succeeded)
	} else {
		stage.Stage = api.TaxRejected
	}
	return s.writeTax(ctx, key, c, attempt, evidence, attempt.OperationID, stage, state, outcome)
}

func taxAuthoritative(res interop.Result) bool {
	if !api.Text(res.OriginalText, api.MaxResultText) {
		return false
	}
	switch {
	case res.State == interop.Failed && res.Outcome == api.TaxRejected:
		return true
	case res.State == interop.Succeeded && res.Outcome == api.TaxApproved && res.AmountMinor > 0 && len(res.CurrencyCode) == 3:
		return true
	default:
		return false
	}
}

func (s Service) approvedAmount(ctx context.Context, ref *api.CurrencyRef, res interop.Result) (string, *api.CurrencyRef, error) {
	if ref == nil {
		return "", nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/currency", "The approval needs the case currency")
	}
	resolved, err := s.resolveCurrency(ctx, *ref)
	if err != nil {
		return "", nil, err
	}
	cur, err := currencyOf(resolved, "/args/currency")
	if err != nil {
		return "", nil, err
	}
	if cur.Code != res.CurrencyCode {
		return "", nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/currency", "The professional amount is in "+res.CurrencyCode)
	}
	kc, err := cur.Kernel()
	if err != nil {
		return "", nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/currency", "")
	}
	m, err := kernel.NewMoney(kc, res.AmountMinor)
	if err != nil {
		return "", nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/amount", "The professional amount is not a valid "+cur.Code+" amount")
	}
	return m.Amount(), ref, nil
}

// reference is the operation id recorded on the submitted result. It is empty when the case was
// already submitted. stage is the result after that submission, or the only result of a lookup.
func (s Service) writeTax(ctx context.Context, key string, c *tr.Case, attempt *postgres.ChannelAttempt, evidence []string, reference string, stage api.RecordTaxResult, settleState, settleOutcome string) (api.ChannelAnswer, error) {
	version := c.RowVersion
	var steps []api.RecordTaxResult
	if c.Status == tr.Planned {
		steps = append(steps, api.RecordTaxResult{Stage: api.TaxSubmitted, ExternalReference: reference, EvidenceIDs: evidence})
	}
	steps = append(steps, stage)
	for i := range steps {
		steps[i].IdempotencyKey = phaseKey(key, "tax-"+steps[i].Stage)
		steps[i].CaseID = c.ID
		steps[i].ExpectedVersion = 1
		if err := steps[i].Validate(); err != nil {
			return api.ChannelAnswer{}, err
		}
	}
	var last api.TaxCaseReceipt
	for i := range steps {
		steps[i].ExpectedVersion = version
		got, err := s.RecordTaxResult(ctx, steps[i])
		if err != nil {
			return api.ChannelAnswer{}, err
		}
		version = got.RowVersion
		last = got
	}
	reason := stage.Reason
	if err := s.settle(ctx, key, interop.ChannelTaxRefund, c.ID, settleState, settleOutcome, reason, false); err != nil {
		return api.ChannelAnswer{}, err
	}
	a, err := s.loadAttempt(ctx, interop.ChannelTaxRefund, c.ID, api.CommandGetTaxCase)
	if err != nil {
		return api.ChannelAnswer{}, err
	}
	return answered(last.Receipt, a), nil
}

func (s Service) taxUnknown(ctx context.Context, command string, version int64, key string, c *tr.Case, attempt *postgres.ChannelAttempt, refuseAgain bool) (api.ChannelAnswer, error) {
	if err := tr.CheckVersion(c, version); err != nil {
		return api.ChannelAnswer{}, s.refuse(ctx, command, c.ID, err)
	}
	if attempt != nil && attempt.State == string(interop.Unknown) && refuseAgain {
		return api.ChannelAnswer{}, s.refuse(ctx, command, c.ID, kernel.Reject(kernel.CodeInvalidState, "/args/case_id",
			"The filing's result is unknown: verify it before filing again"))
	}
	if attempt != nil && !attemptTerminal(attempt.State) {
		if err := s.settle(ctx, key, interop.ChannelTaxRefund, c.ID, string(interop.Unknown), "", channelTimeoutReason, false); err != nil {
			return api.ChannelAnswer{}, err
		}
		var err error
		attempt, err = s.loadAttempt(ctx, interop.ChannelTaxRefund, c.ID, api.CommandGetTaxCase)
		if err != nil {
			return api.ChannelAnswer{}, err
		}
	}
	return answered(api.Receipt{ObjectID: c.ID, RowVersion: c.RowVersion, Status: c.Status}, attempt), nil
}

func (s Service) finishTax(ctx context.Context, key string, c *tr.Case, attempt *postgres.ChannelAttempt) (api.ChannelAnswer, error) {
	if state, outcome, ok := connectorTax(c.Status); ok && attempt != nil && attempt.State != state && !attemptTerminal(attempt.State) {
		if err := s.settle(ctx, key, interop.ChannelTaxRefund, c.ID, state, outcome, "", false); err != nil {
			return api.ChannelAnswer{}, err
		}
		var err error
		attempt, err = s.loadAttempt(ctx, interop.ChannelTaxRefund, c.ID, api.CommandGetTaxCase)
		if err != nil {
			return api.ChannelAnswer{}, err
		}
	}
	return answered(api.Receipt{ObjectID: c.ID, RowVersion: c.RowVersion, Status: c.Status}, attempt), nil
}

func connectorTax(status string) (state, outcome string, ok bool) {
	switch status {
	case tr.Approved:
		return string(interop.Succeeded), status, true
	case tr.Rejected:
		return string(interop.Failed), status, true
	case tr.NotReceived:
		return string(interop.NotReceived), "", true
	default:
		return "", "", false
	}
}
