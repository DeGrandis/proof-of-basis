package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/openai"
)

// The assistant on a hosted provider (aiMode "remote"), in the OpenAI chat
// format. Local Ollama is the default; switching takes the sentence below,
// typed in a warning in Settings, because everything the assistant sends
// then leaves this server.

// remoteConsent is what the user types to allow it.
const remoteConsent = "I accept my data leaking to a provider"

func acceptsRemote(typed string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(typed), " "), remoteConsent)
}

func (s *Server) remote() bool { return s.st.Setting("aiMode", "local") == "remote" }

// remoteClient is the provider as set up in Settings. A key is optional:
// local servers in the same format (LM Studio, llama.cpp) don't need one.
func (s *Server) remoteClient() (*openai.Client, error) {
	url, model := s.st.Setting("aiRemoteUrl", ""), s.st.Setting("aiRemoteModel", "")
	if url == "" || model == "" {
		return nil, errors.New("the assistant isn't set up: add the provider's address, key and model in Settings")
	}
	return openai.New(url, s.st.Setting("aiRemoteKey", ""), model)
}

// aiModelName is the model new chats are saved with.
func (s *Server) aiModelName() string {
	if s.remote() {
		return s.st.Setting("aiRemoteModel", "")
	}
	return s.st.Setting("aiModel", "")
}

// remoteSettings handles the provider's key and the switch to it before the
// plain settings are saved. It reports false when it has failed the request.
func (s *Server) remoteSettings(w http.ResponseWriter, in map[string]string) bool {
	accept := in["aiRemoteAccept"]
	for _, k := range []string{"aiRemoteAccept", "aiRemoteKeySet", "aiRemoteConsent", "aiRemotePhrase"} {
		delete(in, k)
	}
	if in["aiMode"] == "remote" && !s.remote() {
		if !acceptsRemote(accept) {
			fail(w, 400, fmt.Errorf("to use a hosted provider, type: %s", remoteConsent))
			return false
		}
		if err := s.st.SetSetting("aiRemoteConsent", time.Now().UTC().Format(time.RFC3339)); err != nil {
			fail(w, 500, err)
			return false
		}
		s.note("assistant", "Let the assistant send your data to a hosted provider")
	}
	if key, ok := in["aiRemoteKey"]; ok {
		delete(in, "aiRemoteKey")
		if key = strings.TrimSpace(key); key != "" {
			if err := s.st.SetSetting("aiRemoteKey", key); err != nil {
				fail(w, 500, err)
				return false
			}
			s.note("assistant", "Saved the assistant provider's API key (not shown here)")
		}
	}
	if _, ok := in["aiRemoteForget"]; ok {
		delete(in, "aiRemoteForget")
		for _, k := range []string{"aiRemoteKey", "aiRemoteUrl", "aiRemoteModel"} {
			s.st.SetSetting(k, "")
		}
		s.st.SetSetting("aiMode", "local")
		s.note("assistant", "Stopped using the hosted provider and forgot its API key")
	}
	return true
}

// remoteModels lists a provider's models, for Settings. The key typed in
// the form is used, or the saved one when the address is the saved one.
func (s *Server) remoteModels(w http.ResponseWriter, r *http.Request, url, key, model string) {
	if key == "" && strings.TrimRight(url, "/") == strings.TrimRight(s.st.Setting("aiRemoteUrl", ""), "/") {
		key = s.st.Setting("aiRemoteKey", "")
	}
	c, err := openai.New(url, key, strings.TrimSpace(model))
	if err != nil {
		fail(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	models, toolsKnown, err := c.Models(ctx)
	if errors.Is(err, openai.ErrNoModelList) {
		// Nothing to list, so ask the model itself, if one is named.
		if c.Name() == "" {
			fail(w, 400, errors.New("this provider doesn't list its models. Type the model's name, then test again"))
			return
		}
		if err := c.Ping(ctx); err != nil {
			fail(w, 502, err)
			return
		}
		writeJSON(w, map[string]any{"models": []any{}, "host": c.Host(), "answered": c.Name()})
		return
	}
	if err != nil {
		fail(w, 502, err)
		return
	}
	writeJSON(w, map[string]any{"models": models, "host": c.Host(), "toolsKnown": toolsKnown})
}
