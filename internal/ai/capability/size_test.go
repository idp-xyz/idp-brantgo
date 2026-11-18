package capability

import (
	"os"
	"slices"
	"testing"
)

// clerkTools are the capabilities a sales clerk delegates to the Q01 agent (the evaluation's
// delegation and the three live trials of 2026-11-18): what one model call is sent.
var clerkTools = []string{"source.register", "file.register", "rfq.save_draft", "quote.save_draft", "quote.revise",
	"quote.metadata.update", "quote.response.record", "order.prepare", "order.review.request",
	"rfq.get", "cost_snapshot.list", "costing_context.get", "config.lookup", "sku.get", "party.get",
	"quote.get", "quote.response.get", "order.get", "source.get"}

// The tool list is sent with every model call, so its size is a budget (S4 G3: the live model was
// sent 5,800 to 9,400 input tokens a call, most of them schemas). While the schemas were structural
// only, the names, descriptions and schemas of these 19 tools were 14,767 bytes (the tools file of
// the live trace of 3185097). Stating the required members, the allowed values, the kinds and the
// formats costs about a quarter more, after what was cut for it: minLength on every string, the
// upper bounds of lists, the sentences of the read descriptions that repeat the result's own
// "omitted", and most of rfq.save_draft's description, which the members now carry. The ceiling
// keeps a later member or word from growing the list unnoticed.
const toolListCeiling = 18600

func TestToolListStaysWithinItsSize(t *testing.T) {
	reg, err := Q01Registry(Q01Targets{})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, c := range reg.Capabilities() {
		if !slices.Contains(clerkTools, c.Command) {
			continue
		}
		raw, err := c.Schema()
		if err != nil {
			t.Fatal(err)
		}
		n := len(c.Command) + len(c.Summary) + len(raw)
		total += n
		t.Logf("%-24s description %4d  schema %5d", c.Command, len(c.Summary), len(raw))
		if os.Getenv("BRANT_SHOW_TOOL") == c.Command {
			// go test -v -run TestToolListStaysWithinItsSize with BRANT_SHOW_TOOL=<command> prints
			// the tool as the model is sent it.
			t.Logf("%s\n%s\n%s", c.Command, c.Summary, raw)
		}
	}
	t.Logf("19 tools: %d bytes of names, descriptions and schemas", total)
	if total > toolListCeiling {
		t.Errorf("the tool list grew to %d bytes, above its ceiling of %d", total, toolListCeiling)
	}
}
