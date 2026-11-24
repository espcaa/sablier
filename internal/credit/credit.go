// credited time from raw events, bump Version when a rule changes
package credit

import (
	"fmt"
	"sort"
	"time"

	"github.com/espcaa/sablier/internal/event"
)

const Version = 3

type Rules struct {
	AgentCap time.Duration
	InputCap time.Duration
}

var Default = Rules{AgentCap: 5 * time.Minute, InputCap: 2 * time.Minute}

type Report struct {
	Credited  time.Duration
	ByProject map[string]time.Duration
	BySession map[string]time.Duration
	AgentBusy time.Duration
	Actions   int
	First     time.Time // wall clock span
	Last      time.Time
}

type span struct{ start, end time.Time }

// a credited window, anchor is action that started it
type window struct {
	span
	project  string
	session  string
	anchor   time.Time
	observed bool
}

// rules, per session:
//  1. prompts and approvals earn up to AgentCap after them, and before them once the agent was done
//  2. typing or scrolling earns its tick window plus up to InputCap after it
//  3. leaving the terminal for longer than InputCap cuts credit from the moment you left
//  4. overlapping time counts once, preferring observed input, then the most recent action
//
// nothing is counted after until.
func Compute(events []event.Event, r Rules, until time.Time) Report {
	return ComputeBetween(events, r, time.Time{}, until)
}

// from and until bound the credited windows.
func ComputeBetween(events []event.Event, r Rules, from, until time.Time) Report {
	rep := Report{ByProject: map[string]time.Duration{}, BySession: map[string]time.Duration{}}
	if len(events) == 0 {
		return rep
	}

	sorted := append([]event.Event(nil), events...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].TS.Before(sorted[j].TS) })
	rep.First, rep.Last = sorted[0].TS, sorted[len(sorted)-1].TS

	sessions := map[string][]event.Event{}
	var order []string
	for _, e := range sorted {
		if _, ok := sessions[e.SessionID]; !ok {
			order = append(order, e.SessionID)
		}
		sessions[e.SessionID] = append(sessions[e.SessionID], e)
	}

	var windows []window
	var busy []span
	for _, id := range order {
		w, b, n := session(sessions[id], r, until)
		windows = append(windows, w...)
		busy = append(busy, b...)
		rep.Actions += n
	}

	for i := range windows {
		windows[i].span = clip(windows[i].span, from, until)
	}
	for i := range busy {
		busy[i] = clip(busy[i], from, until)
	}

	rep.Credited = attribute(windows, func(w window, d time.Duration) {
		rep.ByProject[w.project] += d
		rep.BySession[w.session] += d
	})
	rep.AgentBusy = union(busy)
	return rep
}

type focusEvent struct {
	at time.Time
	in bool
}

func session(events []event.Event, r Rules, until time.Time) ([]window, []span, int) {
	var (
		out      []window
		focus    []focusEvent
		start    time.Time // session_start, if seen
		turns    []span
		open     = map[string]time.Time{}
		actions  []event.Event
		latency  = map[int]time.Duration{} // approval index in actions -> dialog open time
		lastSeen = events[len(events)-1].TS
	)

	for _, e := range events {
		switch e.Kind {
		case event.SessionStart:
			start = e.TS
		case event.AgentTurnStart:
			if p, err := e.Payload(); err == nil {
				open[p.(*event.AgentTurnStartData).TurnID] = e.TS
			}
		case event.AgentTurnEnd:
			if p, err := e.Payload(); err == nil {
				id := p.(*event.AgentTurnEndData).TurnID
				if s, ok := open[id]; ok {
					turns = append(turns, span{s, e.TS})
					delete(open, id)
				}
			}
		case event.PromptSent, event.Interrupt:
			actions = append(actions, e)
		case event.Approval:
			if p, err := e.Payload(); err == nil {
				latency[len(actions)] = time.Duration(p.(*event.ApprovalData).LatencyMs) * time.Millisecond
			}
			actions = append(actions, e)
		case event.InputTick:
			if p, err := e.Payload(); err == nil {
				d := p.(*event.InputTickData)
				if d.Keys > 0 || d.Mouse > 0 || d.Pasted {
					from := e.TS.Add(-time.Duration(d.WindowMs) * time.Millisecond)
					out = append(out,
						window{span: span{from, e.TS}, project: e.ProjectID, session: e.SessionID, anchor: e.TS, observed: true},
						window{span: span{e.TS, e.TS.Add(r.InputCap)}, project: e.ProjectID, session: e.SessionID, anchor: e.TS},
					)
				}
			}
		case event.Focus:
			if p, err := e.Payload(); err == nil {
				focus = append(focus, focusEvent{e.TS, p.(*event.FocusData).In})
			}
		}
	}
	for _, s := range open {
		turns = append(turns, span{s, lastSeen}) // never ended, assume busy until the last event
	}

	n := r.AgentCap
	for i, a := range actions {
		h := a.TS
		out = append(out, window{span: span{h, h.Add(n)}, project: a.ProjectID, session: a.SessionID, anchor: h})

		var from time.Time
		if l, ok := latency[i]; ok {
			from = h.Add(-l) // reading the dialog
		} else {
			if busyAt(turns, h) {
				continue
			}
			from = latest(start, lastEnd(turns, h))
			if i > 0 {
				from = latest(from, actions[i-1].TS)
			}
			if from.IsZero() {
				continue // nothing to bound it, don't guess
			}
		}
		from = latest(from, h.Add(-n))
		out = append(out, window{span: span{from, h}, project: a.ProjectID, session: a.SessionID, anchor: h})
	}
	return subtract(out, away(focus, r.InputCap, until)), turns, len(actions)
}

