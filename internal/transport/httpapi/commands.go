package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"

	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/foundation/strictjson"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// receipt is the closed success branch of a command response (Python BC_T01_response.schema.json).
type receipt struct {
	OK         bool   `json:"ok"`
	ActionID   string `json:"action_id"`
	ObjectID   string `json:"object_id"`
	RowVersion int64  `json:"row_version"`
	Status     string `json:"status"`
	Replayed   bool   `json:"replayed"`
}

// result is the shape every context's api.Receipt shares.
type result interface {
	~struct {
		ActionID   string
		ObjectID   string
		RowVersion int64
		Status     string
		Replayed   bool
	}
}

// commandSet is a context's contract: its contract name and the commands it accepts.
type commandSet struct {
	bc       string
	contract string
	commands map[string]commandFunc
	// limits raises the ingress limits of a command above the transport's defaults.
	limits  map[string]strictjson.Limits
	auditor interface {
		AuditRejected(ctx context.Context, command, objectID, code string) error
	}
	audit func(context.Context) ([]kernel.AuditEntry, error)
	// reconcile, when set, reconciles the commands whose type it names (contract 06, 动作对账).
	reconcile *reconcileFunc
}

// commandFunc decodes args of type args within maxValues JSON values into the context's command
// (of type cmd) and runs it.
type commandFunc struct {
	args  reflect.Type
	cmd   reflect.Type
	build func(key string, args json.RawMessage, maxValues int) (any, error)
	run   func(ctx context.Context, cmd any) (receipt, error)
}

// envelope is the uniform command request. Presence is checked field by field, so the members are
// pointers; args stays raw until the command is known.
type envelope struct {
	Contract       *string         `json:"contract"`
	Command        *string         `json:"command"`
	IdempotencyKey *string         `json:"idempotency_key"`
	Args           json.RawMessage `json:"args"`
}

// command answers POST /api/brant/v1/<bc>/commands/<command>. The checks run in the contracts'
// order: size and closed JSON, contract and command, structure of the arguments; then the context
// takes over with permission, idempotency, CAS, state and references. A request refused here is
// audited in the context as the Python service audits it.
func (s *server) command(w http.ResponseWriter, r *http.Request) {
	set, ok := s.contexts[r.PathValue("bc")]
	if !ok {
		s.reject(w, r, kernel.Reject(kernel.CodeUnsupportedContract, "/contract", ""))
		return
	}
	name := r.PathValue("command")
	run, known := set.commands[name]
	audited := name
	if !known {
		audited = "invalid"
	}
	res, err := s.decodeAndRun(r, set, name, run)
	if err != nil {
		if rej, ok := kernel.AsRejection(err); ok && rej.Code != kernel.CodeUnauthenticated && !executed(err) {
			if auditErr := set.auditor.AuditRejected(r.Context(), audited, "", rej.Code); auditErr != nil {
				s.fail(w, r, errors.Join(rej, auditErr))
				return
			}
		}
		s.fail(w, r, unwrapExecuted(err))
		return
	}
	res.OK = true
	writeJSON(w, http.StatusOK, res)
}

// executedError marks an error that the context returned and has already audited.
type executedError struct{ err error }

func (e executedError) Error() string { return e.err.Error() }
func (e executedError) Unwrap() error { return e.err }

func executed(err error) bool {
	var e executedError
	return errors.As(err, &e)
}

func unwrapExecuted(err error) error {
	var e executedError
	if errors.As(err, &e) {
		return e.err
	}
	return err
}

