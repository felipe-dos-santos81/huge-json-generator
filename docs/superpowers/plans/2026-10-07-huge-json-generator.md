# jsongen Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Go CLI that streams 50+ GB of valid random JSON (with `null` and empty values) to stdout, in `chaos` or `typed` schema, deterministically and in parallel.

**Architecture:** `main` parses flags into a `config` and calls `run`. `run` starts N workers that each build fixed-size chunks of records (chunk `i` seeded by `(seed, i)`) and a single writer that emits chunks strictly in index order, with at most 2N chunks in flight. Value generation lives in `value.go` on a per-worker `builder`.

**Tech Stack:** Go 1.27.1, standard library only (`math/rand/v2`, `encoding/json` in tests).

**Spec:** `docs/superpowers/specs/2026-10-07-huge-json-generator-design.md`

## Global Constraints

- `go.mod`: `module jsongen`, `go 1.27.1`. No third-party dependencies.
- Files: `go.mod`, `gen.go`, `value.go`, `gen_test.go` at repo root, `package main`.
- stdout carries only JSON. Progress and summary go to stderr.
- Flags and defaults exactly: `-size 50GB`, `-ndjson false`, `-seed 42`, `-null 0.10`, `-empty 0.10`, `-depth 4`, `-schema chaos`, `-workers runtime.NumCPU()`, `-chunk 1024`.
- Output bytes depend on `(seed, chunk, schema, null, empty, depth, ndjson, size)` and never on `-workers`.
- Typed mode: one JSON type per key path (ignoring `null`); integers within ±(2^53−1); floats fixed-point with a decimal point.
- Invalid flag values: one-line stderr message, exit 2. Write errors: `write: <err>`, exit 1.
- Code is `gofmt`-clean and `go vet`-clean.

## Review Focus

- `-size` smaller than one chunk (e.g. `1B`): output must still be valid JSON containing exactly one chunk of records — `TestTinySizeStillValid` (Task 3).
- Writer fails mid-stream (disk full): `run` must return the error promptly with no deadlock or hang — `TestWriteErrorStops` (Task 3).
- Many workers on tiny output (`-workers 64 -chunk 1`): no deadlock, bytes identical to 1 worker — `TestManyWorkersTinyOutput` (Task 3).
- `-depth 0`: valid output; chaos has only scalars or empty containers; typed `payload` has no `child` — `TestDepthZero` (Task 4).
- Probability extremes `-null 1` and `-empty 1`: every value null / every string, array, object empty, still valid — `TestAllNull`, `TestTypedAllEmpty` (Task 4).

---

### Task 1: Module and flag parsing

**Files:**
- Create: `go.mod`
- Create: `gen.go`
- Test: `gen_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type config struct { target int64; ndjson bool; seed uint64; pNull, pEmpty float64; depth int; typed bool; workers int; chunk int; progress io.Writer }`
  - `func parseSize(s string) (int64, error)`
  - `func parseConfig(args []string) (config, error)` — malformed flag syntax exits (flag.ExitOnError); invalid values return an error.
  - Test helper `func cfgFor(t *testing.T, args ...string) config`.

- [ ] **Step 1: Create the module**

```bash
cd /Users/felipe.dos.santos/code/mine/huge-json-generator
go mod init jsongen
cat go.mod
```
Expected: `module jsongen` and `go 1.27.1`.

- [ ] **Step 2: Write the failing tests**

Create `gen_test.go`:

```go
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
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./...`
Expected: FAIL — build errors `undefined: config`, `undefined: parseConfig`, `undefined: parseSize`.

- [ ] **Step 4: Write the implementation**

Create `gen.go`:

