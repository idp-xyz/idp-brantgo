//go:build integration

package consistency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

const scopePolicy = `USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
	WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))`

// cstDDL is a throwaway copy of what CST will own: cost snapshots and the guard function CST
// publishes. The guard locks the snapshot row FOR SHARE in the caller's transaction.
const cstDDL = `
CREATE TABLE {{schema}}.cost_snapshot (
    tenant_id   text        NOT NULL,
    company_id  text        NOT NULL,
    snapshot_id text        NOT NULL,
    status      text        NOT NULL CHECK (status IN ('adoptable', 'denied')),
    revision    bigint      NOT NULL DEFAULT 1,
    changed_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, company_id, snapshot_id)
);
ALTER TABLE {{schema}}.cost_snapshot ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.cost_snapshot FORCE ROW LEVEL SECURITY;
CREATE POLICY snapshot_scope ON {{schema}}.cost_snapshot ` + scopePolicy + `;
GRANT SELECT, INSERT, UPDATE ON {{schema}}.cost_snapshot TO {{app}};

CREATE FUNCTION {{schema}}.guard_snapshot_adoptable(p_snapshot_id text)
    RETURNS TABLE (status text, revision bigint)
    LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog
AS $$
    SELECT s.status, s.revision FROM {{schema}}.cost_snapshot s
    WHERE s.tenant_id = current_setting('app.tenant_id', true)
      AND s.company_id = current_setting('app.company_id', true)
      AND s.snapshot_id = p_snapshot_id
    FOR SHARE
$$;
REVOKE ALL ON FUNCTION {{schema}}.guard_snapshot_adoptable(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION {{schema}}.guard_snapshot_adoptable(text) TO {{app}};
`

// trdDDL is a throwaway copy of what TRD will own: quote roots and sales orders.
const trdDDL = `
CREATE TABLE {{schema}}.quote_root (
    tenant_id    text   NOT NULL,
    company_id   text   NOT NULL,
    quote_id     text   NOT NULL,
    status       text   NOT NULL CHECK (status IN ('active', 'replaced', 'converted')),
    root_version bigint NOT NULL DEFAULT 1,
    PRIMARY KEY (tenant_id, company_id, quote_id)
);
CREATE TABLE {{schema}}.sales_order (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    order_id          text        NOT NULL,
    quote_id          text,
    snapshot_id       text,
    snapshot_revision bigint,
    activated_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, company_id, order_id)
);
ALTER TABLE {{schema}}.quote_root ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.quote_root FORCE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.sales_order ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.sales_order FORCE ROW LEVEL SECURITY;
CREATE POLICY quote_scope ON {{schema}}.quote_root ` + scopePolicy + `;
CREATE POLICY order_scope ON {{schema}}.sales_order ` + scopePolicy + `;
GRANT SELECT, INSERT, UPDATE ON {{schema}}.quote_root, {{schema}}.sales_order TO {{app}};
`

var errRejected = errors.New("spike: rejected by the business rule")

const (
	committed = "committed"
	rejected  = "rejected"
)

func outcomeOf(t *testing.T, err error) string {
	t.Helper()
	switch {
	case err == nil:
		return committed
	case errors.Is(err, errRejected):
		return rejected
	}
	t.Fatalf("unexpected error: %v", err)
	return ""
}

// spike holds one transaction owner per context, as internal/bootstrap will (rule T1).
type spike struct {
	t        *testing.T
	app      *pgxpool.Pool
	cst, trd *bentopg.DB
	cstTable string
	guard    string
	quotes   string
	orders   string
	scope    kernel.Scope
}

func newSpike(t *testing.T) *spike {
	t.Helper()
	migrator, app := pgtest.MigratorPool(t), pgtest.AppPool(t)
	cst := pgx.Identifier{pgtest.Schema(t, migrator, app, "brant_t_cst_", cstDDL)}.Sanitize()
	trd := pgx.Identifier{pgtest.Schema(t, migrator, app, "brant_t_trd_", trdDDL)}.Sanitize()
	newDB := func() *bentopg.DB {
		db, err := bentopg.NewDB(app)
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	recordServer(t, app)
	return &spike{t: t, app: app, cst: newDB(), trd: newDB(),
		cstTable: cst + ".cost_snapshot", guard: cst + ".guard_snapshot_adoptable",
		quotes: trd + ".quote_root", orders: trd + ".sales_order",
		scope: kernel.Scope{Tenant: kernel.TenantID(pgtest.UniqueID(t, "tenant.")), Company: "company.spike"}}
}

// in runs fn in a scoped transaction of db, with db's own executor.
func (s *spike) in(db *bentopg.DB, fn func(ctx context.Context, exec bentopg.Executor) error) error {
	return bento.ScopeTransactor{DB: db, Scope: s.scope}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return fn(ctx, exec)
	})
}

