package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/DeGrandis/proof-of-basis/internal/ollama"
)

// The assistant's loop, as Claude must see it: system prompt apart, tool
// results answering their calls by id, a later correction as a user note,
// and the speakers alternating.
func TestToParams(t *testing.T) {
	call := func(name string, args map[string]any) ollama.ToolCall {
		var c ollama.ToolCall
		c.Function.Name, c.Function.Arguments = name, args
		return c
	}
	raw := anthropic.NewAssistantMessage(anthropic.NewTextBlock("Looking."), anthropic.NewToolUseBlock("toolu_1", map[string]any{"year": 2025}, "tax_year"))
	msgs := []ollama.Message{
		{Role: "system", Content: "You are the assistant."},
		{Role: "user", Content: "Earlier question"},
		{Role: "assistant", Content: "Earlier answer"},
		{Role: "user", Content: "What did I owe in 2025?"},
		{Role: "assistant", Content: "Looking.", ToolCalls: []ollama.ToolCall{call("tax_year", map[string]any{"year": 2025})}, Raw: raw},
		{Role: "tool", ToolName: "tax_year", Content: "2025: gain $100"},
		{Role: "assistant", Content: "", ToolCalls: []ollama.ToolCall{call("search", nil), call("price", nil)}},
		{Role: "tool", Content: "found 2"},
		{Role: "tool", Content: "Error: no price"},
		{Role: "system", Content: "Answer now."},
	}
	system, out := toParams(msgs)
	if len(system) != 1 || system[0].Text != "You are the assistant." {
		t.Fatalf("system %+v", system)
	}
	b, _ := json.Marshal(out)
	var got []struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			ID        string `json:"id"`
			ToolUseID string `json:"tool_use_id"`
			IsError   bool   `json:"is_error"`
		} `json:"content"`
	}
	json.Unmarshal(b, &got)
	roles := []string{}
	for _, m := range got {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "user,assistant,user,assistant,user,assistant,user" {
		t.Fatalf("roles %v\n%s", roles, b)
	}
	if got[3].Content[1].ID != "toolu_1" || got[4].Content[0].ToolUseID != "toolu_1" {
		t.Errorf("first tool round: %s", b)
	}
	last := got[6].Content
	if len(last) != 3 || last[0].ToolUseID != got[5].Content[0].ID || last[1].ToolUseID != got[5].Content[1].ID || !last[1].IsError ||
		!strings.Contains(last[2].Text, "Answer now.") {
		t.Errorf("second tool round: %s", b)
	}
}

func TestToTools(t *testing.T) {
	in := []ollama.Tool{{Type: "function", Function: ollama.ToolSpec{Name: "price", Description: "BTC price on a day",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{"day": map[string]any{"type": "string"}}, "required": []string{"day"}}}}}
	b, _ := json.Marshal(toTools(in))
	s := string(b)
	for _, want := range []string{`"name":"price"`, `"description":"BTC price on a day"`, `"required":["day"]`, `"type":"object"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
}
