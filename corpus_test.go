package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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
	a := sha256.Sum256(gen(t, "-corpus", "-size", "1MB", "-workers", "1"))
	if b := sha256.Sum256(gen(t, "-corpus", "-size", "1MB", "-workers", "8")); a != b {
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
