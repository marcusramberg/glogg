package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type mode int

const (
	modeLogs mode = iota
	modeFilter
	modeDetail
	modeProjects
	modeWindow
	modeSeverity
	modeRange
)

const (
	keyEnter = "enter"
	keyEsc   = "esc"

	rangeLayout = "2006-01-02 15:04:05"

	followEvery = 5 * time.Second // 12 reads a minute, well inside the 60 a project allows
)

var (
	windows    = []time.Duration{10 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour, 7 * 24 * time.Hour}
	severities = []string{"", "DEBUG", "INFO", "WARNING", "ERROR"}
	errRange   = errors.New("range must be FROM - TO with TO after FROM, like 2026-11-11 10:41:00 - 2026-11-11 11:00:00")

	// ANSI palette colors so the terminal theme decides the actual look
	accent     = lipgloss.Color("12")
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(accent)
	keyStyle   = lipgloss.NewStyle().Bold(true).Foreground(accent)
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	boxStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent)
	sevStyles  = map[string]lipgloss.Style{
		"DEBUG":     dimStyle,
		"NOTICE":    lipgloss.NewStyle().Foreground(lipgloss.Color("14")),
		"WARNING":   lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
		"ERROR":     errStyle,
		"CRITICAL":  errStyle.Bold(true),
		"ALERT":     errStyle.Bold(true),
		"EMERGENCY": errStyle.Bold(true),
	}
)

type entriesMsg struct {
	gen     int
	entries []entry
	next    string
	more    bool
	err     error
}

// continueMsg asks for the next page of query generation gen.
type continueMsg int

// followMsg is a tick of follow session seq; tailMsg is what its poll found.
type followMsg int

type tailMsg struct {
	seq, gen int
	entries  []entry
	err      error
}

type projectsMsg struct {
	projects []field
	err      error
}

// item is a row in the picker: a project, or a field of the selected entry.
type item struct {
	field
	title string
}

func (i item) Title() string       { return i.title }
func (i item) Description() string { return "" }
func (i item) FilterValue() string { return i.title }

// view is a query and its results; esc in the log view steps back through earlier ones.
type view struct {
	project, filter string
	window, sev     int
	since, until    time.Time // fixed per query so page tokens stay valid; until is set only for a custom range

	entries     []entry
	next        string // page token, "" when there is no more
	cursor, top int
}

type model struct {
	gcp  *gcp
	mode mode
	w, h int

	view
	history []view
	gen     int // bumped per new query, to drop stale responses
	loading bool
	status  string

	follow    bool
	followSeq int // bumped per follow session, so a stale tick chain dies

	input     textinput.Model
	picker    list.Model
	projects  []list.Item // from the cache until refreshed
	refreshed bool        // the project list was fetched this launch
}

func (m *model) Init() tea.Cmd {
	if m.project == "" {
		return m.openProjects()
	}
	return m.fetch(false)
}

func (m *model) fetch(more bool) tea.Cmd {
	if m.project == "" {
		return nil
	}
	token := ""
	if more {
		token = m.next
	} else {
		m.gen++
		if m.until.IsZero() {
			m.since = time.Now().Add(-windows[m.window])
		}
	}
	m.loading, m.status = true, ""
	gen, project := m.gen, m.project
	filter := buildFilter(m.filter, m.since, m.until, severities[m.sev])
	return func() tea.Msg {
		es, next, err := m.gcp.entries(project, filter, token)
		return entriesMsg{gen, es, next, more, err}
	}
}

func (m *model) followTick() tea.Cmd {
	seq := m.followSeq
	return tea.Tick(followEvery, func(time.Time) tea.Msg { return followMsg(seq) })
}

// poll asks for entries newer than the newest one on screen.
// shortcut: entries ingested late with an older (or identical) timestamp are missed, and a burst
// of more than one page between polls leaves a gap; switch to the TailLogEntries stream if that matters.
func (m *model) poll() tea.Cmd {
	after := m.since
	if len(m.entries) > 0 {
		after = m.entries[0].ts
	}
	seq, gen, project := m.followSeq, m.gen, m.project
	filter := buildFilter(m.filter, m.since, time.Time{}, severities[m.sev]) +
		` AND timestamp>"` + after.UTC().Format(time.RFC3339Nano) + `"`
	return func() tea.Msg {
		es, _, err := m.gcp.entries(project, filter, "")
		return tailMsg{seq, gen, es, err}
	}
}

