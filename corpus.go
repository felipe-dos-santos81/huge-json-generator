package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const casesPerChunk = 256

// corpusCase is one category of parser test case. verdict follows RFC 8259
// in JSONTestSuite style: y must be accepted, n must be rejected, i is
// implementation-defined.
type corpusCase struct {
	verdict  byte
	category string
	gen      func(c *corpusSource, doc []byte) []byte
}

var catalogue = []corpusCase{
	// y: must accept
	{'y', "top_level_value", func(c *corpusSource, doc []byte) []byte {
		start := len(doc)
		for {
			doc = c.buildInto(doc[:start], func() { c.chaosValue(math.MaxInt) })
			if doc[start] != '[' && doc[start] != '{' {
				return doc // a scalar: string, number, bool or null
			}
		}
	}},
	{'y', "whitespace", (*corpusSource).whitespace},
	{'y', "escapes", (*corpusSource).validEscapes},
	{'y', "numbers", func(c *corpusSource, doc []byte) []byte { return c.list(doc, validNumbers) }},
	{'y', "unicode", (*corpusSource).unicode},
	{'y', "empty_containers", func(c *corpusSource, doc []byte) []byte { return c.buildInto(doc, func() { c.empties(4) }) }},
	{'y', "record", (*corpusSource).valid},

	// n: must reject. Most mutate a valid record, so the defect sits in realistic content.
	{'n', "trailing_comma", func(c *corpusSource, doc []byte) []byte {
		return c.atStructural(doc, "]}", func(d []byte, p int) []byte {
			if d[p-1] == '[' || d[p-1] == '{' {
				return d // empty container: a comma here is a lone comma, not a trailing one
			}
			return slices.Insert(d, p, ',')
		})
	}},
	{'n', "leading_comma", func(c *corpusSource, doc []byte) []byte { return c.atStructural(doc, "[{", insert(1, ",")) }},
	{'n', "missing_comma", func(c *corpusSource, doc []byte) []byte { return c.atStructural(doc, ",", remove) }},
	{'n', "missing_colon", func(c *corpusSource, doc []byte) []byte { return c.atStructural(doc, ":", remove) }},
	{'n', "unclosed", func(c *corpusSource, doc []byte) []byte { d := c.valid(doc); return d[:len(d)-1] }},
	{'n', "extra_close", func(c *corpusSource, doc []byte) []byte { return append(c.valid(doc), c.pick([]string{"]", "}"})...) }},
	{'n', "trailing_garbage", func(c *corpusSource, doc []byte) []byte {
		return append(c.valid(doc), c.pick([]string{" x", " 1", ",{}", `"a"`})...)
	}},
	{'n', "empty_document", func(c *corpusSource, doc []byte) []byte { return append(doc, c.pick([]string{"", " ", "\n\t "})...) }},
	{'n', "single_quote", func(c *corpusSource, doc []byte) []byte {
		return c.atStructural(doc, `"`, func(d []byte, p int) []byte {
			// p opens a string: swap its closing quote too, so a lenient parser
			// that accepts '...' sees a well-formed single-quoted string.
			q := p + 1
			for d[q] != '"' {
				if d[q] == '\\' {
					q++
				}
				q++
			}
			d[p], d[q] = '\'', '\''
			return d
		})
	}},
	{'n', "unquoted_key", func(c *corpusSource, doc []byte) []byte {
		return c.buildInto(append(doc, '{'), func() { c.letters(1 + c.r.IntN(8)); c.buf = append(c.buf, ':', '1', '}') })
	}},
	{'n', "comment", func(c *corpusSource, doc []byte) []byte {
		return c.atStructural(doc, "[{,:", insert(1, c.pick([]string{"/*c*/", "//c\n"})))
	}},
	{'n', "bad_escape", func(c *corpusSource, doc []byte) []byte {
		if c.r.IntN(6) == 0 { // truncated: the string ends two hex digits into an escape
			return c.inString(doc, `\u12`, func(b byte) bool { return b == '"' })
		}
		return c.insideString(doc, c.pick([]string{`\x41`, `\a`, `\u12g4`, `\U0041`, `\'`}))
	}},
	{'n', "raw_control", func(c *corpusSource, doc []byte) []byte { return c.insideString(doc, string(rune(c.r.IntN(0x20)))) }},
	{'n', "bad_number", func(c *corpusSource, doc []byte) []byte {
		return c.list(doc, []string{"01", "-01", "00", ".5", "1.", "+1", "-", "1e", "1e+", "0x10", "1.e2", "NaN", "Infinity", "-Infinity"})
	}},
	{'n', "bad_literal", func(c *corpusSource, doc []byte) []byte {
		return c.list(doc, []string{"tru", "nul", "fals", "True", "NULL", "undefined"})
	}},
	{'n', "invalid_utf8", func(c *corpusSource, doc []byte) []byte {
		return c.insideString(doc, c.pick([]string{"\x80", "\xc0\xaf", "\xe2\x82", "\xed\xa0\x80", "\xf4\x90\x80\x80", "\xff"}))
	}},

	// i: implementation-defined (RFC 8259 leaves the outcome to the parser)
	{'i', "duplicate_keys", func(c *corpusSource, doc []byte) []byte {
		start := len(doc)
		doc = c.valid(doc) // starts with {"id":
		return slices.Insert(doc, start+1, []byte(`"id":`+strconv.Itoa(c.r.IntN(1000))+",")...)
	}},
	{'i', "lone_surrogate", (*corpusSource).loneSurrogates},
	{'i', "huge_exponent", func(c *corpusSource, doc []byte) []byte {
		return c.list(doc, []string{"1e400", "-1e400", "1e-400", "123e99999", "1E+999"})
	}},
	{'i', "big_integer", func(c *corpusSource, doc []byte) []byte { return c.number(doc, 21+c.r.IntN(20)) }},
	{'i', "long_number", func(c *corpusSource, doc []byte) []byte { return c.number(doc, 1000) }},
	{'i', "bom", func(c *corpusSource, doc []byte) []byte { return c.valid(append(doc, "\xef\xbb\xbf"...)) }},
	{'i', "deep_nesting", func(c *corpusSource, doc []byte) []byte {
		k := 10000 + c.r.IntN(2000)
		return append(append(doc, strings.Repeat("[", k)...), strings.Repeat("]", k)...)
	}},
}