// decodeCommand checks the request against the command's contract and returns the context's
// command: size and closed JSON, contract and command, the structure of the arguments.
func (s *server) decodeCommand(r *http.Request, set *commandSet, name string, run commandFunc) (any, error) {
	limit, values := s.cfg.MaxBodyBytes, strictjson.DefaultMaxValues
	if l := set.limits[name]; l.MaxBytes > limit {
		limit, values = l.MaxBytes, max(l.MaxValues, values)
	}
	body := http.MaxBytesReader(nil, r.Body, limit+1)
	raw, err := io.ReadAll(body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, kernel.Reject(kernel.CodeRequestTooLarge, "/", "")
		}
		return nil, err
	}
	var env envelope
	if err = strictjson.Decode(bytes.NewReader(raw), &env, strictjson.Limits{MaxBytes: limit, MaxValues: values}); err != nil {
		return nil, err
	}
	switch {
	case env.Contract == nil || *env.Contract != set.contract:
		return nil, kernel.Reject(kernel.CodeUnsupportedContract, "/contract", "")
	case env.Command == nil || *env.Command != name || run.build == nil:
		return nil, kernel.Reject(kernel.CodeUnsupportedContract, "/command", "")
	case env.IdempotencyKey == nil:
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/idempotency_key", "required")
	case len(env.Args) == 0 || env.Args[0] != '{':
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/args", "required object")
	}
	if p := emptyString(raw); p != "" {
		// Every string of these contracts has at least one character; an unknown value is omitted.
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, p, "empty string")
	}
	cmd, err := run.build(*env.IdempotencyKey, env.Args, values)
	if err != nil {
		return nil, contextError(err)
	}
	return cmd, nil
}

// contextError keeps a rejection found while decoding args as the transport's own and marks any
// other error as the context's.
func contextError(err error) error {
	var schema schemaError
	if errors.As(err, &schema) {
		return schema.rejection
	}
	return executedError{err}
}

func (s *server) decodeAndRun(r *http.Request, set *commandSet, name string, run commandFunc) (receipt, error) {
	cmd, err := s.decodeCommand(r, set, name, run)
	if err != nil {
		return receipt{}, err
	}
	res, err := run.run(r.Context(), cmd)
	if err != nil {
		return receipt{}, contextError(err)
	}
	return res, nil
}

// schemaError is a rejection found while decoding args, before the context saw the command.
type schemaError struct{ rejection *kernel.Rejection }

func (e schemaError) Error() string { return e.rejection.Error() }

// decodeArgs decodes args into a as a closed structure: exact-case member names, no unknown fields,
// types checked with their paths, which become paths under /args.
func decodeArgs(args json.RawMessage, a any, maxValues int) error {
	if err := strictjson.Unmarshal(args, a, strictjson.Limits{MaxBytes: int64(len(args)), MaxValues: maxValues}); err != nil {
		rej, ok := kernel.AsRejection(err)
		if !ok {
			return err
		}
		f := rej.Fields[0]
		path := "/args"
		if f.Path != "/" {
			path += f.Path
		}
		return schemaError{kernel.Reject(rej.Code, path, f.Reason)}
	}
	return nil
}

// emptyString returns the JSON Pointer of the first empty string in a valid JSON document, or "".
func emptyString(raw []byte) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return findEmpty(v, "")
}

func findEmpty(v any, path string) string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return path
		}
	case []any:
		for i, e := range t {
			if p := findEmpty(e, path+"/"+strconv.Itoa(i)); p != "" {
				return p
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if p := findEmpty(t[k], path+"/"+strings.NewReplacer("~", "~0", "/", "~1").Replace(k)); p != "" {
				return p
			}
		}
	}
	return ""
}

// handle builds a commandFunc that decodes A, converts it to the context's command C and runs it.
func handle[A any, C any, R result](build func(key string, a A) (C, error), call func(context.Context, C) (R, error)) commandFunc {
	return commandFunc{args: reflect.TypeFor[A](), cmd: reflect.TypeFor[C](),
		build: func(key string, args json.RawMessage, maxValues int) (any, error) {
			var a A
			if err := decodeArgs(args, &a, maxValues); err != nil {
				return nil, err
			}
			return build(key, a)
		},
		run: runner(call)}
}

// runner runs the context's command C and returns its closed receipt.
func runner[C any, R result](call func(context.Context, C) (R, error)) func(context.Context, any) (receipt, error) {
	return func(ctx context.Context, cmd any) (receipt, error) {
		r, err := call(ctx, cmd.(C))
		if err != nil {
			return receipt{}, err
		}
		v := struct {
			ActionID   string
			ObjectID   string
			RowVersion int64
			Status     string
			Replayed   bool
		}(r)
		return receipt{ActionID: v.ActionID, ObjectID: v.ObjectID, RowVersion: v.RowVersion, Status: v.Status, Replayed: v.Replayed}, nil
	}
}