```go
// jsongen streams a random JSON payload of a target size to stdout.
//
//	go build -o jsongen .
//	./jsongen -size 50GB > big.json                      # one giant array
//	./jsongen -size 50GB -ndjson > big.ndjson            # one object per line
//	./jsongen -size 50GB -schema typed -ndjson | loader  # stable types per key
//
// Records are built in fixed-size chunks on -workers goroutines and written in
// order, so memory is bounded and the bytes depend only on the flags, never on
// -workers. Byte-identical output across runs needs the same Go toolchain
// (math/rand/v2 method outputs may change between releases).
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
)

type config struct {
	target   int64
	ndjson   bool
	seed     uint64
	pNull    float64
	pEmpty   float64
	depth    int
	typed    bool
	workers  int
	chunk    int
	progress io.Writer // nil: no progress output
}

func parseSize(s string) (int64, error) {
	units := []struct {
		suffix string
		mult   int64
	}{
		{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"B", 1},
	}
	u := strings.ToUpper(strings.TrimSpace(s))
	for _, x := range units {
		if strings.HasSuffix(u, x.suffix) {
			f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(u, x.suffix)), 64)
			if err != nil {
				return 0, err
			}
			return int64(f * float64(x.mult)), nil
		}
	}
	return strconv.ParseInt(u, 10, 64)
}

// parseConfig parses and validates command-line flags. Malformed flag syntax
// exits the process (flag.ExitOnError); invalid values return an error.
func parseConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("jsongen", flag.ExitOnError)
	sizeStr := fs.String("size", "50GB", "target size (B, KB, MB, GB decimal; KiB, MiB, GiB binary)")
	ndjson := fs.Bool("ndjson", false, "one object per line instead of a single array")
	seed := fs.Uint64("seed", 42, "RNG seed; same flags give identical output")
	pNull := fs.Float64("null", 0.10, "probability a value is null")
	pEmpty := fs.Float64("empty", 0.10, `probability a value is empty ("", [] or {}); typed numbers/bools are never empty`)
	depth := fs.Int("depth", 4, "maximum nesting depth")
	schema := fs.String("schema", "chaos", "chaos (any type per key) or typed (one type per key)")
	workers := fs.Int("workers", runtime.NumCPU(), "generator goroutines; does not change output")
	chunk := fs.Int("chunk", 1024, "records per chunk; changes output")
	fs.Parse(args)

	target, err := parseSize(*sizeStr)
	switch {
	case err != nil || target <= 0:
		return config{}, fmt.Errorf("bad -size %q", *sizeStr)
	case *pNull < 0 || *pEmpty < 0 || *pNull+*pEmpty > 1:
		return config{}, fmt.Errorf("-null and -empty must be >= 0 and sum to at most 1")
	case *depth < 0:
		return config{}, fmt.Errorf("-depth must be >= 0")
	case *workers < 1:
		return config{}, fmt.Errorf("-workers must be >= 1")
	case *chunk < 1:
		return config{}, fmt.Errorf("-chunk must be >= 1")
	case *schema != "chaos" && *schema != "typed":
		return config{}, fmt.Errorf("-schema must be chaos or typed, got %q", *schema)
	}
	return config{
		target: target, ndjson: *ndjson, seed: *seed,
		pNull: *pNull, pEmpty: *pEmpty, depth: *depth,
		typed: *schema == "typed", workers: *workers, chunk: *chunk,
	}, nil
}

func main() {
	if _, err := parseConfig(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
```

