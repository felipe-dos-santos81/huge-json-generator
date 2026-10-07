# jsongen — huge random JSON generator: design

Date: 2026-10-07 · Status: approved in chat, pending written-spec review

## Goal

Stream 50+ GB of valid random JSON (with `null` and empty values) to stdout,
fast enough to pipe straight into a parser under benchmark, with constant memory
and byte-identical output for the same inputs.

Consumers (all three must be served):

- **A** stress-testing JSON parsers/libraries
- **B** load-testing ingestion pipelines (Spark, BigQuery, Elasticsearch, DB import)
- **C** benchmarking tools (`jq`, simdjson, serde_json), both from disk and piped

Starting point: the user's single-file `gen.go` draft (chaos generator, bufio
writer, size parsing). It does not compile as pasted (`\!` paste artifacts);
its logic is otherwise kept.

## Success criteria

1. Output is valid JSON (array form) / valid NDJSON for both schemas.
2. Output bytes depend only on `(seed, chunk, schema, null, empty, depth, ndjson, size)`
   — never on `-workers`.
3. In `typed` mode every key path has one JSON type (ignoring `null`); integers
   stay within ±(2^53−1).
4. With N workers, generation outpaces a fast consumer (target: multi-GB/s on
   this machine; measured, not assumed).
5. Memory is bounded by ~2 × workers × chunk size, independent of `-size`.

## Layout

```
go.mod        module jsongen; go 1.27.1 (pinned — rand/v2 method outputs are
              not guaranteed stable across Go versions; only PCG itself is)
gen.go        main, flags/config, run(), chunk pipeline, writer
value.go      chaos + typed value generators, string escaping
gen_test.go   stdlib-only tests (see Testing)
```

## CLI

| flag | default | meaning |
|---|---|---|
| `-size` | `50GB` | target size; B/KB/MB/GB decimal, KiB/MiB/GiB binary. Output is ≥ target, overshoot < one chunk |
| `-ndjson` | false | one object per line instead of one array |
| `-seed` | 42 | RNG seed |
| `-null` | 0.10 | probability a value is `null` |
| `-empty` | 0.10 | probability a value is empty (`""`, `[]`, `{}`); `-null + -empty ≤ 1` |
| `-depth` | 4 | max nesting depth (chaos: any value; typed: `payload` recursion) |
| `-schema` | `chaos` | `chaos` \| `typed` |
| `-workers` | `runtime.NumCPU()` | generator goroutines; ≥ 1; does not affect output |
| `-chunk` | 1024 | records per chunk; ≥ 1; **part of the determinism key**; final default fixed after measuring (target ≈ 1–2 MB/chunk) |

stdout carries only JSON. stderr: progress line every 1 GB written
(`X / Y GB  Z MB/s`) and a final `done: <records> records, <bytes> bytes in <dur>`.

Array form: `[\n` + records joined by `,\n` + `\n]\n`. NDJSON: each record + `\n`.

Out of scope (use existing tools): compression (`| zstd`), sharding
(`split -l` on NDJSON), config files.

## Chunk pipeline

```
claim idx ──► worker × N ──(idx, buf)──► writer: reorder ──► stdout
   ▲              (build chunk idx)       (writes idx 0,1,2,… in order)
   └──── token (2N in flight) ◄────── released after each write ───┘
```

- **Chunk definition.** Chunk `i` holds records with global ids
  `i·K … i·K+K−1` (K = `-chunk`). A record's leading `,\n` (array mode) depends
  only on `id > 0`, so every chunk is self-contained.
- **Per-chunk RNG.** `rand.New(rand.NewPCG(mix(seed), mix(seed ^ uint64(i))))`
  with `mix` = SplitMix64 finalizer. Hashing is required: Go's PCG takes its two
  seeds as the raw 128-bit state, so unhashed `(seed, i)` and `(seed, i+1)`
  would be adjacent points on one stream and produce near-identical chunks.
- **Workers.** Loop: acquire a token from a buffered channel of capacity 2N
  (select on `done`), claim `i = next.Add(1) − 1`, build the chunk into a
  buffer from a `sync.Pool`, send `(i, buf)` on the results channel (select on
  `done`).
- **Writer.** Holds a `map[int64][]byte` of early arrivals; whenever chunk
  `next` is present, writes it straight to `os.Stdout` (no `bufio` — chunks are
  already large), adds to `written`, returns the buffer to the pool, releases a
  token, increments `next`. No deadlock: indices are claimed in increasing order
  under the token cap, so the awaited chunk is always held by a running worker.
