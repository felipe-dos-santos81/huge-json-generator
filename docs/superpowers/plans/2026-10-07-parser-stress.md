# jsongen Parser-Stress Modes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `-shape` (giant or capped extreme values) and `-corpus` (labelled y/n/i parser test cases) to jsongen, on the existing ordered parallel pipeline.

**Architecture:** `run` drives a `source` (header, body chunks, optional tail chunks, trailer) instead of the record builder. Records, giant shapes, capped shapes and the corpus are each a source; `deep` closes its nesting in a second pass that mirrors regenerated body chunks. Output stays a pure function of the flags.

**Tech Stack:** Go 1.27.1, standard library only.

**Spec:** `docs/superpowers/specs/2026-10-07-parser-stress-design.md` (builds on `2026-10-07-huge-json-generator-design.md`)

## Global Constraints

- Standard library only; Go 1.27.1; `gofmt` and `go vet` clean at every commit.
- Records mode output must stay byte-identical (guarded by `TestRecordsBytesUnchanged` from Task 1).
- Every source's `chunk` and `tail` are pure functions of the config and their indices: output never depends on `-workers`.
- New flag errors are one line on stderr ending ` (see -h)`, exit 2 (existing `main` behaviour).
- `-cap` range: (0, 256MB]. Shapes: `deep`, `wide-object`, `wide-array`, `longstring`, `escapes`, `bignum`.
- Corpus line format: `{"id":N,"verdict":"y|n|i","category":"...","doc_b64":"..."}`.
- **Never type `\u` followed by four hex digits in Go source.** Generated edits have turned such escapes into raw characters. Use numeric rune literals (`0xFEFF`) and the `uEscape` helper. Surrogate escapes inside raw strings (as in `lone_surrogate`) are safe.
- Run all commands from the repo root. All code below is copied from a prototype that passed every step of this plan.

## Review Focus

- A giant shape with `-size` smaller than one chunk must still be one complete, valid value — `TestGiantTinySize` (Task 2).
- A write error during `deep`'s closing pass must stop `run` with that error, not hang — `TestTailWriteErrorStops` (Task 2).
- `-cap` smaller than one unit must still give valid, minimal values (and not a million records per chunk) — `TestCappedTinyCap` (Task 3).
- Corpus verdicts must hold at extreme `-null`/`-empty` rates, when every mutated record is all null or all empty — `TestCorpusExtremeRates` (Task 4).
- Combining mode flags that have no effect (`-cap` without `-shape`, `-schema` with `-corpus`, `-ndjson` with a giant shape) must be rejected, not silently ignored — `TestParseConfigRejects` (Task 5).

---

### Task 1: Pluggable source and two-pass pipeline

**Files:**
- Create: `source.go`
- Modify: `gen.go` (pipeline section and `main`)
- Test: `gen_test.go` (append)

**Interfaces:**
- Consumes: `builder`, `newBuilder(cfg config) *builder`, `(*builder).chunk(dst []byte, i int64) []byte` (value.go).
- Produces:
  - `type source interface { header() []byte; chunk(dst []byte, i int64) []byte; tailLen(body []byte) int64; tail(dst []byte, n, j int64) []byte; trailer() []byte; summary(n int64) string }`
  - `func newSource(cfg config) source` — later tasks add branches.
  - `func pipeline(cfg config, build func(s source, dst []byte, i int64) []byte, emit func(i int64, chunk []byte) (more bool, err error)) error`
  - `func run(w io.Writer, cfg config) (summary string, written int64, err error)` — the first result changes from a record count to a summary string.
  - The builder implements `source` for records.

- [ ] **Step 1: Add the records regression guard**

Append to `gen_test.go` (the hashes were recorded from the current code with Go 1.27.1):

```go

// Records output must stay byte-identical as new sources are added. The
// hashes were recorded before the source refactor; they hold for one Go
// toolchain (math/rand/v2 outputs may change between releases).
func TestRecordsBytesUnchanged(t *testing.T) {
	for _, c := range []struct{ schema, ndjson, sha string }{
		{"chaos", "false", "cd8601c086ad349498604d50c3f97f8a12d1e908c61e7649722fb14ebccbf2a5"},
		{"chaos", "true", "5692b2f4810af0c26acebb2443bbb742d57ad693ff566c0aab171622ea3d4af7"},
		{"typed", "false", "a605078ce2f19a3a9979ba351cb9e1a480d823a8fc12fe5cebaca4eea1ce981d"},
		{"typed", "true", "91f4ef404da704182191519850476a2ad02394853907b84efc9977e7ac12091e"},
	} {
		out := gen(t, "-size", "2MB", "-seed", "9", "-depth", "6", "-schema", c.schema, "-ndjson="+c.ndjson)
		if got := fmt.Sprintf("%x", sha256.Sum256(out)); got != c.sha {
			t.Errorf("%s ndjson=%s: sha256 %s, want %s", c.schema, c.ndjson, got, c.sha)
		}
	}
}
```

- [ ] **Step 2: Run it — it must PASS now**

Run: `go test -count=1 -run TestRecordsBytesUnchanged ./...`
Expected: `ok  	jsongen`. This test is a guard: it must keep passing through the refactor. If it fails here, stop — your toolchain differs from the one the hashes came from (`go version` must say go1.27.1).

- [ ] **Step 3: Create `source.go`**

```go
package main

import "fmt"

// source produces the output in pieces the pipeline can build in parallel.
// Each worker owns its own source; chunk and tail must be pure functions of
// the config and their indices so the output never depends on -workers.
type source interface {
	header() []byte                     // written first
	chunk(dst []byte, i int64) []byte   // body chunk i
	tailLen(body []byte) int64          // bytes pass 2 adds for this body chunk (0: none)
	tail(dst []byte, n, j int64) []byte // closing chunk j after n body chunks
	trailer() []byte                    // written last
	summary(n int64) string             // what n body chunks hold, for the done line
}

// newSource returns a fresh source for the configured mode.
func newSource(cfg config) source {
	return newBuilder(cfg)
}

// Records: the builder is its own source, with array framing unless -ndjson.

func (b *builder) header() []byte {
	if b.cfg.ndjson {
		return nil
	}
	return []byte("[\n")
}

func (b *builder) trailer() []byte {
	if b.cfg.ndjson {
		return nil
	}
	return []byte("\n]\n")
}

func (b *builder) tailLen([]byte) int64 { return 0 }

func (b *builder) tail(dst []byte, _, _ int64) []byte { return dst }

func (b *builder) summary(n int64) string {
	return fmt.Sprintf("%d records", n*int64(b.cfg.chunk))
}
```

- [ ] **Step 4: Replace the pipeline in `gen.go`**

Replace this exact block (from `type chunkOut struct` up to, not including, `// report prints`):

