package app

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// One user, one password. The password is set in the app on the first
// visit (or by BTCTAX_PASSWORD), stored only as a slow salted hash, and a
// login gives the browser a random session token in an HttpOnly cookie.
// The server keeps only a hash of each token.

const (
	sessionCookie = "btctax_session"
	sessionIdle   = 30 * 24 * time.Hour // a session lasts 30 days from last use
	minPassword   = 8
	maxPassword   = 1024
)

// pwIter is the PBKDF2-SHA256 work factor (OWASP's current figure).
// Tests lower it.
var pwIter = 600_000

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	k, err := pbkdf2.Key(sha256.New, pw, salt, pwIter, 32)
	if err != nil {
		return "", err
	}
	b := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pwIter, b.EncodeToString(salt), b.EncodeToString(k)), nil
}

func checkPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	b := base64.RawStdEncoding
	salt, err1 := b.DecodeString(parts[2])
	want, err2 := b.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func tokenHash(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// ApplyEnvPassword makes BTCTAX_PASSWORD the password when it is set and
// differs from the stored one. It is also the way back in for someone who
// forgot the password but can't run the reset command.
func (s *Server) ApplyEnvPassword(pw string) error {
	if pw == "" {
		return nil
	}
	if len([]rune(pw)) < minPassword {
		return fmt.Errorf("BTCTAX_PASSWORD must be at least %d characters", minPassword)
	}
	if cur := s.st.PasswordHash(); cur != "" && checkPassword(cur, pw) {
		return nil
	}
	h, err := hashPassword(pw)
	if err != nil {
		return err
	}
	return s.st.SetPasswordHash(h)
}

// Login attempts are limited per client address: five free tries, then
// a lockout that doubles with each further miss, up to 15 minutes.
type limiter struct {
	mu   sync.Mutex
	hits map[string]*strike
}

type strike struct {
	fails int
	until time.Time
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (l *limiter) wait(ip string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st := l.hits[ip]; st != nil && now.Before(st.until) {
		return st.until.Sub(now)
	}
	return 0
}

func (l *limiter) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string]*strike{}
	}
	st := l.hits[ip]
	if st == nil {
		st = &strike{}
		l.hits[ip] = st
	}
	st.fails++
	if st.fails >= 5 {
		d := 30 * time.Second << min(st.fails-5, 5)
		st.until = now.Add(min(d, 15*time.Minute))
	}
}

func (l *limiter) ok(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, ip)
}

// session returns the hash of the request's session token if it is valid.
func (s *Server) session(w http.ResponseWriter, r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return "", false
	}
	h := tokenHash(c.Value)
	ok, renewed := s.st.TouchSession(h, time.Now(), sessionIdle)
	if ok && renewed {
		setSessionCookie(w, r, c.Value) // slide the browser's expiry too
	}
	return h, ok
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		MaxAge: int(sessionIdle / time.Second), HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secureRequest(r),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureRequest(r)})
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	if err := s.st.AddSession(tokenHash(tok), time.Now()); err != nil {
		return err
	}
	setSessionCookie(w, r, tok)
	return nil
}

// public paths work without a session: the login page, its assets and
// the login endpoints. Static assets are the app's code and hold no data.
func public(p string) bool {
	if p == "/login" || p == "/login.html" || strings.HasPrefix(p, "/api/auth/") {
		return true
	}
	switch path.Ext(p) {
	case ".css", ".js", ".woff2", ".woff", ".svg", ".png", ".ico", ".txt":
		return true
	}
	return false
}

