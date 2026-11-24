package views

import (
	"embed"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"

	"github.com/a-h/templ"
)

// 1h 5m, or 5m under an hour
func long(d time.Duration) string {
	m := int(d.Round(time.Minute).Minutes())
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh %dm", m/60, m%60)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

var harnessNames = map[string]string{"claude-code": "Claude Code", "codex": "Codex", "opencode": "opencode"}

func harnessName(h string) string {
	if n, ok := harnessNames[h]; ok {
		return n
	}
	return h
}

func barWidth(d, max time.Duration) string {
	if max <= 0 {
		return "--w: 0%"
	}
	return fmt.Sprintf("--w: %.1f%%", float64(d)/float64(max)*100)
}

// today, you've logged 3h 5m across a, b (& 2 other models) using c and d.
// built here because templ puts spaces around inline elements
func sentence(s Summary) string {
	var b strings.Builder
	b.WriteString(`Today, you've logged <strong class="total">` + long(s.Total) + `</strong>`)
	if len(s.Models) > 0 {
		b.WriteString(" across " + names(s.Models, 2, "model", "models", func(n string) string { return n }))
	}
	if len(s.Harnesses) > 0 {
		b.WriteString(" using " + names(s.Harnesses, 3, "harness", "harnesses", harnessName))
	}
	b.WriteString(".")
	return b.String()
}

// "a, b and c", or "a, b (& 3 other models)" past max
func names(list []Entry, max int, one, many string, label func(string) string) string {
	var parts []string
	for _, e := range list[:min(max, len(list))] {
		parts = append(parts, "<strong>"+html.EscapeString(label(e.Name))+"</strong>")
	}
	if rest := len(list) - max; rest > 0 {
		return strings.Join(parts, ", ") + fmt.Sprintf(" (&amp; %d other %s)", rest, plural(rest, one, many))
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

func pfpURL(slackID string) string {
	return "https://cachet.dunkirk.sh/users/" + url.PathEscape(slackID) + "/r"
}

func initial(name string) string {
	for _, r := range strings.TrimSpace(name) {
		return strings.ToUpper(string(r))
	}
	return "?"
}

//go:embed icons/*.svg
var iconFiles embed.FS

func icon(name string) templ.Component {
	svg, err := iconFiles.ReadFile("icons/" + name + ".svg")
	if err != nil {
		return templ.Raw("")
	}
	return templ.Raw(string(svg))
}

type modelSlice struct {
	Entry
	color string
	end   string
}

func modelDistribution(s Summary) ([]modelSlice, string) {
	colors := [...]string{
		"var(--color-chart-1)",
		"var(--color-chart-2)",
		"var(--color-chart-3)",
		"var(--color-chart-4)",
		"var(--color-chart-5)",
	}
	var slices []modelSlice
	var known, other time.Duration
	for i, e := range s.Models {
		known += e.Time
		if i < len(colors) {
			slices = append(slices, modelSlice{Entry: e, color: colors[i]})
		} else {
			other += e.Time
		}
	}
	if other > 0 {
		slices = append(slices, modelSlice{Entry: Entry{"other models", other}, color: "var(--color-chart-other)"})
	}
	if unknown := s.Total - known; unknown > 0 {
		slices = append(slices, modelSlice{Entry: Entry{"unknown model", unknown}, color: "var(--color-chart-unreported)"})
	}

	var stops []string
	var elapsed time.Duration
	for i, slice := range slices {
		start := 100 * float64(elapsed) / float64(s.Total)
		elapsed += slice.Time
		end := 100 * float64(elapsed) / float64(s.Total)
		slices[i].end = fmt.Sprintf("%.4f", end)
		stops = append(stops, fmt.Sprintf("%s %.4f%% %.4f%%", slice.color, start, end))
	}
	return slices, "background:conic-gradient(" + strings.Join(stops, ",") + ")"
}
