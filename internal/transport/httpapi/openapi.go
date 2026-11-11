package httpapi

import (
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/foundation/strictjson"
)

// OpenAPIPath is where the generated candidate OpenAPI 3.1 document is kept.
const OpenAPIPath = "contracts/openapi/brant-api.openapi.json"

// OpenAPI returns the candidate OpenAPI 3.1 document of the HTTP API, generated from the command
// tables and request structures the transport decodes, so it cannot drift from the code. Field
// formats (identifier patterns, lengths, decimal strings) and which fields a command requires are
// checked by the contexts' api packages; the document states the closed shape, the routes and the
// answers.
func OpenAPI() ([]byte, error) {
	sets := []*commandSet{evdCommands(struct{ EVD }{}), ptyCommands(struct{ PTY }{}), catCommands(struct{ CAT }{}),
		inqCommands(struct{ INQ }{}), polCommands(struct{ POL }{}), refCommands(struct{ REF }{}),
		supCommands(struct{ SUP }{}), cstCommands(struct{ CST }{}),
		trdCommands(struct{ TRD }{}), purCommands(struct{ PUR }{})}
	paths := map[string]any{}
	for _, set := range sets {
		names := make([]string, 0, len(set.commands))
		for n := range set.commands {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			args := schemaOf(set.commands[n].args)
			if set.bc == "pol" && n == "config.save_draft" {
				types := configDataTypes()
				kinds := make([]string, 0, len(types))
				for k := range types {
					kinds = append(kinds, k)
				}
				sort.Strings(kinds)
				var oneOf []any
				for _, k := range kinds {
					oneOf = append(oneOf, map[string]any{"title": k, "$ref": "#/components/schemas/pol." + k})
				}
				args["properties"].(map[string]any)["data"] = map[string]any{"description": "The closed structure of the configuration kind named by kind.", "oneOf": oneOf}
			}
			limit, values := MaxBody, strictjson.DefaultMaxValues
			if l := set.limits[n]; l.MaxBytes > int64(limit) {
				limit, values = int(l.MaxBytes), max(l.MaxValues, values)
			}
			ingress := "Request bodies are at most " + bytesText(limit) + " and " + strconv.Itoa(values) + " JSON values; duplicate keys, null, numbers that are not integers, unknown or case-variant members and empty strings are refused (SCHEMA_INVALID). Actor, tenant and company come from the bearer token."
			body := map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"contract", "command", "idempotency_key", "args"},
				"properties": map[string]any{
					"contract":        map[string]any{"const": set.contract},
					"command":         map[string]any{"const": n},
					"idempotency_key": map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$"},
					"args":            args,
				}}}}}
			paths["/api/brant/v1/"+set.bc+"/commands/"+n] = map[string]any{"post": map[string]any{
				"operationId": set.bc + "." + n,
				"tags":        []string{set.bc},
				"summary":     n,
				"description": ingress,
				"requestBody": body,
				"responses":   commandResponses(),
			}}
			if set.reconcilable(n) {
				paths["/api/brant/v1/"+set.bc+"/commands/"+n+"/reconcile"] = map[string]any{"post": map[string]any{
					"operationId": set.bc + "." + n + ".reconcile",
					"tags":        []string{set.bc},
					"summary":     "What became of " + n + " sent with this idempotency key (动作对账)",
					"description": "The body is the request exactly as it was sent. Nothing is executed; the context's read permission and the caller's data scope are checked now. " + ingress,
					"requestBody": body,
					"responses":   reconcileResponses(),
				}}
			}
		}
	}
	for _, r := range reads {
		get := map[string]any{"operationId": r.id, "tags": []string{r.tag}, "summary": r.summary, "responses": readResponses()}
		var params []any
		for _, p := range r.params {
			params = append(params, map[string]any{"name": p, "in": "path", "required": true, "schema": map[string]any{"type": "string"}})
		}
		for _, p := range readQuery[r.path] {
			params = append(params, map[string]any{"name": p, "in": "query", "required": false, "schema": map[string]any{"type": "integer", "minimum": 0}})
		}
		if params != nil {
			get["parameters"] = params
		}
		method := "get"
		if r.post {
			method = "post"
		}
		paths[r.path] = map[string]any{method: get}
	}
	paths["/healthz"] = map[string]any{"get": map[string]any{"operationId": "healthz", "security": []any{}, "responses": map[string]any{
		"200": map[string]any{"description": "The process serves requests."}}}}
	schemas := map[string]any{
		"Receipt":        schemaOf(reflect.TypeFor[receipt]()),
		"Reconciliation": schemaOf(reflect.TypeFor[reconciliation]()),
		"Rejection":      schemaOf(reflect.TypeFor[rejection]()),
		"TransportError": schemaOf(reflect.TypeFor[transportError]()),
	}
	for k, t := range configDataTypes() {
		schemas["pol."+k] = schemaOf(t)
	}
	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{"title": "IDP Brant API", "version": "0.1.0-candidate",
			"description": "Candidate contract generated from internal/transport/httpapi (S1). Not a published interface."},
		"security": []any{map[string]any{"bearer": []any{}}},
		"paths":    paths,
		"components": map[string]any{
			"schemas":         schemas,
			"securitySchemes": map[string]any{"bearer": map[string]any{"type": "http", "scheme": "bearer", "bearerFormat": "JWT (OIDC access token, typ at+jwt)"}},
		},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	return append(b, '\n'), err
}

