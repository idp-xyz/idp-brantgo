package api

import (
	"context"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Supplier deliveries on the direct delivery route (WBS S5-15; tactical model
// PUR-AGG-SupplierDelivery, SM-PUR-SUPPLIER-DELIVERY, PUR-INV-008; UL PUR-SupplierDelivery, UL-INV-32;
// fact authority MD-45; S5 decisions D-3 and D-10; event storming ES-12; TD-AT14). Python V0.50 has
// nothing for them.
//
//	supplier_delivery.record  human or AI: what a supplier handed over against lines of one issued or
//	                          confirmed purchase commitment — a quantity per commitment line, the
//	                          handover place (UN/LOCODE), the time, the EVD evidence (sources, and
//	                          business files such as the forwarder's cargo receipt, the loading list
//	                          and photos). It is a claim: it never counts. With expected_version 0
//	                          it records a new delivery; with the delivery's row version it records
//	                          the delivery's next version, a correction with its reason, once no
//	                          claim of the delivery awaits verification.
//	supplier_delivery.verify  human only, not the recorder of the claim
//	                          (INDEPENDENT_REVIEW_REQUIRED): verify — every line of the claim with
//	                          the quantity the evidence shows, never above the claimed one, and for
//	                          each commitment line never, with its other verified deliveries, above
//	                          what the commitment buys (SUPPLIER_DELIVERY_EXCEEDED; no tolerance in
//	                          S5) — or reject, with a reason. A verified correction supersedes the
//	                          version verified before, which it may bring down to nothing. Only
//	                          verified quantities are a source of shipments (FUL) and of purchase
//	                          settlement (STL).
//
// A delivery line is its commitment line: it is named by the order line the commitment's draft buys
// (order_line_id), in every version of the delivery. Other contexts name it DeliveryLineRef
// {DeliveryID, LineID, Version} with that order line as LineID.
//
// Request members in snake_case: supplier_delivery.record {delivery_id, expected_version,
// commitment_id, lines [{order_line_id, quantity}], handover_place, handed_over_at, evidence
// [{source_id, locator}], file_ids (optional), supplier_reference (optional), reason (a
// correction's)}; supplier_delivery.verify {delivery_id, expected_version, decision (verify or
// reject), lines [{order_line_id, verified_quantity}] (verify), reason (reject; optional to verify),
// evidence [{source_id, locator}] (optional)}. Timestamps are the contracts' UTC form with a
// literal Z.

// Delivery commands, reads and events (command catalog, Context Map PUR commands and facts).
const (
	CommandRecordSupplierDelivery = "supplier_delivery.record"
	CommandVerifySupplierDelivery = "supplier_delivery.verify"

	// Reads: one delivery and the deliveries of a commitment (pur.read).
	CommandGetDelivery             = "supplier_delivery.get"
	CommandGetCommitmentDeliveries = "supplier_delivery.list"

	EventSupplierDeliveryVerified = "idp-brant.pur.supplier_delivery_verified.v1"

	// States of a delivery version (SM-PUR-SUPPLIER-DELIVERY). A delivery's own status is verified
	// while one of its versions is the verified one, otherwise that of its latest claim.
	DeliveryRecorded   = "recorded"
	DeliveryVerified   = "verified"
	DeliveryRejected   = "rejected"
	DeliverySuperseded = "superseded"

	DecisionVerify = "verify"

	MaxFiles            = 20
	MaxDeliveryLineRefs = 200
)

// DeliveryClaim is one line of a claim: Quantity (kernel.QuantityFormat, positive, in the commitment
// line's unit) of the commitment line that buys order line OrderLineID.
type DeliveryClaim struct {
	OrderLineID string
	Quantity    string
}

// RecordSupplierDelivery records a claim of delivery DeliveryID against commitment CommitmentID:
// ExpectedVersion 0 a new delivery, otherwise the delivery's row version and its next version, a
// correction for Reason. Human or AI.
type RecordSupplierDelivery struct {
	IdempotencyKey    string
	DeliveryID        string
	ExpectedVersion   int64
	CommitmentID      string
	Lines             []DeliveryClaim
	HandoverPlace     string
	HandedOverAt      string
	Evidence          []Evidence
	FileIDs           []string
	SupplierReference string
	Reason            string
}

// DeliveryVerifiedLine is what a verification states of one line of the claim: VerifiedQuantity
// (kernel.QuantityFormat, zero or more) of the line OrderLineID.
type DeliveryVerifiedLine struct {
	OrderLineID      string
	VerifiedQuantity string
}

// VerifySupplierDelivery decides the claim of delivery DeliveryID that awaits verification, at the
// delivery's row version ExpectedVersion: Decision verify with every line of the claim in Lines, or
// reject with Reason. Evidence is what the verifier adds to the claim's. Human only.
type VerifySupplierDelivery struct {
	IdempotencyKey  string
	DeliveryID      string
	ExpectedVersion int64
	Decision        string
	Lines           []DeliveryVerifiedLine
	Reason          string
	Evidence        []Evidence
}

func (RecordSupplierDelivery) purCommand() {}
func (VerifySupplierDelivery) purCommand() {}

// Location reports whether s is an exact location code: a UN/LOCODE of a two-letter country and a
// three-character place ("CNNGB"), as PTY and SUP write places.
func Location(s string) bool {
	if len(s) != 5 {
		return false
	}
	for i := 0; i < 5; i++ {
		c := s[i]
		upper := c >= 'A' && c <= 'Z'
		if !upper && (i < 2 || c < '2' || c > '9') {
			return false
		}
	}
	return true
}

// Validate checks the closed contract: 1 to MaxLines lines with a positive quantity each, a
// commitment line at most once (DUPLICATE_LINE_ID), a UN/LOCODE, a timestamp, 1 to MaxEvidence
// evidence entries, at most MaxFiles distinct files; a new delivery (expected version 0) states no
// reason, a correction states one.
func (c RecordSupplierDelivery) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DeliveryID):
		return schema("/args/delivery_id")
	case c.ExpectedVersion < 0 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case !kernel.ValidIdentifier(c.CommitmentID):
		return schema("/args/commitment_id")
	case len(c.Lines) < 1 || len(c.Lines) > MaxLines:
		return schema("/args/lines")
	}
	seen := map[string]bool{}
	for i, l := range c.Lines {
		p := index("/args/lines", i)
		switch {
		case !kernel.ValidIdentifier(l.OrderLineID):
			return schema(p + "/order_line_id")
		case quantity(l.Quantity, p+"/quantity") != nil:
			return schema(p + "/quantity")
		case seen[l.OrderLineID]:
			return kernel.Reject(kernel.CodeDuplicateLineID, p+"/order_line_id", "")
		}
		seen[l.OrderLineID] = true
	}
	switch {
	case !Location(c.HandoverPlace):
		return schema("/args/handover_place")
	case !timestamp(c.HandedOverAt):
		return schema("/args/handed_over_at")
	case len(c.FileIDs) > MaxFiles:
		return schema("/args/file_ids")
	case c.SupplierReference != "" && !text(c.SupplierReference, MaxSupplierRef):
		return schema("/args/supplier_reference")
	case (c.ExpectedVersion == 0) != (c.Reason == ""), c.Reason != "" && !Reason(c.Reason):
		return schema("/args/reason")
	}
	files := map[string]bool{}
	for i, id := range c.FileIDs {
		if !kernel.ValidIdentifier(id) || files[id] {
			return schema(index("/args/file_ids", i))
		}
		files[id] = true
	}
	return validEvidence(c.Evidence, "/args/evidence")
}

