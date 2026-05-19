package client

import (
	"fmt"
	"strings"
	"time"

	"grecon/server"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	colorCyan     = lipgloss.Color("6")
	colorGreen    = lipgloss.Color("2")
	colorYellow   = lipgloss.Color("3")
	colorBlue     = lipgloss.Color("4")
	colorRed      = lipgloss.Color("1")
	colorDarkGray = lipgloss.Color("8")

	headerStyle = lipgloss.NewStyle().Foreground(colorCyan).Bold(true)
	dimStyle    = lipgloss.NewStyle().Foreground(colorDarkGray)
	cyanStyle   = lipgloss.NewStyle().Foreground(colorCyan)
	greenStyle  = lipgloss.NewStyle().Foreground(colorGreen)

	selectedBg      = lipgloss.Color("240")
	inputBg         = lipgloss.Color("#322800")
	inputSelectedBg = lipgloss.Color("#504100")
)

type tickMsg struct{}

type tuiModel struct {
	app    *App
	width  int
	height int
}

func newTUIModel() (tuiModel, error) {
	app := NewApp()
	app.Refresh()
	app.StartBackgroundRefresh()
	return tuiModel{app: app, width: 80, height: 24}, nil
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(_ time.Time) tea.Msg { return tickMsg{} })
}

func tickCmd() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(_ time.Time) tea.Msg { return tickMsg{} })
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tickMsg:
		m.app.TryReceive()
		m.app.AdvanceTick()
		return m, tickCmd()

	case tea.KeyMsg:
		code, ctrl := translateKey(msg)
		m.app.HandleKey(code, ctrl)
		if m.app.ShouldQuit {
			return m, tea.Quit
		}
		return m, nil
	}
	return m, nil
}

func translateKey(msg tea.KeyMsg) (string, bool) {
	switch msg.Type {
	case tea.KeyEsc:
		return "esc", false
	case tea.KeyEnter:
		return "enter", false
	case tea.KeyTab:
		return "tab", false
	case tea.KeyUp:
		return "up", false
	case tea.KeyDown:
		return "down", false
	case tea.KeyLeft:
		return "left", false
	case tea.KeyRight:
		return "right", false
	case tea.KeyBackspace:
		return "backspace", false
	case tea.KeyDelete:
		return "delete", false
	case tea.KeyHome:
		return "home", false
	case tea.KeyEnd:
		return "end", false
	case tea.KeyCtrlA:
		return "a", true
	case tea.KeyCtrlE:
		return "e", true
	case tea.KeyCtrlU:
		return "u", true
	case tea.KeyRunes:
		if len(msg.Runes) == 1 {
			return string(msg.Runes), false
		}
	}
	return msg.String(), false
}

func (m tuiModel) View() string {
	var b strings.Builder

	showSearch := m.app.FilterActive || m.app.FilterText != ""
	tableContentHeight := m.height - 2 - 1
	if showSearch {
		tableContentHeight--
	}
	if tableContentHeight < 2 {
		tableContentHeight = 2
	}

	renderTable(&b, m.app, m.width, tableContentHeight)
	if showSearch {
		renderSearchBar(&b, m.app, m.width)
	}
	renderFooter(&b, m.app, m.width)

	return b.String()
}