type readRoute struct {
	path, id, tag, summary string
	params                 []string
	post                   bool
}

var reads = []readRoute{
	{"/api/brant/v1/{bc}/audit", "audit", "audit", "The audit trail of one context in the caller's scope", []string{"bc"}, false},
	{"/api/brant/v1/evd/sources/{id}", "evd.source.get", "evd", "A source with its content and imports", []string{"id"}, false},
	{"/api/brant/v1/evd/files/{id}", "evd.file.get", "evd", "A business file with its bytes in Base64", []string{"id"}, false},
	{"/api/brant/v1/pty/parties/{id}", "pty.party.get", "pty", "A party and its confirmed revisions", []string{"id"}, false},
	{"/api/brant/v1/pty/parties/{id}/screening", "pty.party_screening.get", "pty",
		"A party's screening: its status now as the guard decides it, every conclusion, positive and negative list entries", []string{"id"}, false},
	{"/api/brant/v1/pty/screenings/due", "pty.party_screening.due", "pty",
		"Confirmed parties whose screening is due at ?at= (a UTC timestamp, now by default)", nil, false},
	{"/api/brant/v1/pty/parties/{id}/trading_profile", "pty.trading_profile.get", "pty",
		"A party's trading profile in the caller's company: every revision with its terms, owner and pinned contacts", []string{"id"}, false},
	{"/api/brant/v1/pty/contacts/{id}", "pty.contact.get", "pty", "A contact", []string{"id"}, false},
	{"/api/brant/v1/cat/skus/{id}", "cat.sku.get", "cat", "A SKU and its specification revisions", []string{"id"}, false},
	{"/api/brant/v1/cat/item_code_mappings/{id}", "cat.item_code_mapping.get", "cat",
		"An item-code mapping: the party's code, every revision with its SKU specification and publication", []string{"id"}, false},
	{"/api/brant/v1/inq/rfqs/{id}", "inq.rfq.get", "inq", "A customer RFQ", []string{"id"}, false},
	{"/api/brant/v1/pol/configs/{kind}/{id}", "pol.config.get", "pol", "A configuration and its revisions", []string{"kind", "id"}, false},
	{"/api/brant/v1/pol/snapshots/{id}", "pol.snapshot.get", "pol", "A foundation snapshot", []string{"id"}, false},
	{"/api/brant/v1/sup/supplier_rfqs/{id}", "sup.supplier_rfq.get", "sup", "A supplier RFQ", []string{"id"}, false},
	{"/api/brant/v1/sup/supplier_offers/{id}", "sup.supplier_offer.get", "sup", "A supplier offer and every revision", []string{"id"}, false},
	{"/api/brant/v1/sup/purchase_terms/{id}", "sup.purchase_terms.get", "sup", "Purchase terms and every revision", []string{"id"}, false},
	{"/api/brant/v1/sup/logistics_offers/{id}", "sup.logistics_offer.get", "sup", "A logistics offer and every revision", []string{"id"}, false},
	{"/api/brant/v1/cst/costing_contexts/{id}", "cst.costing_context.get", "cst", "A costing context and every revision", []string{"id"}, false},
	{"/api/brant/v1/cst/measurements/{id}", "cst.measurement.get", "cst", "A measurement and every revision", []string{"id"}, false},
	{"/api/brant/v1/cst/cost_scenarios/{id}", "cst.cost_scenario.get", "cst", "A cost scenario: its draft and the summaries of its evaluations", []string{"id"}, false},
	{"/api/brant/v1/cst/cost_evaluations/{id}", "cst.cost_evaluation.get", "cst",
		"One evaluation with its normalized input and result; access to every bound input is re-checked", []string{"id"}, false},
	{"/api/brant/v1/cst/cost_snapshots/{id}", "cst.cost_snapshot.get", "cst",
		"A frozen cost snapshot with its document, inputs and the current standing of its sources", []string{"id"}, false},
	{"/api/brant/v1/cst/cost_snapshots/{id}/adoptability", "cst.cost_snapshot.query", "cst",
		"Whether the snapshot may be adopted now, as AdoptabilityGuard v1 decides it (Python snapshot_query)", []string{"id"}, false},
	{"/api/brant/v1/cst/source_attestations/{id}", "cst.source_attestation.get", "cst", "A source attestation or denial", []string{"id"}, false},
	{"/api/brant/v1/trd/quotes/{id}", "trd.quote.get", "trd",
		"A quote: root versions, object control, every revision with its calculation, reviews, approvals, holds and the conversion slot", []string{"id"}, false},
	{"/api/brant/v1/trd/quotes/{id}/active", "trd.quote.active.get", "trd", "The quote's active formal revision", []string{"id"}, false},
	{"/api/brant/v1/trd/quotes/{id}/approvals/{approval_id}/check", "trd.quote.approval.check", "trd",
		"Whether an approval releases its revision now (a refusal is a code in the answer, not an error)", []string{"id", "approval_id"}, false},
	{"/api/brant/v1/trd/quotes/{id}/revisions/{revision}/document", "trd.quote.document.get", "trd",
		"A formal quote document: its binding and bytes (Base64), verified on read", []string{"id", "revision"}, false},
	{"/api/brant/v1/trd/quotes/{id}/ready", "trd.quote.ready", "trd",
		"Whether a revision (?business_revision=, the active one by default) serves the next step now: every check that fails is a reason (quote_ready)", []string{"id"}, false},
	{"/api/brant/v1/trd/quotes/{id}/history", "trd.quote.history", "trd", "A page of the quote's revisions with their events (?after=, ?limit=)", []string{"id"}, false},
	{"/api/brant/v1/trd/quotes/{id}/approvals", "trd.quote.approval.history", "trd",
		"A page of the quote's review requests and approvals with their effectivity events (?after=, ?limit=)", []string{"id"}, false},
	{"/api/brant/v1/trd/quotes/{id}/documents", "trd.quote.document.history", "trd",
		"A page of the quote's formal documents with their integrity now (?after=, ?limit=)", []string{"id"}, false},
	{"/api/brant/v1/trd/quotes/{id}/commits", "trd.quote.commit.history", "trd", "A page of the quote's commit evidence (?after=, ?limit=)", []string{"id"}, false},
	{"/api/brant/v1/trd/quotes/{id}/outbox", "trd.quote.outbox.history", "trd",
		"A page of the Outbox events of the quote, its responses and acceptances with their relay state; external delivery is not started (?after=, ?limit=)", []string{"id"}, false},
	{"/api/brant/v1/trd/quotes/{id}/revisions/{revision}/dispatch_preflight", "trd.quote.dispatch.preflight", "trd",
		"What must hold before the formal document of a revision is sent to the customer, checked now", []string{"id", "revision"}, false},
	{"/api/brant/v1/trd/quotes/{id}/revisions/{revision}/dispatch_target", "trd.quote.dispatch.target", "trd",
		"The recipient snapshot of a revision and whether a delivery channel serves it (none is configured)", []string{"id", "revision"}, false},
	{"/api/brant/v1/trd/orders/{id}", "trd.order.get", "trd",
		"A sales order: its binding, the exact snapshot bytes (verified on read), reviews with acknowledged risks, commit evidence and closure", []string{"id"}, false},
	{"/api/brant/v1/trd/orders/{id}/check", "trd.order.check", "trd",
		"The checks order.activate would make now on ?review_id= (none: the source and upstream risks only); a refusal is the answer", []string{"id"}, false},
	{"/api/brant/v1/trd/customer_responses/{id}", "trd.customer_response.get", "trd", "A customer response and its acceptances", []string{"id"}, false},
	{"/api/brant/v1/trd/acceptances/{id}", "trd.acceptance.get", "trd", "An acceptance and its effectivity history", []string{"id"}, false},
	{"/api/brant/v1/pur/purchase_drafts/{id}", "pur.purchase_draft.get", "pur",
		"A purchase draft: its state now (prepared, supply_expired or voided), the exact document its hash covers, lines, totals and void; the caller's access to the order and every supply condition is re-checked", []string{"id"}, false},
	{"/api/brant/v1/pur/orders/{id}/allocations", "pur.demand_allocation.get", "pur",
		"The demand allocation of every drafted line of a sales order: demand, allocated, remaining and the drafts holding it", []string{"id"}, false},
	{"/api/brant/v1/ref/screening_lists/{id}", "ref.screening_list.get", "ref", "A screening list and the versions the company imported", []string{"id"}, false},
	{"/api/brant/v1/ref/screening_lists/{id}/versions/{version}", "ref.screening_list_version.get", "ref",
		"One imported version of a screening list with its entries", []string{"id", "version"}, false},
	{"/api/brant/v1/msg/dead_letters/{source}/{event_id}/replay", "msg.dead_letter.replay", "msg",
		"Republish a dead letter with its original event ID (human holding dead_letter.replay; enabled with -replay-sink)", []string{"source", "event_id"}, true},
}

