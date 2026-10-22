package host

import (
	"fmt"
	"os"
)

// ConfigureGSO decides whether quic-go may use UDP generic segmentation offload (GSO) in
// this process. Call it before any transport exists: quic-go reads QUIC_GO_DISABLE_GSO
// when it wraps a socket. mode is "auto" (default), "on" or "off". In auto, GSO stays on
// only when the uplink interface offloads UDP segmentation. Otherwise the kernel segments
// in software, which measured slower than plain sends (alphac Realtek r8169: 357 MB/s
// with GSO, 682 without; Intel E810 with offload: 1025 with, 715 without; docs/validation.md
// "LAN benchmarks"). An explicit QUIC_GO_DISABLE_GSO in the environment wins over auto.
// It returns whether GSO is now disabled and why.
func ConfigureGSO(mode string) (disabled bool, why string, err error) {
	switch mode {
	case "", "auto":
	case "on":
		return false, "swarm.gso = on", nil
	case "off":
		return true, "swarm.gso = off", os.Setenv("QUIC_GO_DISABLE_GSO", "true")
	default:
		return false, "", fmt.Errorf("swarm.gso: expected auto, on or off, got %q", mode)
	}
	if v, ok := os.LookupEnv("QUIC_GO_DISABLE_GSO"); ok {
		return v == "true" || v == "1", "QUIC_GO_DISABLE_GSO is set", nil
	}
	ifname, offload, err := uplinkOffload()
	if err != nil || ifname == "" {
		return false, "uplink offload unknown; quic-go default", nil
	}
	if offload {
		return false, ifname + " offloads UDP segmentation", nil
	}
	return true, ifname + " does not offload UDP segmentation", os.Setenv("QUIC_GO_DISABLE_GSO", "true")
}
