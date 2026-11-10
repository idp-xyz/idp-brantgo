package httpapi

import (
	"encoding/base64"

	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
)

// The read responses. Unknown values are omitted, never null.

type actorView struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type auditView struct {
	Seq            int64     `json:"seq"`
	Actor          actorView `json:"actor"`
	Command        string    `json:"command"`
	ObjectID       string    `json:"object_id,omitempty"`
	Outcome        string    `json:"outcome"`
	Code           string    `json:"code,omitempty"`
	CorrelationID  string    `json:"correlation_id"`
	RecordedAt     string    `json:"recorded_at"`
	Reason         string    `json:"reason,omitempty"`
	AuthorityEpoch int64     `json:"authority_epoch,omitempty"`
}

type importView struct {
	ImportID   string    `json:"import_id"`
	Actor      actorView `json:"actor"`
	ImportedAt string    `json:"imported_at"`
}

type sourceJSON struct {
	OK                bool         `json:"ok"`
	SourceID          string       `json:"source_id"`
	SourceKind        string       `json:"source_kind"`
	Provider          string       `json:"provider,omitempty"`
	AccountRef        string       `json:"account_ref,omitempty"`
	ExternalMessageID string       `json:"external_message_id,omitempty"`
	AuthorClaim       string       `json:"author_claim,omitempty"`
	ContentSHA256     string       `json:"content_sha256"`
	ContentText       string       `json:"content_text"`
	RegisteredAt      string       `json:"registered_at"`
	Imports           []importView `json:"imports"`
}

func sourceView(v evdapi.SourceView) sourceJSON {
	out := sourceJSON{OK: true, SourceID: v.SourceID, SourceKind: v.SourceKind, Provider: v.Provider, AccountRef: v.AccountRef,
		ExternalMessageID: v.ExternalMessageID, AuthorClaim: v.AuthorClaim, ContentSHA256: v.ContentSHA256, ContentText: v.ContentText,
		RegisteredAt: timestamp(v.RegisteredAt), Imports: []importView{}}
	for _, i := range v.Imports {
		out.Imports = append(out.Imports, importView{ImportID: i.ImportID, Actor: actorView{string(i.Actor.Kind), i.Actor.ID}, ImportedAt: timestamp(i.ImportedAt)})
	}
	return out
}

type fileJSON struct {
	OK               bool   `json:"ok"`
	FileID           string `json:"file_id"`
	FileName         string `json:"file_name"`
	MimeType         string `json:"mime_type"`
	ContentBase64    string `json:"content_base64"`
	ContentSHA256    string `json:"content_sha256"`
	SupersedesFileID string `json:"supersedes_file_id,omitempty"`
	RegisteredAt     string `json:"registered_at"`
}

func fileView(v evdapi.FileView) fileJSON {
	return fileJSON{OK: true, FileID: v.FileID, FileName: v.FileName, MimeType: v.MimeType, ContentBase64: base64.StdEncoding.EncodeToString(v.Content),
		ContentSHA256: v.ContentSHA256, SupersedesFileID: v.SupersedesFileID, RegisteredAt: timestamp(v.RegisteredAt)}
}

type revisionView struct {
	Revision    int64     `json:"revision"`
	Name        string    `json:"name"`
	Roles       []string  `json:"roles,omitempty"`
	BaseUOM     string    `json:"base_uom,omitempty"`
	EvidenceIDs []string  `json:"evidence_ids"`
	ConfirmedBy actorView `json:"confirmed_by"`
}

type partyJSON struct {
	OK         bool           `json:"ok"`
	PartyID    string         `json:"party_id"`
	Status     string         `json:"status"`
	RowVersion int64          `json:"row_version"`
	Name       string         `json:"name,omitempty"`
	Roles      []string       `json:"roles"`
	Revisions  []revisionView `json:"revisions"`
}

func partyView(v ptyapi.PartyView) partyJSON {
	out := partyJSON{OK: true, PartyID: v.PartyID, Status: v.Status, RowVersion: v.RowVersion, Name: v.Name, Roles: v.Roles, Revisions: []revisionView{}}
	for _, r := range v.Revisions {
		out.Revisions = append(out.Revisions, revisionView{Revision: r.Revision, Name: r.Name, Roles: r.Roles, EvidenceIDs: r.EvidenceIDs,
			ConfirmedBy: actorView{string(r.ConfirmedBy.Kind), r.ConfirmedBy.ID}})
	}
	return out
}