```go
type chunkOut struct {
	idx int64
	buf *[]byte
}

// startWorkers launches cfg.workers goroutines. Each takes a token, claims the
// next chunk index, builds that chunk into a pooled buffer and sends it on
// results, until stop is closed.
func startWorkers(cfg config, pool *sync.Pool, tokens <-chan struct{}, results chan<- chunkOut, stop <-chan struct{}) {
	var next atomic.Int64
	for range cfg.workers {
		go func() {
			b := newBuilder(cfg)
			for {
				select {
				case <-tokens:
				case <-stop:
					return
				}
				i := next.Add(1) - 1
				bp := pool.Get().(*[]byte)
				*bp = b.chunk((*bp)[:0], i)
				select {
				case results <- chunkOut{i, bp}:
				case <-stop:
					return
				}
			}
		}()
	}
}

// run writes the payload to w and reports how many records and bytes it wrote.
// Workers build chunks concurrently; at most 2*workers chunks are in flight and
// w receives them strictly in index order.
func run(w io.Writer, cfg config) (records, written int64, err error) {
	stop := make(chan struct{})
	defer close(stop) // releases workers on every return path

	tokens := make(chan struct{}, 2*cfg.workers)
	for range cap(tokens) {
		tokens <- struct{}{}
	}
	results := make(chan chunkOut)
	pool := sync.Pool{New: func() any { b := make([]byte, 0, 1<<20); return &b }}
	startWorkers(cfg, &pool, tokens, results, stop)

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
			report(cfg.progress, written, cfg.target, start)
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
```

with:

```go
type chunkOut struct {
	idx int64
	buf *[]byte
}

// startWorkers launches cfg.workers goroutines, each with its own source. Each
// takes a token, claims the next chunk index, builds that chunk into a pooled
// buffer and sends it on results, until stop is closed.
func startWorkers(cfg config, build func(s source, dst []byte, i int64) []byte, pool *sync.Pool, tokens <-chan struct{}, results chan<- chunkOut, stop <-chan struct{}) {
	var next atomic.Int64
	for range cfg.workers {
		go func() {
			s := newSource(cfg)
			for {
				select {
				case <-tokens:
				case <-stop:
					return
				}
				i := next.Add(1) - 1
				bp := pool.Get().(*[]byte)
				*bp = build(s, (*bp)[:0], i)
				select {
				case results <- chunkOut{i, bp}:
				case <-stop:
					return
				}
			}
		}()
	}
}

// pipeline builds chunks 0, 1, 2, ... with build on cfg.workers goroutines and
// hands them to emit strictly in index order, until emit reports no more or an
// error. At most 2*workers chunks are in flight.
func pipeline(cfg config, build func(s source, dst []byte, i int64) []byte, emit func(i int64, chunk []byte) (more bool, err error)) error {
	stop := make(chan struct{})
	defer close(stop) // releases workers on every return path

	tokens := make(chan struct{}, 2*cfg.workers)
	for range cap(tokens) {
		tokens <- struct{}{}
	}
	results := make(chan chunkOut)
	pool := sync.Pool{New: func() any { b := make([]byte, 0, 1<<20); return &b }}
	startWorkers(cfg, build, &pool, tokens, results, stop)

	pending := map[int64]*[]byte{}
	for idx := int64(0); ; idx++ {
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
		more, err := emit(idx, *bp)
		pool.Put(bp)
		tokens <- struct{}{}
		if err != nil || !more {
			return err
		}
	}
}

// run writes the payload to w: header, body chunks until the size target
// (counting any tail still to come), the tail chunks if the source has them,
// then the trailer. It returns a summary of what was written and the byte count.
func run(w io.Writer, cfg config) (summary string, written int64, err error) {
	src := newSource(cfg)
	start := time.Now()
	nextReport := int64(1e9)
	write := func(p []byte) error {
		n, err := w.Write(p)
		written += int64(n)
		if cfg.progress != nil && written >= nextReport {
			report(cfg.progress, written, cfg.target, start)
			for nextReport <= written {
				nextReport += 1e9
			}
		}
		return err
	}

	if err = write(src.header()); err != nil {
		return
	}
	var n, pendingTail int64
	err = pipeline(cfg, source.chunk, func(i int64, c []byte) (bool, error) {
		n = i + 1
		if err := write(c); err != nil {
			return false, err
		}
		pendingTail += src.tailLen(c)
		return written+pendingTail < cfg.target, nil
	})
	if err != nil {
		return
	}
	if pendingTail > 0 {
		tail := func(s source, dst []byte, j int64) []byte { return s.tail(dst, n, j) }
		err = pipeline(cfg, tail, func(j int64, c []byte) (bool, error) {
			return j+1 < n, write(c)
		})
		if err != nil {
			return
		}
	}
	err = write(src.trailer())
	return src.summary(n), written, err
}
```

- [ ] **Step 5: Update `main` for the summary string**

Replace:

```go
	records, written, err := run(os.Stdout, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "done: %d records, %d bytes in %s\n",
		records, written, time.Since(start).Round(time.Millisecond))
```

with:

```go
	summary, written, err := run(os.Stdout, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "done: %s, %d bytes in %s\n",
		summary, written, time.Since(start).Round(time.Millisecond))
```

- [ ] **Step 6: Verify**

Run: `gofmt -l . ; go vet ./... && go test -count=1 ./...` then `go test -count=1 -race ./...`
Expected: no gofmt output; `ok  	jsongen` both times (the race run takes about 15 s). `TestRecordsBytesUnchanged` still passes, so records output is unchanged.

- [ ] **Commit**

```bash
git add source.go gen.go gen_test.go
git commit -m "Drive the pipeline from a pluggable source

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Giant shapes (`-shape`)

**Files:**
- Create: `shape.go`, `shape_test.go`
- Modify: `value.go` (`strN` split into `strN` + `chars`), `gen.go` (config field, flag), `source.go` (`newSource`)

**Interfaces:**
- Consumes: `source`, `newSource`, `run`, `gen`/`cfgFor`/`failWriter` test helpers (gen_test.go), `builder.chaosValue`, `typedAlphabet`, `accented`.
- Produces:
  - `type shapeKind string` with `MarshalText`/`UnmarshalText`; constants `deep`, `wideObject`, `wideArray`, `longString`, `escapes`, `bigNum`; `shapes map[shapeKind]shapeDef`; `shapeNames() string`.
  - `type shapeSource struct { *builder; kind shapeKind; def shapeDef; scratch []byte }`, `newShape(cfg config) *shapeSource`.
  - `openers(run []byte) int`, `mirror(dst, run []byte) []byte`.
  - `(*builder).chars(alphabet []rune, n int)`, `(*builder).escape()`, `(*builder).uEscape(v int)` — reused by Task 4.
  - `config.shape shapeKind`.
  - Test helper `nesting(t, out []byte) int`.

- [ ] **Step 1: Write the failing tests**

Create `shape_test.go`:

```go
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
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

