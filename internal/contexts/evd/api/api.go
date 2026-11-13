// Package api is the contract of the EVD bounded context (evidence: sources and business files)
// for transports, the AI layer and the contexts downstream of EVD. It exposes no EVD domain type.
package api

import (
	"context"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	// Contract names the request and response shapes of EVD commands.
	Contract = "brant.evd/0.1.0"
	// Source identifies EVD as the producer of integration events.
	Source = "idp-brant.evd"
	// EventSourceRegistered is published when a new source origin is registered.
	EventSourceRegistered = "idp-brant.evd.source_registered.v1"
	// EventFileRegistered is published when a business file is registered.
	EventFileRegistered = "idp-brant.evd.file_registered.v1"

	CommandRegisterSource = "source.register"
	CommandRegisterFile   = "file.register"

	// CodeSourceIdentityConflict: the source ID or the channel message is known with other content
	// or another channel identity (EVD-INV-001).
	CodeSourceIdentityConflict = "SOURCE_IDENTITY_CONFLICT"
	// CodeFileInvalid: the bytes are not valid Base64, empty, too large, or not of the declared type.
	CodeFileInvalid = "FILE_INVALID"
	// CodeBusinessKeyConflict: the file ID exists, or the superseded file already has a successor.
	CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"

	MimeText = "text/plain"
	MimePDF  = "application/pdf"
	MimeXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

	// MaxFileBytes bounds a business file (Python foundation.file.register: 10 MiB).
	MaxFileBytes = 10 << 20

	KindManualText = "manual_text"
	KindEmailText  = "email_text"
	// KindFileExtract is the text an extractor of the AI layer read from a business file under an
	// input profile (WBS S4-04b; ADR-BR-010): EVD keeps it as a source tied to the file, with the
	// located form its locators point into, its SHA-256 and the extractor version.
	KindFileExtract = "file_extract"

	// MaxContentCharacters bounds a text source (Python T01: 50,000 characters).
	MaxContentCharacters = 50000
	// MaxLocatedFormBytes bounds the located form of one file extract: the profile bounds the
	// characters, the form adds a locator per segment and escapes (unit B4 decision; A6: about
	// 2.5 MB at worst).
	MaxLocatedFormBytes = 8 << 20

	// CodeInputProfileUnsupported: the file of a file_extract source is outside the input profile
	// the extractor reads under (baseline 14.2: refused, never truncated; the manual path applies).
	CodeInputProfileUnsupported = "INPUT_PROFILE_UNSUPPORTED"
)

// RegisterSource registers a text source. The same content in one scope is stored once; an email
// is identified by provider, account and message ID, and the same message with another body is
// refused. Every accepted request records an import, except a replay of the same idempotency key.
//
// A file_extract names only the business file: EVD reads the file's bytes and has the AI layer's
// extractor (Extractor) read them, so the text and its locators come from the file, never from the
// caller (AI-INV-004). The registrant must see the file; a file outside the input profile is
// INPUT_PROFILE_UNSUPPORTED.
type RegisterSource struct {
	IdempotencyKey    string
	SourceID          string // args.id
	ContentText       string // manual_text and email_text; empty for a file_extract
	SourceKind        string // manual_text, email_text or file_extract
	Provider          string // email only
	AccountRef        string // email only
	ExternalMessageID string // email only
	AuthorClaim       string // email only: who the message claims to come from, not verified
	// FileID is the business file a file_extract is read from. Left out of the request hash while
	// empty, so requests sent before it replay unchanged (X-INV-001).
	FileID string `json:",omitempty"`
}

// Validate checks the closed contract (Python BC_T01_commands.schema.json, source.register).
func (c RegisterSource) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.SourceID):
		return schema("/args/id")
	}
	channel := c.Provider != "" || c.AccountRef != "" || c.ExternalMessageID != "" || c.AuthorClaim != ""
	if c.SourceKind == KindFileExtract {
		switch {
		case c.ContentText != "":
			return schema("/args/content_text") // the text is read from the file
		case channel:
			return schema("/args/source_kind") // a file extract has no channel identity
		case !kernel.ValidIdentifier(c.FileID):
			return schema("/args/file_id")
		}
		return nil
	}
	switch {
	case !text(c.ContentText, MaxContentCharacters):
		return schema("/args/content_text")
	case c.FileID != "":
		return schema("/args/file_id") // only a file extract names a file
	}
	switch c.SourceKind {
	case KindManualText:
		if channel {
			return schema("/args/source_kind") // a manual text has no channel identity
		}
	case KindEmailText:
		switch {
		case !kernel.ValidIdentifier(c.Provider):
			return schema("/args/provider")
		case !kernel.ValidIdentifier(c.AccountRef):
			return schema("/args/account_ref")
		case !text(c.ExternalMessageID, 200):
			return schema("/args/external_message_id")
		case !text(c.AuthorClaim, 200):
			return schema("/args/author_claim")
		}
	default:
		return schema("/args/source_kind")
	}
	return nil
}