func (s *spike) must(err error) {
	s.t.Helper()
	if err != nil {
		s.t.Fatal(err)
	}
}

func (s *spike) args(id string) []any {
	return []any{string(s.scope.Tenant), string(s.scope.Company), id}
}

// hooks let a scenario interleave transactions deterministically.
type hooks struct {
	pid  chan<- int32 // receives the backend pid when the transaction starts
	hold func()       // runs after the business check passed, while the transaction holds its locks
}

func (h hooks) begin(ctx context.Context, exec bentopg.Executor) error {
	if h.pid == nil {
		return nil
	}
	var pid int32
	if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		return err
	}
	h.pid <- pid
	return nil
}

func (h hooks) wait() {
	if h.hold != nil {
		h.hold()
	}
}

// waitForLock returns once the backend whose pid arrives on pids waits for a lock; it fails if
// done fires first.
func waitForLock[T any](t *testing.T, app *pgxpool.Pool, pids <-chan int32, done <-chan T) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	var pid int32
	select {
	case pid = <-pids:
	case v := <-done:
		t.Fatalf("finished before it started: %v", v)
	case <-deadline:
		t.Fatal("backend never started")
	}
	for {
		var waiting bool
		if err := app.QueryRow(context.Background(),
			"SELECT coalesce(wait_event_type = 'Lock', false) FROM pg_stat_activity WHERE pid = $1", pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case v := <-done:
			t.Fatalf("finished without waiting for the lock: %v", v)
		case <-deadline:
			t.Fatal("backend never waited for the lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// --- CST ---------------------------------------------------------------------------------------

func (s *spike) createSnapshot(id string) {
	s.must(s.in(s.cst, func(ctx context.Context, exec bentopg.Executor) error {
		_, err := exec.Exec(ctx, "INSERT INTO "+s.cstTable+" (tenant_id, company_id, snapshot_id, status) VALUES ($1, $2, $3, 'adoptable')", s.args(id)...)
		return err
	}))
}

// readSnapshot is CST's query side, used before TRD opens its transaction (option A).
func (s *spike) readSnapshot(id string) (status string, revision int64) {
	s.must(s.in(s.cst, func(ctx context.Context, exec bentopg.Executor) error {
		return exec.QueryRow(ctx, "SELECT status, revision FROM "+s.cstTable+" WHERE tenant_id = $1 AND company_id = $2 AND snapshot_id = $3",
			s.args(id)...).Scan(&status, &revision)
	}))
	return status, revision
}

// deny withdraws a snapshot's adoptability. It returns when its UPDATE returned, so a scenario can
// show that the UPDATE waited for a guard holder.
func (s *spike) deny(id string, h hooks) (updated time.Time, err error) {
	err = s.in(s.cst, func(ctx context.Context, exec bentopg.Executor) error {
		if err := h.begin(ctx, exec); err != nil {
			return err
		}
		tag, err := exec.Exec(ctx, "UPDATE "+s.cstTable+" SET status = 'denied', revision = revision + 1, changed_at = clock_timestamp()"+
			" WHERE tenant_id = $1 AND company_id = $2 AND snapshot_id = $3 AND status = 'adoptable'", s.args(id)...)
		updated = time.Now()
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errRejected
		}
		h.wait()
		return nil
	})
	return updated, err
}

// --- TRD ---------------------------------------------------------------------------------------

func (s *spike) insertOrder(ctx context.Context, exec bentopg.Executor, orderID, quoteID, snapshotID string, revision int64) error {
	_, err := exec.Exec(ctx, "INSERT INTO "+s.orders+" (tenant_id, company_id, order_id, quote_id, snapshot_id, snapshot_revision)"+
		" VALUES ($1, $2, $3, NULLIF($4::text, ''), NULLIF($5::text, ''), NULLIF($6::bigint, 0))", append(s.args(orderID), quoteID, snapshotID, revision)...)
	return err
}

// activatePrefetched is option A: the transaction trusts a qualification read before it began.
func (s *spike) activatePrefetched(orderID, snapshotID, status string, revision int64) error {
	return s.in(s.trd, func(ctx context.Context, exec bentopg.Executor) error {
		if status != "adoptable" {
			return errRejected
		}
		return s.insertOrder(ctx, exec, orderID, "", snapshotID, revision)
	})
}

// activateGuarded is option B: inside TRD's transaction, through TRD's own executor, it calls the
// guard CST publishes, which locks the snapshot row FOR SHARE until TRD commits.
func (s *spike) activateGuarded(orderID, snapshotID string, h hooks) error {
	return s.in(s.trd, func(ctx context.Context, exec bentopg.Executor) error {
		if err := h.begin(ctx, exec); err != nil {
			return err
		}
		var status string
		var revision int64
		err := exec.QueryRow(ctx, "SELECT status, revision FROM "+s.guard+"($1)", snapshotID).Scan(&status, &revision)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "adoptable") {
			return errRejected
		}
		if err != nil {
			return err
		}
		h.wait()
		return s.insertOrder(ctx, exec, orderID, "", snapshotID, revision)
	})
}

func (s *spike) createQuote(id string) {
	s.must(s.in(s.trd, func(ctx context.Context, exec bentopg.Executor) error {
		_, err := exec.Exec(ctx, "INSERT INTO "+s.quotes+" (tenant_id, company_id, quote_id, status) VALUES ($1, $2, $3, 'active')", s.args(id)...)
		return err
	}))
}

func (s *spike) readRoot(ctx context.Context, exec bentopg.Executor, quoteID string, rootLock bool) (string, error) {
	q := "SELECT status FROM " + s.quotes + " WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3"
	if rootLock {
		q += " FOR UPDATE"
	}
	var status string
	err := exec.QueryRow(ctx, q, s.args(quoteID)...).Scan(&status)
	return status, err
}

// replace supersedes an active quote. With rootLock it locks the quote root FOR UPDATE before it
// reads, as ADR-BR-011 proposes; without, it checks and then acts.
func (s *spike) replace(quoteID string, rootLock bool, h hooks) error {
	return s.in(s.trd, func(ctx context.Context, exec bentopg.Executor) error {
		if err := h.begin(ctx, exec); err != nil {
			return err
		}
		status, err := s.readRoot(ctx, exec, quoteID, rootLock)
		if err != nil {
			return err
		}
		if status != "active" {
			return errRejected
		}
		h.wait()
		_, err = exec.Exec(ctx, "UPDATE "+s.quotes+" SET status = 'replaced', root_version = root_version + 1"+
			" WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3", s.args(quoteID)...)
		return err
	})
}

// convert turns an active quote into an order and consumes its conversion; the writes are the same
// with and without the root lock.
func (s *spike) convert(quoteID, orderID string, rootLock bool, h hooks) error {
	return s.in(s.trd, func(ctx context.Context, exec bentopg.Executor) error {
		if err := h.begin(ctx, exec); err != nil {
			return err
		}
		status, err := s.readRoot(ctx, exec, quoteID, rootLock)
		if err != nil {
			return err
		}
		if status != "active" {
			return errRejected
		}
		h.wait()
		if err = s.insertOrder(ctx, exec, orderID, quoteID, "", 0); err != nil {
			return err
		}
		_, err = exec.Exec(ctx, "UPDATE "+s.quotes+" SET status = 'converted', root_version = root_version + 1"+
			" WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3", s.args(quoteID)...)
		return err
	})
}

// --- report ------------------------------------------------------------------------------------

type result struct {
	ID       string            `json:"id"`
	ADR      string            `json:"adr"`
	Option   string            `json:"option"`
	Sequence string            `json:"sequence"`
	Outcomes map[string]string `json:"outcomes"`
	Expected string            `json:"expected"`
	Verdict  string            `json:"verdict"`
	Detail   string            `json:"detail,omitempty"`
}

var report struct {
	sync.Mutex
	Server  string   `json:"server"`
	Results []result `json:"results"`
}

func recordServer(t *testing.T, app *pgxpool.Pool) {
	var v, iso string
	if err := app.QueryRow(context.Background(), "SELECT version(), current_setting('default_transaction_isolation')").Scan(&v, &iso); err != nil {
		t.Fatal(err)
	}
	report.Lock()
	report.Server = v + "; default isolation " + iso
	report.Unlock()
}

func record(r result) {
	report.Lock()
	report.Results = append(report.Results, r)
	report.Unlock()
}

func verdict(ok bool) string {
	if ok {
		return "consistent"
	}
	return "violation"
}

func TestMain(m *testing.M) {
	code := m.Run()
	if path := os.Getenv("BRANT_SPIKE_REPORT"); path != "" {
		report.Lock()
		out, err := json.MarshalIndent(struct {
			Generated string   `json:"generated"`
			Server    string   `json:"server"`
			Results   []result `json:"results"`
		}{kernel.FormatTimestamp(time.Now()), report.Server, report.Results}, "", "  ")
		report.Unlock()
		if err == nil {
			err = os.WriteFile(path, append(out, '\n'), 0o644)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "write spike report:", err)
			code = 1
		}
	}
	os.Exit(code)
}

