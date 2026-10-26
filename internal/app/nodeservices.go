package app

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Node services: an app platform such as Umbrel or StartOS runs this app
// next to an Electrum server, Ollama or a Lightning node, and passes their
// addresses in the environment. They become the defaults for those
// settings, so a fresh install connects without typing anything; a value
// the user saves in Settings still wins.

// Version is the build's version (a release tag, or "dev"), set by main.
var Version = "dev"

// NodeDefaults reads the platform's service addresses:
//
//	BTCTAX_ELECTRUM_URL  tcp://host:50001 or ssl://host:50002
//	BTCTAX_OLLAMA_URL    http://host:11434
//	BTCTAX_CLN_URL       https://host:3010 (CLNRest; the rune is still the user's)
//
// It returns the settings they set and a note for each one it can't use.
func NodeDefaults(getenv func(string) string) (map[string]string, []string) {
	set, notes := map[string]string{}, []string(nil)
	if v := strings.TrimSpace(getenv("BTCTAX_ELECTRUM_URL")); v != "" {
		host, port, tls, err := electrumURL(v)
		if err != nil {
			notes = append(notes, fmt.Sprintf("BTCTAX_ELECTRUM_URL ignored: %v", err))
		} else {
			set["fulcrumHost"], set["fulcrumPort"], set["fulcrumTLS"] = host, port, fmt.Sprint(tls)
		}
	}
	for _, e := range []struct{ env, key string }{{"BTCTAX_OLLAMA_URL", "aiUrl"}, {"BTCTAX_CLN_URL", "clnUrl"}} {
		v := strings.TrimSpace(getenv(e.env))
		if v == "" {
			continue
		}
		if u, err := url.Parse(v); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			notes = append(notes, fmt.Sprintf("%s ignored: want http(s)://host:port, got %q", e.env, v))
			continue
		}
		set[e.key] = strings.TrimRight(v, "/")
	}
	return set, notes
}

// electrumURL reads an Electrum server address in the usual scheme form:
// tcp:// for plain connections, ssl:// (or tls://) for TLS.
func electrumURL(v string) (host, port string, tls bool, err error) {
	u, err := url.Parse(v)
	if err != nil || u.Hostname() == "" {
		return "", "", false, fmt.Errorf("want tcp://host:port or ssl://host:port, got %q", v)
	}
	switch u.Scheme {
	case "tcp":
		port = "50001"
	case "ssl", "tls":
		port, tls = "50002", true
	default:
		return "", "", false, fmt.Errorf("unknown scheme %q (use tcp or ssl)", u.Scheme)
	}
	if u.Port() != "" {
		port = u.Port()
	}
	return u.Hostname(), port, tls, nil
}

// UseNodeServices makes the platform's addresses the defaults for their
// settings.
func (s *Server) UseNodeServices(set map[string]string) { s.nodeDefaults = set }

// setting is a setting's saved value, or its default: the platform's
// address when it gave one, else the app's own.
func (s *Server) setting(k string) string {
	d := defaults[k]
	if v, ok := s.nodeDefaults[k]; ok {
		d = v
	}
	return s.st.Setting(k, d)
}

// fromNode lists the settings currently using the platform's address
// (none saved by the user), for Settings to say so.
func (s *Server) fromNode() string {
	var keys []string
	for k := range s.nodeDefaults {
		if s.st.Setting(k, "\x00") == "\x00" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// health answers the platform's checks without a session: the app is up,
// and which version it runs. It says nothing about the user's data.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "version": Version})
}
