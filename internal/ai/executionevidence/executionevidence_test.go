package executionevidence

import (
	"errors"
	"strings"
	"testing"

	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func profile() ModelProfile {
	return ModelProfile{ProfileID: "p.v1", Provider: "openaicompat", Model: "m", MaxOutputTokens: 1024, Currency: "USD", MinorUnits: 2,
		InputPerMTok: "4", OutputPerMTok: "20", ReservePerCall: "0.85"}
}

func bundle() Bundle {
	return Bundle{BundleID: "q01.agent", Application: Application, TaskProfile: "q01.v1", Goals: []string{"quote_ready@1", "order_active@1"},
		Model: profile(), PromptSHA256: SHA256Hex([]byte("prompt")), Canonicalization: Canonicalization,
		Tools: []ToolRef{
			{Command: "rfq.save_draft", Context: "inq", Contract: "brant.inq/0.1.0", Version: 1, SideEffect: "draft", Approval: "none", SchemaSHA256: SHA256Hex([]byte("a"))},
			{Command: "evd.source", Context: "evd", Contract: "brant.evd/0.1.0", Version: 1, SideEffect: "draft", Approval: "none", SchemaSHA256: SHA256Hex([]byte("b"))},
		},
		DefaultBudget: BudgetSpec{MaxModelCalls: 40, MaxRetries: 3, Currency: "USD", MinorUnits: 2, MaxCost: "3.00"}}
}

// Baseline 17.3: a bundle's hash covers everything an execution depends on that may change under it
// — the prompt template, a tool's schema, the model, the default budget — and does not depend on
// the order its goals and tools are listed in.
func TestBundleHashCoversWhatTheExecutionDependsOn(t *testing.T) {
	base, err := bundle().Ref()
	if err != nil {
		t.Fatal(err)
	}
	reordered := bundle()
	reordered.Goals[0], reordered.Goals[1] = reordered.Goals[1], reordered.Goals[0]
	reordered.Tools[0], reordered.Tools[1] = reordered.Tools[1], reordered.Tools[0]
	if r, _ := reordered.Ref(); r != base {
		t.Fatalf("order changed the hash: %v vs %v", r, base)
	}
	changes := map[string]func(*Bundle){
		"prompt":  func(b *Bundle) { b.PromptSHA256 = SHA256Hex([]byte("prompt 2")) },
		"schema":  func(b *Bundle) { b.Tools[0].SchemaSHA256 = SHA256Hex([]byte("c")) },
		"version": func(b *Bundle) { b.Tools[1].Version = 2 },
		"model":   func(b *Bundle) { b.Model.Model = "m2" },
		"price":   func(b *Bundle) { b.Model.OutputPerMTok = "21" },
		"budget":  func(b *Bundle) { b.DefaultBudget.MaxRetries = 2 },
		"goal":    func(b *Bundle) { b.Goals = []string{"quote_ready@2", "order_active@1"} },
		"app":     func(b *Bundle) { b.Application = "brant.ai.execution/0.2.0" },
	}
	for name, change := range changes {
		b := bundle()
		change(&b)
		r, err := b.Ref()
		if err != nil || r == base || r.BundleID != base.BundleID {
			t.Errorf("%s: %v %v", name, r, err)
		}
	}
	invalid := map[string]func(*Bundle){
		"id":       func(b *Bundle) { b.BundleID = "" },
		"goal":     func(b *Bundle) { b.Goals = []string{"quote_ready"} },
		"no goals": func(b *Bundle) { b.Goals = nil },
		"prompt":   func(b *Bundle) { b.PromptSHA256 = "x" },
		"tools":    func(b *Bundle) { b.Tools = nil },
		"tool":     func(b *Bundle) { b.Tools[0].SchemaSHA256 = "" },
		"provider": func(b *Bundle) { b.Model.Provider = "Open AI" },
		"price":    func(b *Bundle) { b.Model.InputPerMTok = "-1" },
		"float":    func(b *Bundle) { b.Model.InputPerMTok = "1e3" },
		"budget":   func(b *Bundle) { b.DefaultBudget.MaxModelCalls = 0 },
		"cost":     func(b *Bundle) { b.DefaultBudget.MaxCost = "3.001" },
	}
	for name, change := range invalid {
		b := bundle()
		change(&b)
		if _, err := b.Ref(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Baseline 17.4 and the money rule: a call's cost is exact until one HALF_UP rounding to the
// budget's minor units; a free profile costs zero in any currency, a priced one only in its own.
func TestCostIsExactUntilOneRounding(t *testing.T) {
	usd := kernel.MustCurrency("USD", 2)
	p := profile()
	cases := []struct {
		in, out int64
		want    string
	}{
		{0, 0, "0"},
		{85_000, 25_000, "84"}, // 0.34 + 0.50
		{1_249, 0, "0"},        // 0.004996 → 0.00
		{1_250, 0, "1"},        // 0.005 → 0.01 (HALF_UP)
		{0, 250, "1"},          // 0.005 → 0.01
		{1_000_000, 1_000_000, "2400"},
	}
	for _, c := range cases {
		m, err := p.Cost(usd, c.in, c.out)
		if err != nil || m.MinorString() != c.want || m.Currency() != usd {
			t.Errorf("%d/%d: %v %v, want %s", c.in, c.out, m, err, c.want)
		}
	}
	if r, err := p.Reservation(usd); err != nil || r.MinorString() != "85" {
		t.Fatalf("reservation %v %v", r, err)
	}
	cny := kernel.MustCurrency("CNY", 2)
	if _, err := p.Cost(cny, 1, 1); !errors.Is(err, ErrCurrency) {
		t.Fatalf("another currency: %v", err)
	}
	if _, err := p.Cost(usd, -1, 0); err == nil {
		t.Fatal("negative tokens")
	}
	free := p
	free.InputPerMTok, free.OutputPerMTok, free.ReservePerCall = "0", "0", "0.00"
	if m, err := free.Cost(cny, 1_000_000, 1_000_000); err != nil || !m.IsZero() || m.Currency() != cny || !free.Free() {
		t.Fatalf("free %v %v", m, err)
	}
}

// A task's own budget wins over the runner's default (task.create budget, baseline 17.4); without a
// cost cap the default's currency records the cost.
func TestLimitsOfATask(t *testing.T) {
	def, err := bundle().DefaultBudget.Limits("runner")
	if err != nil || def.MaxModelCalls != 40 || def.MaxCost == nil || def.MaxCost.MinorString() != "300" || def.Source != "runner" {
		t.Fatalf("default %+v %v", def, err)
	}
	if l, err := LimitsOf(nil, def); err != nil || l.Source != "runner" {
		t.Fatalf("%+v %v", l, err)
	}
	l, err := LimitsOf(&tskapi.Budget{MaxModelCalls: 5, MaxRetries: 1, MaxCost: &tskapi.CostCap{Currency: "CNY", MinorUnits: 2, Amount: "20.50"}}, def)
	if err != nil || l.MaxModelCalls != 5 || l.MaxRetries != 1 || l.MaxCost.String() != "20.50 CNY" || l.Currency.Code() != "CNY" || l.Source != "task" {
		t.Fatalf("%+v %v", l, err)
	}
	l, err = LimitsOf(&tskapi.Budget{MaxModelCalls: 5}, def)
	if err != nil || l.MaxCost != nil || l.Currency.Code() != "USD" {
		t.Fatalf("%+v %v", l, err)
	}
	if _, err = LimitsOf(&tskapi.Budget{MaxModelCalls: 0}, def); err == nil {
		t.Fatal("a budget without model calls")
	}
}

func TestReservationsAndOutcomesAreChecked(t *testing.T) {
	p := profile()
	ref := Ref{BundleID: "q01.agent", SHA256: SHA256Hex([]byte("b"))}
	ok := Reservation{TaskID: "t.1", TaskVersion: 1, Kind: KindModel, Bundle: ref, Profile: &p, PromptSHA: SHA256Hex([]byte("p"))}
	if err := ok.check(); err != nil {
		t.Fatal(err)
	}
	bad := []Reservation{
		{TaskID: "", TaskVersion: 1, Kind: KindModel, Bundle: ref, Profile: &p, PromptSHA: ok.PromptSHA},
		{TaskID: "t.1", TaskVersion: 0, Kind: KindModel, Bundle: ref, Profile: &p, PromptSHA: ok.PromptSHA},
		{TaskID: "t.1", TaskVersion: 1, Kind: KindModel, Bundle: ref, PromptSHA: ok.PromptSHA},
		{TaskID: "t.1", TaskVersion: 1, Kind: KindModel, Bundle: ref, Profile: &p},
		{TaskID: "t.1", TaskVersion: 1, Kind: KindTool, Bundle: ref},
		{TaskID: "t.1", TaskVersion: 1, Kind: KindTool, Bundle: ref, Profile: &p, ToolCommand: "x.y"},
		{TaskID: "t.1", TaskVersion: 1, Kind: "other", Bundle: ref, ToolCommand: "x.y"},
		{TaskID: "t.1", TaskVersion: 1, Kind: KindTool, Bundle: Ref{BundleID: "b"}, ToolCommand: "x.y"},
	}
	for i, r := range bad {
		if err := r.check(); !errors.Is(err, ErrInvalid) {
			t.Errorf("reservation %d: %v", i, err)
		}
	}
	zero, _ := kernel.NewMoney(kernel.MustCurrency("USD", 2), 0)
	neg, _ := kernel.NewMoney(kernel.MustCurrency("USD", 2), -1)
	outcomes := map[string]Outcome{
		"known without cost":   {State: StateSucceeded},
		"unknown with cost":    {State: StateUnknown, Cost: &zero},
		"failed without code":  {State: StateFailed, Cost: &zero},
		"reserved":             {State: StateReserved, Cost: &zero},
		"negative":             {State: StateSucceeded, Cost: &neg},
		"unavailable, no code": {State: StateUnavailable, Cost: &zero},
	}
	for name, o := range outcomes {
		if err := o.check(); err == nil || !strings.Contains(err.Error(), "outcome") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if CallID("t.1", 1) == CallID("t.1", 2) || CallID("t.1", 1) == CallID("t.2", 1) || !kernel.ValidIdentifier(CallID("t.1", 1)) {
		t.Fatal("call IDs")
	}
}