// --- SC-04 · ADR-BR-004 ------------------------------------------------------------------------

// Option A: TRD reads CST's qualification before its transaction and trusts it. A denial that
// commits in between does not stop the activation, so option A needs a compensating risk pause.
func TestSC04OptionAPrefetchLeavesACommitWindow(t *testing.T) {
	s := newSpike(t)
	s.createSnapshot("cs.a")
	status, revision := s.readSnapshot("cs.a")
	_, err := s.deny("cs.a", hooks{})
	s.must(err)
	activation := outcomeOf(t, s.activatePrefetched("so.a", "cs.a", status, revision))
	var flagged int
	s.must(s.in(s.trd, func(ctx context.Context, exec bentopg.Executor) error {
		return exec.QueryRow(ctx, "SELECT count(*) FROM "+s.orders+" o JOIN "+s.cstTable+" c USING (tenant_id, company_id, snapshot_id)"+
			" WHERE c.status = 'denied' AND c.changed_at < o.activated_at").Scan(&flagged)
	}))
	if activation != committed || flagged != 1 {
		t.Fatalf("activation %s, %d orders activated after their snapshot was denied; want the window reproduced", activation, flagged)
	}
	record(result{ID: "SC-04-A", ADR: "ADR-BR-004", Option: "A: prefetch outside the transaction",
		Sequence: "TRD reads adoptable; CST denial commits; TRD activates",
		Outcomes: map[string]string{"denial": committed, "activation": activation},
		Expected: "activation rejected (the denial committed first)", Verdict: verdict(false),
		Detail: fmt.Sprintf("%d order active on a snapshot denied before activation; only an event-driven risk pause can catch it afterwards", flagged)})
}

