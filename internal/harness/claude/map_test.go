package claude

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/prompt"
)

var t0 = time.Date(2026, 11, 19, 17, 0, 0, 0, time.UTC)

// one hook, `at` seconds after t0
type step struct {
	at   float64
	hook Hook
}

func run(t *testing.T, steps ...step) []event.Event {
	t.Helper()
	var st State
	var all []event.Event
	for _, s := range steps {
		if s.hook.SessionID == "" {
			s.hook.SessionID = "s1"
		}
		var out []event.Event
		st, out = Map(st, Input{
			Hook:      s.hook,
			Now:       t0.Add(time.Duration(s.at * float64(time.Second))),
			Salt:      []byte("test-salt"),
			ProjectID: "p1",
		})
		all = append(all, out...)
	}
	return all
}

func describe(t *testing.T, events []event.Event) string {
	t.Helper()
	var b strings.Builder
	for _, e := range events {
		p, err := e.Payload()
		if err != nil {
			t.Fatal(err)
		}
		// hashes are tested in the prompt package
		if ps, ok := p.(*event.PromptSentData); ok && len(ps.Hash) == 64 {
			ps.Hash = "<hash>"
		}
		fmt.Fprintf(&b, "%g %s %+v\n", e.TS.Sub(t0).Seconds(), e.Kind, p)
	}
	return b.String()
}

func expect(t *testing.T, got []event.Event, want string) {
	t.Helper()
	g := strings.TrimSpace(describe(t, got))
	w := strings.TrimSpace(strings.ReplaceAll(want, "\t", ""))
	if g != w {
		t.Errorf("events differ\n--- got\n%s\n--- want\n%s", g, w)
	}
}

func TestSimpleTurn(t *testing.T) {
	got := run(t,
		step{0, Hook{Event: "SessionStart", Source: "startup", Model: "opus"}},
		step{1, Hook{Event: "UserPromptSubmit", PromptID: "p1", Prompt: "fix the test"}},
		step{3, Hook{Event: "PreToolUse", PromptID: "p1", ToolName: "Bash", ToolUseID: "t1"}},
		step{4, Hook{Event: "PostToolUse", PromptID: "p1", ToolName: "Bash", ToolUseID: "t1", DurationMs: 500}},
		step{6, Hook{Event: "Stop", PromptID: "p1"}},
		step{9, Hook{Event: "SessionEnd"}},
	)
	expect(t, got, `
		0 session_start &{HarnessVersion: Model:opus HarnessSession:s1}
		1 prompt_sent &{Pasted:false ComposeMs:0 Queued:false ApprovalLike:false Hash:<hash>}
		1 agent_turn_start &{TurnID:p1}
		4 tool_call &{Tool:shell DurationMs:500}
		6 agent_turn_end &{TurnID:p1 Reason:done}
		9 session_end &{Reason:exit}
	`)
}

func TestEnvelope(t *testing.T) {
	got := run(t, step{0, Hook{Event: "UserPromptSubmit", SessionID: "abc", PromptID: "p1"}})
	for _, e := range got {
		if e.SessionID != "abc" || e.ProjectID != "p1" || e.Harness != Harness || e.Source != event.SourceAdapter {
			t.Errorf("bad envelope: %+v", e)
		}
	}
}

func TestStopFailureAndCompact(t *testing.T) {
	got := run(t,
		step{0, Hook{Event: "SessionStart", Source: "compact"}},
		step{1, Hook{Event: "UserPromptSubmit", PromptID: "p1", Prompt: "yes"}},
		step{2, Hook{Event: "StopFailure", PromptID: "p1"}},
	)
	expect(t, got, `
		1 prompt_sent &{Pasted:false ComposeMs:0 Queued:false ApprovalLike:true Hash:<hash>}
		1 agent_turn_start &{TurnID:p1}
		2 agent_turn_end &{TurnID:p1 Reason:error}
	`)
}

func TestPromptTextNeverStored(t *testing.T) {
	secret := "my secret api key is hunter2"
	for _, e := range run(t, step{0, Hook{Event: "UserPromptSubmit", PromptID: "p1", Prompt: secret}}) {
		if strings.Contains(string(e.Data), "hunter2") {
			t.Fatalf("prompt text leaked into %s: %s", e.Kind, e.Data)
		}
	}
}

func TestCategory(t *testing.T) {
	cases := map[string]event.ToolCategory{
		"Bash": event.ToolShell, "Edit": event.ToolEdit, "Write": event.ToolEdit,
		"Read": event.ToolRead, "Grep": event.ToolRead, "WebFetch": event.ToolWeb,
		"Agent": event.ToolAgent, "mcp__figma__x": event.ToolOther, "NewTool": event.ToolOther,
	}
	for tool, want := range cases {
		if got := category(tool); got != want {
			t.Errorf("category(%q) = %s, want %s", tool, got, want)
		}
	}
}

// real session timings
func TestApprovalAllowed(t *testing.T) {
	got := run(t,
		step{40.700, Hook{Event: "UserPromptSubmit", PromptID: "p1", Prompt: "run it"}},
		step{43.289, Hook{Event: "PreToolUse", PromptID: "p1", ToolName: "Bash", ToolUseID: "t1"}},
		step{43.305, Hook{Event: "PermissionRequest", PromptID: "p1", ToolName: "Bash"}},
		step{45.677, Hook{Event: "PostToolUse", PromptID: "p1", ToolName: "Bash", ToolUseID: "t1", DurationMs: 438}},
		step{48.080, Hook{Event: "Stop", PromptID: "p1"}},
	)
	expect(t, got, `
		40.7 prompt_sent &{Pasted:false ComposeMs:0 Queued:false ApprovalLike:false Hash:<hash>}
		40.7 agent_turn_start &{TurnID:p1}
		45.239 approval &{Allow:true LatencyMs:1934 Tool:shell}
		45.677 tool_call &{Tool:shell DurationMs:438}
		48.08 agent_turn_end &{TurnID:p1 Reason:done}
	`)
}

