//go:build linux

package host

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// uplinkInterface names the interface the default route uses: a connected UDP socket
// picks the route without sending anything.
func uplinkInterface() (string, error) {
	for _, dst := range []string{"192.0.2.1:9", "[2001:db8::1]:9"} {
		c, err := net.Dial("udp", dst)
		if err != nil {
			continue
		}
		local := c.LocalAddr().(*net.UDPAddr).IP
		c.Close()
		ifaces, err := net.Interfaces()
		if err != nil {
			return "", err
		}
		for _, ifc := range ifaces {
			addrs, _ := ifc.Addrs()
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok && n.IP.Equal(local) {
					return ifc.Name, nil
				}
			}
		}
	}
	return "", fmt.Errorf("no default route")
}

// udpSegmentationOffload reports the interface's active ethtool feature
// "tx-udp-segmentation" (UDP GSO done by the NIC).
func udpSegmentationOffload(ifname string) (bool, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	ethtool := func(data []byte) error {
		var ifr [40]byte // struct ifreq: name[16], then a pointer to the ethtool command
		copy(ifr[:unix.IFNAMSIZ-1], ifname)
		*(*uintptr)(unsafe.Pointer(&ifr[16])) = uintptr(unsafe.Pointer(&data[0]))
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCETHTOOL, uintptr(unsafe.Pointer(&ifr[0])))
		runtime.KeepAlive(data)
		if errno != 0 {
			return errno
		}
		return nil
	}
	const ethSSFeatures = 4 // ETH_SS_FEATURES
	le := binary.LittleEndian
	// Number of feature names (struct ethtool_sset_info with one data word).
	info := make([]byte, 20)
	le.PutUint32(info[0:], unix.ETHTOOL_GSSET_INFO)
	le.PutUint64(info[8:], 1<<ethSSFeatures)
	if err := ethtool(info); err != nil {
		return false, err
	}
	n := int(le.Uint32(info[16:]))
	if n <= 0 || n > 4096 {
		return false, fmt.Errorf("ethtool: %d feature names", n)
	}
	// The names (struct ethtool_gstrings, 32 bytes each) give the feature's bit.
	names := make([]byte, 12+32*n)
	le.PutUint32(names[0:], unix.ETHTOOL_GSTRINGS)
	le.PutUint32(names[4:], ethSSFeatures)
	le.PutUint32(names[8:], uint32(n))
	if err := ethtool(names); err != nil {
		return false, err
	}
	bit := -1
	for i := 0; i < n; i++ {
		name := names[12+32*i : 12+32*(i+1)]
		if string(bytes.TrimRight(name, "\x00")) == "tx-udp-segmentation" {
			bit = i
			break
		}
	}
	if bit < 0 {
		return false, nil // the kernel does not know the feature
	}
	// Active features (struct ethtool_gfeatures: blocks of available, requested,
	// active, never_changed).
	blocks := (n + 31) / 32
	feats := make([]byte, 8+16*blocks)
	le.PutUint32(feats[0:], unix.ETHTOOL_GFEATURES)
	le.PutUint32(feats[4:], uint32(blocks))
	if err := ethtool(feats); err != nil {
		return false, err
	}
	active := le.Uint32(feats[8+16*(bit/32)+8:])
	return active&(1<<(bit%32)) != 0, nil
}

func uplinkOffload() (string, bool, error) {
	ifname, err := uplinkInterface()
	if err != nil {
		return "", false, err
	}
	ok, err := udpSegmentationOffload(ifname)
	return ifname, ok, err
}
