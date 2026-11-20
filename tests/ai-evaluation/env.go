package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// env is the running system under evaluation: the issuer, brant-api, the run's relay and the bundle
// every task names.
type env struct {
	cfg       config
	run       *EvalRun
	is        *issuer
	api       *proc
	base      string
	dispatch  *proc
	bundle    BundleRef
	templates map[string][]string
	hc        *http.Client
	keys      atomic.Int64

	mu  sync.Mutex
	obs []Probe
}

func (e *env) close() {
	_ = e.dispatch.stop()
	_ = e.api.stop()
	if e.is != nil {
		e.is.close()
	}
}

func (e *env) probe(p Probe) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.obs = append(e.obs, p)
}

func (e *env) probes() []Probe {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.obs)
}

// startEnv migrates the database, starts brant-api behind the local issuer and the run's relay,
// and learns the ExecutionBundle of the agent runner from a probe worker.
func startEnv(ctx context.Context, cfg config, r *EvalRun) (*env, error) {
	e := &env{cfg: cfg, run: r, hc: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64, IdleConnTimeout: 90 * time.Second}}}
	var err error
	if e.templates, err = roleTemplates(cfg.archive); err != nil {
		return e, err
	}
	last, err := migrate(ctx, cfg.bin, cfg.migrator)
	r.Environment.Migrate = last
	if err != nil {
		return e, err
	}
	r.Environment.Database = databaseOf(cfg.dsn)
	if e.is, err = newIssuer(); err != nil {
		return e, err
	}
	if e.api, e.base, err = e.startAPI(ctx, filepath.Join(cfg.out, "brant-api.log")); err != nil {
		return e, err
	}
	e.dispatch, err = startProc(cfg.bin, "brant-worker", filepath.Join(cfg.out, "brant-worker-dispatch.log"),
		[]string{"IDP_BRANT_POSTGRES_DSN=" + cfg.dsn}, "-dispatch", "-poll", "200ms", "-probe-interval", "0")
	if err != nil {
		return e, err
	}
	if err = e.learnBundle(ctx); err != nil {
		return e, fmt.Errorf("the ExecutionBundle of the agent runner: %w", err)
	}
	r.BundleRef = e.bundle
	return e, nil
}

// startAPI starts a brant-api on a free loopback port behind the run's issuer.
func (e *env) startAPI(ctx context.Context, logPath string) (*proc, string, error) {
	port, err := freePort()
	if err != nil {
		return nil, "", err
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)
	p, err := startProc(e.cfg.bin, "brant-api", logPath, []string{"IDP_BRANT_POSTGRES_DSN=" + e.cfg.dsn},
		"-listen", addr, "-oidc-issuer", e.is.url, "-oidc-audience", audience, "-oidc-insecure-http")
	if err != nil {
		return nil, "", err
	}
	base := "http://" + addr
	if err = waitHealthy(ctx, base, p); err != nil {
		_ = p.stop()
		return nil, "", err
	}
	return p, base, nil
}

// agentModelArgs are brant-worker's model flags of the suite, without the script.
func (e *env) agentModelArgs() []string {
	if e.cfg.suite == suiteLive {
		return e.cfg.live.workerArgs()
	}
	return []string{"-model", "scripted"}
}

var bundleLog = regexp.MustCompile(`bundle=(\S+) bundle_sha256=([0-9a-f]{64})`)

// learnBundle starts brant-worker -agent in a probe tenant with the suite's model and reads the
// bundle and its SHA-256 the runner logs at start (S4 C2 report: task.create names it, else the
// task is paused EXECUTION_BUNDLE_INCOMPATIBLE), then the bundle itself over HTTP.
func (e *env) learnBundle(ctx context.Context) error {
	tenant := "eval." + e.run.RunTag + ".probe"
	if err := admin(ctx, e.cfg.bin, e.cfg.dsn, "agent", "-tenant", tenant, "-company", company); err != nil {
		return err
	}
	if err := admin(ctx, e.cfg.bin, e.cfg.dsn, "grant", "-tenant", tenant, "-company", company, "-actor", "probe.reader", "-kind", kindHuman,
		"-permissions", "ai.read,tsk.read", "-data-scope", "company"); err != nil {
		return err
	}
	if err := admin(ctx, e.cfg.bin, e.cfg.dsn, "assign", "-tenant", tenant, "-company", company, "-actor", "agent.probe", "-kind", kindAI,
		"-roles", "ai_q01_agent", "-data-scope", "company"); err != nil {
		return err
	}
	args := append([]string{"-agent", "-agent-scope", tenant + "/" + company, "-agent-id", "agent.probe", "-agent-poll", "1s"}, e.agentModelArgs()...)
	if e.cfg.suite == suiteScripted {
		script := filepath.Join(e.cfg.out, "script-probe.json")
		if err := os.WriteFile(script, []byte("[]"), 0o600); err != nil {
			return err
		}
		args = append(args, "-model-script", script)
	}
	logPath := filepath.Join(e.cfg.out, "brant-worker-probe.log")
	p, err := startProc(e.cfg.bin, "brant-worker", logPath, []string{"IDP_BRANT_POSTGRES_DSN=" + e.cfg.dsn}, args...)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		raw, _ := os.ReadFile(logPath)
		if m := bundleLog.FindSubmatch(raw); m != nil {
			e.bundle = BundleRef{BundleID: string(m[1]), SHA256: string(m[2])}
			break
		}
		if ended, werr := p.exited(); ended {
			return fmt.Errorf("the probe worker ended (see %s): %w", logPath, werr)
		}
		if time.Now().After(deadline) {
			_ = p.stop()
			return fmt.Errorf("the probe worker logged no bundle within 60 s (see %s)", logPath)
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = p.stop()
	c := e.client(tenant).as("probe.reader", kindHuman)
	b, err := c.get(ctx, "/api/brant/v1/ai/execution_bundles/"+e.bundle.BundleID+"/"+e.bundle.SHA256)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(b["bundle"])
	if err != nil {
		return err
	}
	e.run.ExecutionBundle = raw
	if m, ok := b["bundle"].(map[string]any); ok {
		if mp, ok := m["model"].(map[string]any); ok {
			e.run.Model.ProfileID, _ = mp["profile_id"].(string)
			e.run.Model.Provider, _ = mp["provider"].(string)
		}
	}
	return nil
}

// client is an API client of tenant on the run's brant-api.
func (e *env) client(tenant string) *api {
	return &api{base: e.base, tenant: tenant, company: company, hc: e.hc, is: e.is, keys: &e.keys, prefix: "ev" + e.run.RunTag + "."}
}

func databaseOf(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return "PostgreSQL 16"
	}
	host := dsn[at+1:]
	if q := strings.Index(host, "?"); q >= 0 {
		host = host[:q]
	}
	return "PostgreSQL 16 at " + host
}

