package app

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"btctax/internal/store"
)

// A browser with a cookie jar, talking to one server.
type browser struct {
	t       *testing.T
	h       http.Handler
	cookies map[string]string
	ip      string
}

func (b *browser) do(method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if b.ip != "" {
		r.RemoteAddr = b.ip + ":5555"
	}
	for k, v := range b.cookies {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c.Value
		}
	}
	return w
}

func authServer(t *testing.T) (*store.Store, *Server, func() *browser) {
	old := pwIter
	pwIter = 1000 // fast hashing in tests
	t.Cleanup(func() { pwIter = old })
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	static := fstest.MapFS{
		"index.html": {Data: []byte("<p>app</p>")},
		"login.html": {Data: []byte("<p>login</p>")},
		"app.css":    {Data: []byte("body{}")},
	}
	srv := New(st, static)
	h := srv.Handler()
	return st, srv, func() *browser { return &browser{t: t, h: h, cookies: map[string]string{}, ip: "10.0.0.5"} }
}

func TestFirstVisitSetsPassword(t *testing.T) {
	_, _, newBrowser := authServer(t)
	b := newBrowser()
	if w := b.do("GET", "/", ""); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("app page without a password: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := b.do("GET", "/api/report", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("api without a password: %d", w.Code)
	}
	if w := b.do("GET", "/login", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "login") {
		t.Fatalf("login page: %d %q", w.Code, w.Body.String())
	}
	if w := b.do("GET", "/app.css", ""); w.Code != 200 {
		t.Fatalf("assets must load on the login page: %d", w.Code)
	}
	if w := b.do("GET", "/api/auth/status", ""); !strings.Contains(w.Body.String(), `"passwordSet":false`) {
		t.Fatalf("status: %s", w.Body.String())
	}
	if w := b.do("POST", "/api/auth/setup", `{"password":"short"}`); w.Code != 400 {
		t.Fatalf("short password accepted: %d", w.Code)
	}
	if w := b.do("POST", "/api/auth/setup", `{"password":"correct horse"}`); w.Code != 200 {
		t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	if w := b.do("GET", "/api/report", ""); w.Code != 200 {
		t.Fatalf("setup should log this browser in: %d", w.Code)
	}
	if w := b.do("GET", "/login", ""); w.Code != http.StatusSeeOther {
		t.Fatalf("logged-in visit to /login should go to the app: %d", w.Code)
	}
	// Nobody else can set it again.
	other := newBrowser()
	if w := other.do("POST", "/api/auth/setup", `{"password":"attacker pw"}`); w.Code != http.StatusConflict {
		t.Fatalf("second setup: %d", w.Code)
	}
	if w := other.do("GET", "/api/report", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("other browser got in: %d", w.Code)
	}
}

func TestLoginLogoutAndPasswordChange(t *testing.T) {
	_, _, newBrowser := authServer(t)
	a := newBrowser()
	a.do("POST", "/api/auth/setup", `{"password":"first password"}`)
	b := newBrowser()
	if w := b.do("POST", "/api/auth/login", `{"password":"nope nope"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", w.Code)
	}
	if w := b.do("POST", "/api/auth/login", `{"password":"first password"}`); w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	if w := b.do("GET", "/api/report", ""); w.Code != 200 {
		t.Fatalf("after login: %d", w.Code)
	}
	if c := b.cookies[sessionCookie]; len(c) < 40 {
		t.Fatalf("session token looks weak: %q", c)
	}
	// Changing the password keeps this browser in and signs the other out.
	if w := a.do("POST", "/api/auth/password", `{"current":"wrong one","new":"second password"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("change with wrong current: %d", w.Code)
	}
	if w := a.do("POST", "/api/auth/password", `{"current":"first password","new":"second password"}`); w.Code != 200 {
		t.Fatalf("change: %d %s", w.Code, w.Body.String())
	}
	if w := a.do("GET", "/api/report", ""); w.Code != 200 {
		t.Fatalf("changer signed out: %d", w.Code)
	}
	if w := b.do("GET", "/api/report", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("other session survived a password change: %d", w.Code)
	}
	if w := b.do("POST", "/api/auth/login", `{"password":"second password"}`); w.Code != 200 {
		t.Fatalf("login with new password: %d", w.Code)
	}
	// Log out ends only this session.
	a.do("POST", "/api/auth/logout", "")
	if w := a.do("GET", "/api/report", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("still in after logout: %d", w.Code)
	}
	if w := b.do("GET", "/api/report", ""); w.Code != 200 {
		t.Fatalf("logout ended someone else's session: %d", w.Code)
	}
	// Sign out everywhere ends them all.
	a.do("POST", "/api/auth/login", `{"password":"second password"}`)
	a.do("POST", "/api/auth/logout-all", "")
	for _, x := range []*browser{a, b} {
		if w := x.do("GET", "/api/report", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("session survived sign out everywhere: %d", w.Code)
		}
	}
}

func TestLoginLockout(t *testing.T) {
	_, _, newBrowser := authServer(t)
	newBrowser().do("POST", "/api/auth/setup", `{"password":"the password"}`)
	b := newBrowser()
	for i := 0; i < 5; i++ {
		b.do("POST", "/api/auth/login", `{"password":"guess"}`)
	}
	w := b.do("POST", "/api/auth/login", `{"password":"the password"}`)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("no lockout after 5 misses: %d", w.Code)
	}
	// A different address isn't locked out.
	c := newBrowser()
	c.ip = "10.0.0.9"
	if w := c.do("POST", "/api/auth/login", `{"password":"the password"}`); w.Code != 200 {
		t.Fatalf("other address locked out: %d", w.Code)
	}
}

func TestEnvPasswordAndReset(t *testing.T) {
	st, srv, newBrowser := authServer(t)
	if err := srv.ApplyEnvPassword("short"); err == nil {
		t.Fatal("short env password accepted")
	}
	if err := srv.ApplyEnvPassword("from the env"); err != nil {
		t.Fatal(err)
	}
	b := newBrowser()
	if w := b.do("POST", "/api/auth/login", `{"password":"from the env"}`); w.Code != 200 {
		t.Fatalf("env password: %d", w.Code)
	}
	// The same env password on restart keeps sessions.
	srv.ApplyEnvPassword("from the env")
	if w := b.do("GET", "/api/report", ""); w.Code != 200 {
		t.Fatalf("restart with the same env password signed out: %d", w.Code)
	}
	// Resetting clears the password and every session; setup is open again.
	if err := st.SetPasswordHash(""); err != nil {
		t.Fatal(err)
	}
	if w := b.do("GET", "/api/report", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("session survived reset: %d", w.Code)
	}
	if w := b.do("POST", "/api/auth/setup", `{"password":"a new start"}`); w.Code != 200 {
		t.Fatalf("setup after reset: %d", w.Code)
	}
}

func TestWipeAndRestoreKeepLogin(t *testing.T) {
	st, _, newBrowser := authServer(t)
	b := newBrowser()
	b.do("POST", "/api/auth/setup", `{"password":"keep me in"}`)
	if err := st.Wipe(); err != nil {
		t.Fatal(err)
	}
	if w := b.do("GET", "/api/report", ""); w.Code != 200 {
		t.Fatalf("wipe signed out: %d", w.Code)
	}
	tables, err := st.Dump()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tables["auth"]; ok {
		t.Fatal("backup carries the password hash")
	}
	if _, ok := tables["sessions"]; ok {
		t.Fatal("backup carries sessions")
	}
}

func TestPasswordHashFormat(t *testing.T) {
	old := pwIter
	pwIter = 1000
	defer func() { pwIter = old }()
	h, err := hashPassword("hunter22")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h, "hunter22") || !strings.HasPrefix(h, "pbkdf2-sha256$1000$") {
		t.Fatalf("hash format: %s", h)
	}
	h2, _ := hashPassword("hunter22")
	if h == h2 {
		t.Fatal("same hash twice: salt missing")
	}
	if !checkPassword(h, "hunter22") || checkPassword(h, "hunter23") || checkPassword("garbage", "hunter22") {
		t.Fatal("checkPassword wrong")
	}
}