// Option B: the guard runs in TRD's transaction through TRD's executor and holds the snapshot row
// FOR SHARE until TRD commits. Whichever transaction locks the row first decides.
func TestSC04OptionBGuardDecidesEveryCommitOrder(t *testing.T) {
	s := newSpike(t)

	t.Run("denial committed first", func(t *testing.T) {
		s.createSnapshot("cs.b1")
		_, err := s.deny("cs.b1", hooks{})
		s.must(err)
		activation := outcomeOf(t, s.activateGuarded("so.b1", "cs.b1", hooks{}))
		if activation != rejected {
			t.Fatalf("activation %s, want rejected", activation)
		}
		record(result{ID: "SC-04-B1", ADR: "ADR-BR-004", Option: "B: guard in the downstream transaction",
			Sequence: "CST denial commits; TRD activates through the guard",
			Outcomes: map[string]string{"denial": committed, "activation": activation},
			Expected: "activation rejected", Verdict: verdict(true)})
	})

	t.Run("activation holds the guard first", func(t *testing.T) {
		s.createSnapshot("cs.b2")
		type denial struct {
			updated time.Time
			err     error
		}
		denied := make(chan denial, 1)
		pids := make(chan int32, 1)
		var released time.Time
		err := s.activateGuarded("so.b2", "cs.b2", hooks{hold: func() {
			go func() {
				var d denial
				d.updated, d.err = s.deny("cs.b2", hooks{pid: pids})
				denied <- d
			}()
			waitForLock(t, s.app, pids, denied)
			released = time.Now() // the activation writes and commits after this
		}})
		activation := outcomeOf(t, err)
		d := <-denied
		denialOutcome := outcomeOf(t, d.err)
		status, _ := s.readSnapshot("cs.b2")
		if activation != committed || denialOutcome != committed || !d.updated.After(released) || status != "denied" {
			t.Fatalf("activation %s, denial %s (UPDATE returned after the guard holder moved on: %v), snapshot %s",
				activation, denialOutcome, d.updated.After(released), status)
		}
		record(result{ID: "SC-04-B2", ADR: "ADR-BR-004", Option: "B: guard in the downstream transaction",
			Sequence: "TRD guard passes and holds FOR SHARE; CST denial starts and waits on the lock; TRD commits; denial commits",
			Outcomes: map[string]string{"activation": activation, "denial": denialOutcome},
			Expected: "activation stands (it committed first); the denial follows and raises the risk event", Verdict: verdict(true),
			Detail: fmt.Sprintf("the denial waited on the guard row and its UPDATE returned %s after the activation went on to commit", d.updated.Sub(released).Round(time.Microsecond))})
	})

	t.Run("denial holds the row first", func(t *testing.T) {
		s.createSnapshot("cs.b3")
		activated := make(chan error, 1)
		pids := make(chan int32, 1)
		_, err := s.deny("cs.b3", hooks{hold: func() {
			go func() { activated <- s.activateGuarded("so.b3", "cs.b3", hooks{pid: pids}) }()
			waitForLock(t, s.app, pids, activated)
		}})
		denialOutcome := outcomeOf(t, err)
		activation := outcomeOf(t, <-activated)
		if denialOutcome != committed || activation != rejected {
			t.Fatalf("denial %s, activation %s; want the waiting activation rejected", denialOutcome, activation)
		}
		record(result{ID: "SC-04-B3", ADR: "ADR-BR-004", Option: "B: guard in the downstream transaction",
			Sequence: "CST denial updates and holds the row; TRD guard waits; denial commits; guard re-reads",
			Outcomes: map[string]string{"denial": denialOutcome, "activation": activation},
			Expected: "activation rejected", Verdict: verdict(true)})
	})
}