// push remembers the current view; call it before changing the query.
func (m *model) push() {
	if m.project != "" {
		m.history = append(m.history, m.view)
	}
}

// back restores the previous view from memory, without spending a request.
func (m *model) back() tea.Cmd {
	if len(m.history) == 0 {
		return nil
	}
	m.view, m.history = m.history[len(m.history)-1], m.history[:len(m.history)-1]
	m.gen++ // drop responses still in flight for the view we left
	m.loading, m.status = false, ""
	m.scroll()
	if len(m.entries) == 0 { // it was left before its results arrived
		return m.fetch(false)
	}
	return nil
}

func (m *model) bodyH() int { return max(m.h-6, 1) } // header, filter, 2 border lines, graph, footer

func (m *model) resize() {
	m.input.Width = max(m.w-12, 10)
	m.scroll()
	h := m.bodyH()
	switch m.mode {
	case modeDetail:
		h -= h / 3 // the rest shows the selected value in full
	case modeLogs, modeFilter, modeRange:
		return // the zero list.Model panics on SetSize
	}
	m.picker.SetSize(max(m.w-2, 1), h)
}

func (m *model) scroll() {
	m.cursor = max(0, min(m.cursor, len(m.entries)-1))
	m.top = max(min(m.top, m.cursor), m.cursor-m.bodyH()+1)
}

func (m *model) openPicker(mode mode, title string, items []list.Item) {
	d := list.NewDefaultDelegate()
	d.ShowDescription = false
	d.SetSpacing(0)
	m.picker = list.New(items, d, 0, 0)
	m.picker.Title = title
	m.picker.DisableQuitKeybindings()
	m.mode = mode
	m.resize()
}

// openChoice picks one of n fixed options, starting on the current one.
func (m *model) openChoice(mode mode, title string, n, cur int, label func(int) string) {
	items := make([]list.Item, n)
	for i := range items {
		items[i] = item{title: label(i)}
	}
	m.openPicker(mode, title, items)
	m.picker.SetFilteringEnabled(false) // keeps Index() equal to the option index
	m.picker.Select(cur)
}

func (m *model) openProjects() tea.Cmd {
	m.openPicker(modeProjects, "Projects", m.projects)
	if m.refreshed {
		return nil
	}
	m.picker.Title = "Projects (refreshing…)"
	return func() tea.Msg {
		ps, err := m.gcp.listProjects()
		if err == nil {
			_ = saveProjects(projectsCache(), ps) // a failed write only costs the head start next launch
		}
		return projectsMsg{ps, err}
	}
}

