package execution

import (
	"encoding/json"
	"slices"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/executionevidence"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// What a model learns from a refused tool call (S4 G3). The three live trials of 2026-11-18 showed a
// model that found every reference of a quote draft and was then refused thirteen times: it was
// told a code and a path, one member at a time, and nothing a turn later.
//
// A refused call now answers
//
//	{"ok":false, "code", "path", "member", "missing", "reason", "needs", "allowed"}
//
// code and path are the rejection's. member is the argument of the tool the path names, in the
// tool's own names (the contexts point at their request in their transports' names:
// /args/data/lines/0/unit_price is Data.Lines[0].UnitPrice), and missing says that the call did not
// carry it. needs and allowed are what the tool's schema states of that member — whether it is
// required, its format, the api type's words, the values it takes one of — text the model was
// sent with the tool list. reason is the refusing context's own, and only when it is one of the
// static texts of the product's source (capability.StatedReason): a reason built at run time is
// withheld, so nothing of another party, no credential and no value the caller may not read
// travels in it.
//
// The ledger of the task's calls (brant_ai.model_call) keeps the code, the path and that reason of
// every refused tool call; the next turn's facts list the last of them (refusalFacts), so a new
// turn does not repeat what the last one was refused.

// refusalNote is one refusal as the model sees it.
type refusalNote struct {
	Tool    string   `json:"tool,omitempty"` // in the facts only: the result of a call needs no name
	Code    string   `json:"code"`
	Path    string   `json:"path,omitempty"`
	Member  string   `json:"member,omitempty"`
	Missing bool     `json:"missing,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	Needs   string   `json:"needs,omitempty"`
	Allowed []string `json:"allowed,omitempty"`
	Times   int      `json:"times,omitempty"` // in the facts only: how often the same refusal was met
}

// maxRefusalFacts bounds the refusals a turn's facts carry: the last eight distinct ones (tool,
// code and path). Eight is the number of model calls of a turn by default (Runner.MaxSteps), so the
// list holds what a turn met at one refusal a call; with what each member needs it stays within a
// few thousand characters (the longest entry, config.lookup's kinds, has about 600).
const maxRefusalFacts = 8

// noteOf describes the refusal of a call of tool with arguments args (nil when they are not at
// hand: a refusal read back from the ledger).
func (t *turn) noteOf(tool string, args json.RawMessage, code, path, reason string) refusalNote {
	n := refusalNote{Code: code, Path: path, Reason: capability.StatedReason(code, reason)}
	if t.r.Gateway.Registry == nil || path == "" {
		return n
	}
	c, err := t.r.Gateway.Registry.Lookup(t.scope.Tenant, tool, "/command")
	if err != nil {
		return n
	}
	if m, ok := c.Member(path); ok {
		n.Member, n.Needs, n.Allowed = m.Path, m.Rule.Needs(), m.Rule.Allowed()
		n.Missing = args != nil && m.Missing(args)
	}
	return n
}

// refused is the refusal of call as the model sees it, and as the ledger keeps it.
func (t *turn) refused(call model.ToolCall, err error) (answer string, path, reason string) {
	return t.refusedAs(call.Name, call.Arguments, err)
}

// refusedAs is the refusal of a call of tool with arguments args: a tool call of the model, or the
// request of a proposal the platform sent.
func (t *turn) refusedAs(tool string, args json.RawMessage, err error) (answer string, path, reason string) {
	code := errorCode(err)
	if rej, ok := kernel.AsRejection(err); ok && len(rej.Fields) > 0 {
		path, reason = rej.Fields[0].Path, rej.Fields[0].Reason
	}
	n := t.noteOf(tool, args, code, path, reason)
	body := struct {
		OK bool `json:"ok"`
		refusalNote
	}{refusalNote: n}
	raw, _ := json.Marshal(body)
	if !executionevidence.RefusalPathOK(path) {
		path = "" // a path that carries a name a model made up is told to the model, not kept
	}
	return string(raw), path, n.Reason
}

// refusalFacts returns the refusals a turn's facts carry, oldest first: of the task's refused
// tool calls, those after the tool's last successful call (what was corrected is not repeated),
// the same refusal (tool, code, path) once with the number of times it was met, the last
// maxRefusalFacts of them. A read that found nothing (NOT_FOUND) is left out: it says how things
// stood, not how a call must be made, and the object may exist by now.
func (t *turn) refusalFacts(calls []executionevidence.Call) []refusalNote {
	type key struct{ tool, code, path string }
	var order []key
	notes := map[key]*refusalNote{}
	for _, c := range calls {
		if c.Kind != executionevidence.KindTool || (c.Code == kernel.CodeNotFound && t.isRead(c.ToolCommand)) {
			continue
		}
		switch c.State {
		case executionevidence.StateSucceeded:
			order = slices.DeleteFunc(order, func(k key) bool {
				if k.tool == c.ToolCommand {
					delete(notes, k)
					return true
				}
				return false
			})
		case executionevidence.StateFailed:
			k := key{c.ToolCommand, c.Code, c.RefusalPath}
			if n := notes[k]; n != nil {
				n.Times++
				order = append(slices.DeleteFunc(order, func(o key) bool { return o == k }), k)
				continue
			}
			n := t.noteOf(c.ToolCommand, nil, c.Code, c.RefusalPath, c.RefusalReason)
			n.Tool, n.Times = c.ToolCommand, 1
			notes[k] = &n
			order = append(order, k)
		}
	}
	if len(order) > maxRefusalFacts {
		order = order[len(order)-maxRefusalFacts:]
	}
	out := make([]refusalNote, 0, len(order))
	for _, k := range order {
		out = append(out, *notes[k])
	}
	return out
}
