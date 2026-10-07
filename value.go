package main

import (
	"math/rand/v2"
	"strconv"
	"time"
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

// Typed strings keep escapes and multi-byte UTF-8 but drop the control
// characters that ingestion pipelines commonly reject.
var typedAlphabet = []rune(
	"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" +
		"      _-.,:;/@#" +
		"áçãéõüñß" + "日本語한국" + "😀🚀" +
		"\"\\/\n\t")

var nameAlphabet = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZáçãéõüñß")

var wordAlphabet = []rune("abcdefghijklmnopqrstuvwxyz")

const maxSafeInt = 1<<53 - 1 // largest integer a float64-based parser keeps exact

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
		if b.typed {
			b.typedRecord(id)
		} else {
			b.record(id)
		}
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

// ---- typed: every key path has exactly one JSON type (plus null) ----

// special writes null, or the type's empty form, per the configured rates
// and reports whether it did. Numbers and bools pass empty == "" and get a
// normal value on an empty roll.
func (b *builder) special(empty string) bool {
	x := b.r.Float64()
	if x < b.pNull {
		b.buf = append(b.buf, "null"...)
		return true
	}
	if x < b.pNull+b.pEmpty && empty != "" {
		b.buf = append(b.buf, empty...)
		return true
	}
	return false
}

// open writes null/empty and returns false, or writes c and returns true.
func (b *builder) open(c byte, empty string) bool {
	if b.special(empty) {
		return false
	}
	b.buf = append(b.buf, c)
	return true
}

func (b *builder) comma(k string) {
	b.buf = append(b.buf, ',')
	b.key(k)
}

func (b *builder) sep(i int) {
	if i > 0 {
		b.buf = append(b.buf, ',')
	}
}

func (b *builder) end(c byte) { b.buf = append(b.buf, c) }

func (b *builder) tStr(alphabet []rune, minLen, maxLen int) {
	if !b.special(`""`) {
		b.strN(alphabet, minLen+b.r.IntN(maxLen-minLen+1))
	}
}

func (b *builder) tEmail() {
	if b.special(`""`) {
		return
	}
	b.buf = append(b.buf, '"')
	for range 3 + b.r.IntN(10) {
		b.buf = append(b.buf, byte('a'+b.r.IntN(26)))
	}
	b.buf = append(b.buf, '@')
	for range 3 + b.r.IntN(8) {
		b.buf = append(b.buf, byte('a'+b.r.IntN(26)))
	}
	b.buf = append(b.buf, ".com\""...)
}

func (b *builder) tInt(n int) {
	if !b.special("") {
		b.buf = strconv.AppendInt(b.buf, int64(b.r.IntN(n)), 10)
	}
}

// tFloat always prints a decimal point, so loaders never infer an integer.
func (b *builder) tFloat(scale float64, decimals int) {
	if !b.special("") {
		b.buf = strconv.AppendFloat(b.buf, b.r.Float64()*scale, 'f', decimals, 64)
	}
}

func (b *builder) tBool() {
	if !b.special("") {
		b.buf = strconv.AppendBool(b.buf, b.r.IntN(2) == 0)
	}
}

func (b *builder) tTime() {
	if b.special(`""`) {
		return
	}
	const base = 946684800 // 2000-01-01T00:00:00Z
	t := time.Unix(base+b.r.Int64N(40*365*86400), 0).UTC()
	b.buf = append(b.buf, '"')
	b.buf = t.AppendFormat(b.buf, time.RFC3339)
	b.buf = append(b.buf, '"')
}

func (b *builder) tHex() {
	if b.special(`""`) {
		return
	}
	b.buf = append(b.buf, '"')
	for range 2 {
		v := b.r.Uint64()
		for s := 60; s >= 0; s -= 4 {
			b.buf = append(b.buf, "0123456789abcdef"[v>>s&0xf])
		}
	}
	b.buf = append(b.buf, '"')
}

func (b *builder) tCount() {
	if b.special("") {
		return
	}
	v := int64(b.r.IntN(10000))
	if b.r.IntN(20) == 0 {
		v = int64(b.r.Uint64N(2*maxSafeInt+1)) - maxSafeInt
	}
	b.buf = strconv.AppendInt(b.buf, v, 10)
}

func (b *builder) tPayload(depth int) {
	if !b.open('{', "{}") {
		return
	}
	b.key("k")
	b.tStr(wordAlphabet, 1, 12)
	b.comma("n")
	b.tInt(1000)
	if depth < b.maxDepth {
		b.comma("child")
		b.tPayload(depth + 1)
	}
	b.end('}')
}

func (b *builder) typedField(k string) {
	switch k {
	case "name":
		b.tStr(nameAlphabet, 3, 20)
	case "email":
		b.tEmail()
	case "score":
		b.tFloat(100, 4)
	case "active":
		b.tBool()
	case "tags":
		if b.open('[', "[]") {
			for i := range b.r.IntN(6) {
				b.sep(i)
				b.tStr(wordAlphabet, 1, 10)
			}
			b.end(']')
		}
	case "meta":
		if b.open('{', "{}") {
			b.key("source")
			b.tStr(wordAlphabet, 3, 12)
			b.comma("version")
			b.tInt(1000)
			b.comma("flags")
			if b.open('[', "[]") {
				for i := range b.r.IntN(4) {
					b.sep(i)
					b.tBool()
				}
				b.end(']')
			}
			b.end('}')
		}
	case "note":
		if !b.special(`""`) {
			b.strN(typedAlphabet, b.textLen())
		}
	case "price":
		b.tFloat(1000, 2)
	case "created_at":
		b.tTime()
	case "payload":
		b.tPayload(1)
	case "items":
		if b.open('[', "[]") {
			for i := range b.r.IntN(6) {
				b.sep(i)
				if b.open('{', "{}") {
					b.key("sku")
					b.tStr(wordAlphabet, 6, 10)
					b.comma("qty")
					b.tInt(100)
					b.comma("price")
					b.tFloat(1000, 2)
					b.end('}')
				}
			}
			b.end(']')
		}
	case "owner":
		if b.open('{', "{}") {
			b.key("name")
			b.tStr(nameAlphabet, 3, 20)
			b.comma("email")
			b.tEmail()
			b.end('}')
		}
	case "ref":
		b.tHex()
	case "count":
		b.tCount()
	case "extra":
		if b.open('[', "[]") {
			for i := range b.r.IntN(6) {
				b.sep(i)
				b.tInt(1000)
			}
			b.end(']')
		}
	case "données":
		b.tStr(typedAlphabet, 1, 24)
	}
}

func (b *builder) typedRecord(id int64) {
	b.buf = append(b.buf, `{"id":`...)
	b.buf = strconv.AppendInt(b.buf, id, 10)
	for _, k := range keys {
		if b.r.IntN(3) == 0 {
			continue
		}
		b.comma(k)
		b.typedField(k)
	}
	b.buf = append(b.buf, '}')
}
