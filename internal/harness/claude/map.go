package claude

import (
	"time"

	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/harness"
)

type State struct {
	OpenTurn    string             `json:"open_turn,omitempty"` // prompt_id of the running turn
	LastEventAt time.Time          `json:"last_event_at"`
	LastPre     map[string]preTool `json:"last_pre,omitempty"` // per agent, "" is main
	Pending     []pending          `json:"pending,omitempty"`  // unanswered permission dialogs
}

type preTool struct {
	ToolUseID string `json:"tool_use_id"`
	Tool      string `json:"tool"`
}

type pending struct {
	ToolUseID string    `json:"tool_use_id"`
	Tool      string    `json:"tool"`
	AgentID   string    `json:"agent_id,omitempty"`
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
		st = m.denyPending(st, func(pending) bool { return true })
		st = m.closeTurn(st, "closed")
		m.emit(event.SessionEnd, in.Now, event.SessionEndData{Reason: "exit"})

	case "UserPromptSubmit":
		// no interrupt or deny hook exists, a new prompt means the open turn and dialogs were cut
		st = m.denyPending(st, isMain)
		st = m.closeTurn(st, "interrupted")
		m.out = append(m.out, m.emitter().Prompt(in.Now, h.Prompt, false))
		m.emit(event.AgentTurnStart, in.Now, event.AgentTurnStartData{TurnID: h.PromptID})
		st.OpenTurn = h.PromptID

	case "Stop", "StopFailure":
		reason := "done"
		if h.Event == "StopFailure" {
			reason = "error"
		}
		st = m.denyPending(st, isMain) // subagent dialogs outlive the main turn
		m.emit(event.AgentTurnEnd, in.Now, event.AgentTurnEndData{TurnID: h.PromptID, Reason: reason})
		st.OpenTurn = ""

	case "PreToolUse":
		// PermissionRequest has no tool_use_id, take it from here
		if st.LastPre == nil {
			st.LastPre = map[string]preTool{}
		}
		st.LastPre[h.AgentID] = preTool{ToolUseID: h.ToolUseID, Tool: h.ToolName}

	case "PermissionRequest":
		p := pending{Tool: h.ToolName, AgentID: h.AgentID, At: in.Now}
		if pre, ok := st.LastPre[h.AgentID]; ok && pre.Tool == h.ToolName {
			p.ToolUseID = pre.ToolUseID
		}
		st.Pending = append(st.Pending, p)

	case "PostToolUse":
		// a tool that ran after a dialog was allowed
		started := in.Now.Add(-time.Duration(h.DurationMs) * time.Millisecond)
		for i, p := range st.Pending {
			if p.ToolUseID != "" && p.ToolUseID == h.ToolUseID {
				m.emit(event.Approval, started, event.ApprovalData{
					Allow:     true,
					LatencyMs: max(0, started.Sub(p.At).Milliseconds()),
					Tool:      category(p.Tool),
				})
				st.Pending = append(st.Pending[:i], st.Pending[i+1:]...)
				break
			}
		}
		// ts is the end, duration gives the start
		m.emit(event.ToolCall, in.Now, event.ToolCallData{
			Tool:       category(h.ToolName),
			DurationMs: h.DurationMs,
		})
	}

	st.LastEventAt = in.Now
	return st, m.out
}

func isMain(p pending) bool { return p.AgentID == "" }

func (m *mapper) denyPending(st State, match func(pending) bool) State {
	var keep []pending
	for _, p := range st.Pending {
		if !match(p) {
			keep = append(keep, p)
			continue
		}
		m.emit(event.Approval, m.in.Now, event.ApprovalData{
			Allow:     false,
			LatencyMs: max(0, m.in.Now.Sub(p.At).Milliseconds()),
			Tool:      category(p.Tool),
		})
	}
	st.Pending = keep
	return st
}

// real end is unknown, use the last event seen
func (m *mapper) closeTurn(st State, reason string) State {
	if st.OpenTurn == "" {
		return st
	}
	m.emit(event.AgentTurnEnd, st.LastEventAt, event.AgentTurnEndData{TurnID: st.OpenTurn, Reason: reason})
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
	case "Bash", "BashOutput", "KillShell":
		return event.ToolShell
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		return event.ToolEdit
	case "Read", "Glob", "Grep", "LS":
		return event.ToolRead
	case "WebFetch", "WebSearch":
		return event.ToolWeb
	case "Agent", "Task":
		return event.ToolAgent
	}
	return event.ToolOther
}