// --- SC-03 · ADR-BR-011 ------------------------------------------------------------------------

func (s *spike) quoteState(quoteID string) (status string, orders int) {
	s.must(s.in(s.trd, func(ctx context.Context, exec bentopg.Executor) error {
		return exec.QueryRow(ctx, "SELECT q.status, (SELECT count(*) FROM "+s.orders+" o WHERE o.tenant_id = q.tenant_id AND o.company_id = q.company_id AND o.quote_id = q.quote_id)"+
			" FROM "+s.quotes+" q WHERE q.tenant_id = $1 AND q.company_id = $2 AND q.quote_id = $3", s.args(quoteID)...).Scan(&status, &orders)
	}))
	return status, orders
}

// Without the root lock both requests check "active" before either writes, and both commit: the
// quote is replaced and converted at once.
func TestSC03CheckThenActWithoutRootLockLetsBothWin(t *testing.T) {
	s := newSpike(t)
	s.createQuote("q.nolock")
	replaceRead, convertRead := make(chan struct{}), make(chan struct{})
	replaced := make(chan error, 1)
	go func() {
		replaced <- s.replace("q.nolock", false, hooks{hold: func() { close(replaceRead); <-convertRead }})
	}()
	convertErr := s.convert("q.nolock", "so.nolock", false, hooks{hold: func() { <-replaceRead; close(convertRead) }})
	replace, conversion := outcomeOf(t, <-replaced), outcomeOf(t, convertErr)
	status, orders := s.quoteState("q.nolock")
	if replace != committed || conversion != committed {
		t.Fatalf("replace %s, convert %s; want both committed to reproduce the anomaly", replace, conversion)
	}
	record(result{ID: "SC-03-NOLOCK", ADR: "ADR-BR-011", Option: "check then act, no root lock",
		Sequence: "replace and convert both read active; both write; both commit",
		Outcomes: map[string]string{"replace": replace, "convert": conversion},
		Expected: "exactly one succeeds", Verdict: verdict(false),
		Detail: fmt.Sprintf("final status %s with %d order(s): a replaced quote was also converted", status, orders)})
}

