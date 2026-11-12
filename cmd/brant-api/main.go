// brant-api serves Brant's HTTP API: the S1 command routes and audited reads of EVD, PTY, CAT and
// INQ. Every /api request needs an OIDC access token from the configured issuer; actor, tenant and
// company come from the token, never from a request body.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "brant-api:", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:8080", "address to serve HTTP on")
	issuer := flag.String("oidc-issuer", "", "OIDC issuer URL; tokens must carry exactly this iss")
	audience := flag.String("oidc-audience", "", "audience the access tokens must name")
	jwks := flag.String("oidc-jwks", "", "JWKS URL; empty discovers it from the issuer")
	insecure := flag.Bool("oidc-insecure-http", false, "accept an http:// issuer and JWKS on a loopback host (local development only)")
	replaySink := flag.String("replay-sink", "", "HTTP sink for dead-letter replay by an authorized human; empty disables the route")
	replayDispatch := flag.Bool("replay-dispatch", false, "dead-letter replay delivers in process to the contexts' consumers, as brant-worker -dispatch, instead of -replay-sink")
	replayTimeout := flag.Duration("replay-timeout", 5*time.Second, "timeout for one replayed publish")
	types := flag.String("oidc-token-types", "", "comma-separated accepted JWT typ values; empty accepts at+jwt (RFC 9068) only")
	flag.Parse()
	dsn := os.Getenv("IDP_BRANT_POSTGRES_DSN")
	if dsn == "" || *issuer == "" || *audience == "" {
		return errors.New("IDP_BRANT_POSTGRES_DSN, -oidc-issuer and -oidc-audience are required")
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	platform, err := bootstrap.Open(ctx, bootstrap.Config{PostgresDSN: dsn})
	if err != nil {
		return err
	}
	defer platform.Close()
	handler, err := platform.API(bootstrap.APIConfig{
		Issuer: *issuer, Audience: *audience, JWKSURL: *jwks, TokenTypes: split(*types), AllowInsecureHTTP: *insecure,
		ReplaySinkURL: *replaySink, ReplayDispatch: *replayDispatch, ReplayPublishTimeout: *replayTimeout, Logger: logger,
	})
	if err != nil {
		return err
	}
	if err = platform.VerifyIssuer(ctx); err != nil {
		return err
	}
	srv := &http.Server{
		Addr: *listen, Handler: handler, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10,
	}
	failed := make(chan error, 1)
	go func() { failed <- srv.ListenAndServe() }()
	logger.Info("serving", "listen", *listen, "issuer", *issuer)
	select {
	case err = <-failed:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = srv.Shutdown(shutdown); err != nil {
		return err
	}
	logger.Info("stopped")
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
