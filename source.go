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
	if cfg.shape != "" {
		return newShape(cfg)
	}
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
