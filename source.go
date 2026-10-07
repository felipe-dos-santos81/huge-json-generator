package main

import "fmt"

// source produces the output in pieces the pipeline can build in parallel.
// Each worker owns its own source; chunk must be a pure function of the
// config and i so the output never depends on -workers. chunk may be called
// for indices past where run stops (workers build ahead); those results are
// discarded, so it must accept any index without panicking.
type source interface {
	header() []byte                   // written first
	chunk(dst []byte, i int64) []byte // body chunk i
	trailer() []byte                  // written last
	summary(n int64) string           // what n body chunks hold, for the done line
}

// tailer is a source whose body must be closed by a second pass (deep
// nesting). tail has the same purity rule as chunk; pass 2 may call it with
// j >= n for chunks it then discards.
type tailer interface {
	// tailLen is how many closing bytes pass 2 adds for this body chunk; the
	// first tail chunk may add a constant few more (deep's leaf).
	tailLen(body []byte) int64
	tail(dst []byte, n, j int64) []byte // closing chunk j after n body chunks
}

// newSource returns a fresh source for the configured mode.
func newSource(cfg config) source {
	switch {
	case cfg.corpus:
		return newCorpus(cfg)
	case cfg.shape != "":
		s := newShape(cfg)
		switch {
		case cfg.valueCap > 0:
			return cappedSource{s}
		case s.def.leaf != "":
			return deepSource{s}
		}
		return s
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

func (b *builder) summary(n int64) string {
	return fmt.Sprintf("%d records", n*int64(b.cfg.chunk))
}
