package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The S/C and PI of an order and their signed copies (migrations/trd/000007), loaded with the order
// without their bytes and written in the transaction of the quote root and the order: a document
// once, with its binding, bytes and commit evidence; a signed copy once per kind. Nothing changes or
// is deleted. A document's bytes are read only by LoadOrderDocumentContent.

func loadDocuments(ctx context.Context, exec bentopg.Executor, t, c string, o *order.Order) error {
	rows, err := exec.Query(ctx, `SELECT order_revision, document_kind, order_hash, number, sequence_kind, sequence_id, sequence_revision,
		business_ref, template_kind, template_id, template_revision, template_content_hash, renderer_version, generated_at, file_name, mime_type,
		content_sha256, size_bytes, idempotency_key, authority_epoch, control_epoch, generated_by_kind, generated_by_id, committed_at, entries
		FROM brant_trd.sales_order_document WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 ORDER BY order_revision, document_kind`,
		t, c, o.ID)
	if err != nil {
		return err
	}
	o.Documents, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (*order.Document, error) {
		d := &order.Document{}
		b := &d.Binding
		var kind, entries string
		var generated time.Time
		if err := row.Scan(&b.OrderRevision, &b.DocumentKind, &b.OrderHash, &b.Number, &b.Sequence.Kind, &b.Sequence.ID, &b.Sequence.Revision,
			&b.BusinessRef, &b.Template.Kind, &b.Template.ID, &b.Template.Revision, &b.TemplateHash, &b.RendererVersion, &generated, &b.FileName,
			&b.MimeType, &b.ContentSHA256, &b.SizeBytes, &d.IdempotencyKey, &d.Epoch, &d.ControlEpoch, &kind, &d.By.ID, &d.At, &entries); err != nil {
			return nil, err
		}
		b.OrderID, b.GeneratedAt = o.ID, kernel.FormatTimestamp(kernel.Instant(generated))
		d.By.Kind, d.At = kernel.ActorKind(kind), kernel.Instant(d.At)
		return d, json.Unmarshal([]byte(entries), &d.Entries)
	})
	if err != nil {
		return err
	}
	rows, err = exec.Query(ctx, `SELECT order_revision, document_kind, signature_kind, file_id, file_sha256, file_mime_type, idempotency_key,
		recorded_by_kind, recorded_by_id, record_epoch, recorded_at, evidence
		FROM brant_trd.sales_order_document_signature WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3
		ORDER BY recorded_at, signature_kind`, t, c, o.ID)
	if err != nil {
		return err
	}
	type signed struct {
		revision int64
		kind     string
		sig      *order.Signature
	}
	sigs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (signed, error) {
		s := signed{sig: &order.Signature{}}
		x := s.sig
		var by, id, proofs string
		var epoch int64
		var at time.Time
		if err := row.Scan(&s.revision, &s.kind, &x.Kind, &x.FileID, &x.FileSHA256, &x.MimeType, &x.IdempotencyKey, &by, &id, &epoch, &at,
			&proofs); err != nil {
			return s, err
		}
		var err error
		x.Recorded, err = act(by, id, epoch, at, nil, proofs)
		return s, err
	})
	if err != nil {
		return err
	}
	for _, s := range sigs {
		d := o.Document(s.revision, s.kind)
		if d == nil {
			return errors.New("trd: a signed copy of no document")
		}
		d.Signatures = append(d.Signatures, s.sig)
	}
	return nil
}

// LoadOrderDocumentContent returns the stored bytes of order id's kind document of revision rev, and
// false when there is none.
func LoadOrderDocumentContent(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, rev int64, kind string) ([]byte, bool,
	error) {
	var content []byte
	err := exec.QueryRow(ctx, `SELECT content FROM brant_trd.sales_order_document WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3
		AND order_revision = $4 AND document_kind = $5`, string(scope.Tenant), string(scope.Company), id, rev, kind).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return content, err == nil, err
}

// saveDocuments writes an order command's fresh documents, then its fresh signed copies.
func saveDocuments(ctx context.Context, exec bentopg.Executor, t, c string, o *order.Order) error {
	for _, d := range o.Documents {
		if !d.Fresh {
			continue
		}
		b := d.Binding
		entries, err := d.EntriesJSON()
		if err != nil {
			return err
		}
		generated, err := kernel.ParseTimestamp(b.GeneratedAt)
		if err != nil {
			return err
		}
		if _, err = exec.Exec(ctx, `INSERT INTO brant_trd.sales_order_document (tenant_id, company_id, order_id, order_revision, document_kind,
			order_hash, number, sequence_kind, sequence_id, sequence_revision, business_ref, template_kind, template_id, template_revision,
			template_content_hash, renderer_version, generated_at, file_name, mime_type, content_sha256, size_bytes, content, command,
			idempotency_key, authority_epoch, control_epoch, generated_by_kind, generated_by_id, committed_at, entries, entries_sha256)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22,
				'order.document.generate', $23, $24, $25, $26, $27, $28, $29, $30)`,
			t, c, o.ID, b.OrderRevision, b.DocumentKind, b.OrderHash, b.Number, b.Sequence.Kind, b.Sequence.ID, b.Sequence.Revision, b.BusinessRef,
			b.Template.Kind, b.Template.ID, b.Template.Revision, b.TemplateHash, b.RendererVersion, generated, b.FileName, b.MimeType,
			b.ContentSHA256, b.SizeBytes, d.Content, d.IdempotencyKey, d.Epoch, d.ControlEpoch, string(d.By.Kind), d.By.ID, d.At, string(entries),
			quote.Hash(entries)); err != nil {
			return err
		}
		d.Fresh = false
	}
	for _, d := range o.Documents {
		for _, s := range d.Signatures {
			if !s.Fresh {
				continue
			}
			proofs, err := proofsOf(s.Recorded.Evidence)
			if err != nil {
				return err
			}
			b := d.Binding
			if _, err = exec.Exec(ctx, `INSERT INTO brant_trd.sales_order_document_signature (tenant_id, company_id, order_id, order_revision,
				document_kind, signature_kind, file_id, file_sha256, file_mime_type, document_sha256, order_hash, idempotency_key, recorded_by_kind,
				recorded_by_id, record_epoch, recorded_at, evidence) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
				t, c, o.ID, b.OrderRevision, b.DocumentKind, s.Kind, s.FileID, s.FileSHA256, s.MimeType, b.ContentSHA256, b.OrderHash,
				s.IdempotencyKey, string(s.Recorded.By.Kind), s.Recorded.By.ID, s.Recorded.Epoch, s.Recorded.At, proofs); err != nil {
				return err
			}
			s.Fresh = false
		}
	}
	return nil
}
