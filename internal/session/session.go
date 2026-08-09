package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"bruce-go/internal/config"
	"bruce-go/internal/llm"
	"bruce-go/internal/runtime"
)

const CurrentVersion = 1

type Header struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	ID        string `json:"id"`
	CreatedAt string `json:"createdAt"`
	CWD       string `json:"cwd"`
}

type Entry struct {
	Type             string             `json:"type"`
	ID               string             `json:"id,omitempty"`
	ParentID         string             `json:"parentId,omitempty"`
	Timestamp        string             `json:"timestamp,omitempty"`
	Message          *llm.Message       `json:"message,omitempty"`
	Mode             runtime.AgentMode  `json:"mode,omitempty"`
	TargetID         string             `json:"targetId,omitempty"`
	CustomType       string             `json:"customType,omitempty"`
	Data             any                `json:"data,omitempty"`
	Content          string             `json:"content,omitempty"`
	Display          *bool              `json:"display,omitempty"`
	Details          any                `json:"details,omitempty"`
	Name             string             `json:"name,omitempty"`
	Summary          string             `json:"summary,omitempty"`
	FirstKeptEntryID string             `json:"firstKeptEntryId,omitempty"`
	TokensBefore     int                `json:"tokensBefore,omitempty"`
	Plan             *runtime.PlanEvent `json:"plan,omitempty"`
}

const (
	TypeMessage       = "message"
	TypeModeChange    = "mode_change"
	TypeLeafChange    = "leaf_change"
	TypeCustom        = "custom"
	TypeCustomMessage = "custom_message"
	TypeSessionInfo   = "session_info"
	TypeCompaction    = "compaction"
	TypePlanEvent     = "plan_event"
)

func (e Entry) BranchNode() bool {
	switch e.Type {
	case TypeMessage, TypeModeChange, TypeCustom, TypeCustomMessage, TypeSessionInfo, TypeCompaction, TypePlanEvent:
		return true
	default:
		return false
	}
}

type Context struct {
	SessionID    string
	File         string
	ActiveLeaf   string
	Mode         runtime.AgentMode
	MessageCount int
	Messages     []llm.Message
	Entries      []Entry
	ActivePlan   runtime.PlanState
}

type Summary struct {
	ID           string
	File         string
	CreatedAt    string
	UpdatedAt    time.Time
	Mode         runtime.AgentMode
	ActiveLeafID string
	MessageCount int
	ActivePlan   runtime.PlanState
}

type Store struct {
	mu         sync.Mutex
	HomeDir    string
	Workspace  string
	Directory  string
	File       string
	Header     Header
	Entries    []Entry
	ActiveLeaf string
}

func NewStore(homeDir, workspace string) *Store {
	homeDir = cleanOrHome(homeDir)
	workspace = absClean(workspace)
	return &Store{HomeDir: homeDir, Workspace: workspace, Directory: sessionDir(homeDir, workspace)}
}

func CreateNew(homeDir, workspace string, mode runtime.AgentMode) (*Store, error) {
	s := NewStore(homeDir, workspace)
	return s, s.CreateNew(mode)
}

func (s *Store) CreateNew(mode runtime.AgentMode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Directory = sessionDir(s.HomeDir, s.Workspace)
	if err := os.MkdirAll(s.Directory, 0o755); err != nil {
		return err
	}
	id := newSessionID()
	s.File = filepath.Join(s.Directory, id+".jsonl")
	s.Header = Header{Type: "session", Version: CurrentVersion, ID: id, CreatedAt: now(), CWD: s.Workspace}
	s.Entries = nil
	s.ActiveLeaf = ""
	if err := writeJSONLineNew(s.File, s.Header); err != nil {
		return err
	}
	if mode != "" && mode != runtime.ModeReact {
		return s.appendBranchLocked(Entry{Type: TypeModeChange, ID: newEntryID(), Timestamp: now(), Mode: mode})
	}
	return nil
}

func (s *Store) Context(fallback runtime.AgentMode) Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.activePathLocked()
	return Context{SessionID: s.Header.ID, File: s.File, ActiveLeaf: s.ActiveLeaf, Mode: s.currentModeFromPathLocked(path, fallback), MessageCount: s.messageCountLocked(), Messages: s.buildMessagesFromPathLocked(path), Entries: replayEntriesFromPath(path), ActivePlan: planStateFromPath(path)}
}