// Validate checks the closed contract: a verification states 1 to MaxLines lines, each once, with a
// quantity of zero or more; a rejection states none, and its reason; evidence is optional.
func (c VerifySupplierDelivery) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DeliveryID):
		return schema("/args/delivery_id")
	case !version(c.ExpectedVersion):
		return schema("/args/expected_version")
	case c.Reason != "" && !Reason(c.Reason):
		return schema("/args/reason")
	}
	switch c.Decision {
	case DecisionVerify:
		if len(c.Lines) < 1 || len(c.Lines) > MaxLines {
			return schema("/args/lines")
		}
		seen := map[string]bool{}
		for i, l := range c.Lines {
			p := index("/args/lines", i)
			q, err := kernel.ParseDecimal(l.VerifiedQuantity, kernel.QuantityFormat)
			switch {
			case !kernel.ValidIdentifier(l.OrderLineID):
				return schema(p + "/order_line_id")
			case err != nil || q.Sign() < 0:
				return schema(p + "/verified_quantity")
			case seen[l.OrderLineID]:
				return kernel.Reject(kernel.CodeDuplicateLineID, p+"/order_line_id", "")
			}
			seen[l.OrderLineID] = true
		}
	case DecisionReject:
		switch {
		case c.Lines != nil:
			return schema("/args/lines")
		case c.Reason == "":
			return schema("/args/reason")
		}
	default:
		return schema("/args/decision")
	}
	if c.Evidence == nil {
		return nil
	}
	return validEvidence(c.Evidence, "/args/evidence")
}

