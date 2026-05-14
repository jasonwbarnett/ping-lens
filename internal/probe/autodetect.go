// Auto-detection of local gateway and ISP first hop. Linux-first; on
// other OSes (or when /proc/net/route / unprivileged ICMP isn't
// available) detection returns an error and the caller skips injection.
package probe

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
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

// FirstHopBeyondGateway returns the IP of hop 2 on the path to anchor —
// the first router past your local gateway, typically the ISP edge.
//
// Implementation: send one ICMP Echo with TTL=2 over an unprivileged
// ICMP datagram socket (the same SOCK_DGRAM/IPPROTO_ICMP path used by
// pro-bing for our regular probes). Hop 2 replies with ICMP Time
// Exceeded; the kernel demuxes that to our socket via the embedded
// original packet's ICMP id, so no raw socket / CAP_NET_RAW required.
//
// Requires net.ipv4.ping_group_range to permit our gid — same precondition
// as ping-lens's normal probes when probe.privileged is false.
func FirstHopBeyondGateway(ctx context.Context, anchor string, timeout time.Duration) (string, error) {
	if anchor == "" {
		anchor = "1.1.1.1"
	}
	dst, err := net.ResolveIPAddr("ip4", anchor)
	if err != nil {
		return "", fmt.Errorf("resolve anchor %q: %w", anchor, err)
	}

	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		return "", fmt.Errorf("open unprivileged icmp socket: %w", err)
	}
	defer conn.Close()

	if err := conn.IPv4PacketConn().SetTTL(2); err != nil {
		return "", fmt.Errorf("set ttl: %w", err)
	}

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			// id is overwritten by the kernel for SOCK_DGRAM ICMP — set
			// to a stable value anyway for self-documentation.
			ID:   os.Getpid() & 0xffff,
			Seq:  1,
			Data: []byte("ping-lens-trace"),
		},
	}
	wb, err := msg.Marshal(nil)
	if err != nil {
		return "", fmt.Errorf("marshal echo: %w", err)
	}

	// Honour both the explicit timeout and the caller's ctx deadline,
	// whichever is sooner.
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return "", err
	}

	if _, err := conn.WriteTo(wb, &net.UDPAddr{IP: dst.IP}); err != nil {
		return "", fmt.Errorf("send echo: %w", err)
	}

	rb := make([]byte, 1500)
	n, peer, err := conn.ReadFrom(rb)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	rm, err := icmp.ParseMessage(1 /* iana.ProtocolICMP */, rb[:n])
	if err != nil {
		return "", fmt.Errorf("parse icmp: %w", err)
	}
	switch rm.Type {
	case ipv4.ICMPTypeTimeExceeded, ipv4.ICMPTypeEchoReply:
		// peer is *net.UDPAddr because we used "udp4" as the network.
		if udp, ok := peer.(*net.UDPAddr); ok {
			return udp.IP.String(), nil
		}
		return peer.String(), nil
	default:
		return "", fmt.Errorf("unexpected icmp type %v from %v", rm.Type, peer)
	}
}
