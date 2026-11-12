package shipmentdocumentset

import (
	"strconv"
	"strings"

	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Ack is an acknowledgement as an issue states it: the path of a difference and the reason it is
// accepted.
type Ack struct {
	Path   string
	Reason string
}

// Acknowledged is a difference an issue accepted, with its reason. FromIssue names the earlier
// issue whose acknowledgement of the very same difference (path, document version and values) it
// carries; 0 when the issue's request stated it.
type Acknowledged struct {
	Difference Difference `json:"difference"`
	Reason     string     `json:"reason"`
	FromIssue  int64      `json:"from_issue,omitempty"`
}

// IssuedDocument names a version an issue issued, with the SHA-256 of its content.
type IssuedDocument struct {
	Kind          string `json:"document_kind"`
	Version       int64  `json:"version"`
	Number        string `json:"number"`
	ContentSHA256 string `json:"content_sha256"`
}

// Issue is one issue of a set: the versions it issued, the differences it accepted, the
// reconciliation it was decided on (its SHA-256), who issued when, and its commit evidence.
type Issue struct {
	Seq                  int64
	SetVersion           int64
	Documents            []IssuedDocument
	Acknowledged         []Acknowledged
	DifferencesJSON      string
	ReconciliationSHA256 string
	IdempotencyKey       string
	Issued               Act
	Entries              []Entry
	EntriesJSON          string
	EntriesSHA256        string
}

// Issue decides shipment_document.issue (human only; SM-FUL-DOCUMENT draft → issued, the issued
// version before → superseded; FUL-INV-004) of set s at expected, given the shipment's state and
// the reconciliation diffs of its current versions. A difference an earlier issue acknowledged with
// the very same values stays acknowledged (it is carried, with its reason); a difference whose
// values moved is a new one. It refuses, in this order: another row version (VERSION_CONFLICT), a
// cancelled shipment (INVALID_STATE), a set without a draft (INVALID_STATE), an acknowledgement of no
// difference or of a missing required document (INVALID_KNOWN_VALUE at the acknowledgement), a
// difference neither resolved nor acknowledged (SHIPMENT_DOCUMENTS_UNRECONCILED at the first one's
// path). It returns the issue and the versions it superseded.
func (s *Set) Issue(expected int64, shipmentStatus string, diffs []Difference, acks []Ack, act Act) (*Issue, []*Document, error) {
	if s.RowVersion != expected {
		return nil, nil, Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if shipmentStatus == shipment.StatusCancelled {
		return nil, nil, Reject(kernel.CodeInvalidState, "/args/document_set_id", "The shipment is cancelled")
	}
	drafts := s.Drafts()
	if len(drafts) == 0 {
		return nil, nil, Reject(kernel.CodeInvalidState, "/args/document_set_id", "The set has no draft to issue")
	}
	byPath := map[string]Difference{}
	for _, d := range diffs {
		byPath[d.Path] = d
	}
	accepted := map[string]bool{}
	var acknowledged []Acknowledged
	for i, a := range acks {
		at := "/args/acknowledgements/" + strconv.Itoa(i) + "/path"
		d, ok := byPath[a.Path]
		switch {
		case !ok:
			return nil, nil, Reject(kernel.CodeInvalidKnownValue, at, "The reconciliation has no difference at "+a.Path)
		case d.Required:
			return nil, nil, Reject(kernel.CodeInvalidKnownValue, at, "A missing "+strings.ReplaceAll(d.Document, "_", " ")+" is never acknowledged")
		}
		accepted[a.Path] = true
		acknowledged = append(acknowledged, Acknowledged{Difference: d, Reason: a.Reason})
	}
	carried := map[Difference]Acknowledged{}
	for _, i := range s.Issues {
		for _, a := range i.Acknowledged {
			if a.FromIssue == 0 {
				a.FromIssue = i.Seq
			}
			carried[a.Difference] = a
		}
	}
	var open []Difference
	for _, d := range diffs {
		if accepted[d.Path] {
			continue
		}
		if a, ok := carried[d]; ok {
			acknowledged = append(acknowledged, a)
			continue
		}
		open = append(open, d)
	}
	if len(open) > 0 {
		d := open[0]
		return nil, nil, Reject(CodeShipmentDocumentsUnreconciled, d.Path, strconv.Itoa(len(open))+
			" difference(s) neither resolved nor acknowledged; first: "+d.Item+" of the "+d.Document+" is \""+d.Found+"\", the "+d.Against+
			" says \""+d.Expected+"\"")
	}
	if err := s.CheckCoverage(); err != nil { // S5-16: FUL-INV-005 on the invoice issued
		return nil, nil, err
	}
	act.At = kernel.Instant(act.At)
	s.RowVersion, s.IssueCount = s.RowVersion+1, s.IssueCount+1
	iss := &Issue{Seq: s.IssueCount, SetVersion: s.RowVersion, Acknowledged: acknowledged, Issued: act}
	iss.DifferencesJSON, iss.ReconciliationSHA256 = Seal(diffs)
	var superseded []*Document
	for _, d := range drafts {
		for _, old := range s.Versions(d.Kind) {
			if old.Status == Issued {
				old.Status, old.SupersededAt = Superseded, act.At
				superseded = append(superseded, old)
			}
		}
		d.Status, d.IssueSeq, d.IssuedAt = Issued, iss.Seq, act.At
		iss.Documents = append(iss.Documents, IssuedDocument{Kind: d.Kind, Version: d.Version, Number: d.Number, ContentSHA256: d.ContentSHA256})
	}
	s.Issues = append(s.Issues, iss)
	return iss, superseded, nil
}

// SignRequest is a shipment_document.record_signed as the contract checked it, with the EVD file
// as EVD showed it.
type SignRequest struct {
	Kind           string
	Version        int64
	Language       string
	File           File
	Evidence       []string
	IdempotencyKey string
}

// Sign decides shipment_document.record_signed (human only; CN-AT17 "保存签署版本，草稿不能冒充已签署";
// the tactical model's SignedCopy, as TRD order.document.record_signed): the signed or stamped copy
// of one language of an issued CI or PL version. It refuses, in this order: a record of an external
// document (INVALID_KNOWN_VALUE at /args/document_kind: its file is the document), no such version
// (REFERENCE_INVALID at /args/version), a version that is not issued (INVALID_STATE: a draft is
// never signed, a superseded version no longer), no rendition of the language (REFERENCE_INVALID at
// /args/language), the generated bytes of any rendition of the version as the file
// (INVALID_KNOWN_VALUE at /args/file_id), a source of evidence the actor does not read
// (REFERENCE_INVALID). A language's signed copy is recorded once: the same file again is a replay
// (replayed true), another INVALID_STATE.
func (s *Set) Sign(req SignRequest, found map[string]bool, act Act) (*Signature, bool, error) {
	if !Rendered(req.Kind) {
		return nil, false, Reject(kernel.CodeInvalidKnownValue, "/args/document_kind", "The record of an external document holds its own file")
	}
	d := s.Document(req.Kind, req.Version)
	if d == nil {
		return nil, false, Reject(kernel.CodeReferenceInvalid, "/args/version", "The set has no such version")
	}
	if d.Status != Issued {
		return nil, false, Reject(kernel.CodeInvalidState, "/args/version", "The version is "+d.Status+"; only an issued version is signed")
	}
	r := d.Rendition(req.Language)
	if r == nil {
		return nil, false, Reject(kernel.CodeReferenceInvalid, "/args/language", "The version has no rendition in "+req.Language)
	}
	for _, x := range d.Renditions {
		if req.File.SHA256 == x.SHA256 {
			return nil, false, Reject(kernel.CodeInvalidKnownValue, "/args/file_id", "The generated, unsigned document is not a signed copy")
		}
	}
	if err := Readable(req.Evidence, found, "/args/evidence_ids"); err != nil {
		return nil, false, err
	}
	for _, x := range d.Signatures {
		if x.Language != req.Language {
			continue
		}
		if x.FileID == req.File.ID && x.FileSHA256 == req.File.SHA256 {
			return &x, true, nil
		}
		return nil, false, Reject(kernel.CodeInvalidState, "/args/language", "The "+req.Language+" signed copy of "+d.Number+" v"+
			strconv.FormatInt(d.Version, 10)+" is recorded; it never changes")
	}
	act.At = kernel.Instant(act.At)
	sig := Signature{Language: req.Language, FileID: req.File.ID, FileSHA256: req.File.SHA256, MimeType: req.File.MimeType,
		DocumentSHA256: r.SHA256, Evidence: req.Evidence, IdempotencyKey: req.IdempotencyKey, Recorded: act}
	d.Signatures = append(d.Signatures, sig)
	return &sig, false, nil
}
