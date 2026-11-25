package application

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
)

// guardSampleApprovals calls CAT's SampleGuard v1 for every line of an apparel quote that names a sample
// stage. The caller has already locked the quote root FOR UPDATE and the authority rows FOR SHARE,
// and has called the apparel plugin guard. Lines are checked in (SKU, stage, specification) order
// so two freezes lock the approval rows in the same order. A line with no stage is not a sample
// requirement: a generic quote never reaches here, and an apparel quote that names no stage still
// freezes.
func guardSampleApprovals(ctx context.Context, exec bentopg.Executor, d quote.Data, prefix string) error {
	type named struct {
		index int
		ref   catapi.SampleRef
	}
	var rows []named
	for i, l := range d.Lines {
		if l.SampleStage == "" {
			continue
		}
		rows = append(rows, named{i, catapi.SampleRef{SkuID: l.Sku.SkuID, SpecRevision: l.Sku.SpecRevision, Stage: l.SampleStage}})
	}
	slices.SortFunc(rows, func(a, b named) int {
		return cmp.Or(cmp.Compare(a.ref.SkuID, b.ref.SkuID), cmp.Compare(a.ref.Stage, b.ref.Stage),
			cmp.Compare(a.ref.SpecRevision, b.ref.SpecRevision), cmp.Compare(a.index, b.index))
	})
	for _, row := range rows {
		v, err := catapi.ScanSampleGuard(row.ref, exec.QueryRow(ctx, catapi.SampleGuardSQL, catapi.SampleGuardArgs(row.ref)...).Scan)
		if err != nil {
			return err
		}
		if err = v.Check(fmt.Sprintf("%s/lines/%d/sample_stage", prefix, row.index)); err != nil {
			return err
		}
	}
	return nil
}