type contactJSON struct {
	OK          bool     `json:"ok"`
	ContactID   string   `json:"contact_id"`
	PartyID     string   `json:"party_id,omitempty"`
	Status      string   `json:"status"`
	RowVersion  int64    `json:"row_version"`
	Name        string   `json:"name,omitempty"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
}

func contactView(v ptyapi.ContactView) contactJSON {
	return contactJSON{OK: true, ContactID: v.ContactID, PartyID: v.PartyID, Status: v.Status, RowVersion: v.RowVersion, Name: v.Name, EvidenceIDs: v.EvidenceIDs}
}

type skuJSON struct {
	OK         bool           `json:"ok"`
	SkuID      string         `json:"sku_id"`
	Status     string         `json:"status"`
	RowVersion int64          `json:"row_version"`
	Name       string         `json:"name,omitempty"`
	BaseUOM    string         `json:"base_uom,omitempty"`
	Revisions  []revisionView `json:"spec_revisions"`
}

func skuView(v catapi.SkuView) skuJSON {
	out := skuJSON{OK: true, SkuID: v.SkuID, Status: v.Status, RowVersion: v.RowVersion, Name: v.Name, BaseUOM: v.BaseUnit, Revisions: []revisionView{}}
	for _, r := range v.Revisions {
		out.Revisions = append(out.Revisions, revisionView{Revision: r.Revision, Name: r.Name, BaseUOM: r.BaseUnit, EvidenceIDs: r.EvidenceIDs,
			ConfirmedBy: actorView{string(r.ConfirmedBy.Kind), r.ConfirmedBy.ID}})
	}
	return out
}

type lineJSON struct {
	LineID          string `json:"line_id"`
	SkuID           string `json:"sku_id,omitempty"`
	Quantity        string `json:"quantity,omitempty"`
	UOM             string `json:"uom,omitempty"`
	RequestedItem   string `json:"requested_item,omitempty"`
	SourceID        string `json:"source_id,omitempty"`
	Locator         string `json:"locator,omitempty"`
	SkuSpecRevision int64  `json:"sku_spec_revision,omitempty"`
}

type confirmationJSON struct {
	CustomerRowVersion int64     `json:"customer_row_version"`
	ContactRowVersion  int64     `json:"contact_row_version"`
	EvidenceIDs        []string  `json:"evidence_ids"`
	ConfirmedBy        actorView `json:"confirmed_by"`
	AuthorityEpoch     int64     `json:"authority_epoch"`
}

type rfqJSON struct {
	OK               bool              `json:"ok"`
	RfqID            string            `json:"rfq_id"`
	Status           string            `json:"status"`
	RowVersion       int64             `json:"row_version"`
	CustomerID       string            `json:"customer_id,omitempty"`
	ContactID        string            `json:"contact_id,omitempty"`
	Lines            []lineJSON        `json:"lines"`
	SourceIDs        []string          `json:"source_ids"`
	UnresolvedFields []string          `json:"unresolved_fields"`
	Confirmation     *confirmationJSON `json:"confirmation,omitempty"`
}

func rfqView(v inqapi.RfqView) rfqJSON {
	out := rfqJSON{OK: true, RfqID: v.RfqID, Status: v.Status, RowVersion: v.RowVersion, CustomerID: v.CustomerID, ContactID: v.ContactID,
		Lines: []lineJSON{}, SourceIDs: v.SourceIDs, UnresolvedFields: v.UnresolvedFields}
	for _, l := range v.Lines {
		out.Lines = append(out.Lines, lineJSON{LineID: l.LineID, SkuID: l.SkuID, Quantity: l.Quantity, UOM: l.UOM, RequestedItem: l.RequestedItem,
			SourceID: l.SourceID, Locator: l.Locator, SkuSpecRevision: l.SkuSpecRevision})
	}
	if c := v.Confirmation; c != nil {
		out.Confirmation = &confirmationJSON{CustomerRowVersion: c.CustomerRowVersion, ContactRowVersion: c.ContactRowVersion, EvidenceIDs: c.EvidenceIDs,
			ConfirmedBy: actorView{string(c.ConfirmedBy.Kind), c.ConfirmedBy.ID}, AuthorityEpoch: c.AuthorityEpoch}
	}
	return out
}
