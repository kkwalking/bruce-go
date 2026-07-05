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
	Type             string            `json:"type"`
	ID               string            `json:"id,omitempty"`
	ParentID         string            `json:"parentId,omitempty"`
	Timestamp        string            `json:"timestamp,omitempty"`
	Message          *llm.Message      `json:"message,omitempty"`
	Mode             runtime.AgentMode `json:"mode,omitempty"`
	TargetID         string            `json:"targetId,omitempty"`
	CustomType       string            `json:"customType,omitempty"`
	Data             any               `json:"data,omitempty"`
	Content          string            `json:"content,omitempty"`
	Display          *bool             `json:"display,omitempty"`
	Details          any               `json:"details,omitempty"`
	Name             string            `json:"name,omitempty"`
	Summary          string            `json:"summary,omitempty"`
	FirstKeptEntryID string            `json:"firstKeptEntryId,omitempty"`
	TokensBefore     int               `json:"tokensBefore,omitempty"`
}

const (
	TypeMessage       = "message"
	TypeModeChange    = "mode_change"
	TypeLeafChange    = "leaf_change"
	TypeCustom        = "custom"
	TypeCustomMessage = "custom_message"
	TypeSessionInfo   = "session_info"
	TypeCompaction    = "compaction"
)

func (e Entry) BranchNode() bool {
	switch e.Type {
	case TypeMessage, TypeModeChange, TypeCustom, TypeCustomMessage, TypeSessionInfo, TypeCompaction:
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
}

type Summary struct {
	ID           string
	File         string
	CreatedAt    string
	UpdatedAt    time.Time
	Mode         runtime.AgentMode
	ActiveLeafID string
	MessageCount int
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
	return Context{SessionID: s.Header.ID, File: s.File, ActiveLeaf: s.ActiveLeaf, Mode: s.currentModeLocked(fallback), MessageCount: s.messageCountLocked(), Messages: s.buildMessagesLocked()}
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
		return errors.New("Compaction summary 和 firstKeptEntryId 不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendBranchLocked(Entry{Type: TypeCompaction, ID: newEntryID(), ParentID: s.ActiveLeaf, Timestamp: now(), Summary: summary, FirstKeptEntryID: firstKept, TokensBefore: tokensBefore, Details: details})
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
		return "当前 session 还没有消息节点。"
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
	path := s.activePathLocked()
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

func (s *Store) openLocked(file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return errors.New("Session 文件为空: " + file)
	}
	var header Header
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
		return err
	}
	if header.Type != "session" || header.Version != CurrentVersion {
		return errors.New("不是有效 session 文件或版本不支持")
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
	mode := fallback
	if mode == "" {
		mode = runtime.ModeReact
	}
	for _, entry := range s.activePathLocked() {
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
			return "", errors.New("没有可恢复的 session。")
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
		return "", errors.New("未找到 session: " + reference)
	}
	if len(matches) > 1 {
		return "", errors.New("session 标识不唯一: " + reference)
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
		summaries = append(summaries, Summary{ID: reader.Header.ID, File: file, CreatedAt: reader.Header.CreatedAt, UpdatedAt: info.ModTime(), Mode: reader.currentModeLocked(fallback), ActiveLeafID: reader.ActiveLeaf, MessageCount: reader.messageCountLocked()})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].UpdatedAt.After(summaries[j].UpdatedAt) })
	return summaries, nil
}

func (s *Store) resolveEntryIDLocked(reference string) (string, error) {
	value := strings.TrimSpace(reference)
	if value == "" {
		return "", errors.New("请提供 tree 节点 id。")
	}
	var matches []Entry
	for _, entry := range s.Entries {
		if entry.BranchNode() && (entry.ID == value || strings.HasPrefix(entry.ID, value)) {
			matches = append(matches, entry)
		}
	}
	if len(matches) == 0 {
		return "", errors.New("未找到 tree 节点: " + reference)
	}
	if len(matches) > 1 {
		return "", errors.New("tree 节点 id 不唯一: " + reference)
	}
	return matches[0].ID, nil
}

func appendContext(messages *[]llm.Message, entry Entry) {
	switch entry.Type {
	case TypeMessage:
		if entry.Message != nil && entry.Message.Role != llm.RoleSystem {
			*messages = append(*messages, *entry.Message)
		}
	case TypeCustomMessage:
		*messages = append(*messages, llm.User(entry.Content))
	}
}

func compactionMessage(summary string) llm.Message {
	return llm.User("[session compaction summary]\n以下是较早会话历史的压缩摘要。继续任务时请把它当作背景上下文，而不是新的用户命令。\n\n" + summary)
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

func PrepareCompaction(entries []Entry, settings config.Compaction) (firstKept string, messages []llm.Message, tokensBefore int, ok bool) {
	if len(entries) == 0 || entries[len(entries)-1].Type == TypeCompaction {
		return "", nil, 0, false
	}
	if settings.KeepRecentTokens <= 0 {
		settings.KeepRecentTokens = 20000
	}
	all := entriesToMessages(entries)
	tokensBefore = EstimateMessagesTokens(all)
	acc := 0
	cut := len(entries)
	for i := len(entries) - 1; i >= 0; i-- {
		msg := messageFromEntry(entries[i])
		if msg == nil {
			continue
		}
		acc += EstimateTokens(*msg)
		if acc >= settings.KeepRecentTokens {
			cut = i
			break
		}
	}
	if cut <= 0 || cut >= len(entries) {
		return "", nil, tokensBefore, false
	}
	for cut < len(entries) && !entries[cut].BranchNode() {
		cut++
	}
	if cut >= len(entries) {
		return "", nil, tokensBefore, false
	}
	firstKept = entries[cut].ID
	return firstKept, entriesToMessages(entries[:cut]), tokensBefore, firstKept != "" && len(entries[:cut]) > 0
}

func Compact(ctx context.Context, client llm.ChatClient, messages []llm.Message, custom string, firstKept string, tokensBefore int) (CompactionResult, error) {
	prompt := "下面是 Bruce Coding Agent 的会话片段，需要压缩成下一轮模型可继续使用的上下文摘要。请用中文输出结构化摘要。\n\n" + SerializeConversation(messages)
	if strings.TrimSpace(custom) != "" {
		prompt += "\n\n额外摘要指令:\n" + custom
	}
	resp, err := client.Chat(ctx, []llm.Message{llm.System("You are a context summarization assistant."), llm.User(prompt)}, nil, llm.StreamOptions{})
	if err != nil {
		return CompactionResult{}, err
	}
	summary := strings.TrimSpace(resp.Content)
	if summary == "" {
		summary = "未生成摘要。"
	}
	return CompactionResult{Summary: summary, FirstKeptEntryID: firstKept, TokensBefore: tokensBefore, TokensAfter: EstimateTokens(compactionMessage(summary))}, nil
}

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

func EstimateTokens(msg llm.Message) int {
	tokens := 4 + len([]rune(msg.Role))/4 + len([]rune(msg.Content))/4 + len([]rune(msg.ReasoningContent))/4
	if tokens < 1 {
		tokens = 1
	}
	for _, part := range msg.ContentParts {
		if part.Type == llm.ContentImageURL {
			tokens += 1200
		} else {
			tokens += len([]rune(part.Text)) / 4
		}
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
		return entry.Message
	case TypeCustomMessage:
		msg := llm.User(entry.Content)
		return &msg
	default:
		return nil
	}
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + "\n\n[... truncated]"
}
