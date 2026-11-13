//go:build integration

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// WBS S5-11: the load generator runs as `make load` and `make drill` run it, against the migrated
// PostgreSQL 16 of the suite. It builds brant-api and brant-admin, provisions a fresh tenant with
// brant-admin, builds BRC-001's front over HTTP, primes one order cycle and runs one stage of one
// command user and one read user: every request of the window succeeds, cycles complete, and the
// observer samples the database and the Outbox. The relay (brant-worker -dispatch) stays off: it
// would deliver the events of the suite's other tests, which share this database and own their
// events; `make load` runs it against a database of its own. In writes mode every command
// it acknowledges is in its log and in the contexts' command ledgers (action_result), which is
// what the recovery drill compares a restored database with.
func TestLoadGeneratorSmoke(t *testing.T) {
	dsn, observerDSN := os.Getenv("IDP_BRANT_POSTGRES_DSN"), os.Getenv("IDP_BRANT_MIGRATOR_DSN")
	if dsn == "" || observerDSN == "" {
		if os.Getenv("CI") == "true" {
			t.Fatal("CI=true: IDP_BRANT_POSTGRES_DSN and IDP_BRANT_MIGRATOR_DSN are required")
		}
		t.Skip("IDP_BRANT_POSTGRES_DSN and IDP_BRANT_MIGRATOR_DSN are not set")
	}
	bin := t.TempDir()
	build := exec.Command("go", "build", "-o", bin, "./cmd/brant-api", "./cmd/brant-admin")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the processes: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	out := t.TempDir()
	// No relay: the suite's other packages share this database and their tests own their events.
	cfg, err := parseFlags([]string{"-bin", bin, "-dsn", dsn, "-observer-dsn", observerDSN, "-stages", "1", "-warmup", "1s",
		"-measure", "6s", "-prime", "1", "-worker=false", "-out", out, "-label", "smoke"})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := run(ctx, cfg)
	if err != nil {
		t.Fatalf("staged run: %v", err)
	}
	if err = writeReport(cfg, rep); err != nil {
		t.Fatal(err)
	}
	if rep.Setup.Requests < 90 || rep.Prime.Commands.OK != 17 || rep.Prime.Reads.OK != 4 || len(rep.Stages) != 1 {
		t.Fatalf("setup %+v prime %+v stages %d", rep.Setup, rep.Prime, len(rep.Stages))
	}
	s := rep.Stages[0]
	if s.Commands.OK == 0 || s.Commands.Errors+s.Commands.Timeouts+s.Commands.Transport != 0 || s.Reads.OK == 0 ||
		s.Reads.Errors+s.Reads.Timeouts+s.Reads.Transport != 0 || s.Cycles.Completed == 0 || s.Cycles.Failed != 0 {
		t.Fatalf("stage %+v %+v %+v", s.Commands, s.Reads, s.Cycles)
	}
	// A probe must finish within one sampling period; on a database shared with the rest of the suite
	// (a large Outbox backlog, other packages' load) a few probes can overrun it. The smoke test proves
	// the observer works, so it needs successful samples, not a run without a single late probe.
	if s.DB.Samples == 0 || s.DB.ObservationErrors > s.DB.Samples || s.Commands.Latency.P95 <= 0 || s.Reads.Latency.P99 <= 0 {
		t.Fatalf("observation %+v latency %+v %+v", s.DB, s.Commands.Latency, s.Reads.Latency)
	}
	for _, f := range []string{"report.json", "summary.md", "brant-api.log"} {
		if _, err = os.Stat(filepath.Join(out, f)); err != nil {
			t.Fatal(err)
		}
	}

	acks := filepath.Join(out, "acks.jsonl")
	wcfg, err := parseFlags([]string{"-mode", "writes", "-bin", bin, "-dsn", dsn, "-vus", "1", "-duration", "4s", "-worker=false",
		"-ack-log", acks, "-out", filepath.Join(out, "writes")})
	if err != nil {
		t.Fatal(err)
	}
	wrep, err := run(ctx, wcfg)
	if err != nil {
		t.Fatalf("writes run: %v", err)
	}
	check, err := checkAcks(ctx, dsn, wcfg.tenant, acks)
	if err != nil {
		t.Fatal(err)
	}
	if check.Acknowledged == 0 || check.Acknowledged != wrep.Writes.Commands.OK || check.Present != check.Acknowledged || check.Lost != 0 ||
		check.LostWindowS != 0 || check.LastPresentAt != check.LastAckAt {
		t.Fatalf("acknowledged %d commands, check %+v", wrep.Writes.Commands.OK, check)
	}
}
