package httpapi

import (
	"encoding/json"
	"net/http"

	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/strictjson"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// POL is what the transport needs from POL.
type POL interface {
	polapi.Commands
	polapi.Queries
	polapi.Auditor
}

type configRefJSON struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

func (r configRefJSON) api() polapi.ConfigRef {
	return polapi.ConfigRef{Kind: r.Kind, ConfigID: r.ID, Revision: r.Revision}
}

type currencyJSON struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	MinorUnits int    `json:"minor_units"`
	Rounding   string `json:"rounding"`
}

type unitJSON struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Dimension string `json:"dimension"`
	Precision int    `json:"precision"`
}

type fxRateJSON struct {
	FromCurrency string `json:"from_currency"`
	ToCurrency   string `json:"to_currency"`
	Rate         string `json:"rate"`
	Purpose      string `json:"purpose"`
	SourceName   string `json:"source_name"`
	ObservedAt   string `json:"observed_at"`
}

type paymentStageJSON struct {
	StageID string `json:"stage_id"`
	Percent string `json:"percent"`
	Trigger string `json:"trigger"`
	Days    int    `json:"days"`
}

type paymentTermsJSON struct {
	Name   string             `json:"name"`
	Stages []paymentStageJSON `json:"stages"`
}

type numberSequenceJSON struct {
	DocumentType string `json:"document_type"`
	Prefix       string `json:"prefix"`
	Width        int    `json:"width"`
	Reset        string `json:"reset"`
	TimeZone     string `json:"time_zone"`
}

// configData decodes data as the closed structure of kind. A kind POL does not implement yet is
// passed on with empty data, so POL answers it with its own verdict.
func configData(kind string, data json.RawMessage) (polapi.ConfigData, error) {
	var d polapi.ConfigData
	if len(data) == 0 || data[0] != '{' {
		return d, schemaError{kernel.Reject(kernel.CodeSchemaInvalid, "/args/data", "required object")}
	}
	decode := func(v any) error {
		if err := strictjson.Unmarshal(data, v, strictjson.Limits{MaxBytes: int64(len(data))}); err != nil {
			if rej, ok := kernel.AsRejection(err); ok {
				path := "/args/data"
				if f := rej.Fields[0]; f.Path != "/" {
					path += f.Path
				}
				return schemaError{kernel.Reject(rej.Code, path, rej.Fields[0].Reason)}
			}
			return err
		}
		return nil
	}
	switch kind {
	case polapi.KindCurrency:
		var c currencyJSON
		if err := decode(&c); err != nil {
			return d, err
		}
		d.Currency = &polapi.Currency{Code: c.Code, Name: c.Name, MinorUnits: c.MinorUnits, Rounding: c.Rounding}
	case polapi.KindUnit:
		var u unitJSON
		if err := decode(&u); err != nil {
			return d, err
		}
		d.Unit = &polapi.Unit{Code: u.Code, Name: u.Name, Dimension: u.Dimension, Precision: u.Precision}
	case polapi.KindFXRate:
		var x fxRateJSON
		if err := decode(&x); err != nil {
			return d, err
		}
		d.FXRate = &polapi.FXRate{FromCurrency: x.FromCurrency, ToCurrency: x.ToCurrency, Rate: x.Rate, Purpose: x.Purpose,
			SourceName: x.SourceName, ObservedAt: x.ObservedAt}
	case polapi.KindPaymentTerms:
		var p paymentTermsJSON
		if err := decode(&p); err != nil {
			return d, err
		}
		t := &polapi.PaymentTerms{Name: p.Name}
		for _, s := range p.Stages {
			t.Stages = append(t.Stages, polapi.PaymentStage(s))
		}
		d.PaymentTerms = t
	case polapi.KindNumberSequence:
		var n numberSequenceJSON
		if err := decode(&n); err != nil {
			return d, err
		}
		d.NumberSequence = &polapi.NumberSequence{DocumentType: n.DocumentType, Prefix: n.Prefix, Width: n.Width, Reset: n.Reset, TimeZone: n.TimeZone}
	}
	return d, nil
}

