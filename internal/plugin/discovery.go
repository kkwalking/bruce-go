package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Discovery finds and validates manifests under the user and workspace roots.
//
// The user root holds plugins installed for the whole machine, the workspace
// root holds plugins that belong to one project. A workspace plugin with the
// same name as a user plugin replaces it, exactly like project skills replace
// user skills: the project is the more specific choice.
type Discovery struct {
	// UserRoot is the user-level plugin directory, e.g. <home>/.bruce/plugins.
	UserRoot string
	// WorkspaceRoot is the project-level plugin directory, e.g.
	// <workspace>/.bruce/plugins.
	WorkspaceRoot string
	// FailFast turns a load error into a returned error instead of a
	// diagnostic. Default false: a broken plugin must not stop Bruce.
	FailFast bool
}

// LoadResult is the outcome of one discovery pass.
type LoadResult struct {
	Manifests   []*Manifest
	Diagnostics []Diagnostic
	Overrides   []string // "<name>: workspace overrides user"
}

// Diagnostic is one non-fatal problem found while loading plugins.
//
// A diagnostic never stops Bruce: the broken plugin is skipped and every other
// plugin still loads. Path and Plugin are best effort, because a manifest can
// fail before either is known.
type Diagnostic struct {
	Path    string
	Plugin  string
	Message string
}

// String renders the diagnostic in the same shape as a ManifestError, so a
// caller can log diagnostics and errors through one path.
func (d Diagnostic) String() string {
	plugin := strings.TrimSpace(d.Plugin)
	if plugin == "" {
		plugin = "<unknown>"
	}
	return fmt.Sprintf("plugin %q (%s): %s", plugin, d.Path, Redact(d.Message))
}

// searchRoot pairs a directory with the source it represents.
type searchRoot struct {
	root   string
	source Source
}

// candidate is one manifest file found by scanning a search root. Both
// supported layouts reduce to a manifest path: entry resolution follows the
// manifest's directory, which is the plugin root for <root>/<name>.json and
// the plugin directory for <root>/<name>/plugin.json.
type candidate struct {
	path string
}

// Load scans both roots, validates every manifest and resolves conflicts.
//
// Loading is best effort by default: a manifest that fails to parse becomes a
// Diagnostic and the plugin is skipped, because one broken third-party plugin
// must not stop the editor from starting. FailFast inverts that for hosts that
// treat a broken manifest as a configuration error.
func (d Discovery) Load() (LoadResult, error) {
	result := LoadResult{Manifests: []*Manifest{}}
	roots := []searchRoot{
		{root: strings.TrimSpace(d.UserRoot), source: SourceUser},
		{root: strings.TrimSpace(d.WorkspaceRoot), source: SourceWorkspace},
	}
	// byName keeps the winning manifest per plugin name. Roots are visited in
	// precedence order, so a later root overrides an earlier one.
	byName := map[string]*Manifest{}
	for _, search := range roots {
		if search.root == "" {
			continue
		}
		candidates, scanDiagnostics, err := scanRoot(search.root)
		if err != nil {
			wrapped := NewError("", search.root, "", StageDiscover, CategoryManifest,
				errors.New("failed to scan plugin directory: "+err.Error()))
			if d.FailFast {
				return LoadResult{}, wrapped
			}
			result.Diagnostics = append(result.Diagnostics, Diagnostic{
				Path:    search.root,
				Message: "failed to scan plugin directory: " + err.Error(),
			})
			continue
		}
		for _, diagnostic := range scanDiagnostics {
			if d.FailFast {
				return LoadResult{}, &ManifestError{Path: diagnostic.Path, Message: diagnostic.Message}
			}
			result.Diagnostics = append(result.Diagnostics, diagnostic)
		}
		for _, found := range candidates {
			manifest, err := LoadManifest(found.path)
			if err != nil {
				if d.FailFast {
					return LoadResult{}, err
				}
				result.Diagnostics = append(result.Diagnostics, diagnosticFromError(found.path, err))
				continue
			}
			manifest.Source = search.source
			if previous, ok := byName[manifest.Name]; ok {
				if previous.Source == search.source {
					// Two manifests in one root claiming the same plugin name
					// are ambiguous; guessing would make the winner depend on
					// scan order.
					message := fmt.Sprintf(
						"plugin name %q is already declared by %s; this manifest is ignored",
						manifest.Name, previous.File)
					if d.FailFast {
						return LoadResult{}, &ManifestError{
							Plugin: manifest.Name, Path: manifest.File, Field: "name", Message: message,
						}
					}
					result.Diagnostics = append(result.Diagnostics, Diagnostic{
						Path: manifest.File, Plugin: manifest.Name, Message: message,
					})
					continue
				}
				result.Overrides = append(result.Overrides, fmt.Sprintf(
					"%s: %s overrides %s", manifest.Name, manifest.Source, previous.Source))
			}
			byName[manifest.Name] = manifest
		}
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	// Claiming runs in name order so that a cross-plugin conflict is always
	// resolved the same way, no matter how the filesystem enumerated entries.
	sort.Strings(names)
	claims := newClaimRegistry()
	for _, name := range names {
		manifest := byName[name]
		result.Manifests = append(result.Manifests, manifest)
		result.Diagnostics = append(result.Diagnostics, claims.claim(manifest)...)
	}
	sortDiagnostics(result.Diagnostics)
	return result, nil
}

// scanRoot lists the manifests directly under one plugin root.
//
// Two layouts are supported: <root>/<name>/plugin.json and <root>/<name>.json.
// A missing root is not an error — most projects have no plugins at all.
//
// Entries are stat'ed through symlinks, so a plugin installed by linking a
// checkout into the plugin directory works like a copied one. A directory that
// holds a plugin.json which cannot be used is reported instead of skipped:
// silently ignoring it would look exactly like "the plugin did not load".
func scanRoot(root string) ([]candidate, []Diagnostic, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	// ReadDir already sorts, but the contract of this package is deterministic
	// output, so it does not depend on that implementation detail.
	sort.Strings(names)

	var found []candidate
	var diagnostics []Diagnostic
	for _, name := range names {
		full := filepath.Join(root, name)
		info, err := os.Stat(full)
		if err != nil {
			// A broken symlink or an entry that vanished between ReadDir and
			// Stat is not a plugin directory.
			continue
		}
		switch {
		case info.IsDir():
			manifestPath := filepath.Join(full, "plugin.json")
			manifestInfo, err := os.Stat(manifestPath)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue // a directory that simply is not a plugin
				}
				diagnostics = append(diagnostics, Diagnostic{
					Path:    manifestPath,
					Message: "failed to read plugin.json: " + err.Error(),
				})
				continue
			}
			if !manifestInfo.Mode().IsRegular() {
				diagnostics = append(diagnostics, Diagnostic{
					Path:    manifestPath,
					Message: fmt.Sprintf("plugin.json must be a regular file, got %s", manifestInfo.Mode().Type()),
				})
				continue
			}
			found = append(found, candidate{path: manifestPath})
		case info.Mode().IsRegular() && strings.HasSuffix(name, ".json"):
			found = append(found, candidate{path: full})
		}
	}
	return found, diagnostics, nil
}

