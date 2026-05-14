// Package config loads ping-lens YAML configuration.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Source string `yaml:"source"`
	Mode   string `yaml:"mode"` // auto | single_isp | multi_isp

	ISP struct {
		Name           string `yaml:"name"`
		Plan           string `yaml:"plan"`
		ConnectionType string `yaml:"connection_type"`
		LocationLabel  string `yaml:"location_label"`
	} `yaml:"isp"`

	Database struct {
		URL    string `yaml:"url"`
		URLEnv string `yaml:"url_env"`
	} `yaml:"database"`

	Probe struct {
		IntervalSeconds int `yaml:"interval_seconds"`
		TimeoutSeconds  int `yaml:"timeout_seconds"`
		Privileged      bool `yaml:"privileged"` // raw ICMP vs unprivileged UDP echo
	} `yaml:"probe"`

	Flush struct {
		IntervalMinutes   int    `yaml:"interval_minutes"`
		SpoolDir          string `yaml:"spool_dir"`
		SpoolFlushSeconds int    `yaml:"spool_flush_seconds"`
	} `yaml:"flush"`

	Rollup struct {
		WindowMinutes int `yaml:"window_minutes"`
	} `yaml:"rollup"`

	Retention struct {
		RawDays     int `yaml:"raw_days"`
		RollupDays  int `yaml:"rollup_days"`
		SpoolHours  int `yaml:"spool_hours"`
	} `yaml:"retention"`

	Network struct {
		LocalGateway string `yaml:"local_gateway"`
		ISPFirstHop  string `yaml:"isp_first_hop"`
	} `yaml:"network"`

	Outage struct {
		// Number of consecutive failed ticks before a target is treated as
		// "failing" for outage classification. Single dropped packets below
		// this threshold still count toward loss% but do not open an event.
		MinConsecutiveFailures int `yaml:"min_consecutive_failures"`
	} `yaml:"outage"`

	HTTP struct {
		Listen string `yaml:"listen"`
	} `yaml:"http"`

	Targets map[string]Target `yaml:"targets"`

	Thresholds Thresholds `yaml:"thresholds"`
}

type Target struct {
	Address string `yaml:"address"`
	Group   string `yaml:"group"`
}

// Thresholds holds tunable grading cutoffs. Defaults applied if zero.
type Thresholds struct {
	LossPct  LabelBands `yaml:"loss_pct"`
	P95MS    LabelBands `yaml:"p95_ms"`
	P99MS    LabelBands `yaml:"p99_ms"`
}

// LabelBands describes cutoffs for excellent/good/fair/poor. Anything
// above Poor is "bad".
type LabelBands struct {
	Excellent float64 `yaml:"excellent"`
	Good      float64 `yaml:"good"`
	Fair      float64 `yaml:"fair"`
	Poor      float64 `yaml:"poor"`
}

// ProbeInterval returns the configured probe interval as a Duration.
func (c *Config) ProbeInterval() time.Duration {
	return time.Duration(c.Probe.IntervalSeconds) * time.Second
}

func (c *Config) ProbeTimeout() time.Duration {
	return time.Duration(c.Probe.TimeoutSeconds) * time.Second
}

func (c *Config) FlushInterval() time.Duration {
	return time.Duration(c.Flush.IntervalMinutes) * time.Minute
}

func (c *Config) SpoolFlushInterval() time.Duration {
	return time.Duration(c.Flush.SpoolFlushSeconds) * time.Second
}

func (c *Config) RollupWindow() time.Duration {
	return time.Duration(c.Rollup.WindowMinutes) * time.Minute
}

// Load reads the YAML config at path, applies defaults, and resolves
// the database URL from env if url_env is set.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(&c)
	if c.Database.URLEnv != "" && c.Database.URL == "" {
		c.Database.URL = os.Getenv(c.Database.URLEnv)
	}
	if c.Source == "" {
		return nil, fmt.Errorf("source is required")
	}
	if len(c.Targets) == 0 {
		return nil, fmt.Errorf("at least one target is required")
	}
	for name, t := range c.Targets {
		if t.Address == "" {
			return nil, fmt.Errorf("target %q missing address", name)
		}
	}
	return &c, nil
}

func applyDefaults(c *Config) {
	if c.Mode == "" {
		c.Mode = "auto"
	}
	if c.Probe.IntervalSeconds == 0 {
		c.Probe.IntervalSeconds = 5
	}
	if c.Probe.TimeoutSeconds == 0 {
		c.Probe.TimeoutSeconds = 3
	}
	if c.Flush.IntervalMinutes == 0 {
		c.Flush.IntervalMinutes = 30
	}
	if c.Flush.SpoolDir == "" {
		c.Flush.SpoolDir = "/var/lib/ping-lens/spool"
	}
	if c.Flush.SpoolFlushSeconds == 0 {
		c.Flush.SpoolFlushSeconds = 60
	}
	if c.Rollup.WindowMinutes == 0 {
		c.Rollup.WindowMinutes = 30
	}
	if c.Retention.RawDays == 0 {
		c.Retention.RawDays = 14
	}
	if c.Retention.RollupDays == 0 {
		c.Retention.RollupDays = 365
	}
	if c.Retention.SpoolHours == 0 {
		c.Retention.SpoolHours = 24
	}
	if c.Outage.MinConsecutiveFailures == 0 {
		c.Outage.MinConsecutiveFailures = 2
	}
	if c.HTTP.Listen == "" {
		c.HTTP.Listen = "127.0.0.1:8080"
	}
	if c.Thresholds.LossPct == (LabelBands{}) {
		c.Thresholds.LossPct = LabelBands{Excellent: 0.01, Good: 0.05, Fair: 0.25, Poor: 1.00}
	}
	if c.Thresholds.P95MS == (LabelBands{}) {
		c.Thresholds.P95MS = LabelBands{Excellent: 25, Good: 50, Fair: 100, Poor: 200}
	}
	if c.Thresholds.P99MS == (LabelBands{}) {
		c.Thresholds.P99MS = LabelBands{Excellent: 75, Good: 150, Fair: 300, Poor: 750}
	}
}
