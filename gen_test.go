package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
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
	for _, in := range []string{"abc", "GB", "1.5", "", "InfGB", "1e30GB", "NaNGB"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) succeeded, want error", in)
		}
	}
}

func TestParseConfigDefaults(t *testing.T) {
	cfg := cfgFor(t)
	if cfg.target != 50e9 || cfg.ndjson || cfg.seed != 42 || cfg.pNull != 0.1 ||
		cfg.pEmpty != 0.1 || cfg.depth != 4 || cfg.schema != chaos || cfg.workers < 1 || cfg.chunk != 1024 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestParseConfigRejects(t *testing.T) {
	for _, args := range [][]string{
		{"-size", "0"}, {"-size", "lots"},
		{"-null", "-0.1"}, {"-null", "0.6", "-empty", "0.5"},
		{"-depth", "-1"}, {"-workers", "0"}, {"-chunk", "0"},
		{"-schema", "strict"},
		{"-depth", "11"}, {"-null", "NaN"}, {"-empty", "NaN"},
		{"-ndjson", "true"}, {"10MB"},
		{"-workers", "abc"}, {"-bogus"},
	} {
		if _, err := parseConfig(args); err == nil {
			t.Errorf("parseConfig(%q) succeeded, want error", args)
		}
	}
}

// Flag-syntax errors come back as one-line errors instead of exiting, and
// -h reports flag.ErrHelp so main can print the usage.
func TestParseConfigSyntaxErrors(t *testing.T) {
	_, err := parseConfig([]string{"-workers", "abc"})
	if err == nil || strings.Contains(err.Error(), "\n") {
		t.Errorf("-workers abc: err = %q, want a one-line error", err)
	}
	if _, err := parseConfig([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: err = %v, want flag.ErrHelp", err)
	}
}

func TestParseConfigFlags(t *testing.T) {
	cfg := cfgFor(t, "-size", "2MiB", "-ndjson", "-seed", "7", "-null", "0.3", "-empty", "0.2",
		"-depth", "10", "-schema", "typed", "-workers", "3", "-chunk", "5")
	want := config{target: 2 << 20, ndjson: true, seed: 7, pNull: 0.3, pEmpty: 0.2,
		depth: 10, schema: typed, workers: 3, chunk: 5}
	if cfg != want {
		t.Fatalf("got %+v, want %+v", cfg, want)
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

func gen(t *testing.T, args ...string) []byte {
	t.Helper()
	var out bytes.Buffer
	if _, _, err := run(&out, cfgFor(t, args...)); err != nil {
		t.Fatalf("run(%q): %v", args, err)
	}
	return out.Bytes()
}

func checkIDs(t *testing.T, recs []map[string]any) {
	t.Helper()
	if len(recs) == 0 {
		t.Fatal("no records")
	}
	for i, r := range recs {
		if r["id"] != json.Number(strconv.Itoa(i)) {
			t.Fatalf("record %d has id %v", i, r["id"])
		}
	}
}

func TestValidOutput(t *testing.T) {
	for _, schema := range []string{"chaos", "typed"} {
		for _, ndjson := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ndjson=%v", schema, ndjson), func(t *testing.T) {
				out := gen(t, "-size", "5MB", "-schema", schema, "-ndjson="+strconv.FormatBool(ndjson), "-chunk", "64")
				if len(out) < 5e6 {
					t.Fatalf("output %d bytes, want >= 5MB", len(out))
				}
				checkIDs(t, records(t, out, ndjson))
			})
		}
	}
}

func TestDeterministicAcrossWorkers(t *testing.T) {
	sum := func(args ...string) [32]byte {
		return sha256.Sum256(gen(t, append([]string{"-size", "2MB", "-chunk", "16"}, args...)...))
	}
	base := sum("-workers", "1")
	for _, w := range []string{"4", "16"} {
		if sum("-workers", w) != base {
			t.Errorf("-workers %s changed the output", w)
		}
	}
	if sum("-workers", "4", "-seed", "7") == base {
		t.Error("-seed did not change the output")
	}
}

// ---- Review Focus edge cases ----

func TestTinySizeStillValid(t *testing.T) {
	for _, ndjson := range []bool{false, true} {
		out := gen(t, "-size", "1B", "-chunk", "3", "-ndjson="+strconv.FormatBool(ndjson))
		recs := records(t, out, ndjson)
		checkIDs(t, recs)
		if len(recs) != 3 {
			t.Errorf("ndjson=%v: got %d records, want one chunk of 3", ndjson, len(recs))
		}
	}
}

func TestManyWorkersTinyOutput(t *testing.T) {
	a := gen(t, "-size", "10KB", "-workers", "64", "-chunk", "1")
	b := gen(t, "-size", "10KB", "-workers", "1", "-chunk", "1")
	if !bytes.Equal(a, b) {
		t.Fatal("64 workers and 1 worker differ")
	}
	checkIDs(t, records(t, a, false))
}

type failWriter struct{ left int }

func (w *failWriter) Write(p []byte) (int, error) {
	if len(p) > w.left {
		n := w.left
		w.left = 0
		return n, errors.New("disk full")
	}
	w.left -= len(p)
	return len(p), nil
}

func TestWriteErrorStops(t *testing.T) {
	for _, left := range []int{0, 1 << 20} {
		_, written, err := run(&failWriter{left: left}, cfgFor(t, "-size", "50GB", "-workers", "8", "-chunk", "16"))
		if err == nil || err.Error() != "disk full" {
			t.Fatalf("left=%d: err = %v, want disk full", left, err)
		}
		if written != int64(left) {
			t.Fatalf("left=%d: written = %d", left, written)
		}
	}
}

func TestSchemaChangesOutput(t *testing.T) {
	a := gen(t, "-size", "100KB", "-chunk", "16")
	b := gen(t, "-size", "100KB", "-chunk", "16", "-schema", "typed")
	if bytes.Equal(a, b) {
		t.Fatal("-schema typed produced the same bytes as chaos")
	}
}

// walk records each key path's JSON type in seen, skipping nulls, and fails
// on integers outside the float64-exact range.
func walk(t *testing.T, path string, v any, seen map[string]string) {
	t.Helper()
	var kind string
	switch v := v.(type) {
	case nil:
		return
	case string:
		kind = "string"
	case bool:
		kind = "bool"
	case json.Number:
		kind = "number"
		if !strings.ContainsAny(string(v), ".eE") {
			n, err := strconv.ParseInt(string(v), 10, 64)
			if err != nil || n > maxSafeInt || n < -maxSafeInt {
				t.Fatalf("%s: integer %s outside ±(2^53-1)", path, v)
			}
		}
	case []any:
		kind = "array"
		for _, e := range v {
			walk(t, path+"[]", e, seen)
		}
	case map[string]any:
		kind = "object"
		for k, e := range v {
			walk(t, path+"."+k, e, seen)
		}
	}
	if prev, ok := seen[path]; ok && prev != kind {
		t.Fatalf("%s is both %s and %s", path, prev, kind)
	}
	seen[path] = kind
}

func TestTypedStable(t *testing.T) {
	recs := records(t, gen(t, "-size", "5MB", "-schema", "typed", "-ndjson", "-chunk", "64"), true)
	seen := map[string]string{}
	for _, r := range recs {
		walk(t, "", r, seen)
	}
	// Spot-check that deep and nested paths were actually generated.
	for _, p := range []string{".score", ".items[].qty", ".payload.child.child.n", ".meta.flags[]", ".count", ".données"} {
		if _, ok := seen[p]; !ok {
			t.Errorf("path %s never generated", p)
		}
	}
}

// Floats in typed mode always carry a decimal point, never an exponent.
func TestTypedFloatsFixedPoint(t *testing.T) {
	recs := records(t, gen(t, "-size", "1MB", "-schema", "typed", "-ndjson", "-chunk", "64"), true)
	for _, r := range recs {
		for _, k := range []string{"score", "price"} {
			if n, ok := r[k].(json.Number); ok && (!strings.Contains(string(n), ".") || strings.ContainsAny(string(n), "eE")) {
				t.Fatalf("%s = %s, want fixed-point with a decimal point", k, n)
			}
			if n, ok := r[k].(json.Number); ok {
				v, err := strconv.ParseFloat(string(n), 64)
				if err != nil {
					t.Fatalf("%s = %s: %v", k, n, err)
				}
				if k == "score" && (v < 0 || v >= 100) {
					t.Fatalf("score = %.4f, want [0, 100)", v)
				}
				if k == "price" && (v < 0 || v >= 1000) {
					t.Fatalf("price = %.2f, want [0, 1000)", v)
				}
			}
		}
	}
}

func TestNullRate(t *testing.T) {
	for _, schema := range []string{"chaos", "typed"} {
		recs := records(t, gen(t, "-size", "5MB", "-schema", schema, "-ndjson", "-null", "0.2", "-empty", "0", "-chunk", "64"), true)
		var nulls, total int
		for _, r := range recs {
			for k, v := range r {
				if k == "id" {
					continue
				}
				total++
				if v == nil {
					nulls++
				}
			}
		}
		if rate := float64(nulls) / float64(total); math.Abs(rate-0.2) > 0.02 {
			t.Errorf("%s: null rate %.3f over %d values, want 0.20 ± 0.02", schema, rate, total)
		}
	}
}

// ---- Review Focus edge cases ----

func TestDepthZero(t *testing.T) {
	for _, schema := range []string{"chaos", "typed"} {
		out := gen(t, "-size", "200KB", "-depth", "0", "-schema", schema, "-ndjson", "-chunk", "64")
		for _, r := range records(t, out, true) {
			for k, v := range r {
				switch v := v.(type) {
				case []any:
					if schema == "chaos" && len(v) > 0 {
						t.Fatalf("chaos depth 0: %s is a non-empty array", k)
					}
				case map[string]any:
					if schema == "chaos" && len(v) > 0 {
						t.Fatalf("chaos depth 0: %s is a non-empty object", k)
					}
					if _, ok := v["child"]; ok && k == "payload" {
						t.Fatal("typed depth 0: payload has a child")
					}
				}
			}
		}
	}
}

func TestAllNull(t *testing.T) {
	for _, schema := range []string{"chaos", "typed"} {
		out := gen(t, "-size", "50KB", "-null", "1", "-empty", "0", "-schema", schema, "-ndjson", "-chunk", "64")
		for _, r := range records(t, out, true) {
			for k, v := range r {
				if k != "id" && v != nil {
					t.Fatalf("%s: -null 1 but %s = %v", schema, k, v)
				}
			}
		}
	}
}

func TestTypedAllEmpty(t *testing.T) {
	out := gen(t, "-size", "50KB", "-null", "0", "-empty", "1", "-schema", "typed", "-ndjson", "-chunk", "64")
	for _, r := range records(t, out, true) {
		for k, v := range r {
			switch v := v.(type) {
			case string:
				if v != "" {
					t.Fatalf("-empty 1: %s = %q", k, v)
				}
			case []any:
				if len(v) != 0 {
					t.Fatalf("-empty 1: %s has %d elements", k, len(v))
				}
			case map[string]any:
				if len(v) != 0 {
					t.Fatalf("-empty 1: %s has %d fields", k, len(v))
				}
			case nil:
				t.Fatalf("-empty 1 -null 0: %s is null", k)
			}
		}
	}
}

// typedFixed must stay below its limit and keep its decimals, even for the largest draw.
func TestFixedRange(t *testing.T) {
	b := newBuilder(cfgFor(t, "-null", "0", "-empty", "0"))
	b.pcg.Seed(1, 2)
	seen := map[string]bool{}
	for range 1000 {
		b.buf = b.buf[:0]
		b.typedFixed(1, 1)
		seen[string(b.buf)] = true
	}
	for d := range 10 {
		if want := fmt.Sprintf("0.%d", d); !seen[want] {
			t.Errorf("never produced %s", want)
		}
	}
	if len(seen) != 10 {
		t.Errorf("got %d distinct values, want exactly 0.0..0.9: %v", len(seen), seen)
	}
}
