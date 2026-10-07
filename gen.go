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
	"sync"
	"sync/atomic"
	"time"
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
