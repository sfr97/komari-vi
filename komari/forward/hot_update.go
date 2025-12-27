package forward

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/models"
)

// ApplyHotUpdate applies a running rule change by comparing old/new Instance Plan and
// dispatching REALM_INSTANCE_APPLY ops in reverse traffic direction (entry last).
//
// Note: It may mutate newRule.ConfigJSON (e.g. filling *_current_port for newly added instances).
func ApplyHotUpdate(oldRule, newRule *models.ForwardRule) error {
	if oldRule == nil || newRule == nil {
		return fmt.Errorf("rule is nil")
	}
	if strings.TrimSpace(newRule.Type) == "" {
		return fmt.Errorf("missing rule type")
	}
	if strings.TrimSpace(newRule.ConfigJSON) == "" {
		return fmt.Errorf("missing config_json")
	}

	var oldCfg RuleConfig
	var newCfg RuleConfig
	_ = json.Unmarshal([]byte(oldRule.ConfigJSON), &oldCfg)
	if err := json.Unmarshal([]byte(newRule.ConfigJSON), &newCfg); err != nil {
		return err
	}
	if err := ValidateRuleConfigStrategies(newRule.Type, newCfg); err != nil {
		return err
	}

	// running rule: only fill missing current ports (do not verify/avoid ports already used by itself)
	changed, err := EnsureRuleCurrentPorts(newRule.Type, newRule.ID, &newCfg, EnsurePortsOptions{
		VerifyCurrentAvailability: false,
		Timeout:                   10 * time.Second,
	})
	if err != nil {
		return err
	}
	if changed {
		if b, err := json.Marshal(newCfg); err == nil {
			newRule.ConfigJSON = string(b)
		} else {
			return err
		}
	}

	oldPlan, err := BuildPlannedInstances(oldRule.Type, oldRule.ID, oldCfg, resolveNodeIP)
	if err != nil {
		return err
	}
	newPlan, err := BuildPlannedInstances(newRule.Type, newRule.ID, newCfg, resolveNodeIP)
	if err != nil {
		return err
	}

	opsByNode := map[string][]RealmInstanceApplyOp{}

	// Build delete ops for instances removed.
	oldInstances := map[string]PlannedInstance{}
	for _, ins := range oldPlan {
		oldInstances[ins.InstanceID] = ins
	}
	newInstances := map[string]PlannedInstance{}
	for _, ins := range newPlan {
		newInstances[ins.InstanceID] = ins
	}
	for id, ins := range oldInstances {
		if _, ok := newInstances[id]; ok {
			continue
		}
		opsByNode[ins.NodeID] = append(opsByNode[ins.NodeID],
			RealmInstanceApplyOp{Op: "stop", InstanceID: id},
			RealmInstanceApplyOp{Op: "delete", InstanceID: id},
		)
	}

	// Build upsert/start ops for new or changed instances.
	for _, ins := range newPlan {
		if oldIns, ok := oldInstances[ins.InstanceID]; ok && oldIns.ListenPort != ins.ListenPort {
			opsByNode[ins.NodeID] = append(opsByNode[ins.NodeID], RealmInstanceApplyOp{
				Op:         "stop",
				InstanceID: ins.InstanceID,
			})
		}
		opsByNode[ins.NodeID] = append(opsByNode[ins.NodeID],
			RealmInstanceApplyOp{Op: "upsert", InstanceID: ins.InstanceID, Config: ins.Config},
			RealmInstanceApplyOp{Op: "start", InstanceID: ins.InstanceID},
		)
	}

	// Determine apply order (entry last), but include nodes only present in opsByNode.
	order := BuildApplyNodeOrder(newRule.Type, newCfg)
	order = filterOrderByNodes(order, opsByNode)
	order = append(order, appendMissingNodes(order, opsByNode)...)
	order = moveEntryLast(order, strings.TrimSpace(newCfg.EntryNodeID))

	for _, nodeID := range order {
		ops := opsByNode[nodeID]
		if len(ops) == 0 {
			continue
		}

		ensureRes, ensureErr := SendTaskToNode(nodeID, TaskRealmApiEnsure, RealmApiEnsureRequest{
			RealmDownloadURL: "",
			ForceReinstall:   false,
		}, 60*time.Second)
		if ensureErr != nil {
			return ensureErr
		}
		var ensurePayload RealmApiEnsureResponse
		if err := json.Unmarshal(ensureRes.Payload, &ensurePayload); err != nil {
			return err
		}
		if !ensurePayload.Success {
			return fmt.Errorf("realm api ensure failed (node=%s): %s", nodeID, ensurePayload.Message)
		}

		applyRes, applyErr := SendTaskToNode(nodeID, TaskRealmInstanceApply, RealmInstanceApplyRequest{
			RuleID: newRule.ID,
			NodeID: nodeID,
			Ops:    ops,
		}, 30*time.Second)
		if applyErr != nil {
			return applyErr
		}
		var applyPayload RealmInstanceApplyResponse
		if err := json.Unmarshal(applyRes.Payload, &applyPayload); err != nil {
			return err
		}
		if !applyPayload.Success {
			return fmt.Errorf("apply failed (node=%s): %s", nodeID, applyPayload.Message)
		}
	}

	return nil
}

func resolveNodeIP(nodeID string) (string, error) {
	cli, err := clients.GetClientByUUID(nodeID)
	if err != nil {
		return "", err
	}
	if ip := strings.TrimSpace(cli.IPv4); ip != "" {
		return ip, nil
	}
	if ip := strings.TrimSpace(cli.IPv6); ip != "" {
		return ip, nil
	}
	return "", fmt.Errorf("node %s has no IP", nodeID)
}

func filterOrderByNodes(order []string, opsByNode map[string][]RealmInstanceApplyOp) []string {
	out := make([]string, 0, len(order))
	for _, nodeID := range order {
		if len(opsByNode[nodeID]) == 0 {
			continue
		}
		out = append(out, nodeID)
	}
	return out
}

func appendMissingNodes(order []string, opsByNode map[string][]RealmInstanceApplyOp) []string {
	seen := map[string]struct{}{}
	for _, id := range order {
		seen[id] = struct{}{}
	}
	missing := make([]string, 0)
	for nodeID, ops := range opsByNode {
		if len(ops) == 0 {
			continue
		}
		if _, ok := seen[nodeID]; ok {
			continue
		}
		missing = append(missing, nodeID)
	}
	return missing
}

func moveEntryLast(order []string, entryNodeID string) []string {
	if entryNodeID == "" {
		return order
	}
	out := make([]string, 0, len(order))
	found := false
	for _, nodeID := range order {
		if nodeID == entryNodeID {
			found = true
			continue
		}
		out = append(out, nodeID)
	}
	if found {
		out = append(out, entryNodeID)
	}
	return out
}
