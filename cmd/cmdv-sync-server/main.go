// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Command cmdv-sync-server runs the reference sync server for CMDV Reader.
//
// Deliberately thin. Everything worth testing — reading the environment, validating it, preparing
// the database directory, routing, storage — lives in internal packages, because none of it is
// reachable by a test from a main package. What is left here is the order things happen in and what
// the operator is told.
//
// TLS is not terminated here. Anyone self-hosting already has a reverse proxy that does certificates
// better than this ever would, and half-doing it here would invite someone to run it directly on the
// internet with a certificate nobody renews.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/marc2912/cmdv-sync-server/internal/api"
	"github.com/marc2912/cmdv-sync-server/internal/auth"
	"github.com/marc2912/cmdv-sync-server/internal/config"
	"github.com/marc2912/cmdv-sync-server/internal/store"
)

// version is stamped at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

// Exit codes, from sysexits.h. Distinct ones save guessing whether a container that died
// immediately crashed or was handed something impossible, and both Docker and systemd surface them.
const (
	exitConfig = 78 // EX_CONFIG: the process is fine, what it was told is not
	exitIO     = 74 // EX_IOERR: the database could not be used
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	healthCheck := flag.Bool("health-check", false,
		"probe the health endpoint of a server on the configured host and port, then exit 0 or 1")
	backupTo := flag.String("backup", "",
		"write a consistent copy of the database to this path and exit; safe while the server is running")
	flag.Parse()

	if *showVersion {
		fmt.Println("cmdv-sync-server", version)
		return
	}

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cmdv-sync-server:", err)
		os.Exit(exitConfig)
	}

	// The health check is the binary itself rather than curl, because the runtime image is
	// FROM scratch and has neither a shell nor curl in it. It reads the same configuration the
	// server did, so it follows CMDV_SYNC_PORT without the Dockerfile having to repeat it.
	if *healthCheck {
		os.Exit(probeHealth(cfg))
	}

	if *backupTo != "" {
		os.Exit(backup(cfg, *backupTo))
	}

	run(cfg)
}

// backup writes a consistent copy of the database and exits.
//
// A subcommand of the server rather than a documented `sqlite3` incantation, because the runtime image
// has no shell and no `sqlite3` in it — and because a plain file copy of a WAL-mode database can
// capture a torn state that looks fine and restores broken. See store.Backup.
func backup(cfg *config.Config, destination string) int {
	st, err := store.Open(cfg.Database)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cmdv-sync-server: cannot open the database:", err)
		return exitIO
	}
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := st.Backup(ctx, destination); err != nil {
		fmt.Fprintln(os.Stderr, "cmdv-sync-server: backup failed:", err)
		return exitIO
	}
	fmt.Fprintln(os.Stderr, "cmdv-sync-server: wrote", destination)
	return 0
}

func run(cfg *config.Config) {
	for _, name := range config.Unrecognised(os.Environ()) {
		// A warning rather than a failure. A stray CMDV_SYNC_ variable is nearly always a typo worth
		// mentioning, but refusing to start over one would make this server the awkward member of a
		// compose file that also sets variables for something else.
		fmt.Fprintf(os.Stderr, "cmdv-sync-server: warning: %s is set but is not read by this server.\n", name)
	}

	if err := cfg.PrepareDatabaseDirectory(); err != nil {
		fmt.Fprintln(os.Stderr, "cmdv-sync-server:", err)
		os.Exit(exitConfig)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	// Opened and migrated *before* the listener starts, so a database this process cannot use makes
	// the server fail immediately and visibly rather than answer its health check and refuse every
	// sync.
	st, err := store.Open(cfg.Database)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cmdv-sync-server: cannot open the database:", err)
		os.Exit(exitIO)
	}
	defer st.Close()

	tokens := auth.New(cfg.Tokens)

	fmt.Fprintf(os.Stderr, "cmdv-sync-server %s\n%s\n\nTLS is not terminated here. Put this behind a reverse proxy; do not expose it directly.\n\n",
		version, cfg.Summary(tokens.Datasets()))

	server := &http.Server{
		Addr:    net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Handler: api.New(st, tokens, logger).Handler(),

		// A slow or stalled client must not be able to hold a connection open indefinitely. Generous
		// enough for a first sync of a long history over a slow link, bounded enough that an idle
		// half-open connection is reclaimed.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 16,
	}

	// SIGTERM is what `docker stop` sends, and honouring it is what makes a stop finish in-flight
	// syncs rather than sever them. This works only because the container runs this binary as PID 1 —
	// see the Dockerfile's ENTRYPOINT, which is in exec form for exactly that reason.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	failed := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()

	select {
	case err := <-failed:
		fmt.Fprintln(os.Stderr, "cmdv-sync-server: cannot listen:", err)
		os.Exit(exitIO)
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	// Bounded, so a client that never finishes cannot stop the process exiting. Shorter than
	// Docker's default ten-second stop timeout, so the graceful path completes rather than being
	// overtaken by SIGKILL.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown did not complete cleanly", "error", err)
	}
}

// probeHealth asks a running server whether it is well. Used by the container's HEALTHCHECK.
func probeHealth(cfg *config.Config) int {
	// The loopback address rather than cfg.Host: the check runs inside the container alongside the
	// server, and 0.0.0.0 is a bind address rather than somewhere to connect to.
	//
	// /healthz rather than /api/v1/health, because it sits outside the protocol version check. A
	// container's liveness must not be able to depend on a version header, and this way it provably
	// cannot. Both endpoints touch the database, so nothing is given up by choosing the stabler path.
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.Port)
	client := &http.Client{Timeout: 3 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "health check failed:", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "health check returned", resp.Status)
		return 1
	}
	return 0
}
