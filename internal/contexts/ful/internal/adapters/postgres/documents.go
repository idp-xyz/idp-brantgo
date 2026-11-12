package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	docs "go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipmentdocumentset"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// LoadBookings returns the booking versions of shipment id, oldest first.
func LoadBookings(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*shipment.Booking, error) {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT version, service_party_id, service_role_id, service_role_revision, service_kind, booking_number, vessel,
		voyage, to_char(etd, 'YYYY-MM-DD'), cut_off, container_type, container_count, port_of_loading, port_of_discharge, evidence_ids,
		coalesce(reason, ''), recorded_by_kind, recorded_by_id, record_epoch, recorded_at
		FROM brant_ful.shipment_booking WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = $3 ORDER BY version`, t, c, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*shipment.Booking, error) {
		var b shipment.Booking
		var by string
		err := row.Scan(&b.Version, &b.ServiceRole.PartyID, &b.ServiceRole.ServiceRoleID, &b.ServiceRole.Revision, &b.ServiceKind, &b.Number,
			&b.Vessel, &b.Voyage, &b.ETD, &b.CutOff, &b.ContainerType, &b.ContainerCount, &b.PortOfLoading, &b.PortOfDischarge, &b.Evidence,
			&b.Reason, &by, &b.Recorded.By.ID, &b.Recorded.Epoch, &b.Recorded.At)
		b.Recorded.By.Kind, b.Recorded.At, b.CutOff, b.Evidence = kernel.ActorKind(by), kernel.Instant(b.Recorded.At), kernel.Instant(b.CutOff),
			strs(b.Evidence)
		return &b, err
	})
}

// CurrentBooking returns the latest booking version of shipment id, or nil.
func CurrentBooking(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*shipment.Booking, error) {
	all, err := LoadBookings(ctx, exec, scope, id)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return all[len(all)-1], nil
}

// InsertBooking stores booking version b of shipment id.
func InsertBooking(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, b *shipment.Booking) error {
	t, c := scope2(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_ful.shipment_booking (tenant_id, company_id, shipment_id, version, service_party_id,
		service_role_id, service_role_revision, service_kind, booking_number, vessel, voyage, etd, cut_off, container_type, container_count,
		port_of_loading, port_of_discharge, evidence_ids, reason, recorded_by_kind, recorded_by_id, record_epoch, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::date, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)`,
		t, c, id, b.Version, b.ServiceRole.PartyID, b.ServiceRole.ServiceRoleID, b.ServiceRole.Revision, b.ServiceKind, b.Number, b.Vessel,
		b.Voyage, b.ETD, b.CutOff, b.ContainerType, b.ContainerCount, b.PortOfLoading, b.PortOfDischarge, strs(b.Evidence), nullable(b.Reason),
		string(b.Recorded.By.Kind), b.Recorded.By.ID, b.Recorded.Epoch, b.Recorded.At)
	return err
}

// SetOfShipment returns the ID of shipment id's document set, or "".
func SetOfShipment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (string, error) {
	t, c := scope2(scope)
	var set string
	err := exec.QueryRow(ctx, `SELECT document_set_id FROM brant_ful.shipment_document_set WHERE tenant_id = $1 AND company_id = $2
		AND shipment_id = $3`, t, c, id).Scan(&set)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return set, err
}

