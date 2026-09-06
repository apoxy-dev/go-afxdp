//go:build linux

package main

import (
	"testing"

	"github.com/atoonk/go-afxdp"
)

// The startup probe must work under this module's dependency set too.
func TestXSKFeaturesProbe(t *testing.T) {
	f, err := afxdp.QueryXSKFeatures("lo")
	if err != nil {
		t.Fatalf("QueryXSKFeatures(lo): %v", err)
	}
	t.Logf("lo: %+v", f)
}