func renderTable(b *strings.Builder, app *App, width, contentHeight int) {
	innerW := width - 2

	colName := 30
	colStatus := 12
	colSummary := innerW - colName - colStatus
	if colSummary < 20 {
		colSummary = 20
	}

	title := " grecon — Claude Code Sessions "
	topBorder := "┌" + title
	remaining := innerW - lipgloss.Width(title)
	if remaining > 0 {
		topBorder += strings.Repeat("─", remaining)
	}
	topBorder += "┐"
	b.WriteString(topBorder)
	b.WriteString("\n")

	header := buildRow([]colSpec{
		{colName, " Name"},
		{colStatus, "Status"},
		{colSummary, "Summary"},
	})
	b.WriteString("│")
	b.WriteString(headerStyle.Render(fitToWidth(header, innerW)))
	b.WriteString("│\n")

	rows := app.DisplayRows()
	rowsAvail := contentHeight - 1
	agentIdx := 0

	for di, row := range rows {
		if di >= rowsAvail {
			break
		}

		switch row.Kind {
		case RowHeader:
			headerText := truncEllipsis(row.Header, colName-1)
			line := " \x1b[1m" + headerText + "\x1b[0m"
			plainLen := visibleWidth(line)
			if plainLen < innerW {
				line += strings.Repeat(" ", innerW-plainLen)
			}
			b.WriteString("│")
			b.WriteString(line)
			b.WriteString("│\n")

		case RowAgent:
			s := row.Session
			isSelected := agentIdx == app.Selected
			needBg := isSelected || s.Status == server.StatusInput

			var prefix string
			if row.IsLast {
				prefix = " └ "
			} else {
				prefix = " ├ "
			}

			agentName := s.ClaudeName
			if agentName == "" {
				agentName = s.ProjectName
			}
			if agentName == "" {
				agentName = "—"
			}
			nameCol := ansiColor("90", prefix) + truncEllipsis(agentName, colName-visibleWidth(prefix))

			statusCol := formatStatus(s.Status)

			summaryCol := s.Summary
			if summaryCol == "" {
				summaryCol = ansiColor("90", "—")
			}

			rowStr := padCol(nameCol, colName) +
				padCol(statusCol, colStatus) +
				padCol(summaryCol, colSummary)

			plainLen := visibleWidth(rowStr)
			if plainLen < innerW {
				rowStr += strings.Repeat(" ", innerW-plainLen)
			}

			if needBg {
				var bgCode string
				if s.Status == server.StatusInput && isSelected {
					bgCode = "\x1b[48;2;80;65;0m"
				} else if s.Status == server.StatusInput {
					bgCode = "\x1b[48;2;50;40;0m"
				} else {
					bgCode = "\x1b[48;5;240m"
				}
				rowStr = applyRowBg(rowStr, bgCode)
			}

			b.WriteString("│")
			b.WriteString(rowStr)
			b.WriteString("│\n")
			agentIdx++

		case RowSubagent:
			sa := row.Subagent

			var vbar string
			if row.AgentIsLast {
				vbar = "   "
			} else {
				vbar = " │ "
			}
			var branch string
			if row.IsLast {
				branch = " └ "
			} else {
				branch = " ├ "
			}
			prefix := vbar + branch

			nameCol := ansiColor("90", prefix) + ansiColor("36", truncEllipsis(sa.AgentType, colName-visibleWidth(prefix)))

			statusCol := formatStatus(sa.Status)

			summaryCol := sa.Summary
			if summaryCol == "" && sa.Description != "" {
				summaryCol = sa.Description
			}
			if summaryCol == "" {
				summaryCol = ansiColor("90", "—")
			}

			rowStr := padCol(nameCol, colName) +
				padCol(statusCol, colStatus) +
				padCol(summaryCol, colSummary)

			plainLen := visibleWidth(rowStr)
			if plainLen < innerW {
				rowStr += strings.Repeat(" ", innerW-plainLen)
			}

			b.WriteString("│")
			b.WriteString(rowStr)
			b.WriteString("│\n")

		case RowWakeup:
			w := row.Wakeup

			var vbar string
			if row.AgentIsLast {
				vbar = "   "
			} else {
				vbar = " │ "
			}
			var branch string
			if row.IsLast {
				branch = " └ "
			} else {
				branch = " ├ "
			}
			prefix := vbar + branch

			remaining := time.Until(w.FiresAt)
			var countdown string
			if remaining > 0 {
				mins := int(remaining.Minutes())
				secs := int(remaining.Seconds()) % 60
				if mins > 0 {
					countdown = fmt.Sprintf("%dm%ds", mins, secs)
				} else {
					countdown = fmt.Sprintf("%ds", secs)
				}
			} else {
				countdown = "now"
			}

			nameCol := ansiColor("90", prefix) + ansiColor("35", "wakeup in "+countdown)
			statusCol := ansiColor("35", "● Sleep")

			summaryCol := w.Reason
			if summaryCol == "" {
				summaryCol = ansiColor("90", "—")
			}

			rowStr := padCol(nameCol, colName) +
				padCol(statusCol, colStatus) +
				padCol(summaryCol, colSummary)

			plainLen := visibleWidth(rowStr)
			if plainLen < innerW {
				rowStr += strings.Repeat(" ", innerW-plainLen)
			}

			b.WriteString("│")
			b.WriteString(rowStr)
			b.WriteString("│\n")
		}
	}

	rendered := len(rows)
	if rendered > rowsAvail {
		rendered = rowsAvail
	}
	emptyRow := strings.Repeat(" ", innerW)
	for i := rendered; i < rowsAvail; i++ {
		b.WriteString("│")
		b.WriteString(emptyRow)
		b.WriteString("│\n")
	}

	b.WriteString("└")
	b.WriteString(strings.Repeat("─", innerW))
	b.WriteString("┘\n")
}

