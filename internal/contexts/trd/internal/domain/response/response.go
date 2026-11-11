// Package response is the CustomerResponse aggregate of TRD (tactical model
// TRD-AGG-CustomerResponse; WBS S3-06; TRD-INV-011, 012; UL TRD-CustomerResponse,
// TRD-AcceptanceReceivedAt, TRD-CustomerPO; Python T04 A36 record_customer_response), identified by
// (tenant, company, response_id): the customer's reply to an active formal quote revision as an
// operator or an AI agent recorded it — bound to the revision, its business hash and its formal
// document's number, classified, summarized, located in the customer's message (an EVD source and
// the place in it), and with the time the message arrived and the basis of that time.
//
// A response is an observation and is only appended (concurrency: 只追加；同 ID 相同内容可重放): it is
// never changed, the same ID with the same content answers from the stored response, and with other
// content it is RESPONSE_IMMUTABLE. No classification creates an acceptance or an order; a human
// verifies a full_unconditional response into one (package acceptance).
//
// The arrival time (ReceivedAt) counts when its basis can be verified — the channel server's
// receiving time or a trusted import's record — and lies between the revision's formalization and
// the recording (ACCEPTANCE_TIME_CONFLICT otherwise). The sender's own claim (source_claimed, such as
// an email's Date header) is kept and never counts.
package response

