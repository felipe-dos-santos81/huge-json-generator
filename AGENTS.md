# AGENTS.md

Go 1.27.1 CLI that streams huge random JSON to stdout. Standard library only;
do not add dependencies.

## Layout

| File | Responsibility |
|---|---|
| `gen.go` | `config`, flag parsing and validation (`parseSize`, `parseConfig`), the ordered parallel pipeline (`run`), `main` |
| `value.go` | Per-worker `builder`: `chunk()` framing, chaos generator (`value`, `fields`, `record`), typed generator (`typedField`, `typedRecord`, `t*` helpers) |
| `gen_test.go` | All tests, stdlib `testing` only |
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
  their max (`tFixed` draws integers for this reason). Adding a key means adding
  it to both `keys` and `typedField`.
- **`-depth` is capped at 10,** because chaos memory grows about 1.4× per level.

## Gotchas

- **zsh MULTIOS:** `cmd 2>&1 >/dev/null | grep` also sends stdout into the
  pipe. Redirect stderr to a file instead. Makefile recipes run under `/bin/sh`
  and are unaffected.
- **Big runs:** write them under `out/` (git-ignored) and delete them after. Make
  sure no process (such as `tail`) still holds a deleted file open, or the disk
  space is not freed.
