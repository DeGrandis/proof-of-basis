package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/offsite"
)

// Advanced backup: an encrypted backup file sent on a schedule to an
// S3-compatible bucket or a folder (a network share mounted into the
// container). Each run first compares the data's hash with the newest
// backup already there and uploads only when they differ, so a schedule
// costs one listing while nothing changes. Its settings live in their own
// table, outside backups and restores.

type remoteBackups struct {
	mu      sync.Mutex // one run at a time
	running atomic.Bool
}

// The backup's own files: the time it was made and the first 16 hex digits
// of the data's sha256, which is how a run tells whether anything changed.
var remoteName = regexp.MustCompile(`^proof-of-basis-(\d{8}-\d{6})Z-([0-9a-f]{16})\.json$`)

const remoteTestFile = "proof-of-basis-connection-test.txt"

var remoteEvery = map[string]time.Duration{"off": 0, "hour": time.Hour, "day": 24 * time.Hour, "week": 7 * 24 * time.Hour}

// remoteShown are the settings sent back to the page; the secret key and
// the passphrase are write-only.
var remoteShown = []string{"target", "s3Endpoint", "s3Region", "s3Bucket", "s3Prefix", "s3PathStyle", "s3AccessKey", "folderPath", "every", "keep"}

var remoteState = []string{"lastCheck", "lastUpload", "lastFile", "lastResult", "lastError"}

const remoteKeepDefault = 10

func remoteTarget(cfg map[string]string) (offsite.Target, string, error) {
	switch cfg["target"] {
	case "s3":
		c := &offsite.S3{Endpoint: strings.TrimSpace(cfg["s3Endpoint"]), Region: strings.TrimSpace(cfg["s3Region"]), Bucket: strings.TrimSpace(cfg["s3Bucket"]),
			Prefix: strings.Trim(strings.TrimSpace(cfg["s3Prefix"]), "/"), PathStyle: cfg["s3PathStyle"] != "false", AccessKey: strings.TrimSpace(cfg["s3AccessKey"]), SecretKey: cfg["s3SecretKey"]}
		if err := c.Check(); err != nil {
			return nil, "", err
		}
		host := "AWS"
		if u, err := url.Parse(c.Endpoint); err == nil && c.Endpoint != "" {
			host = u.Host
		}
		where := "bucket " + c.Bucket
		if c.Prefix != "" {
			where += "/" + c.Prefix
		}
		return c, where + " on " + host, nil
	case "folder":
		dir := strings.TrimSpace(cfg["folderPath"])
		if !filepath.IsAbs(dir) {
			return nil, "", errors.New("the folder needs a full path, for example /backup")
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return nil, "", fmt.Errorf("there's no folder at %s. In Docker, mount your share there first (see the ⓘ)", dir)
		}
		return offsite.Folder{Dir: dir}, "folder " + dir, nil
	}
	return nil, "", errors.New("advanced backup is off")
}

func (s *Server) getRemoteBackup(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.st.RemoteBackup()
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, s.remoteView(cfg))
}

func (s *Server) remoteView(cfg map[string]string) map[string]any {
	out := map[string]any{}
	for _, k := range append(remoteShown, remoteState...) {
		out[k] = cfg[k]
	}
	if out["every"] == "" {
		out["every"] = "off"
	}
	if out["keep"] == "" {
		out["keep"] = strconv.Itoa(remoteKeepDefault)
	}
	if out["s3PathStyle"] == "" { // works with nearly every provider, and self-hosted ones need it
		out["s3PathStyle"] = "true"
	}
	if out["folderPath"] == "" {
		out["folderPath"] = "/backup"
	}
	out["s3SecretSet"] = cfg["s3SecretKey"] != ""
	out["passphraseSet"] = cfg["passphrase"] != ""
	out["running"] = s.backups.running.Load()
	return out
}