(`main` only validates for now; Task 3 replaces it.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `gofmt -l . && go vet ./... && go test ./...`
Expected: no gofmt output, `ok  	jsongen`.

- [ ] **Step 6: Commit**

```bash
git add go.mod gen.go gen_test.go
git commit -m "Add config and flag parsing

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Chunk builder with chaos values

**Files:**
- Create: `value.go`
- Modify: `gen_test.go` (imports, new helper, new tests)

**Interfaces:**
- Consumes: `config`, `cfgFor` (Task 1).
- Produces:
  - `type builder struct` with fields `pcg *rand.PCG; r *rand.Rand; buf []byte; seed uint64; chunkLen int64; ndjson, typed bool; pNull, pEmpty float64; maxDepth int`
  - `func newBuilder(cfg config) *builder`
  - `func (b *builder) chunk(dst []byte, i int64) []byte` — appends records `[i*chunk, (i+1)*chunk)` to `dst`, framed for the output mode (array: `,\n` before every record with id > 0; NDJSON: `\n` after every record). Pure function of config and `i`.
  - `func (b *builder) strN(alphabet []rune, n int)`, `func (b *builder) textLen() int`, `func (b *builder) key(k string)` — reused by Task 4.
  - `var keys []string` — reused by Task 4.
  - Test helper `func records(t *testing.T, out []byte, ndjson bool) []map[string]any` (decodes with `UseNumber`).

- [ ] **Step 1: Write the failing tests**

Replace the import block of `gen_test.go` with:

```go
import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)
```

Append to `gen_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./...`
Expected: FAIL — `undefined: newBuilder`.

- [ ] **Step 3: Write the implementation**

Create `value.go`. The chaos generator is the user's draft ported onto `builder` (`gen` → `builder`, `str()` → `strN(alphabet, n)` + `textLen()`, `fields` always writes a leading comma):

```go
package main

import (
	"math/rand/v2"
	"strconv"
	"unicode/utf8"
)

// Keys are drawn as an ordered random subset, so an object never has duplicates.
var keys = []string{
	"name", "email", "score", "active", "tags", "meta", "note", "price",
	"created_at", "payload", "items", "owner", "ref", "count", "extra", "données",
}

// Plain ASCII plus the things that hit parser slow paths: escapes,
// control characters, 2/3/4-byte UTF-8.
var chaosAlphabet = []rune(
	"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" +
		"      _-.,:;/@#" +
		"áçãéõüñß" + "日本語한국" + "😀🚀" +
		"\"\\/\n\t\r\b\f\x01\x1f")

// builder generates one chunk of records at a time. Each worker owns one.
type builder struct {
	pcg      *rand.PCG
	r        *rand.Rand
	buf      []byte
	seed     uint64
	chunkLen int64
	ndjson   bool
	typed    bool
	pNull    float64
	pEmpty   float64
	maxDepth int
}

func newBuilder(cfg config) *builder {
	pcg := rand.NewPCG(0, 0)
	return &builder{
		pcg: pcg, r: rand.New(pcg), seed: cfg.seed, chunkLen: int64(cfg.chunk),
		ndjson: cfg.ndjson, typed: cfg.typed,
		pNull: cfg.pNull, pEmpty: cfg.pEmpty, maxDepth: cfg.depth,
	}
}

// chunk appends records [i*chunkLen, (i+1)*chunkLen) to dst, framed for the
// output mode. The result depends only on the config and i.
func (b *builder) chunk(dst []byte, i int64) []byte {
	b.pcg.Seed(b.seed, uint64(i))
	b.buf = dst
	for id := i * b.chunkLen; id < (i+1)*b.chunkLen; id++ {
		if id > 0 && !b.ndjson {
			b.buf = append(b.buf, ',', '\n')
		}
		b.record(id)
		if b.ndjson {
			b.buf = append(b.buf, '\n')
		}
	}
	return b.buf
}

// strN writes a quoted JSON string of n runes drawn from alphabet.
func (b *builder) strN(alphabet []rune, n int) {
	b.buf = append(b.buf, '"')
	for range n {
		c := alphabet[b.r.IntN(len(alphabet))]
		switch c {
		case '"':
			b.buf = append(b.buf, '\\', '"')
		case '\\':
			b.buf = append(b.buf, '\\', '\\')
		case '\n':
			b.buf = append(b.buf, '\\', 'n')
		case '\t':
			b.buf = append(b.buf, '\\', 't')
		case '\r':
			b.buf = append(b.buf, '\\', 'r')
		case '\b':
			b.buf = append(b.buf, '\\', 'b')
		case '\f':
			b.buf = append(b.buf, '\\', 'f')
		default:
			if c < 0x20 {
				b.buf = append(b.buf, '\\', 'u', '0', '0',
					"0123456789abcdef"[c>>4], "0123456789abcdef"[c&0xf])
			} else {
				b.buf = utf8.AppendRune(b.buf, c)
			}
		}
	}
	b.buf = append(b.buf, '"')
}

