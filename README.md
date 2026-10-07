# jsongen

Streams random JSON of any size to stdout, 50 GB and beyond, with `null` and
empty values mixed in. Memory stays bounded, output is deterministic, and
records stream at 1.5–3 GB/s on all cores. Go 1.27.1, standard library only.

## Quick start

```sh
make build                                                  # → bin/jsongen
bin/jsongen -size 50GB > big.json                           # one array
bin/jsongen -size 50GB -ndjson > big.ndjson                 # one object per line
bin/jsongen -size 50GB -schema typed -ndjson | your-loader  # no disk needed
bin/jsongen -shape deep -size 1GB | your-parser             # one extreme value
bin/jsongen -corpus -size 100MB > cases.ndjson              # labelled parser tests
```

`make help` lists shortcuts that write to `out/`.

## Modes

| Mode | Use it for | Output |
|---|---|---|
| records, `-schema chaos` (default) | parser benchmarks | any key holds any type: escapes, control characters, 2–4-byte UTF-8, full-range int64, exponent floats |
| records, `-schema typed` | pipelines (Spark, BigQuery, Elasticsearch, DB import) | one JSON type per key path (plus `null`), integers within ±(2^53−1), fixed-point floats |
| `-shape NAME` | crashes, out-of-memory | one valid document whose single value fills `-size` |
| `-shape NAME -cap SIZE` | performance cliffs | records `{"id":N,"v":…}`, each value about `SIZE` |
| `-corpus` | conformance, edge cases | NDJSON cases `{"id","verdict","category","doc_b64"}` |

| `-shape` | Value | Targets |
|---|---|---|
| `deep` | nested `[` and `{"k":` around a `0` leaf | stack overflow, recursion limits |
| `wide-object` | `{"k0":v,"k1":v,…}` | hash-map growth, out-of-memory |
| `wide-array` | `[v,v,…]` | array growth, out-of-memory |
| `longstring` | one string of ASCII and 2–4-byte UTF-8 | buffer growth, UTF-8 validation |
| `escapes` | one string of escapes, surrogate pairs and raw multi-byte UTF-8 | escape decoding |
| `bignum` | `1`, then digits, then `.5e-7` | big-number parsing, float conversion |

Corpus verdicts follow RFC 8259 in JSONTestSuite style: `y` must be accepted,
`n` must be rejected, `i` is implementation-defined. Every `y` and `n` document
is checked as it is generated. Decode `doc_b64` before parsing; filter with
`jq -c 'select(.verdict=="n")'`.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-size` | `50GB` | Target size: `B`, `KB`, `MB`, `GB`, or `KiB`, `MiB`, `GiB` |
| `-ndjson` | off | One object per line instead of one array |
| `-schema` | `chaos` | `chaos` or `typed` |
| `-null` | `0.10` | Probability a value is `null` |
| `-empty` | `0.10` | Probability a value is empty (`""`, `[]`, `{}`); `-null + -empty ≤ 1` |
| `-depth` | `4` | Maximum nesting depth, 0–10 |
| `-seed` | `42` | RNG seed |
| `-workers` | CPU count | Generator goroutines; never changes the output |
| `-chunk` | `1024` | Records per chunk; changes the output |
| `-shape` | none | Write one extreme value (see Modes) |
| `-cap` | none | With `-shape`: records of values about this size, at most 256 MB |
| `-corpus` | off | Write labelled parser test cases |

A flag that would have no effect in the chosen mode, such as `-schema` with
`-shape`, is rejected with an error.

## Guarantees

- **Deterministic:** the same flags give the same bytes, whatever `-workers` is,
  for a given Go toolchain. Reproduce old files with `GOTOOLCHAIN=go1.27.1`.
- **Prefix:** with `-ndjson` or `-corpus`, a smaller `-size` gives a prefix of a
  larger one.
- **Size:** output ends just above `-size`, by less than one chunk.
- **Memory:** tens of MB for records and giant shapes; about
  3 × workers × `-cap` for capped shapes.

## Performance

Measured on 14 cores, writing to `/dev/null`:

| Schema | 1 worker | 14 workers |
|---|---|---|
| chaos | 0.21 GB/s | 1.8 GB/s |
| typed | 0.35 GB/s | 3.0 GB/s |

A 50 GB chaos file took 53 s; a 1 GB giant shape takes under a second.
`make bench` measures your machine. simdjson parses chaos output faster than it
is generated, so write that benchmark's input to disk first.

## Development

```sh
make check      # gofmt check, vet, tests; run before committing
make test-race  # slow
```

Design specs are in [`docs/superpowers/specs/`](docs/superpowers/specs/); notes
for coding agents are in [`AGENTS.md`](AGENTS.md).
