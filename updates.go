package acpx

import (
	"encoding/json"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

// Session update kinds acp-go-sdk v0.13.5 does not decode (it hands its client another kind's
// update for them, see Wire.Classify). The Wire decodes them for the client.
const (
	UpdateNotice                 = "notice"
	UpdateCompaction             = "compaction_update"
	UpdateCompactionSummaryChunk = "compaction_summary_chunk"
)

// Notice is a user-facing notice from the agent: a limit reached, a degraded service, an error
// it recovered from. Severity is info, warning, error or the agent's own.
type Notice struct {
	Severity    string `json:"severity"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

// Compaction is the state of one compaction of the session's context: in_progress, completed,
// failed or cancelled. Summary is the text of its summary, when the update carries one.
type Compaction struct {
	ID      string `json:"compactionId"`
	Status  string `json:"status"`
	Summary string `json:"-"`
	Error   string `json:"error,omitempty"`
}

// CompactionSummaryChunk is a piece of a compaction's summary as it is written.
type CompactionSummaryChunk struct {
	ID   string `json:"compactionId"`
	Text string `json:"-"`
}

// ExtraUpdate is one of those updates; exactly one of its pointers is set.
type ExtraUpdate struct {
	SessionID    string
	Notice       *Notice
	Compaction   *Compaction
	SummaryChunk *CompactionSummaryChunk
}

// sdkKinds are the update kinds acp-go-sdk v0.13.5 decodes.
var sdkKinds = map[string]bool{
	"user_message_chunk": true, "agent_message_chunk": true, "agent_thought_chunk": true,
	"tool_call": true, "tool_call_update": true, "plan": true, "plan_update": true, "plan_removed": true,
	"available_commands_update": true, "current_mode_update": true, "config_option_update": true,
	"session_info_update": true, "usage_update": true,
}

// KindOf is the sessionUpdate kind of an update as the SDK decoded it ("" for none).
func KindOf(u acp.SessionUpdate) string {
	switch {
	case u.UserMessageChunk != nil:
		return "user_message_chunk"
	case u.AgentMessageChunk != nil:
		return "agent_message_chunk"
	case u.AgentThoughtChunk != nil:
		return "agent_thought_chunk"
	case u.ToolCall != nil:
		return "tool_call"
	case u.ToolCallUpdate != nil:
		return "tool_call_update"
	case u.Plan != nil:
		return "plan"
	case u.PlanUpdate != nil:
		return "plan_update"
	case u.PlanRemoved != nil:
		return "plan_removed"
	case u.AvailableCommandsUpdate != nil:
		return "available_commands_update"
	case u.CurrentModeUpdate != nil:
		return "current_mode_update"
	case u.ConfigOptionUpdate != nil:
		return "config_option_update"
	case u.SessionInfoUpdate != nil:
		return "session_info_update"
	case u.UsageUpdate != nil:
		return "usage_update"
	}
	return ""
}

// decodeUpdate reads a session/update notification: its kind and, for one the SDK does not
// decode, what acpx decodes of it (nil when acpx does not either).
func decodeUpdate(line []byte) (kind string, extra *ExtraUpdate, ok bool) {
	var msg struct {
		Method string `json:"method"`
		Params struct {
			SessionID string          `json:"sessionId"`
			Update    json.RawMessage `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &msg) != nil || msg.Method != "session/update" {
		return "", nil, false
	}
	var k struct {
		SessionUpdate string `json:"sessionUpdate"`
	}
	if json.Unmarshal(msg.Params.Update, &k) != nil || k.SessionUpdate == "" {
		return "", nil, false
	}
	if sdkKinds[k.SessionUpdate] {
		return k.SessionUpdate, nil, true
	}
	if u, ok := decodeExtraUpdate(line); ok {
		return k.SessionUpdate, &u, true
	}
	return k.SessionUpdate, nil, true
}

// decodeExtraUpdate reads a session/update notification of a kind the SDK does not decode.
func decodeExtraUpdate(line []byte) (ExtraUpdate, bool) {
	var msg struct {
		Method string `json:"method"`
		Params struct {
			SessionID string          `json:"sessionId"`
			Update    json.RawMessage `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &msg) != nil || msg.Method != "session/update" {
		return ExtraUpdate{}, false
	}
	var kind struct {
		SessionUpdate string `json:"sessionUpdate"`
	}
	if json.Unmarshal(msg.Params.Update, &kind) != nil {
		return ExtraUpdate{}, false
	}
	u := ExtraUpdate{SessionID: msg.Params.SessionID}
	raw := msg.Params.Update
	switch kind.SessionUpdate {
	case UpdateNotice:
		var n struct {
			Notice
			Description *string `json:"description"`
		}
		if json.Unmarshal(raw, &n) != nil || n.Title == "" {
			return ExtraUpdate{}, false
		}
		if n.Description != nil {
			n.Notice.Description = *n.Description
		}
		u.Notice = &n.Notice
	case UpdateCompaction:
		var c struct {
			Compaction
			Summary []contentBlock `json:"summary"`
			Error   *string        `json:"error"`
		}
		if json.Unmarshal(raw, &c) != nil || c.ID == "" || c.Status == "" {
			return ExtraUpdate{}, false
		}
		c.Compaction.Summary = blocksText(c.Summary)
		if c.Error != nil {
			c.Compaction.Error = *c.Error
		}
		u.Compaction = &c.Compaction
	case UpdateCompactionSummaryChunk:
		var c struct {
			ID      string       `json:"compactionId"`
			Content contentBlock `json:"content"`
		}
		if json.Unmarshal(raw, &c) != nil || c.ID == "" {
			return ExtraUpdate{}, false
		}
		u.SummaryChunk = &CompactionSummaryChunk{ID: c.ID, Text: c.Content.Text}
	default:
		return ExtraUpdate{}, false
	}
	return u, true
}

// contentBlock is the text of an ACP content block (other kinds have none).
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func blocksText(blocks []contentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
