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
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// schema selects how values are generated.
type schema string

const (
	chaos schema = "chaos" // any key may hold any type
	typed schema = "typed" // every key path keeps one JSON type
)

func (s schema) MarshalText() ([]byte, error) { return []byte(s), nil }

func (s *schema) UnmarshalText(text []byte) error {
	switch v := schema(text); v {
	case chaos, typed:
		*s = v
		return nil
	}
	return fmt.Errorf("must be %s or %s", chaos, typed)
}

const maxDepth = 10 // chaos output grows ~1.4x per nesting level

type config struct {
	target   int64
	ndjson   bool
	seed     uint64
	pNull    float64
	pEmpty   float64
	depth    int
	schema   schema
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
		if num, ok := strings.CutSuffix(u, x.suffix); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
			if err != nil {
				return 0, err
			}
			v := f * float64(x.mult)
			if !(v < math.MaxInt64) { // also rejects NaN and +Inf
				return 0, fmt.Errorf("size %q out of range", s)
			}
			return int64(v), nil
		}
	}
	return strconv.ParseInt(u, 10, 64)
}

// flags declares every command-line flag, bound to cfg's fields.
func flags(cfg *config) *flag.FlagSet {
	fs := flag.NewFlagSet("jsongen", flag.ContinueOnError)
	cfg.target = 50e9
	fs.Func("size", "target size: B, KB, MB, GB (decimal) or KiB, MiB, GiB (binary) (default 50GB)", func(s string) error {
		v, err := parseSize(s)
		if err != nil || v <= 0 {
			return errors.New("want a positive size such as 500MB or 2GiB")
		}
		cfg.target = v
		return nil
	})
	fs.BoolVar(&cfg.ndjson, "ndjson", false, "one object per line instead of a single array")
	fs.Uint64Var(&cfg.seed, "seed", 42, "RNG seed; same flags give identical output")
	fs.Float64Var(&cfg.pNull, "null", 0.10, "probability a value is null")
	fs.Float64Var(&cfg.pEmpty, "empty", 0.10, `probability a value is empty ("", [] or {}); typed numbers/bools are never empty`)
	fs.IntVar(&cfg.depth, "depth", 4, fmt.Sprintf("maximum nesting depth (0-%d)", maxDepth))
	fs.TextVar(&cfg.schema, "schema", chaos, "chaos (any type per key) or typed (one type per key)")
	fs.IntVar(&cfg.workers, "workers", runtime.NumCPU(), "generator goroutines; does not change output")
	fs.IntVar(&cfg.chunk, "chunk", 1024, "records per chunk; changes output")
	return fs
}

// usage prints the synopsis, examples and flag defaults to w.
func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: jsongen [flags] > out.json
Streams random JSON of the target size to stdout.

  jsongen -size 50GB > big.json                      # one giant array
  jsongen -size 50GB -ndjson > big.ndjson            # one object per line
  jsongen -size 50GB -schema typed -ndjson | loader  # stable types per key

Flags:
`)
	fs := flags(new(config))
	fs.SetOutput(w)
	fs.PrintDefaults()
}

// parseConfig parses and validates command-line flags. Every failure is a
// one-line error; -h returns flag.ErrHelp without printing anything.
func parseConfig(args []string) (config, error) {
	var cfg config
	fs := flags(&cfg)
	fs.SetOutput(io.Discard) // the flag package would print the whole usage on error
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func (c config) validate() error {
	switch {
	case !(c.pNull >= 0) || !(c.pEmpty >= 0) || !(c.pNull+c.pEmpty <= 1): // NaN fails
		return errors.New("-null and -empty must be >= 0 and sum to at most 1")
	case c.depth < 0 || c.depth > maxDepth:
		return fmt.Errorf("-depth must be between 0 and %d", maxDepth)
	case c.workers < 1:
		return errors.New("-workers must be >= 1")
	case c.chunk < 1:
		return errors.New("-chunk must be >= 1")
	}
	return nil
}

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

// report prints one progress line: bytes written, target, and average rate.
func report(w io.Writer, written, target int64, start time.Time) {
	fmt.Fprintf(w, "%.1f / %.1f GB  %.0f MB/s\n",
		float64(written)/1e9, float64(target)/1e9, float64(written)/1e6/time.Since(start).Seconds())
}

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		usage(os.Stderr)
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v (see -h)\n", err)
		os.Exit(2)
	}
	cfg.progress = os.Stderr
	start := time.Now()
	records, written, err := run(os.Stdout, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "done: %d records, %d bytes in %s\n",
		records, written, time.Since(start).Round(time.Millisecond))
}