// roleTemplates reads the role templates of the master archive (BC-D02): the permissions of each
// template as the archive states them, which brant-admin assign also applies.
func roleTemplates(archive string) (map[string][]string, error) {
	raw, err := os.ReadFile(filepath.Join(archive, "05_TACTICAL_DDD", "06_IDP_Brant_Role_Templates_V0.1.json"))
	if err != nil {
		return nil, fmt.Errorf("role templates: %w", err)
	}
	var doc struct {
		Templates []struct {
			Code        string   `json:"code"`
			Permissions []string `json:"permissions"`
		} `json:"templates"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("role templates: %w", err)
	}
	out := map[string][]string{}
	for _, t := range doc.Templates {
		out[t.Code] = t.Permissions
	}
	for _, need := range []string{"sales_clerk", "quote_reviewer", "order_reviewer", "ai_q01_agent"} {
		if len(out[need]) == 0 {
			return nil, errors.New("role templates: no template " + need)
		}
	}
	return out, nil
}

// reviewerExtra is what the reviewers' templates lack for a step on a bound root through the
// task-step route (D-2, TRD-INV-015): the execution entry records the person's attempt and
// reconciles it as the caller (action.record_attempt, action.reconcile). Decision D1-D10: the
// evaluation grants them on top of the template and reports the gap.
var reviewerExtra = []string{"action.record_attempt", "action.reconcile"}

// provision grants the people of a trial's tenant: the setup people as tests/load does, the
// documentation clerks and credit managers by their templates, the task's owner (sales_clerk) and
// agent (ai_q01_agent) by their templates, the reviewers by their templates plus reviewerExtra, the
// evaluation's platform actor, the event consumers (brant-admin inbox) and the agent runner's
// system actor (brant-admin agent).
func (e *env) provision(ctx context.Context, tenant string) error {
	grant := func(actor, kind string, permissions []string) error {
		return admin(ctx, e.cfg.bin, e.cfg.dsn, "grant", "-tenant", tenant, "-company", company, "-actor", actor, "-kind", kind,
			"-permissions", strings.Join(permissions, ","), "-data-scope", "company", "-team", "trade")
	}
	assign := func(actor, kind, role string) error {
		return admin(ctx, e.cfg.bin, e.cfg.dsn, "assign", "-tenant", tenant, "-company", company, "-actor", actor, "-kind", kind,
			"-roles", role, "-data-scope", "company")
	}
	for _, a := range []string{actorSales, actorAdmin, actorController, actorCompliance, actorBuyer, actorCostChecker} {
		if err := grant(a, kindHuman, setupPermissions); err != nil {
			return err
		}
	}
	for actor, role := range map[string]string{actorDocumentation: "documentation", actorDocVerifier: "documentation",
		actorCreditManager: "credit_manager", actorCreditSecond: "credit_manager", actorClerk: "sales_clerk"} {
		if err := assign(actor, kindHuman, role); err != nil {
			return err
		}
	}
	if err := assign(actorAgent, kindAI, "ai_q01_agent"); err != nil {
		return err
	}
	for actor, tpl := range map[string]string{actorQuoteReviewer: "quote_reviewer", actorOrderReviewer: "order_reviewer"} {
		if err := grant(actor, kindHuman, append(slices.Clone(e.templates[tpl]), reviewerExtra...)); err != nil {
			return err
		}
	}
	if err := grant(actorPlatform, kindSystem, platformPermissions); err != nil {
		return err
	}
	if err := admin(ctx, e.cfg.bin, e.cfg.dsn, "inbox", "-tenant", tenant, "-company", company); err != nil {
		return err
	}
	return admin(ctx, e.cfg.bin, e.cfg.dsn, "agent", "-tenant", tenant, "-company", company)
}
