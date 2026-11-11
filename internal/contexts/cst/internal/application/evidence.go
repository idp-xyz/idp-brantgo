package application

import (
	"context"
	"fmt"

	"go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Evidence (Context Map R64, SourceRef v1). Every source a command cites — a verification's or a
// withdrawal's evidence, a measurement's, an assumption's — must be one the acting actor reads in
// EVD. EVD is asked before CST's transaction, in a transaction of its own (the ledger's Preparer
// step), so an actor without EVD's read permission is PERMISSION_DENIED there, before CST judges
// the command; a command that cites no source does not ask and needs no EVD permission. Inside the
// transaction a source the actor does not read is REFERENCE_INVALID at the entry's source_id, after
// the command's state checks, in the order of the S1 contexts and SUP. Duplicate entries are
// allowed, as the contract allows them; each is judged.

// sources asks EVD which of ids the caller reads.
func (s Service) sources(ctx context.Context, ids []string) (map[string]bool, error) {
	readable := map[string]bool{}
	if len(ids) == 0 {
		return readable, nil
	}
	refs, err := s.Upstream.Sources.ResolveSources(ctx, ids)
	for id := range refs {
		readable[id] = true
	}
	return readable, err
}

// cited returns the source IDs of es.
func cited(es []api.Evidence) []string {
	ids := make([]string, len(es))
	for i, e := range es {
		ids[i] = e.SourceID
	}
	return ids
}

// readable refuses the first entry of es, at path, whose source the caller does not read. A missing
// origin and one outside the caller's data scope are refused alike, so the refusal reveals nothing
// (Python _source_ref).
func readable(es []api.Evidence, sources map[string]bool, path string) error {
	for i, e := range es {
		if !sources[e.SourceID] {
			return kernel.Reject(kernel.CodeReferenceInvalid, fmt.Sprintf("%s/%d/source_id", path, i), "Source is not accessible")
		}
	}
	return nil
}
