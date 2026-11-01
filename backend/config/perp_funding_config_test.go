package config

import "testing"

func TestLoadPerpFundingConfigDefaults(t *testing.T) {
	t.Setenv("PERP_INDEX_PRICES", "")
	t.Setenv("PERP_TICK_INTERVAL", "")
	t.Setenv("PERP_FUNDING_INTERVAL", "")
	t.Setenv("PERP_MAX_PRICE_AGE", "")
	t.Setenv("PERP_MAX_MARK_STALENESS", "")
	t.Setenv("PERP_MAX_FUNDING_RATE", "")
	t.Setenv("PERP_MARK_MAX_DEVIATION", "")

	cfg := LoadPerpFundingConfig()
	if cfg.TickSeconds != 5 {
		t.Fatalf("tick = %d, want 5", cfg.TickSeconds)
	}
	if cfg.FundingIntervalSeconds != 3600 {
		t.Fatalf("funding interval = %d, want 3600", cfg.FundingIntervalSeconds)
	}
	if cfg.MaxPriceAgeSeconds != 60 {
		t.Fatalf("max price age = %d, want 60", cfg.MaxPriceAgeSeconds)
	}
	if cfg.MaxMarkStalenessSeconds != 300 {
		t.Fatalf("max mark staleness = %d, want 300", cfg.MaxMarkStalenessSeconds)
	}
	if cfg.MaxFundingRateE18 != "7500000000000000" {
		t.Fatalf("max rate = %q", cfg.MaxFundingRateE18)
	}
	if cfg.MarkMaxDeviationE18 != "5000000000000000" {
		t.Fatalf("max deviation = %q", cfg.MarkMaxDeviationE18)
	}
}

func TestLoadPerpFundingConfigOverrides(t *testing.T) {
	t.Setenv("PERP_INDEX_PRICES", "1:2000")
	t.Setenv("PERP_TICK_INTERVAL", "2")
	t.Setenv("PERP_FUNDING_INTERVAL", "28800")
	t.Setenv("PERP_MAX_PRICE_AGE", "30")
	t.Setenv("PERP_MAX_MARK_STALENESS", "600")
	t.Setenv("PERP_MAX_FUNDING_RATE", "10000000000000000")
	t.Setenv("PERP_MARK_MAX_DEVIATION", "10000000000000000")

	cfg := LoadPerpFundingConfig()
	if cfg.IndexPrices != "1:2000" {
		t.Fatalf("index prices = %q", cfg.IndexPrices)
	}
	if cfg.TickSeconds != 2 || cfg.FundingIntervalSeconds != 28800 {
		t.Fatalf("intervals = %d, %d", cfg.TickSeconds, cfg.FundingIntervalSeconds)
	}
	if cfg.MaxPriceAgeSeconds != 30 || cfg.MaxMarkStalenessSeconds != 600 {
		t.Fatalf("staleness = %d, %d", cfg.MaxPriceAgeSeconds, cfg.MaxMarkStalenessSeconds)
	}
	if cfg.MaxFundingRateE18 != "10000000000000000" {
		t.Fatalf("max rate = %q", cfg.MaxFundingRateE18)
	}
	if cfg.MarkMaxDeviationE18 != "10000000000000000" {
		t.Fatalf("max deviation = %q", cfg.MarkMaxDeviationE18)
	}
}

func TestPerpFundingConfigDurations(t *testing.T) {
	cfg := PerpFundingConfig{TickSeconds: 5, FundingIntervalSeconds: 3600,
		MaxPriceAgeSeconds: 60, MaxMarkStalenessSeconds: 300}
	if cfg.Tick().Seconds() != 5 || cfg.FundingInterval().Seconds() != 3600 {
		t.Fatal("tick/funding durations wrong")
	}
	if cfg.MaxPriceAge().Seconds() != 60 || cfg.MaxMarkStaleness().Seconds() != 300 {
		t.Fatal("staleness durations wrong")
	}
}

func TestLoadPerpFundingConfigIgnoresInvalidNumbers(t *testing.T) {
	t.Setenv("PERP_TICK_INTERVAL", "abc")
	t.Setenv("PERP_FUNDING_INTERVAL", "-1")
	cfg := LoadPerpFundingConfig()
	if cfg.TickSeconds != 5 || cfg.FundingIntervalSeconds != 3600 {
		t.Fatalf("invalid values must fall back to defaults, got %d, %d", cfg.TickSeconds, cfg.FundingIntervalSeconds)
	}
}
