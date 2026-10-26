// Package converse runs the demo's assistant on a non-Claude model in
// Amazon Bedrock (Llama, Amazon Nova and others) through Bedrock's
// Converse API. Like the claude package, it takes and returns the Ollama
// client's chat shapes, so the assistant's loop works unchanged. It needs
// only bedrock:InvokeModel; credentials come from the usual AWS chain (on
// EC2, the instance's role).
package converse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"

	"github.com/DeGrandis/proof-of-basis/internal/ollama"
)

type Client struct {
	c         *bedrockruntime.Client
	model     string
	name      string
	maxTokens int32
}

// New connects to Bedrock in a region and checks that AWS credentials are
// there; nothing is sent to the model until the first question.
func New(ctx context.Context, region, model string) (*Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, err
	}
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return nil, fmt.Errorf("failed to resolve AWS credentials: %w", err)
	}
	return &Client{c: bedrockruntime.NewFromConfig(cfg), model: model, name: DisplayName(model), maxTokens: 2048}, nil
}

// Name is how the model is shown in the app.
func (c *Client) Name() string { return c.name }

// DisplayName turns a Bedrock model ID into a short name.
func DisplayName(model string) string {
	m := model
	for _, p := range []string{"us.", "eu.", "apac.", "global."} {
		m = strings.TrimPrefix(m, p)
	}
	switch {
	case strings.HasPrefix(m, "openai.gpt-oss-"):
		m = "gpt-oss " + strings.ToUpper(strings.SplitN(strings.TrimPrefix(m, "openai.gpt-oss-"), "-", 2)[0])
	case strings.HasPrefix(m, "meta.llama3-3-70b"):
		m = "Llama 3.3 70B"
	case strings.HasPrefix(m, "meta.llama3-1-70b"):
		m = "Llama 3.1 70B"
	case strings.HasPrefix(m, "amazon.nova-"):
		m = "Amazon " + strings.Title(strings.ReplaceAll(strings.SplitN(strings.TrimPrefix(m, "amazon."), "-v", 2)[0], "-", " "))
	}
	return m + " (AWS Bedrock)"
}

// Chat asks for one reply. Converse answers whole (not streamed), so the
// text reaches onDelta in one piece. The reply's own form is kept on Raw so
// the next round sends it back unchanged.
func (c *Client) Chat(ctx context.Context, req ollama.ChatRequest, onDelta, onThink func(string)) (ollama.Message, error) {
	// With no tools offered (the last round), Converse refuses tool blocks
	// in the history, so earlier lookups go as plain text.
	system, msgs := toMessages(req.Messages, len(req.Tools) == 0)
	in := &bedrockruntime.ConverseInput{
		ModelId:         aws.String(c.model),
		Messages:        msgs,
		System:          system,
		InferenceConfig: &types.InferenceConfiguration{MaxTokens: aws.Int32(c.maxTokens), Temperature: aws.Float32(0.2)},
	}
	if len(req.Tools) > 0 {
		in.ToolConfig = &types.ToolConfiguration{Tools: toTools(req.Tools)}
	}
	out, err := c.c.Converse(ctx, in)
	if err != nil {
		return ollama.Message{Role: "assistant"}, friendly(err)
	}
	reply := ollama.Message{Role: "assistant"}
	m, ok := out.Output.(*types.ConverseOutputMemberMessage)
	if !ok {
		return reply, errors.New("the model sent no reply")
	}
	// Reasoning (gpt-oss thinks first) goes to the thinking view and is
	// left out of the reply sent back next round: not every model takes
	// its reasoning back.
	kept := types.Message{Role: m.Value.Role}
	var text strings.Builder
	for _, b := range m.Value.Content {
		if r, ok := b.(*types.ContentBlockMemberReasoningContent); ok {
			if t, ok := r.Value.(*types.ReasoningContentBlockMemberReasoningText); ok && onThink != nil && t.Value.Text != nil {
				onThink(*t.Value.Text)
			}
			continue
		}
		kept.Content = append(kept.Content, b)
		switch x := b.(type) {
		case *types.ContentBlockMemberText:
			text.WriteString(x.Value)
		case *types.ContentBlockMemberToolUse:
			var tc ollama.ToolCall
			tc.Function.Name = aws.ToString(x.Value.Name)
			tc.Function.Arguments = map[string]any{}
			if x.Value.Input != nil {
				x.Value.Input.UnmarshalSmithyDocument(&tc.Function.Arguments)
			}
			reply.ToolCalls = append(reply.ToolCalls, tc)
		}
	}
	reply.Raw = kept
	reply.Content = text.String()
	// Llama sometimes writes its tool calls out as JSON text instead of
	// making them; when the whole reply is such calls to tools it was
	// offered, make them real. The reply is then rebuilt with tool-use
	// blocks when sent back, so it isn't replayed as the text.
	if len(reply.ToolCalls) == 0 {
		if calls := textToolCalls(reply.Content, req.Tools); len(calls) > 0 {
			reply.ToolCalls, reply.Content, reply.Raw = calls, "", nil
		}
	}
	if reply.Content != "" && onDelta != nil {
		onDelta(reply.Content)
	}
	return reply, nil
}