// With the root locked FOR UPDATE before the read, the second request waits, re-reads the committed
// state and is rejected, in either order.
func TestSC03RootLockLetsExactlyOneWin(t *testing.T) {
	s := newSpike(t)
	for _, first := range []string{"replace", "convert"} {
		t.Run(first+" first", func(t *testing.T) {
			quote, order := "q.lock."+first, "so.lock."+first
			s.createQuote(quote)
			second := make(chan error, 1)
			pids := make(chan int32, 1)
			var firstErr error
			if first == "replace" {
				firstErr = s.replace(quote, true, hooks{hold: func() {
					go func() { second <- s.convert(quote, order, true, hooks{pid: pids}) }()
					waitForLock(t, s.app, pids, second)
				}})
			} else {
				firstErr = s.convert(quote, order, true, hooks{hold: func() {
					go func() { second <- s.replace(quote, true, hooks{pid: pids}) }()
					waitForLock(t, s.app, pids, second)
				}})
			}
			a, b := outcomeOf(t, firstErr), outcomeOf(t, <-second)
			status, orders := s.quoteState(quote)
			wantStatus, wantOrders := map[string]string{"replace": "replaced", "convert": "converted"}[first], map[string]int{"replace": 0, "convert": 1}[first]
			if a != committed || b != rejected || status != wantStatus || orders != wantOrders {
				t.Fatalf("first %s, second %s, status %s, %d orders", a, b, status, orders)
			}
			record(result{ID: "SC-03-ROOTLOCK-" + first, ADR: "ADR-BR-011", Option: "root row FOR UPDATE first",
				Sequence: first + " locks the root; the other request waits on the lock; " + first + " commits; the other re-reads",
				Outcomes: map[string]string{first: a, "other": b},
				Expected: "exactly one succeeds", Verdict: verdict(true),
				Detail: fmt.Sprintf("final status %s with %d order(s)", status, orders)})
		})
	}
}

// Repeated unsynchronized races, with 2 ms of work between check and write as rendering or
// numbering would take: the root lock never lets both win and never deadlocks.
func TestSC03StressComparesTheTwoImplementations(t *testing.T) {
	s := newSpike(t)
	const rounds = 200
	for _, rootLock := range []bool{false, true} {
		both, none := 0, 0
		for i := range rounds {
			quote := fmt.Sprintf("q.stress.%t.%d", rootLock, i)
			s.createQuote(quote)
			work := hooks{hold: func() { time.Sleep(2 * time.Millisecond) }}
			var replaceErr, convertErr error
			var wg sync.WaitGroup
			start := make(chan struct{})
			wg.Add(2)
			go func() { defer wg.Done(); <-start; replaceErr = s.replace(quote, rootLock, work) }()
			go func() { defer wg.Done(); <-start; convertErr = s.convert(quote, "so."+quote, rootLock, work) }()
			close(start)
			wg.Wait()
			r, c := outcomeOf(t, replaceErr), outcomeOf(t, convertErr)
			switch {
			case r == committed && c == committed:
				both++
			case r == rejected && c == rejected:
				none++
			}
		}
		name := map[bool]string{false: "no root lock", true: "root row FOR UPDATE first"}[rootLock]
		if rootLock && (both != 0 || none != 0) {
			t.Errorf("%s: %d rounds with two winners, %d with none", name, both, none)
		}
		t.Logf("%s: %d of %d rounds had two winners", name, both, rounds)
		record(result{ID: fmt.Sprintf("SC-03-STRESS-%s", map[bool]string{false: "NOLOCK", true: "ROOTLOCK"}[rootLock]), ADR: "ADR-BR-011",
			Option: name, Sequence: fmt.Sprintf("%d unsynchronized replace/convert races, 2 ms of work between check and write", rounds),
			Outcomes: map[string]string{"rounds_with_two_winners": fmt.Sprint(both), "rounds_with_no_winner": fmt.Sprint(none)},
			Expected: "exactly one winner in every round", Verdict: verdict(both == 0 && none == 0)})
	}
}
