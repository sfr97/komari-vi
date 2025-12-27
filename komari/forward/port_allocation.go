package forward

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	dbforward "github.com/komari-monitor/komari/database/forward"
)

// CollectReservedPortsForNode returns ports reserved by other forward rules on the same node.
// excludeRuleID: the current rule id, so its own ports won't block itself.
func CollectReservedPortsForNode(nodeID string, excludeRuleID uint) []int {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return nil
	}
	rules, err := dbforward.ListForwardRules()
	if err != nil {
		return nil
	}
	seen := map[int]struct{}{}
	add := func(p int) {
		if p <= 0 {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
	}
	for _, rule := range rules {
		if excludeRuleID > 0 && rule.ID == excludeRuleID {
			continue
		}
		if rule.ConfigJSON == "" {
			continue
		}
		var cfg RuleConfig
		if err := json.Unmarshal([]byte(rule.ConfigJSON), &cfg); err != nil {
			continue
		}

		for _, b := range collectConfigPortsForNode(rule.Type, rule.ID, &cfg, nodeID) {
			add(b)
		}
	}
	ports := make([]int, 0, len(seen))
	for p := range seen {
		ports = append(ports, p)
	}
	return ports
}

func collectConfigPortsForNode(ruleType string, ruleID uint, cfg *RuleConfig, nodeID string) []int {
	if cfg == nil {
		return nil
	}
	out := []int{}
	add := func(spec string, current int) {
		p := ResolvePortFallback(spec, current)
		if p > 0 {
			out = append(out, p)
		}
	}
	if cfg.EntryNodeID == nodeID {
		add(cfg.EntryPort, cfg.EntryCurrentPort)
	}
	switch strings.ToLower(strings.TrimSpace(ruleType)) {
	case "relay_group":
		for _, r := range cfg.Relays {
			if r.NodeID == nodeID {
				add(r.Port, r.CurrentPort)
			}
		}
	case "chain":
		for _, hop := range cfg.Hops {
			if strings.ToLower(strings.TrimSpace(hop.Type)) == "direct" && hop.NodeID == nodeID {
				add(hop.Port, hop.CurrentPort)
			}
			if strings.ToLower(strings.TrimSpace(hop.Type)) == "relay_group" {
				for _, r := range hop.Relays {
					if r.NodeID == nodeID {
						add(r.Port, r.CurrentPort)
					}
				}
			}
		}
	}
	return out
}

func MergeExcludedPorts(a []int, b []int) []int {
	seen := map[int]struct{}{}
	out := make([]int, 0, len(a)+len(b))
	for _, v := range append(append([]int{}, a...), b...) {
		if v <= 0 {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

type EnsurePortsOptions struct {
	// If true, and current_port is set, verify the port is available on the node (CHECK_PORT with fixed spec).
	VerifyCurrentAvailability bool
	Timeout                   time.Duration
}

// EnsureRuleCurrentPorts fills *_current_port in cfg by asking the agent via CHECK_PORT.
// It mutates cfg in-place.
func EnsureRuleCurrentPorts(ruleType string, ruleID uint, cfg *RuleConfig, opts EnsurePortsOptions) (bool, error) {
	if cfg == nil {
		return false, fmt.Errorf("config is nil")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	bindings, err := ListInstanceBindings(ruleType, ruleID, cfg)
	if err != nil {
		return false, err
	}

	reservedByNode := map[string][]int{}
	usedByNode := map[string]map[int]struct{}{}

	ensureUsed := func(nodeID string) map[int]struct{} {
		if usedByNode[nodeID] == nil {
			usedByNode[nodeID] = map[int]struct{}{}
		}
		return usedByNode[nodeID]
	}
	getReserved := func(nodeID string) []int {
		if v, ok := reservedByNode[nodeID]; ok {
			return v
		}
		v := CollectReservedPortsForNode(nodeID, ruleID)
		reservedByNode[nodeID] = v
		return v
	}

	changed := false
	for _, b := range bindings {
		nodeID := strings.TrimSpace(b.NodeID)
		if nodeID == "" {
			return changed, fmt.Errorf("missing node_id for instance %s", b.InstanceID)
		}
		if b.Current == nil {
			return changed, fmt.Errorf("missing current port binding for instance %s", b.InstanceID)
		}

		portSpec := strings.TrimSpace(b.PortSpec)
		if portSpec == "" {
			return changed, fmt.Errorf("missing port_spec for instance %s", b.InstanceID)
		}

		used := ensureUsed(nodeID)
		reserved := getReserved(nodeID)

		current := *b.Current
		needPick := false
		if current <= 0 || !PortInSpec(portSpec, current) {
			needPick = true
		}
		if !needPick {
			if _, dup := used[current]; dup {
				needPick = true
			}
		}

		if !needPick && opts.VerifyCurrentAvailability {
			// Verify with a fixed port spec. Exclude other rules and already-picked ports on this node.
			excluded := MergeExcludedPorts(reserved, portsFromSet(used))
			resp, err := SendTaskToNode(nodeID, TaskCheckPort, CheckPortRequest{
				PortSpec:      fmt.Sprintf("%d", current),
				ExcludedPorts: excluded,
			}, opts.Timeout)
			if err != nil {
				return changed, err
			}
			var payload CheckPortResponse
			if err := json.Unmarshal(resp.Payload, &payload); err != nil {
				return changed, fmt.Errorf("decode CHECK_PORT response failed: %w", err)
			}
			if !(payload.Success && payload.AvailablePort != nil && *payload.AvailablePort == current) {
				needPick = true
			}
		}

		if needPick {
			excluded := MergeExcludedPorts(reserved, portsFromSet(used))
			resp, err := SendTaskToNode(nodeID, TaskCheckPort, CheckPortRequest{
				PortSpec:      portSpec,
				ExcludedPorts: excluded,
			}, opts.Timeout)
			if err != nil {
				return changed, err
			}
			var payload CheckPortResponse
			if err := json.Unmarshal(resp.Payload, &payload); err != nil {
				return changed, fmt.Errorf("decode CHECK_PORT response failed: %w", err)
			}
			if !payload.Success || payload.AvailablePort == nil || *payload.AvailablePort <= 0 {
				if payload.Message != "" {
					return changed, fmt.Errorf("pick port failed for node %s (%s): %s", nodeID, b.InstanceID, payload.Message)
				}
				return changed, fmt.Errorf("pick port failed for node %s (%s)", nodeID, b.InstanceID)
			}
			*b.Current = *payload.AvailablePort
			current = *payload.AvailablePort
			changed = true
		}

		used[current] = struct{}{}
	}
	return changed, nil
}

func portsFromSet(set map[int]struct{}) []int {
	if len(set) == 0 {
		return nil
	}
	out := make([]int, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	return out
}
