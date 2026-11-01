package config

import "testing"

func TestPerpRiskConfigMarginModeDefaultIsIsolated(t *testing.T) {
	t.Setenv("PERP_MARGIN_MODE", "")
	cfg := LoadPerpRiskConfig()
	if cfg.CrossMargin() {
		t.Fatalf("default mode must be isolated, got %q", cfg.MarginMode)
	}
}

func TestPerpRiskConfigCrossMode(t *testing.T) {
	t.Setenv("PERP_MARGIN_MODE", "cross")
	cfg := LoadPerpRiskConfig()
	if !cfg.CrossMargin() {
		t.Fatalf("want cross, got %q", cfg.MarginMode)
	}
}

func TestPerpRiskConfigSlippage(t *testing.T) {
	t.Setenv("PERP_LIQUIDATION_SLIPPAGE", "")
	if got := LoadPerpRiskConfig().SlippageE18; got != "" {
		t.Fatalf("want empty (meaning default), got %q", got)
	}
	t.Setenv("PERP_LIQUIDATION_SLIPPAGE", "0")
	if got := LoadPerpRiskConfig().SlippageE18; got != "0" {
		t.Fatalf("an explicit zero must be preserved, got %q", got)
	}
}
