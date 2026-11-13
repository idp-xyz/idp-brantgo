package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// proc is a Brant process the load generator started (brant-api, brant-worker); its output goes to
// a log file in the run's directory.
type proc struct {
	name string
	cmd  *exec.Cmd
	log  *os.File
	done chan error
}

func startProc(bin, name, logPath string, env []string, args ...string) (*proc, error) {
	f, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(filepath.Join(bin, name), args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = f, f
	if err = cmd.Start(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	p := &proc{name: name, cmd: cmd, log: f, done: make(chan error, 1)}
	go func() { p.done <- cmd.Wait() }()
	return p, nil
}

// cpuSeconds is the user and system CPU time the process has used so far (Linux /proc).
func (p *proc) cpuSeconds() float64 {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", p.cmd.Process.Pid))
	if err != nil {
		return 0
	}
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 13 {
		return 0
	}
	utime, _ := strconv.ParseFloat(f[11], 64)
	stime, _ := strconv.ParseFloat(f[12], 64)
	return (utime + stime) / 100 // USER_HZ is 100 on Linux
}

// stop sends SIGTERM and waits up to 15 s, then kills; it returns how the process ended.
func (p *proc) stop() error {
	if p == nil {
		return nil
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	var err error
	select {
	case err = <-p.done:
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		err = <-p.done
	}
	p.done <- err
	_ = p.log.Close()
	return err
}

// exited reports whether the process has ended, and how.
func (p *proc) exited() (bool, error) {
	select {
	case err := <-p.done:
		p.done <- err
		return true, err
	default:
		return false, nil
	}
}

// freePort returns a loopback port nobody listens on right now.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// withParams adds query parameters to a PostgreSQL URL DSN (application_name, pool_max_conns).
func withParams(dsn string, params map[string]string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", fmt.Errorf("the DSN must be a postgres:// URL")
	}
	q := u.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// waitHealthy polls GET /healthz until the API answers 200 or the process ends.
func waitHealthy(ctx context.Context, base string, p *proc) error {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if ended, err := p.exited(); ended {
			return fmt.Errorf("%s ended before it served (see %s): %w", p.name, p.log.Name(), err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return errors.New("brant-api did not answer /healthz within 60 s")
}

// admin runs brant-admin with args as the operator does (grant, assign, inbox).
func admin(ctx context.Context, bin, dsn string, args ...string) error {
	cmd := exec.CommandContext(ctx, filepath.Join(bin, "brant-admin"), args...)
	cmd.Env = append(os.Environ(), "IDP_BRANT_POSTGRES_DSN="+dsn)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("brant-admin %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// provision grants BRC-001's humans in the run's tenant, assigns the role templates of the
// documentation clerks and the credit managers, and provisions the event consumers' system actors
// for brant-worker -dispatch.
func provision(ctx context.Context, bin, dsn, tenant string) error {
	direct, roles := grants()
	for actor, permissions := range direct {
		if err := admin(ctx, bin, dsn, "grant", "-tenant", tenant, "-company", company, "-actor", actor, "-kind", "human",
			"-permissions", strings.Join(permissions, ","), "-data-scope", "company", "-team", "trade"); err != nil {
			return err
		}
	}
	for actor, role := range roles {
		if err := admin(ctx, bin, dsn, "assign", "-tenant", tenant, "-company", company, "-actor", actor, "-kind", "human",
			"-roles", role, "-data-scope", "company"); err != nil {
			return err
		}
	}
	return admin(ctx, bin, dsn, "inbox", "-tenant", tenant, "-company", company)
}