// version returns a required expected_version.
func version(v *int64) (int64, error) {
	if v == nil {
		return 0, schemaError{kernel.Reject(kernel.CodeSchemaInvalid, "/args/expected_version", "required")}
	}
	return *v, nil
}

func evdCommands(c EVD) *commandSet {
	type registerSource struct {
		ID                string `json:"id"`
		ContentText       string `json:"content_text"`
		SourceKind        string `json:"source_kind"`
		Provider          string `json:"provider"`
		AccountRef        string `json:"account_ref"`
		ExternalMessageID string `json:"external_message_id"`
		AuthorClaim       string `json:"author_claim"`
	}
	type registerFile struct {
		ID               string `json:"id"`
		FileName         string `json:"file_name"`
		MimeType         string `json:"mime_type"`
		ContentBase64    string `json:"content_base64"`
		SupersedesFileID string `json:"supersedes_file_id"`
	}
	return &commandSet{bc: "evd", contract: evdapi.Contract, auditor: c, audit: c.Audit,
		// A 10 MiB file is about 13.4 MiB of Base64 (Python allows 15 MiB for this contract).
		limits: map[string]strictjson.Limits{evdapi.CommandRegisterFile: {MaxBytes: MaxFileBody}},
		commands: map[string]commandFunc{
			evdapi.CommandRegisterSource: handle(func(key string, a registerSource) (evdapi.RegisterSource, error) {
				return evdapi.RegisterSource{IdempotencyKey: key, SourceID: a.ID, ContentText: a.ContentText, SourceKind: a.SourceKind,
					Provider: a.Provider, AccountRef: a.AccountRef, ExternalMessageID: a.ExternalMessageID, AuthorClaim: a.AuthorClaim}, nil
			}, c.RegisterSource),
			evdapi.CommandRegisterFile: handle(func(key string, a registerFile) (evdapi.RegisterFile, error) {
				return evdapi.RegisterFile{IdempotencyKey: key, FileID: a.ID, FileName: a.FileName, MimeType: a.MimeType,
					ContentBase64: a.ContentBase64, SupersedesFileID: a.SupersedesFileID}, nil
			}, c.RegisterFile),
		}}
}

type evidenced struct {
	ID              string   `json:"id"`
	ExpectedVersion *int64   `json:"expected_version"`
	EvidenceIDs     []string `json:"evidence_ids"`
}

type reasoned struct {
	ID              string `json:"id"`
	ExpectedVersion *int64 `json:"expected_version"`
	Reason          string `json:"reason"`
}

