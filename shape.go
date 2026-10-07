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
	open, close string
	perChunk    int64 // units per giant chunk
	unit        func(s *shapeSource, idx int64)
}

var shapes = map[shapeKind]shapeDef{
	deep:       {"", "", 1 << 18, (*shapeSource).opener},
	wideObject: {"{", "}", 1 << 14, (*shapeSource).member},
	wideArray:  {"[", "]", 1 << 14, (*shapeSource).element},
	longString: {`"`, `"`, 1 << 12, func(s *shapeSource, _ int64) { s.chars(typedAlphabet, 64) }},
	escapes:    {`"`, `"`, 1 << 17, func(s *shapeSource, _ int64) { s.escape() }},
	bigNum:     {"1", ".5e-7", 1 << 20, func(s *shapeSource, _ int64) { s.buf = append(s.buf, byte('0'+s.r.IntN(10))) }},
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

// chunk writes units [i*perChunk, (i+1)*perChunk) of the giant value.
func (s *shapeSource) chunk(dst []byte, i int64) []byte {
	s.pcg.Seed(s.cfg.seed, uint64(i))
	s.buf = dst
	for idx := i * s.def.perChunk; idx < (i+1)*s.def.perChunk; idx++ {
		s.def.unit(s, idx)
	}
	return s.buf
}

// tailLen is the number of closers a deep body chunk needs: one per opener.
func (s *shapeSource) tailLen(body []byte) int64 {
	if s.kind != deep {
		return 0
	}
	return int64(openers(body))
}

// tail closes deep nesting: chunk j mirrors body chunk n-1-j, rebuilt from its
// seed, so no opener sequence is ever stored. The first tail chunk starts
// with the leaf value.
func (s *shapeSource) tail(dst []byte, n, j int64) []byte {
	s.scratch = s.chunk(s.scratch[:0], n-1-j)
	if j == 0 {
		dst = append(dst, '0')
	}
	return mirror(dst, s.scratch)
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

var (
	simpleEscapes = []string{`\"`, `\\`, `\/`, `\b`, `\f`, `\n`, `\r`, `\t`}
	rawMultibyte  = []rune(accented + "日本語한국" + "😀🚀")
)

// escape writes one escape-heavy unit: a simple escape, a \u escape of a
// non-surrogate BMP code point, a surrogate pair, or a raw multi-byte rune.
func (b *builder) escape() {
	switch b.r.IntN(4) {
	case 0:
		b.buf = append(b.buf, simpleEscapes[b.r.IntN(len(simpleEscapes))]...)
	case 1:
		v := b.r.IntN(0xF800) // skip the surrogate block D800–DFFF
		if v >= 0xD800 {
			v += 0x800
		}
		b.uEscape(v)
	case 2:
		b.uEscape(0xD800 + b.r.IntN(0x400))
		b.uEscape(0xDC00 + b.r.IntN(0x400))
	default:
		b.buf = append(b.buf, string(rawMultibyte[b.r.IntN(len(rawMultibyte))])...)
	}
}

func (b *builder) uEscape(v int) {
	const hex = "0123456789abcdef"
	b.buf = append(b.buf, '\\', 'u', hex[v>>12&0xf], hex[v>>8&0xf], hex[v>>4&0xf], hex[v&0xf])
}