// requireLogin sends anyone without a session to the login page (or a
// 401 for API calls). Before a password exists everything leads to the
// page that sets one.
func (s *Server) requireLogin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/login" || p == "/login.html" {
			if s.st.PasswordHash() != "" {
				if _, ok := s.session(w, r); ok {
					http.Redirect(w, r, "/", http.StatusSeeOther)
					return
				}
			}
			http.ServeFileFS(w, r, s.static, "login.html")
			return
		}
		if public(p) {
			h.ServeHTTP(w, r)
			return
		}
		if s.st.PasswordHash() != "" {
			if _, ok := s.session(w, r); ok {
				h.ServeHTTP(w, r)
				return
			}
		}
		if strings.HasPrefix(p, "/api/") {
			fail(w, http.StatusUnauthorized, errors.New("please log in"))
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	_, ok := s.session(w, r)
	set := s.st.PasswordHash() != ""
	writeJSON(w, map[string]bool{"passwordSet": set, "loggedIn": set && ok})
}

func decodeSmall(w http.ResponseWriter, r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(v); err != nil {
		return errors.New("bad request")
	}
	return nil
}

func validNew(pw string) error {
	if len([]rune(pw)) < minPassword {
		return fmt.Errorf("use at least %d characters", minPassword)
	}
	if len(pw) > maxPassword {
		return errors.New("that password is too long")
	}
	return nil
}

func (s *Server) authSetup(w http.ResponseWriter, r *http.Request) {
	var body struct{ Password string }
	if err := decodeSmall(w, r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if err := validNew(body.Password); err != nil {
		fail(w, 400, err)
		return
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.st.PasswordHash() != "" {
		fail(w, http.StatusConflict, errors.New("a password is already set; log in instead"))
		return
	}
	h, err := hashPassword(body.Password)
	if err == nil {
		err = s.st.SetPasswordHash(h)
	}
	if err == nil {
		err = s.startSession(w, r)
	}
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// tooMany answers a locked-out client.
func tooMany(w http.ResponseWriter, d time.Duration) {
	secs := int(d.Seconds()) + 1
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	fail(w, http.StatusTooManyRequests, fmt.Errorf("too many wrong passwords; try again in %d seconds", secs))
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	ip, now := clientIP(r), time.Now()
	if d := s.limit.wait(ip, now); d > 0 {
		tooMany(w, d)
		return
	}
	var body struct{ Password string }
	if err := decodeSmall(w, r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	stored := s.st.PasswordHash()
	if stored == "" {
		fail(w, http.StatusConflict, errors.New("no password is set yet"))
		return
	}
	if !checkPassword(stored, body.Password) {
		s.limit.fail(ip, now)
		fail(w, http.StatusUnauthorized, errors.New("wrong password"))
		return
	}
	s.limit.ok(ip)
	if err := s.startSession(w, r); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	if h, ok := s.session(w, r); ok {
		s.st.DeleteSession(h)
	}
	clearSessionCookie(w, r)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) authLogoutAll(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(w, r); !ok {
		fail(w, http.StatusUnauthorized, errors.New("please log in"))
		return
	}
	if err := s.st.DeleteSessions(); err != nil {
		fail(w, 500, err)
		return
	}
	clearSessionCookie(w, r)
	writeJSON(w, map[string]bool{"ok": true})
}

// authPassword changes the password. Every other session is signed out;
// this browser stays logged in with a fresh session.
func (s *Server) authPassword(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(w, r); !ok {
		fail(w, http.StatusUnauthorized, errors.New("please log in"))
		return
	}
	ip, now := clientIP(r), time.Now()
	if d := s.limit.wait(ip, now); d > 0 {
		tooMany(w, d)
		return
	}
	var body struct{ Current, New string }
	if err := decodeSmall(w, r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if !checkPassword(s.st.PasswordHash(), body.Current) {
		s.limit.fail(ip, now)
		fail(w, http.StatusUnauthorized, errors.New("the current password is wrong"))
		return
	}
	s.limit.ok(ip)
	if err := validNew(body.New); err != nil {
		fail(w, 400, err)
		return
	}
	h, err := hashPassword(body.New)
	if err == nil {
		err = s.st.SetPasswordHash(h)
	}
	if err == nil {
		err = s.startSession(w, r)
	}
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
