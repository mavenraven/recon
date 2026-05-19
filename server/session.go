package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxLineBytes = 10 * 1024 * 1024

type SessionStatus int

const (
	StatusNew SessionStatus = iota
	StatusWorking
	StatusIdle
	StatusInput
)

func (s SessionStatus) Label() string {
	switch s {
	case StatusNew:
		return "New"
	case StatusWorking:
		return "Working"
	case StatusIdle:
		return "Idle"
	case StatusInput:
		return "Input"
	default:
		return "Unknown"
	}
}

func (s SessionStatus) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Label())
}

func (s *SessionStatus) UnmarshalJSON(data []byte) error {
	var label string
	if err := json.Unmarshal(data, &label); err != nil {
		return err
	}
	switch label {
	case "New":
		*s = StatusNew
	case "Working":
		*s = StatusWorking
	case "Idle":
		*s = StatusIdle
	case "Input":
		*s = StatusInput
	}
	return nil
}

type Session struct {
	SessionID         string            `json:"session_id"`
	ProjectName       string            `json:"project_name"`
	Branch            string            `json:"branch,omitempty"`
	CWD               string            `json:"cwd"`
	RelativeDir       string            `json:"relative_dir,omitempty"`
	TmuxSession       string            `json:"tmux_session,omitempty"`
	PaneTarget        string            `json:"pane_target,omitempty"`
	Model             string            `json:"model,omitempty"`
	Effort            string            `json:"effort,omitempty"`
	TotalInputTokens  uint64            `json:"total_input_tokens"`
	TotalOutputTokens uint64            `json:"total_output_tokens"`
	Status            SessionStatus     `json:"status"`
	PID               int               `json:"pid,omitempty"`
	LastActivity      string            `json:"last_activity,omitempty"`
	StartedAt         uint64            `json:"started_at"`
	JSONLPath         string            `json:"jsonl_path"`
	LastFileSize      uint64            `json:"last_file_size"`
	Tags              map[string]string `json:"tags"`
	SubagentCount     int               `json:"subagent_count"`
	Summary           string            `json:"summary,omitempty"`
	ClaudeName        string            `json:"claude_name,omitempty"`
	Subagents         []*Subagent       `json:"subagents,omitempty"`
	Wakeup            *Wakeup           `json:"wakeup,omitempty"`
	BackgroundTasks   []*BackgroundTask          `json:"background_tasks,omitempty"`
	PendingBgCalls    map[string]*BackgroundTask `json:"-"`
}

type Subagent struct {
	AgentID     string        `json:"agent_id"`
	AgentType   string        `json:"agent_type"`
	Description string        `json:"description"`
	JSONLPath   string        `json:"jsonl_path"`
	Status      SessionStatus `json:"status"`
	Summary     string        `json:"summary,omitempty"`
}

type Wakeup struct {
	Reason  string    `json:"reason"`
	FiresAt time.Time `json:"fires_at"`
}

type BgTaskKind string

const (
	BgShell   BgTaskKind = "shell"
	BgMonitor BgTaskKind = "monitor"
)

type BackgroundTask struct {
	TaskID      string     `json:"task_id"`
	Kind        BgTaskKind `json:"kind"`
	Description string     `json:"description"`
	Command     string     `json:"command"`
	OutputPath  string     `json:"output_path,omitempty"`
	Alive       bool       `json:"alive"`
	DeadSince   time.Time  `json:"dead_since,omitempty"`
}

func (s *Session) RoomID() string {
	if s.RelativeDir != "" {
		return s.ProjectName + " › " + s.RelativeDir
	}
	return s.ProjectName
}

func (s *Session) effectiveWindow() uint64 {
	nominal := uint64(200_000)
	if s.Model != "" {
		nominal = ModelContextWindow(s.Model)
	}
	used := s.TotalInputTokens + s.TotalOutputTokens
	if used > nominal && nominal < 1_000_000 {
		return 1_000_000
	}
	return nominal
}

func (s *Session) TokenDisplay() string {
	used := s.TotalInputTokens + s.TotalOutputTokens
	window := s.effectiveWindow()
	return fmt.Sprintf("%dk / %s", used/1000, FormatWindow(window))
}

func (s *Session) TokenRatio() float64 {
	used := s.TotalInputTokens + s.TotalOutputTokens
	window := s.effectiveWindow()
	if window == 0 {
		return 0
	}
	return float64(used) / float64(window)
}

func (s *Session) ModelDisplay() string {
	if s.Model == "" {
		return "—"
	}
	return FormatModelWithEffort(s.Model, s.Effort)
}

func FormatWindow(tokens uint64) string {
	if tokens >= 1_000_000 {
		return fmt.Sprintf("%dM", tokens/1_000_000)
	}
	return fmt.Sprintf("%dk", tokens/1000)
}

func ValidateCWD(cwd string) bool {
	if !filepath.IsAbs(cwd) {
		return false
	}
	info, err := os.Stat(cwd)
	return err == nil && info.IsDir()
}

// --- Live session discovery ---

type liveSessionInfo struct {
	pid         int
	tmuxSession string
	paneTarget  string
	paneCWD     string
	startedAt   uint64
}

type sessionFileInfo struct {
	sessionID string
	startedAt uint64
}

type parsedInfo struct {
	inputTokens     uint64
	outputTokens    uint64
	model           string
	effort          string
	cwd             string
	lastActivity    string
	fileSize        uint64
	wakeup          *Wakeup
	backgroundTasks []*BackgroundTask
	pendingBgCalls  map[string]*BackgroundTask
}

// --- Status debounce ---

type statusHold struct {
	status SessionStatus
	since  time.Time
}

var (
	statusDebounceMu  sync.Mutex
	statusDebounceMap = make(map[string]statusHold)
)

const statusHoldSecs = 0.5

