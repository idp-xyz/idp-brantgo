package httpapi

import (
	"encoding/base64"

	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/kernel"
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
	Extract           *extractRef  `json:"extract,omitempty"` // file_extract: the file and the reader
}

// extractRef identifies a file extract (S4 B4): the file it was read from and the reader that read
// it, whose version defines the locators.
type extractRef struct {
	FileID     string   `json:"file_id"`
	FileSHA256 string   `json:"file_sha256"`
	Kind       string   `json:"kind"`
	Extractor  string   `json:"extractor"`
	Profile    string   `json:"profile"`
	Characters int      `json:"characters"`
	Pages      int      `json:"pages,omitempty"`
	Sheets     []string `json:"sheets,omitempty"`
	SHA256     string   `json:"extract_sha256"`
}

func extractRefView(r evdapi.ExtractRef) *extractRef {
	return &extractRef{FileID: r.FileID, FileSHA256: r.FileSHA256, Kind: r.Kind, Extractor: r.Extractor, Profile: r.Profile,
		Characters: r.Characters, Pages: r.Pages, Sheets: r.Sheets, SHA256: r.SHA256}
}

func sourceView(v evdapi.SourceView) sourceJSON {
	out := sourceJSON{OK: true, SourceID: v.SourceID, SourceKind: v.SourceKind, Provider: v.Provider, AccountRef: v.AccountRef,
		ExternalMessageID: v.ExternalMessageID, AuthorClaim: v.AuthorClaim, ContentSHA256: v.ContentSHA256, ContentText: v.ContentText,
		RegisteredAt: timestamp(v.RegisteredAt), Imports: []importView{}}
	for _, i := range v.Imports {
		out.Imports = append(out.Imports, importView{ImportID: i.ImportID, Actor: actorView{string(i.Actor.Kind), i.Actor.ID}, ImportedAt: timestamp(i.ImportedAt)})
	}
	if v.Extract != nil {
		out.Extract = extractRefView(*v.Extract)
	}
	return out
}

// extractJSON is the located form of a file_extract source (brant-extract/1: a header line, then
// tab-separated records "segment <locator> <text>" and "note <kind> <locator>", values escaped).
type extractJSON struct {
	OK           bool        `json:"ok"`
	SourceID     string      `json:"source_id"`
	Extract      *extractRef `json:"extract"`
	LocatedForm  string      `json:"located_form"`
	RegisteredAt string      `json:"registered_at"`
}

func extractView(v evdapi.ExtractView) extractJSON {
	return extractJSON{OK: true, SourceID: v.SourceID, Extract: extractRefView(v.ExtractRef), LocatedForm: v.LocatedForm,
		RegisteredAt: timestamp(v.RegisteredAt)}
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
	Addresses   any       `json:"addresses,omitempty"`
	EvidenceIDs []string  `json:"evidence_ids"`
	ConfirmedBy actorView `json:"confirmed_by"`
}

type partyJSON struct {
	OK         bool           `json:"ok"`
	PartyID    string         `json:"party_id"`
	Status     string         `json:"status"`
	RowVersion int64          `json:"row_version"`
	Revision   int64          `json:"revision"` // latest confirmed revision; 0: never confirmed
	Name       string         `json:"name,omitempty"`
	Roles      []string       `json:"roles"`
	Addresses  any            `json:"addresses"`
	Revisions  []revisionView `json:"revisions"`
	// ServiceRoles are every verified revision of the party's service roles.
	ServiceRoles any `json:"service_roles"`
}

// list returns the tagged copy of a slice, never null.
func list[T any](s []T) any {
	if s == nil {
		s = []T{}
	}
	return wire(s)
}

