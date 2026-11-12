package shipment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The guards a confirmation names in its evidence. GuardTradeCompliance, GuardExportLicense,
// GuardPaymentCondition and GuardCommercialRelease are extension points until S5 unit C1: an
// extension point no guard is wired to has the value NotWired and is listed as unchecked, never as
// passed.
const (
	GuardTradeCompliance   = "cnx.trade_compliance_guard.v1"
	GuardExportLicense     = "cnx.export_license_guard.v1"
	GuardPartyScreening    = "pty.party_screening_guard.v1"
	GuardSupplierDelivery  = "pur.supplier_delivery_guard.v1"
	GuardQualityRelease    = "qlt.quality_release_guard.v1"
	GuardPaymentCondition  = "stl.payment_condition_guard.v1"
	GuardOrder             = "trd.order_guard.v1"
	GuardCommercialRelease = "ful.commercial_release.v1"
	NotWired               = "not_wired"
)

// Entry is one named value of a confirmation's guard evidence.
type Entry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Confirmation is the confirmation of a shipment: who confirmed it when, the evidence cited, what
// every guard answered in the confirming transaction (Entries, sealed as the canonical JSON
// EntriesJSON with its SHA-256) and the extension points found not wired.
type Confirmation struct {
	Confirmed     Act
	Evidence      []string
	Entries       []Entry
	EntriesJSON   string
	EntriesSHA256 string
	Unchecked     []string
}