// ShipmentOfSet returns the shipment of document set id (it never changes), or "".
func ShipmentOfSet(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (string, error) {
	t, c := scope2(scope)
	var shipment string
	err := exec.QueryRow(ctx, `SELECT shipment_id FROM brant_ful.shipment_document_set WHERE tenant_id = $1 AND company_id = $2
		AND document_set_id = $3`, t, c, id).Scan(&shipment)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return shipment, err
}

// LoadSet returns document set id with every version (renditions without their bytes), signed copy
// and issue, or nil; lock locks the set row.
func LoadSet(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*docs.Set, error) {
	t, c := scope2(scope)
	s := &docs.Set{}
	var by string
	err := exec.QueryRow(ctx, `SELECT document_set_id, shipment_id, order_id, order_revision, order_hash, row_version, document_count,
		issue_count, owner_actor_id, coalesce(owner_team_id, ''), created_by_kind, created_by_id, create_epoch, created_at
		FROM brant_ful.shipment_document_set WHERE tenant_id = $1 AND company_id = $2 AND document_set_id = $3`+string(lock), t, c, id).Scan(
		&s.ID, &s.ShipmentID, &s.Order.OrderID, &s.Order.OrderRevision, &s.Order.OrderHash, &s.RowVersion, &s.DocumentCount, &s.IssueCount,
		&s.OwnerActor, &s.OwnerTeam, &by, &s.Created.By.ID, &s.Created.Epoch, &s.Created.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.Created.By.Kind, s.Created.At = kernel.ActorKind(by), kernel.Instant(s.Created.At)
	if s.Documents, err = loadDocuments(ctx, exec, scope, id); err != nil {
		return nil, err
	}
	if err = loadRenditions(ctx, exec, scope, s); err != nil {
		return nil, err
	}
	if err = loadSignatures(ctx, exec, scope, s); err != nil {
		return nil, err
	}
	s.Issues, err = loadIssues(ctx, exec, scope, id)
	return s, err
}

func ref(kind, id *string, revision *int64) *docs.Ref {
	if kind == nil || id == nil || revision == nil {
		return nil
	}
	return &docs.Ref{Kind: *kind, ID: *id, Revision: *revision}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func loadDocuments(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*docs.Document, error) {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT document_kind, version, status, set_version, number, sequence_kind, sequence_id, sequence_revision,
		sequence_hash, seller_kind, seller_id, seller_revision, seller_hash, renderer_version, content, content_sha256, file_id, file_sha256,
		file_mime_type, sources, evidence_ids, idempotency_key, prepared_by_kind, prepared_by_id, prepare_epoch, prepared_at, commit_entries,
		commit_entries_sha256, coalesce(issue_seq, 0), issued_at, superseded_at
		FROM brant_ful.shipment_document WHERE tenant_id = $1 AND company_id = $2 AND document_set_id = $3
		ORDER BY array_position(ARRAY['commercial_invoice', 'packing_list', 'bill_of_lading', 'certificate_of_origin'], document_kind), version`,
		t, c, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*docs.Document, error) {
		d := &docs.Document{}
		var seqKind, seqID, seqHash, sellerKind, sellerID, sellerHash, renderer, fileID, fileSHA, fileMime *string
		var seqRev, sellerRev *int64
		var sources, by string
		var issued, superseded *time.Time
		if err := row.Scan(&d.Kind, &d.Version, &d.Status, &d.SetVersion, &d.Number, &seqKind, &seqID, &seqRev, &seqHash, &sellerKind, &sellerID,
			&sellerRev, &sellerHash, &renderer, &d.ContentJSON, &d.ContentSHA256, &fileID, &fileSHA, &fileMime, &sources, &d.Evidence,
			&d.IdempotencyKey, &by, &d.Prepared.By.ID, &d.Prepared.Epoch, &d.Prepared.At, &d.EntriesJSON, &d.EntriesSHA256, &d.IssueSeq, &issued,
			&superseded); err != nil {
			return nil, err
		}
		d.Prepared.By.Kind, d.Prepared.At, d.Evidence = kernel.ActorKind(by), kernel.Instant(d.Prepared.At), strs(d.Evidence)
		d.Sequence, d.SequenceHash = ref(seqKind, seqID, seqRev), deref(seqHash)
		d.Seller, d.SellerHash, d.RendererVersion = ref(sellerKind, sellerID, sellerRev), deref(sellerHash), deref(renderer)
		if issued != nil {
			d.IssuedAt = kernel.Instant(*issued)
		}
		if superseded != nil {
			d.SupersededAt = kernel.Instant(*superseded)
		}
		if fileID != nil {
			d.File = &docs.File{ID: *fileID, SHA256: deref(fileSHA), MimeType: deref(fileMime)}
		}
		if err := json.Unmarshal([]byte(sources), &d.Sources); err != nil {
			return nil, fmt.Errorf("ful: stored sources of %s v%d: %w", d.Kind, d.Version, err)
		}
		if err := json.Unmarshal([]byte(d.EntriesJSON), &d.Entries); err != nil {
			return nil, fmt.Errorf("ful: stored commit evidence of %s v%d: %w", d.Kind, d.Version, err)
		}
		if docs.Rendered(d.Kind) {
			d.Content = &docs.Content{}
			if err := json.Unmarshal([]byte(d.ContentJSON), d.Content); err != nil {
				return nil, fmt.Errorf("ful: stored content of %s v%d: %w", d.Kind, d.Version, err)
			}
		} else {
			d.Record = &docs.Record{}
			if err := json.Unmarshal([]byte(d.ContentJSON), d.Record); err != nil {
				return nil, fmt.Errorf("ful: stored record of %s v%d: %w", d.Kind, d.Version, err)
			}
		}
		return d, nil
	})
}

func loadRenditions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *docs.Set) error {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT document_kind, version, language, template_kind, template_id, template_revision, template_hash,
		file_name, mime_type, content_sha256, size_bytes FROM brant_ful.shipment_document_rendition
		WHERE tenant_id = $1 AND company_id = $2 AND document_set_id = $3 ORDER BY document_kind, version, language`, t, c, s.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var version int64
		var r docs.Rendition
		if err = rows.Scan(&kind, &version, &r.Language, &r.Template.Kind, &r.Template.ID, &r.Template.Revision, &r.TemplateHash, &r.FileName,
			&r.MimeType, &r.SHA256, &r.Size); err != nil {
			return err
		}
		d := s.Document(kind, version)
		if d == nil {
			return fmt.Errorf("ful: rendition of a missing version %s v%d", kind, version)
		}
		d.Renditions = append(d.Renditions, r)
	}
	return rows.Err()
}

func loadSignatures(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *docs.Set) error {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT document_kind, version, language, file_id, file_sha256, file_mime_type, document_sha256, evidence_ids,
		idempotency_key, recorded_by_kind, recorded_by_id, record_epoch, recorded_at FROM brant_ful.shipment_document_signature
		WHERE tenant_id = $1 AND company_id = $2 AND document_set_id = $3 ORDER BY document_kind, version, language`, t, c, s.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, by string
		var version int64
		var x docs.Signature
		if err = rows.Scan(&kind, &version, &x.Language, &x.FileID, &x.FileSHA256, &x.MimeType, &x.DocumentSHA256, &x.Evidence, &x.IdempotencyKey,
			&by, &x.Recorded.By.ID, &x.Recorded.Epoch, &x.Recorded.At); err != nil {
			return err
		}
		x.Recorded.By.Kind, x.Recorded.At, x.Evidence = kernel.ActorKind(by), kernel.Instant(x.Recorded.At), strs(x.Evidence)
		d := s.Document(kind, version)
		if d == nil {
			return fmt.Errorf("ful: signed copy of a missing version %s v%d", kind, version)
		}
		d.Signatures = append(d.Signatures, x)
	}
	return rows.Err()
}

func loadIssues(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*docs.Issue, error) {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT seq, set_version, documents, acknowledgements, differences, reconciliation_sha256, idempotency_key,
		issued_by_kind, issued_by_id, issue_epoch, issued_at, commit_entries, commit_entries_sha256
		FROM brant_ful.shipment_document_issue WHERE tenant_id = $1 AND company_id = $2 AND document_set_id = $3 ORDER BY seq`, t, c, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*docs.Issue, error) {
		i := &docs.Issue{}
		var documents, acks, by string
		if err := row.Scan(&i.Seq, &i.SetVersion, &documents, &acks, &i.DifferencesJSON, &i.ReconciliationSHA256, &i.IdempotencyKey, &by,
			&i.Issued.By.ID, &i.Issued.Epoch, &i.Issued.At, &i.EntriesJSON, &i.EntriesSHA256); err != nil {
			return nil, err
		}
		i.Issued.By.Kind, i.Issued.At = kernel.ActorKind(by), kernel.Instant(i.Issued.At)
		for _, x := range []struct {
			doc string
			to  any
		}{{documents, &i.Documents}, {acks, &i.Acknowledged}, {i.EntriesJSON, &i.Entries}} {
			if err := json.Unmarshal([]byte(x.doc), x.to); err != nil {
				return nil, fmt.Errorf("ful: stored issue %d of %s: %w", i.Seq, id, err)
			}
		}
		return i, nil
	})
}