// textLen is a short length, with an occasional long one.
func (b *builder) textLen() int {
	if b.r.IntN(50) == 0 {
		return 200 + b.r.IntN(2000)
	}
	return 1 + b.r.IntN(24)
}

func (b *builder) key(k string) {
	b.buf = append(b.buf, '"')
	b.buf = append(b.buf, k...)
	b.buf = append(b.buf, '"', ':')
}

// ---- chaos: any key may hold any type ----

// fields writes a random, duplicate-free subset of keys, each preceded by a comma.
func (b *builder) fields(depth int) {
	for _, k := range keys {
		if b.r.IntN(3) == 0 {
			continue
		}
		b.buf = append(b.buf, ',')
		b.key(k)
		b.value(depth)
	}
}

func (b *builder) value(depth int) {
	x := b.r.Float64()
	if x < b.pNull {
		b.buf = append(b.buf, "null"...)
		return
	}
	if x < b.pNull+b.pEmpty {
		switch b.r.IntN(3) {
		case 0:
			b.buf = append(b.buf, `""`...)
		case 1:
			b.buf = append(b.buf, "[]"...)
		default:
			b.buf = append(b.buf, "{}"...)
		}
		return
	}

	kind := b.r.IntN(10)
	if depth >= b.maxDepth {
		kind = b.r.IntN(7) // scalars only at the depth limit
	}
	switch kind {
	case 0, 1, 2:
		b.strN(chaosAlphabet, b.textLen())
	case 3: // small int
		b.buf = strconv.AppendInt(b.buf, int64(b.r.IntN(100000))-1000, 10)
	case 4: // full-range int64
		b.buf = strconv.AppendInt(b.buf, int64(b.r.Uint64()), 10)
	case 5: // float, shortest round-trip repr (may use exponent form)
		b.buf = strconv.AppendFloat(b.buf, b.r.NormFloat64()*float64(int64(1)<<b.r.IntN(40)), 'g', -1, 64)
	case 6:
		b.buf = strconv.AppendBool(b.buf, b.r.IntN(2) == 0)
	case 7, 8: // array
		n := 1 + b.r.IntN(6)
		b.buf = append(b.buf, '[')
		for i := range n {
			if i > 0 {
				b.buf = append(b.buf, ',')
			}
			b.value(depth + 1)
		}
		b.buf = append(b.buf, ']')
	default: // object
		b.buf = append(b.buf, '{')
		b.key("k")
		b.value(depth + 1)
		b.fields(depth + 1)
		b.buf = append(b.buf, '}')
	}
}

func (b *builder) record(id int64) {
	b.buf = append(b.buf, `{"id":`...)
	b.buf = strconv.AppendInt(b.buf, id, 10)
	b.fields(1)
	b.buf = append(b.buf, '}')
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `gofmt -l . && go vet ./... && go test ./...`
Expected: no gofmt output, `ok  	jsongen`.

- [ ] **Step 5: Commit**

```bash
git add value.go gen_test.go
git commit -m "Add chunk builder with chaos value generator

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Ordered parallel pipeline and main

**Files:**
- Modify: `gen.go` (imports, add `chunkOut`, `run`, replace `main`)
- Modify: `gen_test.go` (imports, helpers, new tests)

**Interfaces:**
- Consumes: `config`, `parseConfig` (Task 1); `newBuilder`, `(*builder).chunk` (Task 2); `cfgFor`, `records` (test helpers).
- Produces:
  - `func run(w io.Writer, cfg config) (records, written int64, err error)` — writes the whole payload; always at least one chunk; stops after the first chunk that brings `written >= cfg.target`; returns the first write error.
  - Test helpers `func gen(t *testing.T, args ...string) []byte` and `func checkIDs(t *testing.T, recs []map[string]any)`.

- [ ] **Step 1: Write the failing tests**

Replace the import block of `gen_test.go` with:

```go
import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)
```

Append to `gen_test.go`:

```go
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
	for _, schema := range []string{"chaos"} {
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./...`
Expected: FAIL — `undefined: run`.

- [ ] **Step 3: Write the implementation**

In `gen.go`, replace the import block with:

```go
import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)
```

In `gen.go`, replace the whole `main` function with:

```go
type chunkOut struct {
	idx int64
	buf *[]byte
}

