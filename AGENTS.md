# AGENTS.md

Go 1.27.1 CLI that streams huge random JSON to stdout. Standard library only;
do not add dependencies.

## Layout

| File | Responsibility |
|---|---|
| `gen.go` | `config` and `schema`, flags (`flags`, `usage`, `parseConfig`, `validate`), the ordered parallel pipeline (`startWorkers`, `pipeline`, `run`), `main` |
| `value.go` | Per-worker `builder`: `chunk()` framing, `record`/`someFields` key selection, `strN` escaping, shared alphabets |
| `chaos.go` | `chaosValue`: any key may hold any type |
| `typed.go` | `fields` table (every key, in order, with its typed generator) and the `typed*` helpers |
| `source.go` | `source` interface (header, body chunks, optional tail, trailer) and the records source |
| `shape.go` | `-shape`: giant values (`shapeSource`, `deep` mirrored tail) and capped records (`cappedSource`) |
| `corpus.go` | `-corpus`: the y/n/i `catalogue`, mutations of valid records, the `acceptable` reference check |
| `gen_test.go` | Records and CLI tests, stdlib `testing` only |
| `shape_test.go` | `-shape` tests, including the `nesting` checker |
| `corpus_test.go` | `-corpus` tests |
| `docs/superpowers/specs/` | Design spec: the authority on behaviour |
| `docs/superpowers/plans/` | Implementation plan (historical) |

## Commands

```sh
make check      # gofmt check + go vet + go test; run before every commit
make test-race  # after touching run() or anything concurrent
make bench      # throughput check after touching value generation
```

## Invariants (tests enforce them; keep them green)

- **Output never depends on `-workers`.** Chunk `i` is seeded with
  `PCG(seed, i)` and must be a pure function of the config and `i`. Never share
  RNG state across chunks.
- **`-chunk` is part of the determinism key.** Changing its default changes every
  generated file.
- **stdout carries only JSON.** Progress and errors go to stderr. Bad flags exit
  2; write errors exit 1.
- **Typed mode keeps one JSON type per key path (plus `null`).** Integers stay
  within ±(2^53−1), and floats print fixed-point with a decimal point and below
  their limit (`typedFixed` draws integers for this reason). Adding a key is one
  entry in the `fields` table, which both schemas use.
- **`-depth` is capped at 10** (`maxDepth`), because chaos memory grows about 1.4× per level.
- **Refactors must not change output bytes.** Compare `shasum` of a fixed-seed
  run before and after, for both schemas and both output forms.

- **Every mode is a `source`.** `chunk` and `tail` must be pure functions of the
  config and their indices. `run` writes the body until the target (counting
  pending tail bytes), then the tail (`deep` closers, mirrored from regenerated
  body chunks), then the trailer.
- **Never write `\u` followed by four hex digits in Go source.** Build JSON
  escapes with `uEscape` and runes with numeric literals (`0xFEFF`); generated
  edits have turned such escapes into raw characters before.

## Gotchas

- **zsh MULTIOS:** `cmd 2>&1 >/dev/null | grep` also sends stdout into the
  pipe. Redirect stderr to a file instead. Makefile recipes run under `/bin/sh`
  and are unaffected.
- **Big runs:** write them under `out/` (git-ignored) and delete them after. Make
  sure no process (such as `tail`) still holds a deleted file open, or the disk
  space is not freed.
