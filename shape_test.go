package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"math/big"
	"strconv"
	"testing"
)

// nesting checks that deep output is only openers, the 0 leaf and matching
// closers, and returns the maximum depth. encoding/json stops at 10,000
// levels, so deep values need this instead of json.Valid.
func nesting(t *testing.T, out []byte) int {
	t.Helper()
	var stack []byte
	depth, leaves := 0, 0
	for i := 0; i < len(out); i++ {
		switch c := out[i]; {
		case c == '[':
			stack = append(stack, ']')
		case bytes.HasPrefix(out[i:], []byte(`{"k":`)):
			stack = append(stack, '}')
			i += 4
		case c == '0':
			leaves++
		case c == ']' || c == '}':
			if len(stack) == 0 || stack[len(stack)-1] != c {
				t.Fatalf("byte %d: unexpected %q", i, c)
			}
			stack = stack[:len(stack)-1]
		default:
			t.Fatalf("byte %d: unexpected %q", i, c)
		}
		depth = max(depth, len(stack))
	}
	if len(stack) != 0 || leaves != 1 {
		t.Fatalf("%d unclosed levels, %d leaves; want 0 and 1", len(stack), leaves)
	}
	return depth
}

func TestGiantDeep(t *testing.T) {
	out := gen(t, "-shape", "deep", "-size", "3MB")
	if len(out) < 3e6 {
		t.Fatalf("output %d bytes, want >= 3MB", len(out))
	}
	if depth, opens := nesting(t, out), openers(out); depth != opens {
		t.Fatalf("depth %d, want one level per opener (%d)", depth, opens)
	}
}

func TestGiantShapesValid(t *testing.T) {
	for _, shape := range []string{"wide-object", "wide-array", "longstring", "escapes"} {
		out := gen(t, "-shape", shape, "-size", "3MB")
		if len(out) < 3e6 || !json.Valid(out) {
			t.Fatalf("%s: %d bytes, valid=%v; want >= 3MB of valid JSON", shape, len(out), json.Valid(out))
		}
		var v any
		if err := json.Unmarshal(out, &v); err != nil {
			t.Fatal(err)
		}
		switch v := v.(type) {
		case string:
			if shape != "longstring" && shape != "escapes" {
				t.Fatalf("%s decoded to a string", shape)
			}
		case []any:
			if shape != "wide-array" {
				t.Fatalf("%s decoded to an array", shape)
			}
		case map[string]any:
			if shape != "wide-object" {
				t.Fatalf("%s decoded to an object", shape)
			}
			for i := range len(v) {
				if _, ok := v["k"+strconv.Itoa(i)]; !ok {
					t.Fatalf("wide-object: %d keys but k%d missing", len(v), i)
				}
			}
		default:
			t.Fatalf("%s decoded to %T", shape, v)
		}
	}
}

func TestGiantBignum(t *testing.T) {
	out := gen(t, "-shape", "bignum", "-size", "3MB")
	digits, ok := bytes.CutSuffix(out, []byte(".5e-7"))
	if !ok || len(digits) < 3e6-5 || digits[0] != '1' || bytes.ContainsFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) {
		t.Fatalf("bignum: %d bytes not of the form 1<digits>.5e-7", len(out))
	}
}

func TestGiantShapesIgnoreWorkers(t *testing.T) {
	for _, shape := range []string{"deep", "wide-object", "wide-array", "longstring", "escapes", "bignum"} {
		base := sha256.Sum256(gen(t, "-shape", shape, "-size", "3MB", "-workers", "1"))
		for _, w := range []string{"4", "16"} {
			if sha256.Sum256(gen(t, "-shape", shape, "-size", "3MB", "-workers", w)) != base {
				t.Errorf("%s: -workers %s changed the output", shape, w)
			}
		}
	}
}

// A target smaller than one chunk still yields one complete value.
func TestGiantTinySize(t *testing.T) {
	for _, shape := range []string{"deep", "wide-object", "wide-array", "longstring", "escapes", "bignum"} {
		out := gen(t, "-shape", shape, "-size", "1B")
		if shape == "deep" {
			nesting(t, out)
		} else if !json.Valid(out) {
			t.Fatalf("%s: -size 1B gave invalid JSON (%d bytes)", shape, len(out))
		}
	}
}

// A write error during deep's closing pass stops run with that error. Deep
// output is about 3/4 openers and 1/4 closers, so 9/10 lands in the tail.
func TestTailWriteErrorStops(t *testing.T) {
	inTail := len(gen(t, "-shape", "deep", "-size", "3MB")) * 9 / 10
	_, written, err := run(&failWriter{left: inTail}, cfgFor(t, "-shape", "deep", "-size", "3MB", "-workers", "4"))
	if err == nil || err.Error() != "disk full" || written != int64(inTail) {
		t.Fatalf("err = %v, written = %d; want disk full after %d bytes", err, written, inTail)
	}
}

func TestCappedShapes(t *testing.T) {
	for _, c := range []struct{ shape, cap string }{
		{"deep", "20KB"}, // ~5,000 levels, inside encoding/json's 10,000 limit
		{"wide-object", "200KB"}, {"wide-array", "200KB"},
		{"longstring", "200KB"}, {"escapes", "200KB"}, {"bignum", "200KB"},
	} {
		capBytes, _ := parseSize(c.cap)
		out := gen(t, "-shape", c.shape, "-cap", c.cap, "-size", "2MB", "-ndjson")
		for i, line := range bytes.Split(bytes.TrimSuffix(out, []byte("\n")), []byte("\n")) {
			var rec struct {
				ID int64
				V  json.RawMessage
			}
			if !json.Valid(line) || json.Unmarshal(line, &rec) != nil || rec.ID != int64(i) {
				t.Fatalf("%s: line %d invalid or id %d out of order", c.shape, i, rec.ID)
			}
			if d := float64(len(rec.V))/float64(capBytes) - 1; d < -0.05 || d > 0.05 {
				t.Fatalf("%s: value %d bytes, want %s ± 5%%", c.shape, len(rec.V), c.cap)
			}
			if c.shape == "bignum" {
				if _, ok := new(big.Float).SetString(string(rec.V)); !ok {
					t.Fatalf("bignum: %.40s… does not parse as a number", rec.V)
				}
			}
		}
	}
}

func TestCappedArrayValid(t *testing.T) {
	out := gen(t, "-shape", "wide-array", "-cap", "100KB", "-size", "1MB")
	var recs []struct{ V []any }
	if err := json.Unmarshal(out, &recs); err != nil || len(recs) < 10 {
		t.Fatalf("got %d records, err %v; want >= 10 in a valid array", len(recs), err)
	}
}

// A cap smaller than one unit still yields valid, minimal values.
func TestCappedTinyCap(t *testing.T) {
	for _, shape := range []string{"deep", "wide-object", "wide-array", "longstring", "escapes", "bignum"} {
		out := gen(t, "-shape", shape, "-cap", "1B", "-size", "2KB", "-ndjson")
		for _, line := range bytes.Split(bytes.TrimSuffix(out, []byte("\n")), []byte("\n")) {
			if !json.Valid(line) {
				t.Fatalf("%s -cap 1B: invalid record %q", shape, line)
			}
		}
	}
}
