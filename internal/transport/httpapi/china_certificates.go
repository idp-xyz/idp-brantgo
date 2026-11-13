package httpapi

import (
	"net/http"

	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// cnxCertificateRoutes registers CNX's certificate reads (S5 C5).
func (s *server) cnxCertificateRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/brant/v1/cnx/certificates", s.authenticated(s.certificates))
	mux.Handle("GET /api/brant/v1/cnx/certificates/{id}", s.authenticated(read(s, s.cfg.CNX.GetCertificate, okView[cnxapi.CertificateView])))
	mux.Handle("GET /api/brant/v1/cnx/certificates/{id}/applicability", s.authenticated(s.certificateApplicability))
}

// certificates answers GET /api/brant/v1/cnx/certificates.
func (s *server) certificates(w http.ResponseWriter, r *http.Request) {
	v, err := s.cfg.CNX.Certificates(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if v == nil {
		v = []cnxapi.CertificateSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "certificates": wire(v)})
}

// certificateApplicability answers GET /api/brant/v1/cnx/certificates/<id>/applicability
// ?target_kind=shipment|export_lot&target_id=<id>.
func (s *server) certificateApplicability(w http.ResponseWriter, r *http.Request) {
	var v cnxapi.CertificateApplicabilityView
	var err error
	q := r.URL.Query()
	for k := range q {
		if (k != "target_kind" && k != "target_id") || len(q[k]) != 1 {
			err = kernel.Reject(kernel.CodeSchemaInvalid, "/"+k, "unknown or repeated query parameter")
		}
	}
	if err == nil {
		v, err = s.cfg.CNX.CertificateApplicability(r.Context(), r.PathValue("id"),
			cnxapi.ExportTarget{Kind: q.Get("target_kind"), ID: q.Get("target_id")})
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, okView(v))
}