// textToolCalls reads a reply that is nothing but tool calls written as
// JSON, in the forms Llama uses: {"type":"function","name":...,"parameters":
// {...}}, {"name":...,"arguments":{...}}, a list of them, or several in a
// row (separated by ";" or new lines), maybe after <|python_tag|> or in a
// ```json fence. Every call must name an offered tool; anything else (prose,
// or JSON that isn't a call) returns nil and stays an answer.
func textToolCalls(text string, offered []ollama.Tool) []ollama.ToolCall {
	known := map[string]bool{}
	for _, t := range offered {
		known[t.Function.Name] = true
	}
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "<|python_tag|>"))
	if strings.HasPrefix(s, "```") {
		s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "```json"), "```"), "```")
	}
	s = strings.TrimSpace(s)
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return nil
	}
	var objs []map[string]any
	if s[0] == '[' {
		if json.Unmarshal([]byte(s), &objs) != nil {
			return nil
		}
	} else {
		for rest := s; ; {
			rest = strings.TrimLeft(rest, " \t\r\n;,")
			if rest == "" {
				break
			}
			dec := json.NewDecoder(strings.NewReader(rest))
			var o map[string]any
			if dec.Decode(&o) != nil {
				return nil
			}
			objs = append(objs, o)
			rest = rest[dec.InputOffset():]
		}
	}
	var calls []ollama.ToolCall
	for _, o := range objs {
		name, _ := o["name"].(string)
		if !known[name] {
			return nil
		}
		args := o["parameters"]
		if args == nil {
			args = o["arguments"]
		}
		var tc ollama.ToolCall
		tc.Function.Name = name
		switch a := args.(type) {
		case map[string]any:
			tc.Function.Arguments = a
		case string: // arguments given as a JSON string
			if json.Unmarshal([]byte(a), &tc.Function.Arguments) != nil {
				return nil
			}
		case nil:
			tc.Function.Arguments = map[string]any{}
		default:
			return nil
		}
		calls = append(calls, tc)
	}
	return calls
}

// toMessages turns the assistant's messages into Converse's: leading system
// messages become the system prompt; a later one (a correction from the
// app) becomes a note in the user's turn; tool results answer the tool
// calls before them, in order; and turns by the same speaker are merged so
// they alternate.
func toMessages(in []ollama.Message, plain bool) ([]types.SystemContentBlock, []types.Message) {
	var system []types.SystemContentBlock
	var out []types.Message
	var pending []string // tool-use ids waiting for their results
	add := func(role types.ConversationRole, blocks ...types.ContentBlock) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, types.Message{Role: role, Content: blocks})
	}
	text := func(s string) []types.ContentBlock {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []types.ContentBlock{&types.ContentBlockMemberText{Value: s}}
	}
	for i, m := range in {
		switch m.Role {
		case "system":
			if len(out) == 0 {
				if strings.TrimSpace(m.Content) != "" {
					system = append(system, &types.SystemContentBlockMemberText{Value: m.Content})
				}
				continue
			}
			add(types.ConversationRoleUser, text("[Note from the app] "+m.Content)...)
		case "user":
			add(types.ConversationRoleUser, text(m.Content)...)
		case "assistant":
			pending = pending[:0]
			if plain {
				var b strings.Builder
				if raw, ok := m.Raw.(types.Message); ok {
					for _, blk := range raw.Content {
						if t, ok := blk.(*types.ContentBlockMemberText); ok {
							b.WriteString(t.Value)
						}
					}
				} else {
					b.WriteString(m.Content)
				}
				for _, tc := range m.ToolCalls {
					args, _ := json.Marshal(tc.Function.Arguments)
					fmt.Fprintf(&b, "\n[Lookup: %s %s]", tc.Function.Name, args)
				}
				add(types.ConversationRoleAssistant, text(b.String())...)
				continue
			}
			if raw, ok := m.Raw.(types.Message); ok {
				for _, b := range raw.Content {
					if tu, ok := b.(*types.ContentBlockMemberToolUse); ok {
						pending = append(pending, aws.ToString(tu.Value.ToolUseId))
					}
				}
				add(types.ConversationRoleAssistant, raw.Content...)
				continue
			}
			blocks := text(m.Content)
			for j, tc := range m.ToolCalls {
				id := fmt.Sprintf("tooluse_app_%d_%d", i, j)
				pending = append(pending, id)
				args := tc.Function.Arguments
				if args == nil {
					args = map[string]any{}
				}
				blocks = append(blocks, &types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{
					ToolUseId: aws.String(id), Name: aws.String(tc.Function.Name), Input: document.NewLazyDocument(args)}})
			}
			add(types.ConversationRoleAssistant, blocks...)
		case "tool":
			if len(pending) == 0 {
				add(types.ConversationRoleUser, text("[Lookup result] "+m.Content)...)
				continue
			}
			id := pending[0]
			pending = pending[1:]
			add(types.ConversationRoleUser, &types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{
				ToolUseId: aws.String(id),
				Content:   []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberText{Value: m.Content}}}})
		}
	}
	return system, out
}

// toTools turns Ollama tool definitions (JSON schema objects) into
// Converse's.
func toTools(in []ollama.Tool) []types.Tool {
	out := make([]types.Tool, 0, len(in))
	for _, t := range in {
		schema := t.Function.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, &types.ToolMemberToolSpec{Value: types.ToolSpecification{
			Name: aws.String(t.Function.Name), Description: aws.String(t.Function.Description),
			InputSchema: &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(schema)}}})
	}
	return out
}

// friendly turns AWS errors into something a visitor can read.
func friendly(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "AccessDeniedException", "UnrecognizedClientException":
			return errors.New("the demo's assistant isn't allowed to reach its model right now")
		case "ThrottlingException", "ServiceQuotaExceededException":
			return errors.New("the demo's assistant is busy; try again in a minute")
		case "ValidationException":
			return fmt.Errorf("the model couldn't take that question: %s", ae.ErrorMessage())
		}
	}
	return fmt.Errorf("the assistant couldn't answer: %v", err)
}
