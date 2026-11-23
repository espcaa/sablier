package credit

import (
	"testing"
	"time"

	"github.com/espcaa/sablier/internal/event"
)

var t0 = time.Date(2026, 11, 19, 17, 0, 0, 0, time.UTC)

func at(sec float64) time.Time { return t0.Add(time.Duration(sec * float64(time.Second))) }
func mins(m float64) float64   { return m * 60 }

// builds events for one session in one project
type sess struct {
	id, project string
	events      []event.Event
}

func (s *sess) add(kind event.Kind, sec float64, data any) *sess {
	e, err := event.New(kind, at(sec), data)
	if err != nil {
		panic(err)
	}
	e.SessionID, e.ProjectID = s.id, s.project
	s.events = append(s.events, e)
	return s
}

func (s *sess) start(sec float64) *sess {
	return s.add(event.SessionStart, sec, event.SessionStartData{})
}
func (s *sess) prompt(sec float64) *sess {
	return s.add(event.PromptSent, sec, event.PromptSentData{})
}
func (s *sess) turn(id string, from, to float64) *sess {
	s.add(event.AgentTurnStart, from, event.AgentTurnStartData{TurnID: id})
	return s.add(event.AgentTurnEnd, to, event.AgentTurnEndData{TurnID: id})
}
func (s *sess) approve(sec, latency float64) *sess {
	return s.add(event.Approval, sec, event.ApprovalData{Allow: true, LatencyMs: int64(latency * 1000)})
}