func (s *Store) AppendMessage(message llm.Message) error {
	if message.Role == llm.RoleSystem {
		return nil
	}
	if message.HasImages() {
		message = message.WithoutImages()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendBranchLocked(Entry{Type: TypeMessage, ID: newEntryID(), ParentID: s.ActiveLeaf, Timestamp: now(), Message: &message})
}

func (s *Store) AppendModeChange(mode runtime.AgentMode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mode == "" || s.currentModeLocked(runtime.ModeReact) == mode {
		return nil
	}
	return s.appendBranchLocked(Entry{Type: TypeModeChange, ID: newEntryID(), ParentID: s.ActiveLeaf, Timestamp: now(), Mode: mode})
}

func (s *Store) AppendCustomMessage(customType, content string, display bool, details any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendBranchLocked(Entry{Type: TypeCustomMessage, ID: newEntryID(), ParentID: s.ActiveLeaf, Timestamp: now(), CustomType: customType, Content: content, Display: &display, Details: details})
}

func (s *Store) AppendCompaction(summary, firstKept string, tokensBefore int, details any) error {
	if strings.TrimSpace(summary) == "" || strings.TrimSpace(firstKept) == "" {
		return errors.New("compaction summary and firstKeptEntryId must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendBranchLocked(Entry{Type: TypeCompaction, ID: newEntryID(), ParentID: s.ActiveLeaf, Timestamp: now(), Summary: summary, FirstKeptEntryID: firstKept, TokensBefore: tokensBefore, Details: details})
}

func (s *Store) AppendPlanEvent(plan runtime.PlanEvent) error {
	if strings.TrimSpace(plan.ID) == "" {
		return errors.New("plan_event is missing plan ID")
	}
	if strings.TrimSpace(string(plan.Action)) == "" {
		return errors.New("plan_event is missing action")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendBranchLocked(Entry{Type: TypePlanEvent, ID: newEntryID(), ParentID: s.ActiveLeaf, Timestamp: now(), Plan: &plan})
}

func (s *Store) Resume(reference string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := s.resolveSessionFileLocked(reference)
	if err != nil {
		return err
	}
	return s.openLocked(file)
}

func (s *Store) SelectLeaf(reference string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.resolveEntryIDLocked(reference)
	if err != nil {
		return err
	}
	entry := Entry{Type: TypeLeafChange, ID: newEntryID(), ParentID: s.ActiveLeaf, Timestamp: now(), TargetID: id}
	if err := appendJSONLine(s.File, entry); err != nil {
		return err
	}
	s.Entries = append(s.Entries, entry)
	s.ActiveLeaf = id
	return nil
}

func (s *Store) List(fallback runtime.AgentMode) ([]Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked(fallback)
}

func (s *Store) RenderTree(fallback runtime.AgentMode) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var nodes []Entry
	for _, entry := range s.Entries {
		if entry.BranchNode() {
			nodes = append(nodes, entry)
		}
	}
	if len(nodes) == 0 {
		return "The current session has no message nodes."
	}
	activePath := map[string]bool{}
	for _, entry := range s.activePathLocked() {
		activePath[entry.ID] = true
	}
	children := map[string][]Entry{}
	for _, node := range nodes {
		children[node.ParentID] = append(children[node.ParentID], node)
	}
	var b strings.Builder
	b.WriteString("Session: " + s.Header.ID + "\n")
	b.WriteString("Mode: " + string(s.currentModeLocked(fallback)) + "\n")
	b.WriteString("Active leaf: " + shortID(s.ActiveLeaf) + "\n")
	var render func(parent string, depth int)
	render = func(parent string, depth int) {
		for _, child := range children[parent] {
			prefix := "- "
			if child.ID == s.ActiveLeaf {
				prefix = "* "
			} else if activePath[child.ID] {
				prefix = "> "
			}
			b.WriteString(strings.Repeat("  ", depth) + prefix + shortID(child.ID) + " " + label(child) + "\n")
			render(child.ID, depth+1)
		}
	}
	render("", 0)
	return strings.TrimSpace(b.String())
}

func (s *Store) ActiveEntries() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.activePathLocked()
	return append([]Entry(nil), path...)
}

func (s *Store) buildMessagesLocked() []llm.Message {
	return s.buildMessagesFromPathLocked(s.activePathLocked())
}

func (s *Store) buildMessagesFromPathLocked(path []Entry) []llm.Message {
	compactionIndex := -1
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].Type == TypeCompaction {
			compactionIndex = i
			break
		}
	}
	var messages []llm.Message
	if compactionIndex >= 0 {
		comp := path[compactionIndex]
		messages = append(messages, compactionMessage(comp.Summary))
		found := false
		for i := 0; i < compactionIndex; i++ {
			if path[i].ID == comp.FirstKeptEntryID {
				found = true
			}
			if found {
				appendContext(&messages, path[i])
			}
		}
		for i := compactionIndex + 1; i < len(path); i++ {
			appendContext(&messages, path[i])
		}
		return messages
	}
	for _, entry := range path {
		appendContext(&messages, entry)
	}
	return messages
}

