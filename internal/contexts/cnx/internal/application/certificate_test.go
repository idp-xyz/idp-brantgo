package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	cert "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/certificate"
	db "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/documentbundle"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// humanOnlyCommands (D-17) name certificate.register (role template documentation): no AI agent or
// system actor registers or corrects a certificate result. The reads need cnx.read; the request hash
// covers the business arguments, not the key or the CAS value.
func TestCertificateRules(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	r, err := policy.Rule(api.CommandRegisterCertificate)
	if err != nil || r.Permission != api.CommandRegisterCertificate || !r.Admits(kernel.ActorHuman) || r.Admits(kernel.ActorAI) || r.Admits(kernel.ActorSystem) {
		t.Fatalf("%+v %v", r, err)
	}
	for _, command := range []string{api.CommandGetCertificate, api.CommandCertificates, api.CommandCertificateApplicability} {
		if r, err := policy.Rule(command); err != nil || r.Permission != api.PermissionRead || !r.Admits(kernel.ActorAI) {
			t.Errorf("%s: %+v %v", command, r, err)
		}
	}
	a := api.RegisterCertificate{IdempotencyKey: "k1", CertificateID: "crt-1", ExpectedVersion: 1, Number: "CO-1"}
	b := a
	b.IdempotencyKey, b.ExpectedVersion = "k2", 3
	n1, k1, x := request(a)
	n2, k2, y := request(b)
	hx, errX := action.RequestHash(x)
	hy, errY := action.RequestHash(y)
	if n1 != api.CommandRegisterCertificate || n1 != n2 || k1 == k2 || errX != nil || errY != nil || hx != hy {
		t.Fatalf("%v %v", x, y)
	}
}

// The certificate domain decides with codes the api declares, with the same categories, and its names
// are the contract's; the bundle's certificate kinds are the certificate kinds.
func TestCertificateDomainIsTheContract(t *testing.T) {
	for code, category := range cert.Categories {
		if got, ok := api.Category(code); !ok || got != category {
			t.Errorf("%s: api says %q (declared %v), domain says %q", code, got, ok, category)
		}
	}
	for domain, contract := range map[string]string{cert.KindOrigin: api.CertificateOfOrigin, cert.KindLicenseCopy: api.CertificateLicenseCopy,
		cert.KindInspection: api.CertificateInspection, cert.ObjectSkuSpec: api.ObjectSkuSpec, cert.ObjectExportLot: api.ObjectExportLot,
		cert.ObjectShipment + "s": api.ObjectShipment + "s", cert.ObjectExportLot + "t": api.TargetExportLot + "t", cert.ObjectShipment: api.TargetShipment,
		cert.Registered: api.CertificateRegistered, cert.Valid: api.LicenseValid, cert.Expired: api.LicenseExpired, cert.NotYetValid: api.LicenseNotYetValid,
		cert.ReasonApplicable: api.CertificateApplicable, cert.ReasonUnknown: api.CertificateUnknown, cert.ReasonTargetUnknown: api.CertificateTargetUnknown,
		cert.ReasonSuperseded: api.CertificateSuperseded, cert.ReasonKind: api.CertificateKindMismatch, cert.ReasonMarket: api.CertificateMarket,
		cert.ReasonMarketUnknown: api.CertificateMarketUnknown, cert.ReasonNotYetValid + "r": api.CertificateNotYetValid + "r",
		cert.ReasonExpired + "x": api.CertificateExpired + "x", cert.ReasonGoods: api.CertificateGoods,
		db.CodeCertificateNotApplicable: api.CodeCertificateNotApplicable, db.CertificateKind(db.KindCertificateOfOrigin): api.CertificateOfOrigin,
		db.CertificateKind(db.KindInspectionCertificate): api.CertificateInspection, db.CertificateKind(db.KindExportLicense): api.CertificateLicenseCopy,
		db.KindExportLicense: api.RequirementExportLicense} {
		if domain != contract {
			t.Errorf("domain %q, api %q", domain, contract)
		}
	}
	if len(cert.Kinds) != 3 {
		t.Errorf("kinds %v", cert.Kinds)
	}
}