func check(t *testing.T, got, want time.Duration) {
	t.Helper()
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// agent works 20 min, you read 3, prompt again: 5 after + 3 reading
func TestReadingAfterLongTurn(t *testing.T) {
	s := (&sess{id: "s", project: "p"}).start(0).
		prompt(0).turn("t1", 0, mins(20)).
		prompt(mins(23))
	rep := Compute(s.events, Default, at(mins(23)))
	check(t, rep.Credited, 8*time.Minute)
	check(t, rep.AgentBusy, 20*time.Minute)
}

// prompt, walk away 3 hours: 5 after + 5 before the next prompt
func TestWalkAway(t *testing.T) {
	s := (&sess{id: "s", project: "p"}).start(0).
		prompt(0).turn("t1", 0, mins(10)).
		prompt(mins(180))
	check(t, Compute(s.events, Default, at(mins(180))).Credited, 10*time.Minute)
}

// dialog open for 1 min while the agent is mid-turn counts as reading
func TestApprovalDialog(t *testing.T) {
	s := (&sess{id: "s", project: "p"}).start(0).
		prompt(0).turn("t1", 0, mins(17)).
		approve(mins(12), 60)
	// [0,5] + [11,12] + [12,17]
	check(t, Compute(s.events, Default, at(mins(17))).Credited, 11*time.Minute)
}

// the first prompt can only reach back to session_start
func TestFirstPromptBoundedBySessionStart(t *testing.T) {
	s := (&sess{id: "s", project: "p"}).start(0).prompt(mins(2))
	check(t, Compute(s.events, Default, at(mins(2))).Credited, 2*time.Minute)

	// no session_start seen: don't guess
	s = (&sess{id: "s", project: "p"}).prompt(mins(2))
	check(t, Compute(s.events, Default, at(mins(2))).Credited, 0)
}

// two sessions at once: counted once, latest action's project wins
func TestParallelSessions(t *testing.T) {
	a := (&sess{id: "a", project: "pa"}).prompt(0)
	b := (&sess{id: "b", project: "pb"}).prompt(mins(2))
	rep := Compute(append(a.events, b.events...), Default, at(mins(30)))

	check(t, rep.Credited, 7*time.Minute) // [0,5] u [2,7]
	check(t, rep.ByProject["pa"], 2*time.Minute)
	check(t, rep.ByProject["pb"], 5*time.Minute)
}

func TestObservedInputWinsOverLaterPrompt(t *testing.T) {
	a := (&sess{id: "a", project: "pa"}).tick(60, 10, 5, 0)
	b := (&sess{id: "b", project: "pb"}).start(0).prompt(mins(4))
	rep := Compute(append(a.events, b.events...), Default, at(mins(4)))

	check(t, rep.Credited, 4*time.Minute)
	check(t, rep.ByProject["pa"], 10*time.Second)
	check(t, rep.ByProject["pb"], 3*time.Minute+50*time.Second)
}

// a running session isn't credited into the future
func TestClippedAtUntil(t *testing.T) {
	s := (&sess{id: "s", project: "p"}).prompt(0)
	check(t, Compute(s.events, Default, at(mins(1))).Credited, time.Minute)
}

// a turn that never ended counts as busy until the last event seen
func TestOpenTurn(t *testing.T) {
	s := (&sess{id: "s", project: "p"}).start(0).prompt(0)
	s.add(event.AgentTurnStart, 0, event.AgentTurnStartData{TurnID: "t1"})
	s.add(event.ToolCall, mins(8), event.ToolCallData{})
	check(t, Compute(s.events, Default, at(mins(30))).AgentBusy, 8*time.Minute)
}

func TestCountsAndSpan(t *testing.T) {
	s := (&sess{id: "s", project: "p"}).start(0).prompt(10).approve(20, 1).prompt(30)
	rep := Compute(s.events, Default, at(mins(60)))
	if rep.Actions != 3 || !rep.First.Equal(at(0)) || !rep.Last.Equal(at(30)) {
		t.Errorf("got %+v", rep)
	}
}

func TestEmpty(t *testing.T) {
	if rep := Compute(nil, Default, t0); rep.Credited != 0 || rep.Actions != 0 {
		t.Errorf("got %+v", rep)
	}
}

func (s *sess) tick(sec, window float64, keys, mouse int) *sess {
	return s.add(event.InputTick, sec, event.InputTickData{WindowMs: int64(window * 1000), Keys: keys, Mouse: mouse})
}
func (s *sess) focus(sec float64, in bool) *sess {
	return s.add(event.Focus, sec, event.FocusData{In: in})
}

// one tick: its window plus 2 min
func TestTyping(t *testing.T) {
	s := (&sess{id: "s", project: "p"}).tick(60, 10, 5, 0)
	check(t, Compute(s.events, Default, at(mins(30))).Credited, 2*time.Minute+10*time.Second)
}

func TestSteadyTyping(t *testing.T) {
	s := &sess{id: "s", project: "p"}
	for sec := 10.0; sec <= mins(5); sec += 10 {
		s.tick(sec, 10, 3, 0)
	}
	check(t, Compute(s.events, Default, at(mins(30))).Credited, 7*time.Minute)
}

func TestScrollingCounts(t *testing.T) {
	s := (&sess{id: "s", project: "p"}).tick(10, 10, 0, 4)
	check(t, Compute(s.events, Default, at(mins(30))).Credited, 2*time.Minute+10*time.Second)
}

func TestEmptyTickIgnored(t *testing.T) {
	s := (&sess{id: "s", project: "p"}).tick(10, 10, 0, 0)
	check(t, Compute(s.events, Default, at(mins(30))).Credited, 0)
}

// gone 90s to check docs, back before 2 min: nothing cut
func TestShortTrip(t *testing.T) {
	s := (&sess{id: "s", project: "p"}).start(0).prompt(0).
		focus(mins(1), false).focus(mins(2.5), true)
	check(t, Compute(s.events, Default, at(mins(5))).Credited, 5*time.Minute)
}

// gone 4 min: credit stops when you left, the rest of the prompt's window still counts after you're back
func TestLongTrip(t *testing.T) {
	s := (&sess{id: "s", project: "p"}).start(0).prompt(0).
		focus(mins(1), false).focus(mins(4.5), true)
	// [0,1] + [4.5,5]
	check(t, Compute(s.events, Default, at(mins(30))).Credited, 90*time.Second)
}

// still away: cut once the trip is past 2 min, not before
func TestOpenTrip(t *testing.T) {
	s := (&sess{id: "s", project: "p"}).start(0).prompt(0).focus(mins(1), false)
	check(t, Compute(s.events, Default, at(mins(2.5))).Credited, 150*time.Second)
	check(t, Compute(s.events, Default, at(mins(10))).Credited, time.Minute)
}

// a typing burst after coming back counts normally
func TestTypingAfterLongTrip(t *testing.T) {
	s := (&sess{id: "s", project: "p"}).
		tick(10, 10, 5, 0).
		focus(20, false).focus(mins(10), true).
		tick(mins(10)+10, 10, 5, 0)
	// [0,20] before leaving + [10m, 10m10s+2m] after
	check(t, Compute(s.events, Default, at(mins(30))).Credited, 20*time.Second+130*time.Second)
}

func TestFormat(t *testing.T) {
	cases := map[time.Duration]string{
		0: "0m", 29 * time.Second: "0m", 27*time.Minute + 51*time.Second: "28m",
		time.Hour: "1h00m", 65 * time.Minute: "1h05m",
	}
	for d, want := range cases {
		if got := Format(d); got != want {
			t.Errorf("Format(%s) = %s, want %s", d, got, want)
		}
	}
}
