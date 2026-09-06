//go:build linux

package afxdp

import (
	"errors"
	"syscall"
	"testing"
)

// The loopback device always exists and can never offload anything, so the
// probe must come back clean and empty for it on any kernel with the netdev
// family (6.3+); older kernels have no family and skip.
func TestQueryXSKFeaturesLoopback(t *testing.T) {
	f, err := QueryXSKFeatures("lo")
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.EOPNOTSUPP) {
			t.Skipf("no netdev netlink family: %v", err)
		}
		t.Fatalf("QueryXSKFeatures(lo): %v", err)
	}
	if f.ZeroCopy() || f.TxChecksum || f.TxTimestamp {
		t.Errorf("loopback claims offloads: %+v", f)
	}
	t.Logf("lo: %+v", f)
}
