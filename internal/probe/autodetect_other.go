//go:build !linux

package probe

import (
	"context"
	"errors"
	"runtime"
	"time"
)

// FirstHopBeyondGateway is Linux-only — the IP_RECVERR + UDP-probe
// trick relies on Linux's socket error queue. On other OSes detection
// is unavailable; supply an explicit network.isp_first_hop instead.
func FirstHopBeyondGateway(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", errors.New("isp_first_hop auto detection is not supported on " + runtime.GOOS + "; set an explicit address")
}
