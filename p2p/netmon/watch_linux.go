// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause
// Adapted from tailscale.com v1.104.2 net/netmon/netmon_linux.go; see NOTICE.
// Nu uses x/sys directly and emits bounded invalidations, not decoded routes.
package netmon

import (
	"context"
	"encoding/binary"
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

func openWatcher(_ time.Duration) (watcher, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	groups := uint32(unix.RTMGRP_LINK | unix.RTMGRP_IPV4_IFADDR | unix.RTMGRP_IPV6_IFADDR | unix.RTMGRP_IPV4_ROUTE | unix.RTMGRP_IPV6_ROUTE)
	if err = unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: groups}); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return func(ctx context.Context, emit func(Event)) {
		defer unix.Close(fd)
		b := make([]byte, 64<<10)
		polls := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for ctx.Err() == nil {
			n, err := unix.Poll(polls, 100)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				emit(Event{Kind: Resync, Err: err})
				return
			}
			if n == 0 {
				continue
			}
			n, _, flags, from, err := unix.Recvmsg(fd, b, nil, 0)
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			if errors.Is(err, unix.ENOBUFS) {
				emit(Event{Kind: Resync, Err: err})
				continue
			}
			if err != nil {
				emit(Event{Kind: Resync, Err: err})
				return
			}
			src, ok := from.(*unix.SockaddrNetlink)
			if !ok || src.Pid != 0 {
				continue
			}
			if flags&unix.MSG_TRUNC != 0 {
				emit(Event{Kind: Resync})
				continue
			}
			kind, err := parseNetlink(b[:n], 64<<10)
			if err != nil {
				kind |= Resync
			}
			if kind != 0 {
				emit(Event{Kind: kind, Err: err})
			}
		}
	}, nil
}
func parseNetlink(b []byte, maxBytes int) (Kind, error) {
	if len(b) > maxBytes {
		return Resync, errors.New("netmon: oversized netlink datagram")
	}
	var kind Kind
	for len(b) > 0 {
		if len(b) < 16 {
			return kind, errors.New("netmon: short netlink header")
		}
		n := int(binary.NativeEndian.Uint32(b[:4]))
		typ := binary.NativeEndian.Uint16(b[4:6])
		if n < 16 || n > len(b) {
			return kind, errors.New("netmon: invalid netlink length")
		}
		switch typ {
		case unix.RTM_NEWLINK, unix.RTM_DELLINK:
			kind |= Interface
		case unix.RTM_NEWADDR, unix.RTM_DELADDR:
			kind |= Address
		case unix.RTM_NEWROUTE, unix.RTM_DELROUTE:
			kind |= Route
		case unix.NLMSG_ERROR, unix.NLMSG_OVERRUN:
			kind |= Resync
		}
		aligned := (n + 3) &^ 3
		if aligned > len(b) {
			if n == len(b) {
				break
			}
			return kind, errors.New("netmon: invalid netlink padding")
		}
		b = b[aligned:]
	}
	return kind, nil
}
