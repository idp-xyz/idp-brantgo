// brant-admin is the operator's out-of-band tool for a tenant and company: it provisions the
// authority row and sets one actor's permissions and data scope (Python Core.provision). It is not
// a user command and never runs behind the HTTP API; the identity provider and an administration
// process own access in production.
//
//	brant-admin grant -tenant t -company c -actor alice -kind human -permissions rfq.save_draft,inq.read -data-scope company
//	brant-admin role -tenant t -company c -role trade.clerk -name 外贸业务员 -templates sales_clerk,merchandiser
//	brant-admin assign -tenant t -company c -actor alice -kind human -roles trade.clerk -data-scope team -team east
//
// Roles and their templates are BC-D02: templates come from the master archive; a forbidden
// combination (separation of duties) is refused.
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
	if len(args) > 0 && (args[0] == "role" || args[0] == "assign") {
		return roles(args)
	}
	if len(args) == 0 || args[0] != "grant" {
		return errors.New("usage: brant-admin grant|role|assign -tenant ... -company ... (see the package comment)")
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

// roles runs `role` (define a tenant role from templates) and `assign` (give an actor roles).
func roles(args []string) error {
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	tenant := fs.String("tenant", "", "tenant ID")
	company := fs.String("company", "", "company ID")
	role := fs.String("role", "", "role: tenant role code to define")
	name := fs.String("name", "", "role: display name")
	templates := fs.String("templates", "", "role: comma-separated role templates to merge")
	actor := fs.String("actor", "", "assign: actor ID")
	kind := fs.String("kind", "", "assign: actor kind human, ai or system")
	assigned := fs.String("roles", "", "assign: comma-separated roles (templates or tenant roles); the actor gets exactly these")
	dataScope := fs.String("data-scope", "", "assign: own, team or company")
	team := fs.String("team", "", "assign: team ID for -data-scope team")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	dsn := os.Getenv("IDP_BRANT_POSTGRES_DSN")
	if dsn == "" {
		return errors.New("IDP_BRANT_POSTGRES_DSN is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	platform, err := bootstrap.Open(ctx, bootstrap.Config{PostgresDSN: dsn})
	if err != nil {
		return err
	}
	defer platform.Close()
	var epoch int64
	if args[0] == "role" {
		epoch, err = platform.DefineRoleFor(ctx, *tenant, *company, *role, *name, split(*templates))
	} else {
		epoch, err = platform.AssignRolesFor(ctx, *tenant, *company, *actor, *kind, split(*assigned), *dataScope, *team)
	}
	if err != nil {
		return err
	}
	fmt.Printf("DONE %s %s/%s, authority epoch %d\n", args[0], *tenant, *company, epoch)
	return nil
}

func split(list string) []string {
	var out []string
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
