// brant-admin is the operator's out-of-band tool for a tenant and company: it provisions the
// authority row and sets one actor's permissions and data scope (Python Core.provision). It is not
// a user command and never runs behind the HTTP API; the identity provider and an administration
// process own access in production.
//
//	brant-admin grant -tenant t -company c -actor alice -kind human -permissions rfq.save_draft,inq.read -data-scope company
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "brant-admin:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] != "grant" {
		return errors.New("usage: brant-admin grant -tenant ... -company ... -actor ... -kind human|ai|system -permissions a,b [-data-scope own|team|company] [-team ...]")
	}
	fs := flag.NewFlagSet("grant", flag.ContinueOnError)
	tenant := fs.String("tenant", "", "tenant ID")
	company := fs.String("company", "", "company ID")
	actor := fs.String("actor", "", "actor ID, the token's actor claim")
	kind := fs.String("kind", "", "actor kind: human, ai or system")
	permissions := fs.String("permissions", "", "comma-separated permissions; the actor gets exactly these")
	dataScope := fs.String("data-scope", "", "own, team or company; empty keeps the current one (own for a new actor)")
	team := fs.String("team", "", "team ID, required for -data-scope team")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	dsn := os.Getenv("IDP_BRANT_POSTGRES_DSN")
	if dsn == "" {
		return errors.New("IDP_BRANT_POSTGRES_DSN is required")
	}
	var perms []string
	for _, p := range strings.Split(*permissions, ",") {
		if p = strings.TrimSpace(p); p != "" {
			perms = append(perms, p)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	platform, err := bootstrap.Open(ctx, bootstrap.Config{PostgresDSN: dsn})
	if err != nil {
		return err
	}
	defer platform.Close()
	epoch, err := platform.ProvisionActor(ctx, *tenant, *company, *actor, *kind, perms, *dataScope, *team)
	if err != nil {
		return err
	}
	fmt.Printf("GRANTED %s/%s %s:%s %d permissions, authority epoch %d\n", *tenant, *company, *kind, *actor, len(perms), epoch)
	return nil
}