// DeliveryReceipt is the answer of both delivery commands: the receipt (ObjectID the delivery ID,
// RowVersion the delivery's row version after the command, Status the state of the version the
// command recorded or decided: recorded, verified or rejected) and what the delivery binds and
// counts now: its commitment, order and supplier, its latest version and the version verified now
// (0 when none is). A replay, and a record of a new delivery answered from the stored one, are
// Replayed.
type DeliveryReceipt struct {
	Receipt
	CommitmentID    string
	OrderID         string
	SupplierID      string
	LatestVersion   int64
	VerifiedVersion int64
}

// DeliveryFile is an EVD business file a claim cites, with the SHA-256 it had then.
type DeliveryFile struct {
	FileID string
	SHA256 string
}

// DeliveryLineView is one line of a delivery version: the commitment line (the order line it buys,
// which is the delivery line's ID), its SKU specification revision and unit, the claimed quantity
// and, once the version was verified, the verified one.
type DeliveryLineView struct {
	OrderLineID      string
	SkuID            string
	SpecRevision     int64
	UnitID           string
	UnitRevision     int64
	ClaimedQuantity  string
	VerifiedQuantity string
}

// DeliveryVersionView is one claim of a delivery and what became of it. Zero times and empty actors
// mean the step has not happened.
type DeliveryVersionView struct {
	Version             int64
	Status              string
	HandoverPlace       string
	HandedOverAt        time.Time
	SupplierReference   string
	CorrectionReason    string
	Evidence            []Evidence
	Files               []DeliveryFile
	Lines               []DeliveryLineView
	RecordedBy          kernel.ActorRef
	RecordedAt          time.Time
	RecordEpoch         int64
	Decision            string
	DecidedBy           kernel.ActorRef
	DecidedAt           time.Time
	DecideEpoch         int64
	DecisionReason      string
	DecisionEvidence    []Evidence
	SupersededByVersion int64
	SupersededAt        time.Time
}

// DeliveryView is a delivery as the reads show it: its commitment with the purchase order number,
// order and supplier, its status now (verified while a version is the verified one, otherwise the
// state of its latest claim), the version verified now and the one awaiting verification (0: none),
// and every version.
type DeliveryView struct {
	DeliveryID      string
	Status          string
	RowVersion      int64
	CommitmentID    string
	PONumber        string
	OrderID         string
	SupplierID      string
	LatestVersion   int64
	VerifiedVersion int64
	PendingVersion  int64
	Versions        []DeliveryVersionView
}

// CommitmentLineDeliveries is what the deliveries of a commitment add up to for one of its lines:
// what the line buys, what is verified now over all its deliveries (claims never count) and what
// may still be verified.
type CommitmentLineDeliveries struct {
	OrderLineID       string
	SkuID             string
	SpecRevision      int64
	UnitID            string
	UnitRevision      int64
	PurchaseQuantity  string
	VerifiedQuantity  string
	RemainingQuantity string
}

// CommitmentDeliveriesView is the deliveries of one commitment: the commitment's status and
// purchase order number, the standing of each of its lines and the deliveries by delivery ID.
type CommitmentDeliveriesView struct {
	CommitmentID string
	Status       string
	PONumber     string
	OrderID      string
	SupplierID   string
	Lines        []CommitmentLineDeliveries
	Deliveries   []DeliveryView
}

// DeliveryCommands is PUR's command entry point for supplier deliveries. Errors are
// *kernel.Rejection for refusals.
type DeliveryCommands interface {
	RecordSupplierDelivery(ctx context.Context, cmd RecordSupplierDelivery) (DeliveryReceipt, error)
	VerifySupplierDelivery(ctx context.Context, cmd VerifySupplierDelivery) (DeliveryReceipt, error)
}