func ptyCommands(c PTY) *commandSet {
	type address struct {
		AddressID string `json:"address_id"`
		Role      string `json:"role"`
		Address   string `json:"address"`
	}
	type saveParty struct {
		ID              string    `json:"id"`
		ExpectedVersion *int64    `json:"expected_version"`
		Name            string    `json:"name"`
		Roles           []string  `json:"roles"`
		Addresses       []address `json:"addresses"`
	}
	type saveContact struct {
		ID              string `json:"id"`
		ExpectedVersion *int64 `json:"expected_version"`
		Name            string `json:"name"`
		PartyID         string `json:"party_id"`
	}
	return &commandSet{bc: "pty", contract: ptyapi.Contract, auditor: c, audit: c.Audit, commands: map[string]commandFunc{
		ptyapi.CommandSaveParty: handle(func(key string, a saveParty) (ptyapi.SaveParty, error) {
			v, err := version(a.ExpectedVersion)
			cmd := ptyapi.SaveParty{IdempotencyKey: key, PartyID: a.ID, ExpectedVersion: v, Name: a.Name, Roles: a.Roles}
			for _, ad := range a.Addresses {
				cmd.Addresses = append(cmd.Addresses, ptyapi.Address(ad))
			}
			return cmd, err
		}, c.SaveParty),
		ptyapi.CommandConfirmParty: handle(func(key string, a evidenced) (ptyapi.ConfirmParty, error) {
			v, err := version(a.ExpectedVersion)
			return ptyapi.ConfirmParty{IdempotencyKey: key, PartyID: a.ID, ExpectedVersion: v, EvidenceIDs: a.EvidenceIDs}, err
		}, c.ConfirmParty),
		ptyapi.CommandReviseParty: handle(func(key string, a reasoned) (ptyapi.ReviseParty, error) {
			v, err := version(a.ExpectedVersion)
			return ptyapi.ReviseParty{IdempotencyKey: key, PartyID: a.ID, ExpectedVersion: v, Reason: a.Reason}, err
		}, c.ReviseParty),
		ptyapi.CommandDeactivateParty: handle(func(key string, a reasoned) (ptyapi.DeactivateParty, error) {
			v, err := version(a.ExpectedVersion)
			return ptyapi.DeactivateParty{IdempotencyKey: key, PartyID: a.ID, ExpectedVersion: v, Reason: a.Reason}, err
		}, c.DeactivateParty),
		ptyapi.CommandSaveContact: handle(func(key string, a saveContact) (ptyapi.SaveContact, error) {
			v, err := version(a.ExpectedVersion)
			return ptyapi.SaveContact{IdempotencyKey: key, ContactID: a.ID, ExpectedVersion: v, Name: a.Name, PartyID: a.PartyID}, err
		}, c.SaveContact),
		ptyapi.CommandConfirmContact: handle(func(key string, a evidenced) (ptyapi.ConfirmContact, error) {
			v, err := version(a.ExpectedVersion)
			return ptyapi.ConfirmContact{IdempotencyKey: key, ContactID: a.ID, ExpectedVersion: v, EvidenceIDs: a.EvidenceIDs}, err
		}, c.ConfirmContact),
		ptyapi.CommandReviseContact: handle(func(key string, a reasoned) (ptyapi.ReviseContact, error) {
			v, err := version(a.ExpectedVersion)
			return ptyapi.ReviseContact{IdempotencyKey: key, ContactID: a.ID, ExpectedVersion: v, Reason: a.Reason}, err
		}, c.ReviseContact),
		ptyapi.CommandVerifyServiceRole: handle(func(key string, a verifyServiceRole) (ptyapi.VerifyServiceRole, error) {
			v, err := version(a.ExpectedVersion)
			return ptyapi.VerifyServiceRole{IdempotencyKey: key, PartyID: a.ID, ExpectedVersion: v, ServiceRoleID: a.ServiceRoleID, Role: a.Role,
				ServiceCode: a.ServiceCode, Origin: a.Origin, Destination: a.Destination, ValidFrom: a.ValidFrom, ValidUntil: a.ValidUntil,
				EvidenceIDs: a.EvidenceIDs}, err
		}, c.VerifyServiceRole),
		ptyapi.CommandSaveTradingProfile: handle(func(key string, a saveTradingProfile) (ptyapi.SaveTradingProfile, error) {
			v, err := version(a.ExpectedVersion)
			cmd := ptyapi.SaveTradingProfile{IdempotencyKey: key, PartyID: a.ID, ExpectedVersion: v, PartyRevision: a.PartyRevision,
				PaymentTerms: ptyapi.TermsRef(a.PaymentTerms), TradeTerms: ptyapi.TermsRef(a.TradeTerms), Owner: ptyapi.OwnerRef(a.Owner)}
			for _, r := range a.ContactRefs {
				cmd.ContactRefs = append(cmd.ContactRefs, ptyapi.PinnedContact(r))
			}
			return cmd, err
		}, c.SaveTradingProfile),
		ptyapi.CommandPublishTradingProfile: handle(func(key string, a publishRevision) (ptyapi.PublishTradingProfile, error) {
			v, err := version(a.ExpectedVersion)
			return ptyapi.PublishTradingProfile{IdempotencyKey: key, PartyID: a.ID, ExpectedVersion: v, Revision: a.Revision,
				EvidenceIDs: a.EvidenceIDs, Reason: a.Reason}, err
		}, c.PublishTradingProfile),
		ptyapi.CommandRecordScreening: handle(func(key string, a recordScreening) (ptyapi.RecordScreening, error) {
			v, err := version(a.ExpectedVersion)
			return ptyapi.RecordScreening{IdempotencyKey: key, PartyID: a.ID, ExpectedVersion: v, RegulationID: a.RegulationID,
				RegulationRevision: a.RegulationRevision}, err
		}, c.RecordScreening),
		ptyapi.CommandDecideScreening: handle(func(key string, a decideScreening) (ptyapi.DecideScreening, error) {
			v, err := version(a.ExpectedVersion)
			return ptyapi.DecideScreening{IdempotencyKey: key, PartyID: a.ID, ExpectedVersion: v, ScreeningID: a.ScreeningID,
				Decision: a.Decision, Reason: a.Reason, RegisterPositive: a.RegisterPositive}, err
		}, c.DecideScreening),
		ptyapi.CommandBlockParty: handle(func(key string, a reasoned) (ptyapi.BlockParty, error) {
			v, err := version(a.ExpectedVersion)
			return ptyapi.BlockParty{IdempotencyKey: key, PartyID: a.ID, ExpectedVersion: v, Reason: a.Reason}, err
		}, c.BlockParty),
	}}
}

