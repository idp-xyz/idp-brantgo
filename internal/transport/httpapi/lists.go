package httpapi

import (
	"net/http"
	"reflect"
	"slices"
	"strconv"

	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The list reads (S7-06 read models): GET /api/brant/v1/<bc>/<objects>[?<filter>=][&after=][&limit=].
// Each returns a page of the objects the caller sees, most recently changed first
// (kernel.ListCursor), with "next": the after of the following page, absent on the last. The
// context validates the filter values; the transport refuses unknown and repeated parameters.

// listParams reads the query of a list read: the named text filters (absent ones are empty), after
// and limit.
func listParams(r *http.Request, filters ...string) (map[string]string, int, error) {
	q := r.URL.Query()
	for k, v := range q {
		if !slices.Contains(filters, k) && k != "after" && k != "limit" {
			return nil, 0, kernel.Reject(kernel.CodeSchemaInvalid, "/"+k, "unknown query parameter")
		}
		if len(v) != 1 || v[0] == "" {
			return nil, 0, kernel.Reject(kernel.CodeSchemaInvalid, "/"+k, "one non-empty value")
		}
	}
	limit := 0
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > kernel.MaxListPage {
			return nil, 0, kernel.Reject(kernel.CodeSchemaInvalid, "/limit", "1 to "+strconv.Itoa(kernel.MaxListPage))
		}
		limit = n
	}
	out := map[string]string{"after": q.Get("after")}
	for _, f := range filters {
		out[f] = q.Get(f)
	}
	return out, limit, nil
}

// flag reads a filter that is either absent or "true".
func flag(p map[string]string, name string) (bool, error) {
	switch p[name] {
	case "":
		return false, nil
	case "true":
		return true, nil
	}
	return false, kernel.Reject(kernel.CodeSchemaInvalid, "/"+name, "true, or absent")
}

// listRoute describes a list read for the OpenAPI document: its text filters, the filters that
// are flags (absent or "true"), and the type of its answer, whose tagged form is the 200 response.
// repeated names a parameter that is given once per value, up to max times, on a read that is not
// paged (no after, no limit).
type listRoute struct {
	filters  []string
	flags    []string
	page     reflect.Type
	repeated string
	max      int
}

var listRoutes = map[string]listRoute{
	"/api/brant/v1/pty/parties":  {filters: []string{"status", "role", "q"}, page: reflect.TypeFor[ptyapi.PartyPage]()},
	"/api/brant/v1/pty/contacts": {filters: []string{"party_id", "status", "q"}, page: reflect.TypeFor[ptyapi.ContactPage]()},
	"/api/brant/v1/cat/skus":     {filters: []string{"status", "q"}, page: reflect.TypeFor[catapi.SkuPage]()},
	"/api/brant/v1/inq/rfqs":     {filters: []string{"status", "customer_id"}, page: reflect.TypeFor[inqapi.RfqPage]()},
	"/api/brant/v1/trd/quotes":   {filters: []string{"status", "customer_id", "rfq_id"}, flags: []string{"awaiting_response"}, page: reflect.TypeFor[trdapi.QuotePage]()},
	"/api/brant/v1/trd/orders":   {filters: []string{"status", "quote_id"}, flags: []string{"open_review"}, page: reflect.TypeFor[trdapi.OrderPage]()},
	"/api/brant/v1/trd/customer_responses": {filters: []string{"quote_id", "classification"}, flags: []string{"unverified"},
		page: reflect.TypeFor[trdapi.ResponsePage]()},
	"/api/brant/v1/trd/rfq_coverage": {repeated: "rfq_id", max: trdapi.MaxCoverageRfqs, page: reflect.TypeFor[trdapi.RfqCoverage]()},
	"/api/brant/v1/tsk/tasks":        {filters: []string{"lifecycle", "control_mode", "rfq_id", "quote_id"}, page: reflect.TypeFor[tskapi.TaskPage]()},
}

// params is listParams for the route at path, flags included.
func (l listRoute) params(r *http.Request) (map[string]string, int, error) {
	return listParams(r, append(append([]string{}, l.filters...), l.flags...)...)
}

func (s *server) listRfqs(w http.ResponseWriter, r *http.Request) {
	p, limit, err := listRoutes["/api/brant/v1/inq/rfqs"].params(r)
	if err == nil {
		var page inqapi.RfqPage
		if page, err = s.cfg.INQ.ListRfqs(r.Context(), inqapi.RfqQuery{Status: p["status"], CustomerID: p["customer_id"], After: p["after"], Limit: limit}); err == nil {
			writeJSON(w, http.StatusOK, okView(page))
			return
		}
	}
	s.fail(w, r, err)
}

