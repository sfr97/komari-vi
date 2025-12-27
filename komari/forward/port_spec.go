package forward

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func ParsePortSpec(spec string) ([]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("port spec is empty")
	}

	// list: "80,81,82"
	if strings.Contains(spec, ",") {
		parts := strings.Split(spec, ",")
		out := make([]int, 0, len(parts))
		seen := map[int]struct{}{}
		for _, part := range parts {
			p, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || p <= 0 || p > 65535 {
				return nil, fmt.Errorf("invalid port in spec: %q", part)
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
		sort.Ints(out)
		return out, nil
	}

	// range: "8000-9000"
	if strings.Contains(spec, "-") {
		parts := strings.SplitN(spec, "-", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid port range spec: %q", spec)
		}
		start, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		end, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil || start <= 0 || end <= 0 || start > 65535 || end > 65535 || start > end {
			return nil, fmt.Errorf("invalid port range spec: %q", spec)
		}
		// hard cap to avoid gigantic allocations; realm/agent uses similar bounded specs.
		if end-start > 10000 {
			return nil, fmt.Errorf("port range too large: %q", spec)
		}
		out := make([]int, 0, end-start+1)
		for p := start; p <= end; p++ {
			out = append(out, p)
		}
		return out, nil
	}

	// fixed: "8080"
	p, err := strconv.Atoi(spec)
	if err != nil || p <= 0 || p > 65535 {
		return nil, fmt.Errorf("invalid port spec: %q", spec)
	}
	return []int{p}, nil
}

func PortInSpec(spec string, port int) bool {
	if port <= 0 {
		return false
	}
	ports, err := ParsePortSpec(spec)
	if err != nil {
		return false
	}
	i := sort.SearchInts(ports, port)
	return i >= 0 && i < len(ports) && ports[i] == port
}