func polCommands(c POL) *commandSet {
	type saveDraft struct {
		Kind            string          `json:"kind"`
		ID              string          `json:"id"`
		ExpectedVersion *int64          `json:"expected_version"`
		ValidFrom       string          `json:"valid_from"`
		ValidUntil      string          `json:"valid_until"`
		Data            json.RawMessage `json:"data"`
	}
	type rooted struct {
		Kind            string `json:"kind"`
		ID              string `json:"id"`
		ExpectedVersion *int64 `json:"expected_version"`
		Reason          string `json:"reason"`
	}
	type publish struct {
		Kind            string   `json:"kind"`
		ID              string   `json:"id"`
		ExpectedVersion *int64   `json:"expected_version"`
		Revision        int64    `json:"revision"`
		EvidenceIDs     []string `json:"evidence_ids"`
		Reason          string   `json:"reason"`
	}
	type issueNumber struct {
		Sequence    configRefJSON `json:"sequence"`
		BusinessRef string        `json:"business_ref"`
	}
	type snapshot struct {
		ID      string          `json:"id"`
		Purpose string          `json:"purpose"`
		Refs    []configRefJSON `json:"refs"`
	}
	return &commandSet{bc: "pol", contract: polapi.Contract, auditor: c, audit: c.Audit, commands: map[string]commandFunc{
		polapi.CommandSaveDraft: handle(func(key string, a saveDraft) (polapi.SaveDraft, error) {
			v, err := version(a.ExpectedVersion)
			if err != nil {
				return polapi.SaveDraft{}, err
			}
			data, err := configData(a.Kind, a.Data)
			return polapi.SaveDraft{IdempotencyKey: key, Kind: a.Kind, ConfigID: a.ID, ExpectedVersion: v, ValidFrom: a.ValidFrom,
				ValidUntil: a.ValidUntil, Data: data}, err
		}, c.SaveDraft),
		polapi.CommandDiscardDraft: handle(func(key string, a rooted) (polapi.DiscardDraft, error) {
			v, err := version(a.ExpectedVersion)
			return polapi.DiscardDraft{IdempotencyKey: key, Kind: a.Kind, ConfigID: a.ID, ExpectedVersion: v, Reason: a.Reason}, err
		}, c.DiscardDraft),
		polapi.CommandPublish: handle(func(key string, a publish) (polapi.Publish, error) {
			v, err := version(a.ExpectedVersion)
			return polapi.Publish{IdempotencyKey: key, Kind: a.Kind, ConfigID: a.ID, ExpectedVersion: v, Revision: a.Revision,
				EvidenceIDs: a.EvidenceIDs, Reason: a.Reason}, err
		}, c.Publish),
		polapi.CommandRetire: handle(func(key string, a rooted) (polapi.Retire, error) {
			v, err := version(a.ExpectedVersion)
			return polapi.Retire{IdempotencyKey: key, Kind: a.Kind, ConfigID: a.ID, ExpectedVersion: v, Reason: a.Reason}, err
		}, c.Retire),
		polapi.CommandIssueNumber: handle(func(key string, a issueNumber) (polapi.IssueNumber, error) {
			return polapi.IssueNumber{IdempotencyKey: key, Sequence: a.Sequence.api(), BusinessRef: a.BusinessRef}, nil
		}, c.IssueNumber),
		polapi.CommandCaptureSnapshot: handle(func(key string, a snapshot) (polapi.CaptureSnapshot, error) {
			cmd := polapi.CaptureSnapshot{IdempotencyKey: key, SnapshotID: a.ID, Purpose: a.Purpose}
			for _, r := range a.Refs {
				cmd.Refs = append(cmd.Refs, r.api())
			}
			return cmd, nil
		}, c.CaptureSnapshot),
	}}
}

