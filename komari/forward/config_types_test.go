package forward

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRuleConfigNetworkPreservesExplicitZero(t *testing.T) {
	zero := uint64(0)
	cfg := RuleConfig{
		Type:     "relay_group",
		Strategy: "failover",
		Network: &NetworkConfig{
			FailoverRetryWindowMs: &zero,
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if !strings.Contains(string(b), `"failover_retry_window_ms":0`) {
		t.Fatalf("expected explicit 0 to be preserved, got: %s", string(b))
	}

	var roundTrip RuleConfig
	if err := json.Unmarshal(b, &roundTrip); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	b2, err := json.Marshal(roundTrip)
	if err != nil {
		t.Fatalf("marshal roundtrip failed: %v", err)
	}
	if !strings.Contains(string(b2), `"failover_retry_window_ms":0`) {
		t.Fatalf("expected explicit 0 to be preserved after roundtrip, got: %s", string(b2))
	}
}
