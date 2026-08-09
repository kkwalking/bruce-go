package instructions

import (
	"os"
	"path/filepath"
)

const MaxPromptBytes = 32768

type LoadResult struct {
	Prompt      string
	Diagnostics []string
}

func Load(userHome, workspace string) LoadResult {
	home := abs(userHome)
	root := abs(workspace)
	var diagnostics []string
	builder := bounded{max: MaxPromptBytes}
	appendFile(filepath.Join(home, ".bruce", "AGENTS.md"), &builder, &diagnostics)
	for _, dir := range instructionDirs(root) {
		if builder.full {
			break
		}
		appendFile(filepath.Join(dir, "AGENTS.md"), &builder, &diagnostics)
	}
	return LoadResult{Prompt: builder.text, Diagnostics: diagnostics}
}

func instructionDirs(workspace string) []string {
	gitRoot := findGitRoot(workspace)
	if gitRoot == "" {
		return []string{workspace}
	}
	var dirs []string
	for cur := workspace; ; cur = filepath.Dir(cur) {
		dirs = append(dirs, cur)
		if cur == gitRoot || cur == filepath.Dir(cur) {
			break
		}
	}
	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}
	return dirs
}

func findGitRoot(workspace string) string {
	for cur := workspace; ; cur = filepath.Dir(cur) {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		if cur == filepath.Dir(cur) {
			return ""
		}
	}
}

func appendFile(file string, b *bounded, diagnostics *[]string) {
	if b.full {
		return
	}
	data, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		*diagnostics = append(*diagnostics, file+": failed to read AGENTS.md: "+err.Error())
		return
	}
	b.append(string(data))
}

type bounded struct {
	text string
	max  int
	full bool
}

func (b *bounded) append(content string) {
	content = trimSpace(content)
	if content == "" || b.full {
		return
	}
	sep := ""
	if b.text != "" {
		sep = "\n\n"
	}
	remaining := b.max - len([]byte(b.text)) - len([]byte(sep))
	if remaining <= 0 {
		b.full = true
		return
	}
	added := truncateUTF8(content, remaining)
	b.text += sep + added
	if len([]byte(content)) > remaining || len([]byte(b.text)) >= b.max {
		b.full = true
	}
}

func truncateUTF8(text string, max int) string {
	out := make([]rune, 0, len(text))
	bytes := 0
	for _, r := range text {
		next := len(string(r))
		if bytes+next > max {
			break
		}
		out = append(out, r)
		bytes += next
	}
	return string(out)
}

func trimSpace(value string) string {
	start, end := 0, len(value)
	for start < end && (value[start] == ' ' || value[start] == '\n' || value[start] == '\r' || value[start] == '\t') {
		start++
	}
	for end > start && (value[end-1] == ' ' || value[end-1] == '\n' || value[end-1] == '\r' || value[end-1] == '\t') {
		end--
	}
	return value[start:end]
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
