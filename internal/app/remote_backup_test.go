package app

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/demo"
	"github.com/DeGrandis/proof-of-basis/internal/store"
)

func newRemoteTest(t *testing.T) (*store.Store, *Server, http.Handler) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetPasswordHash("test-only")
	st.AddSession(tokenHash("test-token"), time.Now())
	if err := demo.Seed(st, 10); err != nil {
		t.Fatal(err)
	}
	s := New(st, fstest.MapFS{})
	h := s.Handler()
	return st, s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "test-token"})
		h.ServeHTTP(w, r)
	})
}

func backupsIn(t *testing.T, dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestRemoteBackupToFolder(t *testing.T) {
	st, s, h := newRemoteTest(t)
	dir := t.TempDir()

	// Checks before anything is saved.
	for body, want := range map[string]string{
		`{"target":"folder","folderPath":"` + dir + `","passphrase":"short"}`:                       "at least 8",
		`{"target":"folder","folderPath":"relative","passphrase":"long enough"}`:                    "full path",
		`{"target":"folder","folderPath":"` + dir + `/missing","passphrase":"long enough"}`:         "no folder",
		`{"target":"s3","passphrase":"long enough"}`:                                                "bucket",
		`{"target":"folder","folderPath":"` + dir + `","passphrase":"long enough","every":"often"}`: "how often",
		`{"bogus":"x"}`: "unknown setting",
	} {
		if w := call(h, "POST", "/api/backup/remote", body); w.Code != 400 || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d %s, want %q", body, w.Code, w.Body.String(), want)
		}
	}
	var fresh map[string]any
	json.Unmarshal(call(h, "GET", "/api/backup/remote", "").Body.Bytes(), &fresh)
	if fresh["s3PathStyle"] != "true" {
		t.Errorf("bucket in the path should start on, got %v", fresh["s3PathStyle"])
	}
	if w := call(h, "POST", "/api/backup/remote/test", `{"target":"folder","folderPath":"`+dir+`","passphrase":"correct horse"}`); w.Code != 200 {
		t.Fatalf("test: %d %s", w.Code, w.Body.String())
	}
	if got := backupsIn(t, dir); len(got) != 0 {
		t.Errorf("the test left files behind: %v", got)
	}
	if v, _ := st.RemoteBackup(); v["target"] != "" {
		t.Error("testing saved the settings")
	}

	w := call(h, "POST", "/api/backup/remote", `{"target":"folder","folderPath":"`+dir+`","passphrase":"correct horse","every":"day","keep":"2"}`)
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "correct horse") {
		t.Error("the passphrase was sent back")
	}

	run := func(want string) remoteResult {
		t.Helper()
		w := call(h, "POST", "/api/backup/remote/run", `{}`)
		var r remoteResult
		json.Unmarshal(w.Body.Bytes(), &r)
		if w.Code != 200 || r.Result != want {
			t.Fatalf("run: %d %s, want %s", w.Code, w.Body.String(), want)
		}
		return r
	}
	// The first report makes a setting once (exportSecret); after that,
	// reading pages doesn't change the data, so a check is a no-op.
	call(h, "GET", "/api/report", "")
	first := run("uploaded")
	call(h, "GET", "/api/report", "")
	call(h, "GET", "/api/settings", "")
	call(h, "GET", "/api/backup/remote", "")
	if r := run("unchanged"); r.File != first.File {
		t.Errorf("unchanged run points at %s, want %s", r.File, first.File)
	}

	// The file is a normal encrypted backup of everything.
	b, err := os.ReadFile(filepath.Join(dir, first.File))
	if err != nil {
		t.Fatal(err)
	}
	tables, err := readBackup(b, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if len(tables["events"]) == 0 {
		t.Error("the backup has no events")
	}
	if _, ok := tables["remote_backup"]; ok || strings.Contains(string(b), "correct horse") {
		t.Error("the backup holds its own settings")
	}

	// A change uploads; keep=2 drops the oldest.
	time.Sleep(1100 * time.Millisecond)
	call(h, "POST", "/api/settings", `{"rateLong":"20"}`)
	run("uploaded")
	time.Sleep(1100 * time.Millisecond)
	call(h, "POST", "/api/settings", `{"rateLong":"15"}`)
	third := run("uploaded")
	if got := backupsIn(t, dir); len(got) != 2 || got[1] != third.File || got[0] == first.File {
		t.Errorf("kept %v", got)
	}

	// A new passphrase: the next run uploads the same data under it.
	time.Sleep(1100 * time.Millisecond)
	call(h, "POST", "/api/backup/remote", `{"passphrase":"new passphrase"}`)
	r := run("uploaded")
	b, _ = os.ReadFile(filepath.Join(dir, r.File))
	if _, err := readBackup(b, "new passphrase"); err != nil {
		t.Errorf("newest backup doesn't open with the new passphrase: %v", err)
	}
	run("unchanged")

	// Listing and downloading.
	var list struct{ Files []struct{ Name string } }
	json.Unmarshal(call(h, "GET", "/api/backup/remote/files", "").Body.Bytes(), &list)
	if len(list.Files) != 2 || list.Files[0].Name != r.File {
		t.Errorf("list: %+v", list.Files)
	}
	if w := call(h, "GET", "/api/backup/remote/files/"+r.File, ""); w.Code != 200 || w.Body.Len() != len(b) {
		t.Errorf("download: %d, %d bytes", w.Code, w.Body.Len())
	}
	if w := call(h, "GET", "/api/backup/remote/files/..%2Ft.db", ""); w.Code != 404 {
		t.Errorf("download outside the folder: %d", w.Code)
	}

	// The schedule: not due right after a run, due once lastCheck is old.
	before := len(backupsIn(t, dir))
	call(h, "POST", "/api/settings", `{"rateLong":"21"}`)
	s.remoteIfDue()
	if len(backupsIn(t, dir)) != before {
		t.Error("ran before it was due")
	}
	st.SetRemoteBackup(map[string]string{"lastCheck": time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)})
	s.remoteIfDue()
	if v, _ := st.RemoteBackup(); v["lastResult"] != "uploaded" {
		t.Errorf("scheduled run: %v %s", v["lastResult"], v["lastError"])
	}

	// A restore leaves the backup settings alone; a wipe forgets them.
	if err := st.Restore(tables); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.RemoteBackup(); v["passphrase"] != "new passphrase" {
		t.Error("a restore changed the backup settings")
	}
	call(h, "POST", "/api/wipe", `{"confirm":"WIPE"}`)
	if v, _ := st.RemoteBackup(); len(v) != 0 {
		t.Errorf("wipe kept %v", v)
	}
}

