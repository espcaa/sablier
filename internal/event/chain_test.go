package event

import (
	"testing"
	"time"
)

func chainEvent() Event {
	e, _ := New(PromptSent, time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC), PromptSentData{Hash: "abc"})
	e.Seq, e.SessionID, e.ProjectID, e.Harness, e.Source = 7, "run:x", "p", "codex", SourceAdapter
	return e
}

func TestChainHash(t *testing.T) {
	e := chainEvent()
	h, _ := ChainHash("", e)

	// fixed, the client and server must agree on these bytes forever
	if h != "1eace0bea7e026afcee8399abccc48a8e81f08a418fd29cf82f91b8e777b99dd" {
		t.Fatalf("hash changed: %s", h)
	}

	// spacing in data doesn't matter, the server gets it compacted
	spaced := e
	spaced.Data = []byte(`{ "pasted": false, "compose_ms": 0, "queued": false, "approval_like": false, "hash": "abc" }`)
	if got, _ := ChainHash("", spaced); got != h {
		t.Error("data spacing changed the hash")
	}

	changes := map[string]func(*Event){
		"seq":     func(e *Event) { e.Seq++ },
		"ts":      func(e *Event) { e.TS = e.TS.Add(time.Millisecond) },
		"kind":    func(e *Event) { e.Kind = Focus },
		"session": func(e *Event) { e.SessionID = "run:y" },
		"project": func(e *Event) { e.ProjectID = "q" },
		"harness": func(e *Event) { e.Harness = "opencode" },
		"source":  func(e *Event) { e.Source = SourceLauncher },
		"data":    func(e *Event) { e.Data = []byte(`{"hash":"abd"}`) },
	}
	for name, change := range changes {
		c := e
		change(&c)
		if got, _ := ChainHash("", c); got == h {
			t.Errorf("changing %s kept the hash", name)
		}
	}
	if got, _ := ChainHash("prev", e); got == h {
		t.Error("prev doesn't change the hash")
	}
}