// Seal returns entries as canonical JSON ([{"name": ..., "value": ...}, ...]) and its SHA-256.
func Seal(entries []Entry) (string, string) {
	if entries == nil {
		entries = []Entry{}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(entries) // strings only: never fails
	doc := bytes.TrimSpace(b.Bytes())
	sum := sha256.Sum256(doc)
	return string(doc), hex.EncodeToString(sum[:])
}

// DeliveryUse is what the shipment's lines ship together of one supplier delivery line at the
// version they bind; Line is the index of the first line that names it.
type DeliveryUse struct {
	Ref      DeliveryRef
	Version  int64
	Quantity kernel.Decimal
	Line     int
}

// ReleaseUse is what the shipment's lines ship together under one quality release.
type ReleaseUse struct {
	ID       string
	Quantity kernel.Decimal
	Line     int
}

// OrderLineUse is what the shipment's lines ship together of one order line, which orders Ordered
// in the unit named at the order revision the shipment binds.
type OrderLineUse struct {
	OrderLineID  string
	UnitID       string
	UnitRevision int64
	Ordered      kernel.Decimal
	Quantity     kernel.Decimal
	Line         int
}

// Deliveries returns what the shipment ships of each supplier delivery line, in delivery and line
// ID order.
func (s *Shipment) Deliveries() []DeliveryUse {
	var out []DeliveryUse
	for i, l := range s.Lines {
		at := slices.IndexFunc(out, func(u DeliveryUse) bool { return u.Ref == l.Delivery })
		if at < 0 {
			out = append(out, DeliveryUse{Ref: l.Delivery, Version: l.DeliveryVersion, Line: i})
			at = len(out) - 1
		}
		out[at].Quantity = out[at].Quantity.Add(l.Quantity)
	}
	slices.SortFunc(out, func(a, b DeliveryUse) int { return a.Ref.Compare(b.Ref) })
	return out
}

// Releases returns what the shipment ships under each quality release, in release ID order.
func (s *Shipment) Releases() []ReleaseUse {
	var out []ReleaseUse
	for i, l := range s.Lines {
		at := slices.IndexFunc(out, func(u ReleaseUse) bool { return u.ID == l.ReleaseID })
		if at < 0 {
			out = append(out, ReleaseUse{ID: l.ReleaseID, Line: i})
			at = len(out) - 1
		}
		out[at].Quantity = out[at].Quantity.Add(l.Quantity)
	}
	slices.SortFunc(out, func(a, b ReleaseUse) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	return out
}

// OrderLines returns what the shipment ships of each order line, in order line ID order.
func (s *Shipment) OrderLines() []OrderLineUse {
	var out []OrderLineUse
	for i, l := range s.Lines {
		at := slices.IndexFunc(out, func(u OrderLineUse) bool { return u.OrderLineID == l.OrderLineID })
		if at < 0 {
			out = append(out, OrderLineUse{OrderLineID: l.OrderLineID, UnitID: l.UnitID, UnitRevision: l.UnitRevision, Ordered: l.OrderQuantity, Line: i})
			at = len(out) - 1
		}
		out[at].Quantity = out[at].Quantity.Add(l.Quantity)
	}
	slices.SortFunc(out, func(a, b OrderLineUse) int {
		switch {
		case a.OrderLineID < b.OrderLineID:
			return -1
		case a.OrderLineID > b.OrderLineID:
			return 1
		}
		return 0
	})
	return out
}

// ScreenedParties returns the IDs of the parties the confirmation screens, in party ID order.
func (s *Shipment) ScreenedParties() []string {
	var out []string
	for _, p := range s.Parties {
		if !slices.Contains(out, p.PartyID) {
			out = append(out, p.PartyID)
		}
	}
	slices.Sort(out)
	return out
}

// StoredLinePath is the path of member of the shipment's line i in a refusal of a command that
// names the shipment only.
func StoredLinePath(i int, member string) string {
	return "/shipment/lines/" + strconv.Itoa(i) + "/" + member
}

// ScreeningVerdict is PTY's PartyScreeningGuard v1 on a party at the confirmation's instant.
type ScreeningVerdict struct {
	Clear         bool
	Status        string
	Reason        string
	ScreeningID   string
	PartyRevision int64
}

// CheckScreening judges PTY's verdict on party id of the shipment: anything but clear is
// PARTY_SCREENING_NOT_CLEAR (R44; no exception overrides it, FUL-INV-002).
func (s *Shipment) CheckScreening(id string, v ScreeningVerdict) (Entry, error) {
	e := Entry{Name: "screening:" + id, Value: v.Status + "/" + v.Reason + "/" + v.ScreeningID}
	if v.Clear {
		return e, nil
	}
	at := slices.IndexFunc(s.Parties, func(p Party) bool { return p.PartyID == id })
	role := ""
	if at >= 0 {
		role = " (" + s.Parties[at].Role + ")"
	}
	return e, Reject(CodePartyScreeningNotClear, "/shipment/parties/"+strconv.Itoa(max(at, 0)),
		"Party "+id+role+" screening is "+v.Status+": "+v.Reason)
}

// DeliveryVerdict is PUR's SupplierDeliveryGuard v1 on a delivery line at the version the shipment
// binds: Known false for a delivery, version or line PUR does not have; otherwise the version's
// status, the version verified now, what the line is of, the verified quantity, and Code — "" when
// the version bound is the verified one.
type DeliveryVerdict struct {
	Known           bool
	Status          string
	VerifiedVersion int64
	Scope           Scope
	Verified        kernel.Decimal
	Code            string
}

// CheckDelivery judges PUR's verdict on a delivery line the shipment ships from, and what the
// shipment ships of it with what FUL's confirmed shipments shipped of it before: an unknown line
// and a claim are SUPPLIER_DELIVERY_UNVERIFIED (a supplier's claim never counts), a superseded
// version REFERENCE_STALE, a line no longer of every shipment line that names it REFERENCE_INVALID,
// and more than verified SUPPLIER_DELIVERY_INSUFFICIENT (FUL-INV-003).
func (s *Shipment) CheckDelivery(u DeliveryUse, v DeliveryVerdict, before kernel.Decimal) (Entry, error) {
	e := Entry{Name: "supplier_delivery:" + u.Ref.DeliveryID + "/" + u.Ref.LineID + "/v" + strconv.FormatInt(u.Version, 10),
		Value: v.Status + "/verified_version=" + strconv.FormatInt(v.VerifiedVersion, 10) + "/verified=" + v.Verified.String()}
	path := StoredLinePath(u.Line, "supplier_delivery")
	switch {
	case !v.Known:
		return e, Reject(CodeSupplierDeliveryUnverified, path, "PUR has no such supplier delivery line")
	case v.Code != "":
		return e, Upstream(v.Code, path, "The supplier delivery is "+v.Status)
	}
	for i, l := range s.Lines {
		if l.Delivery != u.Ref {
			continue
		}
		g := v.Scope
		if g.OrderID != s.Order.OrderID || g.OrderLineID != l.OrderLineID || g.SkuID != l.SkuID || g.SpecRevision != l.SpecRevision ||
			g.UnitID != l.UnitID || g.UnitRevision != l.UnitRevision || g.SupplierID != l.SupplierID || g.CommitmentID != l.CommitmentID {
			return e, Reject(kernel.CodeReferenceInvalid, StoredLinePath(i, "supplier_delivery"), "The supplier delivery line is not of this line")
		}
	}
	return e, WithinDelivery(before, u.Quantity, v.Verified, StoredLinePath(u.Line, "quantity"))
}

// ReleaseVerdict is QLT's QualityReleaseGuard v1 on a release at the confirmation's instant: Known
// false for a release QLT does not have; otherwise its status, what it is of, the released
// quantity, the end of its validity and Code — "" when it serves a shipment then.
type ReleaseVerdict struct {
	Known       bool
	Status      string
	Code        string
	SubjectKind string
	Scope       Scope
	Quantity    kernel.Decimal
	ValidUntil  time.Time
}

// CheckRelease judges QLT's verdict on a release the shipment ships under, and what the shipment
// ships under it with what FUL's confirmed shipments shipped under it before: unknown, revoked or
// outside its validity is QUALITY_RELEASE_REQUIRED; a release that is not of every line that names
// it (order line, specification, unit, supplier, purchase commitment) is REFERENCE_INVALID; more
// than released is QUALITY_RELEASE_INSUFFICIENT.
func (s *Shipment) CheckRelease(u ReleaseUse, v ReleaseVerdict, before kernel.Decimal) (Entry, error) {
	e := Entry{Name: "quality_release:" + u.ID, Value: v.Status + "/released=" + v.Quantity.String()}
	path := StoredLinePath(u.Line, "quality_release_id")
	switch {
	case !v.Known:
		return e, Reject(CodeQualityReleaseRequired, path, "The quality release is unknown")
	case v.Code != "" && v.Status != "granted":
		return e, Reject(CodeQualityReleaseRequired, path, "The quality release was "+v.Status)
	case v.Code != "":
		return e, Reject(CodeQualityReleaseRequired, path, "The quality release is not valid at this instant")
	}
	e.Value += "/valid_until=" + kernel.FormatTimestamp(v.ValidUntil)
	for i, l := range s.Lines {
		if l.ReleaseID != u.ID {
			continue
		}
		g := v.Scope
		if g.OrderID != s.Order.OrderID || g.OrderLineID != l.OrderLineID || g.SkuID != l.SkuID || g.SpecRevision != l.SpecRevision ||
			g.UnitID != l.UnitID || g.UnitRevision != l.UnitRevision || g.SupplierID != l.SupplierID ||
			(v.SubjectKind == SubjectCommitmentLine && g.CommitmentID != l.CommitmentID) {
			return e, Reject(kernel.CodeReferenceInvalid, StoredLinePath(i, "quality_release_id"), "The quality release is not of this line")
		}
	}
	return e, WithinRelease(before, u.Quantity, v.Quantity, StoredLinePath(u.Line, "quantity"))
}

// OrderVerdict is TRD's OrderGuard v1 on an order binding: Known false for an order TRD does not
// have; otherwise its state, current revision, whether its activating approval was revoked and Code
// — "" when it is active at the bound revision and hash with its activating approval in force.
type OrderVerdict struct {
	Known           bool
	Status          string
	OrderRevision   int64
	ApprovalRevoked bool
	Code            string
}

// CheckOrder judges TRD's verdict on the order at path: an unknown order is REFERENCE_INVALID, and
// TRD's refusal its code — REFERENCE_STALE for an order at another revision or hash than the
// shipment binds, ORDER_NOT_ACTIVE, ORDER_APPROVAL_REVOKED.
func CheckOrder(b OrderBinding, v OrderVerdict, path string) (Entry, error) {
	e := Entry{Name: GuardOrder, Value: v.Status + "/r" + strconv.FormatInt(v.OrderRevision, 10)}
	switch {
	case !v.Known:
		return e, Reject(kernel.CodeReferenceInvalid, path, "TRD has no such order")
	case v.Code == CodeReferenceStale:
		return e, Reject(CodeReferenceStale, path, "The order is at revision "+strconv.FormatInt(v.OrderRevision, 10)+
			", the shipment binds revision "+strconv.FormatInt(b.OrderRevision, 10)+" at its hash")
	case v.Code != "":
		return e, Upstream(v.Code, path, "The order is "+v.Status)
	}
	return e, nil
}

// Confirmable refuses a shipment that is not prepared (INVALID_STATE) or is at another row version
// than expected (VERSION_CONFLICT).
func (s *Shipment) Confirmable(expected int64) error { return s.open(expected) }

// Confirm confirms a prepared shipment (SM-FUL-SHIPMENT prepared → confirmed) by a human, citing
// evidence, with the guard evidence entries of the confirming transaction and the extension points
// found not wired. The caller has run Confirmable, every guard and the quantity rules.
func (s *Shipment) Confirm(evidence []string, entries []Entry, unchecked []string, act Act) error {
	if !act.By.IsHuman() {
		return Reject(kernel.CodePermissionDenied, "/", "")
	}
	if s.Status != StatusPrepared {
		return Reject(kernel.CodeInvalidState, "/args/shipment_id", "The shipment is "+s.Status)
	}
	act.At = kernel.Instant(act.At)
	doc, sum := Seal(entries)
	if unchecked == nil {
		unchecked = []string{}
	}
	if evidence == nil {
		evidence = []string{}
	}
	s.Status, s.RowVersion = StatusConfirmed, s.RowVersion+1
	s.Confirmation = &Confirmation{Confirmed: act, Evidence: evidence, Entries: entries, EntriesJSON: doc, EntriesSHA256: sum, Unchecked: unchecked}
	return nil
}