func replayEntriesFromPath(path []Entry) []Entry {
	compactionIndex := -1
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].Type == TypeCompaction {
			compactionIndex = i
			break
		}
	}
	if compactionIndex < 0 {
		return append([]Entry(nil), path...)
	}
	out := []Entry{path[compactionIndex]}
	found := false
	for i := 0; i < compactionIndex; i++ {
		if path[i].ID == path[compactionIndex].FirstKeptEntryID {
			found = true
		}
		if found {
			out = append(out, path[i])
		}
	}
	out = append(out, path[compactionIndex+1:]...)
	return out
}

func (s *Store) openLocked(file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return errors.New("session file is empty: " + file)
	}
	var header Header
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
		return err
	}
	if header.Type != "session" || header.Version != CurrentVersion {
		return errors.New("invalid session file or unsupported session version")
	}
	var entries []Entry
	active := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return err
		}
		entries = append(entries, entry)
		if entry.BranchNode() {
			active = entry.ID
		}
		if entry.Type == TypeLeafChange && entry.TargetID != "" {
			active = entry.TargetID
		}
	}
	s.File = filepath.Clean(file)
	s.Header = header
	s.Workspace = absClean(header.CWD)
	s.Directory = sessionDir(s.HomeDir, s.Workspace)
	s.Entries = entries
	s.ActiveLeaf = active
	return scanner.Err()
}

func (s *Store) appendBranchLocked(entry Entry) error {
	if err := appendJSONLine(s.File, entry); err != nil {
		return err
	}
	s.Entries = append(s.Entries, entry)
	s.ActiveLeaf = entry.ID
	return nil
}

func (s *Store) activePathLocked() []Entry {
	byID := map[string]Entry{}
	for _, entry := range s.Entries {
		if entry.BranchNode() {
			byID[entry.ID] = entry
		}
	}
	var path []Entry
	seen := map[string]bool{}
	for cursor := s.ActiveLeaf; cursor != ""; {
		if seen[cursor] {
			break
		}
		seen[cursor] = true
		entry, ok := byID[cursor]
		if !ok {
			break
		}
		path = append([]Entry{entry}, path...)
		cursor = entry.ParentID
	}
	return path
}

func (s *Store) currentModeLocked(fallback runtime.AgentMode) runtime.AgentMode {
	return s.currentModeFromPathLocked(s.activePathLocked(), fallback)
}

func (s *Store) currentModeFromPathLocked(path []Entry, fallback runtime.AgentMode) runtime.AgentMode {
	mode := fallback
	if mode == "" {
		mode = runtime.ModeReact
	}
	for _, entry := range path {
		if entry.Type == TypeModeChange && entry.Mode != "" {
			mode = entry.Mode
		}
	}
	return mode
}

func (s *Store) messageCountLocked() int {
	count := 0
	for _, entry := range s.Entries {
		if entry.Type == TypeMessage {
			count++
		}
	}
	return count
}

