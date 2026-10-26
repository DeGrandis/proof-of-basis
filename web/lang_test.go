package web

import (
	"io/fs"
	"regexp"
	"testing"
)

// Every language file translates the same English text, with the same
// patterns, so a string added to one is added to all.
func TestLanguagesMatch(t *testing.T) {
	key := regexp.MustCompile(`(?m)^  ("(?:[^"\\]|\\.)*"): `)
	pattern := regexp.MustCompile(`(?m)^  \[(/.*/u), `)
	read := func(code string) (keys, pats []string) {
		b, err := fs.ReadFile(Static(), "lang/"+code+".js")
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range key.FindAllStringSubmatch(string(b), -1) {
			keys = append(keys, m[1])
		}
		for _, m := range pattern.FindAllStringSubmatch(string(b), -1) {
			pats = append(pats, m[1])
		}
		return keys, pats
	}
	wantKeys, wantPats := read("es")
	if len(wantKeys) == 0 || len(wantPats) == 0 {
		t.Fatal("no strings or patterns found in lang/es.js")
	}
	for _, code := range []string{"fr", "de", "pl", "ru"} {
		keys, pats := read(code)
		if len(keys) != len(wantKeys) || len(pats) != len(wantPats) {
			t.Errorf("%s: %d strings and %d patterns, es has %d and %d", code, len(keys), len(pats), len(wantKeys), len(wantPats))
			continue
		}
		for i := range keys {
			if keys[i] != wantKeys[i] {
				t.Errorf("%s: string %d is %s, es has %s", code, i, keys[i], wantKeys[i])
				break
			}
		}
		for i := range pats {
			if pats[i] != wantPats[i] {
				t.Errorf("%s: pattern %d is %s, es has %s", code, i, pats[i], wantPats[i])
				break
			}
		}
	}
}
