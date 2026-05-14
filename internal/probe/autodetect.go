// Auto-detection of local gateway and ISP first hop. Linux-only; on other
// OSes detection returns an error and the caller skips injection.
package probe

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultGateway returns the IPv4 default gateway from /proc/net/route.
func DefaultGateway() (string, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		// Iface Destination Gateway Flags ...
		if fields[1] != "00000000" {
			continue
		}
		b, err := hex.DecodeString(fields[2])
		if err != nil || len(b) != 4 {
			continue
		}
		// Stored little-endian.
		return fmt.Sprintf("%d.%d.%d.%d", b[3], b[2], b[1], b[0]), nil
	}
	return "", errors.New("no default route in /proc/net/route")
}

// FirstHopBeyondGateway runs a brief traceroute to anchor and returns hop
// 2's IP — the first router past your local gateway, typically the ISP's
// edge. Requires the `traceroute` binary in PATH; works without root via
// the default UDP probe mode on Linux.
func FirstHopBeyondGateway(ctx context.Context, anchor string, timeout time.Duration) (string, error) {
	if _, err := exec.LookPath("traceroute"); err != nil {
		return "", fmt.Errorf("traceroute binary not in PATH")
	}
	if anchor == "" {
		anchor = "1.1.1.1"
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// -n no DNS, -q 1 one probe per hop, -m 3 max 3 hops, -w 2 wait 2s.
	cmd := exec.CommandContext(cctx, "traceroute", "-n", "-q", "1", "-m", "3", "-w", "2", anchor)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("traceroute: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "2" {
			continue
		}
		ip := fields[1]
		if ip == "*" || net.ParseIP(ip) == nil {
			return "", errors.New("hop 2 did not respond")
		}
		return ip, nil
	}
	return "", errors.New("no hop 2 in traceroute output")
}