// DeliveryQueries is PUR's audited read side of supplier deliveries.
type DeliveryQueries interface {
	// GetDelivery returns a delivery the caller sees, audited as supplier_delivery.get (pur.read):
	// NOT_FOUND otherwise; REFERENCE_INVALID when the caller no longer reads its order.
	GetDelivery(ctx context.Context, deliveryID string) (DeliveryView, error)
	// GetCommitmentDeliveries returns the deliveries of a commitment the caller sees, with the
	// standing of each commitment line, audited as supplier_delivery.list (pur.read): NOT_FOUND
	// otherwise; REFERENCE_INVALID when the caller no longer reads its order.
	GetCommitmentDeliveries(ctx context.Context, commitmentID string) (CommitmentDeliveriesView, error)
}

// SupplierDeliveryGuard v1 (Context Map R59 PUR→FUL, "Customer／Supplier（守卫函数）", guardLock true:
// 直发路线的已核实供应商交付; risk 出运量超过核实交付量; FUL-INV-003; ADR-BR-004 option B, ADR-BR-011). FUL
// reads a delivery line before its own transaction through DeliveryLookups and binds it as
// DeliveryLineRef; inside its transaction, after it locked its own roots FOR UPDATE, it re-checks
// the line with the guard and keeps the delivery so until that transaction ends. FUL keeps its own
// count of what it shipped of a delivery line (DeliveryID, LineID) across versions; PUR knows
// nothing of shipments (SUPPLIER_DELIVERY_INSUFFICIENT is FUL's).
//
// The guard is a SECURITY DEFINER function in PUR's schema with a fixed search_path
// (migrations/pur/000003_pur_supplier_deliveries.sql):
//
//	brant_pur.guard_supplier_delivery_v1(p_delivery_id text, p_line_id text, p_version bigint)
//	    RETURNS TABLE (known boolean, status text, verified_version bigint, commitment_id text, order_id text,
//	                   order_line_id text, supplier_id text, sku_id text, spec_revision bigint, unit_id text,
//	                   unit_revision bigint, verified_quantity text, code text)
//
// It always returns one row in the caller's scope (app.tenant_id and app.company_id) and locks the
// one delivery root row FOR SHARE in the caller's transaction. supplier_delivery.record and
// supplier_delivery.verify lock that row FOR UPDATE and change it, so a correction either committed
// before the guard read the row, and the guard answers REFERENCE_STALE for the superseded version,
// or waits until the caller's transaction ends; a guard called while a delivery command holds the
// row waits for it and reads the delivery as the command left it. The guard locks nothing else,
// and PUR never locks a FUL row. A caller locks its own root first:
//
//	ref := purapi.DeliveryLineRef{DeliveryID: id, LineID: orderLineID, Version: version}
//	row := exec.QueryRow(ctx, purapi.SupplierDeliveryGuardSQL, purapi.SupplierDeliveryGuardArgs(ref)...)
//	verdict, err := purapi.ScanSupplierDeliveryGuard(ref, row.Scan)
//	if err == nil {
//		err = verdict.Check("/args/lines/0/supplier_delivery")
//	}
//
// Code is empty when the version is the delivery's verified one: VerifiedQuantity is then what may
// ship of the line in total, in the line's unit. It is SUPPLIER_DELIVERY_UNVERIFIED (409) for an
// unknown delivery, version or line (known false, every other column NULL) and for a claim recorded
// or rejected — a claim never counts —, and REFERENCE_STALE (409) for a version a correction
// superseded, with VerifiedVersion the version verified now. A correction that only awaits its
// verification changes nothing. The guard does not apply the caller's data scope (own, team): the
// caller checks visibility when it resolves the line before its transaction (DeliveryLookups).
const (
	SupplierDeliveryGuardFunction = "brant_pur.guard_supplier_delivery_v1"
	SupplierDeliveryGuardSQL      = `SELECT known, status, verified_version, commitment_id, order_id, order_line_id, supplier_id, sku_id,
		spec_revision, unit_id, unit_revision, verified_quantity, code FROM brant_pur.guard_supplier_delivery_v1($1, $2, $3)`
)

// DeliveryLineRef names a line of a supplier delivery at a version of the delivery: LineID is the
// order line the line's commitment line buys.
type DeliveryLineRef struct {
	DeliveryID string
	LineID     string
	Version    int64
}

// SupplierDeliveryGuardArgs returns the arguments of SupplierDeliveryGuardSQL for ref.
func SupplierDeliveryGuardArgs(ref DeliveryLineRef) []any {
	return []any{ref.DeliveryID, ref.LineID, ref.Version}
}

