package store

import (
	"path/filepath"
	"testing"
	"time"

	"btctax/internal/importers"
)

func TestOverlappingImportsDoNotDuplicate(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	acct, _ := st.ExchangeAccount("River", "river")
	ev := func(uid string) importers.Event {
		return importers.Event{Kind: importers.Buy, Time: time.Unix(1, 0), Sats: 1, UID: uid}
	}
	if _, skipped, err := st.AddImport(acct, "river", "jan.csv", "a", []importers.Event{ev("r:1"), ev("r:2")}, nil); err != nil || skipped != 0 {
		t.Fatal(skipped, err)
	}
	_, skipped, err := st.AddImport(acct, "river", "jan-feb.csv", "b", []importers.Event{ev("r:1"), ev("r:2"), ev("r:3")}, nil)
	if err != nil || skipped != 2 {
		t.Fatal(skipped, err)
	}
	if _, _, err := st.AddImport(acct, "river", "jan-feb.csv", "b", nil, nil); err != ErrDuplicate {
		t.Fatal("same file accepted twice")
	}
	all, _ := st.ExchangeEvents()
	if len(all) != 3 {
		t.Errorf("%d events", len(all))
	}
}