func projectItems(ps []field) []list.Item {
	items := make([]list.Item, len(ps))
	for i, p := range ps {
		items[i] = item{p, p.k + dimStyle.Render("  "+clean(p.v))}
	}
	return items
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.resize()
		return m, nil
	case entriesMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		m.loading = false
		if msg.err != nil {
			m.status = msg.err.Error()
			return m, nil
		}
		if msg.more {
			m.entries = append(m.entries, msg.entries...)
		} else {
			m.entries, m.cursor, m.top = msg.entries, 0, 0
		}
		m.next = msg.next
		if m.next != "" && len(m.entries) < m.bodyH() {
			// sparse matches: keep scanning until the screen is full, paced to stay under the read quota
			m.loading = true
			gen := m.gen
			return m, tea.Tick(time.Second, func(time.Time) tea.Msg { return continueMsg(gen) })
		}
		return m, nil
	case continueMsg:
		if int(msg) != m.gen {
			return m, nil
		}
		return m, m.fetch(true)
	case followMsg:
		if !m.follow || int(msg) != m.followSeq {
			return m, nil
		}
		// while a load is running it owns the read quota and the entry list; a custom range has no "latest"
		if m.project == "" || m.loading || !m.until.IsZero() {
			return m, m.followTick()
		}
		return m, m.poll()
	case tailMsg:
		if !m.follow || msg.seq != m.followSeq {
			return m, nil
		}
		if msg.gen == m.gen {
			m.status = ""
			if msg.err != nil {
				m.status = msg.err.Error()
			}
			if n := len(msg.entries); n > 0 {
				m.entries = append(msg.entries, m.entries...)
				if m.cursor > 0 { // reading further down: stay on the same entry
					m.cursor += n
					m.top += n
				}
			}
		}
		return m, m.followTick()
	case projectsMsg:
		if msg.err != nil {
			m.status = msg.err.Error()
			return m, nil
		}
		m.projects, m.refreshed = projectItems(msg.projects), true
		if m.mode == modeProjects {
			m.picker.Title = "Projects"
			return m, m.picker.SetItems(m.projects)
		}
		return m, nil
	case tea.MouseMsg:
		return m, m.mouse(msg)
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		switch m.mode {
		case modeLogs:
			return m, m.logsKey(msg)
		case modeFilter:
			return m, m.filterKey(msg)
		case modeRange:
			return m, m.rangeKey(msg)
		default:
			return m, m.pickerKey(msg)
		}
	}
	// everything else (cursor blink, list filter results) belongs to the active widget
	var cmd tea.Cmd
	switch m.mode {
	case modeFilter, modeRange:
		m.input, cmd = m.input.Update(msg)
	case modeDetail, modeProjects, modeWindow, modeSeverity:
		m.picker, cmd = m.picker.Update(msg)
	}
	return m, cmd
}

func (m *model) key(k tea.KeyMsg) tea.Cmd {
	_, cmd := m.Update(k)
	return cmd
}

// mouse maps the wheel and left clicks onto the keys that do the same thing.
// A click selects a row; a click on the selected row is enter.
func (m *model) mouse(e tea.MouseMsg) tea.Cmd {
	enter := tea.KeyMsg{Type: tea.KeyEnter}
	switch e.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		k := tea.KeyMsg{Type: tea.KeyUp}
		if e.Button == tea.MouseButtonWheelDown {
			k.Type = tea.KeyDown
		}
		return tea.Batch(m.key(k), m.key(k), m.key(k))
	case tea.MouseButtonLeft:
		if e.Action != tea.MouseActionPress {
			return nil
		}
	default:
		return nil
	}
	row := e.Y - 3 // header, filter, top border
	switch {
	case e.Y == m.h-1:
		return m.clickFooter(e.X)
	case row < 0 || row >= m.bodyH() || m.mode == modeFilter || m.mode == modeRange:
		return nil
	case m.mode == modeLogs:
		i := m.top + row
		if i >= len(m.entries) {
			return nil
		}
		if i == m.cursor {
			return m.key(enter)
		}
		m.cursor = i
		return nil
	}
	row -= 4 // the list's title and status bars, two lines each
	p := m.picker.Paginator
	i := p.Page*p.PerPage + row
	if row < 0 || row >= p.PerPage || i >= len(m.picker.VisibleItems()) {
		return nil
	}
	if i == m.picker.Index() {
		return m.key(enter)
	}
	m.picker.Select(i)
	return nil
}

func (m *model) clickFooter(x int) tea.Cmd {
	if m.status != "" { // the footer shows the error instead
		return nil
	}
	ps := m.footerHints()
	for i := 0; i+1 < len(ps); i += 2 {
		x -= lipgloss.Width(hints(ps[i], ps[i+1]))
		if x >= 0 {
			continue
		}
		switch ps[i] {
		case keyEnter:
			return m.key(tea.KeyMsg{Type: tea.KeyEnter})
		case keyEsc:
			return m.key(tea.KeyMsg{Type: tea.KeyEsc})
		}
		return m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(ps[i])})
	}
	return nil
}

// footerHints are the key/label pairs of the footer, which are also what a click on it presses.
func (m *model) footerHints() []string {
	hs := []string{keyEnter, "select"}
	switch m.mode {
	case modeProjects:
		hs = append(hs, "/", "search")
	case modeWindow, modeSeverity:
	case modeDetail:
		hs = []string{keyEnter, "filter on field", "e", "exclude field", "/", "search"}
	case modeFilter, modeRange:
		return []string{keyEnter, "apply", keyEsc, "cancel"}
	default:
		return []string{keyEnter, "fields", "/", "filter", "c", "clear", "p", "project", "t", "window", "s", "severity", "f", "follow", "r", "refresh", keyEsc, "back", "q", "quit"}
	}
	return append(hs, keyEsc, "back")
}