// Auditor records a request an inbound adapter refused before it became a command, such as a body
// that is not valid JSON, in the context's audit trail. The principal in ctx is the requester.
type Auditor interface {
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// RegisterFile registers an immutable business file. A new version names the file it supersedes;
// a file has at most one successor, so versions never branch.
type RegisterFile struct {
	IdempotencyKey   string
	FileID           string // args.id
	FileName         string
	MimeType         string // text/plain, application/pdf or the XLSX type
	ContentBase64    string // standard Base64 of at most MaxFileBytes bytes
	SupersedesFileID string
}

// Validate checks the closed contract (Python foundation.commands.schema.json, file.register).
// Whether the bytes match the type is a FILE_INVALID decision of the context.
func (c RegisterFile) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.FileID):
		return schema("/args/id")
	case !text(c.FileName, 255):
		return schema("/args/file_name")
	case c.MimeType != MimeText && c.MimeType != MimePDF && c.MimeType != MimeXLSX:
		return schema("/args/mime_type")
	case c.ContentBase64 == "":
		return schema("/args/content_base64")
	case c.SupersedesFileID != "" && !kernel.ValidIdentifier(c.SupersedesFileID):
		return schema("/args/supersedes_file_id")
	}
	return nil
}

// Receipt is the closed success answer of a command.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// Commands is EVD's command entry point. Errors are *kernel.Rejection for refusals; anything else
// is an infrastructure failure.
type Commands interface {
	RegisterSource(ctx context.Context, cmd RegisterSource) (Receipt, error)
	RegisterFile(ctx context.Context, cmd RegisterFile) (Receipt, error)
}

// Command is one of EVD's commands: RegisterSource or RegisterFile.
type Command interface{ evdCommand() }

func (RegisterSource) evdCommand() {}
func (RegisterFile) evdCommand()   {}

// Reconciliation is what became of a command whose answer the caller lost.
type Reconciliation struct {
	State    kernel.ReconcileState
	ActionID string   // committed and payload_conflict
	ObjectID string   // committed and payload_conflict
	Receipt  *Receipt // the stored receipt; committed only
}

// Reconciler recovers from an unknown outcome without executing anything (contract 06, 动作对账;
// WBS S4-02 closes the S3 gap "动作对账：EVD 尚无"). The caller passes the command exactly as it
// sent it, idempotency key included; EVD hashes it as the command did. Reconciling needs evd.read,
// not the command's own permission, and re-checks it and the caller's data scope over the source or
// file the stored result names now (X-INV-003): PERMISSION_DENIED and NOT_FOUND as for a read.
type Reconciler interface {
	Reconcile(ctx context.Context, cmd Command) (Reconciliation, error)
}

// ActionReconciler is EVD's ActionReconciliation v1 (Context Map R66 EVD→TSK, S4 decision D-11):
// reconciliation by the ledger identity of a command, for a caller that kept the request rather
// than the command (TSK's ActionExecution, which records it before the command is sent; a file's
// bytes need not be sent again to learn what became of its registration).
//
// RequestOf returns the command name, idempotency key and request hash EVD's ledger records for cmd
// — hashed exactly as executing it hashes it — and executes and reads nothing; a command EVD does
// not know is SCHEMA_INVALID. ReconcileHash answers what Reconcile answers for the command that
// hashes to requestHash: committed with the stored receipt, payload_conflict or not_committed,
// after the same checks, audited the same way (action.reconcile), executing nothing. An unknown
// command name, an invalid key or a request hash that is not 64 lower-case hexadecimal digits is
// SCHEMA_INVALID.
type ActionReconciler interface {
	RequestOf(cmd Command) (kernel.ActionRequest, error)
	ReconcileHash(ctx context.Context, command, idempotencyKey, requestHash string) (Reconciliation, error)
}

