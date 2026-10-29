package converse

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/DeGrandis/proof-of-basis/internal/ollama"
)

// The assistant's loop, as Converse must see it: system prompt apart, tool
// results answering their calls by id, a later correction as a user note,
// and the speakers alternating.
func TestToMessages(t *testing.T) {
	call := func(name string) ollama.ToolCall {
		var c ollama.ToolCall
		c.Function.Name, c.Function.Arguments = name, map[string]any{"year": 2025}
		return c
	}
	raw := types.Message{Role: types.ConversationRoleAssistant, Content: []types.ContentBlock{
		&types.ContentBlockMemberText{Value: "Looking."},
		&types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{ToolUseId: aws.String("tu_1"), Name: aws.String("tax_year"), Input: document.NewLazyDocument(map[string]any{"year": 2025})}},
	}}
	history := []ollama.Message{
		{Role: "system", Content: "You are the assistant."},
		{Role: "user", Content: "What did I owe in 2025?"},
		{Role: "assistant", Content: "Looking.", ToolCalls: []ollama.ToolCall{call("tax_year")}, Raw: raw},
		{Role: "tool", Content: "2025: gain $100"},
		{Role: "assistant", ToolCalls: []ollama.ToolCall{call("search"), call("price")}},
		{Role: "tool", Content: "found 2"},
		{Role: "tool", Content: "no price"},
		{Role: "system", Content: "Answer now."},
	}
	system, out := toMessages(history, false)
	if len(system) != 1 || system[0].(*types.SystemContentBlockMemberText).Value != "You are the assistant." {
		t.Fatalf("system %+v", system)
	}
	roles := ""
	for _, m := range out {
		roles += string(m.Role)[:1]
	}
	if roles != "uauau" {
		t.Fatalf("roles %q", roles)
	}
	result := func(b types.ContentBlock) string {
		r, ok := b.(*types.ContentBlockMemberToolResult)
		if !ok {
			return ""
		}
		return aws.ToString(r.Value.ToolUseId)
	}
	if result(out[2].Content[0]) != "tu_1" {
		t.Errorf("first round answers %q", result(out[2].Content[0]))
	}
	second := out[3].Content
	if len(second) != 2 || len(out[4].Content) != 3 ||
		result(out[4].Content[0]) != aws.ToString(second[0].(*types.ContentBlockMemberToolUse).Value.ToolUseId) ||
		result(out[4].Content[1]) != aws.ToString(second[1].(*types.ContentBlockMemberToolUse).Value.ToolUseId) {
		t.Errorf("second round: %d tool uses, %d answers", len(second), len(out[4].Content))
	}
	if note, ok := out[4].Content[2].(*types.ContentBlockMemberText); !ok || note.Value != "[Note from the app] Answer now." {
		t.Errorf("correction: %#v", out[4].Content[2])
	}
	// The last round offers no tools: Converse then refuses tool blocks,
	// so the same history goes as text.
	_, out = toMessages(history, true)
	for _, m := range out {
		for _, b := range m.Content {
			if _, ok := b.(*types.ContentBlockMemberText); !ok {
				t.Fatalf("plain history has a %T", b)
			}
		}
	}
	if s := out[1].Content[0].(*types.ContentBlockMemberText).Value; !strings.Contains(s, "Looking.") || !strings.Contains(s, "[Lookup: tax_year") {
		t.Errorf("plain call: %q", s)
	}
}

func TestDisplayName(t *testing.T) {
	for id, want := range map[string]string{
		"us.meta.llama3-3-70b-instruct-v1:0": "Llama 3.3 70B (AWS Bedrock)",
		"openai.gpt-oss-20b-1:0":             "gpt-oss 20B (AWS Bedrock)",
		"us.amazon.nova-pro-v1:0":            "Amazon Nova Pro (AWS Bedrock)",
		"us.amazon.nova-2-lite-v1:0":         "Amazon Nova 2 Lite (AWS Bedrock)",
	} {
		if got := DisplayName(id); got != want {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
	}
}

// Llama's tool calls written as text become real calls; prose, unknown
// tools and JSON that isn't a call stay an answer.
func TestTextToolCalls(t *testing.T) {
	offered := []ollama.Tool{{Function: ollama.ToolSpec{Name: "calculate"}}, {Function: ollama.ToolSpec{Name: "get_tax_year"}}}
	for in, want := range map[string]string{
		`{"type": "function", "name": "calculate", "parameters": {"expression": "1.00190665"}}`:                              "calculate",
		`<|python_tag|>{"name": "get_tax_year", "arguments": {"year": 2025}}`:                                                "get_tax_year",
		`{"name": "calculate", "parameters": {"expression": "2*3"}}; {"name": "get_tax_year", "parameters": {"year": 2025}}`: "calculate,get_tax_year",
		"```json\n[{\"name\": \"get_tax_year\", \"parameters\": {\"year\": 2024}}]\n```":                                     "get_tax_year",
		`{"name": "get_tax_year", "arguments": "{\"year\": 2025}"}`:                                                          "get_tax_year",
		`You hold 1.0019 BTC.`:                                "",
		`{"name": "delete_everything", "parameters": {}}`:     "",
		`{"balance": 1.0019}`:                                 "",
		`Here it is: {"name": "calculate", "parameters": {}}`: "",
	} {
		var names []string
		for _, c := range textToolCalls(in, offered) {
			names = append(names, c.Function.Name)
		}
		if got := strings.Join(names, ","); got != want {
			t.Errorf("%s\n  got %q, want %q", in, got, want)
		}
	}
	if c := textToolCalls(`{"name": "get_tax_year", "arguments": "{\"year\": 2025}"}`, offered); c[0].Function.Arguments["year"] != float64(2025) {
		t.Errorf("arguments from a string: %+v", c[0].Function.Arguments)
	}
}

// Tool names Converse can take back: no prefix, no format tags.
func TestToolName(t *testing.T) {
	for in, want := range map[string]string{
		"analyze_records":                      "analyze_records",
		"functions.get_record":                 "get_record",
		"analyze_records<|channel|>commentary": "analyze_records",
		" calculate json":                      "calculate",
		"<|call|>":                             "unknown_tool",
	} {
		if got := toolName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// The app's note of an earlier answer's lookups (a system message after it)
// goes as a note from the app, not as part of the answer the model would
// copy.
func TestLookupsNoteApart(t *testing.T) {
	_, out := toMessages([]ollama.Message{
		{Role: "system", Content: "You are the assistant."},
		{Role: "user", Content: "biggest sale?"},
		{Role: "assistant", Content: "It was on 2025-10-04."},
		{Role: "system", Content: `Lookups behind that answer: analyze_records {"source":"sales"} -> [ex:431]`},
		{Role: "user", Content: "which account?"},
	}, false)
	if len(out) != 3 {
		t.Fatalf("%d messages", len(out))
	}
	if a := out[1].Content[0].(*types.ContentBlockMemberText).Value; a != "It was on 2025-10-04." {
		t.Errorf("answer: %q", a)
	}
	if n := out[2].Content[0].(*types.ContentBlockMemberText).Value; !strings.Contains(n, "[Note from the app] Lookups behind that answer: analyze_records") {
		t.Errorf("note: %q", n)
	}
}
