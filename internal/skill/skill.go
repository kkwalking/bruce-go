package skill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"bruce-go/internal/tool"
)

const (
	LoadToolName     = "load_skill"
	ResourceToolName = "read_skill_resource"
)

func IsSkillTool(name string) bool {
	return name == LoadToolName || name == ResourceToolName
}

type Source string

const (
	SourceUser    Source = "USER"
	SourceProject Source = "PROJECT"
)

type Definition struct {
	Name         string
	Description  string
	Instructions string
	RootDir      string
	File         string
	Source       Source
}

type LoadResult struct {
	Skills      []Definition
	Diagnostics []string
	Overrides   []string
}

type Loader struct {
	UserHome  string
	Workspace string
}

func NewLoader(userHome, workspace string) Loader {
	return Loader{UserHome: abs(userHome), Workspace: abs(workspace)}
}

func (l Loader) Load() LoadResult {
	byName := map[string]Definition{}
	var diagnostics, overrides []string
	loadDir(filepath.Join(l.UserHome, ".bruce", "skills"), SourceUser, byName, &diagnostics, &overrides)
	loadDir(filepath.Join(l.Workspace, ".bruce", "skills"), SourceProject, byName, &diagnostics, &overrides)
	skills := make([]Definition, 0, len(byName))
	for _, skill := range byName {
		skills = append(skills, skill)
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return LoadResult{Skills: skills, Diagnostics: diagnostics, Overrides: overrides}
}

var validName = regexp.MustCompile(`^[a-z0-9._-]+$`)

func loadDir(dir string, source Source, skills map[string]Definition, diagnostics, overrides *[]string) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		*diagnostics = append(*diagnostics, dir+": 无法扫描目录: "+err.Error())
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		file := filepath.Join(dir, entry.Name(), "SKILL.md")
		def, err := parseFile(file, source)
		if err != nil {
			*diagnostics = append(*diagnostics, file+": "+err.Error())
			continue
		}
		if previous, ok := skills[def.Name]; ok {
			*overrides = append(*overrides, def.Name+": "+string(source)+" 覆盖 "+string(previous.Source))
		}
		skills[def.Name] = def
	}
}

func parseFile(file string, source Source) (Definition, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return Definition{}, err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return Definition{}, errors.New("必须以 YAML frontmatter 开头")
	}
	meta := map[string]string{}
	close := -1
	for i := 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "---" {
			close = i
			break
		}
		if idx := strings.Index(line, ":"); idx > 0 {
			meta[strings.TrimSpace(line[:idx])] = unquote(strings.TrimSpace(line[idx+1:]))
		}
	}
	if close < 0 {
		return Definition{}, errors.New("YAML frontmatter 缺少结束标记 ---")
	}
	name := strings.TrimSpace(meta["name"])
	description := strings.TrimSpace(meta["description"])
	instructions := strings.TrimSpace(strings.Join(lines[close+1:], "\n"))
	if !validName.MatchString(name) {
		return Definition{}, errors.New("name 必须匹配 [a-z0-9._-]+")
	}
	if description == "" {
		return Definition{}, errors.New("description 不能为空")
	}
	if instructions == "" {
		return Definition{}, errors.New("Skill 指令正文不能为空")
	}
	return Definition{Name: name, Description: description, Instructions: instructions, RootDir: filepath.Dir(file), File: file, Source: source}, nil
}

func unquote(value string) string {
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		return value[1 : len(value)-1]
	}
	return value
}

type Catalog struct {
	mu       sync.Mutex
	loader   Loader
	snapshot LoadResult
	active   map[string]string
}

func NewCatalog(userHome, workspace string) *Catalog {
	loader := NewLoader(userHome, workspace)
	return &Catalog{loader: loader, snapshot: loader.Load(), active: map[string]string{}}
}

func (c *Catalog) Reload() LoadResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot = c.loader.Load()
	return c.snapshot
}

func (c *Catalog) ChangeWorkspace(workspace string) LoadResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loader.Workspace = abs(workspace)
	c.snapshot = c.loader.Load()
	return c.snapshot
}

func (c *Catalog) Skills() []Definition {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Definition(nil), c.snapshot.Skills...)
}

func (c *Catalog) Diagnostics() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.snapshot.Diagnostics...)
}

func (c *Catalog) Overrides() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.snapshot.Overrides...)
}

func (c *Catalog) Find(name string) (Definition, bool) {
	for _, skill := range c.Skills() {
		if skill.Name == strings.TrimSpace(name) {
			return skill, true
		}
	}
	return Definition{}, false
}

func (c *Catalog) CatalogPrompt() string {
	skills := c.Skills()
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("可用 Skills（当前仅提供名称和描述）：\n当用户任务与某个 Skill 描述匹配时，先调用 load_skill(name) 加载完整指令，再继续任务。\n")
	for _, skill := range skills {
		line := "- " + skill.Name + ": " + skill.Description + "\n"
		if b.Len()+len(line) > 8000 {
			break
		}
		b.WriteString(line)
	}
	return strings.TrimSpace(b.String())
}