func (s *server) listQuotes(w http.ResponseWriter, r *http.Request) {
	p, limit, err := listRoutes["/api/brant/v1/trd/quotes"].params(r)
	var awaiting bool
	if err == nil {
		awaiting, err = flag(p, "awaiting_response")
	}
	if err == nil {
		var page trdapi.QuotePage
		if page, err = s.cfg.TRD.ListQuotes(r.Context(), trdapi.QuoteListQuery{Status: p["status"], CustomerID: p["customer_id"],
			RfqID: p["rfq_id"], AwaitingResponse: awaiting, After: p["after"], Limit: limit}); err == nil {
			writeJSON(w, http.StatusOK, okView(page))
			return
		}
	}
	s.fail(w, r, err)
}

func (s *server) listResponses(w http.ResponseWriter, r *http.Request) {
	p, limit, err := listRoutes["/api/brant/v1/trd/customer_responses"].params(r)
	var unverified bool
	if err == nil {
		unverified, err = flag(p, "unverified")
	}
	if err == nil {
		var page trdapi.ResponsePage
		if page, err = s.cfg.TRD.ListResponses(r.Context(), trdapi.ResponseListQuery{QuoteID: p["quote_id"], Classification: p["classification"],
			Unverified: unverified, After: p["after"], Limit: limit}); err == nil {
			writeJSON(w, http.StatusOK, okView(page))
			return
		}
	}
	s.fail(w, r, err)
}

// rfqCoverage answers GET /api/brant/v1/trd/rfq_coverage?rfq_id=<id>[&rfq_id=<id>...]: the only
// parameter is rfq_id, once per RFQ; the context checks the values and their number.
func (s *server) rfqCoverage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for k := range q {
		if k != "rfq_id" {
			s.reject(w, r, kernel.Reject(kernel.CodeSchemaInvalid, "/"+k, "unknown query parameter"))
			return
		}
	}
	v, err := s.cfg.TRD.RfqCoverage(r.Context(), trdapi.RfqCoverageQuery{RfqIDs: q["rfq_id"]})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, okView(v))
}

func (s *server) listOrders(w http.ResponseWriter, r *http.Request) {
	p, limit, err := listRoutes["/api/brant/v1/trd/orders"].params(r)
	var open bool
	if err == nil {
		open, err = flag(p, "open_review")
	}
	if err == nil {
		var page trdapi.OrderPage
		if page, err = s.cfg.TRD.ListOrders(r.Context(), trdapi.OrderListQuery{Status: p["status"], QuoteID: p["quote_id"],
			OpenReview: open, After: p["after"], Limit: limit}); err == nil {
			writeJSON(w, http.StatusOK, okView(page))
			return
		}
	}
	s.fail(w, r, err)
}

func (s *server) listTasks(w http.ResponseWriter, r *http.Request) {
	p, limit, err := listRoutes["/api/brant/v1/tsk/tasks"].params(r)
	if err == nil {
		var page tskapi.TaskPage
		if page, err = s.cfg.TSK.ListTasks(r.Context(), tskapi.TaskQuery{Lifecycle: p["lifecycle"], ControlMode: p["control_mode"],
			RfqID: p["rfq_id"], QuoteID: p["quote_id"], After: p["after"], Limit: limit}); err == nil {
			writeJSON(w, http.StatusOK, okView(page))
			return
		}
	}
	s.fail(w, r, err)
}

func (s *server) listParties(w http.ResponseWriter, r *http.Request) {
	p, limit, err := listRoutes["/api/brant/v1/pty/parties"].params(r)
	if err == nil {
		var page ptyapi.PartyPage
		if page, err = s.cfg.PTY.ListParties(r.Context(), ptyapi.PartyQuery{Status: p["status"], Role: p["role"], Search: p["q"], After: p["after"], Limit: limit}); err == nil {
			writeJSON(w, http.StatusOK, okView(page))
			return
		}
	}
	s.fail(w, r, err)
}

func (s *server) listContacts(w http.ResponseWriter, r *http.Request) {
	p, limit, err := listRoutes["/api/brant/v1/pty/contacts"].params(r)
	if err == nil {
		var page ptyapi.ContactPage
		if page, err = s.cfg.PTY.ListContacts(r.Context(), ptyapi.ContactQuery{PartyID: p["party_id"], Status: p["status"], Search: p["q"], After: p["after"], Limit: limit}); err == nil {
			writeJSON(w, http.StatusOK, okView(page))
			return
		}
	}
	s.fail(w, r, err)
}

func (s *server) listSkus(w http.ResponseWriter, r *http.Request) {
	p, limit, err := listRoutes["/api/brant/v1/cat/skus"].params(r)
	if err == nil {
		var page catapi.SkuPage
		if page, err = s.cfg.CAT.ListSkus(r.Context(), catapi.SkuQuery{Status: p["status"], Search: p["q"], After: p["after"], Limit: limit}); err == nil {
			writeJSON(w, http.StatusOK, okView(page))
			return
		}
	}
	s.fail(w, r, err)
}
