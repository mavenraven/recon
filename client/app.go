package client

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"grecon/server"
)

type RowKind int

const (
	RowHeader RowKind = iota
	RowAgent
	RowSubagent
	RowWakeup
)

type DisplayRow struct {
	Kind        RowKind
	Session     *server.Session
	Subagent    *server.Subagent
	Wakeup      *server.Wakeup
	Header      string
	IsLast      bool
	AgentIsLast bool
}

type App struct {
	Sessions     []*server.Session
	Selected     int
	ShouldQuit   bool
	SwitchTarget string
	Tick         uint64
	FilterActive bool
	FilterText   string
	FilterCursor int

	mu       sync.Mutex
	latest   []*server.Session
	hasNew   bool
	stopChan chan struct{}
}

func NewApp() *App {
	return &App{
		stopChan: make(chan struct{}),
	}
}

func (a *App) StartBackgroundRefresh() {
	ch := server.Subscribe(a.stopChan)
	go func() {
		for sessions := range ch {
			a.mu.Lock()
			a.latest = sessions
			a.hasNew = true
			a.mu.Unlock()
		}
	}()
}

func (a *App) StopBackgroundRefresh() {
	select {
	case <-a.stopChan:
	default:
		close(a.stopChan)
	}
}

func (a *App) TryReceive() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hasNew {
		a.Sessions = a.latest
		a.hasNew = false
		a.clampSelection()
	}
}

func (a *App) Refresh() error {
	sessions, err := server.RequireFetch()
	if err != nil {
		return err
	}
	a.Sessions = sessions
	a.clampSelection()
	return nil
}

func (a *App) AdvanceTick() {
	a.Tick++
}

func buildDisplayRows(sessions []*server.Session) []DisplayRow {
	type group struct {
		name     string
		sessions []*server.Session
	}

	var groups []group
	seen := make(map[string]int)

	for _, s := range sessions {
		name := s.TmuxSession
		if name == "" {
			name = "—"
		}
		if idx, ok := seen[name]; ok {
			groups[idx].sessions = append(groups[idx].sessions, s)
		} else {
			seen[name] = len(groups)
			groups = append(groups, group{name: name, sessions: []*server.Session{s}})
		}
	}

	var rows []DisplayRow
	for _, g := range groups {
		rows = append(rows, DisplayRow{Kind: RowHeader, Header: g.name})
		for i, s := range g.sessions {
			lastAgent := i == len(g.sessions)-1
			rows = append(rows, DisplayRow{
				Kind: RowAgent, Session: s,
				IsLast: lastAgent,
			})
			hasWakeup := s.Wakeup != nil && s.Wakeup.FiresAt.After(time.Now())
			for j, sa := range s.Subagents {
				rows = append(rows, DisplayRow{
					Kind: RowSubagent, Session: s, Subagent: sa,
					IsLast:      j == len(s.Subagents)-1 && !hasWakeup,
					AgentIsLast: lastAgent,
				})
			}
			if hasWakeup {
				rows = append(rows, DisplayRow{
					Kind: RowWakeup, Session: s, Wakeup: s.Wakeup,
					IsLast:      true,
					AgentIsLast: lastAgent,
				})
			}
		}
	}
	return rows
}

func (a *App) filteredSessions() []*server.Session {
	if a.FilterText == "" {
		return a.Sessions
	}
	query := strings.ToLower(a.FilterText)
	var result []*server.Session
	for _, s := range a.Sessions {
		if strings.Contains(strings.ToLower(s.ProjectName), query) ||
			strings.Contains(strings.ToLower(s.TmuxSession), query) {
			result = append(result, s)
		}
	}
	return result
}

func (a *App) DisplayRows() []DisplayRow {
	return buildDisplayRows(a.filteredSessions())
}

func (a *App) SelectableCount() int {
	count := 0
	for _, r := range a.DisplayRows() {
		if r.Kind == RowAgent {
			count++
		}
	}
	return count
}

func (a *App) SelectedSession() *server.Session {
	idx := 0
	for _, r := range a.DisplayRows() {
		if r.Kind == RowAgent {
			if idx == a.Selected {
				return r.Session
			}
			idx++
		}
	}
	return nil
}

func (a *App) clampSelection() {
	count := a.SelectableCount()
	if count == 0 {
		a.Selected = 0
	} else if a.Selected >= count {
		a.Selected = count - 1
	}
}