func formatStatus(status server.SessionStatus) string {
	var dot, label, ansi string
	switch status {
	case server.StatusNew:
		dot, label, ansi = "●", "New", "34"
	case server.StatusWorking:
		dot, label, ansi = "●", "Work", "32"
	case server.StatusIdle:
		dot, label, ansi = "●", "Idle", "90"
	case server.StatusInput:
		dot, label, ansi = "●", "Input", "33"
	}
	return ansiColor(ansi, dot+" "+label)
}

func renderSearchBar(b *strings.Builder, app *App, width int) {
	line := cyanStyle.Render("/") + app.FilterText
	if !app.FilterActive && app.FilterText != "" {
		count := app.SelectableCount()
		suffix := "es"
		if count == 1 {
			suffix = ""
		}
		line += dimStyle.Render(fmt.Sprintf("  (%d match%s)", count, suffix))
	}
	b.WriteString(fitToWidth(line, width))
	b.WriteString("\n")
}

func renderFooter(b *strings.Builder, app *App, width int) {
	var line string
	if app.FilterActive {
		line = cyanStyle.Render("Esc") + " clear  " +
			cyanStyle.Render("Enter") + " keep filter  " +
			cyanStyle.Render("j/k") + " navigate"
	} else {
		line = cyanStyle.Render("j/k") + " navigate  " +
			cyanStyle.Render("Enter") + " switch  " +
			cyanStyle.Render("x") + " kill  " +
			cyanStyle.Render("/") + " search  " +
			cyanStyle.Render("i") + " next input  " +
			cyanStyle.Render("q") + " quit"
	}
	b.WriteString(fitToWidth(line, width))
}

func RunTUI() (string, error) {
	m, err := newTUIModel()
	if err != nil {
		return "", err
	}
	p := tea.NewProgram(m, tea.WithAltScreen())
	result, err := p.Run()
	if err != nil && strings.Contains(err.Error(), "resource temporarily unavailable") {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	tm := result.(tuiModel)
	return tm.app.SwitchTarget, nil
}

type colSpec struct {
	width int
	text  string
}

func buildRow(cols []colSpec) string {
	var b strings.Builder
	for _, c := range cols {
		b.WriteString(padCol(c.text, c.width))
	}
	return b.String()
}

func padCol(s string, width int) string {
	s = truncAnsi(s, width)
	visLen := lipgloss.Width(s)
	if visLen >= width {
		return s
	}
	return s + strings.Repeat(" ", width-visLen)
}

func fitToWidth(s string, width int) string {
	visLen := lipgloss.Width(s)
	if visLen >= width {
		return s
	}
	return s + strings.Repeat(" ", width-visLen)
}

func truncAnsi(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	visCount := 0
	inEscape := false
	var result strings.Builder
	hadEscape := false

	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			hadEscape = true
			result.WriteRune(r)
			continue
		}
		if inEscape {
			result.WriteRune(r)
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				inEscape = false
			}
			continue
		}
		if visCount >= maxWidth {
			break
		}
		result.WriteRune(r)
		visCount++
	}

	out := result.String()
	if hadEscape {
		out += "\x1b[0m"
	}
	return out
}

func ansiColor(code, text string) string {
	return "\x1b[" + code + "m" + text + "\x1b[39m"
}

func applyRowBg(row, bgCode string) string {
	row = strings.ReplaceAll(row, "\x1b[0m", "\x1b[0m"+bgCode)
	row = strings.ReplaceAll(row, "\x1b[m", "\x1b[m"+bgCode)
	row = strings.ReplaceAll(row, "\x1b[49m", bgCode)
	row = strings.ReplaceAll(row, "\x1b[39;49m", "\x1b[39m"+bgCode)
	row = strings.ReplaceAll(row, "\x1b[49;39m", "\x1b[39m"+bgCode)
	return bgCode + row + "\x1b[0m"
}

func visibleWidth(s string) int {
	count := 0
	inEscape := false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				inEscape = false
			}
			continue
		}
		count++
	}
	return count
}

func truncEllipsis(s string, maxWidth int) string {
	runes := []rune(s)
	if len(runes) <= maxWidth {
		return s
	}
	if maxWidth <= 3 {
		return string(runes[:maxWidth])
	}
	return string(runes[:maxWidth-3]) + "..."
}

func truncPlain(s string, maxWidth int) string {
	runes := []rune(s)
	if len(runes) <= maxWidth {
		return s
	}
	if maxWidth <= 0 {
		return ""
	}
	return string(runes[:maxWidth])
}
