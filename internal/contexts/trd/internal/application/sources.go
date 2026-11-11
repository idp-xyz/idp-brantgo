package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// A stored result of a TRD command that cited EVD sources is handed back — by a replay of the
// same key or by reconciling it — only to a caller who reads those sources now, as a read is
// (contract 06 §07: 来源权限撤回后，查询、同键写重放与对账均不能返回成功事实; X-INV-003; Context
// Map R16's risk; Python test_t04_a45 test_source_revocation_blocks_query_both_replays_and_both_hashes
// and test_source_scope_change_is_rechecked_independently_of_quote).
//
// EVD is asked before TRD's transaction (storedSources, called by run and Reconcile; no other
// context is called inside TRD's transaction) and only when the key has a stored result; the
// ledger's visibility check, which every replay and reconciliation passes inside the transaction,
// judges the answer (sourcesRead): REFERENCE_INVALID when the caller no longer reads a source, and
// EVD's own refusal (PERMISSION_DENIED without evd.read) as a read answers it, for a matching and a
// conflicting request hash alike. The sources are those the command cites
// as sent (its evidence, a draft's sources, a margin exception's evidence) — the stored ones when
// the hash matches — and, for quote.response.record and quote.response.verify, those of the
// customer response its stored result names (the message and the evidence the response was
// recorded with), so a conflicting hash reveals nothing either. A first execution is not concerned:
// its own checks read the sources it cites. A replay racing the first commit of its own key finds
// no answer and is refused; the caller sends the key again.

// sourcesKey carries EVD's answer for the command a call runs or reconciles.
type sourcesKey struct{}

type sourcesVerdict struct {
	command  string
	readable bool  // the caller reads every source; false also when the key had no stored result yet
	refusal  error // EVD's refusal of the caller (PERMISSION_DENIED without evd.read), answered as a read answers it
}

// citing are the commands that cite EVD sources: their replays and reconciliations need the answer.
var citing = map[string]bool{api.CommandSaveDraft: true, api.CommandDiscardDraft: true, api.CommandDecideReview: true,
	api.CommandRevokeApproval: true, api.CommandWithdraw: true, api.CommandCancel: true, api.CommandRecordResponse: true,
	api.CommandVerifyResponse: true, api.CommandInvalidateAcceptance: true, api.CommandDisputeAcceptance: true,
	api.CommandDecideOrderReview: true, api.CommandRevokeOrderApproval: true, api.CommandCancelOrder: true,
	api.CommandCloseOrderUnfulfilled: true, api.CommandPrepareAmendment: true, api.CommandDecideAmendment: true,
	api.CommandWithdrawAmendment: true, api.CommandCloseOrderRemainder: true, api.CommandCloseOrder: true,
	api.CommandRecordSignedOrderDocument: true}

// cited returns the sources cmd cites as sent, and whether they are instead those of the customer
// response its stored result names (quote.response.record and quote.response.verify).
func cited(cmd api.Command) (ids []string, ofResponse bool) {
	ev := func(es ...[]api.Evidence) []string {
		var out []string
		for _, list := range es {
			for _, e := range list {
				out = append(out, e.SourceID)
			}
		}
		return out
	}
	switch c := cmd.(type) {
	case api.SaveDraft:
		return quoteData(c.Data).Sources(), false
	case api.DiscardDraft:
		return ev(c.Evidence), false
	case api.DecideReview:
		if m := c.MarginException; m != nil {
			return ev(c.Evidence, m.Evidence), false
		}
		return ev(c.Evidence), false
	case api.RevokeApproval:
		return ev(c.Evidence), false
	case api.Withdraw:
		return ev(c.Evidence), false
	case api.Cancel:
		return ev(c.Evidence), false
	case api.InvalidateAcceptance:
		return ev(c.Evidence), false
	case api.DisputeAcceptance:
		return ev(c.Evidence), false
	case api.DecideOrderReview:
		return ev(c.Evidence), false
	case api.RevokeOrderApproval:
		return ev(c.Evidence), false
	case api.CancelOrder:
		return ev(c.Evidence), false
	case api.CloseOrderUnfulfilled:
		return ev(c.Evidence), false
	case api.PrepareAmendment:
		out := ev(c.Source.Evidence)
		for _, a := range c.Impact {
			out = append(out, ev(a.Evidence)...)
		}
		return out, false
	case api.DecideAmendment:
		return ev(c.Evidence), false
	case api.WithdrawAmendment:
		return ev(c.Evidence), false
	case api.CloseOrderRemainder:
		return ev(c.Evidence), false
	case api.CloseOrder:
		return ev(c.Evidence), false
	case api.RecordSignedOrderDocument:
		return ev(c.Evidence), false
	case api.RecordResponse, api.VerifyResponse:
		return nil, true
	}
	return nil, false
}

// storedSources asks EVD, before TRD's transaction, whether the caller reads every source cmd
// cites, when its key has a stored result, and carries the answer in the returned context for
// sourcesRead. Commands citing no source carry nothing. A refusal of the lookups is left to the
// ledger, which refuses the caller the same way.
func (s Service) storedSources(ctx context.Context, cmd api.Command) (context.Context, error) {
	name, key, _ := request(cmd)
	if !citing[name] {
		return ctx, nil
	}
	ids, ofResponse := cited(cmd)
	stored := false
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		act, err := postgres.LoadAction(ctx, exec, scope(ctx), name, key)
		if err != nil || act == nil {
			return err
		}
		stored = true
		if !ofResponse {
			return nil
		}
		id := act.ObjectID
		if name == api.CommandVerifyResponse {
			a, err := postgres.LoadAcceptance(ctx, exec, scope(ctx), act.ObjectID, postgres.NoLock)
			if err != nil || a == nil {
				return err
			}
			id = a.ResponseID
		}
		r, err := postgres.LoadResponse(ctx, exec, scope(ctx), id)
		if err != nil || r == nil {
			return err
		}
		ids = r.Sources()
		return nil
	})
	if _, refused := kernel.AsRejection(err); refused {
		return ctx, nil
	}
	if err != nil {
		return ctx, err
	}
	v := sourcesVerdict{command: name}
	if stored {
		readable, err := s.readEvidence(ctx, ids)
		if _, refused := kernel.AsRejection(err); refused {
			v.refusal, readable, err = err, nil, nil
		}
		if err != nil {
			return ctx, err
		}
		v.readable = true
		for _, id := range ids {
			v.readable = v.readable && readable[id]
		}
	}
	return context.WithValue(ctx, sourcesKey{}, v), nil
}

// sourcesRead decides, inside the ledger's transaction, whether a stored result of command may be
// handed to the caller: for a command that cites sources, EVD's answer in ctx says the caller reads
// them all (REFERENCE_INVALID otherwise, and when ctx carries no answer for the command).
func sourcesRead(ctx context.Context, command string) error {
	if !citing[command] {
		return nil
	}
	v, ok := ctx.Value(sourcesKey{}).(sourcesVerdict)
	if ok && v.command == command && v.refusal != nil {
		return v.refusal
	}
	if !ok || v.command != command || !v.readable {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/source_id", "The caller does not read the sources the command cited")
	}
	return nil
}
