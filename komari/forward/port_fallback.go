package forward

import "strings"

// ResolvePortFallback resolves a listen port from (port spec, current port).
//
// Rules:
// - If current > 0, always return current (it is the authoritative runtime choice).
// - If current is not set, only return a port when the spec is a single fixed port.
//   For range/list specs, return 0 so callers won't mistakenly treat it as an allocated port.
func ResolvePortFallback(spec string, current int) int {
	if current > 0 {
		return current
	}
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0
	}
	ports, err := ParsePortSpec(spec)
	if err != nil {
		return 0
	}
	if len(ports) == 1 {
		return ports[0]
	}
	return 0
}

