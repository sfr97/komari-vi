package forward

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
)

func parsePortSpec(portSpec string) ([]int, error) {
	spec := strings.TrimSpace(portSpec)
	if spec == "" {
		return nil, fmt.Errorf("port spec is empty")
	}

	// 逗号分隔
	if strings.Contains(spec, ",") {
		parts := strings.Split(spec, ",")
		ports := make([]int, 0, len(parts))
		seen := make(map[int]struct{}, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			val, err := strconv.Atoi(p)
			if err != nil {
				return nil, fmt.Errorf("invalid port: %s", p)
			}
			if val <= 0 || val > 65535 {
				return nil, fmt.Errorf("port out of range: %d", val)
			}
			if _, ok := seen[val]; !ok {
				seen[val] = struct{}{}
				ports = append(ports, val)
			}
		}
		if len(ports) == 0 {
			return nil, fmt.Errorf("no valid ports found")
		}
		return ports, nil
	}

	// 区间
	if strings.Contains(spec, "-") {
		parts := strings.Split(spec, "-")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid range: %s", spec)
		}
		start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return nil, fmt.Errorf("invalid start port: %s", parts[0])
		}
		end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, fmt.Errorf("invalid end port: %s", parts[1])
		}
		if start <= 0 || end <= 0 || start > 65535 || end > 65535 || start > end {
			return nil, fmt.Errorf("port range out of bounds: %d-%d", start, end)
		}
		ports := make([]int, 0, end-start+1)
		for p := start; p <= end; p++ {
			ports = append(ports, p)
		}
		return ports, nil
	}

	// 单个端口
	val, err := strconv.Atoi(spec)
	if err != nil {
		return nil, fmt.Errorf("invalid port: %s", spec)
	}
	if val <= 0 || val > 65535 {
		return nil, fmt.Errorf("port out of range: %d", val)
	}
	return []int{val}, nil
}

func isPortAvailable(port int) bool {
	// 完整检查：同时覆盖 IPv4/IPv6（避免出现 v4 可用但 v6 已占用，或反之）。
	if !canListenTCP("tcp4", fmt.Sprintf("0.0.0.0:%d", port)) {
		return false
	}
	if !canListenTCP("tcp6", fmt.Sprintf("[::]:%d", port)) {
		return false
	}
	if !canListenUDP("udp4", fmt.Sprintf("0.0.0.0:%d", port)) {
		return false
	}
	if !canListenUDP("udp6", fmt.Sprintf("[::]:%d", port)) {
		return false
	}
	return true
}

func canIgnoreFamilyErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EPROTONOSUPPORT) {
		return true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "address family not supported") || strings.Contains(msg, "protocol not supported") {
		return true
	}
	return false
}

func canListenTCP(network string, addr string) bool {
	ln, err := net.Listen(network, addr)
	if err != nil {
		return canIgnoreFamilyErr(err)
	}
	_ = ln.Close()
	return true
}

func canListenUDP(network string, addr string) bool {
	pc, err := net.ListenPacket(network, addr)
	if err != nil {
		return canIgnoreFamilyErr(err)
	}
	_ = pc.Close()
	return true
}

func findAvailablePort(portSpec string, excluded []int) (int, error) {
	ports, err := parsePortSpec(portSpec)
	if err != nil {
		return 0, err
	}
	excludeSet := make(map[int]struct{}, len(excluded))
	for _, p := range excluded {
		excludeSet[p] = struct{}{}
	}

	for _, p := range ports {
		if _, skip := excludeSet[p]; skip {
			continue
		}
		if isPortAvailable(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no available port in spec: %s", portSpec)
}
