// Command ping-lens is the network monitoring agent. It runs probes against
// configured targets, persists samples to a local spool, computes rollups,
// classifies outages, ships everything to Postgres on a fixed cadence, and
// serves a local dashboard.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jasonwbarnett/ping-lens/internal/buffer"
	"github.com/jasonwbarnett/ping-lens/internal/config"
	"github.com/jasonwbarnett/ping-lens/internal/db"
	"github.com/jasonwbarnett/ping-lens/internal/flush"
	"github.com/jasonwbarnett/ping-lens/internal/outage"
	"github.com/jasonwbarnett/ping-lens/internal/probe"
	"github.com/jasonwbarnett/ping-lens/internal/rollup"
	"github.com/jasonwbarnett/ping-lens/internal/sample"
	"github.com/jasonwbarnett/ping-lens/internal/server"
	"github.com/jasonwbarnett/ping-lens/internal/spool"
)

// version is set at build time via -ldflags "-X main.version=...".
// Defaults to "dev" for `go run` / unflagged builds.
var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/ping-lens/config.yaml", "path to YAML config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	log.SetFlags(log.LstdFlags | log.LUTC)

	if err := run(*configPath); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if cfg.Database.URL == "" {
		return errors.New("database url is empty (set database.url or database.url_env)")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// SIGUSR1 forces an immediate flush to Postgres. Useful for debugging
	// and to confirm probes are reaching the dashboard without waiting for
	// the next flush tick.
	flushSig := make(chan os.Signal, 1)
	signal.Notify(flushSig, syscall.SIGUSR1)
	defer signal.Stop(flushSig)

	// --- Storage ------------------------------------------------------------
	store, err := db.Open(ctx, cfg.Database.URL)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer store.Close()
	log.Printf("connected to postgres")

	// --- Spool --------------------------------------------------------------
	sp, err := spool.Open(cfg.Flush.SpoolDir)
	if err != nil {
		return fmt.Errorf("open spool: %w", err)
	}
	defer sp.Close()

	// --- Rollup accumulator -------------------------------------------------
	rollups := rollup.New(cfg.RollupWindow())

	// --- Flusher ------------------------------------------------------------
	flusher := flush.New(store, sp, rollups, cfg.Source, cfg.Retention.SpoolHours)

	// --- Outage tracker -----------------------------------------------------
	tracker := outage.NewTracker(cfg.Source, cfg.ISP.Name, cfg.Network.LocalGateway, cfg.Outage.MinConsecutiveFailures, flusher)

	// --- Probe pipeline -----------------------------------------------------
	targets := probe.Targets(cfg)
	if len(targets) == 0 {
		return errors.New("no probe targets configured")
	}
	logTargets(targets)

	sampleCh := make(chan sample.Sample, 1024)
	prober := probe.New(cfg, targets, sampleCh)

	// Wire fan-out: every sample goes to spool + rollup; ticks are observed
	// by the outage tracker after probe.tick() completes.
	spoolSink := &buffer.SpoolSink{Append: sp.Append}
	// Tick collator: aggregate samples per tick by timestamp grouped at probe
	// interval boundaries. Each sample arrives roughly together; we bucket
	// by truncated TS.
	collator := newTickCollator(cfg.ProbeInterval(), tracker)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		buffer.Run(ctx, sampleCh, spoolSink, rollups, collator)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		prober.Run(ctx)
	}()

	// --- Flush loop ---------------------------------------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()
		flusher.Run(ctx, cfg.FlushInterval())
	}()

	// SIGUSR1 → manual flush. Runs until ctx is done.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-flushSig:
				flusher.Trigger()
			}
		}
	}()

	// Periodic spool flush: pushes the in-memory bufio buffer to disk so
	// `tail -F` shows progress and crash loss is bounded by time, not
	// buffer size.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(cfg.SpoolFlushInterval())
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := sp.Flush(); err != nil {
					log.Printf("spool flush: %v", err)
				}
			}
		}
	}()

	// --- HTTP server --------------------------------------------------------
	srv, err := server.New(cfg, store)
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.Serve(ctx, cfg.HTTP.Listen); err != nil {
			log.Printf("http server: %v", err)
		}
	}()

	log.Printf("ping-lens %s started: source=%s isp=%q targets=%d interval=%s flush=%s",
		version, cfg.Source, cfg.ISP.Name, len(targets), cfg.ProbeInterval(), cfg.FlushInterval())

	<-ctx.Done()
	log.Printf("shutting down…")

	// ctx is already cancelled; goroutines will exit on their own. Close
	// any open outage event so it reaches the final-flush sink.
	tracker.CloseAll(time.Now().UTC())
	wg.Wait()
	return nil
}

func logTargets(ts []probe.Target) {
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.String())
	}
	log.Printf("targets: %s", strings.Join(names, ", "))
}

// tickCollator groups samples by their truncated probe-interval boundary
// and pushes each completed tick to the outage tracker.
//
// A tick is considered complete once a sample arrives with a strictly later
// boundary. Final flush happens on shutdown via CloseAll.
type tickCollator struct {
	window  time.Duration
	tracker *outage.Tracker

	mu      sync.Mutex
	current time.Time
	batch   []sample.Sample
}

func newTickCollator(interval time.Duration, tr *outage.Tracker) *tickCollator {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &tickCollator{window: interval, tracker: tr}
}

func (c *tickCollator) Consume(s sample.Sample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	bucket := s.TS.Truncate(c.window)
	if c.current.IsZero() {
		c.current = bucket
	}
	if bucket.After(c.current) {
		c.tracker.Observe(c.current, c.batch)
		c.current = bucket
		c.batch = c.batch[:0]
	}
	c.batch = append(c.batch, s)
}