func (c *Catalog) BeginTask() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = map[string]string{}
}

func (c *Catalog) EndTask() { c.BeginTask() }

func (c *Catalog) LoadSkill(name string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing := c.active[name]; existing != "" {
		return existing, nil
	}
	if len(c.active) >= 3 {
		return "", errors.New("当前任务最多激活 3 个 Skill")
	}
	var def Definition
	found := false
	for _, skill := range c.snapshot.Skills {
		if skill.Name == name {
			def = skill
			found = true
			break
		}
	}
	if !found {
		return "", errors.New("未知 Skill: " + name)
	}
	section := "## Skill: " + def.Name + "\n描述: " + def.Description + "\n\n" + def.Instructions
	if len(section) > 24000 {
		section = section[:24000]
	}
	c.active[def.Name] = section
	return section, nil
}

func (c *Catalog) ActiveInstructions() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var values []string
	for _, v := range c.active {
		values = append(values, v)
	}
	sort.Strings(values)
	return strings.Join(values, "\n\n")
}

func (c *Catalog) ReadResource(skillName, rawPath string) (string, error) {
	c.mu.Lock()
	active := c.active[skillName] != ""
	c.mu.Unlock()
	if !active {
		return "", errors.New("Skill 未在当前任务中激活: " + skillName)
	}
	def, ok := c.Find(skillName)
	if !ok {
		return "", errors.New("未知 Skill: " + skillName)
	}
	if filepath.IsAbs(rawPath) || strings.TrimSpace(rawPath) == "" {
		return "", errors.New("资源路径必须是 Skill 内的相对路径")
	}
	rootReal, err := filepath.EvalSymlinks(def.RootDir)
	if err != nil {
		return "", err
	}
	target := filepath.Clean(filepath.Join(def.RootDir, rawPath))
	targetReal, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", errors.New("资源不存在: " + rawPath)
	}
	if !strings.HasPrefix(targetReal, rootReal+string(os.PathSeparator)) && targetReal != rootReal {
		return "", errors.New("资源路径通过符号链接逃逸 Skill 目录: " + rawPath)
	}
	data, err := os.ReadFile(targetReal)
	if err != nil {
		return "", err
	}
	text := string(data)
	if len(text) > 12000 {
		text = text[:12000] + "\n... Skill 资源过长，已截断 ..."
	}
	return text, nil
}

func RegisterTools(registry *tool.Registry, catalog *Catalog) {
	registry.Register(tool.Tool{
		Name:        LoadToolName,
		Description: "加载一个 Skill 的完整工作流指令；仅当用户任务与 Skill 描述匹配时调用",
		Parameters:  rawSchema("name", "要加载的 Skill name"),
		Exec: func(_ context.Context, args map[string]string) (string, error) {
			return catalog.LoadSkill(args["name"])
		},
	})
	registry.Register(tool.Tool{
		Name:        ResourceToolName,
		Description: "读取当前任务已加载 Skill 目录内的只读资源文件；必须先调用 load_skill",
		Parameters:  rawSchema("skill", "已激活 Skill 的 name", "path", "Skill 目录内的相对资源路径"),
		Exec: func(_ context.Context, args map[string]string) (string, error) {
			return catalog.ReadResource(args["skill"], args["path"])
		},
	})
}

func rawSchema(items ...string) []byte {
	var props []string
	var required []string
	for i := 0; i+1 < len(items); i += 2 {
		name, desc := items[i], items[i+1]
		props = append(props, `"`+name+`":{"type":"string","description":"`+desc+`"}`)
		required = append(required, `"`+name+`"`)
	}
	return []byte(`{"type":"object","properties":{` + strings.Join(props, ",") + `},"required":[` + strings.Join(required, ",") + `]}`)
}

type Invocation struct {
	Names []string
	Task  string
}

var leadingSkill = regexp.MustCompile(`^\$([a-z0-9._-]+)(?:\s+|$)`)

func ParseInvocation(input string) (Invocation, error) {
	remaining := strings.TrimSpace(input)
	var names []string
	seen := map[string]bool{}
	for {
		match := leadingSkill.FindStringSubmatchIndex(remaining)
		if match == nil {
			break
		}
		name := remaining[match[2]:match[3]]
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
			if len(names) > 3 {
				return Invocation{}, errors.New("一次最多显式指定 3 个 Skill")
			}
		}
		remaining = strings.TrimLeft(remaining[match[1]:], " \t\r\n")
	}
	if len(names) > 0 && strings.TrimSpace(remaining) == "" {
		return Invocation{}, errors.New("显式 Skill 后缺少任务内容，用法: $skill-name <任务>")
	}
	return Invocation{Names: names, Task: remaining}, nil
}

func abs(path string) string {
	if path == "" {
		path = "."
	}
	out, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(out)
}
