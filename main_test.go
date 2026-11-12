package main

import (
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
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
