package ollama

import (
	"encoding/json"
	"regexp"
	"strings"
)

// TextToolCalls reads tool calls a model wrote out as text instead of
// making them, in the forms models use when their call isn't recognized:
//
//   - JSON, the reply being nothing but calls, as Llama writes them:
//     {"type":"function","name":...,"parameters":{...}}, {"name":...,
//     "arguments":{...}}, a list of them, or several in a row (separated by
//     ";" or new lines), maybe after <|python_tag|> or in a ```json fence;
//   - Qwen's XML, anywhere in the reply: <tool_call><function=name>
//     <parameter=key>value</parameter></function></tool_call>.
//
// Every call must name an offered tool; anything else returns no calls and
// the reply stays an answer. rest is the reply without the calls.
func TextToolCalls(text string, offered []Tool) (calls []ToolCall, rest string) {
	known := map[string]bool{}
	for _, t := range offered {
		known[t.Function.Name] = true
	}
	if calls := jsonCalls(text, known); calls != nil {
		return calls, ""
	}
	return xmlCalls(text, known)
}

func jsonCalls(text string, known map[string]bool) []ToolCall {
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
	var calls []ToolCall
	for _, o := range objs {
		name, _ := o["name"].(string)
		if !known[name] {
			return nil
		}
		args := o["parameters"]
		if args == nil {
			args = o["arguments"]
		}
		var tc ToolCall
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

var (
	xmlCallRe  = regexp.MustCompile(`(?s)(?:<tool_call>\s*)?<function=([A-Za-z0-9_.-]+)>(.*?)</function>(?:\s*</tool_call>)?`)
	xmlParamRe = regexp.MustCompile(`(?s)<parameter=([A-Za-z0-9_-]+)>(.*?)</parameter>`)
)

func xmlCalls(text string, known map[string]bool) ([]ToolCall, string) {
	found := xmlCallRe.FindAllStringSubmatch(text, -1)
	if len(found) == 0 {
		return nil, text
	}
	var calls []ToolCall
	for _, m := range found {
		name := strings.TrimPrefix(m[1], "functions.")
		if !known[name] {
			return nil, text
		}
		var tc ToolCall
		tc.Function.Name = name
		tc.Function.Arguments = map[string]any{}
		for _, p := range xmlParamRe.FindAllStringSubmatch(m[2], -1) {
			v := strings.TrimSpace(p[2])
			var parsed any // a number, true/false or an object stays one
			if json.Unmarshal([]byte(v), &parsed) == nil {
				tc.Function.Arguments[p[1]] = parsed
			} else {
				tc.Function.Arguments[p[1]] = v
			}
		}
		calls = append(calls, tc)
	}
	rest := xmlCallRe.ReplaceAllString(text, "")
	rest = strings.NewReplacer("<tool_call>", "", "</tool_call>", "").Replace(rest)
	return calls, strings.TrimSpace(rest)
}