func (s *Store) resolveSessionFileLocked(reference string) (string, error) {
	value := strings.TrimSpace(reference)
	if value != "" && (filepath.IsAbs(value) || strings.ContainsAny(value, `/\`)) {
		if !filepath.IsAbs(value) {
			value = filepath.Join(s.Workspace, value)
		}
		if info, err := os.Stat(value); err == nil && !info.IsDir() {
			return filepath.Clean(value), nil
		}
	}
	summaries, err := s.listLocked(runtime.ModeReact)
	if err != nil {
		return "", err
	}
	if value == "" {
		if len(summaries) == 0 {
			return "", errors.New("there are no resumable sessions")
		}
		return summaries[0].File, nil
	}
	var matches []Summary
	for _, summary := range summaries {
		if strings.HasPrefix(summary.ID, value) || strings.HasPrefix(filepath.Base(summary.File), value) {
			matches = append(matches, summary)
		}
	}
	if len(matches) == 0 {
		return "", errors.New("session not found: " + reference)
	}
	if len(matches) > 1 {
		return "", errors.New("session reference is ambiguous: " + reference)
	}
	return matches[0].File, nil
}

func (s *Store) listLocked(fallback runtime.AgentMode) ([]Summary, error) {
	var summaries []Summary
	entries, err := os.ReadDir(s.Directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		file := filepath.Join(s.Directory, entry.Name())
		reader := NewStore(s.HomeDir, s.Workspace)
		if err := reader.openLocked(file); err != nil {
			continue
		}
		info, _ := os.Stat(file)
		path := reader.activePathLocked()
		summaries = append(summaries, Summary{ID: reader.Header.ID, File: file, CreatedAt: reader.Header.CreatedAt, UpdatedAt: info.ModTime(), Mode: reader.currentModeFromPathLocked(path, fallback), ActiveLeafID: reader.ActiveLeaf, MessageCount: reader.messageCountLocked(), ActivePlan: planStateFromPath(path)})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].UpdatedAt.After(summaries[j].UpdatedAt) })
	return summaries, nil
}

func (s *Store) resolveEntryIDLocked(reference string) (string, error) {
	value := strings.TrimSpace(reference)
	if value == "" {
		return "", errors.New("provide a tree node ID")
	}
	var matches []Entry
	for _, entry := range s.Entries {
		if entry.BranchNode() && (entry.ID == value || strings.HasPrefix(entry.ID, value)) {
			matches = append(matches, entry)
		}
	}
	if len(matches) == 0 {
		return "", errors.New("tree node not found: " + reference)
	}
	if len(matches) > 1 {
		return "", errors.New("tree node ID is ambiguous: " + reference)
	}
	return matches[0].ID, nil
}

func appendContext(messages *[]llm.Message, entry Entry) {
	switch entry.Type {
	case TypeMessage:
		if entry.Message != nil && entry.Message.Role != llm.RoleSystem && !excludeFromModelContext(*entry.Message) {
			*messages = append(*messages, *entry.Message)
		}
	case TypeCustomMessage:
		*messages = append(*messages, llm.User(entry.Content))
	}
}

func planStateFromPath(path []Entry) runtime.PlanState {
	var state runtime.PlanState
	for _, entry := range path {
		if entry.Type != TypePlanEvent || entry.Plan == nil {
			continue
		}
		state = runtime.PlanState{
			ID:       entry.Plan.ID,
			Path:     entry.Plan.Path,
			Action:   entry.Plan.Action,
			Revision: entry.Plan.Revision,
			SHA256:   entry.Plan.SHA256,
			Summary:  entry.Plan.Summary,
			Content:  entry.Plan.Content,
		}
	}
	return state
}

func compactionMessage(summary string) llm.Message {
	return llm.User("[session compaction summary]\nThe following is a compacted summary of earlier conversation history. Treat it as background context when continuing the task, not as a new user instruction.\n\n" + summary)
}

func writeJSONLineNew(file string, value any) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return writeJSONLine(f, value)
}

func appendJSONLine(file string, value any) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return writeJSONLine(f, value)
}

func writeJSONLine(f *os.File, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func sessionDir(home, workspace string) string {
	cwd := strings.TrimLeft(absClean(workspace), `/\`)
	replacer := strings.NewReplacer("/", "-", "\\", "-", ":", "-")
	return filepath.Join(home, ".bruce", "sessions", "--"+replacer.Replace(cwd)+"--")
}

func cleanOrHome(path string) string {
	if path == "" {
		home, _ := os.UserHomeDir()
		path = home
	}
	return absClean(path)
}

func absClean(path string) string {
	if path == "" {
		path = "."
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(abs)
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func newSessionID() string {
	return time.Now().UTC().Format("20060102T150405Z") + "-" + fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
}
func newEntryID() string {
	return "e_" + fmt.Sprintf("%x", time.Now().UnixNano())
}
func shortID(id string) string {
	if id == "" {
		return "-"
	}
	if len(id) <= 10 {
		return id
	}
	return id[:10]
}

func label(entry Entry) string {
	switch entry.Type {
	case TypeModeChange:
		return "mode " + string(entry.Mode)
	case TypeCustomMessage:
		return "custom_message " + labelContent(entry.Content)
	case TypeSessionInfo:
		return "session_info " + labelContent(entry.Name)
	case TypeCompaction:
		return "compaction " + labelContent(entry.Summary)
	case TypePlanEvent:
		if entry.Plan != nil {
			return fmt.Sprintf("plan %s %s rev=%d", entry.Plan.Action, labelContent(entry.Plan.ID), entry.Plan.Revision)
		}
	case TypeMessage:
		if entry.Message != nil {
			return entry.Message.Role + " " + labelContent(entry.Message.Content)
		}
	}
	return entry.Type
}

func labelContent(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 48 {
		return value[:45] + "..."
	}
	return value
}

type CompactionResult struct {
	Summary          string
	FirstKeptEntryID string
	TokensBefore     int
	TokensAfter      int
	Details          Details
}

type Details struct {
	ReadFiles     []string `json:"readFiles"`
	ModifiedFiles []string `json:"modifiedFiles"`
}

type CompactionPreparation struct {
	FirstKeptEntryID    string
	MessagesToSummarize []llm.Message
	TurnPrefixMessages  []llm.Message
	IsSplitTurn         bool
	TokensBefore        int
	PreviousSummary     string
	Details             Details
	Settings            config.Compaction
}

type ContextUsageEstimate struct {
	Tokens         int
	UsageTokens    int
	TrailingTokens int
	LastUsageIndex int
}

func PrepareCompaction(entries []Entry, settings config.Compaction) (*CompactionPreparation, bool) {
	if len(entries) == 0 || entries[len(entries)-1].Type == TypeCompaction {
		return nil, false
	}
	if settings.ReserveTokens <= 0 {
		settings.ReserveTokens = 16384
	}
	if settings.KeepRecentTokens <= 0 {
		settings.KeepRecentTokens = 20000
	}

	previousIndex := latestCompactionIndex(entries)
	boundaryStart := 0
	previousSummary := ""
	if previousIndex >= 0 {
		previous := entries[previousIndex]
		previousSummary = previous.Summary
		boundaryStart = indexOfEntry(entries, previous.FirstKeptEntryID)
		if boundaryStart < 0 {
			boundaryStart = previousIndex + 1
		}
	}

	cut, turnStart, split := findCutPoint(entries, boundaryStart, len(entries), settings.KeepRecentTokens)
	if cut < boundaryStart || cut >= len(entries) || strings.TrimSpace(entries[cut].ID) == "" {
		return nil, false
	}
	historyEnd := cut
	if split {
		historyEnd = turnStart
	}
	messages := messagesFromRange(entries, boundaryStart, historyEnd)
	var turnPrefix []llm.Message
	if split {
		turnPrefix = messagesFromRange(entries, turnStart, cut)
	}
	if len(messages) == 0 && len(turnPrefix) == 0 {
		return nil, false
	}

	fileOps := newFileOperations()
	if previousIndex >= 0 {
		fileOps.merge(detailsFromAny(entries[previousIndex].Details))
	}
	for _, message := range append(append([]llm.Message(nil), messages...), turnPrefix...) {
		fileOps.extract(message)
	}
	contextMessages := buildContextMessages(entries)

	return &CompactionPreparation{
		FirstKeptEntryID:    entries[cut].ID,
		MessagesToSummarize: messages,
		TurnPrefixMessages:  turnPrefix,
		IsSplitTurn:         split,
		TokensBefore:        EstimateContextTokens(contextMessages).Tokens,
		PreviousSummary:     previousSummary,
		Details:             fileOps.details(),
		Settings:            settings,
	}, true
}

func Compact(ctx context.Context, client llm.ChatClient, preparation CompactionPreparation, custom string) (CompactionResult, error) {
	historySummary := ""
	turnSummary := ""
	if preparation.IsSplitTurn && len(preparation.TurnPrefixMessages) > 0 {
		type summaryResult struct {
			kind    string
			summary string
			err     error
		}
		count := 1
		needsHistorySummary := len(preparation.MessagesToSummarize) > 0 || preparation.PreviousSummary != ""
		if needsHistorySummary {
			count++
		}
		results := make(chan summaryResult, count)
		if needsHistorySummary {
			go func() {
				summary, err := generateHistorySummary(ctx, client, preparation, custom)
				results <- summaryResult{kind: "history", summary: summary, err: err}
			}()
		} else {
			historySummary = "No earlier history."
		}
		go func() {
			summary, err := generateTurnPrefixSummary(ctx, client, preparation)
			results <- summaryResult{kind: "turn", summary: summary, err: err}
		}()
		for i := 0; i < count; i++ {
			result := <-results
			if result.err != nil {
				return CompactionResult{}, result.err
			}
			if result.kind == "history" {
				historySummary = result.summary
			} else {
				turnSummary = result.summary
			}
		}
	} else {
		var err error
		historySummary, err = generateHistorySummary(ctx, client, preparation, custom)
		if err != nil {
			return CompactionResult{}, err
		}
	}

	summary := historySummary
	if turnSummary != "" {
		summary += "\n\n---\n\n## Current Oversized-Turn Context\n\n" + turnSummary
	}
	summary += formatFileDetails(preparation.Details)
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return CompactionResult{}, errors.New("compaction summary is empty")
	}
	return CompactionResult{
		Summary:          summary,
		FirstKeptEntryID: preparation.FirstKeptEntryID,
		TokensBefore:     preparation.TokensBefore,
		TokensAfter:      EstimateTokens(compactionMessage(summary)),
		Details:          preparation.Details,
	}, nil
}

func generateHistorySummary(ctx context.Context, client llm.ChatClient, preparation CompactionPreparation, custom string) (string, error) {
	prompt := initialSummaryPrompt
	if preparation.PreviousSummary != "" {
		prompt = updateSummaryPrompt
	}
	if strings.TrimSpace(custom) != "" {
		prompt += "\n\nAdditional focus requirements: " + strings.TrimSpace(custom)
	}
	content := "<conversation>\n" + SerializeConversation(preparation.MessagesToSummarize) + "\n</conversation>\n\n"
	if preparation.PreviousSummary != "" {
		content += "<previous-summary>\n" + preparation.PreviousSummary + "\n</previous-summary>\n\n"
	}
	return requestSummary(ctx, client, content+prompt, summaryTokenLimit(client, preparation.Settings.ReserveTokens, 0.8))
}

func generateTurnPrefixSummary(ctx context.Context, client llm.ChatClient, preparation CompactionPreparation) (string, error) {
	content := "<conversation>\n" + SerializeConversation(preparation.TurnPrefixMessages) + "\n</conversation>\n\n" + turnPrefixSummaryPrompt
	return requestSummary(ctx, client, content, summaryTokenLimit(client, preparation.Settings.ReserveTokens, 0.5))
}

func requestSummary(ctx context.Context, client llm.ChatClient, prompt string, maxTokens int) (string, error) {
	response, err := client.Chat(ctx, []llm.Message{
		llm.System("You are a context-summarization assistant. Do not continue the conversation or answer any question from it. Output only the structured summary in the requested format."),
		llm.User(prompt),
	}, nil, llm.StreamOptions{MaxTokens: maxTokens})
	if err != nil {
		return "", fmt.Errorf("failed to generate compaction summary: %w", err)
	}
	if strings.EqualFold(response.FinishReason, "error") {
		return "", errors.New("failed to generate compaction summary: model returned an error finish reason")
	}
	summary := strings.TrimSpace(response.Content)
	if summary == "" {
		return "", errors.New("failed to generate compaction summary: model returned empty content")
	}
	return summary, nil
}

func summaryTokenLimit(client llm.ChatClient, reserveTokens int, ratio float64) int {
	limit := int(float64(reserveTokens) * ratio)
	if modelLimit := client.MaxOutputTokens(); modelLimit > 0 && (limit <= 0 || modelLimit < limit) {
		limit = modelLimit
	}
	return limit
}

const initialSummaryPrompt = `The messages above are a conversation that must be compacted. Create a structured context checkpoint that another model can use to continue the work. Use exactly this format:

## Objectives
[What the user is trying to accomplish; include multiple objectives when applicable]

## Constraints and Preferences
- [Constraints, preferences, and requirements stated by the user; write "None" if there are none]

## Progress
### Completed
- [x] [Completed work]

### In Progress
- [ ] [Current work]

### Blocked
- [Blocking issue; write "None" if there are no blockers]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered follow-up action]

## Critical Context
- [Data, examples, or references required to continue the work; write "None" if there is none]

Be concise. Preserve exact file paths, function names, and error messages.`

const updateSummaryPrompt = `The messages above are new conversation content that must be merged into <previous-summary>. Update the existing structured summary: retain information that is still valid; incorporate new progress, decisions, and context; move finished work from "In Progress" to "Completed"; and update the next steps. Preserve exact file paths, function names, and error messages.

Use exactly this section structure:

## Objectives

## Constraints and Preferences

## Progress
### Completed
### In Progress
### Blocked

## Key Decisions

## Next Steps

## Critical Context`

const turnPrefixSummaryPrompt = `This is the earlier portion of a turn that was split because it became too long. The newer portion will remain in context. Summarize the earlier portion using exactly this format:

## Original Request
[What the user requested in this turn]

## Early Progress
- [Work completed and key decisions made in the earlier portion]

## Context Required for the Remaining Portion
- [Information required to understand the retained newer portion]

Be concise.`

func SerializeConversation(messages []llm.Message) string {
	var parts []string
	for _, msg := range messages {
		switch msg.Role {
		case llm.RoleUser:
			parts = append(parts, "[User]: "+msg.Content)
		case llm.RoleAssistant:
			if msg.ReasoningContent != "" {
				parts = append(parts, "[Assistant reasoning]: "+msg.ReasoningContent)
			}
			if msg.Content != "" {
				parts = append(parts, "[Assistant]: "+msg.Content)
			}
			if len(msg.ToolCalls) > 0 {
				calls := make([]string, 0, len(msg.ToolCalls))
				for _, call := range msg.ToolCalls {
					calls = append(calls, call.Function.Name+"("+call.Function.Arguments+")")
				}
				parts = append(parts, "[Assistant tool calls]: "+strings.Join(calls, "; "))
			}
		case llm.RoleTool:
			parts = append(parts, "[Tool result]: "+truncate(msg.Content, 2000))
		}
	}
	return strings.Join(parts, "\n\n")
}

func EstimateMessagesTokens(messages []llm.Message) int {
	total := 0
	for _, msg := range messages {
		total += EstimateTokens(msg)
	}
	return total
}

func EstimateContextTokens(messages []llm.Message) ContextUsageEstimate {
	lastUsage := -1
	usageTokens := 0
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		if hasValidAssistantUsage(message) {
			lastUsage = i
			usageTokens = message.TotalUsageTokens()
			break
		}
	}
	if lastUsage < 0 {
		tokens := EstimateMessagesTokens(messages)
		return ContextUsageEstimate{Tokens: tokens, TrailingTokens: tokens, LastUsageIndex: -1}
	}
	trailing := EstimateMessagesTokens(messages[lastUsage+1:])
	return ContextUsageEstimate{Tokens: usageTokens + trailing, UsageTokens: usageTokens, TrailingTokens: trailing, LastUsageIndex: lastUsage}
}

func hasValidAssistantUsage(message llm.Message) bool {
	if message.Role != llm.RoleAssistant || message.TotalUsageTokens() <= 0 {
		return false
	}
	finishReason := strings.ToLower(strings.TrimSpace(message.FinishReason))
	return finishReason != "aborted" && finishReason != "error"
}

func ShouldCompact(contextTokens, contextWindow int, settings config.Compaction) bool {
	if !settings.Enabled || contextWindow <= 0 {
		return false
	}
	threshold, err := settings.Threshold(contextWindow)
	return err == nil && contextTokens > threshold
}

func EstimateTokens(msg llm.Message) int {
	chars := len([]rune(msg.Content)) + len([]rune(msg.ReasoningContent))
	if len(msg.ContentParts) > 0 {
		chars = len([]rune(msg.ReasoningContent))
		for _, part := range msg.ContentParts {
			if part.Type == llm.ContentImageURL {
				chars += 4800
			} else {
				chars += len([]rune(part.Text))
			}
		}
	}
	for _, call := range msg.ToolCalls {
		chars += len([]rune(call.Function.Name)) + len([]rune(call.Function.Arguments))
	}
	tokens := (chars + 3) / 4
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

func entriesToMessages(entries []Entry) []llm.Message {
	var messages []llm.Message
	for _, entry := range entries {
		if msg := messageFromEntry(entry); msg != nil {
			messages = append(messages, *msg)
		}
	}
	return messages
}

func messageFromEntry(entry Entry) *llm.Message {
	switch entry.Type {
	case TypeMessage:
		if entry.Message != nil && excludeFromModelContext(*entry.Message) {
			return nil
		}
		return entry.Message
	case TypeCustomMessage:
		msg := llm.User(entry.Content)
		return &msg
	default:
		return nil
	}
}

func excludeFromModelContext(message llm.Message) bool {
	return message.Role == llm.RoleAssistant && strings.EqualFold(message.FinishReason, "length") &&
		message.OutputTokens == 0 && message.InputTokens+message.CachedInputTokens > 0 &&
		strings.TrimSpace(message.Content) == "" && len(message.ToolCalls) == 0
}

func truncate(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max]) + fmt.Sprintf("\n\n[... %d characters omitted]", len(runes)-max)
}