// SupplierDeliveryVerdict is the guard's answer for Ref: Known false for a delivery, version or line
// that does not exist in the caller's company; otherwise the version's status, the version verified
// now (0: none), the commitment, order, order line and supplier, the SKU specification revision and
// unit, the verified quantity (verified versions only) and Code ("" when the line serves a
// shipment).
type SupplierDeliveryVerdict struct {
	Ref              DeliveryLineRef
	Known            bool
	Status           string
	VerifiedVersion  int64
	CommitmentID     string
	OrderID          string
	OrderLineID      string
	SupplierID       string
	SkuID            string
	SpecRevision     int64
	UnitID           string
	UnitRevision     int64
	VerifiedQuantity string
	Code             string
}

// ScanSupplierDeliveryGuard reads the row of SupplierDeliveryGuardSQL for ref through scan, such as
// a pgx row's Scan.
func ScanSupplierDeliveryGuard(ref DeliveryLineRef, scan func(dest ...any) error) (SupplierDeliveryVerdict, error) {
	v := SupplierDeliveryVerdict{Ref: ref}
	var status, commitment, order, line, supplier, sku, unit, quantity, code *string
	var verified, spec, unitRev *int64
	if err := scan(&v.Known, &status, &verified, &commitment, &order, &line, &supplier, &sku, &spec, &unit, &unitRev, &quantity, &code); err != nil {
		return SupplierDeliveryVerdict{}, err
	}
	for _, f := range []struct {
		from *string
		to   *string
	}{{status, &v.Status}, {commitment, &v.CommitmentID}, {order, &v.OrderID}, {line, &v.OrderLineID}, {supplier, &v.SupplierID},
		{sku, &v.SkuID}, {unit, &v.UnitID}, {quantity, &v.VerifiedQuantity}, {code, &v.Code}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	for _, f := range []struct {
		from *int64
		to   *int64
	}{{verified, &v.VerifiedVersion}, {spec, &v.SpecRevision}, {unitRev, &v.UnitRevision}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	return v, nil
}

// Usable reports whether the line serves a shipment: its version is the delivery's verified one.
func (v SupplierDeliveryVerdict) Usable() bool { return v.Known && v.Code == "" }

// Check returns nil for a usable line; otherwise REFERENCE_STALE (409) for a superseded version and
// SUPPLIER_DELIVERY_UNVERIFIED (409) for anything else, at path of the caller's request.
func (v SupplierDeliveryVerdict) Check(path string) error {
	return deliveryCheck(v.Known, v.Status, v.Code, v.VerifiedVersion, path)
}

func deliveryCheck(known bool, status, code string, verified int64, path string) error {
	switch {
	case known && code == "":
		return nil
	case !known:
		return kernel.RejectAs(CodeSupplierDeliveryUnverified, kernel.CategoryConflict, path, "No such supplier delivery line")
	case code == CodeReferenceStale:
		reason := "A correction superseded this version of the supplier delivery"
		if verified > 0 {
			reason += "; version " + strconv.FormatInt(verified, 10) + " is the verified one"
		}
		return kernel.RejectAs(CodeReferenceStale, kernel.CategoryConflict, path, reason)
	}
	return kernel.RejectAs(CodeSupplierDeliveryUnverified, kernel.CategoryConflict, path,
		"The supplier delivery is "+status+": a claim counts only once a human verified it")
}

// DeliveryLine is what PUR says of one delivery line now, before the caller's transaction: the
// version's status and the delivery's verified version and row version, the commitment line it is
// delivered against as InspectionSubject v1 shows it (Line: commitment, order binding, supplier, SKU
// specification, unit and the quantity the line buys), the claimed and the verified quantity (the
// latter for a version that is or was verified), where and when the goods were handed over, who
// verified it, and Code as the guard decides it.
type DeliveryLine struct {
	Ref              DeliveryLineRef
	Status           string
	VerifiedVersion  int64
	RowVersion       int64
	Line             CommitmentLine
	ClaimedQuantity  string
	VerifiedQuantity string
	HandoverPlace    string
	HandedOverAt     time.Time
	VerifiedBy       kernel.ActorRef
	VerifiedAt       time.Time
	Code             string
}

// Check returns nil for a line whose version is the delivery's verified one, and the guard's
// refusal at path otherwise.
func (l DeliveryLine) Check(path string) error {
	return deliveryCheck(true, l.Status, l.Code, l.VerifiedVersion, path)
}

// SettlementSource is PurchaseSettlementSource v1 (Context Map R31 PUR→STL: 采购承诺与收货或直发交付证据（采购
// 结算来源）; risk 无交付证据形成应付): a delivery line as a source of purchase settlement. It is one when
// its Code is "": the verified quantity of the delivery's verified version; (DeliveryID, LineID)
// names the goods across versions, so a line is settled once and a later verified version shows as
// REFERENCE_STALE on the version settled. Besides the delivery line it carries what the commitment
// line buys in money, from the sealed purchase draft — the currency with its minor units and the
// line's net, tax and gross amounts in minor units for the whole PurchaseQuantity (Line) —, what is
// verified now for the commitment line over all its deliveries, and the delivery's evidence: the
// claim's sources and files and what the verifier added. PUR states no amount for a part of a line:
// how a partial delivery is valued is STL's rule.
type SettlementSource struct {
	DeliveryLine
	Currency             string
	MinorUnits           int
	LineNetMinor         string
	LineTaxMinor         string
	LineGrossMinor       string
	LineVerifiedQuantity string
	Evidence             []Evidence
	Files                []DeliveryFile
	VerificationEvidence []Evidence
}

// PurchaseDelivery is one verified delivery of a purchase line: the delivery line at its verified
// version, the verified quantity, where and when the goods were handed over and when it was
// verified.
type PurchaseDelivery struct {
	Ref              DeliveryLineRef
	VerifiedQuantity string
	HandoverPlace    string
	HandedOverAt     time.Time
	VerifiedAt       time.Time
}

// PurchaseLine is PurchaseLineRef v1 (Context Map R37 PUR→CNX: 采购与收货明细（购进发票匹配）; risk 红冲作废后
// 匹配未复核): a commitment line with what it buys in money and what was delivered of it. Line is the
// line as InspectionSubject v1 shows it (Code "" for a line of an issued or confirmed commitment);
// the currency and the net, tax and gross amounts in minor units are the sealed draft's for the
// whole PurchaseQuantity; Deliveries are the line's verified deliveries by delivery ID and
// VerifiedQuantity their sum. Claims are not shown: they never count.
type PurchaseLine struct {
	Line             CommitmentLine
	Currency         string
	MinorUnits       int
	NetMinor         string
	TaxMinor         string
	GrossMinor       string
	VerifiedQuantity string
	Deliveries       []PurchaseDelivery
}

// DeliveryLookups answer the lookups of the contexts downstream of PUR's supplier deliveries: FUL
// before its transaction (R59) and QLT for delivery line subjects (R24) with ResolveDeliveryLines,
// STL with ResolveSettlementSources (R31) and CNX with ResolvePurchaseLines (R37). Like the other
// lookups they need no permission beyond an active grant; commitments and deliveries outside the
// caller's data scope are absent, and so are refs of deliveries, versions or lines that do not
// exist. Ask before opening your own transaction.
type DeliveryLookups interface {
	// ResolveDeliveryLines answers at most MaxDeliveryLineRefs refs.
	ResolveDeliveryLines(ctx context.Context, refs []DeliveryLineRef) (map[DeliveryLineRef]DeliveryLine, error)
	// ResolveSettlementSources answers at most MaxDeliveryLineRefs refs.
	ResolveSettlementSources(ctx context.Context, refs []DeliveryLineRef) (map[DeliveryLineRef]SettlementSource, error)
	// ResolvePurchaseLines answers at most MaxCommitmentLineRefs refs.
	ResolvePurchaseLines(ctx context.Context, refs []CommitmentLineRef) (map[CommitmentLineRef]PurchaseLine, error)
}

// ValidateDeliveryLineRefs checks a lookup's refs.
func ValidateDeliveryLineRefs(refs []DeliveryLineRef) error {
	if len(refs) > MaxDeliveryLineRefs {
		return schema("/refs")
	}
	for i, r := range refs {
		p := index("/refs", i)
		switch {
		case !kernel.ValidIdentifier(r.DeliveryID):
			return schema(p + "/delivery_id")
		case !kernel.ValidIdentifier(r.LineID):
			return schema(p + "/line_id")
		case !version(r.Version):
			return schema(p + "/version")
		}
	}
	return nil
}
