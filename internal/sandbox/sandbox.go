// Package sandbox runs the public demo. Every visitor gets a private copy
// of an invented history, found again by a cookie and deleted after a few
// idle hours; nobody sees or changes anyone else's. Each copy is a normal
// app server in demo mode (no login, a simulated node, uploads off).
package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"btctax/internal/app"
	"btctax/internal/demo"
	"btctax/internal/store"
)

type Config struct {
	Dir          string        // working directory for the copies
	Static       fs.FS         // the UI
	AI           app.ChatModel // the assistant; nil turns it off
	MaxCopies    int           // copies kept at once; the longest idle goes first
	IdleTTL      time.Duration // a copy unused this long is deleted
	AIPerVisitor int           // assistant questions per copy
	AIPerDay     int           // assistant questions per day, for everyone
}

const cookieName = "btctax_demo"

type Hub struct {
	cfg      Config
	template string
	static   http.Handler

	mu       sync.Mutex
	copies   map[string]*box
	day      string
	dayAsked int
}

type box struct {
	id       string
	dir      string
	st       *store.Store
	h        http.Handler
	last     time.Time
	inflight int
	asked    int
}

// New builds the invented history once, as a template every copy starts
// from, and starts the cleanup of idle copies.
func New(cfg Config) (*Hub, error) {
	if cfg.MaxCopies <= 0 {
		cfg.MaxCopies = 100
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = 3 * time.Hour
	}
	if err := os.RemoveAll(filepath.Join(cfg.Dir, "copies")); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.Dir, "copies"), 0o700); err != nil {
		return nil, err
	}
	h := &Hub{cfg: cfg, template: filepath.Join(cfg.Dir, "template.db"), static: app.StaticHandler(cfg.Static), copies: map[string]*box{}}
	if err := h.buildTemplate(); err != nil {
		return nil, fmt.Errorf("building the demo data: %w", err)
	}
	go h.cleanup()
	return h, nil
}

func (h *Hub) buildTemplate() error {
	build := filepath.Join(h.cfg.Dir, "build")
	os.RemoveAll(build)
	os.Remove(h.template)
	if err := os.MkdirAll(build, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(build)
	st, err := store.Open(filepath.Join(build, "btctax.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if err := demo.Seed(st, 1); err != nil {
		return err
	}
	if err := demo.SeedLightning(st, 1); err != nil {
		return err
	}
	return st.SnapshotTo(h.template)
}

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	// The UI's files are the same for everyone and need no copy.
	if !strings.HasPrefix(p, "/api/") && p != "/" && p != "/index.html" && p != "/login" && p != "/login.html" {
		h.static.ServeHTTP(w, r)
		return
	}
	if p == "/api/demo/reset" && r.Method == http.MethodPost {
		h.reset(w, r)
		return
	}
	c, err := h.copyFor(w, r)
	if err != nil {
		log.Printf("demo copy: %v", err)
		http.Error(w, "The demo is busy; try again in a moment.", http.StatusServiceUnavailable)
		return
	}
	defer h.done(c)
	c.h.ServeHTTP(w, r)
}

// copyFor finds the visitor's copy by its cookie, or makes a new one.
func (h *Hub) copyFor(w http.ResponseWriter, r *http.Request) (*box, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ck, err := r.Cookie(cookieName); err == nil {
		if c := h.copies[ck.Value]; c != nil {
			c.last, c.inflight = time.Now(), c.inflight+1
			return c, nil
		}
	}
	for len(h.copies) >= h.cfg.MaxCopies {
		if !h.dropIdlest() {
			return nil, errors.New("every copy is in use")
		}
	}
	c, err := h.newCopy()
	if err != nil {
		return nil, err
	}
	h.copies[c.id] = c
	c.inflight++
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: c.id, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"})
	return c, nil
}

func (h *Hub) newCopy() (*box, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(b)
	dir := filepath.Join(h.cfg.Dir, "copies", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := copyFile(h.template, filepath.Join(dir, "btctax.db")); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	st, err := store.Open(filepath.Join(dir, "btctax.db"))
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	c := &box{id: id, dir: dir, st: st, last: time.Now()}
	srv := app.New(st, h.cfg.Static)
	srv.SetDemo(&app.DemoOptions{AI: h.cfg.AI, Ask: func() error { return h.ask(c) },
		Allowance: fmt.Sprintf("%d questions per visitor a day", h.cfg.AIPerVisitor)})
	c.h = srv.Handler()
	return c, nil
}

// ask spends one assistant question from the visitor's allowance and the
// day's total.
func (h *Hub) ask(c *box) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if today := time.Now().UTC().Format("2006-01-02"); h.day != today {
		h.day, h.dayAsked = today, 0
	}
	if c.asked >= h.cfg.AIPerVisitor {
		return fmt.Errorf("you've asked the demo's assistant its %d questions; run your own copy for more", h.cfg.AIPerVisitor)
	}
	if h.dayAsked >= h.cfg.AIPerDay {
		return errors.New("the demo's assistant has answered all it can for today; try again tomorrow")
	}
	c.asked++
	h.dayAsked++
	return nil
}

func (h *Hub) done(c *box) {
	h.mu.Lock()
	c.inflight--
	h.mu.Unlock()
}

// reset drops the visitor's copy; the next page load starts a fresh one.
func (h *Hub) reset(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(cookieName); err == nil {
		h.mu.Lock()
		if c := h.copies[ck.Value]; c != nil && c.inflight == 0 {
			h.drop(c)
		}
		h.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// dropIdlest removes the copy unused the longest; false if all are busy.
func (h *Hub) dropIdlest() bool {
	var oldest *box
	for _, c := range h.copies {
		if c.inflight == 0 && (oldest == nil || c.last.Before(oldest.last)) {
			oldest = c
		}
	}
	if oldest == nil {
		return false
	}
	h.drop(oldest)
	return true
}

func (h *Hub) drop(c *box) {
	delete(h.copies, c.id)
	c.st.Close()
	os.RemoveAll(c.dir)
}

func (h *Hub) cleanup() {
	for range time.Tick(5 * time.Minute) {
		h.mu.Lock()
		for _, c := range h.copies {
			if c.inflight == 0 && time.Since(c.last) > h.cfg.IdleTTL {
				h.drop(c)
			}
		}
		h.mu.Unlock()
	}
}

// Copies is how many visitors' copies exist now.
func (h *Hub) Copies() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.copies)
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