func (m *model) logsKey(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "q":
		return tea.Quit
	case keyEsc:
		return m.back()
	case "up", "k":
		m.cursor--
	case "down", "j":
		m.cursor++
	case "pgup", "ctrl+b":
		m.cursor -= m.bodyH()
	case "pgdown", "ctrl+f", " ":
		m.cursor += m.bodyH()
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = len(m.entries) - 1
	case keyEnter:
		if len(m.entries) == 0 {
			return nil
		}
		fs := m.entries[m.cursor].fields
		items := make([]list.Item, len(fs))
		for i, f := range fs {
			items[i] = item{f, f.k + " = " + oneLine(f.v)}
		}
		m.openPicker(modeDetail, "Entry fields", items)
		return nil
	case "/":
		m.mode = modeFilter
		m.input.SetValue(m.filter)
		m.input.CursorEnd()
		return m.input.Focus()
	case "p":
		return m.openProjects()
	case "t":
		cur := m.window
		if !m.until.IsZero() {
			cur = len(windows)
		}
		m.openChoice(modeWindow, "Window", len(windows)+1, cur, func(i int) string {
			if i == len(windows) {
				return "custom range…"
			}
			return fmtWindow(windows[i])
		})
		return nil
	case "s":
		m.openChoice(modeSeverity, "Minimum severity", len(severities), m.sev, fmtSev)
		return nil
	case "f":
		if !m.until.IsZero() {
			m.status = "follow needs a relative window, not a custom range"
			return nil
		}
		m.follow = !m.follow
		if !m.follow {
			return nil
		}
		m.followSeq++
		m.cursor, m.top = 0, 0
		seq := m.followSeq
		return func() tea.Msg { return followMsg(seq) }
	case "c":
		if m.filter == "" {
			return nil
		}
		m.push()
		m.filter = ""
		return m.fetch(false)
	case "r":
		return m.fetch(false)
	}
	m.scroll()
	if m.cursor == len(m.entries)-1 && m.next != "" && !m.loading {
		return m.fetch(true)
	}
	return nil
}

func (m *model) filterKey(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case keyEsc:
		m.mode = modeLogs
		return nil
	case keyEnter:
		m.mode = modeLogs
		f := strings.TrimSpace(m.input.Value())
		if f == m.filter {
			return nil
		}
		m.push()
		m.filter = f
		return m.fetch(false)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return cmd
}

func (m *model) rangeKey(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case keyEsc:
		m.mode, m.status = modeLogs, ""
		return nil
	case keyEnter:
		from, to, err := parseRange(m.input.Value())
		if err != nil {
			m.status = err.Error()
			return nil
		}
		m.mode, m.follow = modeLogs, false
		m.push()
		m.since, m.until = from, to
		return m.fetch(false)
	}
	m.status = "" // bring the format hint back once the range is being corrected
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return cmd
}

// pickWindow applies option i of the window picker; the one past the presets asks for a custom range.
func (m *model) pickWindow(i int) tea.Cmd {
	if i == len(windows) {
		from, to := m.since, m.until
		if to.IsZero() { // start from the window on screen
			to = time.Now()
			from = to.Add(-windows[m.window])
		}
		m.mode = modeRange
		m.input.SetValue(from.Format(rangeLayout) + " - " + to.Format(rangeLayout))
		m.input.CursorEnd()
		return m.input.Focus()
	}
	m.mode = modeLogs
	if i == m.window && m.until.IsZero() {
		return nil
	}
	m.push()
	m.window, m.until = i, time.Time{}
	return m.fetch(false)
}