- **Stop.** When `written ≥ target` after a chunk, the writer closes `done`,
  writes the array trailer if needed, and returns. In-flight chunks are
  discarded. Stopping at a chunk boundary is what makes output independent of N.
- **Per-chunk builder.** The draft's `gen` struct (rng, buf, pNull, pEmpty,
  maxDepth) becomes the per-chunk builder: new RNG per chunk, buffer reused.

## Value generators

### Shared

- Every record starts with `"id": <global id>`. Remaining keys follow the
  draft's fixed order, each included with probability ⅔ (no duplicate keys).
- Each value: `null` with probability `-null`; else empty with probability
  `-empty`; else a normal value.
- `str(alphabet)` — the draft's escaping (`\" \\ \n \t \r \b \f`, other
  `< 0x20` as `\u00XX`, everything else UTF-8), parameterized by alphabet.
  Optimize (one `Uint64` per 8 characters) only if profiling shows it is the
  bottleneck.

### `chaos`

The draft's `value()` unchanged: any key may hold any type; full alphabet
including `\u0001`/`\u001f`; full-range int64; `'g'` floats (exponent form
allowed); empty picks uniformly from `""`, `[]`, `{}`; scalars only at max depth.

### `typed`

| key | type | values |
|---|---|---|
| `name` | string | 3–20 chars, ASCII + accented Latin |
| `email` | string | `word@word.com` |
| `score` | float | [0, 100), fixed-point (no exponent) |
| `active` | bool | |
| `tags` | string[] | 0–5 short strings |
| `meta` | object | `{source: string, version: int, flags: bool[]}` |
| `note` | string | usually short; 1 in 50 is 200–2200 chars; full UTF-8 incl. 4-byte; `\" \\ \n \t` escapes |
| `price` | float | 2 decimal places |
| `created_at` | string | RFC 3339 timestamp |
| `payload` | object | `{k: string, n: int, child: payload}`, recursion stops at `-depth` (child omitted) |
| `items` | object[] | 0–5 × `{sku: string, qty: int, price: float}` |
| `owner` | object | `{name: string, email: string}` |
| `ref` | string | 32 lowercase hex chars |
| `count` | int | usually small; 1 in 20 uniform in ±(2^53−1) |
| `extra` | int[] | 0–5 small ints |
| `données` | string | non-ASCII key is the point |

- `null`/empty rolls apply at every field, including nested ones and arrays/objects as a whole.
- Empty is type-appropriate: `""` / `[]` / `{}`. Numbers and bools have no
  empty form — an empty roll yields a normal value (effective empty rate 0 for
  those; stated in `-empty` help text).
- Typed string alphabet excludes `\b`, `\f`, `\u0001`, `\u001f`; keeps `"`,
  `\`, `\n`, `\t`, multi-byte UTF-8.
- Known caveat: `{}` for an object-typed field is valid and type-consistent,
  but strict Avro/Parquet converters may reject it; use `-empty 0` there.

## Errors

- Invalid flags (`-size` unparsable or ≤ 0, `-null`/`-empty` < 0 or sum > 1,
  `-depth` < 0, `-workers` < 1, `-chunk` < 1, unknown `-schema`): one-line
  stderr message, exit 2.
- Write error (e.g. `ENOSPC`): `write: <err>` on stderr, exit 1. Partial output
  is left as-is (stdout may not be a file).
- Consumer closes the pipe: Go's default `SIGPIPE` on fd 1 terminates the
  process silently — desired for `jsongen | head`.
- No panic recovery.

## Testing

`main()` only parses flags into `config` and calls
`run(w io.Writer, cfg config) error`; tests drive `run` with a `bytes.Buffer`.

`gen_test.go` (stdlib only):

1. **Validity** — {chaos, typed} × {array, NDJSON} at ~5 MB: `json.Valid` on
   the whole array / each line; ids are 0…n−1 contiguous.
2. **Determinism & N-invariance** — SHA-256 of output equal for workers 1, 4,
   16; differs when seed or schema changes.
3. **Typed stability** — decode typed NDJSON; every key path (incl.
   `items[].qty`, `payload.child.n`, …) has ≤ 1 JSON type excluding `null`;
   every integer within ±(2^53−1).
4. **Rates** — `-null 0.2 -empty 0`: top-level `null` share is 20% ± 2%.

Manual, before declaring done:

- `./jsongen -size 5GB > /dev/null` at workers 1 and N; record MB/s; fix the
  `-chunk` default from measured record size.
- `./jsongen -size 1GB -schema typed -ndjson | jq -c . > /dev/null` (second,
  independent parser).
- One full 50 GB run to disk: record time and final size; delete afterwards
  unless asked to keep (≈ 157 GB free on the volume).
