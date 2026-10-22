package host

import (
	"os"
	"testing"
)

// TestGSOUplink: the ethtool lookup answers for this machine's uplink (when it has
// one), and an explicit mode needs no lookup.
func TestGSOUplink(t *testing.T) {
	ifname, offload, err := uplinkOffload()
	t.Logf("uplink %q offloads UDP segmentation: %v (err %v)", ifname, offload, err)
	if err != nil && ifname != "" {
		t.Fatal(err)
	}
	t.Setenv("QUIC_GO_DISABLE_GSO", "")
	os.Unsetenv("QUIC_GO_DISABLE_GSO")
	if off, _, err := ConfigureGSO("off"); err != nil || !off || os.Getenv("QUIC_GO_DISABLE_GSO") != "true" {
		t.Fatalf("off: %v %v %q", off, err, os.Getenv("QUIC_GO_DISABLE_GSO"))
	}
	if _, _, err := ConfigureGSO("sometimes"); err == nil {
		t.Fatal("bad mode accepted")
	}
}
