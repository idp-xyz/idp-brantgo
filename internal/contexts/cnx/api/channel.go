package api

import (
	"context"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Channel commands (S6-05, S6-D05). The five filing and lookup commands are on the CNX command
// route, POST /api/brant/v1/cnx/commands/<command>, with the permissions the manual submission,
// receipt and tax result already use. Settle stays in process: it moves the attempt after the
// case has moved and is not a second filing. A timeout is recorded as unknown, never as a success.
const (
	CommandSubmitDeclarationChannel    = "declaration_case.channel_submit"
	CommandSettleDeclarationChannel    = "declaration_case.channel_settle"
	CommandIngestDeclarationReceipts   = "declaration_case.channel_ingest"
	CommandReconcileDeclarationChannel = "declaration_case.channel_reconcile"
	CommandSubmitTaxChannel            = "tax_refund_case.channel_submit"
	CommandSettleTaxChannel            = "tax_refund_case.channel_settle"
	CommandReconcileTaxChannel         = "tax_refund_case.channel_reconcile"
)

// MaxChannelReceipts is the most external receipts one ingest records.
const MaxChannelReceipts = 20

// SubmitDeclarationChannel files a confirmed declaration through the connector (human only, the
// documentation permission of record_submission). SubmitterName is our clerk's name. EvidenceIDs
// may be empty; an authoritative customs result needs at least one to be recorded as such.
type SubmitDeclarationChannel struct {
	IdempotencyKey  string
	DeclarationID   string
	ExpectedVersion int64
	SubmitterName   string
	EvidenceIDs     []string
}

// Validate checks the closed contract.
func (c SubmitDeclarationChannel) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DeclarationID):
		return schema("/args/declaration_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case !Text(c.SubmitterName, MaxSubmitterName):
		return schema("/args/submitter_name")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 0)
}

// ReconcileDeclarationChannel looks up an unknown declaration (human only, the documentation
// permission of record_receipt). EvidenceIDs are required: a found result is recorded with them,
// and not_received rests on them. Reason is required when the lookup finds nothing.
type ReconcileDeclarationChannel struct {
	IdempotencyKey  string
	DeclarationID   string
	ExpectedVersion int64
	EvidenceIDs     []string
	Reason          string
}

// Validate checks the closed contract.
func (c ReconcileDeclarationChannel) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DeclarationID):
		return schema("/args/declaration_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case c.Reason != "" && !Text(c.Reason, MaxReason):
		return schema("/args/reason")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 1)
}

// IngestDeclarationReceipts stores a burst of external receipts of a declaration that was already
// submitted (human only, record_receipt). A receipt that is not after the result already recorded,
// or that does not follow the state, is kept on the channel chain and does not advance the case
// (DECLARATION_RECEIPT_STALE, CNX-INV-003, CN-AT06). An exact repeat of one already chained is skipped.
type IngestDeclarationReceipts struct {
	IdempotencyKey  string
	DeclarationID   string
	ExpectedVersion int64
	Receipts        []ChannelReceiptIn
}

// ChannelReceiptIn is one external receipt as it arrived. ExportedAt is required for released and
// absent otherwise.
type ChannelReceiptIn struct {
	Outcome           string
	Sequence          int64
	StatusCode        string
	OriginalText      string
	MappingVersion    string
	DeclarationNumber string
	ReceivedAt        string
	ExportedAt        string
}

// Validate checks the closed contract.
func (c IngestDeclarationReceipts) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DeclarationID):
		return schema("/args/declaration_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case len(c.Receipts) < 1 || len(c.Receipts) > MaxChannelReceipts:
		return schema("/args/receipts")
	}
	for i, r := range c.Receipts {
		p := "/args/receipts/" + itoa(i)
		switch {
		case r.Outcome != DeclarationReceived && r.Outcome != DeclarationAccepted && r.Outcome != DeclarationReleased && r.Outcome != DeclarationRejected:
			return schema(p + "/outcome")
		case r.Sequence < 1:
			return schema(p + "/sequence")
		case !Text(r.StatusCode, MaxExternalStatus):
			return schema(p + "/status_code")
		case !Text(r.OriginalText, MaxOriginalText):
			return schema(p + "/original_text")
		case !kernel.ValidIdentifier(r.MappingVersion):
			return schema(p + "/mapping_version")
		case !Text(r.DeclarationNumber, MaxDeclarationRef):
			return schema(p + "/declaration_number")
		case !timestamp(r.ReceivedAt):
			return schema(p + "/received_at")
		case r.Outcome == DeclarationReleased && !timestamp(r.ExportedAt),
			r.ExportedAt != "" && r.Outcome != DeclarationReleased:
			return schema(p + "/exported_at")
		}
	}
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// SubmitTaxChannel files a planned tax refund case through the connector (human only, the tax
// specialist permission of record_result). EvidenceIDs are the filing's evidence. Currency is
// required only when the connector answers with an approved amount.
type SubmitTaxChannel struct {
	IdempotencyKey  string
	CaseID          string
	ExpectedVersion int64
	EvidenceIDs     []string
	Currency        *CurrencyRef
}

// Validate checks the closed contract.
func (c SubmitTaxChannel) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.CaseID):
		return schema("/args/case_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	}
	if c.Currency != nil {
		switch {
		case !kernel.ValidIdentifier(c.Currency.ID):
			return schema("/args/currency/id")
		case c.Currency.Revision < 1 || c.Currency.Revision > maxVersion:
			return schema("/args/currency/revision")
		}
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 1)
}

