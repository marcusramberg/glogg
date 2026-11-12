package main

import (
	"context"
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
)

const (
	keyEnter = "enter"
	keyEsc   = "esc"
)

var (
	windows    = []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour, 7 * 24 * time.Hour}
	severities = []string{"", "DEBUG", "INFO", "WARNING", "ERROR"}

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
	since           time.Time // fixed per query so page tokens stay valid

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

	input    textinput.Model
	picker   list.Model
	projects []list.Item
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
		m.since = time.Now().Add(-windows[m.window])
	}
	m.loading, m.status = true, ""
	gen, project := m.gen, m.project
	filter := buildFilter(m.filter, m.since, severities[m.sev])
	return func() tea.Msg {
		es, next, err := m.gcp.entries(project, filter, token)
		return entriesMsg{gen, es, next, more, err}
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

func (m *model) bodyH() int { return max(m.h-5, 1) } // header, filter, 2 border lines, footer

func (m *model) resize() {
	m.input.Width = max(m.w-12, 10)
	m.scroll()
	h := m.bodyH()
	switch m.mode {
	case modeDetail:
		h -= h / 3 // the rest shows the selected value in full
	case modeProjects:
	default:
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

func (m *model) openProjects() tea.Cmd {
	m.openPicker(modeProjects, "Projects", m.projects)
	if m.projects != nil {
		return nil
	}
	m.picker.Title = "Projects (loading…)"
	return func() tea.Msg {
		ps, err := m.gcp.listProjects()
		return projectsMsg{ps, err}
	}
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
	case projectsMsg:
		if msg.err != nil {
			m.status = msg.err.Error()
			return m, nil
		}
		m.projects = make([]list.Item, len(msg.projects))
		for i, p := range msg.projects {
			m.projects[i] = item{p, p.k + dimStyle.Render("  "+clean(p.v))}
		}
		if m.mode == modeProjects {
			m.picker.Title = "Projects"
			return m, m.picker.SetItems(m.projects)
		}
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		switch m.mode {
		case modeLogs:
			return m, m.logsKey(msg)
		case modeFilter:
			return m, m.filterKey(msg)
		default:
			return m, m.pickerKey(msg)
		}
	}
	// everything else (cursor blink, list filter results) belongs to the active widget
	var cmd tea.Cmd
	switch m.mode {
	case modeFilter:
		m.input, cmd = m.input.Update(msg)
	case modeDetail, modeProjects:
		m.picker, cmd = m.picker.Update(msg)
	}
	return m, cmd
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
		m.push()
		m.window = (m.window + 1) % len(windows)
		return m.fetch(false)
	case "s":
		m.push()
		m.sev = (m.sev + 1) % len(severities)
		return m.fetch(false)
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
		case (key == keyEnter || key == "x") && ok && m.mode == modeDetail:
			term := sel.expr()
			if key == "x" {
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
	sev := "any"
	if s := severities[m.sev]; s != "" {
		sev = "≥" + s
	}
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
		{"window", fmtWindow(windows[m.window])},
		{"severity", sev},
		{"entries", count},
	} {
		head.WriteString("  " + dimStyle.Render(kv[0]+":") + " " + kv[1])
	}

	filter := " " + dimStyle.Render("filter:") + " "
	if m.mode == modeFilter {
		filter += m.input.View()
	} else {
		filter += cmp(m.filter, dimStyle.Render("none"))
	}

	var body, footer string
	switch m.mode {
	case modeProjects:
		body = m.picker.View()
		footer = hints(keyEnter, "select", "/", "search", keyEsc, "back")
	case modeDetail:
		value := ""
		if sel, ok := m.picker.SelectedItem().(item); ok {
			value = clean(sel.v)
		}
		body = m.picker.View() + "\n" + dimStyle.Render(strings.Repeat("─", inner)) + "\n" +
			lipgloss.NewStyle().Width(inner).MaxHeight(max(m.bodyH()/3-1, 1)).Render(value)
		footer = hints(keyEnter, "filter on field", "x", "exclude field", "/", "search", keyEsc, "back")
	default:
		body = m.logsView(inner)
		footer = hints(keyEnter, "fields", "/", "filter", "c", "clear", "p", "project", "t", "window", "s", "severity", "r", "refresh", keyEsc, "back", "q", "quit")
		if m.mode == modeFilter {
			footer = hints(keyEnter, "apply", keyEsc, "cancel") + dimStyle.Render(" Logging query language, e.g. resource.type=\"k8s_container\" AND \"timeout\"")
		}
	}
	if m.status != "" {
		footer = errStyle.Render(" " + oneLine(m.status))
	}
	return strings.Join([]string{
		ansi.Truncate(head.String(), m.w, "…"),
		ansi.Truncate(filter, m.w, "…"),
		boxStyle.Width(inner).Height(m.bodyH()).MaxHeight(m.bodyH() + 2).Render(body),
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
		return dimStyle.Render(" No entries match in the last " + fmtWindow(windows[m.window]))
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

func fmtWindow(d time.Duration) string {
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
	m := &model{gcp: g, project: *project, filter: *filter, input: textinput.New()}
	m.input.Prompt = ""
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "glogg:", err)
		os.Exit(1)
	}
}