func debounceStatus(sessionID string, raw SessionStatus) SessionStatus {
	statusDebounceMu.Lock()
	defer statusDebounceMu.Unlock()

	now := time.Now()
	if prev, ok := statusDebounceMap[sessionID]; ok {
		if prev.status == StatusWorking && raw == StatusIdle {
			if time.Since(prev.since).Seconds() < statusHoldSecs {
				return StatusWorking
			}
		}
	}
	statusDebounceMap[sessionID] = statusHold{status: raw, since: now}
	return raw
}

// --- Git cache ---

type gitInfo struct {
	repoName    string
	relativeDir string
	branch      string
	fetchedAt   time.Time
}

var (
	gitCacheMu sync.Mutex
	gitCache   = make(map[string]gitInfo)
)

const gitCacheTTL = 30 * time.Second

func gitProjectInfo(cwd string) (projectName, relativeDir, branch string) {
	if !ValidateCWD(cwd) {
		return filepath.Base(cwd), "", ""
	}

	gitCacheMu.Lock()
	if info, ok := gitCache[cwd]; ok && time.Since(info.fetchedAt) < gitCacheTTL {
		gitCacheMu.Unlock()
		return info.repoName, info.relativeDir, info.branch
	}
	gitCacheMu.Unlock()

	projectName, relativeDir, branch = fetchGitInfoCombined(cwd)

	gitCacheMu.Lock()
	gitCache[cwd] = gitInfo{
		repoName:    projectName,
		relativeDir: relativeDir,
		branch:      branch,
		fetchedAt:   time.Now(),
	}
	gitCacheMu.Unlock()
	return
}

func fetchGitInfoCombined(cwd string) (repoName, relativeDir, branch string) {
	fallback := filepath.Base(cwd)

	out, err := exec.Command("git", "-C", cwd, "rev-parse",
		"--git-common-dir", "--show-toplevel", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return fallback, "", ""
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 3 {
		return fallback, "", ""
	}

	common := strings.TrimSpace(lines[0])
	var commonPath string
	if filepath.IsAbs(common) {
		commonPath = common
	} else {
		commonPath = filepath.Join(cwd, common)
	}
	resolved, err := filepath.EvalSymlinks(commonPath)
	if err != nil {
		resolved = commonPath
	}
	if filepath.Base(resolved) == ".git" {
		repoName = filepath.Base(filepath.Dir(resolved))
	} else {
		repoName = filepath.Base(resolved)
	}
	if repoName == "" {
		repoName = fallback
	}

	toplevel := strings.TrimSpace(lines[1])
	cwdResolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		cwdResolved = cwd
	}
	topResolved, err := filepath.EvalSymlinks(toplevel)
	if err != nil {
		topResolved = toplevel
	}
	rel, err := filepath.Rel(topResolved, cwdResolved)
	if err == nil && rel != "." && rel != "" {
		relativeDir = rel
	}

	branchStr := strings.TrimSpace(lines[2])
	if branchStr != "" && branchStr != "HEAD" {
		branch = branchStr
	}
	return
}

func DecodeProjectPath(projectDir string) string {
	name := filepath.Base(projectDir)
	if strings.HasPrefix(name, "-") {
		return strings.Replace(strings.Replace(name, "-", "/", 1), "-", "/", -1)
	}
	return name
}

// --- JSONL parsing ---

func readLineCapped(reader *bufio.Reader) (string, int, error) {
	var buf []byte
	overflowed := false
	totalConsumed := 0

	for {
		chunk, err := reader.Peek(reader.Buffered())
		if len(chunk) == 0 && err != nil {
			if err == io.EOF && totalConsumed > 0 {
				break
			}
			if totalConsumed == 0 {
				return "", 0, err
			}
			break
		}

		line, err := reader.ReadBytes('\n')
		n := len(line)
		totalConsumed += n

		if !overflowed {
			if len(buf)+n <= maxLineBytes {
				buf = append(buf, line...)
			} else {
				overflowed = true
				buf = nil
			}
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			return "", totalConsumed, err
		}
		break
	}

	if totalConsumed == 0 {
		return "", 0, io.EOF
	}
	if overflowed {
		return "", totalConsumed, nil
	}
	return string(buf), totalConsumed, nil
}

type jsonlEntry struct {
	Message   *messageEntry `json:"message,omitempty"`
	Timestamp string        `json:"timestamp,omitempty"`
	CWD       string        `json:"cwd,omitempty"`
	Type      string        `json:"type,omitempty"`
}

type messageEntry struct {
	Model string     `json:"model,omitempty"`
	Usage *usageEntry `json:"usage,omitempty"`
}

type usageEntry struct {
	InputTokens              uint64 `json:"input_tokens"`
	OutputTokens             uint64 `json:"output_tokens"`
	CacheCreationInputTokens uint64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     uint64 `json:"cache_read_input_tokens"`
}

