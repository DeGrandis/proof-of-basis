// Package openai runs the assistant on a hosted provider that speaks the
// OpenAI chat format (/v1/chat/completions): OpenRouter, Mistral, Groq,
// Together, Venice and many others, or a local server such as LM Studio,
// llama.cpp or Ollama's own /v1. Like the claude package, it takes and
// returns the Ollama client's chat shapes, so the assistant's loop works
// unchanged. Nothing here reaches anything but the address the user gave.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/DeGrandis/proof-of-basis/internal/ollama"
)

type Client struct {
	base  string // up to and including /v1, or the provider's equivalent
	key   string
	model string
	http  *http.Client
}

// New checks the address and returns a client for it. A key is only sent
// over https, or plain http to this computer or the local network.
func New(base, key, model string) (*Client, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	base = strings.TrimSuffix(base, "/chat/completions")
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("enter the provider's address like https://openrouter.ai/api/v1")
	}
	if u.Scheme == "http" && !local(u.Hostname()) {
		return nil, errors.New("use the provider's https address, so your key and data aren't sent in the clear")
	}
	return &Client{base: base, key: strings.TrimSpace(key), model: model, http: &http.Client{}}, nil
}

// local reports whether a host is this computer or on the local network
// (by name or address; a name that doesn't resolve counts as not local).
func local(host string) bool {
	if host == "localhost" || host == "host.docker.internal" {
		return true
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
			return false
		}
	}
	return true
}

// Host is the address's host, for messages.
func (c *Client) Host() string {
	u, _ := url.Parse(c.base)
	return u.Hostname()
}

// Name is how the model is shown in the app.
func (c *Client) Name() string { return c.model }

func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, friendly(err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, apiError(resp)
	}
	return resp, nil
}

// statusError is a provider's refusal: the status and its own message, kept
// so a caller can tell what was refused.
type statusError struct {
	status int
	detail string
	error
}

// apiError reads the provider's error message, and says what the common
// statuses mean.
func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := errorMessage(b)
	var err error
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		err = withDetail("the provider didn't accept the API key", msg, "")
	case resp.StatusCode == 402:
		err = withDetail("the provider says the account is out of credit", msg, "")
	case resp.StatusCode == 404:
		err = withDetail("the provider doesn't know that address or model", msg, "")
	case resp.StatusCode == 429:
		err = withDetail("the provider is limiting requests", msg, "; try again in a minute")
	case resp.StatusCode >= 500:
		err = withDetail("the provider had a problem", msg, "; try again shortly")
	case msg != "":
		err = fmt.Errorf("the provider: %s", msg)
	default:
		err = fmt.Errorf("the provider answered %s", resp.Status)
	}
	return &statusError{resp.StatusCode, msg, err}
}

// refused reports whether err is the provider refusing a request with one of
// these statuses, and its message mentions word (when word isn't empty).
func refused(err error, word string, statuses ...int) bool {
	var se *statusError
	if !errors.As(err, &se) {
		return false
	}
	for _, st := range statuses {
		if se.status == st {
			return word == "" || strings.Contains(strings.ToLower(se.detail), word)
		}
	}
	return false
}

// errorMessage finds the message in an error body: {"error": {"message": …}}
// in the OpenAI format, {"error": "…"} or {"message": "…"} elsewhere.
func errorMessage(b []byte) string {
	var e struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(b, &e) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(e.Error, &s) == nil && s != "" {
		return s
	}
	var o struct {
		Message  string
		Metadata struct{ Raw json.RawMessage } // OpenRouter: the upstream provider's own error
	}
	if json.Unmarshal(e.Error, &o) == nil && o.Message != "" {
		var raw string
		if json.Unmarshal(o.Metadata.Raw, &raw) == nil && raw != "" {
			if m := errorMessage([]byte(raw)); m != "" {
				raw = m // the upstream error is itself JSON
			}
			if len(raw) > 300 {
				raw = raw[:300] + "…"
			}
			return raw
		}
		return o.Message
	}
	return e.Message
}

// withDetail: what went wrong, the provider's own words, then what to do.
func withDetail(what, detail, advice string) error {
	if detail != "" {
		what += " (" + detail + ")"
	}
	return errors.New(what + advice)
}

func friendly(err error) error {
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return err
	case errors.As(err, &ne) && ne.Timeout():
		return errors.New("the provider didn't answer in time")
	case strings.Contains(err.Error(), "connection refused"):
		return errors.New("nothing is listening at that address")
	case strings.Contains(err.Error(), "no such host"):
		return errors.New("that host name doesn't resolve")
	}
	return fmt.Errorf("can't reach the provider: %v", err)
}