var validNumbers = []string{
	"0", "-0", "0.0", "-0.0", "0e0", "0E+0", "1E+2", "1e-2", "2.5E-3", "-123.456e-7",
	"1.7976931348623157e308", "5e-324", "9223372036854775807", "-9223372036854775808",
}

// acceptable is the reference check for y and n verdicts: strict RFC 8259
// syntax (json.Valid) plus valid UTF-8, which json.Valid does not check
// inside strings.
func acceptable(doc []byte) bool { return json.Valid(doc) && utf8.Valid(doc) }

// corpusSource writes NDJSON test cases {"id","verdict","category","doc_b64"}.
type corpusSource struct {
	*builder
	doc []byte // the case under construction
	tmp []byte // scratch record for generators that rebuild one
}

// newCorpus returns a corpus source. parseConfig marks the corpus as NDJSON,
// so the builder's framing (header and trailer) is empty.
func newCorpus(cfg config) *corpusSource { return &corpusSource{builder: newBuilder(cfg)} }

func (c *corpusSource) summary(n int64) string { return fmt.Sprintf("%d cases", n*casesPerChunk) }

func (c *corpusSource) chunk(dst []byte, i int64) []byte {
	c.pcg.Seed(c.cfg.seed, uint64(i))
	for id := i * casesPerChunk; id < (i+1)*casesPerChunk; id++ {
		dst = c.oneCase(dst, id)
	}
	return dst
}

// oneCase draws categories until one yields a document whose y/n verdict the
// reference check confirms (a mutation can land somewhere harmless), then
// appends its NDJSON line. Redraws come from the same seeded stream, so the
// output stays deterministic.
func (c *corpusSource) oneCase(dst []byte, id int64) []byte {
	var cc corpusCase
	for {
		cc = catalogue[c.r.IntN(len(catalogue))]
		c.doc = cc.gen(c, c.doc[:0])
		if cc.verdict == 'i' || acceptable(c.doc) == (cc.verdict == 'y') {
			break
		}
	}
	dst = append(dst, `{"id":`...)
	dst = strconv.AppendInt(dst, id, 10)
	dst = append(dst, `,"verdict":"`...)
	dst = append(dst, cc.verdict)
	dst = append(dst, `","category":"`...)
	dst = append(dst, cc.category...)
	dst = append(dst, `","doc_b64":"`...)
	dst = base64.StdEncoding.AppendEncode(dst, c.doc)
	return append(dst, "\"}\n"...)
}