func latestCompactionIndex(entries []Entry) int {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type == TypeCompaction {
			return i
		}
	}
	return -1
}

func indexOfEntry(entries []Entry, id string) int {
	for i := range entries {
		if entries[i].ID == id {
			return i
		}
	}
	return -1
}

func findCutPoint(entries []Entry, start, end, keepRecentTokens int) (cut, turnStart int, split bool) {
	valid := make([]int, 0)
	for i := start; i < end; i++ {
		if isValidCutEntry(entries[i]) {
			valid = append(valid, i)
		}
	}
	if len(valid) == 0 {
		return start, -1, false
	}
	cut = valid[0]
	accumulated := 0
	for i := end - 1; i >= start; i-- {
		message := messageFromEntry(entries[i])
		if message == nil {
			continue
		}
		accumulated += EstimateTokens(*message)
		if accumulated >= keepRecentTokens {
			for _, candidate := range valid {
				if candidate >= i {
					cut = candidate
					break
				}
			}
			break
		}
	}
	if isTurnStartEntry(entries[cut]) {
		return cut, -1, false
	}
	turnStart = findTurnStart(entries, cut, start)
	return cut, turnStart, turnStart >= 0
}

func isValidCutEntry(entry Entry) bool {
	if entry.Type == TypeCustomMessage {
		return true
	}
	return entry.Type == TypeMessage && entry.Message != nil && (entry.Message.Role == llm.RoleUser || entry.Message.Role == llm.RoleAssistant)
}

