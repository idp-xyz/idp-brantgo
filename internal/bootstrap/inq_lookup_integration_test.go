//go:build integration

package bootstrap_test

import (
	"testing"

	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// RfqLineRef v1 (Context Map R04, T02 §13.1: rfq_id + rfq_row_version + line_id): INQ answers its
// downstream contexts with an RFQ's state, row version and lines, including the SKU specification
// revisions its confirmation fixed; RFQs outside the caller's company or data scope are absent.
func TestResolveRfqsForDownstreamContexts(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	cmd := w.readyArgs(ctx)
	_, err := w.save(ctx, cmd)
	w.must(err)
	ready, err := w.confirm(ctx, "rfq.1", 1, "src.1")
	w.must(err)
	got, err := w.c.INQ.ResolveRfqs(ctx, []string{"rfq.1", "rfq.none", "rfq.1"})
	w.must(err)
	r, ok := got["rfq.1"]
	if len(got) != 1 || !ok || r.Status != inqapi.StatusReady || r.RowVersion != ready.RowVersion || r.LinesHash == "" ||
		len(r.Lines) != 1 || r.Lines[0].LineID != "L1" || r.Lines[0].SkuID != "sku.1" || r.Lines[0].SkuSpecRevision != 1 ||
		r.Lines[0].Quantity != "120" {
		t.Fatalf("resolved %+v", got)
	}
	other := w.actor("human.2", "inq.read") // own data scope: another actor's RFQ is not seen
	if theirs, err := w.c.INQ.ResolveRfqs(other, []string{"rfq.1"}); err != nil || len(theirs) != 0 {
		t.Fatalf("own data scope: %+v %v", theirs, err)
	}
	if _, err = w.c.INQ.ResolveRfqs(ctx, []string{"not an id"}); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("a malformed ID: %s", code(err))
	}
	if none, err := w.c.INQ.ResolveRfqs(ctx, nil); err != nil || len(none) != 0 {
		t.Fatalf("nothing asked: %+v %v", none, err)
	}
}