type polRevisionJSON struct {
	Revision    int64           `json:"revision"`
	Status      string          `json:"status"`
	ContentHash string          `json:"content_hash"`
	ValidFrom   string          `json:"valid_from"`
	ValidUntil  string          `json:"valid_until,omitempty"`
	Data        json.RawMessage `json:"data"`
	SavedBy     actorView       `json:"saved_by"`
	SavedAt     string          `json:"saved_at"`
	PublishedBy *actorView      `json:"published_by,omitempty"`
	PublishedAt string          `json:"published_at,omitempty"`
	EvidenceIDs []string        `json:"evidence_ids,omitempty"`
	EndedAt     string          `json:"ended_at,omitempty"`
}

func dataJSON(d polapi.ConfigData) json.RawMessage {
	var v any
	switch {
	case d.Currency != nil:
		c := d.Currency
		v = currencyJSON{c.Code, c.Name, c.MinorUnits, c.Rounding}
	case d.Unit != nil:
		u := d.Unit
		v = unitJSON{u.Code, u.Name, u.Dimension, u.Precision}
	case d.FXRate != nil:
		x := d.FXRate
		v = fxRateJSON{x.FromCurrency, x.ToCurrency, x.Rate, x.Purpose, x.SourceName, x.ObservedAt}
	case d.PaymentTerms != nil:
		p := paymentTermsJSON{Name: d.PaymentTerms.Name, Stages: []paymentStageJSON{}}
		for _, s := range d.PaymentTerms.Stages {
			p.Stages = append(p.Stages, paymentStageJSON(s))
		}
		v = p
	case d.NumberSequence != nil:
		n := d.NumberSequence
		v = numberSequenceJSON{n.DocumentType, n.Prefix, n.Width, n.Reset, n.TimeZone}
	default:
		v = struct{}{}
	}
	b, _ := json.Marshal(v) // plain structs of strings and integers always encode
	return b
}

func configView(v polapi.ConfigView) any {
	revisions := []polRevisionJSON{}
	for _, r := range v.Revisions {
		j := polRevisionJSON{Revision: r.Revision, Status: r.Status, ContentHash: r.ContentHash, ValidFrom: timestamp(r.ValidFrom),
			Data: dataJSON(r.Data), SavedBy: actorView{string(r.SavedBy.Kind), r.SavedBy.ID}, SavedAt: timestamp(r.SavedAt), EvidenceIDs: r.EvidenceIDs}
		if !r.ValidUntil.IsZero() {
			j.ValidUntil = timestamp(r.ValidUntil)
		}
		if r.PublishedBy.ID != "" {
			j.PublishedBy, j.PublishedAt = &actorView{string(r.PublishedBy.Kind), r.PublishedBy.ID}, timestamp(r.PublishedAt)
		}
		if !r.EndedAt.IsZero() {
			j.EndedAt = timestamp(r.EndedAt)
		}
		revisions = append(revisions, j)
	}
	return map[string]any{"ok": true, "kind": v.Kind, "id": v.ConfigID, "root_version": v.RootVersion,
		"working_revision": v.WorkingRevision, "active_revision": v.ActiveRevision, "revisions": revisions}
}

func snapshotView(v polapi.SnapshotView) any {
	entries := []map[string]any{}
	for _, e := range v.Entries {
		entries = append(entries, map[string]any{"kind": e.Ref.Kind, "id": e.Ref.ConfigID, "revision": e.Ref.Revision, "content_hash": e.ContentHash})
	}
	return map[string]any{"ok": true, "snapshot_id": v.SnapshotID, "purpose": v.Purpose, "captured_at": timestamp(v.CapturedAt),
		"captured_by": actorView{string(v.CapturedBy.Kind), v.CapturedBy.ID}, "content_hash": v.ContentHash, "entries": entries}
}

// configRead answers GET /api/brant/v1/pol/configs/<kind>/<id>.
func (s *server) configRead(w http.ResponseWriter, r *http.Request) {
	v, err := s.cfg.POL.GetConfig(r.Context(), r.PathValue("kind"), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, configView(v))
}