// ErrNoModelList: the provider has no /models to list (the format doesn't
// require one), though chat may still work.
var ErrNoModelList = errors.New("the provider has no model list")

// Models lists the provider's models by name. The format itself says nothing
// about tools; where the provider says (OpenRouter's supported_parameters),
// toolsKnown is true and each model's Tools is set from it. Otherwise every
// model is offered as able to use tools.
func (c *Client) Models(ctx context.Context) (models []ollama.Model, toolsKnown bool, err error) {
	resp, err := c.request(ctx, "GET", "/models", nil)
	if refused(err, "", 404, 405) {
		return nil, false, ErrNoModelList
	}
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID      string   `json:"id"`
			Params  []string `json:"supported_parameters"`
			Context int      `json:"context_length"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&list); err != nil {
		return nil, false, fmt.Errorf("the provider's model list didn't read: %v", err)
	}
	for _, m := range list.Data {
		if m.Params != nil {
			toolsKnown = true
		}
	}
	models = make([]ollama.Model, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID == "" {
			continue
		}
		tools := !toolsKnown
		for _, p := range m.Params {
			tools = tools || p == "tools"
		}
		models = append(models, ollama.Model{Name: m.ID, Tools: tools, Context: m.Context})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models, toolsKnown, nil
}

// Ping sends the model a one-line question, for a provider without a model
// list: an answer of any kind means the address, key and model all work.
func (c *Client) Ping(ctx context.Context) error {
	hi := "Reply with OK."
	resp, err := c.request(ctx, "POST", "/chat/completions", chatRequest{Model: c.model, Messages: []message{{Role: "user", Content: &hi}}})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// noTemperature holds the models (by address and name) that refused a
// temperature, so later requests leave it out from the start. OpenAI's
// reasoning models only take their default.
var noTemperature sync.Map

// Chat types, as /chat/completions takes them.

type message struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"` // null on an assistant turn that only calls tools
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON, as a string
	} `json:"function"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []message     `json:"messages"`
	Tools       []ollama.Tool `json:"tools,omitempty"`
	Stream      bool          `json:"stream"`
	Temperature any           `json:"temperature,omitempty"`
}

// Chat streams one reply: answer text to onDelta and reasoning (where the
// provider sends it) to onThink as they arrive. Tool calls come back on
// the message, with their ids kept on Raw so the results can answer them.
func (c *Client) Chat(ctx context.Context, req ollama.ChatRequest, onDelta, onThink func(string)) (ollama.Message, error) {
	body := chatRequest{Model: c.model, Messages: toMessages(req.Messages), Tools: req.Tools, Stream: true}
	if req.Model != "" {
		body.Model = req.Model
	}
	which := c.base + "|" + body.Model
	if t, ok := req.Options["temperature"]; ok {
		if _, refuses := noTemperature.Load(which); !refuses {
			body.Temperature = t
		}
	}
	out := ollama.Message{Role: "assistant"}
	resp, err := c.request(ctx, "POST", "/chat/completions", body)
	if err != nil && body.Temperature != nil && refused(err, "temperature", 400, 422) {
		noTemperature.Store(which, true)
		body.Temperature = nil
		resp, err = c.request(ctx, "POST", "/chat/completions", body)
	}
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	var text, think strings.Builder
	var calls []toolCall // by the index the stream gives each one
	var other bytes.Buffer // anything that isn't a stream line: a provider that answered without streaming
	streamed := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			// Blank lines, comments (": keep-alive") and event names are
			// part of a stream; anything else is kept.
			if len(line) > 0 && line[0] != ':' && !bytes.HasPrefix(line, []byte("event:")) && !bytes.HasPrefix(line, []byte("id:")) && other.Len() < 8<<20 {
				other.Write(line)
				other.WriteByte('\n')
			}
			continue
		}
		streamed = true
		data = bytes.TrimSpace(data)
		if string(data) == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"` // DeepSeek, vLLM, llama.cpp
					Reasoning        string `json:"reasoning"`         // OpenRouter, Ollama
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return out, fmt.Errorf("the provider sent something unexpected: %v", err)
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			if msg := errorMessage(data); msg != "" {
				return out, fmt.Errorf("the provider: %s", msg)
			}
			return out, errors.New("the provider stopped with an error")
		}
		for _, ch := range chunk.Choices {
			d := ch.Delta
			if r := d.ReasoningContent + d.Reasoning; r != "" {
				think.WriteString(r)
				if onThink != nil {
					onThink(r)
				}
			}
			if d.Content != "" {
				text.WriteString(d.Content)
				if onDelta != nil {
					onDelta(d.Content)
				}
			}
			// A call arrives in pieces: the id and name first, then its
			// arguments a few characters at a time.
			for _, tc := range d.ToolCalls {
				for len(calls) <= tc.Index {
					calls = append(calls, toolCall{Type: "function"})
				}
				t := &calls[tc.Index]
				if tc.ID != "" {
					t.ID = tc.ID
				}
				t.Function.Name += tc.Function.Name
				t.Function.Arguments += tc.Function.Arguments
			}
		}
	}
	if err := sc.Err(); err != nil {
		return out, friendly(err)
	}
	if !streamed {
		var err error
		if calls, err = whole(other.Bytes(), &text, &think, onDelta, onThink); err != nil {
			return out, err
		}
	}
	out.Content = text.String()
	out.Thinking = think.String()
	var ids []string
	for i, t := range calls {
		if t.Function.Name == "" {
			continue
		}
		if t.ID == "" {
			t.ID = fmt.Sprintf("call_%d", i)
		}
		var tc ollama.ToolCall
		tc.Function.Name = t.Function.Name
		if err := json.Unmarshal([]byte(t.Function.Arguments), &tc.Function.Arguments); err != nil || tc.Function.Arguments == nil {
			tc.Function.Arguments = map[string]any{}
		}
		out.ToolCalls = append(out.ToolCalls, tc)
		ids = append(ids, t.ID)
	}
	if len(ids) > 0 {
		out.Raw = ids
	}
	return out, nil
}

