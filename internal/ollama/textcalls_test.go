package ollama

import (
	"strings"
	"testing"
)

// Qwen-style calls written out as text become real calls, with their
// arguments typed and the markup taken out of the answer; a call to a tool
// that wasn't offered leaves the text alone.
func TestXMLToolCalls(t *testing.T) {
	offered := []Tool{{Function: ToolSpec{Name: "analyze_records"}}, {Function: ToolSpec{Name: "get_tax_year"}}}
	in := "Let me check.\n<tool_call>\n<function=analyze_records>\n<parameter=source>\nbuys\n</parameter>\n<parameter=top_n>\n5\n</parameter>\n</function>\n</tool_call>"
	calls, rest := TextToolCalls(in, offered)
	if len(calls) != 1 || calls[0].Function.Name != "analyze_records" || calls[0].Function.Arguments["source"] != "buys" || calls[0].Function.Arguments["top_n"] != float64(5) {
		t.Fatalf("calls: %+v", calls)
	}
	if rest != "Let me check." {
		t.Errorf("rest: %q", rest)
	}
	// No <tool_call> wrapper, and two calls.
	calls, rest = TextToolCalls("<function=get_tax_year><parameter=year>2024</parameter></function><function=get_tax_year><parameter=year>2025</parameter></function>", offered)
	if len(calls) != 2 || calls[1].Function.Arguments["year"] != float64(2025) || rest != "" {
		t.Errorf("two calls: %+v %q", calls, rest)
	}
	in = "<tool_call><function=delete_everything></function></tool_call>"
	if calls, rest := TextToolCalls(in, offered); calls != nil || rest != in {
		t.Errorf("unknown tool: %+v %q", calls, rest)
	}
	if calls, rest := TextToolCalls("You hold 1.0019 BTC.", offered); calls != nil || !strings.Contains(rest, "1.0019") {
		t.Errorf("prose: %+v %q", calls, rest)
	}
}