func (m *model) pickerKey(k tea.KeyMsg) tea.Cmd {
	sel, ok := m.picker.SelectedItem().(item)
	if m.picker.FilterState() != list.Filtering { // while typing a filter every key is text
		switch key := k.String(); {
		case key == "q", key == keyEsc && m.picker.FilterState() == list.Unfiltered:
			m.mode = modeLogs
			return nil
		case key == keyEnter && ok && m.mode == modeProjects:
			m.push()
			m.project, m.entries, m.next = sel.k, nil, ""
			m.mode = modeLogs
			return m.fetch(false)
		case key == keyEnter && ok && m.mode == modeSeverity:
			m.mode = modeLogs
			i := m.picker.Index()
			if i == m.sev {
				return nil
			}
			m.push()
			m.sev = i
			return m.fetch(false)
		case key == keyEnter && ok && m.mode == modeWindow:
			return m.pickWindow(m.picker.Index())
		case (key == keyEnter || key == "e" || key == "-" || key == "x") && ok && m.mode == modeDetail:
			term := sel.expr()
			if key != keyEnter {
				term = "NOT " + term
			}
			if m.filter != "" {
				term = m.filter + " AND " + term
			}
			m.push()
			m.filter = term
			m.mode = modeLogs
			return m.fetch(false)
		}
	}
	var cmd tea.Cmd
	m.picker, cmd = m.picker.Update(k)
	return cmd
}

func (m *model) View() string {
	if m.w == 0 {
		return ""
	}
	inner := m.w - 2
	count := fmt.Sprint(len(m.entries))
	if m.next != "" {
		count += "+"
	}
	if m.loading {
		count += " (loading…)"
	}
	var head strings.Builder
	head.WriteString(titleStyle.Render(" glogg "))
	for _, kv := range [][2]string{
		{"project", cmp(m.project, "-")},
		{"window", m.windowLabel()},
		{"severity", fmtSev(m.sev)},
		{"entries", count},
	} {
		head.WriteString("  " + dimStyle.Render(kv[0]+":") + " " + kv[1])
	}

	if m.follow {
		head.WriteString("  " + keyStyle.Render("following"))
	}

	filter := " " + dimStyle.Render("filter:") + " "
	switch m.mode {
	case modeFilter:
		filter += m.input.View()
	case modeRange:
		filter = " " + dimStyle.Render("range:") + " " + m.input.View()
	default:
		filter += cmp(m.filter, dimStyle.Render("none"))
	}

	var body string
	footer := hints(m.footerHints()...)
	switch m.mode {
	case modeProjects, modeWindow, modeSeverity:
		body = m.picker.View()
	case modeDetail:
		value := ""
		if sel, ok := m.picker.SelectedItem().(item); ok {
			value = clean(sel.v)
		}
		body = m.picker.View() + "\n" + dimStyle.Render(strings.Repeat("─", inner)) + "\n" +
			lipgloss.NewStyle().Width(inner).MaxHeight(max(m.bodyH()/3-1, 1)).Render(value)
	default:
		body = m.logsView(inner)
		switch m.mode {
		case modeFilter:
			footer += dimStyle.Render(" Logging query language, e.g. resource.type=\"k8s_container\" AND \"timeout\"")
		case modeRange:
			footer += dimStyle.Render(" FROM - TO in local time, YYYY-MM-DD HH:MM:SS")
		}
	}
	if m.status != "" {
		footer = errStyle.Render(" " + oneLine(m.status))
	}
	return strings.Join([]string{
		ansi.Truncate(head.String(), m.w, "…"),
		ansi.Truncate(filter, m.w, "…"),
		boxStyle.Width(inner).Height(m.bodyH()).MaxHeight(m.bodyH() + 2).Render(body),
		ansi.Truncate(m.graphView(), m.w, "…"),
		ansi.Truncate(footer, m.w, "…"),
	}, "\n")
}

func (m *model) logsView(width int) string {
	switch {
	case m.project == "":
		return dimStyle.Render(" Press p to pick a project")
	case len(m.entries) == 0 && m.loading:
		return dimStyle.Render(" Loading…")
	case len(m.entries) == 0 && m.status != "":
		return "" // the footer shows the error
	case len(m.entries) == 0:
		return dimStyle.Render(" No entries match (window " + m.windowLabel() + ")")
	}
	var b strings.Builder
	for i := m.top; i < min(m.top+m.bodyH(), len(m.entries)); i++ {
		e := m.entries[i]
		line := fmt.Sprintf("%s %-9s %-24s %s", e.ts.Local().Format("01-02 15:04:05.000"), e.sev,
			ansi.Truncate(oneLine(e.src), 24, "…"), oneLine(e.msg))
		st := sevStyles[e.sev]
		if i == m.cursor {
			st = st.Reverse(true).Width(width)
		}
		if i > m.top {
			b.WriteByte('\n')
		}
		b.WriteString(st.Render(ansi.Truncate(line, width, "…")))
	}
	return b.String()
}