func (a *App) HandleKey(code string, ctrl bool) {
	if a.FilterActive {
		a.handleKeyFilter(code, ctrl)
		return
	}
	if code == "tab" || code == "i" {
		a.jumpToNextInput()
		return
	}
	a.handleKeyTable(code, ctrl)
}

func (a *App) jumpToNextInput() {
	for _, s := range a.Sessions {
		if s.Status == server.StatusInput && s.PaneTarget != "" {
			a.SwitchTarget = s.PaneTarget
			a.ShouldQuit = true
			return
		}
	}
}

func (a *App) handleKeyTable(code string, ctrl bool) {
	switch code {
	case "q":
		a.ShouldQuit = true
	case "esc":
		if a.FilterText != "" {
			a.FilterText = ""
			a.Selected = 0
		} else {
			a.ShouldQuit = true
		}
	case "/":
		a.FilterActive = true
		a.FilterText = ""
		a.FilterCursor = 0
		a.Selected = 0
	case "j", "down":
		count := a.SelectableCount()
		if count > 0 && a.Selected+1 < count {
			a.Selected++
		}
	case "k", "up":
		if a.Selected > 0 {
			a.Selected--
		}
	case "enter":
		if s := a.SelectedSession(); s != nil {
			if s.PaneTarget != "" {
				a.SwitchTarget = s.PaneTarget
				a.ShouldQuit = true
			}
		}
	case "x":
		if s := a.SelectedSession(); s != nil {
			if s.TmuxSession != "" {
				KillSession(s.TmuxSession)
			}
		}
	}
}

func (a *App) handleKeyFilter(code string, ctrl bool) {
	switch {
	case code == "esc":
		a.FilterActive = false
		a.FilterText = ""
		a.FilterCursor = 0
		a.Selected = 0
	case code == "enter":
		if a.SelectableCount() == 1 {
			if s := a.SelectedSession(); s != nil && s.PaneTarget != "" {
				a.SwitchTarget = s.PaneTarget
				a.ShouldQuit = true
				return
			}
		}
		a.FilterActive = false
	case code == "backspace":
		runes := []rune(a.FilterText)
		if a.FilterCursor > 0 && a.FilterCursor <= len(runes) {
			runes = append(runes[:a.FilterCursor-1], runes[a.FilterCursor:]...)
			a.FilterText = string(runes)
			a.FilterCursor--
			a.clampSelection()
		}
	case code == "delete":
		runes := []rune(a.FilterText)
		if a.FilterCursor < len(runes) {
			runes = append(runes[:a.FilterCursor], runes[a.FilterCursor+1:]...)
			a.FilterText = string(runes)
			a.clampSelection()
		}
	case code == "left":
		if a.FilterCursor > 0 {
			a.FilterCursor--
		}
	case code == "right":
		runes := []rune(a.FilterText)
		if a.FilterCursor < len(runes) {
			a.FilterCursor++
		}
	case code == "home" || (ctrl && code == "a"):
		a.FilterCursor = 0
	case code == "end" || (ctrl && code == "e"):
		a.FilterCursor = len([]rune(a.FilterText))
	case ctrl && code == "u":
		a.FilterText = ""
		a.FilterCursor = 0
		a.clampSelection()
	case code == "down" || code == "j":
		count := a.SelectableCount()
		if count > 0 && a.Selected+1 < count {
			a.Selected++
		}
	case code == "up" || code == "k":
		if a.Selected > 0 {
			a.Selected--
		}
	case code == "tab" || code == "i":
		a.jumpToNextInput()
	case len(code) == 1 && !ctrl:
		runes := []rune(a.FilterText)
		ch := []rune(code)[0]
		newRunes := make([]rune, 0, len(runes)+1)
		newRunes = append(newRunes, runes[:a.FilterCursor]...)
		newRunes = append(newRunes, ch)
		newRunes = append(newRunes, runes[a.FilterCursor:]...)
		a.FilterText = string(newRunes)
		a.FilterCursor++
		a.clampSelection()
	}
}

func ShortenHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

func FormatTimestamp(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse(time.RFC3339, ts)
		if err != nil {
			return ts
		}
	}
	diff := time.Since(t)

	if diff.Seconds() < 60 {
		return "< 1m"
	} else if diff.Minutes() < 60 {
		return fmt.Sprintf("%dm ago", int(diff.Minutes()))
	} else if diff.Hours() < 24 {
		return fmt.Sprintf("%dh ago", int(diff.Hours()))
	}
	return t.Local().Format("Jan 02 15:04")
}
