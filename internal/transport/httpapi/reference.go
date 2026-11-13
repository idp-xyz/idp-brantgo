package httpapi

import (
	"context"
	"net/http"

	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// REFProgrammes is what the transport needs from reference data's country programme lists (S5
// E-3): the human-only import and the audited reads. ResolveCountryProgrammes is a lookup of
// downstream contexts, not a route.
type REFProgrammes interface {
	refapi.ProgrammeCommands
	GetProgrammeList(ctx context.Context, listCode string) (refapi.ProgrammeListView, error)
	GetProgrammeListVersion(ctx context.Context, listCode, version string) (refapi.ProgrammeListVersionView, error)
}

// programmeImport decodes country_programme.import reflectively from refapi.ImportCountryProgrammes
// (list_code, jurisdiction, version, source_name, source_url, published_on, file_sha256,
// programme_count, country_count, programmes[{programme_id, name, measure, countries, scope}]).
// It shares screening_list.import's ingress limits.
func programmeImport(c REF) commandFunc { return reflected("", "", c.ImportCountryProgrammes) }

// programmeListVersion answers GET /api/brant/v1/ref/country_programme_lists/<code>/versions/<version>.
func (s *server) programmeListVersion(w http.ResponseWriter, r *http.Request) {
	v, err := s.cfg.REF.GetProgrammeListVersion(r.Context(), r.PathValue("id"), r.PathValue("version"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, okView(v))
}
