package capability

import (
	"context"
	"errors"
	"testing"

	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
)

// A preparation completes a decoded command before it is hashed (unit B4); it must take the
// capability's own input type, and a capability without one returns the command as it is.
func TestPreparedCompletesTheCommand(t *testing.T) {
	c := probeCapability(t, draftSpec("probe.save_draft"))
	p, err := Prepared(c, nil, func(_ context.Context, cmd probe) (probe, error) {
		cmd.Lines = append(cmd.Lines, "located")
		return cmd, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Prepare(context.Background(), probe{ID: "p.1"})
	if err != nil || len(got.(probe).Lines) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	if same, err := c.Prepare(context.Background(), probe{ID: "p.1"}); err != nil || same.(probe).Lines != nil {
		t.Fatalf("%+v %v", same, err)
	}
	if _, err := p.Prepare(context.Background(), "not a probe"); err == nil {
		t.Fatal("a value of another type was prepared")
	}
	if _, err := Prepared(c, nil, func(_ context.Context, cmd trdapi.SaveDraft) (trdapi.SaveDraft, error) { return cmd, nil }); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a preparation of another type: %v", err)
	}
	failed := errors.New("define failed")
	if _, err := Prepared(c, failed, func(_ context.Context, cmd probe) (probe, error) { return cmd, nil }); !errors.Is(err, failed) {
		t.Fatalf("%v", err)
	}
}

// Q01's rfq.save_draft (version 2) locates its lines: without an EVD reader a line placing fields in
// a source is REFERENCE_INVALID, a line without locators passes as it is (WBS S4-04b).
func TestQ01SaveRfqIsPrepared(t *testing.T) {
	reg, err := Q01Registry(Q01Targets{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := reg.Lookup("tenant.1", inqapi.CommandSaveRfq, "/command")
	if err != nil || c.Version != 2 {
		t.Fatalf("%+v %v", c.Spec, err)
	}
	plain := inqapi.SaveRfq{RfqID: "rfq.1", Lines: []inqapi.RfqLine{{LineID: "L1", SourceID: "src.1", Quantity: "1"}}}
	if got, err := c.Prepare(context.Background(), plain); err != nil || got.(inqapi.SaveRfq).Lines[0].Pending != nil {
		t.Fatalf("%+v %v", got, err)
	}
	located := plain
	located.Lines = []inqapi.RfqLine{{LineID: "L1", SourceID: "src.1", Locators: []inqapi.FieldLocator{{Field: "quantity", Locator: "1:1", Quote: "1"}}}}
	if _, err := c.Prepare(context.Background(), located); code(err) != "REFERENCE_INVALID" {
		t.Fatalf("%v", err)
	}
}