// buildInto makes doc the builder's buffer, runs write against it, and returns
// the grown buffer.
func (c *corpusSource) buildInto(doc []byte, write func()) []byte {
	c.buf = doc
	write()
	return c.buf
}

// valid appends a small valid record, chaos or typed.
func (c *corpusSource) valid(doc []byte) []byte {
	c.cfg.schema = []schema{chaos, typed}[c.r.IntN(2)]
	return c.buildInto(doc, func() { c.record(int64(c.r.IntN(1000))) })
}

func (c *corpusSource) pick(options []string) string { return options[c.r.IntN(len(options))] }

// list appends [a,b,...] of one to four picks from options.
func (c *corpusSource) list(doc []byte, options []string) []byte {
	doc = append(doc, '[')
	for i := range 1 + c.r.IntN(4) {
		if i > 0 {
			doc = append(doc, ',')
		}
		doc = append(doc, c.pick(options)...)
	}
	return append(doc, ']')
}

// number appends [d...] with n digits, the first non-zero.
func (c *corpusSource) number(doc []byte, n int) []byte {
	doc = append(doc, '[', byte('1'+c.r.IntN(9)))
	for range n - 1 {
		doc = append(doc, byte('0'+c.r.IntN(10)))
	}
	return append(doc, ']')
}

// empties writes a random nest of empty arrays and objects.
func (c *corpusSource) empties(depth int) {
	switch k := c.r.IntN(4); {
	case depth == 0 || k == 0:
		c.buf = append(c.buf, "[]"...)
	case k == 1:
		c.buf = append(c.buf, "{}"...)
	case k == 2:
		c.buf = append(c.buf, '[')
		for i := range 1 + c.r.IntN(3) {
			if i > 0 {
				c.buf = append(c.buf, ',')
			}
			c.empties(depth - 1)
		}
		c.buf = append(c.buf, ']')
	default:
		c.buf = append(c.buf, `{"a":`...)
		c.empties(depth - 1)
		c.buf = append(c.buf, '}')
	}
}

// whitespace appends a valid record with a run of all four JSON whitespace
// characters, in random order, on both sides of every structural token and
// around the whole document, in one forward pass.
func (c *corpusSource) whitespace(doc []byte) []byte {
	c.tmp = c.valid(c.tmp[:0])
	rec, prev := c.tmp, 0
	doc = c.ws(doc)
	for _, p := range c.structuralIn(rec, "{}[],:") {
		doc = append(doc, rec[prev:p]...)
		doc = c.ws(doc)
		doc = append(doc, rec[p])
		doc = c.ws(doc)
		prev = p + 1
	}
	doc = append(doc, rec[prev:]...)
	return c.ws(doc)
}

// ws appends the four JSON whitespace characters in random order.
func (c *corpusSource) ws(dst []byte) []byte {
	w := [4]byte{' ', '\t', '\n', '\r'}
	c.r.Shuffle(len(w), func(i, j int) { w[i], w[j] = w[j], w[i] })
	return append(dst, w[:]...)
}

// validEscapes appends strings of every escape kind, always including the
// NUL escape and an escaped solidus.
func (c *corpusSource) validEscapes(doc []byte) []byte {
	return c.buildInto(append(doc, `["`...), func() {
		c.uEscape(0)
		c.buf = append(c.buf, `\/`...)
		for range 1 + c.r.IntN(20) {
			c.escape()
		}
		c.buf = append(c.buf, `"]`...)
	})
}

// unicodeRunes: a 4-byte emoji, noncharacters (U+FFFF, U+FDD0, U+10FFFF),
// a BOM (ordinary inside a string) and a CJK character.
var unicodeRunes = []rune{0x1F600, 0xFFFF, 0xFDD0, 0x10FFFF, 0xFEFF, 0x65E5}

