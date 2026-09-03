// SPDX-FileCopyrightText: 2026 MKZ Systems LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

// Command fl-lobby runs the Fighters Legacy matchmaking/listing service.
//
// Hosting is self-host only and federated: anyone may run one, and players opt in by adding its
// URL to `[client] lobby_urls`. There is no central registry and no account system.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fighters-legacy/fl-lobby/internal/lobby"
)

// version is stamped at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fl-lobby: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	def := lobby.DefaultConfig()

	fs := flag.NewFlagSet("fl-lobby", flag.ContinueOnError)
	addr := fs.String("addr", def.Addr, "listen address")
	heartbeat := fs.Duration("heartbeat", def.Heartbeat, "assumed server heartbeat interval when a registration does not state one")
	ttlMult := fs.Float64("ttl-multiplier", def.TTLMultiplier, "entry lifetime as a multiple of the heartbeat")
	maxEntries := fs.Int("max-entries", def.MaxEntries, "maximum listed servers")
	maxPerHost := fs.Int("max-per-host", def.MaxPerHost, "maximum listed servers from one source address")
	writeRate := fs.Float64("write-rate", def.WriteRate, "POST/DELETE per second per source address")
	writeBurst := fs.Int("write-burst", def.WriteBurst, "POST/DELETE burst per source address")
	readRate := fs.Float64("read-rate", def.ReadRate, "GET per second per source address")
	readBurst := fs.Int("read-burst", def.ReadBurst, "GET burst per source address")
	proxies := fs.String("trusted-proxies", "", "comma-separated CIDRs whose X-Forwarded-For is believed (empty = trust none)")
	pruneEvery := fs.Duration("prune-interval", def.PruneInterval, "background sweep interval for expired entries")
	logLevel := fs.String("log-level", "info", "debug | info | warn | error")
	showVersion := fs.Bool("version", false, "print version and exit")

	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	// A container is usually configured by environment, not argv. Any flag NOT given explicitly
	// falls back to FL_LOBBY_<FLAG>, with '-' becoming '_' -- so -max-per-host is FL_LOBBY_MAX_PER_HOST.
	if err := applyEnv(fs); err != nil {
		return err
	}

	if *showVersion {
		fmt.Printf("fl-lobby %s\n", version)
		return nil
	}

	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("invalid -log-level %q", *logLevel)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	trusted, err := parsePrefixes(*proxies)
	if err != nil {
		return err
	}

	cfg := lobby.Config{
		Addr:           *addr,
		Heartbeat:      *heartbeat,
		TTLMultiplier:  *ttlMult,
		MaxEntries:     *maxEntries,
		MaxPerHost:     *maxPerHost,
		WriteRate:      *writeRate,
		WriteBurst:     *writeBurst,
		ReadRate:       *readRate,
		ReadBurst:      *readBurst,
		TrustedProxies: trusted,
		PruneInterval:  *pruneEvery,
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	svc := lobby.New(cfg, log, nil)
	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: svc.Handler(),
		// A public service needs every one of these. Without them a handful of idle or slow
		// connections holds sockets open indefinitely, which is a denial of service that costs
		// the attacker nothing.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		t := time.NewTicker(cfg.PruneInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				svc.Sweep()
			}
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		log.Info("fl-lobby listening",
			"addr", cfg.Addr, "version", version,
			"heartbeat", cfg.Heartbeat, "ttl", time.Duration(float64(cfg.Heartbeat)*cfg.TTLMultiplier),
			"max_entries", cfg.MaxEntries, "trusted_proxies", len(cfg.TrustedProxies))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// applyEnv fills in any flag the operator did not pass on the command line from the environment.
func applyEnv(fs *flag.FlagSet) error {
	explicit := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	var err error
	fs.VisitAll(func(f *flag.Flag) {
		if err != nil || explicit[f.Name] {
			return
		}
		name := "FL_LOBBY_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			return
		}
		if serr := f.Value.Set(v); serr != nil {
			err = fmt.Errorf("%s=%q: %w", name, v, serr)
		}
	})
	return err
}

func parsePrefixes(s string) ([]netip.Prefix, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			// A bare address is a reasonable thing for an operator to write for a single proxy.
			if a, aerr := netip.ParseAddr(part); aerr == nil {
				p = netip.PrefixFrom(a, a.BitLen())
			} else {
				return nil, fmt.Errorf("invalid -trusted-proxies entry %q", part)
			}
		}
		out = append(out, p)
	}
	return out, nil
}
