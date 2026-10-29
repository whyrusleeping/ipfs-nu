// Command xferbench measures Nu transfers between two machines without either disk in
// the way:
//
//	xferbench serve [-listen /ip4/0.0.0.0/udp/4181/quic-v1] [-files 1] [-size 4GiB] [-seed 1] [-link-file f]
//	xferbench fetch [-mode discard|fetcher] [-repeat 3] [-peer-requests 16] [-chunk 64] [-store dir] [-cpuprofile f] <link>
//
// serve generates synthetic files (deterministic pseudo-random bytes, so two servers with
// the same flags hold the same CIDs) into a store on tmpfs, starts the swarm a `nu daemon`
// runs and prints a share link. fetch takes a link (several servers' providers may be
// combined into one) and moves every blob, in one of two modes:
//
//   - discard requests every blob over /nu/bytes/1 from one shared queue, with
//     -peer-requests in flight per peer, and Bao-verifies each slice into a sink that
//     discards it: the network, server and verification ceiling.
//   - fetcher runs Nu's fetcher into a fresh store (on tmpfs by default; -store puts it
//     elsewhere, such as a disk): the full receive pipeline.
//
// scripts/xfer-bench.sh runs the 1:1 and 2:1 matrix on real machines; scripts/lan-bench.sh
// xfer1/xfer2 run fetch against `nu daemon` sources with real data instead.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, os.Args[2:])
	case "fetch":
		err = fetchCmd(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "xferbench:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: xferbench serve [flags] | xferbench fetch [flags] <nu:// link>")
	os.Exit(2)
}

// tmpRoot is where stores go by default: tmpfs when the machine has one.
func tmpRoot() string {
	if fi, err := os.Stat("/dev/shm"); err == nil && fi.IsDir() {
		return "/dev/shm"
	}
	return os.TempDir()
}

func tempStoreDir(root, pattern string) (string, error) {
	if root == "" {
		root = tmpRoot()
	}
	return os.MkdirTemp(root, pattern)
}

// parseSize parses a byte count with an optional KiB/MiB/GiB (or K/M/G) suffix.
func parseSize(s string) (int64, error) {
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}} {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSuffix(s, u.suffix), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return n * mult, nil
}

// cpuMeter reports the process's CPU use, in cores, since it was started.
type cpuMeter struct {
	wall time.Time
	cpu  time.Duration
}

func startCPU() cpuMeter { return cpuMeter{time.Now(), cpuTime()} }

func (m cpuMeter) cores() float64 {
	el := time.Since(m.wall)
	if el <= 0 {
		return 0
	}
	return float64(cpuTime()-m.cpu) / float64(el)
}

func removeAll(dir string) {
	if dir != "" && filepath.Dir(dir) != dir {
		os.RemoveAll(dir)
	}
}
