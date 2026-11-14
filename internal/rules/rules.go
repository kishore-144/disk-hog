// Package rules describes what disk-hog considers reclaimable.
//
// There are three kinds of rule:
//
//   - project rules match a directory by name inside a project, usually
//     confirmed by a marker file next to it (node_modules beside
//     package.json, target beside Cargo.toml);
//   - global rules name fixed locations such as package-manager caches,
//     resolved per operating system and honouring the tools' own
//     environment variables (GOMODCACHE, CARGO_HOME, PIP_CACHE_DIR...);
//   - file rules match individual files: crash dumps, huge logs, partial
//     downloads and anything above the large-file threshold.
package rules

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"disk-hog/internal/units"
)

// Risk levels, from "regenerated automatically" to "look before deleting".
const (
	Safe     = "safe"
	Moderate = "moderate"
	Review   = "review"
)

// Categories group rules in the dashboard.
const (
	CatDeps      = "deps"
	CatBuild     = "build"
	CatCache     = "cache"
	CatToolchain = "toolchain"
	CatIDE       = "ide"
	CatTrash     = "trash"
	CatLogs      = "logs"
	CatTemp      = "temp"
	CatVM        = "vm"
	CatLarge     = "large"
)

// Categories lists every category with its display label, in display order.
var Categories = []struct{ ID, Label string }{
	{CatDeps, "Dependencies"},
	{CatBuild, "Build output"},
	{CatCache, "Caches"},
	{CatIDE, "Editors & IDEs"},
	{CatToolchain, "Toolchains & SDKs"},
	{CatVM, "VMs, images & models"},
	{CatTrash, "Trash"},
	{CatLogs, "Logs & crash dumps"},
	{CatTemp, "Temp & partial files"},
	{CatLarge, "Large files"},
}

// Rule is one thing disk-hog knows how to find. Custom rules in the config
// file use exactly this shape.
type Rule struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Risk     string `json:"risk"`
	Note     string `json:"note,omitempty"`
	Regen    string `json:"regen,omitempty"`
	// OS limits the rule to these GOOS values ("xdg" means Linux and the
	// BSDs); empty means everywhere.
	OS []string `json:"os,omitempty"`

	// Project rules: glob patterns on a directory's name. At least one
	// Siblings pattern must exist next to it, and at least one Children
	// name inside it, when those lists are set.
	Dirs     []string `json:"dirs,omitempty"`
	Siblings []string `json:"siblings,omitempty"`
	Children []string `json:"children,omitempty"`

	// Global rules: path templates. Each entry is a chain of alternatives
	// separated by "|"; the first whose variables are all set is used.
	// Tokens: {home} {cache} {config} {data} {localappdata} {temp}, and a
	// leading $VAR for an environment variable. Globs are expanded.
	Paths []string `json:"paths,omitempty"`
	// Contents means cleaning empties the directory but keeps it (Trash).
	Contents bool `json:"contents,omitempty"`

	// File rules: glob patterns on a file's name, matched case-insensitively,
	// for files at least MinSize bytes.
	Files   []string   `json:"files,omitempty"`
	MinSize units.Size `json:"minSize,omitempty"`
}

// Kind reports what sort of rule r is.
func (r *Rule) Kind() string {
	switch {
	case len(r.Paths) > 0:
		return "global"
	case len(r.Files) > 0:
		return "file"
	case len(r.Dirs) > 0:
		return "project"
	}
	return "large"
}