func away(focus []focusEvent, limit time.Duration, until time.Time) []span {
	var trips []span
	var left time.Time
	for _, f := range focus {
		switch {
		case !f.in && left.IsZero():
			left = f.at
		case f.in && !left.IsZero():
			if f.at.Sub(left) > limit {
				trips = append(trips, span{left, f.at})
			}
			left = time.Time{}
		}
	}
	if !left.IsZero() && until.Sub(left) > limit {
		trips = append(trips, span{left, until})
	}
	return trips
}

func subtract(ws []window, gaps []span) []window {
	for _, g := range gaps {
		var next []window
		for _, w := range ws {
			if !w.end.After(g.start) || !w.start.Before(g.end) {
				next = append(next, w)
				continue
			}
			if w.start.Before(g.start) {
				before := w
				before.end = g.start
				next = append(next, before)
			}
			if w.end.After(g.end) {
				after := w
				after.start = g.end
				next = append(next, after)
			}
		}
		ws = next
	}
	return ws
}

func busyAt(turns []span, t time.Time) bool {
	for _, s := range turns {
		if s.start.Before(t) && t.Before(s.end) {
			return true
		}
	}
	return false
}

// latest turn end at or before t
func lastEnd(turns []span, t time.Time) time.Time {
	var last time.Time
	for _, s := range turns {
		if !s.end.After(t) && s.end.After(last) {
			last = s.end
		}
	}
	return last
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func clip(s span, from, until time.Time) span {
	s.start = latest(s.start, from)
	if s.end.After(until) {
		s.end = until
	}
	if s.start.After(s.end) {
		s.start = s.end
	}
	return s
}

// splits time at window edges and attributes each piece once
func attribute(ws []window, add func(window, time.Duration)) time.Duration {
	var points []time.Time
	for _, w := range ws {
		points = append(points, w.start, w.end)
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Before(points[j]) })

	var total time.Duration
	for i := 0; i+1 < len(points); i++ {
		a, b := points[i], points[i+1]
		if !a.Before(b) {
			continue
		}
		best := -1
		for j, w := range ws {
			if w.start.After(a) || w.end.Before(b) {
				continue
			}
			if best < 0 || w.precedes(ws[best]) {
				best = j
			}
		}
		if best >= 0 {
			add(ws[best], b.Sub(a))
			total += b.Sub(a)
		}
	}
	return total
}

func (w window) precedes(other window) bool {
	if w.observed != other.observed {
		return w.observed
	}
	return w.anchor.After(other.anchor)
}

func union(spans []span) time.Duration {
	ws := make([]window, len(spans))
	for i, s := range spans {
		ws[i] = window{span: s}
	}
	return attribute(ws, func(window, time.Duration) {})
}

// minutes, like 0m, 27m, 1h05m
func Format(d time.Duration) string {
	m := int(d.Round(time.Minute).Minutes())
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
}