// run writes the payload to w and reports how many records and bytes it wrote.
// Workers build chunks concurrently; at most 2*workers chunks are in flight and
// w receives them strictly in index order.
func run(w io.Writer, cfg config) (records, written int64, err error) {
	done := make(chan struct{})
	defer close(done) // releases workers on every return path

	tokens := make(chan struct{}, 2*cfg.workers)
	for range cap(tokens) {
		tokens <- struct{}{}
	}
	results := make(chan chunkOut)
	var next atomic.Int64
	pool := sync.Pool{New: func() any { b := make([]byte, 0, 1<<20); return &b }}

	for range cfg.workers {
		go func() {
			b := newBuilder(cfg)
			for {
				select {
				case <-tokens:
				case <-done:
					return
				}
				i := next.Add(1) - 1
				bp := pool.Get().(*[]byte)
				*bp = b.chunk((*bp)[:0], i)
				select {
				case results <- chunkOut{i, bp}:
				case <-done:
					return
				}
			}
		}()
	}

	write := func(p []byte) error {
		n, err := w.Write(p)
		written += int64(n)
		return err
	}
	if !cfg.ndjson {
		if err = write([]byte("[\n")); err != nil {
			return
		}
	}

	start := time.Now()
	nextReport := int64(1e9)
	pending := map[int64]*[]byte{}
	for idx := int64(0); idx == 0 || written < cfg.target; idx++ {
		bp, ok := pending[idx]
		for !ok {
			c := <-results
			if c.idx == idx {
				bp, ok = c.buf, true
			} else {
				pending[c.idx] = c.buf
			}
		}
		delete(pending, idx)
		err = write(*bp)
		pool.Put(bp)
		tokens <- struct{}{}
		if err != nil {
			return
		}
		records += int64(cfg.chunk)

		if cfg.progress != nil && written >= nextReport {
			el := time.Since(start).Seconds()
			fmt.Fprintf(cfg.progress, "\r%.1f / %.1f GB  %.0f MB/s",
				float64(written)/1e9, float64(cfg.target)/1e9, float64(written)/1e6/el)
			for nextReport <= written {
				nextReport += 1e9
			}
		}
	}
	if !cfg.ndjson {
		err = write([]byte("\n]\n"))
	}
	return
}

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cfg.progress = os.Stderr
	start := time.Now()
	records, written, err := run(os.Stdout, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "\nwrite:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "\rdone: %d records, %d bytes in %s\n",
		records, written, time.Since(start).Round(time.Millisecond))
}
```

Why it cannot deadlock: indices are claimed in increasing order only after taking a token. If the writer waits for chunk `idx` and `idx` is unclaimed, every lower chunk has been written and its token returned, so a worker can claim `idx`. If `idx` is claimed, its worker will deliver it, because the writer keeps draining `results` into `pending`.

- [ ] **Step 4: Run tests to verify they pass, including the race detector**

Run: `gofmt -l . && go vet ./... && go test -count=1 ./... && go test -count=1 -race ./...`
Expected: no gofmt output, `ok  	jsongen` twice (the race run takes about 30 s).

- [ ] **Step 5: Commit**

```bash
git add gen.go gen_test.go
git commit -m "Add ordered parallel chunk pipeline and main

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Typed schema