// InsertSet stores new set s, at row version 1 with its first version (InsertDocument follows).
func InsertSet(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *docs.Set) error {
	t, c := scope2(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_ful.shipment_document_set (tenant_id, company_id, document_set_id, shipment_id, order_id,
		order_revision, order_hash, row_version, document_count, issue_count, owner_actor_id, owner_team_id, created_by_kind, created_by_id,
		create_epoch, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		t, c, s.ID, s.ShipmentID, s.Order.OrderID, s.Order.OrderRevision, s.Order.OrderHash, s.RowVersion, s.DocumentCount, s.IssueCount,
		s.OwnerActor, nullable(s.OwnerTeam), string(s.Created.By.Kind), s.Created.By.ID, s.Created.Epoch, s.Created.At)
	return err
}

// MoveSet writes set s's version and counts if the stored row is still at version from.
func MoveSet(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *docs.Set, from int64) error {
	t, c := scope2(scope)
	tag, err := exec.Exec(ctx, `UPDATE brant_ful.shipment_document_set SET row_version = $4, document_count = $5, issue_count = $6
		WHERE tenant_id = $1 AND company_id = $2 AND document_set_id = $3 AND row_version = $7`, t, c, s.ID, s.RowVersion, s.DocumentCount,
		s.IssueCount, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// MoveDocument writes version d's state (issued or superseded).
func MoveDocument(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, setID string, d *docs.Document) error {
	t, c := scope2(scope)
	var seq *int64
	var issued, superseded *time.Time
	if d.IssueSeq > 0 {
		seq, issued = &d.IssueSeq, &d.IssuedAt
	}
	if !d.SupersededAt.IsZero() {
		superseded = &d.SupersededAt
	}
	_, err := exec.Exec(ctx, `UPDATE brant_ful.shipment_document SET status = $6, issue_seq = $7, issued_at = $8, superseded_at = $9
		WHERE tenant_id = $1 AND company_id = $2 AND document_set_id = $3 AND document_kind = $4 AND version = $5`,
		t, c, setID, d.Kind, d.Version, d.Status, seq, issued, superseded)
	return err
}

// InsertDocument stores new draft d of set setID with its renditions and their bytes.
func InsertDocument(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, setID string, d *docs.Document) error {
	t, c := scope2(scope)
	sources, _ := docs.Canonical(nonNilEntries(d.Sources))
	var seqKind, seqID, seqHash, sellerKind, sellerID, sellerHash, renderer, fileID, fileSHA, fileMime *string
	var seqRev, sellerRev *int64
	if r := d.Sequence; r != nil {
		seqKind, seqID, seqRev, seqHash = &r.Kind, &r.ID, &r.Revision, &d.SequenceHash
	}
	if r := d.Seller; r != nil {
		sellerKind, sellerID, sellerRev, sellerHash = &r.Kind, &r.ID, &r.Revision, &d.SellerHash
	}
	if d.RendererVersion != "" {
		renderer = &d.RendererVersion
	}
	if f := d.File; f != nil {
		fileID, fileSHA, fileMime = &f.ID, &f.SHA256, &f.MimeType
	}
	if _, err := exec.Exec(ctx, `INSERT INTO brant_ful.shipment_document (tenant_id, company_id, document_set_id, document_kind, version,
		status, set_version, number, sequence_kind, sequence_id, sequence_revision, sequence_hash, seller_kind, seller_id, seller_revision,
		seller_hash, renderer_version, content, content_sha256, file_id, file_sha256, file_mime_type, sources, evidence_ids, idempotency_key,
		prepared_by_kind, prepared_by_id, prepare_epoch, prepared_at, commit_entries, commit_entries_sha256)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28,
		$29, $30, $31)`, t, c, setID, d.Kind, d.Version, d.Status, d.SetVersion, d.Number, seqKind, seqID, seqRev, seqHash, sellerKind, sellerID,
		sellerRev, sellerHash, renderer, d.ContentJSON, d.ContentSHA256, fileID, fileSHA, fileMime, sources, strs(d.Evidence), d.IdempotencyKey,
		string(d.Prepared.By.Kind), d.Prepared.By.ID, d.Prepared.Epoch, d.Prepared.At, d.EntriesJSON, d.EntriesSHA256); err != nil {
		return err
	}
	for _, r := range d.Renditions {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_ful.shipment_document_rendition (tenant_id, company_id, document_set_id, document_kind,
			version, language, template_kind, template_id, template_revision, template_hash, file_name, mime_type, content, content_sha256,
			size_bytes) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`, t, c, setID, d.Kind, d.Version, r.Language,
			r.Template.Kind, r.Template.ID, r.Template.Revision, r.TemplateHash, r.FileName, r.MimeType, r.Content, r.SHA256, r.Size); err != nil {
			return err
		}
	}
	return nil
}

func nonNilEntries(e []docs.Entry) []docs.Entry {
	if e == nil {
		return []docs.Entry{}
	}
	return e
}

// InsertIssue stores issue i of set setID.
func InsertIssue(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, setID string, i *docs.Issue) error {
	t, c := scope2(scope)
	documents, _ := docs.Canonical(i.Documents)
	acks := i.Acknowledged
	if acks == nil {
		acks = []docs.Acknowledged{}
	}
	ackJSON, _ := docs.Canonical(acks)
	_, err := exec.Exec(ctx, `INSERT INTO brant_ful.shipment_document_issue (tenant_id, company_id, document_set_id, seq, set_version, documents,
		acknowledgements, differences, reconciliation_sha256, idempotency_key, issued_by_kind, issued_by_id, issue_epoch, issued_at, commit_entries,
		commit_entries_sha256) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`, t, c, setID, i.Seq, i.SetVersion,
		documents, ackJSON, i.DifferencesJSON, i.ReconciliationSHA256, i.IdempotencyKey, string(i.Issued.By.Kind), i.Issued.By.ID, i.Issued.Epoch,
		i.Issued.At, i.EntriesJSON, i.EntriesSHA256)
	return err
}

// InsertSignature stores signed copy x of version version of kind in set setID.
func InsertSignature(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, setID, kind string, version int64, x *docs.Signature) error {
	t, c := scope2(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_ful.shipment_document_signature (tenant_id, company_id, document_set_id, document_kind, version,
		language, file_id, file_sha256, file_mime_type, document_sha256, evidence_ids, idempotency_key, recorded_by_kind, recorded_by_id,
		record_epoch, recorded_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`, t, c, setID, kind, version,
		x.Language, x.FileID, x.FileSHA256, x.MimeType, x.DocumentSHA256, strs(x.Evidence), x.IdempotencyKey, string(x.Recorded.By.Kind),
		x.Recorded.By.ID, x.Recorded.Epoch, x.Recorded.At)
	return err
}

// LoadRenditionContent returns the bytes of the language rendition of version version of kind in set
// setID, or nil.
func LoadRenditionContent(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, setID, kind string, version int64,
	language string) ([]byte, error) {
	t, c := scope2(scope)
	var content []byte
	err := exec.QueryRow(ctx, `SELECT content FROM brant_ful.shipment_document_rendition WHERE tenant_id = $1 AND company_id = $2
		AND document_set_id = $3 AND document_kind = $4 AND version = $5 AND language = $6`, t, c, setID, kind, version, language).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return content, err
}
