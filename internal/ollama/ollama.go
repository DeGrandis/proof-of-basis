// Package ollama talks to a local Ollama server: which models it has,
// what they can do, and streaming chat with tool calls. Nothing here
// reaches anything but the address the user gave.
package ollama

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
	"time"
)

type Client struct {
	base string
	http *http.Client
}

// New checks the address and returns a client for it.
func New(base string) (*Client, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("enter the Ollama address like http://192.168.1.20:11434")
	}
	return &Client{base: base, http: &http.Client{}}, nil
}

// Host is the address's host, for messages.
func (c *Client) Host() string {
	u, _ := url.Parse(c.base)
	return u.Hostname()
}

// Public reports whether the address resolves to somewhere outside the
// local network, so the UI can warn before data is sent there.
func (c *Client) Public() bool {
	ips, err := net.LookupIP(c.Host())
	if err != nil {
		return false
	}
	for _, ip := range ips {
		if !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
			return true
		}
	}
	return false
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return friendly(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return apiError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func apiError(resp *http.Response) error {
	var e struct{ Error string }
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return fmt.Errorf("Ollama: %s", e.Error)
	}
	return fmt.Errorf("Ollama answered %s", resp.Status)
}

func friendly(err error) error {
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return err
	case errors.As(err, &ne) && ne.Timeout():
		return errors.New("Ollama didn't answer in time")
	case strings.Contains(err.Error(), "connection refused"):
		return errors.New("nothing is listening at that address (is Ollama running, and listening on the network?)")
	case strings.Contains(err.Error(), "no such host"):
		return errors.New("that host name doesn't resolve")
	}
	return fmt.Errorf("can't reach Ollama: %v", err)
}

// Version is a cheap reachability check.
func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct{ Version string }
	err := c.do(ctx, "GET", "/api/version", nil, &v)
	return v.Version, err
}

type Model struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Tools    bool   `json:"tools"`
	Thinking bool   `json:"thinking"`
	Context  int    `json:"context"` // the model's maximum context, in tokens
}

// Show reads one model's capabilities.
func (c *Client) Show(ctx context.Context, name string) (Model, error) {
	var d struct {
		Capabilities []string       `json:"capabilities"`
		ModelInfo    map[string]any `json:"model_info"`
	}
	if err := c.do(ctx, "POST", "/api/show", map[string]string{"model": name}, &d); err != nil {
		return Model{}, err
	}
	m := Model{Name: name}
	for _, cap := range d.Capabilities {
		switch cap {
		case "tools":
			m.Tools = true
		case "thinking":
			m.Thinking = true
		}
	}
	for k, v := range d.ModelInfo {
		if strings.HasSuffix(k, ".context_length") {
			if f, ok := v.(float64); ok {
				m.Context = int(f)
			}
		}
	}
	return m, nil
}

// Models lists what the server has, with capabilities, tool-capable first.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var tags struct {
		Models []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"models"`
	}
	if err := c.do(ctx, "GET", "/api/tags", nil, &tags); err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(tags.Models))
	for _, t := range tags.Models {
		m, err := c.Show(ctx, t.Name)
		if err != nil {
			m = Model{Name: t.Name}
		}
		m.Size = t.Size
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Tools != out[j].Tools {
			return out[i].Tools
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Chat types, as Ollama's /api/chat takes them.

type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Thinking  string     `json:"thinking,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"`
}

type ToolCall struct {
	Function struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"function"`
}

type Tool struct {
	Type     string   `json:"type"`
	Function ToolSpec `json:"function"`
}

type ToolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type ChatRequest struct {
	Model     string         `json:"model"`
	Messages  []Message      `json:"messages"`
	Tools     []Tool         `json:"tools,omitempty"`
	Stream    bool           `json:"stream"`
	Think     *bool          `json:"think,omitempty"`
	Options   map[string]any `json:"options,omitempty"`
	KeepAlive string         `json:"keep_alive,omitempty"`
}

// Chat streams one reply. onDelta gets each piece of the answer as it
// arrives, onThink each piece of the model's reasoning (thinking models
// with Think set). The full assistant message, with any tool calls, is
// returned at the end.
func (c *Client) Chat(ctx context.Context, req ChatRequest, onDelta, onThink func(string)) (Message, error) {
	req.Stream = true
	b, err := json.Marshal(req)
	if err != nil {
		return Message{}, err
	}
	hr, err := http.NewRequestWithContext(ctx, "POST", c.base+"/api/chat", bytes.NewReader(b))
	if err != nil {
		return Message{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(hr)
	if err != nil {
		return Message{}, friendly(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return Message{}, apiError(resp)
	}
	out := Message{Role: "assistant"}
	var text, think strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk struct {
			Message Message `json:"message"`
			Done    bool    `json:"done"`
			Error   string  `json:"error"`
		}
		if err := json.Unmarshal(line, &chunk); err != nil {
			return out, fmt.Errorf("Ollama sent something unexpected: %v", err)
		}
		if chunk.Error != "" {
			return out, fmt.Errorf("Ollama: %s", chunk.Error)
		}
		if chunk.Message.Thinking != "" {
			think.WriteString(chunk.Message.Thinking)
			if onThink != nil {
				onThink(chunk.Message.Thinking)
			}
		}
		if chunk.Message.Content != "" {
			text.WriteString(chunk.Message.Content)
			if onDelta != nil {
				onDelta(chunk.Message.Content)
			}
		}
		out.ToolCalls = append(out.ToolCalls, chunk.Message.ToolCalls...)
		if chunk.Done {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return out, friendly(err)
	}
	out.Content = text.String()
	out.Thinking = think.String()
	return out, nil
}

// Timeout is how long a quick call (version, models) may take.
const Timeout = 8 * time.Second
