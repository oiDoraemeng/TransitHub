package settings

import (
	"encoding/json"
	"testing"
)

func TestRecoveryProbeSettingsDefaultsLegacyAndBounds(t *testing.T) {
	legacy := DefaultStrategySettings()
	if err := json.Unmarshal([]byte(`{"refreshInterval":120}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.RecoveryProbeFirstTimeoutSeconds != 90 || legacy.RecoveryProbeSubsequentTimeoutSeconds != 60 {
		t.Fatalf("legacy defaults: %+v", legacy)
	}
	zero := normalizeStrategySettings(StrategySettings{})
	if zero.RecoveryProbeFirstTimeoutSeconds != 90 || zero.RecoveryProbeSubsequentTimeoutSeconds != 60 {
		t.Fatalf("empty defaults: %+v", zero)
	}
	custom := normalizeStrategySettings(StrategySettings{RecoveryProbeFirstTimeoutSeconds: 90, RecoveryProbeSubsequentTimeoutSeconds: 25})
	if custom.RecoveryProbeFirstTimeoutSeconds != 90 || custom.RecoveryProbeSubsequentTimeoutSeconds != 25 {
		t.Fatal("custom timeouts changed")
	}
	bounded := normalizeStrategySettings(StrategySettings{RecoveryProbeFirstTimeoutSeconds: 900, RecoveryProbeSubsequentTimeoutSeconds: -1})
	if bounded.RecoveryProbeFirstTimeoutSeconds != 300 || bounded.RecoveryProbeSubsequentTimeoutSeconds != 60 {
		t.Fatalf("invalid bounds: %+v", bounded)
	}
}