// readQuery names the optional integer query parameters of a read.
var readQuery = map[string][]string{
	"/api/brant/v1/trd/quotes/{id}/ready":     {"business_revision"},
	"/api/brant/v1/trd/quotes/{id}/history":   {"after", "limit"},
	"/api/brant/v1/trd/quotes/{id}/approvals": {"after", "limit"},
	"/api/brant/v1/trd/quotes/{id}/documents": {"after", "limit"},
	"/api/brant/v1/trd/quotes/{id}/commits":   {"after", "limit"},
	"/api/brant/v1/trd/quotes/{id}/outbox":    {"after", "limit"},
}

func commandResponses() map[string]any {
	ref := func(name, desc string) map[string]any {
		return map[string]any{"description": desc, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/" + name}}}}
	}
	out := readResponses()
	out["200"] = ref("Receipt", "Committed, or the stored result of the same idempotency key (replayed: true).")
	out["400"] = ref("Rejection", "UNSUPPORTED_CONTRACT")
	out["409"] = ref("Rejection", "VERSION_CONFLICT, IDEMPOTENCY_PAYLOAD_CONFLICT or a context's conflict code")
	out["413"] = ref("Rejection", "REQUEST_TOO_LARGE")
	return out
}

func reconcileResponses() map[string]any {
	out := readResponses()
	out["200"] = map[string]any{"description": "committed (with the stored receipt), payload_conflict (the key committed another request) or not_committed (the caller may send the command again with the same key).",
		"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Reconciliation"}}}}
	out["400"] = commandResponses()["400"]
	out["413"] = commandResponses()["413"]
	return out
}