func (r *Rule) validate() error {
	if r.ID == "" {
		return fmt.Errorf("rule without an id")
	}
	if r.Name == "" {
		r.Name = r.ID
	}
	switch r.Risk {
	case Safe, Moderate, Review:
	case "":
		r.Risk = Review
	default:
		return fmt.Errorf("rule %s: risk must be safe, moderate or review", r.ID)
	}
	if r.Category == "" {
		r.Category = CatCache
	}
	n := 0
	for _, l := range [][]string{r.Dirs, r.Paths, r.Files} {
		if len(l) > 0 {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("rule %s: set exactly one of dirs, paths or files", r.ID)
	}
	for _, l := range [][]string{r.Dirs, r.Siblings, r.Files} {
		for _, p := range l {
			if _, err := path.Match(p, ""); err != nil {
				return fmt.Errorf("rule %s: bad pattern %q", r.ID, p)
			}
		}
	}
	return nil
}

func (r *Rule) forOS(goos string) bool {
	if len(r.OS) == 0 {
		return true
	}
	for _, o := range r.OS {
		if o == goos || (o == "xdg" && goos != "windows" && goos != "darwin") {
			return true
		}
	}
	return false
}

type globRule struct {
	pat string
	r   *Rule
}

// Set is a compiled rule list, organised for fast matching: exact names
// and extensions are map lookups, only real wildcards fall back to globbing.
type Set struct {
	All    []*Rule
	Global []*Rule
	Large  *Rule

	dirExact  map[string][]*Rule
	dirGlob   []globRule
	fileExact map[string][]*Rule
	fileExt   map[string][]*Rule
	fileGlob  []globRule
	minFile   int64
}

// LargeRule is the synthetic rule attached to files above the large-file
// threshold.
var LargeRule = Rule{
	ID: "large-file", Name: "Large file", Category: CatLarge, Risk: Review,
	Note: "Not regenerable. Check what it is before deleting.",
}

// NewSet compiles the built-in rules plus extra, minus the disabled IDs, for
// the given GOOS ("" means the running one).
func NewSet(extra []Rule, disabled []string, goos string) (*Set, error) {
	if goos == "" {
		goos = runtime.GOOS
	}
	off := make(map[string]bool, len(disabled))
	for _, id := range disabled {
		off[id] = true
	}
	large := LargeRule
	s := &Set{
		Large:     &large,
		dirExact:  map[string][]*Rule{},
		fileExact: map[string][]*Rule{},
		fileExt:   map[string][]*Rule{},
		minFile:   1 << 62,
	}
	if off[LargeRule.ID] {
		s.Large = nil
	}
	seen := map[string]bool{}
	// Custom rules come first so they win over a built-in with the same
	// pattern, and can replace a built-in outright by reusing its ID.
	list := make([]Rule, 0, len(extra)+len(builtin))
	list = append(list, extra...)
	list = append(list, builtin...)
	for i := range list {
		r := list[i]
		if off[r.ID] || seen[r.ID] || !r.forOS(goos) {
			continue
		}
		if err := r.validate(); err != nil {
			return nil, err
		}
		seen[r.ID] = true
		rp := &r
		s.All = append(s.All, rp)
		switch rp.Kind() {
		case "global":
			s.Global = append(s.Global, rp)
		case "project":
			for _, p := range rp.Dirs {
				if hasMeta(p) {
					s.dirGlob = append(s.dirGlob, globRule{p, rp})
				} else {
					s.dirExact[p] = append(s.dirExact[p], rp)
				}
			}
		case "file":
			if int64(rp.MinSize) < s.minFile {
				s.minFile = int64(rp.MinSize)
			}
			for _, p := range rp.Files {
				p = strings.ToLower(p)
				switch {
				case !hasMeta(p):
					s.fileExact[p] = append(s.fileExact[p], rp)
				case strings.HasPrefix(p, "*.") && !hasMeta(p[2:]):
					s.fileExt[p[1:]] = append(s.fileExt[p[1:]], rp)
				default:
					s.fileGlob = append(s.fileGlob, globRule{p, rp})
				}
			}
		}
	}
	return s, nil
}

func hasMeta(p string) bool { return strings.ContainsAny(p, "*?[") }

// MatchDir returns the project rule for a directory called name, or nil.
// sibling reports whether a name pattern exists next to it, child whether
// a name exists inside it; both are only called when a rule needs them.
func (s *Set) MatchDir(name string, sibling, child func(string) bool) *Rule {
	if r := s.checkDir(s.dirExact[name], sibling, child); r != nil {
		return r
	}
	for _, g := range s.dirGlob {
		if ok, _ := path.Match(g.pat, name); ok {
			if r := s.checkDir([]*Rule{g.r}, sibling, child); r != nil {
				return r
			}
		}
	}
	return nil
}

func (s *Set) checkDir(rs []*Rule, sibling, child func(string) bool) *Rule {
next:
	for _, r := range rs {
		if len(r.Siblings) > 0 {
			ok := false
			for _, m := range r.Siblings {
				if sibling(m) {
					ok = true
					break
				}
			}
			if !ok {
				continue next
			}
		}
		if len(r.Children) > 0 {
			ok := false
			for _, m := range r.Children {
				if child(m) {
					ok = true
					break
				}
			}
			if !ok {
				continue next
			}
		}
		return r
	}
	return nil
}

// MinFileSize is the smallest file any file rule can match, given the
// large-file threshold. Smaller files skip rule matching entirely.
func (s *Set) MinFileSize(large int64) int64 {
	if s.Large != nil && large > 0 && large < s.minFile {
		return large
	}
	return s.minFile
}

// MatchFile returns the rule for a file of the given name and allocated
// size, or nil. large is the large-file threshold (0 disables it).
func (s *Set) MatchFile(name string, size, large int64) *Rule {
	if size >= s.minFile {
		lower := strings.ToLower(name)
		for _, r := range s.fileExact[lower] {
			if size >= int64(r.MinSize) {
				return r
			}
		}
		if ext := path.Ext(lower); ext != "" {
			for _, r := range s.fileExt[ext] {
				if size >= int64(r.MinSize) {
					return r
				}
			}
		}
		for _, g := range s.fileGlob {
			if size < int64(g.r.MinSize) {
				continue
			}
			if ok, _ := path.Match(g.pat, lower); ok {
				return g.r
			}
		}
	}
	if s.Large != nil && large > 0 && size >= large {
		return s.Large
	}
	return nil
}

// ByID finds a rule.
func (s *Set) ByID(id string) *Rule {
	for _, r := range s.All {
		if r.ID == id {
			return r
		}
	}
	if s.Large != nil && id == s.Large.ID {
		return s.Large
	}
	return nil
}

// Env holds the base directories global rule templates expand against.
type Env struct {
	Home, Cache, Config, Data, LocalAppData, Temp string
	Getenv                                        func(string) string
}

// DefaultEnv describes the current user on the current OS.
func DefaultEnv() Env {
	e := Env{Getenv: os.Getenv}
	e.Home, _ = os.UserHomeDir()
	e.Cache, _ = os.UserCacheDir()
	e.Config, _ = os.UserConfigDir()
	e.Temp = os.TempDir()
	switch runtime.GOOS {
	case "windows":
		e.LocalAppData = os.Getenv("LOCALAPPDATA")
		e.Data = e.LocalAppData
	case "darwin":
		e.Data = filepath.Join(e.Home, "Library", "Application Support")
	default:
		e.Data = os.Getenv("XDG_DATA_HOME")
		if e.Data == "" && e.Home != "" {
			e.Data = filepath.Join(e.Home, ".local", "share")
		}
	}
	return e
}

// expand resolves one alternative chain, returning "" when no alternative
// has all of its variables set.
func (e Env) expand(chain string) string {
	for _, alt := range strings.Split(chain, "|") {
		alt = strings.TrimSpace(alt)
		if strings.HasPrefix(alt, "$") {
			end := strings.IndexAny(alt, `/\`)
			if end < 0 {
				end = len(alt)
			}
			v := e.Getenv(alt[1:end])
			if v == "" {
				continue
			}
			// A list variable such as GOPATH uses its first entry.
			if i := strings.IndexRune(v, os.PathListSeparator); i > 0 {
				v = v[:i]
			}
			alt = v + alt[end:]
		}
		ok := true
		for tok, val := range map[string]string{
			"{home}": e.Home, "{cache}": e.Cache, "{config}": e.Config,
			"{data}": e.Data, "{localappdata}": e.LocalAppData, "{temp}": e.Temp,
		} {
			if strings.Contains(alt, tok) {
				if val == "" {
					ok = false
					break
				}
				alt = strings.ReplaceAll(alt, tok, filepath.ToSlash(val))
			}
		}
		if ok && alt != "" {
			return filepath.Clean(filepath.FromSlash(alt))
		}
	}
	return ""
}

// Target is a resolved global rule location.
type Target struct {
	Path string
	Rule *Rule
}

// ResolveGlobal expands every global rule into the existing paths it names.
// A path claimed by an earlier rule is not claimed again, so specific rules
// listed before generic ones ("{cache}/pip" before "{cache}/*") win.
func (s *Set) ResolveGlobal(e Env) []Target {
	var out []Target
	claimed := map[string]bool{}
	for _, r := range s.Global {
		for _, chain := range r.Paths {
			p := e.expand(chain)
			if p == "" {
				continue
			}
			matches := []string{p}
			if hasMeta(p) {
				matches, _ = filepath.Glob(p)
			}
			for _, m := range matches {
				if claimed[m] {
					continue
				}
				fi, err := os.Lstat(m)
				if err != nil || fi.Mode()&os.ModeSymlink != 0 {
					continue
				}
				if !fi.IsDir() && !fi.Mode().IsRegular() {
					continue
				}
				claimed[m] = true
				out = append(out, Target{Path: m, Rule: r})
			}
		}
	}
	return out
}