func parseJSONL(path string, prevFileSize, prevInput, prevOutput uint64, prevModel, prevEffort, prevActivity string, prevWakeup *Wakeup, prevBgTasks []*BackgroundTask, prevPending map[string]*BackgroundTask) parsedInfo {
	f, err := os.Open(path)
	if err != nil {
		return parsedInfo{
			inputTokens: prevInput, outputTokens: prevOutput,
			model: prevModel, effort: prevEffort, lastActivity: prevActivity,
			wakeup: prevWakeup, backgroundTasks: prevBgTasks,
			pendingBgCalls: prevPending,
		}
	}
	defer f.Close()

	stat, _ := f.Stat()
	fileSize := uint64(stat.Size())

	if fileSize == prevFileSize && prevFileSize > 0 {
		return parsedInfo{
			inputTokens: prevInput, outputTokens: prevOutput,
			model: prevModel, effort: prevEffort, lastActivity: prevActivity,
			fileSize: fileSize, wakeup: prevWakeup, backgroundTasks: prevBgTasks,
			pendingBgCalls: prevPending,
		}
	}

	totalInput := prevInput
	totalOutput := prevOutput
	model := prevModel
	effort := prevEffort
	lastActivity := prevActivity
	var cwd string

	if prevFileSize > 0 {
		f.Seek(int64(prevFileSize), io.SeekStart)
	} else {
		totalInput = 0
		totalOutput = 0
		model = ""
		effort = ""
		lastActivity = ""
	}

	wakeup := prevWakeup
	bgTasks := make(map[string]*BackgroundTask)
	for _, t := range prevBgTasks {
		bgTasks[t.TaskID] = t
	}
	completedTasks := make(map[string]bool)
	pendingBgCalls := make(map[string]*BackgroundTask)
	for k, v := range prevPending {
		pendingBgCalls[k] = v
	}

	reader := bufio.NewReaderSize(f, 64*1024)
	for {
		line, _, err := readLineCapped(reader)
		if err != nil {
			break
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" || !strings.Contains(trimmed, `"type"`) {
			continue
		}

		if strings.Contains(trimmed, `"type":"assistant"`) {
			if strings.Contains(trimmed, `"<synthetic>"`) {
				continue
			}
			var entry jsonlEntry
			if json.Unmarshal([]byte(trimmed), &entry) != nil {
				continue
			}
			if entry.Timestamp != "" {
				lastActivity = entry.Timestamp
			}
			if entry.CWD != "" {
				cwd = entry.CWD
			}
			if entry.Message != nil {
				if entry.Message.Model != "" {
					model = entry.Message.Model
				}
				if u := entry.Message.Usage; u != nil {
					totalInput = u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
					totalOutput = u.OutputTokens
				}
			}
			if strings.Contains(trimmed, `"ScheduleWakeup"`) {
				if w := parseWakeup(trimmed, entry.Timestamp); w != nil {
					wakeup = w
				}
			}
			if strings.Contains(trimmed, `"Bash"`) {
				parseBgLaunch(trimmed, pendingBgCalls)
			}
			if strings.Contains(trimmed, `"Monitor"`) {
				parseMonitorLaunch(trimmed, pendingBgCalls)
			}
		} else if strings.Contains(trimmed, `"type":"user"`) || strings.Contains(trimmed, `"type":"system"`) {
			var entry jsonlEntry
			if json.Unmarshal([]byte(trimmed), &entry) != nil {
				continue
			}
			if entry.Timestamp != "" {
				lastActivity = entry.Timestamp
			}
			if entry.CWD != "" {
				cwd = entry.CWD
			}

			if strings.Contains(trimmed, "Command running in background with ID:") {
				parseBgResult(trimmed, pendingBgCalls, bgTasks)
			} else {
				cleanupPendingCalls(trimmed, pendingBgCalls)
			}
			if strings.Contains(trimmed, "Monitor started") {
				parseMonitorResult(trimmed, pendingBgCalls, bgTasks)
			}
			if strings.Contains(trimmed, "<task-notification>") {
				if tid := extractTaskNotificationID(trimmed); tid != "" {
					completedTasks[tid] = true
				}
			}

			if strings.Contains(trimmed, "<local-command-stdout>Set model to") &&
				!strings.Contains(trimmed, "toolUseResult") &&
				!strings.Contains(trimmed, "tool_result") {

				idx := strings.Index(trimmed, "<local-command-stdout>Set model to")
				tagEnd := idx + len("<local-command-stdout>Set model to")
				remainder := trimmed[tagEnd:]
				if closeIdx := strings.Index(remainder, "</local-command-stdout>"); closeIdx >= 0 {
					remainder = remainder[:closeIdx]
				}
				remainder = stripANSI(remainder)
				remainder = strings.TrimSpace(remainder)

				modelPart := remainder
				if wp := strings.Index(remainder, "with "); wp >= 0 {
					after := remainder[wp+5:]
					if ep := strings.Index(after, " effort"); ep >= 0 {
						e := strings.TrimSpace(after[:ep])
						if e != "" {
							effort = e
						}
					}
					modelPart = remainder[:wp]
				}

				modelName := strings.TrimSpace(modelPart)
				modelName = strings.TrimSuffix(modelName, "(default)")
				modelName = strings.TrimSpace(modelName)
				modelName = strings.TrimSuffix(modelName, "(1M context)")
				modelName = strings.TrimSpace(modelName)
				modelName = strings.TrimSuffix(modelName, "(200k context)")
				modelName = strings.TrimSpace(modelName)

				if id, ok := ModelIDFromDisplayName(modelName); ok {
					model = id
				}
			}
		}
	}

	if wakeup != nil && wakeup.FiresAt.Before(time.Now()) {
		wakeup = nil
	}

	var activeBg []*BackgroundTask
	for _, t := range bgTasks {
		if !completedTasks[t.TaskID] {
			activeBg = append(activeBg, t)
		}
	}

	return parsedInfo{
		inputTokens: totalInput, outputTokens: totalOutput,
		model: model, effort: effort, cwd: cwd,
		lastActivity: lastActivity, fileSize: fileSize, wakeup: wakeup,
		backgroundTasks: activeBg, pendingBgCalls: pendingBgCalls,
	}
}