// A write error during deep's closing pass stops run with that error.
func TestTailWriteErrorStops(t *testing.T) {
	body := len(gen(t, "-shape", "deep", "-size", "3MB")) / 2
	_, written, err := run(&failWriter{left: body + 1000}, cfgFor(t, "-shape", "deep", "-size", "3MB", "-workers", "4"))
	if err == nil || err.Error() != "disk full" || written != int64(body+1000) {
		t.Fatalf("err = %v, written = %d; want disk full after %d bytes", err, written, body+1000)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./...`
Expected: FAIL — build error `undefined: openers`.

- [ ] **Step 3: Split `strN` in `value.go`**

Replace:

```go
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
```

with:

```go
// strN writes a quoted JSON string of n runes drawn from alphabet.
func (b *builder) strN(alphabet []rune, n int) {
	b.buf = append(b.buf, '"')
	b.chars(alphabet, n)
	b.buf = append(b.buf, '"')
}

// chars writes n runes drawn from alphabet, escaped for a JSON string body.
func (b *builder) chars(alphabet []rune, n int) {
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
}
```

- [ ] **Step 4: Add the config field and flag in `gen.go`**

Replace:

```go
	chunk    int
	progress io.Writer // nil: no progress output
```

with:

```go
	chunk    int
	shape    shapeKind // "": records
	progress io.Writer // nil: no progress output
```

and directly after the line `fs.IntVar(&cfg.chunk, "chunk", 1024, "records per chunk; changes output")` add:

```go
	fs.TextVar(&cfg.shape, "shape", shapeKind(""), "write one giant value of this shape: "+shapeNames())
```

- [ ] **Step 5: Route `-shape` in `source.go`**

Replace the `newSource` function with:

```go
func newSource(cfg config) source {
	if cfg.shape != "" {
		return newShape(cfg)
	}
	return newBuilder(cfg)
}
```

- [ ] **Step 6: Create `shape.go`**

```go
package main

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// shapeKind names an extreme value for stress-testing parsers.
type shapeKind string

const (
	deep       shapeKind = "deep"
	wideObject shapeKind = "wide-object"
	wideArray  shapeKind = "wide-array"
	longString shapeKind = "longstring"
	escapes    shapeKind = "escapes"
	bigNum     shapeKind = "bignum"
)

// shapeDef describes a value as open + units + close. A giant value spreads
// its units over chunks; unit idx is global, so commas and keys depend only
// on the index.
type shapeDef struct {
	open, close string
	perChunk    int64 // units per giant chunk
	unit        func(s *shapeSource, idx int64)
}

var shapes = map[shapeKind]shapeDef{
	deep:       {"", "", 1 << 18, (*shapeSource).opener},
	wideObject: {"{", "}", 1 << 14, (*shapeSource).member},
	wideArray:  {"[", "]", 1 << 14, (*shapeSource).element},
	longString: {`"`, `"`, 1 << 12, func(s *shapeSource, _ int64) { s.chars(typedAlphabet, 64) }},
	escapes:    {`"`, `"`, 1 << 17, func(s *shapeSource, _ int64) { s.escape() }},
	bigNum:     {"1", ".5e-7", 1 << 20, func(s *shapeSource, _ int64) { s.buf = append(s.buf, byte('0'+s.r.IntN(10))) }},
}

func shapeNames() string {
	names := make([]string, 0, len(shapes))
	for k := range shapes {
		names = append(names, string(k))
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

func (k shapeKind) MarshalText() ([]byte, error) { return []byte(k), nil }

func (k *shapeKind) UnmarshalText(text []byte) error {
	if _, ok := shapes[shapeKind(text)]; !ok {
		return fmt.Errorf("must be one of %s", shapeNames())
	}
	*k = shapeKind(text)
	return nil
}

// shapeSource writes one giant value of the configured shape.
type shapeSource struct {
	*builder
	kind    shapeKind
	def     shapeDef
	scratch []byte
}

func newShape(cfg config) *shapeSource {
	return &shapeSource{builder: newBuilder(cfg), kind: cfg.shape, def: shapes[cfg.shape]}
}

func (s *shapeSource) header() []byte  { return []byte(s.def.open) }
func (s *shapeSource) trailer() []byte { return []byte(s.def.close) }

func (s *shapeSource) summary(int64) string { return fmt.Sprintf("1 %s value", s.kind) }

// chunk writes units [i*perChunk, (i+1)*perChunk) of the giant value.
func (s *shapeSource) chunk(dst []byte, i int64) []byte {
	s.pcg.Seed(s.cfg.seed, uint64(i))
	s.buf = dst
	for idx := i * s.def.perChunk; idx < (i+1)*s.def.perChunk; idx++ {
		s.def.unit(s, idx)
	}
	return s.buf
}

// tailLen is the number of closers a deep body chunk needs: one per opener.
func (s *shapeSource) tailLen(body []byte) int64 {
	if s.kind != deep {
		return 0
	}
	return int64(openers(body))
}

// tail closes deep nesting: chunk j mirrors body chunk n-1-j, rebuilt from its
// seed, so no opener sequence is ever stored. The first tail chunk starts
// with the leaf value.
func (s *shapeSource) tail(dst []byte, n, j int64) []byte {
	s.scratch = s.chunk(s.scratch[:0], n-1-j)
	if j == 0 {
		dst = append(dst, '0')
	}
	return mirror(dst, s.scratch)
}

// openers counts the [ and { in a run of deep openers.
func openers(run []byte) int {
	n := 0
	for _, c := range run {
		if c == '[' || c == '{' {
			n++
		}
	}
	return n
}

// mirror appends the closers for a run of openers, innermost first.
func mirror(dst, run []byte) []byte {
	for k := len(run) - 1; k >= 0; k-- {
		switch run[k] {
		case '[':
			dst = append(dst, ']')
		case '{':
			dst = append(dst, '}')
		}
	}
	return dst
}

func (s *shapeSource) opener(int64) {
	if s.r.IntN(2) == 0 {
		s.buf = append(s.buf, '[')
	} else {
		s.buf = append(s.buf, `{"k":`...)
	}
}

// small writes a small chaos value: a scalar, null or an empty container.
func (s *shapeSource) small() { s.chaosValue(math.MaxInt) }

func (s *shapeSource) member(idx int64) {
	if idx > 0 {
		s.buf = append(s.buf, ',')
	}
	s.buf = append(s.buf, `"k`...)
	s.buf = strconv.AppendInt(s.buf, idx, 10)
	s.buf = append(s.buf, '"', ':')
	s.small()
}

func (s *shapeSource) element(idx int64) {
	if idx > 0 {
		s.buf = append(s.buf, ',')
	}
	s.small()
}

var (
	simpleEscapes = []string{`\"`, `\\`, `\/`, `\b`, `\f`, `\n`, `\r`, `\t`}
	rawMultibyte  = []rune(accented + "日本語한국" + "😀🚀")
)

// escape writes one escape-heavy unit: a simple escape, a \u escape of a
// non-surrogate BMP code point, a surrogate pair, or a raw multi-byte rune.
func (b *builder) escape() {
	switch b.r.IntN(4) {
	case 0:
		b.buf = append(b.buf, simpleEscapes[b.r.IntN(len(simpleEscapes))]...)
	case 1:
		v := b.r.IntN(0xF800) // skip the surrogate block D800–DFFF
		if v >= 0xD800 {
			v += 0x800
		}
		b.uEscape(v)
	case 2:
		b.uEscape(0xD800 + b.r.IntN(0x400))
		b.uEscape(0xDC00 + b.r.IntN(0x400))
	default:
		b.buf = append(b.buf, string(rawMultibyte[b.r.IntN(len(rawMultibyte))])...)
	}
}

func (b *builder) uEscape(v int) {
	const hex = "0123456789abcdef"
	b.buf = append(b.buf, '\\', 'u', hex[v>>12&0xf], hex[v>>8&0xf], hex[v>>4&0xf], hex[v&0xf])
}
```

- [ ] **Step 7: Verify**

Run: `gofmt -l . ; go vet ./... && go test -count=1 ./...`
Expected: no gofmt output, `ok  	jsongen`.

- [ ] **Commit**

```bash
git add value.go gen.go source.go shape.go shape_test.go
git commit -m "Add giant extreme shapes (-shape)

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Capped shapes (`-cap`)

**Files:**
- Modify: `value.go` (record framing helper), `gen.go` (field, flag), `source.go` (`newSource`), `shape.go` (insert `cappedSource`), `shape_test.go` (import + append)

**Interfaces:**
- Consumes: `shapeSource`, `shapes`, `mirror`, `parseSize`, test helpers.
- Produces:
  - `(*builder).records(dst []byte, i, n int64, rec func(id int64)) []byte` — array/NDJSON framing shared by records and capped shapes.
  - `const maxCap = 256e6`; `type cappedSource struct{ *shapeSource }`.
  - `config.cap int64` (0 = giant).

- [ ] **Step 1: Write the failing tests**

In `shape_test.go`, add `"math/big"` to the import block (after `"encoding/json"`), then append:

```go

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
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./...`
Expected: FAIL — `TestCappedShapes` reports `flag provided but not defined: -cap`.

- [ ] **Step 3: Extract record framing in `value.go`**

Replace:

```go
// chunk appends records [i*chunk, (i+1)*chunk) to dst, framed for the output
// mode. The result depends only on the config and i.
func (b *builder) chunk(dst []byte, i int64) []byte {
	b.pcg.Seed(b.cfg.seed, uint64(i))
	b.buf = dst
	n := int64(b.cfg.chunk)
	for id := i * n; id < (i+1)*n; id++ {
		if id > 0 && !b.cfg.ndjson {
			b.buf = append(b.buf, ',', '\n')
		}
		b.record(id)
		if b.cfg.ndjson {
			b.buf = append(b.buf, '\n')
		}
	}
	return b.buf
}
```

with:

```go
// chunk appends records [i*chunk, (i+1)*chunk) to dst, framed for the output
// mode. The result depends only on the config and i.
func (b *builder) chunk(dst []byte, i int64) []byte {
	b.pcg.Seed(b.cfg.seed, uint64(i))
	return b.records(dst, i, int64(b.cfg.chunk), b.record)
}

// records appends records [i*n, (i+1)*n) to dst, framed for the output mode:
// ",\n" before every record but the first in an array, "\n" after each in
// NDJSON. rec writes the record with the given id.
func (b *builder) records(dst []byte, i, n int64, rec func(id int64)) []byte {
	b.buf = dst
	for id := i * n; id < (i+1)*n; id++ {
		if id > 0 && !b.cfg.ndjson {
			b.buf = append(b.buf, ',', '\n')
		}
		rec(id)
		if b.cfg.ndjson {
			b.buf = append(b.buf, '\n')
		}
	}
	return b.buf
}
```

- [ ] **Step 4: Add the config field and flag in `gen.go`**

Replace:

```go
	chunk    int
	shape    shapeKind // "": records
	progress io.Writer // nil: no progress output
```

with:

```go
	chunk    int
	shape    shapeKind // "": records
	cap      int64     // with shape: bytes per capped value; 0: one giant value
	progress io.Writer // nil: no progress output
```

and directly after the `fs.TextVar(&cfg.shape, ...)` line add:

```go
	fs.Func("cap", "with -shape: write records each holding a value of about this size (at most 256MB)", func(s string) error {
		v, err := parseSize(s)
		if err != nil || v <= 0 || v > maxCap {
			return errors.New("want a size from 1B to 256MB")
		}
		cfg.cap = v
		return nil
	})
```

- [ ] **Step 5: Route capped shapes in `source.go`**

Replace the `newSource` function with:

```go
func newSource(cfg config) source {
	if cfg.shape != "" {
		if cfg.cap > 0 {
			return cappedSource{newShape(cfg)}
		}
		return newShape(cfg)
	}
	return newBuilder(cfg)
}
```

- [ ] **Step 6: Add `cappedSource` to `shape.go`**

Insert this block directly before the line `func (s *shapeSource) opener(int64) {`:

```go
// maxCap bounds -cap: capped chunks hold at least one value, so memory is
// about 2 x workers x cap.
const maxCap = 256e6

// cappedSource writes records {"id":N,"v":<value of about -cap bytes>}, each
// value built whole inside its record.
type cappedSource struct{ *shapeSource }

func (c cappedSource) header() []byte         { return c.builder.header() }
func (c cappedSource) trailer() []byte        { return c.builder.trailer() }
func (c cappedSource) tailLen([]byte) int64   { return 0 }
func (c cappedSource) perChunk() int64        { return min(1024, max(1, (1<<20)/c.cfg.cap)) }
func (c cappedSource) summary(n int64) string { return fmt.Sprintf("%d records", n*c.perChunk()) }

func (c cappedSource) chunk(dst []byte, i int64) []byte {
	c.pcg.Seed(c.cfg.seed, uint64(i))
	return c.records(dst, i, c.perChunk(), c.cappedRecord)
}

// cappedRecord appends units until the value, counting the closers deep
// still needs, reaches the cap.
func (c cappedSource) cappedRecord(id int64) {
	c.buf = append(c.buf, `{"id":`...)
	c.buf = strconv.AppendInt(c.buf, id, 10)
	c.buf = append(c.buf, `,"v":`...)
	c.buf = append(c.buf, c.def.open...)
	start := len(c.buf)
	budget := c.cfg.cap - int64(len(c.def.open)+len(c.def.close))
	for idx := int64(0); ; idx++ {
		size := int64(len(c.buf) - start)
		if c.kind == deep {
			size += idx + 1 // a closer per opener, plus the leaf
		}
		if size >= budget {
			break
		}
		c.def.unit(c.shapeSource, idx)
	}
	if c.kind == deep {
		run := c.buf[start:]
		c.buf = append(c.buf, '0')
		c.buf = mirror(c.buf, run)
	}
	c.buf = append(c.buf, c.def.close...)
	c.buf = append(c.buf, '}')
}
```

- [ ] **Step 7: Verify**

Run: `gofmt -l . ; go vet ./... && go test -count=1 ./...`
Expected: no gofmt output, `ok  	jsongen`.

- [ ] **Commit**

```bash
git add value.go gen.go source.go shape.go shape_test.go
git commit -m "Add capped extreme-value records (-cap)

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Corpus (`-corpus`)

**Files:**
- Create: `corpus.go`, `corpus_test.go`
- Modify: `gen.go` (field, flag), `source.go` (`newSource`)

**Interfaces:**
- Consumes: `builder` (`record`, `chaosValue`, `letters`, `escape`, `uEscape`), `schema`/`chaos`/`typed`, test helpers.
- Produces:
  - `type corpusCase struct { verdict byte; category string; gen func(c *corpusSource, doc []byte) []byte }`; `var catalogue []corpusCase` (30 categories: 7 y, 16 n, 7 i).
  - `acceptable(doc []byte) bool` — `json.Valid(doc) && utf8.Valid(doc)`.
  - `type corpusSource`, `newCorpus(cfg config) *corpusSource`; `scan(doc []byte) (structural, inside []int)`.
  - `config.corpus bool`.

- [ ] **Step 1: Write the failing tests**

Create `corpus_test.go`:

```go
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestCorpus(t *testing.T) {
	out := gen(t, "-corpus", "-size", "5MB", "-workers", "8")
	seen := map[string]bool{}
	for i, line := range bytes.Split(bytes.TrimSuffix(out, []byte("\n")), []byte("\n")) {
		var c struct {
			ID       int64
			Verdict  string
			Category string
			DocB64   string `json:"doc_b64"`
		}
		if err := json.Unmarshal(line, &c); err != nil || c.ID != int64(i) {
			t.Fatalf("line %d: %v (id %d)", i, err, c.ID)
		}
		doc, err := base64.StdEncoding.DecodeString(c.DocB64)
		if err != nil {
			t.Fatalf("case %d: bad base64: %v", c.ID, err)
		}
		if ok := acceptable(doc); (c.Verdict == "y" && !ok) || (c.Verdict == "n" && ok) {
			t.Fatalf("case %d %s/%s: reference check says acceptable=%v: %.80q", c.ID, c.Verdict, c.Category, ok, doc)
		}
		seen[c.Verdict+"/"+c.Category] = true
	}
	for _, cc := range catalogue {
		if !seen[string(cc.verdict)+"/"+cc.category] {
			t.Errorf("category %c/%s never generated", cc.verdict, cc.category)
		}
	}
}

func TestCorpusIgnoresWorkers(t *testing.T) {
	a := sha256.Sum256(gen(t, "-corpus", "-size", "1MB", "-workers", "1"))
	if b := sha256.Sum256(gen(t, "-corpus", "-size", "1MB", "-workers", "8")); a != b {
		t.Fatal("-workers changed the corpus")
	}
}

// The reference check must reject what json.Valid alone misses.
func TestAcceptable(t *testing.T) {
	for doc, want := range map[string]bool{
		`["ok"]`: true, "[\"\xff\"]": false, `[1,]`: false, `[01]`: false, "": false,
	} {
		if got := acceptable([]byte(doc)); got != want {
			t.Errorf("acceptable(%q) = %v, want %v", doc, got, want)
		}
	}
}

// Verdicts must hold even when every mutated record is all null or all empty.
func TestCorpusExtremeRates(t *testing.T) {
	for _, rates := range [][]string{{"-null", "1", "-empty", "0"}, {"-null", "0", "-empty", "1"}} {
		out := gen(t, append([]string{"-corpus", "-size", "300KB"}, rates...)...)
		for _, line := range bytes.Split(bytes.TrimSuffix(out, []byte("\n")), []byte("\n")) {
			var c struct {
				Verdict string
				DocB64  string `json:"doc_b64"`
			}
			if json.Unmarshal(line, &c) != nil {
				t.Fatalf("%v: bad line %q", rates, line)
			}
			doc, _ := base64.StdEncoding.DecodeString(c.DocB64)
			if ok := acceptable(doc); (c.Verdict == "y" && !ok) || (c.Verdict == "n" && ok) {
				t.Fatalf("%v: %s case, acceptable=%v: %.80q", rates, c.Verdict, ok, doc)
			}
		}
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./...`
Expected: FAIL — build error `undefined: acceptable` (and `catalogue`).

- [ ] **Step 3: Add the config field and flag in `gen.go`**

Replace:

```go
	chunk    int
	shape    shapeKind // "": records
	cap      int64     // with shape: bytes per capped value; 0: one giant value
	progress io.Writer // nil: no progress output
```

with:

```go
	chunk    int
	shape    shapeKind // "": records
	cap      int64     // with shape: bytes per capped value; 0: one giant value
	corpus   bool      // labelled test cases instead of records
	progress io.Writer // nil: no progress output
```

and directly after the closing `})` of the `fs.Func("cap", ...)` call add:

```go
	fs.BoolVar(&cfg.corpus, "corpus", false, "write labelled y/n/i parser test cases as NDJSON (base64 documents)")
```

- [ ] **Step 4: Route `-corpus` in `source.go`**

Replace the `newSource` function with:

```go
func newSource(cfg config) source {
	if cfg.corpus {
		return newCorpus(cfg)
	}
	if cfg.shape != "" {
		if cfg.cap > 0 {
			return cappedSource{newShape(cfg)}
		}
		return newShape(cfg)
	}
	return newBuilder(cfg)
}
```

- [ ] **Step 5: Create `corpus.go`**

```go
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const casesPerChunk = 256

// corpusCase is one category of parser test case. verdict follows RFC 8259
// in JSONTestSuite style: y must be accepted, n must be rejected, i is
// implementation-defined.
type corpusCase struct {
	verdict  byte
	category string
	gen      func(c *corpusSource, doc []byte) []byte
}

var catalogue = []corpusCase{
	// y: must accept
	{'y', "top_level_value", func(c *corpusSource, doc []byte) []byte { return c.with(doc, func() { c.chaosValue(math.MaxInt) }) }},
	{'y', "whitespace", (*corpusSource).whitespace},
	{'y', "escapes", (*corpusSource).validEscapes},
	{'y', "numbers", func(c *corpusSource, doc []byte) []byte { return c.list(doc, validNumbers) }},
	{'y', "unicode", (*corpusSource).unicode},
	{'y', "empty_containers", func(c *corpusSource, doc []byte) []byte { return c.with(doc, func() { c.empties(4) }) }},
	{'y', "record", (*corpusSource).valid},

	// n: must reject. Most mutate a valid record, so the defect sits in realistic content.
	{'n', "trailing_comma", func(c *corpusSource, doc []byte) []byte { return c.atStructural(doc, "]}", insert(0, ",")) }},
	{'n', "leading_comma", func(c *corpusSource, doc []byte) []byte { return c.atStructural(doc, "[{", insert(1, ",")) }},
	{'n', "missing_comma", func(c *corpusSource, doc []byte) []byte { return c.atStructural(doc, ",", remove) }},
	{'n', "missing_colon", func(c *corpusSource, doc []byte) []byte { return c.atStructural(doc, ":", remove) }},
	{'n', "unclosed", func(c *corpusSource, doc []byte) []byte { d := c.valid(doc); return d[:len(d)-1] }},
	{'n', "extra_close", func(c *corpusSource, doc []byte) []byte { return append(c.valid(doc), c.pick([]string{"]", "}"})...) }},
	{'n', "trailing_garbage", func(c *corpusSource, doc []byte) []byte {
		return append(c.valid(doc), c.pick([]string{" x", " 1", ",{}", `"a"`})...)
	}},
	{'n', "empty_document", func(c *corpusSource, doc []byte) []byte { return append(doc, c.pick([]string{"", " ", "\n\t "})...) }},
	{'n', "single_quote", func(c *corpusSource, doc []byte) []byte {
		return c.atStructural(doc, `"`, func(d []byte, p int) []byte { d[p] = '\''; return d })
	}},
	{'n', "unquoted_key", func(c *corpusSource, doc []byte) []byte {
		return c.with(append(doc, '{'), func() { c.letters(1 + c.r.IntN(8)); c.buf = append(c.buf, ':', '1', '}') })
	}},
	{'n', "comment", func(c *corpusSource, doc []byte) []byte {
		return c.atStructural(doc, "[{,:", insert(1, c.pick([]string{"/*c*/", "//c\n"})))
	}},
	{'n', "bad_escape", func(c *corpusSource, doc []byte) []byte {
		return c.insideString(doc, c.pick([]string{`\x41`, `\a`, `\u12g4`, `\U0041`, `\'`}))
	}},
	{'n', "raw_control", func(c *corpusSource, doc []byte) []byte { return c.insideString(doc, string(rune(c.r.IntN(0x20)))) }},
	{'n', "bad_number", func(c *corpusSource, doc []byte) []byte {
		return c.list(doc, []string{"01", "-01", "00", ".5", "1.", "+1", "-", "1e", "1e+", "0x10", "1.e2", "NaN", "Infinity", "-Infinity"})
	}},
	{'n', "bad_literal", func(c *corpusSource, doc []byte) []byte {
		return c.list(doc, []string{"tru", "nul", "fals", "True", "NULL", "undefined"})
	}},
	{'n', "invalid_utf8", func(c *corpusSource, doc []byte) []byte {
		return c.insideString(doc, c.pick([]string{"\x80", "\xc0\xaf", "\xe2\x82", "\xed\xa0\x80", "\xf4\x90\x80\x80", "\xff"}))
	}},

	// i: implementation-defined (RFC 8259 leaves the outcome to the parser)
	{'i', "duplicate_keys", func(c *corpusSource, doc []byte) []byte {
		doc = c.valid(doc) // starts with {"id":
		return slices.Insert(doc, 1, []byte(`"id":`+strconv.Itoa(c.r.IntN(1000))+",")...)
	}},
	{'i', "lone_surrogate", func(c *corpusSource, doc []byte) []byte {
		return c.list(doc, []string{`"\ud800"`, `"\udbff"`, `"\udc00"`, `"\udc00\ud800"`, `"\ud800x"`, `"\ud800\n"`})
	}},
	{'i', "huge_exponent", func(c *corpusSource, doc []byte) []byte {
		return c.list(doc, []string{"1e400", "-1e400", "1e-400", "123e99999", "1E+999"})
	}},
	{'i', "big_integer", func(c *corpusSource, doc []byte) []byte { return c.number(doc, 21+c.r.IntN(20)) }},
	{'i', "long_number", func(c *corpusSource, doc []byte) []byte { return c.number(doc, 1000) }},
	{'i', "bom", func(c *corpusSource, doc []byte) []byte { return c.valid(append(doc, "\xef\xbb\xbf"...)) }},
	{'i', "deep_nesting", func(c *corpusSource, doc []byte) []byte {
		k := 10000 + c.r.IntN(2000)
		return append(append(doc, strings.Repeat("[", k)...), strings.Repeat("]", k)...)
	}},
}

var validNumbers = []string{
	"0", "-0", "0.0", "-0.0", "0e0", "0E+0", "1E+2", "1e-2", "2.5E-3", "-123.456e-7",
	"1.7976931348623157e308", "5e-324", "9223372036854775807", "-9223372036854775808",
}

// acceptable is the reference check for y and n verdicts: strict RFC 8259
// syntax (json.Valid) plus valid UTF-8, which json.Valid does not check
// inside strings.
func acceptable(doc []byte) bool { return json.Valid(doc) && utf8.Valid(doc) }

// corpusSource writes NDJSON test cases {"id","verdict","category","doc_b64"}.
type corpusSource struct {
	*builder
	doc []byte
}

func newCorpus(cfg config) *corpusSource { return &corpusSource{builder: newBuilder(cfg)} }

func (c *corpusSource) header() []byte                     { return nil }
func (c *corpusSource) trailer() []byte                    { return nil }
func (c *corpusSource) tailLen([]byte) int64               { return 0 }
func (c *corpusSource) tail(dst []byte, _, _ int64) []byte { return dst }
func (c *corpusSource) summary(n int64) string             { return fmt.Sprintf("%d cases", n*casesPerChunk) }

func (c *corpusSource) chunk(dst []byte, i int64) []byte {
	c.pcg.Seed(c.cfg.seed, uint64(i))
	for id := i * casesPerChunk; id < (i+1)*casesPerChunk; id++ {
		dst = c.oneCase(dst, id)
	}
	return dst
}

// oneCase draws categories until one yields a document whose y/n verdict the
// reference check confirms (a mutation can land somewhere harmless), then
// appends its NDJSON line. Redraws come from the same seeded stream, so the
// output stays deterministic.
func (c *corpusSource) oneCase(dst []byte, id int64) []byte {
	var cc corpusCase
	for {
		cc = catalogue[c.r.IntN(len(catalogue))]
		c.doc = cc.gen(c, c.doc[:0])
		if cc.verdict == 'i' || acceptable(c.doc) == (cc.verdict == 'y') {
			break
		}
	}
	dst = append(dst, `{"id":`...)
	dst = strconv.AppendInt(dst, id, 10)
	dst = append(dst, `,"verdict":"`...)
	dst = append(dst, cc.verdict)
	dst = append(dst, `","category":"`...)
	dst = append(dst, cc.category...)
	dst = append(dst, `","doc_b64":"`...)
	dst = base64.StdEncoding.AppendEncode(dst, c.doc)
	return append(dst, "\"}\n"...)
}

// with runs write against the builder with doc as its buffer and returns the result.
func (c *corpusSource) with(doc []byte, write func()) []byte {
	c.buf = doc
	write()
	return c.buf
}

// valid appends a small valid record, chaos or typed.
func (c *corpusSource) valid(doc []byte) []byte {
	c.cfg.schema = []schema{chaos, typed}[c.r.IntN(2)]
	return c.with(doc, func() { c.record(int64(c.r.IntN(1000))) })
}

func (c *corpusSource) pick(options []string) string { return options[c.r.IntN(len(options))] }

// list appends [a,b,...] of one to four picks from options.
func (c *corpusSource) list(doc []byte, options []string) []byte {
	doc = append(doc, '[')
	for i := range 1 + c.r.IntN(4) {
		if i > 0 {
			doc = append(doc, ',')
		}
		doc = append(doc, c.pick(options)...)
	}
	return append(doc, ']')
}

// number appends [d...] with n digits, the first non-zero.
func (c *corpusSource) number(doc []byte, n int) []byte {
	doc = append(doc, '[', byte('1'+c.r.IntN(9)))
	for range n - 1 {
		doc = append(doc, byte('0'+c.r.IntN(10)))
	}
	return append(doc, ']')
}

// empties writes a random nest of empty arrays and objects.
func (c *corpusSource) empties(depth int) {
	switch k := c.r.IntN(4); {
	case depth == 0 || k == 0:
		c.buf = append(c.buf, "[]"...)
	case k == 1:
		c.buf = append(c.buf, "{}"...)
	case k == 2:
		c.buf = append(c.buf, '[')
		for i := range 1 + c.r.IntN(3) {
			if i > 0 {
				c.buf = append(c.buf, ',')
			}
			c.empties(depth - 1)
		}
		c.buf = append(c.buf, ']')
	default:
		c.buf = append(c.buf, `{"a":`...)
		c.empties(depth - 1)
		c.buf = append(c.buf, '}')
	}
}

// whitespace appends a valid record with runs of all four JSON whitespace
// characters around its structural tokens and the whole document.
func (c *corpusSource) whitespace(doc []byte) []byte {
	start := len(doc)
	doc = c.valid(doc)
	tokens := c.structuralIn(doc[start:], "{}[],:")
	for k := len(tokens) - 1; k >= 0; k-- {
		if c.r.IntN(2) == 0 {
			doc = slices.Insert(doc, start+tokens[k]+1, c.ws()...)
		}
	}
	doc = slices.Insert(doc, start, c.ws()...)
	return append(doc, c.ws()...)
}

func (c *corpusSource) ws() []byte {
	w := make([]byte, 1+c.r.IntN(3))
	for k := range w {
		w[k] = " \t\n\r"[c.r.IntN(4)]
	}
	return w
}

// validEscapes appends strings of every escape kind, always including the
// NUL escape and an escaped solidus.
func (c *corpusSource) validEscapes(doc []byte) []byte {
	return c.with(append(doc, `["`...), func() {
		c.uEscape(0)
		c.buf = append(c.buf, `\/`...)
		for range 1 + c.r.IntN(20) {
			c.escape()
		}
		c.buf = append(c.buf, `"]`...)
	})
}

// unicode appends a string of raw 4-byte UTF-8 and noncharacters (U+FFFF,
// U+FDD0, U+10FFFF), plus a BOM inside the string, where it is ordinary.
var unicodeRunes = []rune{0x1F600, 0xFFFF, 0xFDD0, 0x10FFFF, 0xFEFF, 0x65E5}

func (c *corpusSource) unicode(doc []byte) []byte {
	doc = append(doc, `["`...)
	for range 1 + c.r.IntN(10) {
		doc = utf8.AppendRune(doc, unicodeRunes[c.r.IntN(len(unicodeRunes))])
	}
	return append(doc, `"]`...)
}

// atStructural appends a valid record and applies edit at one random byte
// from set outside its strings (p is an offset into the whole doc). With no
// such byte the record is returned unchanged, and the verdict check redraws.
func (c *corpusSource) atStructural(doc []byte, set string, edit func(doc []byte, p int) []byte) []byte {
	start := len(doc)
	doc = c.valid(doc)
	pos := c.structuralIn(doc[start:], set)
	if len(pos) == 0 {
		return doc
	}
	return edit(doc, start+pos[c.r.IntN(len(pos))])
}

// insert returns an edit that inserts s at p+delta.
func insert(delta int, s string) func([]byte, int) []byte {
	return func(doc []byte, p int) []byte { return slices.Insert(doc, p+delta, []byte(s)...) }
}

// remove is an edit that deletes the byte at p.
func remove(doc []byte, p int) []byte { return slices.Delete(doc, p, p+1) }

// structuralIn returns the offsets of bytes from set outside strings; a
// string-opening quote counts as structural.
func (c *corpusSource) structuralIn(doc []byte, set string) []int {
	structural, _ := scan(doc)
	var pos []int
	for _, p := range structural {
		if strings.IndexByte(set, doc[p]) >= 0 {
			pos = append(pos, p)
		}
	}
	return pos
}

// insideString appends a valid record with s inserted at a rune boundary
// inside one of its strings.
func (c *corpusSource) insideString(doc []byte, s string) []byte {
	start := len(doc)
	doc = c.valid(doc)
	_, inside := scan(doc[start:])
	if len(inside) == 0 {
		return doc
	}
	return slices.Insert(doc, start+inside[c.r.IntN(len(inside))], []byte(s)...)
}

// scan returns the offsets of structural bytes ({}[],: and string-opening
// quotes) and of insertion points strictly inside strings: rune starts that
// do not split an escape sequence.
func scan(doc []byte) (structural, inside []int) {
	inString, escaped := false, false
	for p, ch := range doc {
		switch {
		case !inString:
			if strings.IndexByte(`{}[],:"`, ch) >= 0 {
				structural = append(structural, p)
			}
			inString = ch == '"'
		case escaped:
			escaped = false
		case ch == '\\':
			escaped = true
			inside = append(inside, p)
		case ch == '"':
			inside = append(inside, p)
			inString = false
		case utf8.RuneStart(ch):
			inside = append(inside, p)
		}
	}
	return structural, inside
}
```

- [ ] **Step 6: Verify**

Run: `gofmt -l . ; go vet ./... && go test -count=1 ./...`
Expected: no gofmt output, `ok  	jsongen`.

- [ ] **Commit**

```bash
git add gen.go source.go corpus.go corpus_test.go
git commit -m "Add labelled parser test corpus (-corpus)

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Reject flags that have no effect; usage examples

**Files:**
- Modify: `gen.go` (`parseConfig`, `validate`, `usage`), `gen_test.go`

**Interfaces:**
- Consumes: `config` with `shape`, `cap`, `corpus`.
- Produces: `func (c config) validate(set map[string]bool) error` — `set` holds the names of explicitly given flags (from `fs.Visit`).

- [ ] **Step 1: Write the failing tests**

In `gen_test.go`, replace:

```go
		{"-workers", "abc"}, {"-bogus"},
	} {
```

with:

```go
		{"-workers", "abc"}, {"-bogus"},
		{"-shape", "nope"}, {"-shape", "deep", "-corpus"}, {"-cap", "1MB"},
		{"-shape", "deep", "-cap", "0"}, {"-shape", "deep", "-cap", "300MB"},
		{"-shape", "deep", "-ndjson"}, {"-corpus", "-schema", "typed"},
		{"-shape", "deep", "-depth", "3"}, {"-corpus", "-chunk", "5"},
	} {
```

and insert directly before `func TestParseConfigFlags`:

```go
func TestParseConfigModes(t *testing.T) {
	for _, args := range [][]string{
		{"-shape", "deep"}, {"-shape", "bignum", "-cap", "1MB", "-ndjson"},
		{"-corpus"}, {"-corpus", "-ndjson", "-null", "0.5"},
	} {
		if _, err := parseConfig(args); err != nil {
			t.Errorf("parseConfig(%q): %v", args, err)
		}
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./...`
Expected: FAIL — `TestParseConfigRejects` reports `parseConfig(["-shape" "deep" "-corpus"]) succeeded, want error` and similar lines for `-cap 1MB`, `-shape deep -ndjson`, `-corpus -schema typed`, `-shape deep -depth 3`, `-corpus -chunk 5`.

- [ ] **Step 3: Collect explicitly set flags in `parseConfig`**

Replace:

```go
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err := cfg.validate(); err != nil {
```

with:

```go
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if err := cfg.validate(set); err != nil {
```

- [ ] **Step 4: Add the mode rules to `validate`**

Replace:

```go
func (c config) validate() error {
	switch {
```

with:

```go
// validate checks flag values; set names the flags given explicitly, so
// flags that have no effect in the chosen mode are rejected, not ignored.
func (c config) validate(set map[string]bool) error {
	switch {
	case c.shape != "" && c.corpus:
		return errors.New("-shape and -corpus cannot be combined")
	case set["cap"] && c.shape == "":
		return errors.New("-cap needs -shape")
	case c.shape != "" && c.cap == 0 && c.ndjson:
		return errors.New("-ndjson needs records, but a giant -shape is one document (add -cap)")
	case (c.shape != "" || c.corpus) && (set["schema"] || set["depth"] || set["chunk"]):
		return errors.New("-schema, -depth and -chunk only apply to records")
```

(The existing cases for `-null`/`-empty`, `-depth`, `-workers` and `-chunk` follow unchanged.)

- [ ] **Step 5: Add usage examples**

In `usage`, replace the example line `  jsongen -size 50GB -schema typed -ndjson | loader  # stable types per key` (and its newline) with:

```text
  jsongen -size 50GB -schema typed -ndjson | loader  # stable types per key
  jsongen -shape deep -size 1GB | parser             # one giant extreme value
  jsongen -shape escapes -cap 10MB -ndjson           # records of 10 MB extreme values
  jsongen -corpus -size 100MB                        # labelled y/n/i parser test cases
```

- [ ] **Step 6: Verify**

Run: `gofmt -l . ; go vet ./... && go test -count=1 ./...` then `go test -count=1 -race ./...`
Expected: no gofmt output; `ok  	jsongen` both times.

- [ ] **Commit**

```bash
git add gen.go gen_test.go
git commit -m "Reject mode flags that have no effect; add usage examples

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: Docs and end-to-end checks

No Go changes unless a check fails. These checks probe parsers; report what they find, do not "fix" the parsers.

**Files:**
- Modify: `README.md`, `Makefile`, `AGENTS.md`, `docs/superpowers/specs/2026-10-07-huge-json-generator-design.md`

**Interfaces:**
- Consumes: the CLI from Tasks 1–5.
- Produces: docs; measured results reported to the user.

- [ ] **Step 1: README**

Insert directly before the line `## Determinism`:

````markdown
## Parser stress

Three more modes produce input that breaks parsers rather than loads pipelines.

| Mode | Example | Output |
|---|---|---|
| Giant shape | `jsongen -shape deep -size 1GB` | one valid document whose single value fills `-size` |
| Capped shape | `jsongen -shape escapes -cap 10MB -size 1GB -ndjson` | records `{"id":N,"v":...}`, each value about `-cap` bytes |
| Corpus | `jsongen -corpus -size 100MB` | NDJSON test cases `{"id","verdict","category","doc_b64"}` |

| `-shape` | Value | Finds |
|---|---|---|
| `deep` | nested `[` and `{"k":` around a `0` leaf | stack overflow, recursion limits |
| `wide-object` | `{"k0":v,"k1":v,...}` | hash-map growth, out-of-memory |
| `wide-array` | `[v,v,...]` | array growth, out-of-memory |
| `longstring` | one string of ASCII and 2–4-byte UTF-8 | buffer growth, UTF-8 validation speed |
| `escapes` | one string of only escapes, surrogate pairs and raw multi-byte UTF-8 | escape-decoding slow paths |
| `bignum` | `1` + digits + `.5e-7` | big-number parsing, float conversion |

Corpus verdicts follow RFC 8259 in the JSONTestSuite convention: `y` must be
accepted, `n` must be rejected, `i` is implementation-defined. Every `y` and `n`
document is checked against strict RFC 8259 syntax plus UTF-8 validation while
it is generated. Filter with `jq`, for example
`jq -c 'select(.verdict=="n")'`, and decode `doc_b64` before feeding a parser.

Giant values stream at 1–2 GB/s in under 100 MB of memory. Capped chunks hold at
least one value, so memory grows to about 2 × workers × `-cap` (at most 256 MB
per value). `-schema`, `-depth` and `-chunk` apply only to records.
````

- [ ] **Step 2: Makefile**

Change the `.PHONY` line to include `shape corpus` after `bench`, and insert directly before the line `# ── Development` (recipe lines start with a tab):

```make
shape: build ## Write one extreme value to out/ (usage: make shape shape=deep [size=1GB] [cap=10MB])
	@mkdir -p $(OUT_DIR)
	$(BIN) -shape "$(shape)" -size "$(size)" $(if $(cap),-cap "$(cap)" -ndjson) $(args) > $(OUT_DIR)/shape-$(shape)-$(size).$(if $(cap),ndjson,json)

corpus: build ## Write labelled parser test cases to out/ (usage: make corpus [size=100MB])
	@mkdir -p $(OUT_DIR)
	$(BIN) -corpus -size "$(size)" $(args) > $(OUT_DIR)/corpus-$(size).ndjson
```

- [ ] **Step 3: AGENTS.md**

In the Layout table, after the `typed.go` row add:

````markdown
| `source.go` | `source` interface (header, body chunks, optional tail, trailer) and the records source |
| `shape.go` | `-shape`: giant values (`shapeSource`, `deep` mirrored tail) and capped records (`cappedSource`) |
| `corpus.go` | `-corpus`: the y/n/i `catalogue`, mutations of valid records, the `acceptable` reference check |
````

and insert directly before `## Gotchas`:

````markdown
- **Every mode is a `source`.** `chunk` and `tail` must be pure functions of the
  config and their indices. `run` writes the body until the target (counting
  pending tail bytes), then the tail (`deep` closers, mirrored from regenerated
  body chunks), then the trailer.
- **Never write `\u` followed by four hex digits in Go source.** Build JSON
  escapes with `uEscape` and runes with numeric literals (`0xFEFF`); generated
  edits have turned such escapes into raw characters before.
````

- [ ] **Step 4: Main spec pointer**

In `docs/superpowers/specs/2026-10-07-huge-json-generator-design.md`, insert directly before the paragraph starting `stdout carries only JSON.`:

````markdown
Parser-stress modes (`-shape`, `-cap`, `-corpus`): see
`2026-10-07-parser-stress-design.md`.
````

- [ ] **Step 5: Run every new Makefile target**

```bash
make check && make shape shape=deep size=2MB && make shape shape=escapes size=2MB cap=100KB && make corpus size=1MB && ls out && make clean
```
Expected: checks pass; `out/` lists `corpus-1MB.ndjson`, `shape-deep-2MB.json`, `shape-escapes-2MB.ndjson`; clean removes it.

- [ ] **Step 6: Scale and memory at 1 GB**

```bash
go build -o /private/tmp/claude-501/jsongen . && for k in deep longstring escapes wide-object bignum; do /usr/bin/time -l /private/tmp/claude-501/jsongen -shape $k -size 1GB 2>/private/tmp/claude-501/t.txt >/dev/null; printf "%-12s %s  maxRSS=%s\n" $k "$(grep '^done' /private/tmp/claude-501/t.txt)" "$(awk '/maximum resident/ {printf "%.0fMB", $1/1e6}' /private/tmp/claude-501/t.txt)"; done
```
Expected (prototype, 14 cores): each about 0.5–0.9 s for 1 GB, max RSS under 100 MB.

- [ ] **Step 7: Show a real parser failure (spec success criterion 5)**

```bash
/private/tmp/claude-501/jsongen -shape deep -size 100MB 2>/dev/null | python3 -c 'import json,sys; sys.setrecursionlimit(10**6); json.load(sys.stdin)'; echo "exit=$?"
```
Expected: Python raises `RecursionError: maximum recursion depth exceeded`, `exit=1` (the prototype already failed at 3 MB).

- [ ] **Step 8: Probe jq with the corpus `n` cases**

```bash
/private/tmp/claude-501/jsongen -corpus -size 20MB 2>/dev/null > /private/tmp/claude-501/corpus.ndjson && python3 -c '
import json,base64,subprocess,collections
acc,tot=collections.Counter(),collections.Counter()
for l in open("/private/tmp/claude-501/corpus.ndjson"):
    r=json.loads(l)
    if r["verdict"]!="n": continue
    tot[r["category"]]+=1
    if subprocess.run(["jq","."],input=base64.b64decode(r["doc_b64"]),capture_output=True).returncode==0: acc[r["category"]]+=1
    if sum(tot.values())>=600: break
print({k:f"{acc[k]}/{tot[k]}" for k in sorted(tot) if acc[k]}, "wrongly accepted:", sum(acc.values()), "of", sum(tot.values()))'
```
Expected (prototype): jq accepts some `n` cases — all `empty_document` and `invalid_utf8`, about half of `trailing_garbage`, some `bad_number`. Several are jq design choices (it reads a stream of values; it accepts `nan`); report them as findings, with that caveat.

- [ ] **Step 9: Clean up and commit**

```bash
rm -f /private/tmp/claude-501/jsongen /private/tmp/claude-501/t.txt /private/tmp/claude-501/corpus.ndjson
git add README.md Makefile AGENTS.md docs/superpowers/specs/2026-10-07-huge-json-generator-design.md
git commit -m "Document parser-stress modes; add shape and corpus make targets

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
