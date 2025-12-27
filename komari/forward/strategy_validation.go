package forward

import (
	"fmt"
	"strings"
)

var allowedStrategies = map[string]struct{}{
	"roundrobin": {},
	"iphash":     {},
	"failover":   {},
}

func ValidateRuleConfigStrategies(ruleType string, cfg RuleConfig) error {
	ruleType = strings.ToLower(strings.TrimSpace(firstNonEmpty(ruleType, cfg.Type)))
	if ruleType == "" {
		return fmt.Errorf("missing rule type")
	}

	validateStrategy := func(path string, strategy string) error {
		strategy = strings.ToLower(strings.TrimSpace(strategy))
		if strategy == "" {
			return fmt.Errorf("missing strategy at %s", path)
		}
		if strategy == "priority" {
			return fmt.Errorf("strategy %q has been removed; use %q instead", "priority", "failover")
		}
		if _, ok := allowedStrategies[strategy]; !ok {
			return fmt.Errorf("invalid strategy %q at %s (allowed: roundrobin, iphash, failover)", strategy, path)
		}
		return nil
	}

	switch ruleType {
	case "direct":
		return nil
	case "relay_group":
		return validateStrategy("strategy", cfg.Strategy)
	case "chain":
		for i, hop := range cfg.Hops {
			if strings.ToLower(strings.TrimSpace(hop.Type)) != "relay_group" {
				continue
			}
			if err := validateStrategy(fmt.Sprintf("hops[%d].strategy", i), hop.Strategy); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported rule type %q", ruleType)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
