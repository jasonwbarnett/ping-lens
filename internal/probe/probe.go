// Package probe runs DNS lookups and ICMP echo tests against configured targets.
package probe

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	probing "github.com/prometheus-community/pro-bing"

	"github.com/jasonwbarnett/ping-lens/internal/config"
	"github.com/jasonwbarnett/ping-lens/internal/sample"
)

// Target is one resolved probe destination.
type Target struct {
	Name    string
	Address string // raw config address (may be hostname or IP)
	Group   string
	IsIP    bool
}

// Targets builds the runtime probe list from a config map. Synthetic
// "local_gateway" and "isp_first_hop" targets are auto-injected when
// the corresponding network.* setting is non-empty.
func Targets(cfg *config.Config) []Target {
	out := make([]Target, 0, len(cfg.Targets)+2)
	seenAddr := map[string]bool{}
	seenName := map[string]bool{}
	for name, t := range cfg.Targets {
		ip := net.ParseIP(t.Address)
		out = append(out, Target{
			Name:    name,
			Address: t.Address,
			Group:   t.Group,
			IsIP:    ip != nil,
		})
		seenAddr[t.Address] = true
		seenName[name] = true
	}
	if addr := resolveNetworkTarget("local_gateway", cfg.Network.LocalGateway, DefaultGateway); addr != "" {
		out = appendNetworkTarget(out, "local_gateway", addr, seenName, seenAddr)
	}
	if addr := resolveNetworkTarget("isp_first_hop", cfg.Network.ISPFirstHop, func() (string, error) {
		return FirstHopBeyondGateway(context.Background(), "1.1.1.1", 5*time.Second)
	}); addr != "" {
		out = appendNetworkTarget(out, "isp_first_hop", addr, seenName, seenAddr)
	}
	return out
}

// resolveNetworkTarget interprets the user-facing setting:
//   - "" / unset       -> skip injection
//   - "auto"           -> run autoFn; on error, log and skip
//   - any other string -> treat as a literal address
func resolveNetworkTarget(label, setting string, autoFn func() (string, error)) string {
	switch setting {
	case "":
		return ""
	case "auto":
		addr, err := autoFn()
		if err != nil {
			log.Printf("auto-detect %s: %v (skipping)", label, err)
			return ""
		}
		log.Printf("auto-detected %s = %s", label, addr)
		return addr
	default:
		return setting
	}
}

func appendNetworkTarget(out []Target, name, addr string, seenName, seenAddr map[string]bool) []Target {
	if seenName[name] || seenAddr[addr] {
		return out // user already configured this; don't double-probe
	}
	out = append(out, Target{
		Name:    name,
		Address: addr,
		Group:   "network",
		IsIP:    net.ParseIP(addr) != nil,
	})
	seenName[name] = true
	seenAddr[addr] = true
	return out
}

// Prober executes one probe per target per tick and emits samples.
type Prober struct {
	cfg     *config.Config
	targets []Target
	out     chan<- sample.Sample
	resolver *net.Resolver
}

func New(cfg *config.Config, targets []Target, out chan<- sample.Sample) *Prober {
	return &Prober{
		cfg:     cfg,
		targets: targets,
		out:     out,
		resolver: &net.Resolver{PreferGo: true},
	}
}

// Run drives the probe loop until ctx is cancelled.
func (p *Prober) Run(ctx context.Context) {
	interval := p.cfg.ProbeInterval()
	t := time.NewTicker(interval)
	defer t.Stop()

	// fire immediately on startup
	p.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx)
		}
	}
}

func (p *Prober) tick(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range p.targets {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			s := p.runOne(ctx, t)
			select {
			case p.out <- s:
			case <-ctx.Done():
			}
		}(t)
	}
	wg.Wait()
}

func (p *Prober) runOne(ctx context.Context, t Target) sample.Sample {
	now := time.Now().UTC()
	s := sample.Sample{
		TS:          now,
		Source:      p.cfg.Source,
		ISPName:     p.cfg.ISP.Name,
		Target:      t.Name,
		TargetType:  ifThen(t.IsIP, "ip", "hostname"),
		TargetGroup: t.Group,
	}

	addr := t.Address

	// DNS step (only for hostnames)
	if !t.IsIP {
		dnsCtx, cancel := context.WithTimeout(ctx, p.cfg.ProbeTimeout())
		dnsStart := time.Now()
		ips, err := p.resolver.LookupHost(dnsCtx, t.Address)
		cancel()
		ms := float64(time.Since(dnsStart).Microseconds()) / 1000.0
		s.DNSLookupMS = sample.F64Ptr(ms)
		if err != nil || len(ips) == 0 {
			s.DNSSuccess = sample.BoolPtr(false)
			s.PingSuccess = false
			s.Error = "dns: " + errString(err)
			return s
		}
		s.DNSSuccess = sample.BoolPtr(true)
		addr = pickIP(ips)
	}

	s.TargetIP = addr

	// ICMP step
	latency, err := pingOnce(addr, p.cfg.ProbeTimeout(), p.cfg.Probe.Privileged)
	if err != nil {
		s.PingSuccess = false
		s.Error = "ping: " + err.Error()
		return s
	}
	if latency < 0 {
		s.PingSuccess = false
		s.Error = "ping: no reply"
		return s
	}
	s.PingSuccess = true
	s.LatencyMS = sample.F64Ptr(float64(latency.Microseconds()) / 1000.0)
	return s
}

// pingOnce sends a single ICMP echo and returns the RTT. Returns -1 RTT
// with nil error if the packet timed out (caller decides classification).
func pingOnce(addr string, timeout time.Duration, privileged bool) (time.Duration, error) {
	pinger, err := probing.NewPinger(addr)
	if err != nil {
		return 0, err
	}
	pinger.SetPrivileged(privileged)
	pinger.Count = 1
	pinger.Timeout = timeout
	pinger.RecordRtts = false

	var rtt time.Duration = -1
	pinger.OnRecv = func(pkt *probing.Packet) {
		rtt = pkt.Rtt
	}
	if err := pinger.Run(); err != nil {
		return 0, err
	}
	stats := pinger.Statistics()
	if stats.PacketsRecv == 0 {
		return -1, nil
	}
	return rtt, nil
}

func pickIP(ips []string) string {
	// Prefer IPv4 for ICMP simplicity.
	for _, ip := range ips {
		if a, err := netip.ParseAddr(ip); err == nil && a.Is4() {
			return ip
		}
	}
	return ips[0]
}

func ifThen[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// Compact for readability in logs/spool.
	msg = strings.ReplaceAll(msg, "\n", " ")
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

// String renders a target for logs.
func (t Target) String() string {
	return fmt.Sprintf("%s(%s)", t.Name, t.Address)
}