type verifyServiceRole struct {
	ID              string   `json:"id"`
	ExpectedVersion *int64   `json:"expected_version"`
	ServiceRoleID   string   `json:"service_role_id"`
	Role            string   `json:"role"`
	ServiceCode     string   `json:"service_code"`
	Origin          string   `json:"origin"`
	Destination     string   `json:"destination"`
	ValidFrom       string   `json:"valid_from"`
	ValidUntil      string   `json:"valid_until"`
	EvidenceIDs     []string `json:"evidence_ids"`
}

type termsRef struct {
	Kind     string `json:"kind"`
	ConfigID string `json:"id"`
	Revision int64  `json:"revision"`
}

type saveTradingProfile struct {
	ID              string   `json:"id"`
	ExpectedVersion *int64   `json:"expected_version"`
	PartyRevision   int64    `json:"party_revision"`
	PaymentTerms    termsRef `json:"payment_terms"`
	TradeTerms      termsRef `json:"trade_terms"`
	Owner           struct {
		ActorID string `json:"actor_id"`
		TeamID  string `json:"team_id"`
	} `json:"owner"`
	ContactRefs []struct {
		ContactID  string `json:"id"`
		RowVersion int64  `json:"row_version"`
	} `json:"contact_refs"`
}

// publishRevision publishes one revision of a versioned root on evidence (trading profiles, item
// code mappings).
type publishRevision struct {
	ID              string   `json:"id"`
	ExpectedVersion *int64   `json:"expected_version"`
	Revision        int64    `json:"revision"`
	EvidenceIDs     []string `json:"evidence_ids"`
	Reason          string   `json:"reason"`
}

type recordScreening struct {
	ID                 string `json:"id"`
	ExpectedVersion    *int64 `json:"expected_version"`
	RegulationID       string `json:"regulation_id"`
	RegulationRevision int64  `json:"regulation_revision"`
}

type decideScreening struct {
	ID               string `json:"id"`
	ExpectedVersion  *int64 `json:"expected_version"`
	ScreeningID      string `json:"screening_id"`
	Decision         string `json:"decision"`
	Reason           string `json:"reason"`
	RegisterPositive bool   `json:"register_positive"`
}

// MaxListImportBody and MaxListImportValues are the ingress limits of screening_list.import: up
// to refapi.MaxEntries entries with their aliases. Published lists are far smaller (the UN list
// about 1,000 entries, the OFAC SDN list about 18,000).
const (
	MaxListImportBody   = 32 << 20
	MaxListImportValues = 1_000_000
)

