package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestSeverityPicker(t *testing.T) {
	t.Parallel()
	m := &model{w: 80, h: 24}
	m.sev = 1
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if m.mode != modeSeverity || m.picker.Index() != 1 {
		t.Fatalf("mode %v index %d, want picker open on the current severity", m.mode, m.picker.Index())
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modeLogs || severities[m.sev] != "WARNING" {
		t.Fatalf("mode %v sev %q, want logs view with WARNING", m.mode, severities[m.sev])
	}
}

func TestMouse(t *testing.T) {
	t.Parallel()
	m := &model{w: 120, h: 24}
	m.entries = []entry{{msg: "a"}, {msg: "b", fields: []field{{k: "k", v: "v"}}}}
	lines := func() []string { return strings.Split(ansi.Strip(m.View()), "\n") }
	click := func(x, y int) {
		m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	}
	rowOf := func(s string) int {
		for y, l := range lines() {
			if strings.Contains(l, s) {
				return y
			}
		}
		t.Fatalf("%q is not on screen", s)
		return 0
	}

	click(5, 4) // second row of the body
	if m.cursor != 1 || m.mode != modeLogs {
		t.Fatalf("cursor %d mode %v, want the clicked entry selected", m.cursor, m.mode)
	}
	click(5, 4)
	if m.mode != modeDetail {
		t.Fatalf("mode %v, want a click on the selected entry to open it", m.mode)
	}
	click(strings.Index(lines()[23], "<esc>"), 23)
	if m.mode != modeLogs {
		t.Fatalf("mode %v, want the esc hint to go back", m.mode)
	}

	click(strings.Index(lines()[23], "<s>"), 23)
	if m.mode != modeSeverity {
		t.Fatalf("mode %v, want the severity hint to open its picker", m.mode)
	}
	y := rowOf("≥WARNING")
	click(5, y)
	if m.picker.Index() != 3 || m.mode != modeSeverity {
		t.Fatalf("index %d mode %v, want the clicked option selected", m.picker.Index(), m.mode)
	}
	click(5, y)
	if m.mode != modeLogs || severities[m.sev] != "WARNING" {
		t.Fatalf("mode %v sev %q, want logs view with WARNING", m.mode, severities[m.sev])
	}

	m.cursor = 0
	m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown})
	if m.cursor != 1 {
		t.Errorf("cursor %d, want the wheel to scroll to the last entry", m.cursor)
	}
}

func TestCustomRange(t *testing.T) {
	t.Parallel()
	m := &model{w: 80, h: 24, input: textinput.New()}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if m.picker.Index() != 0 || fmtWindow(windows[0]) != "10m" {
		t.Fatalf("index %d, want picker open on the 10m default", m.picker.Index())
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modeRange {
		t.Fatalf("mode %v, want range input", m.mode)
	}
	m.input.SetValue("2026-11-11 10:41:00-2026-11-11 11:00:00")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	want := time.Date(2026, 11, 11, 10, 41, 0, 0, time.Local)
	if m.mode != modeLogs || !m.since.Equal(want) || m.until.Sub(m.since) != 19*time.Minute {
		t.Fatalf("mode %v since %v until %v", m.mode, m.since, m.until)
	}
	for _, bad := range []string{"", "2026-11-11 10:41:00", "2026-11-11 11:00:00 - 2026-11-11 10:41:00", "yesterday - today"} {
		if _, _, err := parseRange(bad); err == nil {
			t.Errorf("parseRange(%q) accepted", bad)
		}
	}
}

func TestSparkline(t *testing.T) {
	t.Parallel()
	from := time.Unix(0, 0)
	at := func(s int) entry { return entry{ts: from.Add(time.Duration(s) * time.Second)} }
	es := []entry{at(0), at(5), at(15), at(25), at(26), at(27), at(28), at(40)} // 40 is the inclusive end
	if got := sparkline(es, from, from.Add(40*time.Second), 4); got != "▄▂█▂" {
		t.Errorf("got %q", got)
	}
	if got := sparkline(nil, from, from, 3); got != "   " {
		t.Errorf("got %q", got)
	}
}

func TestFollowPrepends(t *testing.T) {
	t.Parallel()
	at := func(s int64) entry { return entry{ts: time.Unix(s, 0)} }
	m := &model{w: 80, h: 24, follow: true, followSeq: 1}
	m.entries, m.cursor = []entry{at(2), at(1)}, 1
	m.Update(tailMsg{seq: 1, entries: []entry{at(3)}})
	if len(m.entries) != 3 || !m.entries[0].ts.Equal(time.Unix(3, 0)) || m.cursor != 2 {
		t.Fatalf("entries %v cursor %d, want the new entry on top and the cursor on the same entry", m.entries, m.cursor)
	}
	m.cursor, m.top = 0, 0
	m.Update(tailMsg{seq: 1, entries: []entry{at(4)}})
	if m.cursor != 0 {
		t.Errorf("cursor %d, want it to stay on the newest entry", m.cursor)
	}
	m.Update(tailMsg{seq: 0, entries: []entry{at(5)}})
	m.Update(tailMsg{seq: 1, gen: 7, entries: []entry{at(5)}})
	if len(m.entries) != 4 {
		t.Errorf("%d entries, want stale polls dropped", len(m.entries))
	}
}
