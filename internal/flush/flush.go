// Package flush ships data from the local spool + in-memory rollup
// accumulator to Postgres on a fixed cadence.
package flush

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jasonwbarnett/ping-lens/internal/db"
	"github.com/jasonwbarnett/ping-lens/internal/outage"
	"github.com/jasonwbarnett/ping-lens/internal/rollup"
	"github.com/jasonwbarnett/ping-lens/internal/sample"
)

// Spool is the subset of *spool.Spool the flusher needs. Allows swapping the
// real spool in tests.
type Spool interface {
	Rotate() (string, error)
	Ready() ([]string, error)
}

// Flusher orchestrates one flush cycle: rotate spool, copy samples,
// upsert rollups, append outages, delete consumed files.
type Flusher struct {
	store      *db.Store
	spool      Spool
	rollups    *rollup.Accumulator
	source     string
	spoolHours int

	mu      sync.Mutex
	pending []outage.Event
}

func New(store *db.Store, sp Spool, rollups *rollup.Accumulator, source string, spoolHours int) *Flusher {
	if spoolHours <= 0 {
		spoolHours = 24
	}
	return &Flusher{
		store:      store,
		spool:      sp,
		rollups:    rollups,
		source:     source,
		spoolHours: spoolHours,
	}
}

// RecordOutage implements outage.EventSink. Events are queued and flushed
// on the next cycle.
func (f *Flusher) RecordOutage(e outage.Event) {
	f.mu.Lock()
	f.pending = append(f.pending, e)
	f.mu.Unlock()
}

func (f *Flusher) drainOutages() []outage.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pending) == 0 {
		return nil
	}
	out := f.pending
	f.pending = nil
	return out
}

// Run loops until ctx is done, flushing on each tick.
func (f *Flusher) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			f.finalFlush(context.Background())
			return
		case <-t.C:
			if err := f.Once(ctx); err != nil {
				log.Printf("flush error: %v", err)
			}
		}
	}
}

// Once executes a single flush cycle.
func (f *Flusher) Once(ctx context.Context) error {
	// 1. Rotate current spool file -> ready.
	if _, err := f.spool.Rotate(); err != nil {
		return fmt.Errorf("rotate: %w", err)
	}
	// 2. Process any ready files.
	files, err := f.spool.Ready()
	if err != nil {
		return fmt.Errorf("list ready: %w", err)
	}
	for _, path := range files {
		if err := f.consumeFile(ctx, path); err != nil {
			log.Printf("consume %s: %v", path, err)
		}
	}
	// 3. Closed rollup buckets.
	rows := f.rollups.Flush(time.Now().UTC())
	if err := f.store.UpsertRollups(ctx, rows); err != nil {
		return fmt.Errorf("upsert rollups: %w", err)
	}
	// 4. Pending outages.
	events := f.drainOutages()
	if err := f.store.InsertOutages(ctx, events); err != nil {
		// requeue so we try again next cycle
		f.mu.Lock()
		f.pending = append(events, f.pending...)
		f.mu.Unlock()
		return fmt.Errorf("insert outages: %w", err)
	}
	// 5. Best-effort cleanup of old spool files that have already been
	//    converted to .done (extension chosen on success below).
	if err := f.cleanupOldDone(); err != nil {
		log.Printf("cleanup: %v", err)
	}
	return nil
}

func (f *Flusher) finalFlush(ctx context.Context) {
	// last-chance flush on shutdown
	rows := f.rollups.FlushAll()
	if len(rows) > 0 {
		if err := f.store.UpsertRollups(ctx, rows); err != nil {
			log.Printf("final rollup flush: %v", err)
		}
	}
	events := f.drainOutages()
	if len(events) > 0 {
		if err := f.store.InsertOutages(ctx, events); err != nil {
			log.Printf("final outage flush: %v", err)
		}
	}
}

// consumeFile reads a *.ready.ndjson file, bulk-inserts its samples, then
// renames it to *.done.ndjson with the flush timestamp embedded so retention
// can delete it later.
func (f *Flusher) consumeFile(ctx context.Context, path string) error {
	rows, err := readNDJSON(path)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return os.Remove(path)
	}
	if _, err := f.store.InsertSamples(ctx, rows); err != nil {
		return fmt.Errorf("insert samples: %w", err)
	}
	done := strings.TrimSuffix(path, ".ready.ndjson") + ".done.ndjson"
	if err := os.Rename(path, done); err != nil {
		log.Printf("rename done: %v", err)
		// still consumed in DB; leave file in place rather than retry insert
	}
	return nil
}

// cleanupOldDone removes *.done.ndjson files older than spoolHours.
func (f *Flusher) cleanupOldDone() error {
	dir := spoolDirFromReady(f.spool)
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-time.Duration(f.spoolHours) * time.Hour)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".done.ndjson") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return nil
}

// spoolDirFromReady inspects the first ready file to derive the spool dir.
// Returns "" if none are present.
func spoolDirFromReady(sp Spool) string {
	files, err := sp.Ready()
	if err != nil || len(files) == 0 {
		return ""
	}
	return filepath.Dir(files[0])
}

func readNDJSON(path string) ([]sample.Sample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64*1024)
	dec := json.NewDecoder(br)
	var out []sample.Sample
	for {
		var s sample.Sample
		if err := dec.Decode(&s); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, fmt.Errorf("decode %s: %w", path, err)
		}
		out = append(out, s)
	}
}