func isTurnStartEntry(entry Entry) bool {
	return entry.Type == TypeCustomMessage || (entry.Type == TypeMessage && entry.Message != nil && entry.Message.Role == llm.RoleUser)
}

func findTurnStart(entries []Entry, from, start int) int {
	for i := from; i >= start; i-- {
		if isTurnStartEntry(entries[i]) {
			return i
		}
	}
	return -1
}

func messagesFromRange(entries []Entry, start, end int) []llm.Message {
	var messages []llm.Message
	for i := start; i < end; i++ {
		if message := messageFromEntry(entries[i]); message != nil {
			messages = append(messages, *message)
		}
	}
	return messages
}

func buildContextMessages(entries []Entry) []llm.Message {
	compactionIndex := latestCompactionIndex(entries)
	if compactionIndex < 0 {
		return entriesToMessages(entries)
	}
	messages := []llm.Message{compactionMessage(entries[compactionIndex].Summary)}
	firstKept := indexOfEntry(entries[:compactionIndex], entries[compactionIndex].FirstKeptEntryID)
	if firstKept >= 0 {
		messages = append(messages, entriesToMessages(entries[firstKept:compactionIndex])...)
	}
	messages = append(messages, entriesToMessages(entries[compactionIndex+1:])...)
	return messages
}

