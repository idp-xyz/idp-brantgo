// brant-cli runs local operations and demonstrations against Brant's PostgreSQL 16 database. It
// is not an authenticated remote API: it acts as synthetic actors it provisions itself.
//
//	IDP_BRANT_POSTGRES_DSN=... brant-cli foundation-demo [-directory DIR]
//
// foundation-demo (Python V0.50 `python -m brant_core foundation-demo`) provisions a fresh synthetic
// company in tenant tenant.synthetic.demo, publishes POL configurations of every kind through an
// independent editor and reviewer, captures a foundation snapshot, computes an exchange, a tax
// split and a unit conversion through POL's lookups, and writes foundation-demo-result.json into
// DIR (a new temporary directory when it is not given). DIR must not hold a result yet. Every value
// is synthetic: no real rate, tax policy or company.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
)

const usage = "usage: brant-cli foundation-demo [-directory DIR] (IDP_BRANT_POSTGRES_DSN names the database)"

func main() {
	if err := run(os.Args[1:], os.Getenv("IDP_BRANT_POSTGRES_DSN"), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "brant-cli:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

var errUsage = errors.New(usage)

func run(args []string, dsn string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "foundation-demo" {
		return errUsage
	}
	fs := flag.NewFlagSet("foundation-demo", flag.ContinueOnError)
	directory := fs.String("directory", "", "directory for foundation-demo-result.json; empty creates a temporary one")
	if err := fs.Parse(args[1:]); err != nil {
		return errors.Join(errUsage, err)
	}
	if fs.NArg() != 0 {
		return errUsage
	}
	if dsn == "" {
		return errors.New("IDP_BRANT_POSTGRES_DSN is required")
	}
	if *directory == "" {
		dir, err := os.MkdirTemp("", "brant-foundation-demo-")
		if err != nil {
			return err
		}
		*directory = dir
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	platform, err := bootstrap.Open(ctx, bootstrap.Config{PostgresDSN: dsn})
	if err != nil {
		return err
	}
	defer platform.Close()
	result, err := platform.FoundationDemo(ctx, bootstrap.DemoConfig{Directory: *directory})
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(struct {
		Synthetic          bool                   `json:"synthetic"`
		Tenant             string                 `json:"tenant"`
		Company            string                 `json:"company"`
		Configurations     int                    `json:"configurations"`
		ConfigurationKinds int                    `json:"configuration_kinds"`
		FX                 bootstrap.DemoAmount   `json:"fx"`
		Unit               bootstrap.DemoQuantity `json:"unit"`
		Tax                bootstrap.DemoTaxSplit `json:"tax"`
		ResultFile         string                 `json:"result_file"`
	}{result.Synthetic, result.Tenant, result.Company, result.Configurations, result.ConfigurationKinds, result.FX, result.Unit, result.Tax,
		result.ResultFile})
}
