package opencode

import (
	"time"

	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/prompt"
)

const Harness = "opencode"

type Event struct {
	Type     string `json:"type"`
	Created  int64  `json:"created"` // unix ms
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
	Data struct {
		SessionID string `json:"sessionID"`
		ParentID  string `json:"parentID"` // set on subagent sessions
		ID        string `json:"id"`       // tool call or permission request
		Name      string `json:"name"`     // tool name
		InboxID   string `json:"inboxID"`
		Item      struct {
			Type    string `json:"type"`
			Payload struct {
				Text string `json:"text"`
			} `json:"payload"`
		} `json:"item"`
		Action    string `json:"action"` // permission's tool
		RequestID string `json:"requestID"`
		Reply     string `json:"reply"` // once, always, reject
	} `json:"data"`
}

// keeps state between events, used by one goroutine
type Mapper struct {
	Salt    []byte
	Session string                  // sablier's run session
	Project func(dir string) string // project id for a folder
	Default string                  // project when an event has no folder

	sessions map[string]*session
	projects map[string]string // by folder, most events carry one
	parents  map[string]string // subagent session to its parent
	tools    map[string]tool
	asks     map[string]ask
}

type session struct {
	project string
	turn    string // inbox id of the prompt that started the running turn
	prompt  string // last prompt's inbox id
}

type tool struct {
	name string
	at   time.Time
}

type ask struct {
	tool    string
	session string
	at      time.Time
}

func (m *Mapper) Map(e Event) []event.Event {
	d := e.Data
	if d.SessionID == "" {
		return nil
	}
	if m.sessions == nil {
		m.sessions, m.projects, m.parents = map[string]*session{}, map[string]string{}, map[string]string{}
		m.tools, m.asks = map[string]tool{}, map[string]ask{}
	}
	at := time.UnixMilli(e.Created)
	var out []event.Event
	emit := func(s *session, kind event.Kind, ts time.Time, data any) {
		out = append(out, m.event(s, kind, ts, data))
	}

	if e.Type == "session.created" && d.ParentID != "" {
		m.parents[d.SessionID] = d.ParentID
	}

	// subagents ask too, and the user answers
	switch e.Type {
	case "permission.v2.asked":
		sid := d.SessionID
		if p, ok := m.parents[sid]; ok {
			sid = p
		}
		m.asks[d.ID] = ask{tool: d.Action, session: sid, at: at}
		return nil
	case "permission.v2.replied":
		if a, ok := m.asks[d.RequestID]; ok {
			delete(m.asks, d.RequestID)
			emit(m.session(a.session, e, &out), event.Approval, at, event.ApprovalData{
				Allow:     d.Reply != "reject",
				LatencyMs: max(0, at.Sub(a.at).Milliseconds()),
				Tool:      category(a.tool),
			})
		}
		return out
	}

	// a subagent's work is inside the parent's task tool call
	if _, ok := m.parents[d.SessionID]; ok {
		return nil
	}
	s := m.session(d.SessionID, e, &out)

	switch e.Type {
	case "session.inbox.enqueued":
		if d.Item.Type != "user" {
			break
		}
		emit(s, event.PromptSent, at, event.PromptSentData{
			Hash:         prompt.Hash(prompt.Key(m.Salt, m.Session, at), d.Item.Payload.Text),
			ApprovalLike: prompt.ApprovalLike(d.Item.Payload.Text),
			Queued:       s.turn != "",
		})
		s.prompt = d.InboxID

	case "session.execution.started":
		if s.turn == "" {
			s.turn = s.prompt
			if s.turn == "" {
				s.turn = d.SessionID
			}
			emit(s, event.AgentTurnStart, at, event.AgentTurnStartData{TurnID: s.turn})
		}

	case "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted":
		reason := map[string]string{
			"session.execution.succeeded":   "done",
			"session.execution.failed":      "error",
			"session.execution.interrupted": "interrupted",
		}[e.Type]
		if reason == "interrupted" {
			emit(s, event.Interrupt, at, event.InterruptData{})
		}
		if s.turn != "" {
			emit(s, event.AgentTurnEnd, at, event.AgentTurnEndData{TurnID: s.turn, Reason: reason})
			s.turn = ""
		}

	case "session.tool.input.started":
		m.tools[d.ID] = tool{name: d.Name}

	case "session.tool.called":
		if t, ok := m.tools[d.ID]; ok {
			t.at = at
			m.tools[d.ID] = t
		}

	case "session.tool.success", "session.tool.failed":
		if t, ok := m.tools[d.ID]; ok {
			delete(m.tools, d.ID)
			if !t.at.IsZero() {
				emit(s, event.ToolCall, at, event.ToolCallData{
					Tool:       category(t.name),
					DurationMs: at.Sub(t.at).Milliseconds(),
				})
			}
		}
	}
	return out
}

// running turns are cut and every session ends, when the launch ends
func (m *Mapper) Close(at time.Time) []event.Event {
	var out []event.Event
	for _, s := range m.sessions {
		if s.turn != "" {
			out = append(out, m.event(s, event.AgentTurnEnd, at, event.AgentTurnEndData{TurnID: s.turn, Reason: "closed"}))
			s.turn = ""
		}
		out = append(out, m.event(s, event.SessionEnd, at, event.SessionEndData{Reason: "exit"}))
	}
	m.sessions = nil
	return out
}

// first event of a session starts it
func (m *Mapper) session(id string, e Event, out *[]event.Event) *session {
	s, ok := m.sessions[id]
	if !ok {
		s = &session{project: m.Default}
		m.sessions[id] = s
	}
	if dir := e.Location.Directory; dir != "" && m.Project != nil {
		p, ok := m.projects[dir]
		if !ok {
			p = m.Project(dir)
			m.projects[dir] = p
		}
		if p != "" {
			s.project = p
		}
	}
	if !ok {
		*out = append(*out, m.event(s, event.SessionStart, time.UnixMilli(e.Created), event.SessionStartData{HarnessSession: id}))
	}
	return s
}

func (m *Mapper) event(s *session, kind event.Kind, ts time.Time, data any) event.Event {
	e, err := event.New(kind, ts, data)
	if err != nil {
		panic(err) // unknown kind, a bug here
	}
	e.SessionID = m.Session
	e.ProjectID = s.project
	e.Harness = Harness
	e.Source = event.SourceAdapter
	return e
}

func category(tool string) event.ToolCategory {
	switch tool {
	case "shell", "bash":
		return event.ToolShell
	case "edit", "write", "patch", "apply_patch", "multiedit":
		return event.ToolEdit
	case "read", "glob", "grep", "list", "codesearch":
		return event.ToolRead
	case "webfetch", "websearch":
		return event.ToolWeb
	case "task":
		return event.ToolAgent
	}
	return event.ToolOther
}
