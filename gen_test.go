package main

import (
	"testing"
)

// cfgFor returns a validated config for args, failing the test on error.
func cfgFor(t *testing.T, args ...string) config {
	t.Helper()
	cfg, err := parseConfig(args)
	if err != nil {
		t.Fatalf("parseConfig(%q): %v", args, err)
	}
	return cfg
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"50GB": 50e9, "1.5GiB": 3 << 29, "10mb": 10e6, " 2 KB ": 2000,
		"3KiB": 3072, "7B": 7, "123": 123,
	}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"abc", "GB", "1.5", ""} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) succeeded, want error", in)
		}
	}
}

func TestParseConfigDefaults(t *testing.T) {
	cfg := cfgFor(t)
	if cfg.target != 50e9 || cfg.ndjson || cfg.seed != 42 || cfg.pNull != 0.1 ||
		cfg.pEmpty != 0.1 || cfg.depth != 4 || cfg.typed || cfg.workers < 1 || cfg.chunk != 1024 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestParseConfigRejects(t *testing.T) {
	for _, args := range [][]string{
		{"-size", "0"}, {"-size", "lots"},
		{"-null", "-0.1"}, {"-null", "0.6", "-empty", "0.5"},
		{"-depth", "-1"}, {"-workers", "0"}, {"-chunk", "0"},
		{"-schema", "strict"},
	} {
		if _, err := parseConfig(args); err == nil {
			t.Errorf("parseConfig(%q) succeeded, want error", args)
		}
	}
}