// diagnosticFromError renders a load failure as a diagnostic without losing
// the plugin identity the manifest error already recovered.
func diagnosticFromError(path string, err error) Diagnostic {
	diagnostic := Diagnostic{Path: path, Message: err.Error()}
	var manifestErr *ManifestError
	if errors.As(err, &manifestErr) {
		diagnostic.Plugin = manifestErr.Plugin
		diagnostic.Path = manifestErr.Path
		// Message is already fully rendered by ManifestError; keep it verbatim
		// so the diagnostic and the error cannot drift apart.
		diagnostic.Message = manifestErr.Error()
		return diagnostic
	}
	return diagnostic
}

// claimRegistry tracks which plugin owns each tool and command name.
type claimRegistry struct {
	tools    map[string]claimOwner
	commands map[string]claimOwner
}

type claimOwner struct {
	plugin string
	path   string
}

func newClaimRegistry() *claimRegistry {
	return &claimRegistry{tools: map[string]claimOwner{}, commands: map[string]claimOwner{}}
}

// claim resolves cross-plugin tool and command conflicts for one manifest.
//
// The first plugin to declare a name keeps it and the later declaration is
// dropped with a diagnostic. Silently overwriting would let the plugin that
// happens to load last decide which code an LLM tool call runs.
func (c *claimRegistry) claim(manifest *Manifest) []Diagnostic {
	var diagnostics []Diagnostic
	kept := manifest.Tools[:0:0]
	for _, tool := range manifest.Tools {
		if owner, taken := c.tools[tool.Name]; taken {
			diagnostics = append(diagnostics, conflictDiagnostic(manifest, "tool", tool.Name, owner))
			continue
		}
		c.tools[tool.Name] = claimOwner{plugin: manifest.Name, path: manifest.File}
		kept = append(kept, tool)
	}
	manifest.Tools = kept

	keptCommands := manifest.Commands[:0:0]
	for _, command := range manifest.Commands {
		if owner, taken := c.commands[command.Name]; taken {
			diagnostics = append(diagnostics, conflictDiagnostic(manifest, "command", command.Name, owner))
			continue
		}
		c.commands[command.Name] = claimOwner{plugin: manifest.Name, path: manifest.File}
		keptCommands = append(keptCommands, command)
	}
	manifest.Commands = keptCommands
	return diagnostics
}

func conflictDiagnostic(manifest *Manifest, kind, name string, owner claimOwner) Diagnostic {
	return Diagnostic{
		Path:   manifest.File,
		Plugin: manifest.Name,
		Message: fmt.Sprintf("%s %q is already declared by plugin %q (%s); this declaration is ignored",
			kind, name, owner.plugin, owner.path),
	}
}

// sortDiagnostics orders diagnostics so that two runs over the same directory
// tree report the same sequence.
func sortDiagnostics(diagnostics []Diagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Plugin != right.Plugin {
			return left.Plugin < right.Plugin
		}
		return left.Message < right.Message
	})
}