// graphView is the volume of the loaded entries across the queried time span.
func (m *model) graphView() string {
	if m.since.IsZero() {
		return ""
	}
	to := m.until
	if to.IsZero() {
		to = m.since.Add(windows[m.window])
		if m.follow {
			to = time.Now()
		}
	}
	const label = "01-02 15:04"
	return " " + dimStyle.Render(m.since.Format(label)) + " " +
		keyStyle.Render(sparkline(m.entries, m.since, to, m.w-2*len(label)-4)) + " " + dimStyle.Render(to.Format(label))
}

// sparkline counts entries per time bucket between from and to, one bar per column, scaled to the busiest bucket.
// shortcut: counts only the entries loaded so far, so older buckets stay empty until you page down;
// query Cloud Monitoring's log_entry_count metric if it must show true volume up front.
func sparkline(es []entry, from, to time.Time, width int) string {
	if width < 1 {
		return ""
	}
	counts, peak := make([]int, width), 0
	for _, e := range es {
		i := int(float64(e.ts.Sub(from)) / float64(to.Sub(from)) * float64(width))
		i = max(0, min(i, width-1))
		counts[i]++
		peak = max(peak, counts[i])
	}
	bars := []rune(" ▁▂▃▄▅▆▇█")
	out := make([]rune, width)
	for i, c := range counts {
		out[i] = bars[(c*8+max(peak, 1)-1)/max(peak, 1)] // round up so any entry shows
	}
	return string(out)
}

func hints(pairs ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		b.WriteString(" " + keyStyle.Render("<"+pairs[i]+">") + " " + pairs[i+1])
	}
	return b.String()
}

func cmp(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func fmtSev(i int) string {
	if severities[i] == "" {
		return "any"
	}
	return "≥" + severities[i]
}

func (m *model) windowLabel() string {
	if m.until.IsZero() {
		return fmtWindow(windows[m.window])
	}
	const short = "01-02 15:04:05" // same as the log rows
	return m.since.Format(short) + " – " + m.until.Format(short)
}

// parseRange reads "FROM - TO" in local time, both as rangeLayout.
func parseRange(s string) (time.Time, time.Time, error) {
	s = strings.TrimSpace(s)
	n := min(len(s), len(rangeLayout)) // fixed width, so the dashes inside the dates can't be the separator
	from, err1 := time.ParseInLocation(rangeLayout, s[:n], time.Local)
	to, err2 := time.ParseInLocation(rangeLayout, strings.TrimLeft(s[n:], " -–"), time.Local)
	if err1 != nil || err2 != nil || !to.After(from) {
		return from, to, errRange
	}
	return from, to, nil
}

func fmtWindow(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return fmt.Sprintf("%dh", d/time.Hour)
}

// clean drops escape sequences and control characters so log content can't drive the terminal.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if (r < 0x20 && r != '\n') || r == 0x7f {
			return -1
		}
		return r
	}, ansi.Strip(s))
}

func oneLine(s string) string { return strings.Join(strings.Fields(clean(s)), " ") }

func main() {
	project := flag.String("p", os.Getenv("GOOGLE_CLOUD_PROJECT"), "GCP project id (default $GOOGLE_CLOUD_PROJECT, else pick from a list)")
	filter := flag.String("f", "", "initial filter in the Logging query language")
	flag.Parse()

	g, err := newGCP(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "glogg: %v\nTry: gcloud auth application-default login\n", err)
		os.Exit(1)
	}
	m := &model{gcp: g, project: *project, filter: *filter, input: textinput.New(), projects: projectItems(loadProjects(projectsCache()))}
	m.input.Prompt = ""
	if _, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "glogg:", err)
		os.Exit(1)
	}
}