type fileOperations struct {
	read    map[string]bool
	written map[string]bool
	edited  map[string]bool
}

func newFileOperations() fileOperations {
	return fileOperations{read: map[string]bool{}, written: map[string]bool{}, edited: map[string]bool{}}
}

func (f fileOperations) extract(message llm.Message) {
	if message.Role != llm.RoleAssistant {
		return
	}
	for _, call := range message.ToolCalls {
		var arguments map[string]any
		if json.Unmarshal([]byte(call.Function.Arguments), &arguments) != nil {
			continue
		}
		path, _ := arguments["path"].(string)
		if path == "" {
			continue
		}
		switch call.Function.Name {
		case "read_file":
			f.read[path] = true
		case "write_file":
			f.written[path] = true
		case "edit_file":
			f.edited[path] = true
		}
	}
}

func (f fileOperations) merge(details Details) {
	for _, path := range details.ReadFiles {
		f.read[path] = true
	}
	for _, path := range details.ModifiedFiles {
		f.edited[path] = true
	}
}

func (f fileOperations) details() Details {
	modified := map[string]bool{}
	for path := range f.written {
		modified[path] = true
	}
	for path := range f.edited {
		modified[path] = true
	}
	details := Details{}
	for path := range f.read {
		if !modified[path] {
			details.ReadFiles = append(details.ReadFiles, path)
		}
	}
	for path := range modified {
		details.ModifiedFiles = append(details.ModifiedFiles, path)
	}
	sort.Strings(details.ReadFiles)
	sort.Strings(details.ModifiedFiles)
	return details
}

func detailsFromAny(value any) Details {
	if value == nil {
		return Details{}
	}
	if details, ok := value.(Details); ok {
		return details
	}
	data, err := json.Marshal(value)
	if err != nil {
		return Details{}
	}
	var details Details
	if json.Unmarshal(data, &details) != nil {
		return Details{}
	}
	return details
}

func formatFileDetails(details Details) string {
	var sections []string
	if len(details.ReadFiles) > 0 {
		sections = append(sections, "<read-files>\n"+strings.Join(details.ReadFiles, "\n")+"\n</read-files>")
	}
	if len(details.ModifiedFiles) > 0 {
		sections = append(sections, "<modified-files>\n"+strings.Join(details.ModifiedFiles, "\n")+"\n</modified-files>")
	}
	if len(sections) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(sections, "\n\n")
}