**Files:**
- Modify: `value.go` (imports, alphabets, `chunk` dispatch, typed generator)
- Modify: `gen_test.go` (imports, extend `TestValidOutput`, new tests)

**Interfaces:**
- Consumes: `builder`, `strN`, `textLen`, `key`, `keys` (Task 2); `run` (Task 3); test helpers `cfgFor`, `gen`, `records`, `checkIDs`.
- Produces:
  - `const maxSafeInt = 1<<53 - 1` (used by tests)
  - `func (b *builder) typedRecord(id int64)`; `chunk` calls it when `b.typed`.

- [ ] **Step 1: Write the failing tests**

Replace the import block of `gen_test.go` with:

```go
import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)
```

In `TestValidOutput`, change

```go
	for _, schema := range []string{"chaos"} {
```
to
```go
	for _, schema := range []string{"chaos", "typed"} {
```

Append to `gen_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./...`
Expected: FAIL — build error `undefined: maxSafeInt`.

- [ ] **Step 3: Write the implementation**

In `value.go`, replace the import block with:

```go
import (
	"math/rand/v2"
	"strconv"
	"time"
	"unicode/utf8"
)
```

In `value.go`, directly after the `chaosAlphabet` declaration, add:

```go
// Typed strings keep escapes and multi-byte UTF-8 but drop the control
// characters that ingestion pipelines commonly reject.
var typedAlphabet = []rune(
	"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" +
		"      _-.,:;/@#" +
		"áçãéõüñß" + "日本語한국" + "😀🚀" +
		"\"\\/\n\t")

var nameAlphabet = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZáçãéõüñß")

var wordAlphabet = []rune("abcdefghijklmnopqrstuvwxyz")

const maxSafeInt = 1<<53 - 1 // largest integer a float64-based parser keeps exact
```

In `(*builder).chunk`, replace

```go
		b.record(id)
```
with
```go
		if b.typed {
			b.typedRecord(id)
		} else {
			b.record(id)
		}
```

Append to `value.go`:

