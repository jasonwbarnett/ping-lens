package server

import (
	"testing"

	"github.com/jasonwbarnett/ping-lens/internal/config"
)

// TestTemplatesParse is a compile-time sanity check that every embedded
// template parses successfully. It does not exercise rendering.
func TestTemplatesParse(t *testing.T) {
	cfg := &config.Config{Source: "test"}
	if _, err := New(cfg, nil); err != nil {
		t.Fatalf("templates failed to parse: %v", err)
	}
}