func refCommands(c REF) *commandSet {
	type entry struct {
		EntryID     string   `json:"entry_id"`
		PrimaryName string   `json:"primary_name"`
		Aliases     []string `json:"aliases"`
		EntityType  string   `json:"entity_type"`
		Country     string   `json:"country"`
	}
	type importList struct {
		ListCode     string  `json:"list_code"`
		Jurisdiction string  `json:"jurisdiction"`
		Version      string  `json:"version"`
		SourceName   string  `json:"source_name"`
		SourceURL    string  `json:"source_url"`
		PublishedOn  string  `json:"published_on"`
		FileSHA256   string  `json:"file_sha256"`
		EntryCount   int     `json:"entry_count"`
		Entries      []entry `json:"entries"`
	}
	return &commandSet{bc: "ref", contract: refapi.Contract, auditor: c, audit: c.Audit,
		limits: map[string]strictjson.Limits{refapi.CommandImportScreeningList: {MaxBytes: MaxListImportBody, MaxValues: MaxListImportValues}},
		commands: map[string]commandFunc{
			refapi.CommandImportScreeningList: handle(func(key string, a importList) (refapi.ImportScreeningList, error) {
				cmd := refapi.ImportScreeningList{IdempotencyKey: key, ListCode: a.ListCode, Jurisdiction: a.Jurisdiction, Version: a.Version,
					SourceName: a.SourceName, SourceURL: a.SourceURL, PublishedOn: a.PublishedOn, FileSHA256: a.FileSHA256, EntryCount: a.EntryCount}
				if a.Entries != nil {
					cmd.Entries = make([]refapi.ScreeningEntry, len(a.Entries))
					for i, e := range a.Entries {
						cmd.Entries[i] = refapi.ScreeningEntry(e)
					}
				}
				return cmd, nil
			}, c.ImportScreeningList),
		}}
}

func catCommands(c CAT) *commandSet {
	type saveSku struct {
		ID              string `json:"id"`
		ExpectedVersion *int64 `json:"expected_version"`
		Name            string `json:"name"`
		BaseUOM         string `json:"base_uom"`
	}
	return &commandSet{bc: "cat", contract: catapi.Contract, auditor: c, audit: c.Audit, commands: map[string]commandFunc{
		catapi.CommandSaveSku: handle(func(key string, a saveSku) (catapi.SaveSku, error) {
			v, err := version(a.ExpectedVersion)
			return catapi.SaveSku{IdempotencyKey: key, SkuID: a.ID, ExpectedVersion: v, Name: a.Name, BaseUnit: a.BaseUOM}, err
		}, c.SaveSku),
		catapi.CommandConfirmSku: handle(func(key string, a evidenced) (catapi.ConfirmSku, error) {
			v, err := version(a.ExpectedVersion)
			return catapi.ConfirmSku{IdempotencyKey: key, SkuID: a.ID, ExpectedVersion: v, EvidenceIDs: a.EvidenceIDs}, err
		}, c.ConfirmSku),
		catapi.CommandReviseSpec: handle(func(key string, a reasoned) (catapi.ReviseSpec, error) {
			v, err := version(a.ExpectedVersion)
			return catapi.ReviseSpec{IdempotencyKey: key, SkuID: a.ID, ExpectedVersion: v, Reason: a.Reason}, err
		}, c.ReviseSpec),
		catapi.CommandDeactivateSku: handle(func(key string, a reasoned) (catapi.DeactivateSku, error) {
			v, err := version(a.ExpectedVersion)
			return catapi.DeactivateSku{IdempotencyKey: key, SkuID: a.ID, ExpectedVersion: v, Reason: a.Reason}, err
		}, c.DeactivateSku),
		catapi.CommandSaveItemCodeMapping: handle(func(key string, a saveItemCodeMapping) (catapi.SaveItemCodeMapping, error) {
			v, err := version(a.ExpectedVersion)
			return catapi.SaveItemCodeMapping{IdempotencyKey: key, MappingID: a.ID, ExpectedVersion: v, PartyID: a.PartyID, PartyRole: a.PartyRole,
				ExternalCode: a.ExternalCode, SkuID: a.SkuID, SpecRevision: a.SpecRevision}, err
		}, c.SaveItemCodeMapping),
		catapi.CommandPublishItemCodeMapping: handle(func(key string, a publishRevision) (catapi.PublishItemCodeMapping, error) {
			v, err := version(a.ExpectedVersion)
			return catapi.PublishItemCodeMapping{IdempotencyKey: key, MappingID: a.ID, ExpectedVersion: v, Revision: a.Revision,
				EvidenceIDs: a.EvidenceIDs, Reason: a.Reason}, err
		}, c.PublishItemCodeMapping),
		catapi.CommandSaveDeclarationProfile:   reflected("ProfileID", "", c.SaveDeclarationProfile),
		catapi.CommandVerifyDeclarationProfile: reflected("ProfileID", "", c.VerifyDeclarationProfile),
	}}
}