import (
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of the error catalog rows this package decides; package api declares the same with their
// categories.
const (
	CodeResponseImmutable      = "RESPONSE_IMMUTABLE"
	CodeAcceptanceTimeConflict = "ACCEPTANCE_TIME_CONFLICT"
)

// Categories maps the codes above to their error catalog category.
var Categories = map[string]kernel.Category{
	CodeResponseImmutable:      kernel.CategoryInvalid,
	CodeAcceptanceTimeConflict: kernel.CategoryInvalid,
}

func fail(code, path, reason string) error {
	if c, ok := Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}

// Classification is what the operator read in the reply (Python T04 A36; ES-05): a complete and
// unconditional acceptance, a conditional one (a counter-offer: a customer PO that differs from the
// revision line by line or adds terms is one), a partial one, a rejection, or an ambiguous reply.
type Classification string

const (
	FullUnconditional Classification = "full_unconditional"
	Conditional       Classification = "conditional"
	Partial           Classification = "partial"
	Reject            Classification = "reject"
	Ambiguous         Classification = "ambiguous"
)

// Basis is where an arrival time comes from.
type Basis string

const (
	Channel Basis = "channel_server" // the receiving channel server's time, located in the message
	Import  Basis = "import_record"  // the time a trusted import recorded the message
	Claimed Basis = "source_claimed" // the sender's own claim: kept, never counts
)

// Counts reports whether an arrival on basis b can be verified and judges the acceptance.
func (b Basis) Counts() bool { return b == Channel || b == Import }

// Arrival is when the customer's message reached us, and on what basis.
type Arrival struct {
	At    time.Time
	Basis Basis
}

// Response is the CustomerResponse aggregate.
type Response struct {
	ID             string
	QuoteID        string
	Revision       int64
	BusinessHash   string
	Number         string    // the revision's formal document number
	ExpiresAt      time.Time // the revision's validity end
	Classification Classification
	Summary        string
	Message        quote.Evidence   // the customer's message: an EVD source and the place in it
	Evidence       []quote.Evidence // further evidence
	Arrival        *Arrival         // nil: no arrival stated
	CustomerPO     string
	Recorded       quote.Act // who recorded it, under which authority epoch, when
	Fresh          bool
}

// Draft is a response as quote.response.record states it.
type Draft struct {
	ID             string
	QuoteID        string
	Revision       int64
	BusinessHash   string
	Classification Classification
	Summary        string
	Message        quote.Evidence
	Evidence       []quote.Evidence
	Arrival        *Arrival
	CustomerPO     string
}

// Sources returns the EVD sources the response cites: its message first, then its evidence.
func (r *Response) Sources() []string {
	ids := []string{r.Message.SourceID}
	for _, e := range r.Evidence {
		ids = append(ids, e.SourceID)
	}
	return ids
}

// CountingArrival returns the arrival that judges an acceptance of r, nil when r states none that
// counts.
func (r *Response) CountingArrival() *Arrival {
	if r.Arrival != nil && r.Arrival.Basis.Counts() {
		return r.Arrival
	}
	return nil
}

// Late reports whether no acceptance can form from r at instant at (TD-AT03; UL TRD-LateAcceptance):
// its counting arrival is not before the revision's expires_at, or, without one, the verification
// could come no earlier than the later of the recording and at, which is not before it.
func (r *Response) Late(at time.Time) bool {
	if a := r.CountingArrival(); a != nil {
		return !a.At.Before(r.ExpiresAt)
	}
	earliest := r.Recorded.At
	if at.After(earliest) {
		earliest = at
	}
	return !earliest.Before(r.ExpiresAt)
}

// Same reports whether r records exactly what d states.
func (r *Response) Same(d Draft) bool {
	sameArrival := (r.Arrival == nil) == (d.Arrival == nil) &&
		(r.Arrival == nil || (r.Arrival.At.Equal(d.Arrival.At) && r.Arrival.Basis == d.Arrival.Basis))
	return r.ID == d.ID && r.QuoteID == d.QuoteID && r.Revision == d.Revision && r.BusinessHash == d.BusinessHash &&
		r.Classification == d.Classification && r.Summary == d.Summary && r.Message == d.Message &&
		slices.Equal(r.Evidence, d.Evidence) && sameArrival && r.CustomerPO == d.CustomerPO
}

// Record decides quote.response.record of d on quote q, locked by the caller, at now (Python
// record_customer_response): the actor reads the message's source and every evidence source
// (readable; REFERENCE_INVALID at the first other one); a response stored under d's ID answers when
// it is the same (true) and is RESPONSE_IMMUTABLE otherwise; the revision is a formal revision of q
// (REFERENCE_INVALID) at the stated hash (BUSINESS_HASH_MISMATCH), q's active revision and unheld
// (quote.Usable: QUOTE_NOT_CURRENT, the hold's code); a counting arrival lies between the revision's
// formalization and now (ACCEPTANCE_TIME_CONFLICT). The caller checked the quote's visibility and
// object control first.
func Record(q *quote.Quote, stored *Response, d Draft, readable map[string]bool, by kernel.ActorRef, epoch int64, now time.Time) (*Response, bool, error) {
	if !readable[d.Message.SourceID] {
		return nil, false, kernel.Reject(kernel.CodeReferenceInvalid, "/args/source_ref/source_id", "")
	}
	for i, e := range d.Evidence {
		if !readable[e.SourceID] {
			return nil, false, kernel.Reject(kernel.CodeReferenceInvalid, "/args/evidence/"+strconv.Itoa(i)+"/source_id", "")
		}
	}
	if stored != nil {
		if stored.Same(d) {
			return stored, true, nil
		}
		return nil, false, fail(CodeResponseImmutable, "/args/response_id", "The response is recorded with other content")
	}
	rev := q.Revision(d.Revision)
	switch {
	case rev == nil || rev.Formal == nil || rev.Seal == nil:
		return nil, false, kernel.Reject(kernel.CodeReferenceInvalid, "/args/business_revision", "The revision has no formal document")
	case rev.Seal.BusinessHash != d.BusinessHash:
		return nil, false, kernel.RejectAs(quote.CodeBusinessHashMismatch, quote.Categories[quote.CodeBusinessHashMismatch],
			"/args/business_hash", "The revision has another business hash")
	}
	if err := q.Usable(d.Revision, d.BusinessHash, "/args/business_revision"); err != nil {
		return nil, false, err
	}
	at := kernel.Instant(now)
	if a := d.Arrival; a != nil && a.Basis.Counts() {
		switch {
		case a.At.After(at):
			return nil, false, fail(CodeAcceptanceTimeConflict, "/args/received_at", "The message arrived after it was recorded")
		case a.At.Before(rev.Formal.Commit.At):
			return nil, false, fail(CodeAcceptanceTimeConflict, "/args/received_at", "The message arrived before the revision was formalized")
		}
	}
	r := &Response{ID: d.ID, QuoteID: q.ID, Revision: rev.Number, BusinessHash: rev.Seal.BusinessHash, Number: rev.Formal.Document.Number,
		ExpiresAt: rev.Seal.ExpiresAt, Classification: d.Classification, Summary: d.Summary, Message: d.Message,
		Evidence: slices.Clone(d.Evidence), CustomerPO: d.CustomerPO, Recorded: quote.Act{By: by, Epoch: epoch, At: at}, Fresh: true}
	if a := d.Arrival; a != nil {
		r.Arrival = &Arrival{At: kernel.Instant(a.At), Basis: a.Basis}
	}
	return r, false, nil
}
