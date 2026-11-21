package codex

import (
	"time"

	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/harness"
)

const Harness = "codex"

// tool_input, tool_response and last_assistant_message are never decoded so we never touch prompt data
type Hook struct {
	Event     string `json:"hook_event_name"`
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	Cwd       string `json:"cwd"`
	Model     string `json:"model"`
	Source    string `json:"source"` // startup, resume, clear, compact, fork
	Prompt    string `json:"prompt"` // hashed, never stored
	ToolName  string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`
}

type State struct {
	OpenTurn    string          `json:"open_turn,omitempty"`
	LastEventAt time.Time       `json:"last_event_at"`
	Tools       map[string]tool `json:"tools,omitempty"` // running, by tool_use_id
	LastPre     string          `json:"last_pre,omitempty"`
	Pending     []pending       `json:"pending,omitempty"` // unanswered permission dialogs
}

type tool struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

type pending struct {
	ToolUseID string    `json:"tool_use_id"`
	Tool      string    `json:"tool"`
	At        time.Time `json:"at"`
}

type Input struct {
	Hook      Hook
	Now       time.Time
	Salt      []byte
	ProjectID string
	Session   string
}

// pure, no io
func Map(st State, in Input) (State, []event.Event) {
	h := in.Hook
	m := mapper{in: in}

	switch h.Event {
	case "SessionStart":
		if h.Source != "compact" { // same session continues
			m.emit(event.SessionStart, in.Now, event.SessionStartData{Model: h.Model, HarnessSession: h.SessionID})
		}

	case "SessionEnd":
		st = m.denyPending(st)
		st = m.closeTurn(st, st.LastEventAt, "closed")
		m.emit(event.SessionEnd, in.Now, event.SessionEndData{Reason: "exit"})

	case "UserPromptSubmit":
		// codex reports interrupts, this only catches a turn whose end we missed
		st = m.denyPending(st)
		st = m.closeTurn(st, st.LastEventAt, "interrupted")
		m.out = append(m.out, m.emitter().Prompt(in.Now, h.Prompt, false))
		m.emit(event.AgentTurnStart, in.Now, event.AgentTurnStartData{TurnID: h.TurnID})
		st.OpenTurn = h.TurnID

	case "Stop":
		st = m.denyPending(st)
		st = m.closeTurn(st, in.Now, "done")

	case "Interrupt":
		// also how a denied dialog ends
		st = m.denyPending(st)
		m.emit(event.Interrupt, in.Now, event.InterruptData{})
		st = m.closeTurn(st, in.Now, "interrupted")

	case "PreToolUse":
		if st.Tools == nil {
			st.Tools = map[string]tool{}
		}
		st.Tools[h.ToolUseID] = tool{Name: h.ToolName, At: in.Now}
		st.LastPre = h.ToolUseID

	case "PermissionRequest":
		p := pending{Tool: h.ToolName, At: in.Now}
		if t, ok := st.Tools[st.LastPre]; ok && t.Name == h.ToolName {
			p.ToolUseID = st.LastPre
		}
		st.Pending = append(st.Pending, p)

	case "PostToolUse":
		for i, p := range st.Pending {
			if p.ToolUseID != "" && p.ToolUseID == h.ToolUseID {
				m.emit(event.Approval, in.Now, event.ApprovalData{
					Allow:     true,
					LatencyMs: max(0, in.Now.Sub(p.At).Milliseconds()),
					Tool:      category(p.Tool),
				})
				st.Pending = append(st.Pending[:i], st.Pending[i+1:]...)
				break
			}
		}
		if t, ok := st.Tools[h.ToolUseID]; ok {
			delete(st.Tools, h.ToolUseID)
			m.emit(event.ToolCall, in.Now, event.ToolCallData{
				Tool:       category(t.Name),
				DurationMs: max(0, in.Now.Sub(t.At).Milliseconds()),
			})
		}
	}

	st.LastEventAt = in.Now
	return st, m.out
}

func (m *mapper) denyPending(st State) State {
	for _, p := range st.Pending {
		m.emit(event.Approval, m.in.Now, event.ApprovalData{
			Allow:     false,
			LatencyMs: max(0, m.in.Now.Sub(p.At).Milliseconds()),
			Tool:      category(p.Tool),
		})
	}
	st.Pending = nil
	st.Tools = nil
	return st
}

func (m *mapper) closeTurn(st State, at time.Time, reason string) State {
	if st.OpenTurn == "" {
		return st
	}
	m.emit(event.AgentTurnEnd, at, event.AgentTurnEndData{TurnID: st.OpenTurn, Reason: reason})
	st.OpenTurn = ""
	return st
}

type mapper struct {
	in  Input
	out []event.Event
}

func (m *mapper) session() string {
	if m.in.Session != "" {
		return m.in.Session
	}
	return m.in.Hook.SessionID
}

func (m *mapper) emitter() harness.Emitter {
	return harness.Emitter{Harness: Harness, Session: m.session(), Project: m.in.ProjectID, Salt: m.in.Salt}
}

func (m *mapper) emit(kind event.Kind, ts time.Time, data any) {
	m.out = append(m.out, m.emitter().Event(kind, ts, data))
}

func category(tool string) event.ToolCategory {
	switch tool {
	case "Bash", "shell", "exec_command":
		return event.ToolShell
	case "apply_patch", "Edit", "Write":
		return event.ToolEdit
	case "Read", "view_image":
		return event.ToolRead
	case "web_search", "WebSearch", "WebFetch":
		return event.ToolWeb
	case "spawn_agent", "Agent":
		return event.ToolAgent
	}
	return event.ToolOther
}
