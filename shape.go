package main

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// shapeKind names an extreme value for stress-testing parsers.
type shapeKind string

const (
	deep       shapeKind = "deep"
	wideObject shapeKind = "wide-object"
	wideArray  shapeKind = "wide-array"
	longString shapeKind = "longstring"
	escapes    shapeKind = "escapes"
	bigNum     shapeKind = "bignum"
)

// shapeDef describes a value as open + units + close. A giant value spreads
// its units over chunks; unit idx is global, so commas and keys depend only
// on the index.
type shapeDef struct {
	open, close   string
	unitsPerChunk int64 // units per giant chunk
	unit          func(s *shapeSource, idx int64)
	mirrored      bool // units are openers; closers mirror them (deep)
	smallValues   bool // units hold small chaos values that -null/-empty affect
}

var shapes = map[shapeKind]shapeDef{
	deep:       {unitsPerChunk: 1 << 18, unit: (*shapeSource).opener, mirrored: true},
	wideObject: {open: "{", close: "}", unitsPerChunk: 1 << 14, unit: (*shapeSource).member, smallValues: true},
	wideArray:  {open: "[", close: "]", unitsPerChunk: 1 << 14, unit: (*shapeSource).element, smallValues: true},
	longString: {open: `"`, close: `"`, unitsPerChunk: 1 << 12, unit: func(s *shapeSource, _ int64) { s.chars(typedAlphabet, 64) }},
	escapes:    {open: `"`, close: `"`, unitsPerChunk: 1 << 17, unit: func(s *shapeSource, _ int64) { s.escape() }},
	bigNum: {open: "1", close: ".5e-7", unitsPerChunk: 1 << 20, unit: func(s *shapeSource, _ int64) {
		s.buf = append(s.buf, byte('0'+s.r.IntN(10)))
	}},
}

func shapeNames() string {
	names := make([]string, 0, len(shapes))
	for k := range shapes {
		names = append(names, string(k))
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

func (k shapeKind) MarshalText() ([]byte, error) { return []byte(k), nil }

func (k *shapeKind) UnmarshalText(text []byte) error {
	if _, ok := shapes[shapeKind(text)]; !ok {
		return fmt.Errorf("must be one of %s", shapeNames())
	}
	*k = shapeKind(text)
	return nil
}

// shapeSource writes one giant value of the configured shape.
type shapeSource struct {
	*builder
	kind    shapeKind
	def     shapeDef
	scratch []byte
}

func newShape(cfg config) *shapeSource {
	return &shapeSource{builder: newBuilder(cfg), kind: cfg.shape, def: shapes[cfg.shape]}
}

func (s *shapeSource) header() []byte  { return []byte(s.def.open) }
func (s *shapeSource) trailer() []byte { return []byte(s.def.close) }

func (s *shapeSource) summary(int64) string { return fmt.Sprintf("1 %s value", s.kind) }

// chunk writes units [i*unitsPerChunk, (i+1)*unitsPerChunk) of the giant value.
func (s *shapeSource) chunk(dst []byte, i int64) []byte {
	s.pcg.Seed(s.cfg.seed, uint64(i))
	s.buf = dst
	for idx := i * s.def.unitsPerChunk; idx < (i+1)*s.def.unitsPerChunk; idx++ {
		s.def.unit(s, idx)
	}
	return s.buf
}

// deepSource is a giant mirrored shape (deep): a second pass closes the
// nesting by mirroring each body chunk, rebuilt from its seed, so no opener
// sequence is ever stored.
type deepSource struct{ *shapeSource }

// tailLen is one closer per opener in the body chunk.
func (d deepSource) tailLen(body []byte) int64 { return int64(openers(body)) }

// tail mirrors body chunk n-1-j; the first tail chunk starts with the leaf.
func (d deepSource) tail(dst []byte, n, j int64) []byte {
	d.scratch = d.chunk(d.scratch[:0], n-1-j)
	if j == 0 {
		dst = append(dst, '0')
	}
	return mirror(dst, d.scratch)
}

// openers counts the [ and { in a run of deep openers.
func openers(run []byte) int {
	n := 0
	for _, c := range run {
		if c == '[' || c == '{' {
			n++
		}
	}
	return n
}

// mirror appends the closers for a run of openers, innermost first.
func mirror(dst, run []byte) []byte {
	for k := len(run) - 1; k >= 0; k-- {
		switch run[k] {
		case '[':
			dst = append(dst, ']')
		case '{':
			dst = append(dst, '}')
		}
	}
	return dst
}

// maxCap bounds -cap: capped chunks hold at least one value and append growth
// overshoots it, so memory is about 3 x workers x cap.
const maxCap = 256e6

// cappedSource writes records {"id":N,"v":<value of about -cap bytes>}, each
// value built whole inside its record. The builder (shared with shape)
// supplies record framing; shape supplies the value's units.
type cappedSource struct {
	*builder
	shape *shapeSource
}

func (c cappedSource) recordsPerChunk() int64 {
	return min(1024, max(1, (1<<20)/c.cfg.valueCap))
}

func (c cappedSource) summary(n int64) string {
	return fmt.Sprintf("%d records", n*c.recordsPerChunk())
}

func (c cappedSource) chunk(dst []byte, i int64) []byte {
	c.pcg.Seed(c.cfg.seed, uint64(i))
	return c.records(dst, i, c.recordsPerChunk(), c.cappedRecord)
}

// cappedRecord appends units until the value, counting the closers a
// mirrored shape still needs, reaches the cap.
func (c cappedSource) cappedRecord(id int64) {
	def := c.shape.def
	c.buf = append(c.buf, `{"id":`...)
	c.buf = strconv.AppendInt(c.buf, id, 10)
	c.buf = append(c.buf, `,"v":`...)
	c.buf = append(c.buf, def.open...)
	start := len(c.buf)
	budget := c.cfg.valueCap - int64(len(def.open)+len(def.close))
	for idx := int64(0); ; idx++ {
		size := int64(len(c.buf) - start)
		if def.mirrored {
			size += idx + 1 // a closer per opener, plus the leaf
		}
		if size >= budget {
			break
		}
		def.unit(c.shape, idx)
	}
	if def.mirrored {
		run := c.buf[start:]
		c.buf = append(c.buf, '0')
		c.buf = mirror(c.buf, run)
	}
	c.buf = append(c.buf, def.close...)
	c.buf = append(c.buf, '}')
}

func (s *shapeSource) opener(int64) {
	if s.r.IntN(2) == 0 {
		s.buf = append(s.buf, '[')
	} else {
		s.buf = append(s.buf, `{"k":`...)
	}
}

// small writes a small chaos value: a scalar, null or an empty container.
func (s *shapeSource) small() { s.chaosValue(math.MaxInt) }

func (s *shapeSource) member(idx int64) {
	if idx > 0 {
		s.buf = append(s.buf, ',')
	}
	s.buf = append(s.buf, `"k`...)
	s.buf = strconv.AppendInt(s.buf, idx, 10)
	s.buf = append(s.buf, '"', ':')
	s.small()
}

func (s *shapeSource) element(idx int64) {
	if idx > 0 {
		s.buf = append(s.buf, ',')
	}
	s.small()
}
