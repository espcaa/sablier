package event

import (
	"encoding/json"
	"fmt"
	"time"
)

type Kind string

const (
	SessionStart   Kind = "session_start"
	SessionEnd     Kind = "session_end"
	InputTick      Kind = "input_tick"
	Focus          Kind = "focus"
	PromptSent     Kind = "prompt_sent"
	Approval       Kind = "approval"
	Interrupt      Kind = "interrupt"
	AgentTurnStart Kind = "agent_turn_start"
	AgentTurnEnd   Kind = "agent_turn_end"
	ToolCall       Kind = "tool_call"
)

type Source string

const (
	SourceLauncher  Source = "launcher"
	SourceAdapter   Source = "adapter"
	SourceExtension Source = "extension"
)

type Event struct {
	Seq       int64           `json:"seq"` // per-install counter, from store
	TS        time.Time       `json:"ts"`
	Kind      Kind            `json:"kind"`
	SessionID string          `json:"session_id"`
	ProjectID string          `json:"project_id"`
	Harness   string          `json:"harness"` // "claude-code", "codex", "unknown", ...
	Source    Source          `json:"source"`
	Data      json.RawMessage `json:"data,omitempty"`
	PrevHash  string          `json:"prev_hash,omitempty"` // empty on events from before the chain
	Hash      string          `json:"hash,omitempty"`
}

// payloads, one per kind

type SessionStartData struct {
	HarnessVersion string `json:"harness_version,omitempty"`
	Model          string `json:"model,omitempty"`           // stats only
	HarnessSession string `json:"harness_session,omitempty"` // the harness's own id, to find its transcript
}

type SessionEndData struct {
	Reason string `json:"reason"` // "exit", "crash", "closed"
}

// InputTickData is only emitted for non-empty windows
type InputTickData struct {
	WindowMs int64 `json:"window_ms"`
	Keys     int   `json:"keys"`
	Mouse    int   `json:"mouse"` // scrolls and clicks
	Pasted   bool  `json:"pasted"`
}

type FocusData struct {
	In bool `json:"in"`
}

type PromptSentData struct {
	Pasted       bool   `json:"pasted"`
	ComposeMs    int64  `json:"compose_ms"`
	Queued       bool   `json:"queued"`        // sent while the agent was busy
	ApprovalLike bool   `json:"approval_like"` // whole prompt matched the yes/continue list
	Hash         string `json:"hash"`          // commitment to the text, see prompt.Hash
}

type ApprovalData struct {
	Allow     bool         `json:"allow"`
	LatencyMs int64        `json:"latency_ms"`
	Tool      ToolCategory `json:"tool"`
}

type InterruptData struct{}

type AgentTurnStartData struct {
	TurnID string `json:"turn_id"`
}

type AgentTurnEndData struct {
	TurnID string `json:"turn_id"`
	Reason string `json:"reason,omitempty"` // "done", "interrupted", "error"
}

type ToolCallData struct {
	Tool       ToolCategory `json:"tool"`
	DurationMs int64        `json:"duration_ms"`
}

type ToolCategory string

const (
	ToolShell ToolCategory = "shell"
	ToolEdit  ToolCategory = "edit"
	ToolRead  ToolCategory = "read"
	ToolWeb   ToolCategory = "web"
	ToolAgent ToolCategory = "agent" // subagents
	ToolOther ToolCategory = "other"
)

var payloads = map[Kind]func() any{
	SessionStart:   func() any { return &SessionStartData{} },
	SessionEnd:     func() any { return &SessionEndData{} },
	InputTick:      func() any { return &InputTickData{} },
	Focus:          func() any { return &FocusData{} },
	PromptSent:     func() any { return &PromptSentData{} },
	Approval:       func() any { return &ApprovalData{} },
	Interrupt:      func() any { return &InterruptData{} },
	AgentTurnStart: func() any { return &AgentTurnStartData{} },
	AgentTurnEnd:   func() any { return &AgentTurnEndData{} },
	ToolCall:       func() any { return &ToolCallData{} },
}

func (k Kind) Valid() bool {
	_, ok := payloads[k]
	return ok
}

func New(kind Kind, ts time.Time, data any) (Event, error) {
	if !kind.Valid() {
		return Event{}, fmt.Errorf("unknown event kind %q", kind)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, err
	}
	return Event{Kind: kind, TS: ts.UTC(), Data: raw}, nil
}

func (e Event) Payload() (any, error) {
	mk, ok := payloads[e.Kind]
	if !ok {
		return nil, fmt.Errorf("unknown event kind %q", e.Kind)
	}
	p := mk()
	if len(e.Data) > 0 {
		if err := json.Unmarshal(e.Data, p); err != nil {
			return nil, fmt.Errorf("decode %s: %w", e.Kind, err)
		}
	}
	return p, nil
}