```go
// ---- typed: every key path has exactly one JSON type (plus null) ----

// special writes null, or the type's empty form, per the configured rates
// and reports whether it did. Numbers and bools pass empty == "" and get a
// normal value on an empty roll.
func (b *builder) special(empty string) bool {
	x := b.r.Float64()
	if x < b.pNull {
		b.buf = append(b.buf, "null"...)
		return true
	}
	if x < b.pNull+b.pEmpty && empty != "" {
		b.buf = append(b.buf, empty...)
		return true
	}
	return false
}

// open writes null/empty and returns false, or writes c and returns true.
func (b *builder) open(c byte, empty string) bool {
	if b.special(empty) {
		return false
	}
	b.buf = append(b.buf, c)
	return true
}

func (b *builder) comma(k string) {
	b.buf = append(b.buf, ',')
	b.key(k)
}

func (b *builder) sep(i int) {
	if i > 0 {
		b.buf = append(b.buf, ',')
	}
}

func (b *builder) end(c byte) { b.buf = append(b.buf, c) }

func (b *builder) tStr(alphabet []rune, minLen, maxLen int) {
	if !b.special(`""`) {
		b.strN(alphabet, minLen+b.r.IntN(maxLen-minLen+1))
	}
}

func (b *builder) tEmail() {
	if b.special(`""`) {
		return
	}
	b.buf = append(b.buf, '"')
	for range 3 + b.r.IntN(10) {
		b.buf = append(b.buf, byte('a'+b.r.IntN(26)))
	}
	b.buf = append(b.buf, '@')
	for range 3 + b.r.IntN(8) {
		b.buf = append(b.buf, byte('a'+b.r.IntN(26)))
	}
	b.buf = append(b.buf, ".com\""...)
}

func (b *builder) tInt(n int) {
	if !b.special("") {
		b.buf = strconv.AppendInt(b.buf, int64(b.r.IntN(n)), 10)
	}
}

// tFloat always prints a decimal point, so loaders never infer an integer.
func (b *builder) tFloat(scale float64, decimals int) {
	if !b.special("") {
		b.buf = strconv.AppendFloat(b.buf, b.r.Float64()*scale, 'f', decimals, 64)
	}
}

func (b *builder) tBool() {
	if !b.special("") {
		b.buf = strconv.AppendBool(b.buf, b.r.IntN(2) == 0)
	}
}

func (b *builder) tTime() {
	if b.special(`""`) {
		return
	}
	const base = 946684800 // 2000-01-01T00:00:00Z
	t := time.Unix(base+b.r.Int64N(40*365*86400), 0).UTC()
	b.buf = append(b.buf, '"')
	b.buf = t.AppendFormat(b.buf, time.RFC3339)
	b.buf = append(b.buf, '"')
}

func (b *builder) tHex() {
	if b.special(`""`) {
		return
	}
	b.buf = append(b.buf, '"')
	for range 2 {
		v := b.r.Uint64()
		for s := 60; s >= 0; s -= 4 {
			b.buf = append(b.buf, "0123456789abcdef"[v>>s&0xf])
		}
	}
	b.buf = append(b.buf, '"')
}

func (b *builder) tCount() {
	if b.special("") {
		return
	}
	v := int64(b.r.IntN(10000))
	if b.r.IntN(20) == 0 {
		v = int64(b.r.Uint64N(2*maxSafeInt+1)) - maxSafeInt
	}
	b.buf = strconv.AppendInt(b.buf, v, 10)
}

func (b *builder) tPayload(depth int) {
	if !b.open('{', "{}") {
		return
	}
	b.key("k")
	b.tStr(wordAlphabet, 1, 12)
	b.comma("n")
	b.tInt(1000)
	if depth < b.maxDepth {
		b.comma("child")
		b.tPayload(depth + 1)
	}
	b.end('}')
}

func (b *builder) typedField(k string) {
	switch k {
	case "name":
		b.tStr(nameAlphabet, 3, 20)
	case "email":
		b.tEmail()
	case "score":
		b.tFloat(100, 4)
	case "active":
		b.tBool()
	case "tags":
		if b.open('[', "[]") {
			for i := range b.r.IntN(6) {
				b.sep(i)
				b.tStr(wordAlphabet, 1, 10)
			}
			b.end(']')
		}
	case "meta":
		if b.open('{', "{}") {
			b.key("source")
			b.tStr(wordAlphabet, 3, 12)
			b.comma("version")
			b.tInt(1000)
			b.comma("flags")
			if b.open('[', "[]") {
				for i := range b.r.IntN(4) {
					b.sep(i)
					b.tBool()
				}
				b.end(']')
			}
			b.end('}')
		}
	case "note":
		if !b.special(`""`) {
			b.strN(typedAlphabet, b.textLen())
		}
	case "price":
		b.tFloat(1000, 2)
	case "created_at":
		b.tTime()
	case "payload":
		b.tPayload(1)
	case "items":
		if b.open('[', "[]") {
			for i := range b.r.IntN(6) {
				b.sep(i)
				if b.open('{', "{}") {
					b.key("sku")
					b.tStr(wordAlphabet, 6, 10)
					b.comma("qty")
					b.tInt(100)
					b.comma("price")
					b.tFloat(1000, 2)
					b.end('}')
				}
			}
			b.end(']')
		}
	case "owner":
		if b.open('{', "{}") {
			b.key("name")
			b.tStr(nameAlphabet, 3, 20)
			b.comma("email")
			b.tEmail()
			b.end('}')
		}
	case "ref":
		b.tHex()
	case "count":
		b.tCount()
	case "extra":
		if b.open('[', "[]") {
			for i := range b.r.IntN(6) {
				b.sep(i)
				b.tInt(1000)
			}
			b.end(']')
		}
	case "données":
		b.tStr(typedAlphabet, 1, 24)
	}
}

func (b *builder) typedRecord(id int64) {
	b.buf = append(b.buf, `{"id":`...)
	b.buf = strconv.AppendInt(b.buf, id, 10)
	for _, k := range keys {
		if b.r.IntN(3) == 0 {
			continue
		}
		b.comma(k)
		b.typedField(k)
	}
	b.buf = append(b.buf, '}')
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `gofmt -l . && go vet ./... && go test -count=1 ./...`
Expected: no gofmt output, `ok  	jsongen` (about 1 s).

- [ ] **Step 5: Commit**

```bash
git add value.go gen_test.go
git commit -m "Add typed schema with one JSON type per key path

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: End-to-end verification

No code changes unless a check fails. Run every command from the repo root. **zsh note:** never write `cmd 2>&1 >/dev/null | x` — zsh's MULTIOS also sends stdout into the pipe. Send stderr to a file instead, as below.

**Files:**
- Create (untracked, deleted at the end): `jsongen` binary, `err.txt`, `big.json`

**Interfaces:**
- Consumes: the built CLI from Tasks 1–4.
- Produces: measured numbers, reported to the user.

- [ ] **Step 1: Build and run the full suite under the race detector**

```bash
go build -o jsongen . && go test -count=1 -race ./...
```
Expected: `ok  	jsongen`.

- [ ] **Step 2: Bad flags exit 2 with one line**

```bash
./jsongen -null 0.7 -empty 0.7; echo "exit=$?"
./jsongen -schema strict; echo "exit=$?"
```
Expected: `-null and -empty must be >= 0 and sum to at most 1` then `exit=2`; `-schema must be chaos or typed, got "strict"` then `exit=2`.

- [ ] **Step 3: An independent parser accepts both forms**

```bash
./jsongen -size 200MB -schema typed -ndjson 2>/dev/null | jq -c . > /dev/null && echo "typed ndjson OK"
./jsongen -size 200MB 2>/dev/null | jq 'length'
```
Expected: `typed ndjson OK`, then a record count (about 87040).

- [ ] **Step 4: A consumer quitting early is silent**

```bash
./jsongen | head -c 100 > /dev/null; echo "status: ${pipestatus[*]}"
```
Expected: `status: 141 0` (SIGPIPE) and no error text.

- [ ] **Step 5: Throughput, 1 worker vs all cores**

```bash
for s in chaos typed; do for w in 1 $(sysctl -n hw.ncpu); do
  ./jsongen -size 5GB -schema $s -workers $w > /dev/null 2> err.txt
  printf "%s workers=%-3s %s\n" $s $w "$(LC_ALL=C tr '\r' '\n' < err.txt | grep '^done')"
done; done; rm err.txt
```
Expected (prototype on 14 cores): chaos about 0.18 GB/s on 1 worker and 1.6 GB/s on all cores; typed about 0.32 GB/s and 2.5 GB/s. Byte counts must be the same for 1 worker and all workers.

- [ ] **Step 6: Full 50 GB run to disk**

Check first: `df -h .` must show more than 55 GB free.

```bash
time ./jsongen -size 50GB > big.json
ls -l big.json
tail -c 200 big.json
```
Expected: the stderr progress line climbs to `50.0 / 50.0 GB`, then `done: ... records, ... bytes`. The file size is at least 50000000000 bytes, overshooting by less than about 3 MB, and the file ends with `}\n]\n`.

- [ ] **Step 7: Clean up**

```bash
rm -f big.json jsongen
git status --short
```
Expected: clean working tree. Report the Step 5 and Step 6 numbers to the user.
