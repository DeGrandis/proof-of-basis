package app

import (
	"encoding/json"
	"testing"
)

// The disclaimer is unaccepted on a new install, and stays accepted after.
func TestDisclaimer(t *testing.T) {
	_, h := newTest(t)
	accepted := func() string {
		var rep struct{ Settings map[string]string }
		json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
		return rep.Settings["disclaimerAccepted"]
	}
	if a := accepted(); a != "" {
		t.Fatalf("new install: %q", a)
	}
	if w := call(h, "POST", "/api/disclaimer", "{}"); w.Code != 200 {
		t.Fatalf("accept: %d %s", w.Code, w.Body)
	}
	if accepted() == "" {
		t.Error("not recorded")
	}
}