func TestRemoteBackupFailureIsReported(t *testing.T) {
	st, s, h := newRemoteTest(t)
	dir := t.TempDir()
	call(h, "POST", "/api/backup/remote", `{"target":"folder","folderPath":"`+dir+`","passphrase":"correct horse","every":"week"}`)
	os.RemoveAll(dir)
	s.remoteIfDue()
	v, _ := st.RemoteBackup()
	if v["lastResult"] != "failed" || v["lastError"] == "" {
		t.Fatalf("got %v", v)
	}
	// A failure is tried again within the hour, not a week later.
	os.MkdirAll(dir, 0o700)
	st.SetRemoteBackup(map[string]string{"lastCheck": time.Now().Add(-61 * time.Minute).UTC().Format(time.RFC3339)})
	s.remoteIfDue()
	if v, _ := st.RemoteBackup(); v["lastResult"] != "uploaded" {
		t.Errorf("retry: %v", v["lastResult"])
	}
}

func TestRemoteBackupOffInDemo(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(st, fstest.MapFS{})
	s.SetDemo(&DemoOptions{})
	h := s.Handler()
	for _, c := range [][2]string{{"GET", "/api/backup/remote"}, {"POST", "/api/backup/remote"}, {"POST", "/api/backup/remote/run"}, {"POST", "/api/backup/remote/test"}, {"GET", "/api/backup/remote/files"}} {
		if w := call(h, c[0], c[1], `{"target":"folder","folderPath":"/tmp","passphrase":"long enough"}`); w.Code != 403 {
			t.Errorf("%s %s in the demo: %d", c[0], c[1], w.Code)
		}
	}
}
