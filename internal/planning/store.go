package planning

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bruce-go/internal/runtime"
)

type EventRecorder interface {
	AppendPlanEvent(runtime.PlanEvent) error
}

type Store struct {
	homeDir  string
	plansDir string
	recorder EventRecorder
}

func NewStore(homeDir string, recorder EventRecorder) *Store {
	if strings.TrimSpace(homeDir) == "" {
		if home, err := os.UserHomeDir(); err == nil {
			homeDir = home
		}
	}
	abs, err := filepath.Abs(homeDir)
	if err != nil {
		abs = filepath.Clean(homeDir)
	}
	return &Store{
		homeDir:  filepath.Clean(abs),
		plansDir: filepath.Join(filepath.Clean(abs), ".bruce", "plans"),
		recorder: recorder,
	}
}

func (s *Store) Directory() string {
	if s == nil {
		return ""
	}
	return s.plansDir
}

func (s *Store) Read(state runtime.PlanState) (string, error) {
	path, err := s.pathForState(state)
	if err != nil {
		return "", err
	}
	if err := rejectSymlink(path); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && strings.TrimSpace(state.Content) != "" {
			return state.Content, nil
		}
		return "", err
	}
	return string(data), nil
}

func (s *Store) Replace(state runtime.PlanState, content, summary string) (runtime.PlanState, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return runtime.PlanState{}, errors.New("计划内容不能为空")
	}
	action := runtime.PlanActionUpdated
	next := state
	if strings.TrimSpace(next.ID) == "" {
		next.ID = newPlanID()
		next.Revision = 0
		action = runtime.PlanActionCreated
	}
	next.Revision++
	next.Path = s.planPath(next.ID)
	next.Action = action
	next.SHA256 = hashContent(content)
	next.Summary = strings.TrimSpace(summary)
	next.Content = content
	if err := s.writeAtomic(next.Path, content); err != nil {
		return runtime.PlanState{}, err
	}
	if err := s.record(stateToEvent(next)); err != nil {
		return runtime.PlanState{}, err
	}
	return next, nil
}

func (s *Store) Record(action runtime.PlanAction, state runtime.PlanState, content, summary string) (runtime.PlanState, error) {
	if strings.TrimSpace(state.ID) == "" {
		return runtime.PlanState{}, errors.New("当前没有 active plan")
	}
	if strings.TrimSpace(content) == "" {
		content = state.Content
	}
	next := state
	next.Action = action
	next.Summary = strings.TrimSpace(summary)
	if strings.TrimSpace(content) != "" {
		next.Content = content
		next.SHA256 = hashContent(content)
	}
	if next.Path == "" {
		next.Path = s.planPath(next.ID)
	}
	if err := s.record(stateToEvent(next)); err != nil {
		return runtime.PlanState{}, err
	}
	return next, nil
}

func (s *Store) Recover(state runtime.PlanState) runtime.PlanState {
	if s == nil || strings.TrimSpace(state.ID) == "" {
		return state
	}
	if state.Path == "" {
		state.Path = s.planPath(state.ID)
	}
	if err := rejectSymlink(state.Path); err != nil {
		state.MissingFile = true
		return state
	}
	data, err := os.ReadFile(state.Path)
	if errors.Is(err, os.ErrNotExist) {
		state.MissingFile = true
		if strings.TrimSpace(state.Content) != "" && s.writeAtomic(state.Path, state.Content) == nil {
			state.MissingFile = false
			state.RecoveredFromSnapshot = true
		}
		return state
	}
	if err != nil {
		state.MissingFile = true
		return state
	}
	if state.SHA256 != "" && hashContent(string(data)) != state.SHA256 {
		state.HashMismatch = true
	}
	return state
}

func (s *Store) pathForState(state runtime.PlanState) (string, error) {
	if strings.TrimSpace(state.ID) == "" {
		return "", errors.New("当前没有 active plan")
	}
	path := state.Path
	if path == "" {
		path = s.planPath(state.ID)
	}
	path = filepath.Clean(path)
	dir := filepath.Clean(s.plansDir)
	if path != dir && !strings.HasPrefix(path, dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("plan 路径超出计划目录: %s", path)
	}
	return path, nil
}

func (s *Store) planPath(id string) string {
	cleaned := sanitizePlanID(id)
	return filepath.Join(s.plansDir, cleaned+".md")
}

func (s *Store) writeAtomic(path, content string) error {
	if err := os.MkdirAll(s.plansDir, 0o755); err != nil {
		return err
	}
	if err := rejectSymlink(s.plansDir); err != nil {
		return err
	}
	if filepath.Dir(filepath.Clean(path)) != filepath.Clean(s.plansDir) {
		return fmt.Errorf("plan 路径必须位于计划目录下: %s", path)
	}
	if err := rejectSymlink(path); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.plansDir, "."+filepath.Base(path)+".tmp.")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func (s *Store) record(event runtime.PlanEvent) error {
	if s.recorder == nil {
		return nil
	}
	return s.recorder.AppendPlanEvent(event)
}

func stateToEvent(state runtime.PlanState) runtime.PlanEvent {
	return runtime.PlanEvent{
		ID:       state.ID,
		Path:     state.Path,
		Action:   state.Action,
		Revision: state.Revision,
		SHA256:   state.SHA256,
		Summary:  state.Summary,
		Content:  state.Content,
	}
}

func hashContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func newPlanID() string {
	return "plan_" + time.Now().UTC().Format("20060102T150405.000000000")
}

func sanitizePlanID(id string) string {
	id = strings.TrimSpace(id)
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return newPlanID()
	}
	return b.String()
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("拒绝跟随 symlink: %s", path)
	}
	return nil
}
