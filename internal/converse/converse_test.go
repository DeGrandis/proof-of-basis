package converse

import (
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
	system, out := toMessages([]ollama.Message{
		{Role: "system", Content: "You are the assistant."},
		{Role: "user", Content: "What did I owe in 2025?"},
		{Role: "assistant", Content: "Looking.", ToolCalls: []ollama.ToolCall{call("tax_year")}, Raw: raw},
		{Role: "tool", Content: "2025: gain $100"},
		{Role: "assistant", ToolCalls: []ollama.ToolCall{call("search"), call("price")}},
		{Role: "tool", Content: "found 2"},
		{Role: "tool", Content: "no price"},
		{Role: "system", Content: "Answer now."},
	})
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
}

func TestDisplayName(t *testing.T) {
	for id, want := range map[string]string{
		"us.meta.llama3-3-70b-instruct-v1:0": "Llama 3.3 70B (AWS Bedrock)",
		"us.amazon.nova-pro-v1:0":            "Amazon Nova Pro (AWS Bedrock)",
		"us.amazon.nova-2-lite-v1:0":         "Amazon Nova 2 Lite (AWS Bedrock)",
	} {
		if got := DisplayName(id); got != want {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
	}
}
