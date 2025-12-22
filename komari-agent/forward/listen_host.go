package forward

import (
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
)

var (
	listenHostOnce sync.Once
	listenHost     = "0.0.0.0"
)

func chooseListenHost() string {
	listenHostOnce.Do(func() {
		hasV4 := hasGlobalIP(true)
		hasV6 := hasGlobalIP(false)

		// IPv6-only：优先监听 ::，否则无法承载 IPv6 入站
		if hasV6 && !hasV4 {
			listenHost = "::"
			return
		}
		// 双栈：若内核允许 v6 socket 接收 v4（bindv6only=0），使用 :: 以同时覆盖 v4/v6
		if hasV6 && bindV6Only() == 0 {
			listenHost = "::"
			return
		}
		listenHost = "0.0.0.0"
	})
	return listenHost
}

func bindV6Only() int {
	b, err := os.ReadFile("/proc/sys/net/ipv6/bindv6only")
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(b))
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return v
}

func hasGlobalIP(v4 bool) bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			if v4 {
				ip4 := ip.To4()
				if ip4 == nil {
					continue
				}
				if ip4.IsLoopback() || ip4.IsUnspecified() || ip4.IsLinkLocalUnicast() {
					continue
				}
				return true
			}
			if ip.To4() != nil {
				continue
			}
			if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
				continue
			}
			return true
		}
	}
	return false
}
