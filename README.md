# jsongen

Streams a random JSON payload of any size (50 GB and beyond) to stdout, with
`null` and empty values (`""`, `[]`, `{}`) mixed in. Memory stays constant, the
output is deterministic, and generation runs on all cores at 1.5–3 GB/s.

Standard library only (Go 1.27.1).

## Quick start

```sh
make build                                        # → bin/jsongen
bin/jsongen -size 50GB > big.json                 # one giant array
bin/jsongen -size 50GB -ndjson > big.ndjson       # one object per line
bin/jsongen -size 50GB -schema typed -ndjson | your-loader   # no disk needed
make generate size=10GB schema=typed ndjson=1     # → out/typed-10GB.ndjson
```

## Schemas

| `-schema` | For | Values |
|---|---|---|
| `chaos` (default) | parser stress tests, benchmarks | Any key holds any type. Strings include escapes, control characters (`\u0001`) and 2–4-byte UTF-8; integers span full int64; floats may use exponents. |
| `typed` | ingestion pipelines (Spark, BigQuery, Elasticsearch, DB import) | Every key path keeps one JSON type (plus `null`). Integers stay within ±(2^53−1), floats are fixed-point, no raw control characters. |

Both schemas apply `-null` and `-empty` at every level. In `typed` mode, empty
means the field's own empty form; numbers and booleans are never empty.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-size` | `50GB` | Target size: `B`/`KB`/`MB`/`GB` (decimal) or `KiB`/`MiB`/`GiB`. Output ends slightly above it (less than one chunk). |
| `-ndjson` | off | One object per line instead of one array |
| `-schema` | `chaos` | `chaos` or `typed` |
| `-null` | `0.10` | Probability a value is `null` |
| `-empty` | `0.10` | Probability a value is empty; `-null + -empty ≤ 1` |
| `-depth` | `4` | Maximum nesting depth, `0`–`10` |
| `-seed` | `42` | RNG seed |
| `-workers` | CPU count | Generator goroutines; never changes the output |
| `-chunk` | `1024` | Records per chunk; changes the output |

## Determinism

The same flags produce byte-identical output, whatever `-workers` is. A smaller
`-size` gives a prefix of a larger one. The guarantee holds per Go toolchain,
because `math/rand/v2` may change between releases. To reproduce an old file,
build with `GOTOOLCHAIN=go1.27.1`.

## Performance

Measured on 14 cores, writing to `/dev/null`:

| Schema | 1 worker | 14 workers |
|---|---|---|
| chaos | 0.21 GB/s | 1.8 GB/s |
| typed | 0.35 GB/s | 3.0 GB/s |

A 50 GB chaos file took 53 s. `make bench` measures your machine. simdjson can
parse chaos output faster than it is generated, so for that benchmark write the
file to disk first.

## Development

```sh
make help     # list targets
make check    # gofmt check + vet + tests
make test-race
```

Design: [`docs/superpowers/specs/`](docs/superpowers/specs/). Notes for coding
agents: [`AGENTS.md`](AGENTS.md).