// mergeRemote applies the page's form to the saved settings and checks
// the result. A blank secret keeps the saved one.
func mergeRemote(cfg, in map[string]string) (map[string]string, error) {
	next := map[string]string{}
	for k, v := range cfg {
		next[k] = v
	}
	for k, v := range in {
		switch k {
		case "s3SecretKey", "passphrase":
			if v != "" {
				next[k] = v
			}
		case "target", "s3Endpoint", "s3Region", "s3Bucket", "s3Prefix", "s3PathStyle", "s3AccessKey", "folderPath", "every", "keep":
			next[k] = strings.TrimSpace(v)
		default:
			return nil, fmt.Errorf("unknown setting %q", k)
		}
	}
	if next["every"] == "" {
		next["every"] = "off"
	}
	if _, ok := remoteEvery[next["every"]]; !ok {
		return nil, errors.New("how often must be off, hour, day or week")
	}
	if k := next["keep"]; k != "" {
		if n, err := strconv.Atoi(k); err != nil || n < 1 || n > 1000 {
			return nil, errors.New("keep between 1 and 1000 backups")
		}
	}
	if next["target"] == "" {
		return next, nil
	}
	if next["target"] != "s3" && next["target"] != "folder" {
		return nil, errors.New("send backups to s3 or a folder")
	}
	if len(next["passphrase"]) < 8 {
		return nil, errors.New("choose a passphrase of at least 8 characters: backups sent elsewhere are always encrypted")
	}
	if _, _, err := remoteTarget(next); err != nil {
		return nil, err
	}
	return next, nil
}