func partyView(v ptyapi.PartyView) partyJSON {
	out := partyJSON{OK: true, PartyID: v.PartyID, Status: v.Status, RowVersion: v.RowVersion, Revision: v.Revision, Name: v.Name, Roles: v.Roles,
		Addresses: list(v.Addresses), Revisions: []revisionView{}, ServiceRoles: list(v.ServiceRoles)}
	for _, r := range v.Revisions {
		out.Revisions = append(out.Revisions, revisionView{Revision: r.Revision, Name: r.Name, Roles: r.Roles, Addresses: list(r.Addresses),
			EvidenceIDs: r.EvidenceIDs, ConfirmedBy: actorView{string(r.ConfirmedBy.Kind), r.ConfirmedBy.ID}})
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
	LineID          string             `json:"line_id"`
	SkuID           string             `json:"sku_id,omitempty"`
	Quantity        string             `json:"quantity,omitempty"`
	UOM             string             `json:"uom,omitempty"`
	RequestedItem   string             `json:"requested_item,omitempty"`
	SourceID        string             `json:"source_id,omitempty"`
	Locator         string             `json:"locator,omitempty"`
	SkuSpecRevision int64              `json:"sku_spec_revision,omitempty"`
	Locators        []fieldLocatorJSON `json:"locators,omitempty"`
	Pending         []pendingFieldJSON `json:"pending,omitempty"`
	ProposedBy      *actorView         `json:"proposed_by,omitempty"`
	QuotesWithheld  bool               `json:"quotes_withheld,omitempty"` // CT16: the source is not readable now
}

type fieldLocatorJSON struct {
	Field   string `json:"field"`
	Locator string `json:"locator"`
	Quote   string `json:"quote,omitempty"`
}

type pendingFieldJSON struct {
	Field      string   `json:"field"`
	Reason     string   `json:"reason"`
	Candidates []string `json:"candidates,omitempty"`
}

type correctionJSON struct {
	Seq         int64      `json:"seq"`
	RowVersion  int64      `json:"row_version"`
	Command     string     `json:"command"`
	LineID      string     `json:"line_id"`
	Field       string     `json:"field,omitempty"`
	Kind        string     `json:"kind"`
	From        string     `json:"from,omitempty"`
	To          string     `json:"to,omitempty"`
	Locator     string     `json:"locator,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	Candidates  []string   `json:"candidates,omitempty"`
	ProposedBy  *actorView `json:"proposed_by,omitempty"`
	CorrectedBy actorView  `json:"corrected_by"`
	RecordedAt  string     `json:"recorded_at"`
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
	Corrections      []correctionJSON  `json:"corrections"`
}

// actorRef is the view of an actor, nil for an unknown one.
func actorRef(a kernel.ActorRef) *actorView {
	if a.ID == "" {
		return nil
	}
	return &actorView{string(a.Kind), a.ID}
}

func rfqView(v inqapi.RfqView) rfqJSON {
	out := rfqJSON{OK: true, RfqID: v.RfqID, Status: v.Status, RowVersion: v.RowVersion, CustomerID: v.CustomerID, ContactID: v.ContactID,
		Lines: []lineJSON{}, SourceIDs: v.SourceIDs, UnresolvedFields: v.UnresolvedFields, Corrections: []correctionJSON{}}
	for _, l := range v.Lines {
		line := lineJSON{LineID: l.LineID, SkuID: l.SkuID, Quantity: l.Quantity, UOM: l.UOM, RequestedItem: l.RequestedItem,
			SourceID: l.SourceID, Locator: l.Locator, SkuSpecRevision: l.SkuSpecRevision, ProposedBy: actorRef(l.ProposedBy),
			QuotesWithheld: l.QuotesWithheld}
		for _, x := range l.Locators {
			line.Locators = append(line.Locators, fieldLocatorJSON(x))
		}
		for _, x := range l.Pending {
			line.Pending = append(line.Pending, pendingFieldJSON(x))
		}
		out.Lines = append(out.Lines, line)
	}
	for _, c := range v.Corrections {
		out.Corrections = append(out.Corrections, correctionJSON{Seq: c.Seq, RowVersion: c.RowVersion, Command: c.Command, LineID: c.LineID,
			Field: c.Field, Kind: c.Kind, From: c.From, To: c.To, Locator: c.Locator, Reason: c.Reason, Candidates: c.Candidates,
			ProposedBy: actorRef(c.ProposedBy), CorrectedBy: actorView{string(c.CorrectedBy.Kind), c.CorrectedBy.ID},
			RecordedAt: timestamp(c.RecordedAt)})
	}
	if c := v.Confirmation; c != nil {
		out.Confirmation = &confirmationJSON{CustomerRowVersion: c.CustomerRowVersion, ContactRowVersion: c.ContactRowVersion, EvidenceIDs: c.EvidenceIDs,
			ConfirmedBy: actorView{string(c.ConfirmedBy.Kind), c.ConfirmedBy.ID}, AuthorityEpoch: c.AuthorityEpoch}
	}
	return out
}
