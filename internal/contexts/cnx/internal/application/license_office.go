package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	dcl "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/declaration"
	el "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/exportlicense"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5R-D04 between export licences and customs declarations (CNX-INV-002; 一批一证、一证一关; the
// customs entry guide: "一份报关单只允许填报一个许可证号"). A declaration is of one export lot, and the
// licence it uses is the one whose live allocation covers the lot: the lot's own, or its shipment's
// (a shipment never holds its own allocation beside its lots', so there is one). That licence's
// customs office, when it states one, is the office the declaration must name; a single_use
// licence allocated to a whole shipment is used by one declaration only. A declaration checks this
// when it is prepared and when it is confirmed (the declared data handed over), holding the licence
// FOR SHARE after its own case FOR UPDATE; an allocation checks the live declarations of its target
// against the licence it allocates, under the licence's FOR UPDATE, reading the cases without a lock
// (no CNX command locks a case after a licence, so the two orders never wait on each other).

// declaredOffices refuses to allocate licence l to target while a declaration of the target that
// is confirmed and not ended names another customs office than l's (INVALID_KNOWN_VALUE at path).
// A licence registered before S5R-D04 states none and is not checked.
func declaredOffices(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, l *el.License, target api.ExportTarget, path string) error {
	if l.CustomsOffice == "" {
		return nil
	}
	var cases []*dcl.Case
	if target.Kind == api.TargetExportLot {
		c, err := postgres.DeclarationOfLot(ctx, exec, sc, target.ID, postgres.NoLock)
		if err != nil {
			return err
		}
		if c != nil {
			cases = append(cases, c)
		}
	} else {
		var err error
		if cases, err = postgres.DeclarationsOfShipment(ctx, exec, sc, target.ID); err != nil {
			return err
		}
	}
	for _, c := range cases {
		if c.Status == dcl.Draft || dcl.Terminal(c.Status) {
			continue
		}
		office, err := declaredOffice(ctx, exec, sc, c)
		if err != nil {
			return err
		}
		if office != l.CustomsOffice {
			return kernel.Reject(kernel.CodeInvalidKnownValue, path, "Declaration "+c.ID+" is declared at customs office "+quoted(office)+
				", the licence is for "+l.CustomsOffice+" (一证一关, S5R-D04)")
		}
	}
	return nil
}

// declaredOffice is the customs office the current version of case c names ("" when none).
func declaredOffice(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, c *dcl.Case) (string, error) {
	vs, err := postgres.Versions(ctx, exec, sc, c.ID, c.Version)
	if err != nil || len(vs) != 1 {
		return "", err
	}
	return vs[0].CustomsOffice, nil
}

func quoted(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// declarationLicense checks the declaration of export lot lotID of shipment shipmentID (case
// declarationID, "" for a new one) against the licence that covers the lot, held FOR SHARE: the
// customs office it names (office, at officePath) must be the licence's (INVALID_KNOWN_VALUE); and
// a single_use licence allocated to the whole shipment may be used by no other live declaration of
// the shipment's lots (INVALID_STATE at lotPath: allocate a licence per export lot). Without a
// covering licence nothing is checked: whether the goods need one is TradeComplianceGuard v1's
// statement, which FUL's release asks.
func declarationLicense(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, declarationID, lotID, shipmentID, office, officePath,
	lotPath string) error {
	l, kind, err := postgres.CoveringLicense(ctx, exec, sc, lotID, shipmentID, postgres.ForShare)
	if err != nil || l == nil {
		return err
	}
	if l.CustomsOffice != "" && office != l.CustomsOffice {
		return kernel.Reject(kernel.CodeInvalidKnownValue, officePath, "Licence "+l.Number+" is for customs office "+l.CustomsOffice+
			", the declaration names "+quoted(office)+" (一证一关, S5R-D04)")
	}
	if l.UsageMode != el.SingleUse || kind != api.TargetShipment {
		return nil
	}
	cases, err := postgres.DeclarationsOfShipment(ctx, exec, sc, shipmentID)
	if err != nil {
		return err
	}
	for _, c := range cases {
		if c.ID != declarationID && c.ExportLotID != lotID && !dcl.Terminal(c.Status) {
			return kernel.Reject(kernel.CodeInvalidState, lotPath, "Single_use licence "+l.Number+" covers the whole shipment and declaration "+
				c.ID+" of another export lot uses it: allocate a licence per export lot (一批一证, S5R-D04)")
		}
	}
	return nil
}
