// Package spool persists samples to NDJSON files between flush windows.
//
// The current file accumulates new samples. Rotate() closes the current file
// and returns its path so a flusher can consume + delete it. Files remain on
// disk until the flusher explicitly removes them.
package spool

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jasonwbarnett/ping-lens/internal/sample"
)

const (
	currentExt = ".ndjson"
	readyExt   = ".ready.ndjson"
)

type Spool struct {
	dir string

	mu      sync.Mutex
	f       *os.File
	w       *bufio.Writer
	path    string
	enc     *json.Encoder
	rotated bool
}

// Open initialises the spool directory and starts a fresh current file.
//
// Any pre-existing *.ndjson files left behind by a previous process (current
// files that were never rotated) are renamed to *.ready.ndjson so the next
// flush picks them up. Without this, restarts strand samples on disk.
func Open(dir string) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir spool: %w", err)
	}
	if err := promoteOrphans(dir); err != nil {
		return nil, fmt.Errorf("promote orphans: %w", err)
	}
	s := &Spool{dir: dir}
	if err := s.openCurrent(); err != nil {
		return nil, err
	}
	return s, nil
}

// promoteOrphans renames any *.ndjson files (excluding *.ready.ndjson and
// *.done.ndjson) to *.ready.ndjson so they are processed on the next flush.
func promoteOrphans(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, currentExt) {
			continue
		}
		if strings.HasSuffix(name, readyExt) || strings.HasSuffix(name, ".done.ndjson") {
			continue
		}
		old := filepath.Join(dir, name)
		ready := old[:len(old)-len(currentExt)] + readyExt
		if err := os.Rename(old, ready); err != nil {
			return fmt.Errorf("rename %s: %w", old, err)
		}
	}
	return nil
}

func (s *Spool) openCurrent() error {
	name := fmt.Sprintf("samples-%s%s", time.Now().UTC().Format("20060102T150405Z"), currentExt)
	p := filepath.Join(s.dir, name)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open spool: %w", err)
	}
	s.f = f
	s.w = bufio.NewWriterSize(f, 64*1024)
	s.enc = json.NewEncoder(s.w)
	s.path = p
	return nil
}

// Append writes one sample as a JSON line. Safe for concurrent use.
func (s *Spool) Append(samp sample.Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enc == nil {
		return fmt.Errorf("spool closed")
	}
	if err := s.enc.Encode(&samp); err != nil {
		return fmt.Errorf("encode sample: %w", err)
	}
	return nil
}

// Flush pushes any buffered bytes to the underlying file. Bounds the
// data-loss window on crash and lets `tail -F` see new samples without
// waiting for the buffer to fill or for Rotate.
func (s *Spool) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w == nil {
		return nil
	}
	return s.w.Flush()
}

// Rotate finalises the current file, renames it to *.ready.ndjson, and
// opens a fresh current file. Returns the path of the ready file (empty
// string if the current file held no bytes — in which case it is deleted).
func (s *Spool) Rotate() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return "", nil
	}
	if err := s.w.Flush(); err != nil {
		return "", err
	}
	if err := s.f.Sync(); err != nil {
		return "", err
	}
	old := s.path
	if err := s.f.Close(); err != nil {
		return "", err
	}
	s.f, s.w, s.enc, s.path = nil, nil, nil, ""

	info, statErr := os.Stat(old)
	if statErr == nil && info.Size() == 0 {
		_ = os.Remove(old)
		if err := s.openCurrent(); err != nil {
			return "", err
		}
		return "", nil
	}

	ready := old[:len(old)-len(currentExt)] + readyExt
	if err := os.Rename(old, ready); err != nil {
		return "", fmt.Errorf("rename ready: %w", err)
	}
	if err := s.openCurrent(); err != nil {
		return ready, err
	}
	return ready, nil
}

// Ready lists all *.ready.ndjson files in chronological order.
func (s *Spool) Ready() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) > len(readyExt) && name[len(name)-len(readyExt):] == readyExt {
			out = append(out, filepath.Join(s.dir, name))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Close flushes and closes the current file. Does not rotate.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	if err := s.w.Flush(); err != nil {
		return err
	}
	err := s.f.Close()
	s.f, s.w, s.enc = nil, nil, nil
	return err
}

// CurrentPath returns the path of the in-progress spool file.
func (s *Spool) CurrentPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}
