package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// proc is a Brant process the harness started (brant-api, brant-worker); its output goes to a log
// file in the run's directory.
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

// kill ends the process with SIGKILL, as a crash would (AN05): no shutdown, nothing flushed.
func (p *proc) kill() {
	_ = p.cmd.Process.Kill()
	err := <-p.done
	p.done <- err
	_ = p.log.Close()
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
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("brant-api did not answer /healthz within 60 s")
}

// admin runs brant-admin with args as the operator does (grant, assign, inbox, agent).
func admin(ctx context.Context, bin, dsn string, args ...string) error {
	cmd := exec.CommandContext(ctx, filepath.Join(bin, "brant-admin"), args...)
	cmd.Env = append(os.Environ(), "IDP_BRANT_POSTGRES_DSN="+dsn)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("brant-admin %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// migrate runs brant-migrate -apply with the migrator's DSN, as the operator does before a release,
// and returns its last line ("DONE: n applied, m already recorded ...").
func migrate(ctx context.Context, bin, migratorDSN string) (string, error) {
	cmd := exec.CommandContext(ctx, filepath.Join(bin, "brant-migrate"), "-apply")
	cmd.Env = append(os.Environ(), "IDP_BRANT_MIGRATOR_DSN="+migratorDSN)
	out, err := cmd.CombinedOutput()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	last := lines[len(lines)-1]
	if err != nil {
		return last, fmt.Errorf("brant-migrate -apply: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return last, nil
}
