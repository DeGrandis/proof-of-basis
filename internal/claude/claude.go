// Package claude runs the assistant on Claude in Amazon Bedrock, for the
// public demo, where there is no Ollama server to use. It takes and returns
// the same chat shapes as the Ollama client, so the assistant's loop works
// unchanged. Credentials come from the usual AWS chain (on EC2, the
// instance's role).
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/bedrock"

	"github.com/DeGrandis/proof-of-basis/internal/ollama"
)

type Client struct {
	c         *bedrock.MantleClient
	model     string
	name      string
	maxTokens int64
}

// New connects to Claude in Amazon Bedrock in a region. Nothing is sent
// until the first question.
func New(ctx context.Context, region, model string) (*Client, error) {
	c, err := bedrock.NewMantleClient(ctx, bedrock.MantleClientConfig{AWSRegion: region})
	if err != nil {
		return nil, err
	}
	return &Client{c: c, model: model, name: displayName(model), maxTokens: 4096}, nil
}

// Name is how the model is shown in the app.
func (c *Client) Name() string { return c.name }

func displayName(model string) string {
	m := strings.TrimPrefix(model, "anthropic.")
	if strings.HasPrefix(m, "claude-haiku") {
		return "Claude Haiku (AWS Bedrock)"
	}
	return m + " (AWS Bedrock)"
}

// Chat streams one reply: answer text to onDelta as it arrives. Tool calls
// come back on the message, and the reply's own content is kept on Raw so
// the next round sends it back unchanged.
func (c *Client) Chat(ctx context.Context, req ollama.ChatRequest, onDelta, onThink func(string)) (ollama.Message, error) {
	system, msgs := toParams(req.Messages)
	params := anthropic.MessageNewParams{
		Model:        anthropic.Model(c.model),
		MaxTokens:    c.maxTokens,
		Messages:     msgs,
		System:       system,
		OutputConfig: anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffortLow},
	}
	if len(req.Tools) > 0 {
		params.Tools = toTools(req.Tools)
	}
	stream := c.c.Messages.NewStreaming(ctx, params)
	var msg anthropic.Message
	for stream.Next() {
		ev := stream.Current()
		msg.Accumulate(ev)
		if d, ok := ev.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			switch x := d.Delta.AsAny().(type) {
			case anthropic.TextDelta:
				if onDelta != nil {
					onDelta(x.Text)
				}
			case anthropic.ThinkingDelta:
				if onThink != nil && x.Thinking != "" {
					onThink(x.Thinking)
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return ollama.Message{Role: "assistant"}, friendly(err)
	}
	out := ollama.Message{Role: "assistant", Raw: msg.ToParam()}
	var text strings.Builder
	for _, b := range msg.Content {
		switch x := b.AsAny().(type) {
		case anthropic.TextBlock:
			text.WriteString(x.Text)
		case anthropic.ToolUseBlock:
			var tc ollama.ToolCall
			tc.Function.Name = x.Name
			if err := json.Unmarshal([]byte(x.JSON.Input.Raw()), &tc.Function.Arguments); err != nil {
				tc.Function.Arguments = map[string]any{}
			}
			out.ToolCalls = append(out.ToolCalls, tc)
		}
	}
	out.Content = text.String()
	if msg.StopReason == anthropic.StopReasonRefusal && out.Content == "" {
		out.Content = "I can't help with that one."
		if onDelta != nil {
			onDelta(out.Content)
		}
	}
	return out, nil
}

// toParams turns the assistant's messages into Claude's: leading system
// messages become the system prompt; a later one (a correction from the
// app) becomes a note in the user's turn; tool results answer the tool
// calls before them, in order; and turns by the same speaker are merged so
// they alternate.
func toParams(in []ollama.Message) ([]anthropic.TextBlockParam, []anthropic.MessageParam) {
	var system []anthropic.TextBlockParam
	var out []anthropic.MessageParam
	var pending []string // tool_use ids waiting for their results
	add := func(role anthropic.MessageParamRole, blocks ...anthropic.ContentBlockParamUnion) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, anthropic.MessageParam{Role: role, Content: blocks})
	}
	text := func(s string) []anthropic.ContentBlockParamUnion {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(s)}
	}
	for i, m := range in {
		switch m.Role {
		case "system":
			if len(out) == 0 {
				if strings.TrimSpace(m.Content) != "" {
					system = append(system, anthropic.TextBlockParam{Text: m.Content})
				}
				continue
			}
			add(anthropic.MessageParamRoleUser, text("[Note from the app] "+m.Content)...)
		case "user":
			add(anthropic.MessageParamRoleUser, text(m.Content)...)
		case "assistant":
			pending = pending[:0]
			if raw, ok := m.Raw.(anthropic.MessageParam); ok {
				for _, b := range raw.Content {
					if b.OfToolUse != nil {
						pending = append(pending, b.OfToolUse.ID)
					}
				}
				add(anthropic.MessageParamRoleAssistant, raw.Content...)
				continue
			}
			blocks := text(m.Content)
			for j, tc := range m.ToolCalls {
				id := fmt.Sprintf("toolu_app_%d_%d", i, j)
				pending = append(pending, id)
				blocks = append(blocks, anthropic.NewToolUseBlock(id, tc.Function.Arguments, tc.Function.Name))
			}
			add(anthropic.MessageParamRoleAssistant, blocks...)
		case "tool":
			if len(pending) == 0 {
				add(anthropic.MessageParamRoleUser, text("[Lookup result] "+m.Content)...)
				continue
			}
			id := pending[0]
			pending = pending[1:]
			add(anthropic.MessageParamRoleUser, anthropic.NewToolResultBlock(id, m.Content, strings.HasPrefix(m.Content, "Error:")))
		}
	}
	return system, out
}

// toTools turns Ollama tool definitions (JSON schema objects) into Claude's.
func toTools(in []ollama.Tool) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(in))
	for _, t := range in {
		schema := anthropic.ToolInputSchemaParam{}
		if p, ok := t.Function.Parameters.(map[string]any); ok {
			schema.Properties = p["properties"]
			switch r := p["required"].(type) {
			case []string:
				schema.Required = r
			case []any:
				for _, x := range r {
					if s, ok := x.(string); ok {
						schema.Required = append(schema.Required, s)
					}
				}
			}
		}
		tool := anthropic.ToolParam{Name: t.Function.Name, Description: anthropic.String(t.Function.Description), InputSchema: schema}
		out = append(out, anthropic.ToolUnionParam{OfTool: &tool})
	}
	return out
}

// friendly turns AWS and API errors into something a visitor can read.
func friendly(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 401, 403:
			return errors.New("the demo's assistant isn't allowed to reach its model right now")
		case 429:
			return errors.New("the demo's assistant is busy; try again in a minute")
		}
		if apiErr.StatusCode >= 500 {
			return errors.New("the model service had a problem; try again shortly")
		}
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return fmt.Errorf("the assistant couldn't answer: %v", err)
}
