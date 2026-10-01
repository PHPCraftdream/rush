package agent

import "github.com/PHPCraftdream/rush/internal/message"

// deferUserMessagesPastToolRound reorders history so a user message persisted
// between an assistant tool_use message and the tool message carrying its
// results (a web InjectMessage or `rush sessions inject` landing while the
// tool runs: its created_at precedes the tool message's) sits AFTER the
// round. Providers reject tool_use not immediately followed by its
// tool_result, and the session would stay locked on every later turn (#1066).
// A round whose results never arrive is left untouched: the orphan filter
// supplies a synthetic result right after the assistant message, which is
// already valid.
func deferUserMessagesPastToolRound(msgs []message.Message) []message.Message {
	out := make([]message.Message, 0, len(msgs))
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		out = append(out, m)
		if m.Role != message.Assistant {
			continue
		}
		need := make(map[string]struct{})
		for _, tc := range m.ToolCalls() {
			need[tc.ID] = struct{}{}
		}
		if len(need) == 0 {
			continue
		}
		var tools, users []message.Message
		end := -1
	scan:
		for j := i + 1; j < len(msgs); j++ {
			switch msgs[j].Role {
			case message.Tool:
				tools = append(tools, msgs[j])
				for _, tr := range msgs[j].ToolResults() {
					delete(need, tr.ToolCallID)
				}
				if len(need) == 0 {
					end = j
					break scan
				}
			case message.User:
				users = append(users, msgs[j])
			default:
				break scan // another assistant step: not an injection window
			}
		}
		if end < 0 || len(users) == 0 {
			continue
		}
		out = append(out, tools...)
		out = append(out, users...)
		i = end
	}
	return out
}
