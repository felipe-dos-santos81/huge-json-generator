package main

import (
	"math/rand/v2"
	"strconv"
	"unicode/utf8"
)

const (
	lower    = "abcdefghijklmnopqrstuvwxyz"
	upper    = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	accented = "áçãéõüñß"

	// text is plain ASCII plus 2/3/4-byte UTF-8, shared by both schemas.
	text = lower + upper + "0123456789" + "      _-.,:;/@#" + accented + "日本語한국" + "😀🚀"
)

// Chaos strings add every escape and control characters that hit parser slow paths.
var chaosAlphabet = []rune(text + "\"\\/\n\t\r\b\f\x01\x1f")

// Typed strings keep the common escapes but drop the control characters that
// ingestion pipelines commonly reject.
var typedAlphabet = []rune(text + "\"\\/\n\t")

var nameAlphabet = []rune(lower + upper + accented)

var wordAlphabet = []rune(lower)

// builder generates one chunk of records at a time. Each worker owns one.
type builder struct {
	cfg config
	pcg *rand.PCG
	r   *rand.Rand
	buf []byte
}

func newBuilder(cfg config) *builder {
	pcg := rand.NewPCG(0, 0)
	return &builder{cfg: cfg, pcg: pcg, r: rand.New(pcg)}
}

// chunk appends records [i*chunk, (i+1)*chunk) to dst, framed for the output
// mode. The result depends only on the config and i.
func (b *builder) chunk(dst []byte, i int64) []byte {
	b.pcg.Seed(b.cfg.seed, uint64(i))
	return b.records(dst, i, int64(b.cfg.chunk), b.record)
}

// records appends records [i*n, (i+1)*n) to dst, framed for the output mode:
// ",\n" before every record but the first in an array, "\n" after each in
// NDJSON. rec writes the record with the given id.
func (b *builder) records(dst []byte, i, n int64, rec func(id int64)) []byte {
	b.buf = dst
	for id := i * n; id < (i+1)*n; id++ {
		if id > 0 && !b.cfg.ndjson {
			b.buf = append(b.buf, ',', '\n')
		}
		rec(id)
		if b.cfg.ndjson {
			b.buf = append(b.buf, '\n')
		}
	}
	return b.buf
}

// record writes {"id":id, ...} with a random subset of fields.
func (b *builder) record(id int64) {
	b.buf = append(b.buf, `{"id":`...)
	b.buf = strconv.AppendInt(b.buf, id, 10)
	b.someFields(1)
	b.buf = append(b.buf, '}')
}

// someFields writes each field with probability 2/3, in table order so keys
// never repeat, each preceded by a comma. Values come from the configured
// schema; depth only matters to chaos.
func (b *builder) someFields(depth int) {
	for f := range fields {
		if b.r.IntN(3) == 0 {
			continue
		}
		b.nextKey(fields[f].key)
		if b.cfg.schema == chaos {
			b.chaosValue(depth)
		} else {
			fields[f].typed(b)
		}
	}
}

// strN writes a quoted JSON string of n runes drawn from alphabet.
func (b *builder) strN(alphabet []rune, n int) {
	b.buf = append(b.buf, '"')
	b.chars(alphabet, n)
	b.buf = append(b.buf, '"')
}

// chars writes n runes drawn from alphabet, escaped for a JSON string body.
func (b *builder) chars(alphabet []rune, n int) {
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
}

// textLen is a short length, with an occasional long one.
func (b *builder) textLen() int {
	if b.r.IntN(50) == 0 {
		return 200 + b.r.IntN(2000)
	}
	return 1 + b.r.IntN(24)
}

// key writes "k": (the first key of an object).
func (b *builder) key(k string) {
	b.buf = append(b.buf, '"')
	b.buf = append(b.buf, k...)
	b.buf = append(b.buf, '"', ':')
}

// nextKey writes ,"k": (every key after the first).
func (b *builder) nextKey(k string) {
	b.buf = append(b.buf, ',')
	b.key(k)
}
