package main

import (
	"math/rand/v2"
	"strconv"
	"unicode/utf8"
)

// Keys are drawn as an ordered random subset, so an object never has duplicates.
var keys = []string{
	"name", "email", "score", "active", "tags", "meta", "note", "price",
	"created_at", "payload", "items", "owner", "ref", "count", "extra", "données",
}

// Plain ASCII plus the things that hit parser slow paths: escapes,
// control characters, 2/3/4-byte UTF-8.
var chaosAlphabet = []rune(
	"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" +
		"      _-.,:;/@#" +
		"áçãéõüñß" + "日本語한국" + "😀🚀" +
		"\"\\/\n\t\r\b\f\x01\x1f")

// builder generates one chunk of records at a time. Each worker owns one.
type builder struct {
	pcg      *rand.PCG
	r        *rand.Rand
	buf      []byte
	seed     uint64
	chunkLen int64
	ndjson   bool
	typed    bool
	pNull    float64
	pEmpty   float64
	maxDepth int
}

func newBuilder(cfg config) *builder {
	pcg := rand.NewPCG(0, 0)
	return &builder{
		pcg: pcg, r: rand.New(pcg), seed: cfg.seed, chunkLen: int64(cfg.chunk),
		ndjson: cfg.ndjson, typed: cfg.typed,
		pNull: cfg.pNull, pEmpty: cfg.pEmpty, maxDepth: cfg.depth,
	}
}

// chunk appends records [i*chunkLen, (i+1)*chunkLen) to dst, framed for the
// output mode. The result depends only on the config and i.
func (b *builder) chunk(dst []byte, i int64) []byte {
	b.pcg.Seed(b.seed, uint64(i))
	b.buf = dst
	for id := i * b.chunkLen; id < (i+1)*b.chunkLen; id++ {
		if id > 0 && !b.ndjson {
			b.buf = append(b.buf, ',', '\n')
		}
		b.record(id)
		if b.ndjson {
			b.buf = append(b.buf, '\n')
		}
	}
	return b.buf
}

// strN writes a quoted JSON string of n runes drawn from alphabet.
func (b *builder) strN(alphabet []rune, n int) {
	b.buf = append(b.buf, '"')
	for range n {
		c := alphabet[b.r.IntN(len(alphabet))]
		switch c {
		case '"':
			b.buf = append(b.buf, '\\', '"')
		case '\\':
			b.buf = append(b.buf, '\\', '\\')
		case '\n':
			b.buf = append(b.buf, '\\', 'n')
		case '\t':
			b.buf = append(b.buf, '\\', 't')
		case '\r':
			b.buf = append(b.buf, '\\', 'r')
		case '\b':
			b.buf = append(b.buf, '\\', 'b')
		case '\f':
			b.buf = append(b.buf, '\\', 'f')
		default:
			if c < 0x20 {
				b.buf = append(b.buf, '\\', 'u', '0', '0',
					"0123456789abcdef"[c>>4], "0123456789abcdef"[c&0xf])
			} else {
				b.buf = utf8.AppendRune(b.buf, c)
			}
		}
	}
	b.buf = append(b.buf, '"')
}

// textLen is a short length, with an occasional long one.
func (b *builder) textLen() int {
	if b.r.IntN(50) == 0 {
		return 200 + b.r.IntN(2000)
	}
	return 1 + b.r.IntN(24)
}

func (b *builder) key(k string) {
	b.buf = append(b.buf, '"')
	b.buf = append(b.buf, k...)
	b.buf = append(b.buf, '"', ':')
}

// ---- chaos: any key may hold any type ----

// fields writes a random, duplicate-free subset of keys, each preceded by a comma.
func (b *builder) fields(depth int) {
	for _, k := range keys {
		if b.r.IntN(3) == 0 {
			continue
		}
		b.buf = append(b.buf, ',')
		b.key(k)
		b.value(depth)
	}
}

func (b *builder) value(depth int) {
	x := b.r.Float64()
	if x < b.pNull {
		b.buf = append(b.buf, "null"...)
		return
	}
	if x < b.pNull+b.pEmpty {
		switch b.r.IntN(3) {
		case 0:
			b.buf = append(b.buf, `""`...)
		case 1:
			b.buf = append(b.buf, "[]"...)
		default:
			b.buf = append(b.buf, "{}"...)
		}
		return
	}

	kind := b.r.IntN(10)
	if depth >= b.maxDepth {
		kind = b.r.IntN(7) // scalars only at the depth limit
	}
	switch kind {
	case 0, 1, 2:
		b.strN(chaosAlphabet, b.textLen())
	case 3: // small int
		b.buf = strconv.AppendInt(b.buf, int64(b.r.IntN(100000))-1000, 10)
	case 4: // full-range int64
		b.buf = strconv.AppendInt(b.buf, int64(b.r.Uint64()), 10)
	case 5: // float, shortest round-trip repr (may use exponent form)
		b.buf = strconv.AppendFloat(b.buf, b.r.NormFloat64()*float64(int64(1)<<b.r.IntN(40)), 'g', -1, 64)
	case 6:
		b.buf = strconv.AppendBool(b.buf, b.r.IntN(2) == 0)
	case 7, 8: // array
		n := 1 + b.r.IntN(6)
		b.buf = append(b.buf, '[')
		for i := range n {
			if i > 0 {
				b.buf = append(b.buf, ',')
			}
			b.value(depth + 1)
		}
		b.buf = append(b.buf, ']')
	default: // object
		b.buf = append(b.buf, '{')
		b.key("k")
		b.value(depth + 1)
		b.fields(depth + 1)
		b.buf = append(b.buf, '}')
	}
}

func (b *builder) record(id int64) {
	b.buf = append(b.buf, `{"id":`...)
	b.buf = strconv.AppendInt(b.buf, id, 10)
	b.fields(1)
	b.buf = append(b.buf, '}')
}
