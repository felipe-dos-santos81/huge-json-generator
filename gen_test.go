package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
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

// records decodes array or NDJSON output into objects, failing on invalid JSON.
func records(t *testing.T, out []byte, ndjson bool) []map[string]any {
	t.Helper()
	var recs []map[string]any
	if !ndjson {
		if !json.Valid(out) {
			t.Fatal("array output is not valid JSON")
		}
		dec := json.NewDecoder(bytes.NewReader(out))
		dec.UseNumber()
		if err := dec.Decode(&recs); err != nil {
			t.Fatal(err)
		}
		return recs
	}
	if len(out) == 0 || out[len(out)-1] != '\n' {
		t.Fatal("NDJSON output must end with a newline")
	}
	for i, line := range bytes.Split(out[:len(out)-1], []byte("\n")) {
		if !json.Valid(line) {
			t.Fatalf("line %d is not valid JSON: %.200s", i, line)
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, m)
	}
	return recs
}

func TestChaosChunkValid(t *testing.T) {
	b := newBuilder(cfgFor(t, "-chunk", "256", "-ndjson"))
	for i := int64(0); i < 4; i++ {
		recs := records(t, b.chunk(nil, i), true)
		if len(recs) != 256 {
			t.Fatalf("chunk %d: %d records, want 256", i, len(recs))
		}
		for j, r := range recs {
			if want := json.Number(strconv.FormatInt(i*256+int64(j), 10)); r["id"] != want {
				t.Fatalf("chunk %d record %d: id %v, want %s", i, j, r["id"], want)
			}
		}
	}
}

// In array mode only record 0 lacks the ",\n" separator.
func TestChunkArrayFraming(t *testing.T) {
	b := newBuilder(cfgFor(t, "-chunk", "8"))
	if c := b.chunk(nil, 0); !bytes.HasPrefix(c, []byte(`{"id":0`)) {
		t.Fatalf("chunk 0 starts with %.20q", c)
	}
	if c := b.chunk(nil, 1); !bytes.HasPrefix(c, []byte(",\n{\"id\":8")) {
		t.Fatalf("chunk 1 starts with %.20q", c)
	}
}

// Chunks with neighbouring indices must not share their random stream.
func TestChunksDiffer(t *testing.T) {
	b := newBuilder(cfgFor(t, "-chunk", "4", "-ndjson"))
	strip := func(c []byte) string { // drop the "id" values, which always differ
		var parts []string
		for _, line := range strings.Split(string(c), "\n") {
			if i := strings.IndexByte(line, ','); i >= 0 {
				parts = append(parts, line[i:])
			}
		}
		return strings.Join(parts, "\n")
	}
	a := strip(b.chunk(nil, 0))
	if a == strip(b.chunk(nil, 1)) {
		t.Fatal("chunk 0 and chunk 1 have identical content")
	}
	if a != strip(b.chunk(nil, 0)) {
		t.Fatal("rebuilding chunk 0 gave different content")
	}
}
