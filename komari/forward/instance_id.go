package forward

import (
	"fmt"
	"strconv"
	"strings"
)

func InstanceIDEntry(ruleID uint, entryNodeID string) string {
	return fmt.Sprintf("komari-r%d-n%s-entry", ruleID, entryNodeID)
}

func InstanceIDRelay(ruleID uint, relayNodeID string, idx int) string {
	return fmt.Sprintf("komari-r%d-n%s-relay-%d", ruleID, relayNodeID, idx)
}

func InstanceIDHop(ruleID uint, hopNodeID string, hopIndex int) string {
	return fmt.Sprintf("komari-r%d-n%s-hop%d", ruleID, hopNodeID, hopIndex)
}

func InstanceIDHopRelay(ruleID uint, relayNodeID string, hopIndex int, relayIndex int) string {
	return fmt.Sprintf("komari-r%d-n%s-hop%d-relay%d", ruleID, relayNodeID, hopIndex, relayIndex)
}

func ParseNodeIDFromInstanceID(instanceID string) (string, bool) {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return "", false
	}
	if !strings.HasPrefix(instanceID, "komari-r") {
		return "", false
	}
	nIdx := strings.Index(instanceID, "-n")
	if nIdx <= 0 {
		return "", false
	}
	// sanity: ensure rule id is numeric
	rulePart := strings.TrimPrefix(instanceID[:nIdx], "komari-r")
	if rulePart == "" {
		return "", false
	}
	if _, err := strconv.ParseUint(rulePart, 10, 64); err != nil {
		return "", false
	}

	nodeStart := nIdx + 2
	nodeEnd := -1
	if i := strings.LastIndex(instanceID, "-hop"); i > nodeStart {
		nodeEnd = i
	} else if i := strings.LastIndex(instanceID, "-relay-"); i > nodeStart {
		nodeEnd = i
	} else if i := strings.LastIndex(instanceID, "-entry"); i > nodeStart {
		nodeEnd = i
	}
	if nodeEnd <= nodeStart {
		return "", false
	}
	nodeID := instanceID[nodeStart:nodeEnd]
	if strings.TrimSpace(nodeID) == "" {
		return "", false
	}
	return nodeID, true
}