type saveItemCodeMapping struct {
	ID              string `json:"id"`
	ExpectedVersion *int64 `json:"expected_version"`
	PartyID         string `json:"party_id"`
	PartyRole       string `json:"party_role"`
	ExternalCode    string `json:"external_code"`
	SkuID           string `json:"sku_id"`
	SpecRevision    int64  `json:"spec_revision"`
}

func inqCommands(c INQ) *commandSet {
	type line struct {
		LineID        string `json:"line_id"`
		SkuID         string `json:"sku_id"`
		Quantity      string `json:"quantity"`
		UOM           string `json:"uom"`
		RequestedItem string `json:"requested_item"`
		SourceID      string `json:"source_id"`
		Locator       string `json:"locator"`
	}
	type saveRfq struct {
		ID               string   `json:"id"`
		ExpectedVersion  *int64   `json:"expected_version"`
		CustomerID       string   `json:"customer_id"`
		ContactID        string   `json:"contact_id"`
		Lines            []line   `json:"lines"`
		SourceIDs        []string `json:"source_ids"`
		UnresolvedFields []string `json:"unresolved_fields"`
	}
	return &commandSet{bc: "inq", contract: inqapi.Contract, auditor: c, audit: c.Audit, reconcile: reconciler(c.Reconcile), commands: map[string]commandFunc{
		inqapi.CommandSaveRfq: handle(func(key string, a saveRfq) (inqapi.SaveRfq, error) {
			v, err := version(a.ExpectedVersion)
			cmd := inqapi.SaveRfq{IdempotencyKey: key, RfqID: a.ID, ExpectedVersion: v, CustomerID: a.CustomerID, ContactID: a.ContactID,
				SourceIDs: a.SourceIDs, UnresolvedFields: a.UnresolvedFields}
			if a.Lines != nil {
				cmd.Lines = make([]inqapi.RfqLine, len(a.Lines))
				for i, l := range a.Lines {
					cmd.Lines[i] = inqapi.RfqLine(l)
				}
			}
			return cmd, err
		}, c.SaveRfq),
		inqapi.CommandConfirmRfq: handle(func(key string, a evidenced) (inqapi.ConfirmRfqScope, error) {
			v, err := version(a.ExpectedVersion)
			return inqapi.ConfirmRfqScope{IdempotencyKey: key, RfqID: a.ID, ExpectedVersion: v, EvidenceIDs: a.EvidenceIDs}, err
		}, c.ConfirmRfqScope),
		inqapi.CommandCancelRfq: handle(func(key string, a reasoned) (inqapi.CancelRfq, error) {
			v, err := version(a.ExpectedVersion)
			return inqapi.CancelRfq{IdempotencyKey: key, RfqID: a.ID, ExpectedVersion: v, Reason: a.Reason}, err
		}, c.CancelRfq),
		inqapi.CommandCloseRfq: handle(func(key string, a reasoned) (inqapi.CloseRfq, error) {
			v, err := version(a.ExpectedVersion)
			return inqapi.CloseRfq{IdempotencyKey: key, RfqID: a.ID, ExpectedVersion: v, Reason: a.Reason}, err
		}, c.CloseRfq),
	}}
}
