package application

import (
	"context"
	"testing"

	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
)

type noCertificates struct{}

func (noCertificates) ResolveCertificates(context.Context, []cnxapi.CertificateUse) ([]cnxapi.CertificateResult, error) {
	return nil, nil
}

// S5 C5: the document commands need CNX's CertificateResultRef v1 (R56) wired; WithCertificates adds it
// to the other document ports.
func TestDocumentUpstreamNeedsCNXCertificates(t *testing.T) {
	u := DocumentUpstream{Configs: nil}
	if u.Complete() {
		t.Fatal("empty ports are complete")
	}
	if w := u.WithCertificates(noCertificates{}); w.Certificates == nil || u.Certificates != nil {
		t.Fatal("WithCertificates")
	}
}