func readResponses() map[string]any {
	ref := func(name, desc string) map[string]any {
		return map[string]any{"description": desc, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/" + name}}}}
	}
	return map[string]any{
		"200": map[string]any{"description": "The object as the caller may see it."},
		"401": ref("Rejection", "UNAUTHENTICATED: no valid bearer token"),
		"403": ref("Rejection", "PERMISSION_DENIED"),
		"404": ref("Rejection", "NOT_FOUND: missing or outside the caller's data scope"),
		"422": ref("Rejection", "SCHEMA_INVALID or a business rejection"),
		"503": ref("TransportError", "UNAVAILABLE or COMMIT_UNCERTAIN: reconcile the command, or replay it with the same idempotency key"),
	}
}

// schemaOf describes a request or response structure as JSON Schema.
func schemaOf(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == reflect.TypeFor[json.RawMessage]():
		return map[string]any{"type": "object"}
	case t == reflect.TypeFor[time.Time]():
		return map[string]any{"type": "string", "format": "date-time"}
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string", "minLength": 1}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int32, reflect.Int64, reflect.Uint32:
		return map[string]any{"type": "integer"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": schemaOf(t.Elem())}
	case reflect.Struct:
		props := map[string]any{}
		var required []string
		for f := range t.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" || !f.IsExported() {
				continue
			}
			props[name] = schemaOf(f.Type)
			if name == "id" || (name == "expected_version" && f.Type.Kind() == reflect.Pointer) {
				required = append(required, name)
			}
		}
		out := map[string]any{"type": "object", "additionalProperties": false, "properties": props}
		if required != nil {
			out["required"] = required
		}
		return out
	}
	return map[string]any{}
}

func bytesText(n int) string {
	if n%(1<<20) == 0 {
		return strconv.Itoa(n>>20) + " MiB"
	}
	return strconv.Itoa(n) + " bytes"
}