func parseBgLaunch(line string, pending map[string]*BackgroundTask) {
	type bgEntry struct {
		Message struct {
			Content []struct {
				Type  string `json:"type"`
				ID    string `json:"id"`
				Name  string `json:"name"`
				Input struct {
					Command         string `json:"command"`
					Description     string `json:"description"`
					RunInBackground bool   `json:"run_in_background"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	var entry bgEntry
	if json.Unmarshal([]byte(line), &entry) != nil {
		return
	}
	for _, c := range entry.Message.Content {
		if c.Name == "Bash" && c.ID != "" && c.Input.Command != "" {
			pending[c.ID] = &BackgroundTask{
				Kind:        BgShell,
				Description: c.Input.Description,
				Command:     c.Input.Command,
			}
		}
	}
}

func cleanupPendingCalls(line string, pending map[string]*BackgroundTask) {
	type resultEntry struct {
		Message struct {
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"message"`
	}
	var entry resultEntry
	if json.Unmarshal([]byte(line), &entry) != nil {
		return
	}
	for _, c := range entry.Message.Content {
		if c.Type == "tool_result" && c.ToolUseID != "" {
			delete(pending, c.ToolUseID)
		}
	}
}

func parseBgResult(line string, pending map[string]*BackgroundTask, bgTasks map[string]*BackgroundTask) {
	type resultEntry struct {
		Message struct {
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
				Content   string `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	var entry resultEntry
	if json.Unmarshal([]byte(line), &entry) != nil {
		return
	}

	prefix := "Command running in background with ID: "
	for _, c := range entry.Message.Content {
		if c.Type != "tool_result" || c.ToolUseID == "" {
			continue
		}
		idx := strings.Index(c.Content, prefix)
		if idx < 0 {
			continue
		}
		rest := c.Content[idx+len(prefix):]
		dot := strings.IndexByte(rest, '.')
		if dot < 0 || dot > 20 {
			continue
		}
		taskID := rest[:dot]
		if !isValidTaskID(taskID) {
			continue
		}

		task := &BackgroundTask{TaskID: taskID, Kind: BgShell}
		if pathPrefix := "Output is being written to: "; true {
			if pi := strings.Index(rest, pathPrefix); pi >= 0 {
				pathStr := rest[pi+len(pathPrefix):]
				if end := strings.IndexAny(pathStr, "\"\n"); end >= 0 {
					pathStr = pathStr[:end]
				}
				task.OutputPath = strings.TrimSpace(pathStr)
			}
		}

		if p, ok := pending[c.ToolUseID]; ok {
			task.Description = p.Description
			task.Command = p.Command
			delete(pending, c.ToolUseID)
		}

		bgTasks[taskID] = task
		return
	}
}

func parseMonitorLaunch(line string, pending map[string]*BackgroundTask) {
	type monEntry struct {
		Message struct {
			Content []struct {
				Type  string `json:"type"`
				ID    string `json:"id"`
				Name  string `json:"name"`
				Input struct {
					Command     string `json:"command"`
					Description string `json:"description"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	var entry monEntry
	if json.Unmarshal([]byte(line), &entry) != nil {
		return
	}
	for _, c := range entry.Message.Content {
		if c.Name == "Monitor" && c.ID != "" {
			pending[c.ID] = &BackgroundTask{
				Kind:        BgMonitor,
				Description: c.Input.Description,
				Command:     c.Input.Command,
			}
		}
	}
}

func parseMonitorResult(line string, pending map[string]*BackgroundTask, bgTasks map[string]*BackgroundTask) {
	type resultEntry struct {
		Message struct {
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
				Content   string `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	var entry resultEntry
	if json.Unmarshal([]byte(line), &entry) != nil {
		return
	}

	prefix := "Monitor started (task "
	for _, c := range entry.Message.Content {
		if c.Type != "tool_result" || c.ToolUseID == "" {
			continue
		}
		idx := strings.Index(c.Content, prefix)
		if idx < 0 {
			continue
		}
		rest := c.Content[idx+len(prefix):]
		comma := strings.IndexByte(rest, ',')
		if comma < 0 || comma > 20 {
			continue
		}
		taskID := rest[:comma]
		if !isValidTaskID(taskID) {
			continue
		}

		task := &BackgroundTask{TaskID: taskID, Kind: BgMonitor}
		if p, ok := pending[c.ToolUseID]; ok {
			task.Description = p.Description
			task.Command = p.Command
			delete(pending, c.ToolUseID)
		}

		bgTasks[taskID] = task
		return
	}
}

func extractTaskNotificationID(line string) string {
	start := strings.Index(line, "<task-id>")
	if start < 0 {
		return ""
	}
	start += len("<task-id>")
	end := strings.Index(line[start:], "</task-id>")
	if end < 0 {
		return ""
	}
	return line[start : start+end]
}

func parseWakeup(line, timestamp string) *Wakeup {
	type wakeupEntry struct {
		Message struct {
			Content []struct {
				Name  string `json:"name"`
				Input struct {
					DelaySeconds float64 `json:"delaySeconds"`
					Reason       string  `json:"reason"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	var entry wakeupEntry
	if json.Unmarshal([]byte(line), &entry) != nil {
		return nil
	}
	for _, c := range entry.Message.Content {
		if c.Name == "ScheduleWakeup" && c.Input.DelaySeconds > 0 {
			ts, err := time.Parse(time.RFC3339Nano, timestamp)
			if err != nil {
				ts, err = time.Parse(time.RFC3339, timestamp)
				if err != nil {
					return nil
				}
			}
			return &Wakeup{
				Reason:  c.Input.Reason,
				FiresAt: ts.Add(time.Duration(c.Input.DelaySeconds) * time.Second),
			}
		}
	}
	return nil
}

func isValidTaskID(id string) bool {
	if len(id) < 5 || len(id) > 15 {
		return false
	}
	for _, r := range id {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func stripANSI(s string) string {
	var result strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if runes[i] == '\x1b' {
			for i < len(runes) && runes[i] != 'm' {
				i++
			}
		} else if runes[i] == '\\' && i+5 < len(runes) {
			rest := string(runes[i+1 : i+6])
			if rest == "u001b" || rest == "u001B" {
				i += 5
				for i < len(runes) && runes[i] != 'm' {
					i++
				}
			} else {
				result.WriteRune(runes[i])
			}
		} else {
			result.WriteRune(runes[i])
		}
	}
	return result.String()
}

// --- Session discovery ---

func DiscoverSessions(prevSessions map[string]*Session) []*Session {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	claudeDir := filepath.Join(home, ".claude", "projects")
	if _, err := os.Stat(claudeDir); err != nil {
		return nil
	}

	var paneLines string
	var pidSessionMap map[int]sessionFileInfo
	var pt *processTree

	var wgA sync.WaitGroup
	wgA.Add(3)
	go func() {
		defer wgA.Done()
		out, err := exec.Command("tmux", "list-panes", "-a", "-F",
			"#{pane_pid}|||#{session_name}|||#{pane_current_command}|||#{pane_current_path}|||#{window_index}|||#{pane_index}").Output()
		if err == nil {
			paneLines = string(out)
		}
	}()
	go func() {
		defer wgA.Done()
		pidSessionMap = readPIDSessionMap()
	}()
	go func() {
		defer wgA.Done()
		pt = buildProcessTree()
	}()
	wgA.Wait()

	claudePanes, sessionNames := processPaneLines(paneLines, pt.children)
	liveMap := buildLiveMapFromPanes(claudePanes, pidSessionMap)

	var claudeTargets []string
	for _, live := range liveMap {
		claudeTargets = append(claudeTargets, live.paneTarget)
	}

	var paneContents map[string]string
	var tmuxEnv map[string]map[string]string

	var wgB sync.WaitGroup
	wgB.Add(2)
	go func() {
		defer wgB.Done()
		paneContents = capturePanesByTarget(claudeTargets)
	}()
	go func() {
		defer wgB.Done()
		tmuxEnv = readEnvForSessions(sessionNames)
	}()
	wgB.Wait()

	candidates := make(map[string][2]string)
	entries, err := os.ReadDir(claudeDir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		projectDir := filepath.Join(claudeDir, entry.Name())
		files, err := os.ReadDir(projectDir)
		if err != nil {
			continue
		}
		for _, file := range files {
			if file.IsDir() || filepath.Ext(file.Name()) != ".jsonl" {
				continue
			}
			sessionID := strings.TrimSuffix(file.Name(), ".jsonl")
			if _, ok := liveMap[sessionID]; !ok {
				continue
			}
			path := filepath.Join(projectDir, file.Name())
			if existing, ok := candidates[sessionID]; ok {
				existingInfo, _ := os.Stat(existing[0])
				newInfo, _ := os.Stat(path)
				if existingInfo != nil && newInfo != nil && newInfo.Size() <= existingInfo.Size() {
					continue
				}
			}
			candidates[sessionID] = [2]string{path, projectDir}
		}
	}

	candidateList := make([][3]string, 0, len(candidates))
	for sid, paths := range candidates {
		candidateList = append(candidateList, [3]string{sid, paths[0], paths[1]})
	}

	results := make([]*Session, len(candidateList))
	var wg2 sync.WaitGroup
	for i, c := range candidateList {
		wg2.Add(1)
		go func(idx int, sessionID, path, projectDir string) {
			defer wg2.Done()
			live := liveMap[sessionID]
			prev := prevSessions[sessionID]

			var prevSize, prevIn, prevOut uint64
			var prevModel, prevEffort, prevAct string
			var prevWakeup *Wakeup
			var prevBgTasks []*BackgroundTask
			var prevPending map[string]*BackgroundTask
			if prev != nil {
				prevSize = prev.LastFileSize
				prevIn = prev.TotalInputTokens
				prevOut = prev.TotalOutputTokens
				prevModel = prev.Model
				prevEffort = prev.Effort
				prevAct = prev.LastActivity
				prevWakeup = prev.Wakeup
				prevBgTasks = prev.BackgroundTasks
				prevPending = prev.PendingBgCalls
			}

			info := parseJSONL(path, prevSize, prevIn, prevOut, prevModel, prevEffort, prevAct, prevWakeup, prevBgTasks, prevPending)
			markBgTaskLiveness(info.backgroundTasks, live.pid, pt)
			info.backgroundTasks = pruneStaleBgTasks(info.backgroundTasks)
			cwd := info.cwd
			if cwd == "" && prev != nil {
				cwd = prev.CWD
			}
			if cwd == "" {
				cwd = DecodeProjectPath(projectDir)
			}

			projName, relDir, branch := gitProjectInfo(cwd)
			rawStatus := determineStatus(info.inputTokens, info.outputTokens, live.paneTarget, paneContents)
			status := debounceStatus(sessionID, rawStatus)
			SaveTmuxName(sessionID, live.tmuxSession)
			saveClaudeNameFromEnv(sessionID, tmuxEnv, live.tmuxSession)
			tags := readTmuxTagsFrom(tmuxEnv, live.tmuxSession)
			subagents := discoverSubagents(path)
			claudeName := LoadClaudeName(sessionID)

			results[idx] = &Session{
				SessionID:         sessionID,
				ProjectName:       projName,
				Branch:            branch,
				CWD:               cwd,
				RelativeDir:       relDir,
				TmuxSession:       live.tmuxSession,
				PaneTarget:        live.paneTarget,
				Model:             info.model,
				Effort:            info.effort,
				TotalInputTokens:  info.inputTokens,
				TotalOutputTokens: info.outputTokens,
				Status:            status,
				PID:               live.pid,
				LastActivity:      info.lastActivity,
				StartedAt:         live.startedAt,
				JSONLPath:         path,
				LastFileSize:      info.fileSize,
				Tags:              tags,
				SubagentCount:     len(subagents),
				Subagents:         subagents,
				ClaudeName:        claudeName,
				Wakeup:            info.wakeup,
				BackgroundTasks:   info.backgroundTasks,
				PendingBgCalls:    info.pendingBgCalls,
			}
		}(i, c[0], c[1], c[2])
	}
	wg2.Wait()

	var sessions []*Session
	for _, s := range results {
		if s != nil {
			sessions = append(sessions, s)
		}
	}

	knownPIDs := make(map[int]bool)
	for _, s := range sessions {
		if s.PID != 0 {
			knownPIDs[s.PID] = true
		}
	}

	type unmatchedEntry struct {
		sessionID string
		live      *liveSessionInfo
	}
	var unmatched []unmatchedEntry
	for sid, live := range liveMap {
		if !knownPIDs[live.pid] {
			unmatched = append(unmatched, unmatchedEntry{sid, live})
		}
	}

	unmatchedResults := make([]*Session, len(unmatched))
	var wg3 sync.WaitGroup
	for i, u := range unmatched {
		wg3.Add(1)
		go func(idx int, sessionID string, live *liveSessionInfo) {
			defer wg3.Done()

			var resolvedPath string
			if !strings.HasPrefix(sessionID, "tmux-") {
				if prev, ok := prevSessions[sessionID]; ok && prev.JSONLPath != "" {
					resolvedPath = prev.JSONLPath
				}
				if resolvedPath == "" {
					resolvedPath = findJSONLForResumedSession(live.pid)
				}
			}

			if resolvedPath != "" {
				prev := prevSessions[sessionID]
				var prevSize, prevIn, prevOut uint64
				var prevModel, prevEffort, prevAct string
				var prevWakeup *Wakeup
				var prevBgTasks []*BackgroundTask
				var prevPending map[string]*BackgroundTask
				if prev != nil {
					prevSize = prev.LastFileSize
					prevIn = prev.TotalInputTokens
					prevOut = prev.TotalOutputTokens
					prevModel = prev.Model
					prevEffort = prev.Effort
					prevAct = prev.LastActivity
					prevWakeup = prev.Wakeup
					prevBgTasks = prev.BackgroundTasks
					prevPending = prev.PendingBgCalls
				}

				info := parseJSONL(resolvedPath, prevSize, prevIn, prevOut, prevModel, prevEffort, prevAct, prevWakeup, prevBgTasks, prevPending)
				markBgTaskLiveness(info.backgroundTasks, live.pid, pt)
			info.backgroundTasks = pruneStaleBgTasks(info.backgroundTasks)
				cwd := info.cwd
				if cwd == "" {
					cwd = live.paneCWD
				}
				projName, relDir, branch := gitProjectInfo(cwd)
				rawStatus := determineStatus(info.inputTokens, info.outputTokens, live.paneTarget, paneContents)
				status := debounceStatus(sessionID, rawStatus)
				SaveTmuxName(sessionID, live.tmuxSession)
				saveClaudeNameFromEnv(sessionID, tmuxEnv, live.tmuxSession)
				tags := readTmuxTagsFrom(tmuxEnv, live.tmuxSession)
				subagents := discoverSubagents(resolvedPath)
				claudeName := LoadClaudeName(sessionID)

				unmatchedResults[idx] = &Session{
					SessionID:         sessionID,
					ProjectName:       projName,
					Branch:            branch,
					CWD:               cwd,
					RelativeDir:       relDir,
					TmuxSession:       live.tmuxSession,
					PaneTarget:        live.paneTarget,
					Model:             info.model,
					Effort:            info.effort,
					TotalInputTokens:  info.inputTokens,
					TotalOutputTokens: info.outputTokens,
					Status:            status,
					PID:               live.pid,
					LastActivity:      info.lastActivity,
					StartedAt:         live.startedAt,
					JSONLPath:         resolvedPath,
					LastFileSize:      info.fileSize,
					Tags:              tags,
					SubagentCount:     len(subagents),
					Subagents:         subagents,
					ClaudeName:        claudeName,
					Wakeup:            info.wakeup,
					BackgroundTasks:   info.backgroundTasks,
					PendingBgCalls:    info.pendingBgCalls,
				}
			} else {
				SaveTmuxName(sessionID, live.tmuxSession)
				saveClaudeNameFromEnv(sessionID, tmuxEnv, live.tmuxSession)
				projName, relDir, branch := gitProjectInfo(live.paneCWD)
				tags := readTmuxTagsFrom(tmuxEnv, live.tmuxSession)
				claudeName := LoadClaudeName(sessionID)

				unmatchedResults[idx] = &Session{
					SessionID:   sessionID,
					ProjectName: projName,
					Branch:      branch,
					CWD:         live.paneCWD,
					RelativeDir: relDir,
					TmuxSession: live.tmuxSession,
					PaneTarget:  live.paneTarget,
					Status:      StatusNew,
					PID:         live.pid,
					StartedAt:   live.startedAt,
					Tags:        tags,
					ClaudeName:  claudeName,
				}
			}
		}(i, u.sessionID, u.live)
	}
	wg3.Wait()

	for _, s := range unmatchedResults {
		if s != nil {
			sessions = append(sessions, s)
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		ai := truncateToMinute(sessions[i].LastActivity)
		aj := truncateToMinute(sessions[j].LastActivity)
		if ai != aj {
			return ai > aj
		}
		return sessions[i].StartedAt > sessions[j].StartedAt
	})

	return sessions
}

func truncateToMinute(ts string) string {
	if len(ts) >= 16 {
		return ts[:16]
	}
	return ts
}

func readPIDSessionMap() map[int]sessionFileInfo {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	dir := filepath.Join(home, ".claude", "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	m := make(map[int]sessionFileInfo)
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var v map[string]interface{}
		if json.Unmarshal(data, &v) != nil {
			continue
		}
		pidF, pidOK := v["pid"].(float64)
		sid, sidOK := v["sessionId"].(string)
		if !pidOK || !sidOK {
			continue
		}
		startedAt := uint64(0)
		if sa, ok := v["startedAt"].(float64); ok {
			startedAt = uint64(sa)
		}
		m[int(pidF)] = sessionFileInfo{sessionID: sid, startedAt: startedAt}
	}
	return m
}

type processTree struct {
	children map[int][]int
	args     map[int]string
}

func buildProcessTree() *processTree {
	out, err := exec.Command("ps", "-eo", "pid,ppid,args").Output()
	if err != nil {
		return &processTree{children: nil, args: nil}
	}
	children := make(map[int][]int)
	args := make(map[int]string)
	for _, line := range strings.Split(string(out), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		children[ppid] = append(children[ppid], pid)
		if len(fields) > 2 {
			args[pid] = strings.Join(fields[2:], " ")
		}
	}
	return &processTree{children: children, args: args}
}

func (pt *processTree) descendantArgs(pid int) []string {
	if pt.children == nil {
		return nil
	}
	var result []string
	queue := pt.children[pid]
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if a, ok := pt.args[cur]; ok {
			result = append(result, a)
		}
		queue = append(queue, pt.children[cur]...)
	}
	return result
}

const bgTaskDeadTTL = 2 * time.Minute

func bgCommandFragments(cmd string) []string {
	first := cmd
	if idx := strings.IndexByte(cmd, '|'); idx >= 0 {
		first = cmd[:idx]
	}
	first = strings.TrimSpace(first)
	first = strings.TrimSuffix(first, "2>&1")
	first = strings.TrimSpace(first)
	if first == "" {
		return nil
	}
	return []string{first, cmd}
}

func markBgTaskLiveness(tasks []*BackgroundTask, sessionPID int, pt *processTree) {
	if len(tasks) == 0 || pt == nil {
		return
	}
	descArgs := pt.descendantArgs(sessionPID)
	now := time.Now()
	for _, t := range tasks {
		alive := false
		if t.Command != "" {
			frags := bgCommandFragments(t.Command)
			for _, a := range descArgs {
				for _, frag := range frags {
					if strings.Contains(a, frag) {
						alive = true
						break
					}
				}
				if alive {
					break
				}
			}
		}
		t.Alive = alive
		if alive {
			t.DeadSince = time.Time{}
		} else if t.DeadSince.IsZero() {
			t.DeadSince = now
		}
	}
}

func pruneStaleBgTasks(tasks []*BackgroundTask) []*BackgroundTask {
	now := time.Now()
	var kept []*BackgroundTask
	for _, t := range tasks {
		if !t.DeadSince.IsZero() && now.Sub(t.DeadSince) > bgTaskDeadTTL {
			continue
		}
		kept = append(kept, t)
	}
	return kept
}

func processPaneLines(paneOutput string, childrenMap map[int][]int) (claudePanes [][4]string, sessionNames []string) {
	home, _ := os.UserHomeDir()
	sessionsDir := filepath.Join(home, ".claude", "sessions")
	nameSet := make(map[string]bool)

	for _, line := range strings.Split(strings.TrimSpace(paneOutput), "\n") {
		parts := strings.SplitN(line, "|||", 6)
		if len(parts) < 6 {
			continue
		}
		pid, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		sessionName := parts[1]
		command := parts[2]
		panePath := parts[3]
		windowIdx := parts[4]
		paneIdx := parts[5]

		nameSet[sessionName] = true

		firstChar := ' '
		if len(command) > 0 {
			firstChar = rune(command[0])
		}
		isClaude := (firstChar >= '0' && firstChar <= '9') ||
			command == "claude" || command == "claude.exe" || command == "node"

		checkPID := func(p int) int {
			if fileExists(filepath.Join(sessionsDir, fmt.Sprintf("%d.json", p))) {
				return p
			}
			return findClaudeChildPID(p, sessionsDir, childrenMap)
		}

		paneTarget := fmt.Sprintf("%s:%s.%s", sessionName, windowIdx, paneIdx)

		if isClaude {
			if cpid := checkPID(pid); cpid > 0 {
				claudePanes = append(claudePanes, [4]string{
					strconv.Itoa(cpid), sessionName, paneTarget, panePath,
				})
			}
		} else if command == "bash" || command == "sh" || command == "zsh" {
			if cpid := findClaudeChildPID(pid, sessionsDir, childrenMap); cpid > 0 {
				claudePanes = append(claudePanes, [4]string{
					strconv.Itoa(cpid), sessionName, paneTarget, panePath,
				})
			}
		}
	}

	for name := range nameSet {
		sessionNames = append(sessionNames, name)
	}
	return
}

func buildLiveMapFromPanes(claudePanes [][4]string, pidSessionMap map[int]sessionFileInfo) map[string]*liveSessionInfo {
	m := make(map[string]*liveSessionInfo)
	for _, pane := range claudePanes {
		pid, _ := strconv.Atoi(pane[0])
		tmuxSession := pane[1]
		paneTarget := pane[2]
		paneCWD := pane[3]

		if info, ok := pidSessionMap[pid]; ok {
			m[info.sessionID] = &liveSessionInfo{
				pid: pid, tmuxSession: tmuxSession,
				paneTarget: paneTarget, paneCWD: paneCWD,
				startedAt: info.startedAt,
			}
		} else {
			key := fmt.Sprintf("tmux-%s", paneTarget)
			m[key] = &liveSessionInfo{
				pid: pid, tmuxSession: tmuxSession,
				paneTarget: paneTarget, paneCWD: paneCWD,
			}
		}
	}
	return m
}

func capturePanesByTarget(targets []string) map[string]string {
	type result struct {
		target  string
		content string
	}
	ch := make(chan result, len(targets))
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			out, err := exec.Command("tmux", "capture-pane", "-t", target, "-p").Output()
			if err == nil {
				ch <- result{target, string(out)}
			}
		}(t)
	}
	wg.Wait()
	close(ch)

	m := make(map[string]string)
	for r := range ch {
		m[r.target] = r.content
	}
	return m
}

func readEnvForSessions(sessionNames []string) map[string]map[string]string {
	type result struct {
		name string
		vars map[string]string
	}
	ch := make(chan result, len(sessionNames))
	var wg sync.WaitGroup
	for _, name := range sessionNames {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			out, err := exec.Command("tmux", "show-environment", "-t", n).Output()
			if err != nil {
				return
			}
			vars := make(map[string]string)
			for _, line := range strings.Split(string(out), "\n") {
				if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
					vars[k] = v
				}
			}
			ch <- result{n, vars}
		}(name)
	}
	wg.Wait()
	close(ch)

	m := make(map[string]map[string]string)
	for r := range ch {
		m[r.name] = r.vars
	}
	return m
}

func findClaudeChildPID(parentPID int, sessionsDir string, childrenMap map[int][]int) int {
	queue := []int{parentPID}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if kids, ok := childrenMap[pid]; ok {
			for _, child := range kids {
				if fileExists(filepath.Join(sessionsDir, fmt.Sprintf("%d.json", child))) {
					return child
				}
				queue = append(queue, child)
			}
		}
	}
	return 0
}

func determineStatus(inputTokens, outputTokens uint64, paneTarget string, paneContents map[string]string) SessionStatus {
	if paneTarget != "" {
		content := paneContents[paneTarget]
		pane := paneStatusFromContent(content)
		if inputTokens == 0 && outputTokens == 0 && pane == StatusIdle {
			return StatusNew
		}
		return pane
	}
	if inputTokens == 0 && outputTokens == 0 {
		return StatusNew
	}
	return StatusIdle
}

func paneStatusFromContent(content string) SessionStatus {
	lines := strings.Split(content, "\n")
	linesChecked := 0

	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}

		if linesChecked == 0 && strings.Contains(trimmed, "Esc to cancel") {
			return StatusInput
		}

		runes := []rune(trimmed)
		if len(runes) > 0 && isSpinner(runes[0]) && strings.Contains(trimmed, "…") {
			return StatusWorking
		}

		if idx := strings.Index(trimmed, "❯"); idx >= 0 {
			after := strings.TrimSpace(trimmed[idx+len("❯"):])
			if len(after) > 0 && after[0] >= '0' && after[0] <= '9' {
				return StatusInput
			}
		}

		linesChecked++
		if linesChecked >= 10 {
			break
		}
	}
	return StatusIdle
}

