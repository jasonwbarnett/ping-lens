// Auto-detection of local gateway and ISP first hop. Both are
// Linux-first: DefaultGateway reads /proc/net/route, FirstHopBeyondGateway
// uses IP_RECVERR-style traceroute (see autodetect_linux.go). On
// non-Linux, detection returns an error and the caller skips injection.
package probe

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// DefaultGateway returns the IPv4 default gateway from /proc/net/route.
// Returns an error on non-Linux systems where /proc/net/route does not
// exist.
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