// real session: p1 interrupted, p2 typed 3s later
func TestInterrupt(t *testing.T) {
	got := run(t,
		step{0, Hook{Event: "UserPromptSubmit", PromptID: "p1", Prompt: "do x"}},
		step{3, Hook{Event: "UserPromptSubmit", PromptID: "p2", Prompt: "do y"}},
		step{5, Hook{Event: "Stop", PromptID: "p2"}},
	)
	expect(t, got, `
		0 prompt_sent &{Pasted:false ComposeMs:0 Queued:false ApprovalLike:false Hash:<hash>}
		0 agent_turn_start &{TurnID:p1}
		0 agent_turn_end &{TurnID:p1 Reason:interrupted}
		3 prompt_sent &{Pasted:false ComposeMs:0 Queued:false ApprovalLike:false Hash:<hash>}
		3 agent_turn_start &{TurnID:p2}
		5 agent_turn_end &{TurnID:p2 Reason:done}
	`)
}

// real session: write denied, new prompt 14s later, no stop
func TestDenyWithFeedback(t *testing.T) {
	got := run(t,
		step{0, Hook{Event: "UserPromptSubmit", PromptID: "p1", Prompt: "write it"}},
		step{65, Hook{Event: "PreToolUse", PromptID: "p1", ToolName: "Write", ToolUseID: "t1"}},
		step{65, Hook{Event: "PermissionRequest", PromptID: "p1", ToolName: "Write"}},
		step{79, Hook{Event: "UserPromptSubmit", PromptID: "p2", Prompt: "no, put it in docs/"}},
	)
	expect(t, got, `
		0 prompt_sent &{Pasted:false ComposeMs:0 Queued:false ApprovalLike:false Hash:<hash>}
		0 agent_turn_start &{TurnID:p1}
		79 approval &{Allow:false LatencyMs:14000 Tool:edit}
		65 agent_turn_end &{TurnID:p1 Reason:interrupted}
		79 prompt_sent &{Pasted:false ComposeMs:0 Queued:false ApprovalLike:false Hash:<hash>}
		79 agent_turn_start &{TurnID:p2}
	`)
}

// real session: main turn stopped with a subagent dialog open
func TestSubagentDialogOutlivesStop(t *testing.T) {
	got := run(t,
		step{0, Hook{Event: "UserPromptSubmit", PromptID: "p1", Prompt: "look into it"}},
		step{6.738, Hook{Event: "PreToolUse", PromptID: "p1", AgentID: "a1", ToolName: "Read", ToolUseID: "t1"}},
		step{6.759, Hook{Event: "PermissionRequest", PromptID: "p1", AgentID: "a1", ToolName: "Read"}},
		step{6.894, Hook{Event: "Stop", PromptID: "p1"}},
		step{9.229, Hook{Event: "PostToolUse", PromptID: "p1", AgentID: "a1", ToolName: "Read", ToolUseID: "t1", DurationMs: 23}},
	)
	expect(t, got, `
		0 prompt_sent &{Pasted:false ComposeMs:0 Queued:false ApprovalLike:false Hash:<hash>}
		0 agent_turn_start &{TurnID:p1}
		6.894 agent_turn_end &{TurnID:p1 Reason:done}
		9.206 approval &{Allow:true LatencyMs:2447 Tool:read}
		9.229 tool_call &{Tool:read DurationMs:23}
	`)
}

func TestSessionEndClosesEverything(t *testing.T) {
	got := run(t,
		step{0, Hook{Event: "UserPromptSubmit", PromptID: "p1", Prompt: "go"}},
		step{2, Hook{Event: "PreToolUse", PromptID: "p1", ToolName: "Bash", ToolUseID: "t1"}},
		step{2, Hook{Event: "PermissionRequest", PromptID: "p1", ToolName: "Bash"}},
		step{30, Hook{Event: "SessionEnd"}},
	)
	expect(t, got, `
		0 prompt_sent &{Pasted:false ComposeMs:0 Queued:false ApprovalLike:true Hash:<hash>}
		0 agent_turn_start &{TurnID:p1}
		30 approval &{Allow:false LatencyMs:28000 Tool:shell}
		2 agent_turn_end &{TurnID:p1 Reason:closed}
		30 session_end &{Reason:exit}
	`)
}

func TestLaunchSession(t *testing.T) {
	_, out := Map(State{}, Input{
		Hook:    Hook{Event: "SessionStart", SessionID: "claude-1", Source: "startup"},
		Now:     t0,
		Session: "run:r1",
	})
	if len(out) != 1 || out[0].SessionID != "run:r1" {
		t.Fatalf("got %+v", out)
	}
	p, _ := out[0].Payload()
	if p.(*event.SessionStartData).HarnessSession != "claude-1" {
		t.Fatalf("claude's session id lost: %+v", p)
	}
}

func TestPromptHashFromStoredEvent(t *testing.T) {
	got := run(t, step{1.0005, Hook{Event: "UserPromptSubmit", PromptID: "p1", Prompt: "Fix the test"}})
	e := got[0]
	p, _ := e.Payload()
	stored := time.UnixMilli(e.TS.UnixMilli())
	if want := prompt.Hash(prompt.Key([]byte("test-salt"), e.SessionID, stored), "fix the test"); p.(*event.PromptSentData).Hash != want {
		t.Fatal("prompt hash can't be rebuilt from the stored event")
	}
	if e.Prompt != "Fix the test" {
		t.Fatal("prompt text not kept for the local db")
	}
}
