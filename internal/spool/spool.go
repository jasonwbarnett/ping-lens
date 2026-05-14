// Package spool persists samples to NDJSON files between flush windows.
//
// The current file accumulates new samples. Rotate() closes the current file
// and returns its path so a flusher can consume + delete it. Files remain on
// disk until the flusher explicitly removes them.
package spool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	path    string
	enc     *json.Encoder
	rotated bool
}

// Open initialises the spool directory and starts a fresh current file.
func Open(dir string) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir spool: %w", err)
	}
	s := &Spool{dir: dir}
	if err := s.openCurrent(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Spool) openCurrent() error {
	name := fmt.Sprintf("samples-%s%s", time.Now().UTC().Format("20060102T150405Z"), currentExt)
	p := filepath.Join(s.dir, name)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open spool: %w", err)
	}
	s.f = f
	s.enc = json.NewEncoder(f)
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

// Rotate finalises the current file, renames it to *.ready.ndjson, and
// opens a fresh current file. Returns the path of the ready file (empty
// string if the current file held no bytes — in which case it is deleted).
func (s *Spool) Rotate() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return "", nil
	}
	if err := s.f.Sync(); err != nil {
		return "", err
	}
	old := s.path
	if err := s.f.Close(); err != nil {
		return "", err
	}
	s.f, s.enc, s.path = nil, nil, ""

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
	err := s.f.Close()
	s.f, s.enc = nil, nil
	return err
}

// CurrentPath returns the path of the in-progress spool file.
func (s *Spool) CurrentPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}
