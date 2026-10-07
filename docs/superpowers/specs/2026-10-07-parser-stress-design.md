# jsongen parser-stress modes: design

Date: 2026-10-07 · Status: approved in chat, pending written-spec review
Builds on: `2026-10-07-huge-json-generator-design.md` (records mode, chunk pipeline)

## Goal

Make jsongen find JSON parser bugs of four kinds:

1. **Crashes and resource blowups:** stack overflow, OOM, quadratic time.
2. **Performance cliffs:** inputs that push a fast parser onto its slow path.
3. **Conformance:** invalid JSON that must be rejected.
4. **Edge-case correctness:** valid or implementation-defined inputs that
   parsers commonly get wrong.

Kinds 1–2 are served by **shapes** (extreme but valid values). Kinds 3–4 are
served by a **corpus** (many small labelled documents). Both reuse the existing
ordered parallel pipeline, so every mode keeps the current guarantees.

## Success criteria

1. Records mode (no new flags) produces byte-identical output to today.
2. Every mode's bytes depend only on the flags, never on `-workers`; memory
   stays bounded independently of `-size` (and of a single value's size).
3. Giant shapes are valid JSON at any size, including a single 10+ GB value.
4. Every corpus `y` document passes and every `n` document fails the reference
   check `json.Valid(doc) && utf8.Valid(doc)`.
5. A reproducible run (fixed seed) demonstrates at least one real parser
   failure (e.g. Python's recursion limit on `-shape deep`).

## CLI

Backward compatible: without new flags, jsongen behaves exactly as today.

| Invocation | Output |
|---|---|
| `jsongen [-schema chaos\|typed] [-ndjson]` | records (unchanged) |
| `jsongen -shape NAME -size 10GB` | **one** giant valid document of that shape |
| `jsongen -shape NAME -cap 10MB -size 10GB [-ndjson]` | records `{"id":N,"v":<extreme value ≈ cap bytes>}` until `-size` |
| `jsongen -corpus -size 1GB` | NDJSON test cases (see Corpus) |

- `-shape`: `deep`, `wide-object`, `wide-array`, `longstring`, `escapes`, `bignum`.
- `-cap`: byte size of each extreme value, any shape (`deep` 10MB ≈ 2.5M levels: openers average 3 bytes plus a 1-byte closer);
  range (0, 256MB].
- `-size`, `-seed`, `-workers` apply to every mode. `-null`/`-empty` apply to
  records, to the small values inside `wide-*`, and to the valid records the
  corpus mutates.

New errors (one line on stderr with ` (see -h)`, exit 2):

- unknown `-shape`
- `-shape` together with `-corpus`
- `-cap` without `-shape`, or `-cap` ≤ 0 or > 256MB
- `-ndjson` with a giant shape (single document)
- `-schema`, `-depth` or `-chunk` set explicitly together with `-shape` or `-corpus`
  (they have no effect there)

## Engine

`run` drives a `source` instead of the record builder directly:

```go
type source interface {
	header() []byte                      // written first
	chunk(dst []byte, i int64) []byte    // pure function of (config, i)
	tailLen(body []byte) int64           // bytes pass 2 will add for this body chunk (0: no tail)
	tail(dst []byte, n, j int64) []byte  // j-th closing chunk after n body chunks
	trailer() []byte                     // written last
}
```

- **Pass 1 (body):** unchanged pipeline (2N tokens, ordered writer). Stops after
  the first chunk where `written + pendingTail ≥ target`, where `pendingTail`
  sums `tailLen` of every written chunk. Gives `n` body chunks.
- **Pass 2 (tail):** only if `pendingTail > 0`: the same pipeline builds
  `tail(n, j)` for `j = 0…n−1` in order, stopping by count, not size.
- Then `trailer()`.
- `-chunk` keeps its meaning for records; shapes and corpus use fixed internal
  units per chunk (constants, part of the determinism key).

Sources:

| Source | header | chunk `i` | tail | trailer |
|---|---|---|---|---|
| records | `[\n` / none | `-chunk` records (today's builder) | none | `\n]\n` / none |
| giant shape | opening token | units `[i·U, (i+1)·U)` | `deep` only | closing token |
| capped shape | `[\n` / none | `max(1, 1MiB / cap)` records, at most 1024 | none | `\n]\n` / none |
| corpus | none | 256 cases | none | none |

## Shapes

Every shape has a giant form (one value fills `-size`) and a capped form (each
record's `v` is one value of about `-cap` bytes, built whole in its chunk).

| Shape | Value | Targets | Unit `U` per giant chunk |
|---|---|---|---|
| `deep` | openers, each a seeded `[` or `{"k":`, around leaf `0`, closed in mirror order | stack overflow, recursion limits | 2^18 openers |
| `wide-object` | `{"k<idx>":v,…}`: unique sequential keys, small values (chaos scalars, null, empties) | hash-map growth, OOM | 2^14 members |
| `wide-array` | `[v,…]`, same small values | array growth, OOM | 2^14 elements |
| `longstring` | `"…"` from the typed alphabet (ASCII + 2/3/4-byte UTF-8) | buffer growth, OOM, UTF-8 validation | 2^18 runes |
| `escapes` | `"…"` of only `\b \f \n \r \t \" \\ \/`, `\u00XX`/`\uXXXX`, surrogate pairs (`😀`), raw multi-byte UTF-8 | escape-decoding slow paths | 2^17 units |
| `bignum` | `1` + digits + `.5e-7` | big-number parsing, huge-mantissa float conversion | 2^20 digits |

- `deep` tail: `tail(n, j)` regenerates body chunk `n−1−j` and mirrors it in
  reverse (`[` → `]`, `{"k":` → `}`); `tail(n, 0)` starts with the leaf `0`.
  `tailLen(body)` = number of openers in the chunk; the 1-byte leaf is extra, so
  the output is still at least the target. No opener sequence is ever stored.
- In `wide-*`, the comma before a member depends only on its global index > 0.
- Giant strings and numbers are never `null`; `-null`/`-empty` affect only the
  small values inside `wide-*`.
- Memory: giant chunks ≈ 1 MiB; capped chunks ≤ max(1 MiB, cap), so the worst
  case is ≈ 3 × workers × 256 MB (documented in the README).

## Corpus

One NDJSON line per case:

```json
{"id":N,"verdict":"y|n|i","category":"trailing_comma","doc_b64":"..."}
```

`id` is the global case index; the stream interleaves categories by seeded draw.
Base64 keeps invalid bytes and broken UTF-8 from corrupting the framing.

Verdicts follow RFC 8259 in the JSONTestSuite convention:

| Verdict | Categories |
|---|---|
| `y` must accept | top-level scalars; all four whitespace chars around every token; every escape incl. `\u0000` and `\/`; numbers `-0`, `0e0`, `1E+2`, `1e-2`, `0.0`; raw 4-byte UTF-8 and noncharacters; nested empty containers; a valid chaos/typed record |
| `n` must reject | trailing / leading / missing comma; missing colon; unclosed or extra bracket; trailing garbage; empty document; single quotes; unquoted key; comments; bad escapes (`\x41`, `\u12`, `\a`); raw control characters in strings; numbers `01`, `.5`, `1.`, `+1`, `-`, `1e`, `0x10`, `NaN`, `Infinity`; misspelled literals (`tru`, `nul`); invalid UTF-8 (lone continuation byte, overlong, truncated sequence, encoded surrogate `ED A0 80`, above U+10FFFF) |
| `i` implementation-defined | duplicate keys; lone or reversed surrogate escapes; `1e400`, `-1e400`, `1e-400`; integers > 2^64; 1000-digit numbers; leading BOM; 10,000-deep nesting |

- `y` and `i` cases come from templates with randomized parts (values,
  whitespace, key names).
- Most `n` cases **mutate a small valid chaos or typed record** at a seeded
  position (insert a trailing comma before a closer, swap a `"` for `'`, inject
  a raw control char into a string, truncate, …), so the defect sits inside
  realistic content.
- Verdicts are self-checked at generation time with the reference check. If a
  mutation happens to produce valid JSON, the generator takes the next draw from
  the same seeded stream, so output stays deterministic. `i` cases are not
  checked.
- No filtering flags: use `jq -c 'select(.verdict=="n")'`.

## Files

| File | Responsibility |
|---|---|
| `gen.go` | flags, validation, `main`, `run(w, src)` with pass 1 / pass 2 |
| `source.go` | `source` interface; records source (adapter over `builder`) |
| `shape.go` | six shapes, giant and capped; `deep` mirroring |
| `corpus.go` | catalogue, mutations, reference check |
| `value.go`, `chaos.go`, `typed.go` | unchanged; corpus reuses them for valid records |
| `shape_test.go`, `corpus_test.go` | new tests; `gen_test.go` keeps records + CLI tests |

## Testing

1. **Records unchanged:** existing suite, plus fixed-seed SHA-256 regression for
   both schemas × array/NDJSON (hashes recorded before the refactor).
2. **Giant shapes** at ~3 MB (several chunks):
   - valid (for `deep`, a small bracket-matching checker, since
     `encoding/json` stops at 10,000 levels);
   - structure holds: depth equals the bracket count, `longstring` decodes to a
     single string, `wide-object` keys are unique and sequential, `bignum`
     parses with `big.Float`;
   - SHA-256 identical for workers 1, 4, 16.
3. **Capped shapes:** every record valid; each `v` within ±5% of `-cap`.
4. **Corpus** at ~5 MB: every line decodes; every `y` passes and every `n`
   fails the reference check; every catalogue category appears; identical for
   workers 1 and 8.
5. **CLI:** each new rejection.

Manual (probing parsers; findings are reported, not fixed):

- `jsongen -shape deep -size 100MB | python3 -c 'import json,sys; json.load(sys.stdin)'`
  should hit Python's recursion limit.
- Feed corpus `n` cases through `jq` and report how many it wrongly accepts.

## Docs

- README: Shapes and Corpus sections; memory note for `-cap`.
- Makefile: `make shape shape=NAME [cap=SIZE] [size=…]`, `make corpus [size=…]`.
- AGENTS.md: new files and the source/two-pass invariants.
- Main spec: CLI table points here for `-shape`, `-cap`, `-corpus`.

## Out of scope

Filtering or weighting corpus categories by flag; writing the corpus as files;
mutation-based fuzzing of giant shapes; parser harnesses (jsongen only produces
input).