func (s *Server) postRemoteBackup(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	if err := decode(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	cfg, err := s.st.RemoteBackup()
	if err != nil {
		fail(w, 500, err)
		return
	}
	if in["forget"] == "true" {
		gone := map[string]string{}
		for k := range cfg {
			gone[k] = ""
		}
		if err := s.st.SetRemoteBackup(gone); err != nil {
			fail(w, 500, err)
			return
		}
		s.note("backup", "Turned off advanced backup and forgot its keys")
		writeJSON(w, s.remoteView(map[string]string{}))
		return
	}
	next, err := mergeRemote(cfg, in)
	if err != nil {
		fail(w, 400, err)
		return
	}
	// A new passphrase: the next run uploads even if the data is the same,
	// so the newest backup opens with it.
	if next["passphrase"] != cfg["passphrase"] && cfg["passphrase"] != "" {
		next["passChanged"] = time.Now().UTC().Format(time.RFC3339)
	}
	if err := s.st.SetRemoteBackup(next); err != nil {
		fail(w, 500, err)
		return
	}
	if next["target"] != "" {
		_, where, _ := remoteTarget(next)
		s.note("backup", "Saved advanced backup settings: "+where+", "+everyText(next["every"]))
	}
	writeJSON(w, s.remoteView(next))
}

func everyText(e string) string {
	switch e {
	case "hour":
		return "checked every hour"
	case "day":
		return "checked every day"
	case "week":
		return "checked every week"
	}
	return "only when you click Back up now"
}

// testRemoteBackup writes, reads, lists and deletes a small file with the
// form's settings, without saving them.
func (s *Server) testRemoteBackup(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	if err := decode(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	cfg, err := s.st.RemoteBackup()
	if err != nil {
		fail(w, 500, err)
		return
	}
	next, err := mergeRemote(cfg, in)
	if err == nil && next["target"] == "" {
		err = errors.New("choose where backups go first")
	}
	if err != nil {
		fail(w, 400, err)
		return
	}
	t, where, _ := remoteTarget(next)
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	probe := []byte("Proof of Basis checked it can write here. Safe to delete.\n")
	if err := t.Put(ctx, remoteTestFile, probe); err != nil {
		hint := ""
		if next["target"] == "s3" && next["s3PathStyle"] == "false" && strings.Contains(err.Error(), "no such host") {
			hint = `. Self-hosted storage usually needs "Bucket in the path" ticked`
		}
		fail(w, 502, fmt.Errorf("couldn't write to %s: %w%s", where, err, hint))
		return
	}
	got, err := t.Get(ctx, remoteTestFile)
	if err != nil || string(got) != string(probe) {
		if err == nil {
			err = errors.New("what came back didn't match")
		}
		fail(w, 502, fmt.Errorf("wrote to %s but couldn't read it back: %w", where, err))
		return
	}
	names, err := t.List(ctx)
	if err != nil {
		fail(w, 502, fmt.Errorf("couldn't list %s: %w", where, err))
		return
	}
	if err := t.Delete(ctx, remoteTestFile); err != nil {
		fail(w, 502, fmt.Errorf("couldn't delete the test file from %s: %w", where, err))
		return
	}
	writeJSON(w, map[string]any{"where": where, "backups": len(remoteBackupNames(names))})
}

// remoteBackupNames keeps this app's backup files, oldest first.
func remoteBackupNames(names []string) []string {
	var out []string
	for _, n := range names {
		if remoteName.MatchString(n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

type remoteResult struct {
	Result  string `json:"result"` // uploaded or unchanged
	File    string `json:"file"`
	Where   string `json:"where"`
	Size    int    `json:"size,omitempty"`
	Removed int    `json:"removed,omitempty"`
}

// runRemoteBackup makes one run: hash the data, compare with the newest
// backup there, upload if it differs, then drop the oldest beyond keep.
func (s *Server) runRemoteBackup(ctx context.Context) (remoteResult, error) {
	if !s.backups.mu.TryLock() {
		return remoteResult{}, errors.New("a backup is already running")
	}
	defer s.backups.mu.Unlock()
	s.backups.running.Store(true)
	defer s.backups.running.Store(false)

	cfg, err := s.st.RemoteBackup()
	if err != nil {
		return remoteResult{}, err
	}
	res, err := s.remoteRun(ctx, cfg)
	state := map[string]string{"lastCheck": time.Now().UTC().Format(time.RFC3339)}
	if err != nil {
		state["lastResult"], state["lastError"] = "failed", err.Error()
	} else {
		state["lastResult"], state["lastError"], state["lastFile"] = res.Result, "", res.File
		if res.Result == "uploaded" {
			state["lastUpload"] = state["lastCheck"]
		}
	}
	if serr := s.st.SetRemoteBackup(state); serr != nil && err == nil {
		err = serr
	}
	return res, err
}

func (s *Server) remoteRun(ctx context.Context, cfg map[string]string) (remoteResult, error) {
	t, where, err := remoteTarget(cfg)
	if err != nil {
		return remoteResult{}, err
	}
	if len(cfg["passphrase"]) < 8 {
		return remoteResult{}, errors.New("set a passphrase first")
	}
	s.scanMu.Lock()
	tables, err := s.st.Dump()
	s.scanMu.Unlock()
	if err != nil {
		return remoteResult{}, err
	}
	plain, err := json.Marshal(tables)
	if err != nil {
		return remoteResult{}, err
	}
	sum := sha256.Sum256(plain)
	short := hex.EncodeToString(sum[:])[:16]

	names, err := t.List(ctx)
	if err != nil {
		return remoteResult{}, fmt.Errorf("couldn't list %s: %w", where, err)
	}
	ours := remoteBackupNames(names)
	if len(ours) > 0 {
		newest := ours[len(ours)-1]
		m := remoteName.FindStringSubmatch(newest)
		made, _ := time.Parse("20060102-150405", m[1])
		changed, _ := time.Parse(time.RFC3339, cfg["passChanged"])
		if m[2] == short && !made.Before(changed) {
			return remoteResult{Result: "unchanged", File: newest, Where: where}, nil
		}
	}

	body, err := sealBackup(plain, cfg["passphrase"])
	if err != nil {
		return remoteResult{}, err
	}
	name := fmt.Sprintf("proof-of-basis-%sZ-%s.json", time.Now().UTC().Format("20060102-150405"), short)
	if len(ours) > 0 && ours[len(ours)-1][:30] >= name[:30] { // two in one second: the names sort by time
		time.Sleep(time.Second)
		name = fmt.Sprintf("proof-of-basis-%sZ-%s.json", time.Now().UTC().Format("20060102-150405"), short)
	}
	if err := t.Put(ctx, name, body); err != nil {
		return remoteResult{}, fmt.Errorf("couldn't upload to %s: %w", where, err)
	}
	res := remoteResult{Result: "uploaded", File: name, Where: where, Size: len(body)}

	keep := remoteKeepDefault
	if n, err := strconv.Atoi(cfg["keep"]); err == nil && n > 0 {
		keep = n
	}
	ours = append(ours, name)
	for len(ours) > keep {
		if err := t.Delete(ctx, ours[0]); err != nil {
			log.Printf("advanced backup: removing old %s: %v", ours[0], err)
			break
		}
		ours = ours[1:]
		res.Removed++
	}
	return res, nil
}

func (s *Server) postRemoteBackupRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	res, err := s.runRemoteBackup(ctx)
	if err != nil {
		fail(w, 502, err)
		return
	}
	if res.Result == "uploaded" {
		s.note("backup", fmt.Sprintf("Backed up to %s (%s)", res.Where, res.File))
	}
	writeJSON(w, res)
}

// listRemoteBackups lists the backups there, newest first.
func (s *Server) listRemoteBackups(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.st.RemoteBackup()
	if err != nil {
		fail(w, 500, err)
		return
	}
	t, where, err := remoteTarget(cfg)
	if err != nil {
		fail(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	names, err := t.List(ctx)
	if err != nil {
		fail(w, 502, fmt.Errorf("couldn't list %s: %w", where, err))
		return
	}
	type file struct {
		Name    string `json:"name"`
		Created string `json:"created"`
	}
	out := []file{}
	ours := remoteBackupNames(names)
	for i := len(ours) - 1; i >= 0; i-- {
		m := remoteName.FindStringSubmatch(ours[i])
		made, _ := time.Parse("20060102-150405", m[1])
		out = append(out, file{ours[i], made.UTC().Format(time.RFC3339)})
	}
	writeJSON(w, map[string]any{"where": where, "files": out})
}

// getRemoteBackupFile downloads one of them, to restore with the usual form.
func (s *Server) getRemoteBackupFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !remoteName.MatchString(name) {
		fail(w, 404, errors.New("no such backup"))
		return
	}
	cfg, err := s.st.RemoteBackup()
	if err != nil {
		fail(w, 500, err)
		return
	}
	t, where, err := remoteTarget(cfg)
	if err != nil {
		fail(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	b, err := t.Get(ctx, name)
	if errors.Is(err, offsite.ErrNotFound) {
		fail(w, 404, errors.New("no such backup"))
		return
	}
	if err != nil {
		fail(w, 502, fmt.Errorf("couldn't fetch it from %s: %w", where, err))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	w.Write(b)
}

// StartRemoteBackups checks every few minutes whether a scheduled run is
// due. A failed run is tried again within the hour.
func (s *Server) StartRemoteBackups() {
	go func() {
		time.Sleep(time.Minute)
		for {
			s.remoteIfDue()
			time.Sleep(5 * time.Minute)
		}
	}()
}

func (s *Server) remoteIfDue() {
	cfg, err := s.st.RemoteBackup()
	if err != nil || cfg["target"] == "" {
		return
	}
	every := remoteEvery[cfg["every"]]
	if every == 0 {
		return
	}
	if cfg["lastResult"] == "failed" && every > time.Hour {
		every = time.Hour
	}
	last, _ := time.Parse(time.RFC3339, cfg["lastCheck"])
	if time.Since(last) < every {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res, err := s.runRemoteBackup(ctx)
	switch {
	case err != nil:
		log.Printf("advanced backup: %v", err)
		if cfg["lastResult"] != "failed" { // once, not every hour
			s.note("backup", "Advanced backup failed: "+err.Error())
		}
	case res.Result == "uploaded":
		log.Printf("advanced backup: uploaded %s to %s", res.File, res.Where)
		s.note("backup", fmt.Sprintf("Backed up to %s (%s)", res.Where, res.File))
	}
}
