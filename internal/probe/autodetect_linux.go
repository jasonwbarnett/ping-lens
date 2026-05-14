//go:build linux

package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// FirstHopBeyondGateway returns the IP of hop 2 on the path to anchor —
// the first router past your local gateway, typically the ISP edge.
//
// Implementation mirrors what traceroute(8) does in its default mode:
// send a UDP packet with TTL=2 to a high port; the second-hop router
// drops the packet (TTL exceeded) and replies with ICMP Time Exceeded;
// the kernel queues that ICMP error on the originating socket's error
// queue (because we set IP_RECVERR); we read it back via recvmsg with
// MSG_ERRQUEUE and pull the offender IP out of the IP_RECVERR control
// message.
//
// Pure Go (golang.org/x/sys/unix), no external binary, no privileges.
func FirstHopBeyondGateway(ctx context.Context, anchor string, timeout time.Duration) (string, error) {
	if anchor == "" {
		anchor = "1.1.1.1"
	}
	dst, err := net.ResolveIPAddr("ip4", anchor)
	if err != nil {
		return "", fmt.Errorf("resolve anchor %q: %w", anchor, err)
	}
	ip4 := dst.IP.To4()
	if ip4 == nil {
		return "", fmt.Errorf("anchor %q is not IPv4", anchor)
	}

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	if err != nil {
		return "", fmt.Errorf("socket: %w", err)
	}
	defer unix.Close(fd)

	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVERR, 1); err != nil {
		return "", fmt.Errorf("set IP_RECVERR: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TTL, 2); err != nil {
		return "", fmt.Errorf("set IP_TTL: %w", err)
	}

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	tv := unix.NsecToTimeval(time.Until(deadline).Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return "", fmt.Errorf("set SO_RCVTIMEO: %w", err)
	}

	// Port 33434 is traceroute's traditional starting port — chosen to
	// almost certainly be unbound on the destination so a UDP probe
	// that does reach the end gets an ICMP Port Unreachable, not silently
	// accepted. Doesn't matter for hop-2 detection (the packet dies en
	// route) but kept for convention.
	sa := &unix.SockaddrInet4{Port: 33434}
	copy(sa.Addr[:], ip4)
	if err := unix.Sendto(fd, []byte("ping-lens-trace"), 0, sa); err != nil {
		return "", fmt.Errorf("sendto: %w", err)
	}

	buf := make([]byte, 1500)
	oob := make([]byte, 1500)
	_, oobn, _, _, err := unix.Recvmsg(fd, buf, oob, unix.MSG_ERRQUEUE)
	if err != nil {
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			return "", fmt.Errorf("no ICMP response within %v", timeout)
		}
		return "", fmt.Errorf("recvmsg(MSG_ERRQUEUE): %w", err)
	}

	cmsgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return "", fmt.Errorf("parse cmsg: %w", err)
	}
	for _, cm := range cmsgs {
		if cm.Header.Level != unix.IPPROTO_IP || cm.Header.Type != unix.IP_RECVERR {
			continue
		}
		// Layout: struct sock_extended_err (16 bytes) followed by the
		// offender's struct sockaddr_in. sockaddr_in: family(2) +
		// port(2) + addr(4) + zero(8). The IP we want sits at offset
		// 16 (skip extended_err) + 4 (skip family + port) = 20.
		const offenderIPOffset = 16 + 4
		if len(cm.Data) < offenderIPOffset+4 {
			continue
		}
		ip := net.IPv4(
			cm.Data[offenderIPOffset+0],
			cm.Data[offenderIPOffset+1],
			cm.Data[offenderIPOffset+2],
			cm.Data[offenderIPOffset+3],
		)
		if ip.IsUnspecified() {
			return "", errors.New("offender IP unspecified")
		}
		return ip.String(), nil
	}
	return "", errors.New("no IP_RECVERR control message in response")
}