// ReconcileTaxChannel looks up an unknown tax filing (human only, record_result). Currency is
// required when the lookup returns an approved amount. Reason is required when it finds nothing.
type ReconcileTaxChannel struct {
	IdempotencyKey  string
	CaseID          string
	ExpectedVersion int64
	EvidenceIDs     []string
	Reason          string
	Currency        *CurrencyRef
}

// Validate checks the closed contract.
func (c ReconcileTaxChannel) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.CaseID):
		return schema("/args/case_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case c.Reason != "" && !Text(c.Reason, MaxReason):
		return schema("/args/reason")
	}
	if c.Currency != nil {
		switch {
		case !kernel.ValidIdentifier(c.Currency.ID):
			return schema("/args/currency/id")
		case c.Currency.Revision < 1 || c.Currency.Revision > maxVersion:
			return schema("/args/currency/revision")
		}
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 1)
}

// ChannelCommands is the connector entry point on the CNX command route. Settle is not on it.
type ChannelCommands interface {
	SubmitDeclarationChannel(ctx context.Context, cmd SubmitDeclarationChannel) (ChannelAnswer, error)
	ReconcileDeclarationChannel(ctx context.Context, cmd ReconcileDeclarationChannel) (ChannelAnswer, error)
	IngestDeclarationReceipts(ctx context.Context, cmd IngestDeclarationReceipts) (ChannelIngest, error)
	SubmitTaxChannel(ctx context.Context, cmd SubmitTaxChannel) (ChannelAnswer, error)
	ReconcileTaxChannel(ctx context.Context, cmd ReconcileTaxChannel) (ChannelAnswer, error)
}

// SettleDeclarationChannel and SettleTaxChannel move the connector attempt after the case has
// moved. The channel commands run them; they are not a second filing and they are not HTTP.
type SettleDeclarationChannel struct {
	IdempotencyKey string
	Channel        string
	SubjectID      string
	State          string
	Outcome        string
	Reason         string
}

// Validate checks the closed contract.
func (c SettleDeclarationChannel) Validate() error {
	return settleValid(c.Channel, c.SubjectID, c.State, c.Outcome, c.Reason)
}

// SettleTaxChannel moves the tax filing's connector attempt.
type SettleTaxChannel struct {
	IdempotencyKey string
	Channel        string
	SubjectID      string
	State          string
	Outcome        string
	Reason         string
}

// Validate checks the closed contract.
func (c SettleTaxChannel) Validate() error {
	return settleValid(c.Channel, c.SubjectID, c.State, c.Outcome, c.Reason)
}

func settleValid(channel, subject, state, outcome, reason string) error {
	switch {
	case channel != ChannelCustoms && channel != ChannelTax:
		return schema("/args/channel")
	case !kernel.ValidIdentifier(subject):
		return schema("/args/subject_id")
	case state != ChannelUnknown && state != ChannelSucceeded && state != ChannelFailed && state != ChannelNotReceived:
		return schema("/args/state")
	case outcome != "" && !Text(outcome, 32):
		return schema("/args/outcome")
	case reason != "" && !Text(reason, MaxReason):
		return schema("/args/reason")
	case (state == ChannelSucceeded || state == ChannelFailed) == (outcome == ""):
		return schema("/args/outcome")
	}
	return nil
}

// Connector channel names and the states an attempt settles to. They match foundation/interop.
const (
	ChannelCustoms     = "customs_declaration"
	ChannelTax         = "tax_refund"
	ChannelUnknown     = "unknown"
	ChannelSucceeded   = "succeeded"
	ChannelFailed      = "failed"
	ChannelNotReceived = "not_received"
)

// ChannelAnswer is the answer of a channel filing or lookup. Status is the declaration or the tax
// case. ConnectorState is the attempt's state. OperationID is stable for that attempt.
type ChannelAnswer struct {
	Receipt
	OperationID    string
	ConnectorState string
	AttemptNo      int
	Outcome        string
}

// ChannelIngest is the answer of an ingest: the case as stored now, and the chain including the
// receipts that did not advance it.
type ChannelIngest struct {
	Receipt
	DeclarationID string
	LastSequence  int64
	Chain         []ChannelChainEntry
}

// ChannelChainEntry is one ingested receipt, authoritative only when it advanced the case.
type ChannelChainEntry struct {
	ReceiptID      string
	IngressNo      int
	Sequence       int64
	Outcome        string
	Authoritative  bool
	RefusalCode    string
	StatusCode     string
	OriginalText   string
	MappingVersion string
}

// ChannelAttemptView is the connector attempt of one filing.
type ChannelAttemptView struct {
	AttemptID   string
	Channel     string
	SubjectID   string
	AttemptNo   int
	OperationID string
	State       string
	Outcome     string
	Reason      string
	RowVersion  int64
}

// ChannelLog is the attempt, when one was opened, and the ingested chain in arrival order.
type ChannelLog struct {
	Attempt *ChannelAttemptView
	Chain   []ChannelChainEntry
}

func (SubmitDeclarationChannel) cnxCommand()    {}
func (ReconcileDeclarationChannel) cnxCommand() {}
func (IngestDeclarationReceipts) cnxCommand()   {}
func (SubmitTaxChannel) cnxCommand()            {}
func (ReconcileTaxChannel) cnxCommand()         {}
func (SettleDeclarationChannel) cnxCommand()    {}
func (SettleTaxChannel) cnxCommand()            {}
