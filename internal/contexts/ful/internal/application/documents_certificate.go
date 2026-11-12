package application

import (
	"context"
	"errors"

	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	docs "go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipmentdocumentset"
)

// Certificates is FUL's port to CNX's CertificateResultRef v1 (Context Map R56 CNX→FUL; CNX-INV-007,
// MD-40; S5 C5): whether a certificate result CNX registered applies to a shipment. It is a lookup FUL
// runs before its own transaction: a certificate's revisions never change, so what CNX answers of a
// revision stays true, and a correction committed later shows as REFERENCE_STALE at the next prepare.
// *cnx.Module implements it.
type Certificates interface {
	ResolveCertificates(ctx context.Context, uses []cnxapi.CertificateUse) ([]cnxapi.CertificateResult, error)
}

// WithCertificates returns u with the port to CNX's certificate results.
func (u DocumentUpstream) WithCertificates(c Certificates) DocumentUpstream {
	u.Certificates = c
	return u
}

// certificate asks CNX, before FUL's transaction, whether the certificate result a certificate of
// origin record names applies to the shipment now (kind certificate_of_origin; CNX reads the shipment's
// lines and destination itself through its port to FUL, R36). The domain decides on the answer
// (shipmentdocumentset.certificateRef).
func (h *prepareDocumentCommand) certificate(ctx context.Context) error {
	c := h.req.Certificate
	if c == nil || c.CertificateID == "" {
		return nil
	}
	results, err := h.s.Upstream.Documents.Certificates.ResolveCertificates(ctx, []cnxapi.CertificateUse{{
		Ref:    cnxapi.CertificateRef{CertificateID: c.CertificateID, Version: c.CertificateVersion},
		Kind:   cnxapi.CertificateOfOrigin,
		Target: cnxapi.ExportTarget{Kind: cnxapi.TargetShipment, ID: h.cmd.ShipmentID},
		At:     h.s.Clock.Now(),
	}})
	if err != nil {
		return err
	}
	if len(results) != 1 {
		return errors.New("ful: CNX answered no certificate result")
	}
	r := results[0]
	h.up.Certificate = &docs.CertificateResult{Code: r.Code, Reason: r.Reason, Number: r.Number, Binding: r.Evidence()}
	return nil
}