// unicode appends a string of raw 4-byte UTF-8 and noncharacters.
func (c *corpusSource) unicode(doc []byte) []byte {
	doc = append(doc, `["`...)
	for range 1 + c.r.IntN(10) {
		doc = utf8.AppendRune(doc, unicodeRunes[c.r.IntN(len(unicodeRunes))])
	}
	return append(doc, `"]`...)
}

// atStructural appends a valid record and applies edit at one random byte
// from set outside its strings (p is an offset into the whole doc). With no
// such byte the record is returned unchanged, and the verdict check redraws.
func (c *corpusSource) atStructural(doc []byte, set string, edit func(doc []byte, p int) []byte) []byte {
	start := len(doc)
	doc = c.valid(doc)
	pos := c.structuralIn(doc[start:], set)
	if len(pos) == 0 {
		return doc
	}
	return edit(doc, start+pos[c.r.IntN(len(pos))])
}

// insert returns an edit that inserts s at p+delta.
func insert(delta int, s string) func([]byte, int) []byte {
	return func(doc []byte, p int) []byte { return slices.Insert(doc, p+delta, []byte(s)...) }
}

// remove is an edit that deletes the byte at p.
func remove(doc []byte, p int) []byte { return slices.Delete(doc, p, p+1) }

// structuralIn returns the offsets of bytes from set outside strings; a
// string-opening quote counts as structural.
func (c *corpusSource) structuralIn(doc []byte, set string) []int {
	structural, _ := scan(doc)
	var pos []int
	for _, p := range structural {
		if strings.IndexByte(set, doc[p]) >= 0 {
			pos = append(pos, p)
		}
	}
	return pos
}

// insideString appends a valid record with s inserted at a rune boundary
// inside one of its strings.
func (c *corpusSource) insideString(doc []byte, s string) []byte {
	return c.inString(doc, s, func(byte) bool { return true })
}

// inString appends a valid record with s inserted at one in-string insertion
// point whose byte satisfies keep (a closing quote, say).
func (c *corpusSource) inString(doc []byte, s string, keep func(b byte) bool) []byte {
	start := len(doc)
	doc = c.valid(doc)
	_, inside := scan(doc[start:])
	inside = slices.DeleteFunc(inside, func(p int) bool { return !keep(doc[start+p]) })
	if len(inside) == 0 {
		return doc
	}
	return slices.Insert(doc, start+inside[c.r.IntN(len(inside))], []byte(s)...)
}

// loneSurrogates appends [...] of one to four strings holding unpaired,
// reversed, or interrupted surrogate escapes, built with uEscape.
func (c *corpusSource) loneSurrogates(doc []byte) []byte {
	return c.buildInto(append(doc, '['), func() {
		for i := range 1 + c.r.IntN(4) {
			if i > 0 {
				c.buf = append(c.buf, ',')
			}
			c.buf = append(c.buf, '"')
			switch c.r.IntN(6) {
			case 0:
				c.uEscape(0xD800)
			case 1:
				c.uEscape(0xDBFF)
			case 2:
				c.uEscape(0xDC00)
			case 3: // reversed pair
				c.uEscape(0xDC00)
				c.uEscape(0xD800)
			case 4: // high surrogate followed by a plain letter
				c.uEscape(0xD800)
				c.buf = append(c.buf, 'x')
			default: // high surrogate followed by a simple escape
				c.uEscape(0xD800)
				c.buf = append(c.buf, '\\', 'n')
			}
			c.buf = append(c.buf, '"')
		}
		c.buf = append(c.buf, ']')
	})
}

// scan returns the offsets of structural bytes ({}[],: and string-opening
// quotes) and of insertion points strictly inside strings: rune starts that
// do not split an escape sequence, including the four hex digits of a
// unicode escape.
func scan(doc []byte) (structural, inside []int) {
	inString, escaped, hexLeft := false, false, 0
	for p, ch := range doc {
		switch {
		case !inString:
			if strings.IndexByte(`{}[],:"`, ch) >= 0 {
				structural = append(structural, p)
			}
			inString = ch == '"'
		case hexLeft > 0:
			hexLeft--
		case escaped:
			escaped = false
			if ch == 'u' {
				hexLeft = 4
			}
		case ch == '\\':
			escaped = true
			inside = append(inside, p)
		case ch == '"':
			inside = append(inside, p)
			inString = false
		case utf8.RuneStart(ch):
			inside = append(inside, p)
		}
	}
	return structural, inside
}
