package httpapi

import (
	"net/http"

	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
)

// cnxDeclarationRoutes registers CNX's declaration and document bundle reads (S5 C3a).
func (s *server) cnxDeclarationRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/brant/v1/cnx/declaration_cases", s.authenticated(s.declarationCases))
	mux.Handle("GET /api/brant/v1/cnx/declaration_cases/{id}", s.authenticated(read(s, s.cfg.CNX.GetDeclaration, okView[cnxapi.DeclarationView])))
	mux.Handle("GET /api/brant/v1/cnx/document_bundles", s.authenticated(s.documentBundles))
	mux.Handle("GET /api/brant/v1/cnx/document_bundles/{id}", s.authenticated(read(s, s.cfg.CNX.GetBundle, okView[cnxapi.BundleView])))
}

// declarationCases answers GET /api/brant/v1/cnx/declaration_cases.
func (s *server) declarationCases(w http.ResponseWriter, r *http.Request) {
	v, err := s.cfg.CNX.Declarations(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if v == nil {
		v = []cnxapi.DeclarationSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "declaration_cases": wire(v)})
}

// documentBundles answers GET /api/brant/v1/cnx/document_bundles.
func (s *server) documentBundles(w http.ResponseWriter, r *http.Request) {
	v, err := s.cfg.CNX.Bundles(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if v == nil {
		v = []cnxapi.BundleSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "document_bundles": wire(v)})
}