// SourceRef is SourceRef v1: a source origin and the hash of its content.
type SourceRef struct {
	SourceID      string
	ContentSHA256 string
}

// SourceImport is one accepted registration of a source.
type SourceImport struct {
	ImportID   string
	Actor      kernel.ActorRef
	ImportedAt time.Time
}

// SourceView is a source with its content and imports, for a reader allowed to see it.
type SourceView struct {
	SourceID          string
	SourceKind        string
	Provider          string
	AccountRef        string
	ExternalMessageID string
	AuthorClaim       string
	ContentSHA256     string
	ContentText       string
	RegisteredAt      time.Time
	Imports           []SourceImport
	// Extract describes a file_extract: the file and how its text was read; nil for other kinds.
	Extract *ExtractRef
}

// ExtractRef identifies a file extract: the file it was read from and the reader that read it.
// The extractor version defines the locators (WBS S4-04: text line:col, PDF page:line, XLSX
// sheet!cell); the SHA-256 is that of the located form. A hash identifies content; it proves no
// truth (EVD boundary).
type ExtractRef struct {
	FileID     string
	FileSHA256 string
	Kind       string // text, pdf or xlsx
	Extractor  string // the reader and its version, such as "brant.ingest.pdf/1"
	Profile    string // the input profile the file was read under, such as "Q01.upload.v0.1"
	Characters int    // characters of the extract's text
	Pages      int    // PDF: pages, blank ones included
	Sheets     []string
	SHA256     string // SHA-256 of the located form, lower-case hex
}

// ExtractView is the located form of a file_extract source, for a reader who sees the source and
// the file now (CT16). LocatedForm is the extractor's canonical form (internal/ai/ingest
// brant-extract/1): a header line, then one tab-separated record per field, segment ("segment",
// locator, text) and note ("note", kind, locator), every value escaped (backslash, tab, line feed,
// carriage return).
type ExtractView struct {
	SourceID string
	ExtractRef
	LocatedForm  string
	RegisteredAt time.Time
}

// FileExtract is what an Extractor read from a business file: the extract's identity (FileID
// left empty), its text (the segments joined by line feeds) and its located form. EVD checks what
// it can without parsing: the hashes, the bounds, and that the form was read from the file's bytes.
type FileExtract struct {
	ExtractRef
	Text        string
	LocatedForm string
}

// Extractor reads a business file's bytes under an input profile. It is the AI layer's reader
// (internal/ai/ingest; ADR-BR-010: parsing is candidate generation), which bootstrap gives EVD:
// EVD holds no parsing code. A file outside the profile is a *kernel.Rejection
// INPUT_PROFILE_UNSUPPORTED whose reason names the limit; nothing is truncated.
type Extractor interface {
	Extract(mimeType string, content []byte) (FileExtract, error)
}

// ExtractQueries reads file extracts.
type ExtractQueries interface {
	// GetExtract returns the located form of a file_extract source, audited. The caller must hold
	// evd.read and see both the source and the file it was read from now (CT16: a reference never
	// bypasses current access); NOT_FOUND otherwise, and for a source of another kind.
	GetExtract(ctx context.Context, sourceID string) (ExtractView, error)
}

// FileView is a business file with its bytes, for a reader allowed to see it.
type FileView struct {
	FileID           string
	FileName         string
	MimeType         string
	Content          []byte
	ContentSHA256    string
	SupersedesFileID string
	RegisteredAt     time.Time
}

// Queries is EVD's read side.
type Queries interface {
	// GetFile returns a business file the caller sees; the read is audited.
	GetFile(ctx context.Context, fileID string) (FileView, error)
	// GetSource returns a source the caller sees; the read is audited. NOT_FOUND hides sources of
	// other owners as well as missing ones.
	GetSource(ctx context.Context, sourceID string) (SourceView, error)
	// ResolveSources returns, among ids, the sources the caller may read. It needs the EVD read
	// permission (PERMISSION_DENIED otherwise; Python _source_ref). A file_extract is readable only
	// while the caller also sees its file (CT16). Ask before opening your own transaction: EVD
	// answers in a transaction of its own.
	ResolveSources(ctx context.Context, ids []string) (map[string]SourceRef, error)
	// Audit returns EVD's audit trail of the caller's scope.
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

// text reports whether s has 1 to max characters.
func text(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= max && utf8.ValidString(s)
}
