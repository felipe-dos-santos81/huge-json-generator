package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestCorpus(t *testing.T) {
	out := gen(t, "-corpus", "-size", "5MB", "-workers", "8")
	seen := map[string]bool{}
	for i, line := range bytes.Split(bytes.TrimSuffix(out, []byte("\n")), []byte("\n")) {
		var c struct {
			ID       int64
			Verdict  string
			Category string
			DocB64   string `json:"doc_b64"`
		}
		if err := json.Unmarshal(line, &c); err != nil || c.ID != int64(i) {
			t.Fatalf("line %d: %v (id %d)", i, err, c.ID)
		}
		doc, err := base64.StdEncoding.DecodeString(c.DocB64)
		if err != nil {
			t.Fatalf("case %d: bad base64: %v", c.ID, err)
		}
		if ok := acceptable(doc); (c.Verdict == "y" && !ok) || (c.Verdict == "n" && ok) {
			t.Fatalf("case %d %s/%s: reference check says acceptable=%v: %.80q", c.ID, c.Verdict, c.Category, ok, doc)
		}
		seen[c.Verdict+"/"+c.Category] = true
	}
	for _, cc := range catalogue {
		if !seen[string(cc.verdict)+"/"+cc.category] {
			t.Errorf("category %c/%s never generated", cc.verdict, cc.category)
		}
	}
}

func TestCorpusIgnoresWorkers(t *testing.T) {
	a := sha256.Sum256(gen(t, "-corpus", "-size", "2MB", "-workers", "1"))
	if b := sha256.Sum256(gen(t, "-corpus", "-size", "2MB", "-workers", "8")); a != b {
		t.Fatal("-workers changed the corpus")
	}
}

// The reference check must reject what json.Valid alone misses.
func TestAcceptable(t *testing.T) {
	for doc, want := range map[string]bool{
		`["ok"]`: true, "[\"\xff\"]": false, `[1,]`: false, `[01]`: false, "": false,
	} {
		if got := acceptable([]byte(doc)); got != want {
			t.Errorf("acceptable(%q) = %v, want %v", doc, got, want)
		}
	}
}

// Verdicts must hold even when every mutated record is all null or all empty.
func TestCorpusExtremeRates(t *testing.T) {
	for _, rates := range [][]string{{"-null", "1", "-empty", "0"}, {"-null", "0", "-empty", "1"}} {
		out := gen(t, append([]string{"-corpus", "-size", "300KB"}, rates...)...)
		for _, line := range bytes.Split(bytes.TrimSuffix(out, []byte("\n")), []byte("\n")) {
			var c struct {
				Verdict string
				DocB64  string `json:"doc_b64"`
			}
			if json.Unmarshal(line, &c) != nil {
				t.Fatalf("%v: bad line %q", rates, line)
			}
			doc, _ := base64.StdEncoding.DecodeString(c.DocB64)
			if ok := acceptable(doc); (c.Verdict == "y" && !ok) || (c.Verdict == "n" && ok) {
				t.Fatalf("%v: %s case, acceptable=%v: %.80q", rates, c.Verdict, ok, doc)
			}
		}
	}
}

// scan must not offer insertion points inside a unicode escape's hex digits.
func TestScanSkipsUnicodeEscapeDigits(t *testing.T) {
	doc := []byte(`["a\` + "u0041" + `b"]`) // ["a<backslash>u0041b"]
	_, inside := scan(doc)
	if want := []int{2, 3, 9, 10}; !slices.Equal(inside, want) {
		t.Fatalf("inside = %v, want %v", inside, want)
	}
}

// Each category's document must carry the defect its name promises.
func TestCorpusCategoriesHoldTheirDefect(t *testing.T) {
	truncated := 0
	out := gen(t, "-corpus", "-size", "3MB")
	for _, line := range bytes.Split(bytes.TrimSuffix(out, []byte("\n")), []byte("\n")) {
		var c struct {
			Category string
			DocB64   string `json:"doc_b64"`
		}
		if err := json.Unmarshal(line, &c); err != nil {
			t.Fatal(err)
		}
		doc, _ := base64.StdEncoding.DecodeString(c.DocB64)
		switch c.Category {
		case "trailing_comma":
			if bytes.Contains(doc, []byte("[,]")) || bytes.Contains(doc, []byte("{,}")) {
				t.Fatalf("trailing_comma holds a lone comma: %.80q", doc)
			}
		case "whitespace":
			for _, ws := range []byte(" \t\n\r") {
				if !bytes.Contains(doc, []byte{ws}) {
					t.Fatalf("whitespace lacks %q: %.80q", ws, doc)
				}
			}
			for _, p := range (&corpusSource{}).structuralIn(doc, "{}[],:") {
				if !isWS(doc[p-1]) || !isWS(doc[p+1]) {
					t.Fatalf("whitespace: token %q at %d not surrounded: %.80q", doc[p], p, doc)
				}
			}
		case "bad_escape":
			if bytes.Contains(doc, []byte(`\`+"u12\"")) {
				truncated++
			}
		case "single_quote":
			if !singleQuoted(doc) {
				t.Fatalf("single_quote holds no complete single-quoted string: %.80q", doc)
			}
		case "top_level_value":
			if doc[0] == '[' || doc[0] == '{' {
				t.Fatalf("top_level_value is not a scalar: %.80q", doc)
			}
		case "invalid_utf8", "raw_control":
			if brokenUnicodeEscape(doc) {
				t.Fatalf("%s broke a unicode escape: %.80q", c.Category, doc)
			}
		}
	}
	if truncated == 0 {
		t.Error("bad_escape never produced a truncated escape before a closing quote")
	}
}

// singleQuoted reports whether doc holds a pair of single quotes with no
// unescaped double quote between them, i.e. a string delimited by '...'.
func singleQuoted(doc []byte) bool {
	open := bytes.IndexByte(doc, '\'')
	if open < 0 {
		return false
	}
	for i := open + 1; i < len(doc); i++ {
		switch doc[i] {
		case '\\':
			i++
		case '"':
			return false
		case '\'':
			return true
		}
	}
	return false
}

// brokenUnicodeEscape reports whether a string in doc holds a unicode
// escape (backslash, u) whose four hex digits were interrupted.
func brokenUnicodeEscape(doc []byte) bool {
	inString, escaped := false, false
	for i, ch := range doc {
		switch {
		case !inString:
			inString = ch == '"'
		case escaped:
			escaped = false
			if ch == 'u' && (i+5 > len(doc) || !isHex(doc[i+1:i+5])) {
				return true
			}
		case ch == '\\':
			escaped = true
		case ch == '"':
			inString = false
		}
	}
	return false
}

func isHex(b []byte) bool {
	for _, ch := range b {
		if !strings.ContainsRune("0123456789abcdefABCDEF", rune(ch)) {
			return false
		}
	}
	return true
}

func isWS(b byte) bool { return strings.IndexByte(" \t\n\r", b) >= 0 }
