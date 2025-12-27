package forward

import (
	"strings"
)

// BuildApplyNodeOrder returns the node apply order in reverse traffic direction,
// so downstream instances are applied before upstream (entry is always last).
func BuildApplyNodeOrder(ruleType string, cfg RuleConfig) []string {
	entryNodeID := strings.TrimSpace(cfg.EntryNodeID)
	out := make([]string, 0, 8)
	seen := map[string]struct{}{}

	add := func(nodeID string) {
		nodeID = strings.TrimSpace(nodeID)
		if nodeID == "" {
			return
		}
		if nodeID == entryNodeID {
			return // entry is always appended at the end
		}
		if _, ok := seen[nodeID]; ok {
			return
		}
		seen[nodeID] = struct{}{}
		out = append(out, nodeID)
	}

	switch strings.ToLower(strings.TrimSpace(ruleType)) {
	case "direct":
		// only entry
	case "relay_group":
		for _, ref := range sortedRelayRefs(cfg.Relays) {
			add(ref.relay.NodeID)
		}
	case "chain":
		hopRefs := sortedHopRefs(cfg.Hops)
		for i := len(hopRefs) - 1; i >= 0; i-- {
			hop := hopRefs[i].hop
			switch strings.ToLower(strings.TrimSpace(hop.Type)) {
			case "direct":
				add(hop.NodeID)
			case "relay_group":
				for _, ref := range sortedRelayRefs(hop.Relays) {
					add(ref.relay.NodeID)
				}
			}
		}
	}

	if entryNodeID != "" {
		out = append(out, entryNodeID)
	}
	return out
}
