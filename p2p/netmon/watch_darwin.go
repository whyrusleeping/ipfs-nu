// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause
// Adapted from tailscale.com v1.104.2 net/netmon/polling.go; see NOTICE.
// Nu's Darwin fallback compares interfaces/addresses and the BSD routing RIB.
package netmon

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"syscall"
	"time"
)

func snapshot() (string, string, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return "", "", err
	}
	var items []string
	for _, i := range ifs {
		items = append(items, fmt.Sprintf("%d:%s:%d:%d", i.Index, i.Name, i.Flags, i.MTU))
		addrs, err := i.Addrs()
		if err != nil {
			return "", "", err
		}
		for _, a := range addrs {
			items = append(items, fmt.Sprintf("%d:%s", i.Index, a))
		}
	}
	sort.Strings(items)
	rib, err := syscall.RouteRIB(syscall.NET_RT_DUMP, 0)
	return strings.Join(items, "\n"), string(rib), err
}
func openWatcher(interval time.Duration) (watcher, error) {
	oldIf, oldRoutes, err := snapshot()
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, emit func(Event)) {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				curIf, curRoutes, err := snapshot()
				if err != nil {
					emit(Event{Kind: Resync, Err: err})
					continue
				}
				var kind Kind
				if oldIf != curIf {
					kind |= Interface | Address
				}
				if oldRoutes != curRoutes {
					kind |= Route
				}
				oldIf, oldRoutes = curIf, curRoutes
				if kind != 0 {
					emit(Event{Kind: kind})
				}
			}
		}
	}, nil
}