func isSpinner(c rune) bool {
	return (c >= '✠' && c <= '❧') || c == '⏺' || c == '·'
}

func discoverSubagents(jsonlPath string) []*Subagent {
	sessionID := strings.TrimSuffix(filepath.Base(jsonlPath), ".jsonl")
	subagentDir := filepath.Join(filepath.Dir(jsonlPath), sessionID, "subagents")
	entries, err := os.ReadDir(subagentDir)
	if err != nil {
		return nil
	}
	var subagents []*Subagent
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) >= 2*time.Minute {
			continue
		}

		agentID := strings.TrimSuffix(e.Name(), ".jsonl")
		agentID = strings.TrimPrefix(agentID, "agent-")
		jsonlFile := filepath.Join(subagentDir, e.Name())

		var status SessionStatus
		if time.Since(info.ModTime()) < 30*time.Second {
			status = StatusWorking
		} else {
			status = StatusIdle
		}

		sa := &Subagent{
			AgentID:   agentID,
			JSONLPath: jsonlFile,
			Status:    status,
		}

		metaPath := filepath.Join(subagentDir, strings.TrimSuffix(e.Name(), ".jsonl")+".meta.json")
		if metaData, err := os.ReadFile(metaPath); err == nil {
			var meta struct {
				AgentType   string `json:"agentType"`
				Description string `json:"description"`
			}
			if json.Unmarshal(metaData, &meta) == nil {
				sa.AgentType = meta.AgentType
				sa.Description = meta.Description
			}
		}

		subagents = append(subagents, sa)
	}
	return subagents
}

