package forward

import "testing"

func TestValidateRuleConfigStrategies(t *testing.T) {
	t.Run("direct_ok", func(t *testing.T) {
		err := ValidateRuleConfigStrategies("direct", RuleConfig{Type: "direct"})
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("relay_group_missing_strategy", func(t *testing.T) {
		err := ValidateRuleConfigStrategies("relay_group", RuleConfig{Type: "relay_group"})
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
	})

	t.Run("relay_group_priority_rejected", func(t *testing.T) {
		err := ValidateRuleConfigStrategies("relay_group", RuleConfig{Type: "relay_group", Strategy: "priority"})
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
	})

	t.Run("relay_group_random_rejected", func(t *testing.T) {
		err := ValidateRuleConfigStrategies("relay_group", RuleConfig{Type: "relay_group", Strategy: "random"})
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
	})

	t.Run("relay_group_allowed", func(t *testing.T) {
		for _, s := range []string{"roundrobin", "iphash", "failover"} {
			err := ValidateRuleConfigStrategies("relay_group", RuleConfig{Type: "relay_group", Strategy: s})
			if err != nil {
				t.Fatalf("expected nil for %q, got %v", s, err)
			}
		}
	})

	t.Run("chain_hop_relay_group_priority_rejected", func(t *testing.T) {
		err := ValidateRuleConfigStrategies("chain", RuleConfig{
			Type: "chain",
			Hops: []ChainHop{
				{Type: "direct", NodeID: "n1"},
				{Type: "relay_group", Strategy: "priority"},
			},
		})
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
	})
}
