package channel

import (
	"strings"

	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ContentQuoteDocument is the content kind of a formal quote revision's document.
const ContentQuoteDocument = "quote_document"

// FromDispatch is what may be sent of a formal quote revision, as TRD's dispatch read side answers
// it (Python T03 A14–A16; TRD quote.dispatch.preflight and quote.dispatch.target, read as the
// person who authorizes the send): the revision ready to send (preflight), the target's only
// reason the missing channel (EXTERNAL_CHANNEL_NOT_CONFIGURED — Brant has no dispatch channel), the
// content the formal document (business revision and the document's SHA-256), the recipient the
// sealed recipient snapshot and its first address. Anything else is INVALID_STATE with TRD's
// reasons: nothing is sent that TRD would not send.
func FromDispatch(p trdapi.DispatchPreflight, t trdapi.DispatchTarget) (Content, Recipient, error) {
	switch {
	case p.QuoteID != t.QuoteID || p.BusinessRevision != t.BusinessRevision || p.BusinessHash != t.BusinessHash:
		return Content{}, Recipient{}, kernel.Reject(kernel.CodeReferenceInvalid, "/dispatch", "The preflight and the target are of different revisions")
	case !p.Ready || !t.PreflightReady:
		return Content{}, Recipient{}, kernel.Reject(kernel.CodeInvalidState, "/dispatch", "Not ready to send: "+reasons(p.Reasons))
	}
	for _, r := range t.Reasons {
		if r.Check != trdapi.CheckChannel || r.Code != trdapi.CodeExternalChannelNotConfigured {
			return Content{}, Recipient{}, kernel.Reject(kernel.CodeInvalidState, "/dispatch", "Not ready to send: "+reasons(t.Reasons))
		}
	}
	if len(t.Addresses) == 0 || len(t.RecipientSnapshotSHA256) != 64 || len(p.DocumentSHA256) != 64 {
		return Content{}, Recipient{}, kernel.Reject(kernel.CodeReferenceInvalid, "/dispatch", "The revision has no sealed recipient or document")
	}
	return Content{Kind: ContentQuoteDocument, ID: p.QuoteID, Version: p.BusinessRevision, SHA256: p.DocumentSHA256},
		Recipient{PartyID: t.CustomerRef.PartyID, SnapshotSHA256: t.RecipientSnapshotSHA256, Address: t.Addresses[0].Address}, nil
}

func reasons(rs []trdapi.Reason) string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Check + ":" + r.Code
	}
	return strings.Join(out, ", ")
}
