package main

import (
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"time"
)

const maxSafeInt = 1<<53 - 1 // largest integer a float64-based parser keeps exact

// fields lists every record key in output order with its typed generator.
// Chaos mode uses only the keys. Adding a key is this one edit.
var fields = []struct {
	key   string
	typed func(*builder)
}{
	{"name", func(b *builder) { b.typedStr(nameAlphabet, 3, 20) }},
	{"email", (*builder).typedEmail},
	{"score", func(b *builder) { b.typedFixed(100, 4) }},
	{"active", (*builder).typedBool},
	{"tags", func(b *builder) { b.typedArray(6, func() { b.typedStr(wordAlphabet, 1, 10) }) }},
	{"meta", func(b *builder) {
		b.typedObject(func() {
			b.key("source")
			b.typedStr(wordAlphabet, 3, 12)
			b.nextKey("version")
			b.typedInt(1000)
			b.nextKey("flags")
			b.typedArray(4, b.typedBool)
		})
	}},
	{"note", func(b *builder) {
		if !b.nullOrEmpty(`""`) {
			b.strN(typedAlphabet, b.textLen())
		}
	}},
	{"price", func(b *builder) { b.typedFixed(1000, 2) }},
	{"created_at", (*builder).typedTime},
	{"payload", func(b *builder) { b.typedPayload(1) }},
	{"items", func(b *builder) {
		b.typedArray(6, func() {
			b.typedObject(func() {
				b.key("sku")
				b.typedStr(wordAlphabet, 6, 10)
				b.nextKey("qty")
				b.typedInt(100)
				b.nextKey("price")
				b.typedFixed(1000, 2)
			})
		})
	}},
	{"owner", func(b *builder) {
		b.typedObject(func() {
			b.key("name")
			b.typedStr(nameAlphabet, 3, 20)
			b.nextKey("email")
			b.typedEmail()
		})
	}},
	{"ref", (*builder).typedHex},
	{"count", (*builder).typedCount},
	{"extra", func(b *builder) { b.typedArray(6, func() { b.typedInt(1000) }) }},
	{"données", func(b *builder) { b.typedStr(typedAlphabet, 1, 24) }},
}

// roll draws one value's fate: null below -null, empty in the next -empty band.
// Every generator rolls exactly once per value, so both schemas share it.
func (b *builder) roll() (isNull, isEmpty bool) {
	x := b.r.Float64()
	return x < b.cfg.pNull, x >= b.cfg.pNull && x < b.cfg.pNull+b.cfg.pEmpty
}

// nullOrEmpty writes null, or empty (the type's empty form: "", [] or {}), at
// the configured rates and reports whether it did.
func (b *builder) nullOrEmpty(empty string) bool {
	switch isNull, isEmpty := b.roll(); {
	case isNull:
		b.buf = append(b.buf, "null"...)
	case isEmpty:
		b.buf = append(b.buf, empty...)
	default:
		return false
	}
	return true
}

// null writes null at the -null rate and reports whether it did. Numbers and
// bools have no empty form, so an empty roll falls through to a normal value.
func (b *builder) null() bool {
	if isNull, _ := b.roll(); isNull {
		b.buf = append(b.buf, "null"...)
		return true
	}
	return false
}

// typedArray writes null, [], or an array of up to maxLen-1 elements.
func (b *builder) typedArray(maxLen int, elem func()) {
	if b.nullOrEmpty("[]") {
		return
	}
	b.buf = append(b.buf, '[')
	for i := range b.r.IntN(maxLen) {
		if i > 0 {
			b.buf = append(b.buf, ',')
		}
		elem()
	}
	b.buf = append(b.buf, ']')
}

// typedObject writes null, {}, or {body}; body writes every key in order.
func (b *builder) typedObject(body func()) {
	if b.nullOrEmpty("{}") {
		return
	}
	b.buf = append(b.buf, '{')
	body()
	b.buf = append(b.buf, '}')
}

// typedStr writes null, "", or a string of minLen..maxLen runes from alphabet.
func (b *builder) typedStr(alphabet []rune, minLen, maxLen int) {
	if !b.nullOrEmpty(`""`) {
		b.strN(alphabet, minLen+b.r.IntN(maxLen-minLen+1))
	}
}

// letters writes n random lowercase ASCII letters (unquoted).
func (b *builder) letters(n int) {
	for range n {
		b.buf = append(b.buf, byte('a'+b.r.IntN(26)))
	}
}

func (b *builder) typedEmail() {
	if b.nullOrEmpty(`""`) {
		return
	}
	b.buf = append(b.buf, '"')
	b.letters(3 + b.r.IntN(10))
	b.buf = append(b.buf, '@')
	b.letters(3 + b.r.IntN(8))
	b.buf = append(b.buf, ".com\""...)
}

func (b *builder) typedInt(n int) {
	if !b.null() {
		b.buf = strconv.AppendInt(b.buf, int64(b.r.IntN(n)), 10)
	}
}

// typedFixed writes a uniform value in [0, limit) with exactly decimals (>= 1)
// fractional digits. Drawing an integer avoids rounding up to limit, and the
// decimal point keeps loaders from inferring an integer.
func (b *builder) typedFixed(limit, decimals int) {
	if b.null() {
		return
	}
	unit := 1
	for range decimals {
		unit *= 10
	}
	n := b.r.IntN(limit * unit)
	b.buf = strconv.AppendInt(b.buf, int64(n/unit), 10)
	b.buf = append(b.buf, '.')
	for d := unit / 10; d > 0; d /= 10 {
		b.buf = append(b.buf, byte('0'+n%unit/d%10))
	}
}

func (b *builder) typedBool() {
	if !b.null() {
		b.buf = strconv.AppendBool(b.buf, b.r.IntN(2) == 0)
	}
}

// typedTime writes an RFC 3339 UTC timestamp between 2000 and 2040.
func (b *builder) typedTime() {
	if b.nullOrEmpty(`""`) {
		return
	}
	const base = 946684800 // 2000-01-01T00:00:00Z
	t := time.Unix(base+b.r.Int64N(40*365*86400), 0).UTC()
	b.buf = append(b.buf, '"')
	b.buf = t.AppendFormat(b.buf, time.RFC3339)
	b.buf = append(b.buf, '"')
}

// typedHex writes 32 lowercase hex characters.
func (b *builder) typedHex() {
	if b.nullOrEmpty(`""`) {
		return
	}
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[:8], b.r.Uint64())
	binary.BigEndian.PutUint64(raw[8:], b.r.Uint64())
	b.buf = append(b.buf, '"')
	b.buf = hex.AppendEncode(b.buf, raw[:])
	b.buf = append(b.buf, '"')
}

// typedCount writes a small int, or 1 time in 20 any int within ±(2^53−1).
func (b *builder) typedCount() {
	if b.null() {
		return
	}
	v := int64(b.r.IntN(10000))
	if b.r.IntN(20) == 0 {
		v = int64(b.r.Uint64N(2*maxSafeInt+1)) - maxSafeInt
	}
	b.buf = strconv.AppendInt(b.buf, v, 10)
}

// typedPayload writes {k, n, child} recursively; child is omitted at -depth.
func (b *builder) typedPayload(depth int) {
	b.typedObject(func() {
		b.key("k")
		b.typedStr(wordAlphabet, 1, 12)
		b.nextKey("n")
		b.typedInt(1000)
		if depth < b.cfg.depth {
			b.nextKey("child")
			b.typedPayload(depth + 1)
		}
	})
}
