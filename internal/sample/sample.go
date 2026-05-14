// Package sample defines the core measurement record produced by probes
// and consumed by the buffer, spool, rollup, and flush pipelines.
package sample

import "time"

// Sample is a single probe observation.
type Sample struct {
	TS           time.Time `json:"ts"`
	Source       string    `json:"source"`
	ISPName      string    `json:"isp_name,omitempty"`
	Target       string    `json:"target"`
	TargetType   string    `json:"target_type"`
	TargetGroup  string    `json:"target_group,omitempty"`
	TargetIP     string    `json:"target_ip,omitempty"`
	DNSSuccess   *bool     `json:"dns_success,omitempty"`
	DNSLookupMS  *float64  `json:"dns_lookup_ms,omitempty"`
	PingSuccess  bool      `json:"ping_success"`
	LatencyMS    *float64  `json:"latency_ms,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// BoolPtr returns a pointer to b. Useful for the optional DNS fields.
func BoolPtr(b bool) *bool { return &b }

// F64Ptr returns a pointer to f.
func F64Ptr(f float64) *float64 { return &f }
