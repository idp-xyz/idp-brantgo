package httpapi

import (
	"net/http"
	"reflect"
	"slices"
	"strconv"

	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
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

// listRoute describes a list read for the OpenAPI document: its text filters and the type of its
// page, whose tagged form is the 200 response.
type listRoute struct {
	filters []string
	page    reflect.Type
}

var listRoutes = map[string]listRoute{
	"/api/brant/v1/inq/rfqs":   {[]string{"status", "customer_id"}, reflect.TypeFor[inqapi.RfqPage]()},
	"/api/brant/v1/trd/quotes": {[]string{"status", "customer_id", "rfq_id"}, reflect.TypeFor[trdapi.QuotePage]()},
	"/api/brant/v1/trd/orders": {[]string{"status", "quote_id"}, reflect.TypeFor[trdapi.OrderPage]()},
	"/api/brant/v1/tsk/tasks":  {[]string{"lifecycle", "control_mode", "rfq_id", "quote_id"}, reflect.TypeFor[tskapi.TaskPage]()},
}

func (s *server) listRfqs(w http.ResponseWriter, r *http.Request) {
	p, limit, err := listParams(r, listRoutes["/api/brant/v1/inq/rfqs"].filters...)
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
	p, limit, err := listParams(r, listRoutes["/api/brant/v1/trd/quotes"].filters...)
	if err == nil {
		var page trdapi.QuotePage
		if page, err = s.cfg.TRD.ListQuotes(r.Context(), trdapi.QuoteListQuery{Status: p["status"], CustomerID: p["customer_id"],
			RfqID: p["rfq_id"], After: p["after"], Limit: limit}); err == nil {
			writeJSON(w, http.StatusOK, okView(page))
			return
		}
	}
	s.fail(w, r, err)
}

func (s *server) listOrders(w http.ResponseWriter, r *http.Request) {
	p, limit, err := listParams(r, listRoutes["/api/brant/v1/trd/orders"].filters...)
	if err == nil {
		var page trdapi.OrderPage
		if page, err = s.cfg.TRD.ListOrders(r.Context(), trdapi.OrderListQuery{Status: p["status"], QuoteID: p["quote_id"],
			After: p["after"], Limit: limit}); err == nil {
			writeJSON(w, http.StatusOK, okView(page))
			return
		}
	}
	s.fail(w, r, err)
}

func (s *server) listTasks(w http.ResponseWriter, r *http.Request) {
	p, limit, err := listParams(r, listRoutes["/api/brant/v1/tsk/tasks"].filters...)
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