func reconDir(subdir string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".recon", subdir)
}

func SaveTmuxName(sessionID, tmuxName string) {
	if strings.HasPrefix(sessionID, "tmux-") {
		return
	}
	dir := reconDir("tmux-names")
	if dir == "" {
		return
	}
	path := filepath.Join(dir, sessionID)
	existing, _ := os.ReadFile(path)
	if strings.TrimSpace(string(existing)) == tmuxName {
		return
	}
	os.MkdirAll(dir, 0o755)
	os.WriteFile(path, []byte(tmuxName), 0o644)
}

func LoadTmuxName(sessionID string) string {
	dir := reconDir("tmux-names")
	if dir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, sessionID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func SaveClaudeName(sessionID, claudeName string) {
	if strings.HasPrefix(sessionID, "tmux-") {
		return
	}
	dir := reconDir("claude-names")
	if dir == "" {
		return
	}
	path := filepath.Join(dir, sessionID)
	if fileExists(path) {
		return
	}
	os.MkdirAll(dir, 0o755)
	os.WriteFile(path, []byte(claudeName), 0o644)
}

func LoadClaudeName(sessionID string) string {
	dir := reconDir("claude-names")
	if dir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, sessionID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveClaudeNameFromEnv(sessionID string, env map[string]map[string]string, tmuxSession string) {
	name := readEnvFromBatch(env, tmuxSession, "RECON_CLAUDE_NAME")
	if name != "" {
		SaveClaudeName(sessionID, name)
	}
}

func readTmuxTagsFrom(env map[string]map[string]string, sessionName string) map[string]string {
	tags := make(map[string]string)
	vars, ok := env[sessionName]
	if !ok {
		return tags
	}
	val, ok := vars["RECON_TAGS"]
	if !ok {
		return tags
	}
	for _, tag := range strings.Split(val, ",") {
		if k, v, ok := strings.Cut(tag, ":"); ok {
			tags[k] = v
		}
	}
	return tags
}

func findJSONLForResumedSession(pid int) string {
	origID := parseResumeIDFromPS(pid)
	if origID == "" {
		return ""
	}
	return findJSONLBySessionID(origID)
}

func readEnvFromBatch(env map[string]map[string]string, sessionName, varName string) string {
	if vars, ok := env[sessionName]; ok {
		return vars[varName]
	}
	return ""
}

func parseResumeIDFromPS(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	for i, f := range fields {
		if f == "--resume" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

func findJSONLBySessionID(sessionID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	projectsDir := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
	}
	var bestPath string
	var bestSize int64
	for _, entry := range entries {
		candidate := filepath.Join(projectsDir, entry.Name(), sessionID+".jsonl")
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if info.Size() > bestSize {
			bestPath = candidate
			bestSize = info.Size()
		}
	}
	return bestPath
}

func FindSessionCWD(sessionID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	projectsDir := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		jsonlPath := filepath.Join(projectsDir, entry.Name(), sessionID+".jsonl")
		f, err := os.Open(jsonlPath)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for i := 0; i < 20 && scanner.Scan(); i++ {
			var v map[string]interface{}
			if json.Unmarshal([]byte(scanner.Text()), &v) == nil {
				if cwd, ok := v["cwd"].(string); ok {
					f.Close()
					return cwd
				}
			}
		}
		f.Close()
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