// toMessages turns the assistant's messages into the OpenAI format: each
// tool call gets an id (the provider's own, kept on Raw, or a made-up one
// for calls from an earlier answer), and each tool result answers the
// calls before it, in order.
func toMessages(in []ollama.Message) []message {
	out := make([]message, 0, len(in))
	var pending []string // ids of tool calls waiting for their results
	str := func(s string) *string { return &s }
	for i, m := range in {
		switch m.Role {
		case "assistant":
			msg := message{Role: "assistant"}
			ids, _ := m.Raw.([]string)
			pending = pending[:0]
			for j, tc := range m.ToolCalls {
				id := fmt.Sprintf("call_app_%d_%d", i, j)
				if j < len(ids) {
					id = ids[j]
				}
				args, _ := json.Marshal(tc.Function.Arguments)
				if tc.Function.Arguments == nil {
					args = []byte("{}")
				}
				t := toolCall{ID: id, Type: "function"}
				t.Function.Name, t.Function.Arguments = tc.Function.Name, string(args)
				msg.ToolCalls = append(msg.ToolCalls, t)
				pending = append(pending, id)
			}
			if m.Content != "" || len(msg.ToolCalls) == 0 {
				msg.Content = str(m.Content)
			}
			out = append(out, msg)
		case "tool":
			if len(pending) == 0 { // a result with no call to answer: pass it on as a note
				out = append(out, message{Role: "user", Content: str("[Lookup result] " + m.Content)})
				continue
			}
			out = append(out, message{Role: "tool", ToolCallID: pending[0], Content: str(m.Content)})
			pending = pending[1:]
		default: // system and user, as they are; a later system message is the app correcting course
			out = append(out, message{Role: m.Role, Content: str(m.Content)})
		}
	}
	return out
}

// whole reads a reply that came all at once instead of as a stream: a
// complete answer, or an error sent with a success status.
func whole(body []byte, text, think *strings.Builder, onDelta, onThink func(string)) ([]toolCall, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, errors.New("the provider sent an empty reply")
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content          string     `json:"content"`
				ReasoningContent string     `json:"reasoning_content"`
				Reasoning        string     `json:"reasoning"`
				ToolCalls        []toolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err != nil || len(r.Choices) == 0 {
		if msg := errorMessage(body); msg != "" {
			return nil, fmt.Errorf("the provider: %s", msg)
		}
		if len(body) > 200 {
			body = append(body[:200:200], "…"...)
		}
		return nil, fmt.Errorf("the provider sent a reply this app can't read: %s", body)
	}
	m := r.Choices[0].Message
	if t := m.ReasoningContent + m.Reasoning; t != "" {
		think.WriteString(t)
		if onThink != nil {
			onThink(t)
		}
	}
	if m.Content != "" {
		text.WriteString(m.Content)
		if onDelta != nil {
			onDelta(m.Content)
		}
	}
	return m.ToolCalls, nil
}
