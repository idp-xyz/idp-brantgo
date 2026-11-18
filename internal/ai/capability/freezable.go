package capability

import (
	"context"

	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// notFreezable is what an agent is told, besides the code and the path, when its quote draft is
// refused for lacking what a freeze needs. It is a static text of the product's source, so the
// refusal carries it to the model, the ledger and the next turn's facts (StatedReason).
const notFreezable = "A person cannot freeze a draft without this member, so the draft is not saved; send the whole draft with it"

// freezableDraft is the preparation of the agent's quote.save_draft (S4 G6). TRD accepts a partial
// draft, because people build one in steps, and an agent that stops after a save that succeeded is
// not told when the person's freeze is then refused: in the live trial of 2026-11-18
// (bin/ai-eval/live-20261118T105055Z, normal-1) the draft lacked the cost snapshot reference, the
// save was accepted, the model answered that the draft was saved, and quote.freeze was refused
// COST_SNAPSHOT_MISSING with nobody to tell it. An agent's draft therefore has to be one a person
// can freeze as it stands, as far as completeness goes: the draft is refused here, before anything
// reaches TRD, at the first member the freeze would miss, with the code and the path the freeze
// would answer (trdapi.MissingForFreeze, TRD's own rule published for this layer), so the model
// learns of it in the same turn and sends the draft again whole.
//
// Nothing else is judged here: not whether a reference exists or is in force, the validity window
// or the prices, which stay with TRD and the freeze. A person's quote.save_draft does not pass
// through the gateway and is unchanged.
func freezableDraft(_ context.Context, c trdapi.SaveDraft) (trdapi.SaveDraft, error) {
	err := trdapi.MissingForFreeze(c.Data)
	if err == nil {
		return c, nil
	}
	rej, ok := kernel.AsRejection(err)
	if !ok || len(rej.Fields) == 0 {
		return trdapi.SaveDraft{}, err
	}
	return trdapi.SaveDraft{}, kernel.RejectAs(rej.Code, rej.Category, rej.Fields[0].Path, notFreezable)
}
