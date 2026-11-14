// Package scan walks directory trees in parallel and reports what can be
// reclaimed.
//
// One pass does three things at once: it sizes every directory (for the
// explorer view), recognises project artifacts and global caches, and picks
// out large or disposable files. Memory stays small because only
// directories are kept in the tree; files are counted, never stored, and
// nothing below a recognised candidate (the inside of a node_modules) is
// kept at all.
package scan

import (
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"disk-hog/internal/fsutil"
	"disk-hog/internal/rules"
	"disk-hog/internal/walk"
)

// KeepMarker is a file that, placed in a directory, stops disk-hog from
// flagging anything beneath it.
const KeepMarker = ".disk-hog-keep"

// Options configures a scan.
type Options struct {
	Roots         []string
	Exclude       []string // glob on the name, or on the full path when it contains a separator
	Ignores       []string // paths never reported (still sized)
	OneFileSystem bool
	IncludeGlobal bool
	Workers       int
	MinItemSize   int64
	LargeFileSize int64
	Rules         *rules.Set
	Env           rules.Env
}

// Candidate is one reclaimable directory or file.
type Candidate struct {
	ID       int    `json:"id"`
	Path     string `json:"path"`
	Kind     string `json:"kind"` // "dir" or "file"
	Rule     string `json:"rule"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Risk     string `json:"risk"`
	Contents bool   `json:"contents,omitempty"`
	Global   bool   `json:"global,omitempty"`
	Note     string `json:"note,omitempty"`
	Regen    string `json:"regen,omitempty"`
	Project  string `json:"project,omitempty"`
	Size     int64  `json:"size"`
	Files    int64  `json:"files"`
	// Newest is the newest modification time inside (unix ms), Active the
	// latest activity in the owning project, Touched the later of the two.
	Newest  int64  `json:"newest,omitempty"`
	Active  int64  `json:"active,omitempty"`
	Touched int64  `json:"touched,omitempty"`
	Dev     uint64 `json:"dev,omitempty"`
	Ino     uint64 `json:"ino,omitempty"`

	size, files, newest atomic.Int64
}

// Node is a directory in the size tree.
type Node struct {
	Name   string
	Size   int64
	Files  int64
	Small  int64 // bytes in children too small to keep
	SmallN int
	Kids   []*Node
	Cand   *Candidate

	self, nfiles int64 // written only by the worker that visits this dir
}

// Progress is a live view of a running scan.
type Progress struct {
	Running bool    `json:"running"`
	Dirs    int64   `json:"dirs"`
	Files   int64   `json:"files"`
	Bytes   int64   `json:"bytes"`
	Errors  int64   `json:"errors"`
	Found   int64   `json:"found"`
	Current string  `json:"current"`
	Elapsed float64 `json:"elapsed"`
}

// Small aggregates candidates below the minimum item size.
type Small struct {
	Rule     string `json:"rule"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Count    int    `json:"count"`
	Size     int64  `json:"size"`
}

// CatSum totals one category.
type CatSum struct {
	Category string `json:"category"`
	Label    string `json:"label"`
	Count    int    `json:"count"`
	Size     int64  `json:"size"`
	Safe     int64  `json:"safe"`
	Moderate int64  `json:"moderate"`
	Review   int64  `json:"review"`
}

// Result is a finished scan.
type Result struct {
	Started      time.Time    `json:"started"`
	Finished     time.Time    `json:"finished"`
	Duration     float64      `json:"duration"`
	Roots        []string     `json:"roots"`
	Canceled     bool         `json:"canceled"`
	Stats        Progress     `json:"stats"`
	Total        int64        `json:"total"`
	Items        []*Candidate `json:"items"`
	Small        []Small      `json:"small"`
	Summary      []CatSum     `json:"summary"`
	ErrorSamples []string     `json:"errorSamples"`
	Tree         *Node        `json:"-"`
}

type job struct {
	path string
	node *Node      // tree node; nil inside a candidate
	cand *Candidate // set while walking inside a candidate
	dev  uint64
	keep bool
	// inert disables project rules: inside installed editor extensions or
	// Python site-packages a "dist" or "node_modules" is part of an
	// installed program, not a disposable build artifact.
	inert bool
}

// inertDirs never have project rules applied beneath them.
var inertDirs = map[string]bool{
	"site-packages": true, "dist-packages": true, ".vscode-server": true, ".vscode-server-insiders": true,
	".cursor-server": true, ".windsurf-server": true, ".vscode-oss": true, "Extensions": true,
}

// homeInert are top-level home folders holding installed software rather
// than projects. Hidden folders in home (~/.config, ~/.local, ~/.opencode)
// are treated the same way: a node_modules there belongs to a tool such as
// an editor plugin or language server, and deleting it breaks the tool.
// Caches inside them are still found by the global rules.
var homeInert = map[string]bool{"Library": true, "AppData": true, "snap": true}

func inertDir(name, parent string) bool {
	// ~/.vscode/extensions, ~/.cursor/extensions, ~/.antigravity/extensions...
	return inertDirs[name] || (name == "extensions" && strings.HasPrefix(parent, "."))
}

// Scanner runs one scan. Create a new one for every scan.
type Scanner struct {
	opt   Options
	start time.Time
	home  string

	dirs, files, bytes, errs, found atomic.Int64
	cur                             atomic.Pointer[string]
	running                         atomic.Bool

	global map[string]*Candidate // read-only once the walk starts

	linkMu sync.Mutex
	links  map[[2]uint64]struct{}

	mu         sync.Mutex
	cands      []*Candidate
	errSamples []string

	exName  map[string]bool
	exGlob  []string
	exPath  []string
	minFile int64
}

// New prepares a scan.
func New(opt Options) *Scanner {
	if opt.Rules == nil {
		opt.Rules, _ = rules.NewSet(nil, nil, "")
	}
	if opt.Env.Getenv == nil {
		opt.Env = rules.DefaultEnv()
	}
	s := &Scanner{
		opt:    opt,
		home:   filepath.Clean(opt.Env.Home),
		global: map[string]*Candidate{},
		links:  map[[2]uint64]struct{}{},
		exName: map[string]bool{},
	}
	for _, e := range opt.Exclude {
		switch {
		case strings.ContainsAny(e, `/\`):
			s.exPath = append(s.exPath, filepath.Clean(expandHome(e, s.home)))
		case strings.ContainsAny(e, "*?["):
			s.exGlob = append(s.exGlob, e)
		default:
			s.exName[e] = true
		}
	}
	s.minFile = opt.Rules.MinFileSize(opt.LargeFileSize)
	return s
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(home, p[2:])
	}
	return p
}

// Progress reports the scan's current state; safe to call at any time.
func (s *Scanner) Progress() Progress {
	p := Progress{
		Running: s.running.Load(),
		Dirs:    s.dirs.Load(), Files: s.files.Load(), Bytes: s.bytes.Load(),
		Errors: s.errs.Load(), Found: s.found.Load(),
	}
	if c := s.cur.Load(); c != nil {
		p.Current = *c
	}
	if !s.start.IsZero() {
		p.Elapsed = time.Since(s.start).Seconds()
	}
	return p
}

// Run scans until done or ctx is cancelled and returns what it found.
func (s *Scanner) Run(ctx context.Context) *Result {
	s.start = time.Now()
	s.running.Store(true)
	defer s.running.Store(false)

	tree := &Node{}
	var seeds []job
	var roots []string
	for _, r := range s.opt.Roots {
		r = filepath.Clean(expandHome(r, s.home))
		if abs, err := filepath.Abs(r); err == nil {
			r = abs
		}
		if real, err := filepath.EvalSymlinks(r); err == nil {
			r = real
		}
		fi, err := os.Lstat(r)
		if err != nil || !fi.IsDir() {
			if err == nil {
				err = &fs.PathError{Op: "scan", Path: r, Err: fs.ErrInvalid}
			}
			s.fail(r, err)
			continue
		}
		if dupRoot(roots, r) {
			continue
		}
		roots = append(roots, r)
		m := fsutil.MetaOf(fi)
		n := &Node{Name: r, self: m.Alloc}
		tree.Kids = append(tree.Kids, n)
		seeds = append(seeds, job{path: r, node: n, dev: m.Dev})
	}

	if s.opt.IncludeGlobal {
		for _, t := range s.opt.Rules.ResolveGlobal(s.opt.Env) {
			if containsRoot(t.Path, roots) {
				continue // a cache that contains a scan root would double count
			}
			fi, err := os.Lstat(t.Path)
			if err != nil {
				continue
			}
			m := fsutil.MetaOf(fi)
			kind := "file"
			if fi.IsDir() {
				kind = "dir"
			}
			c := s.newCand(t.Rule, t.Path, kind, m, "")
			if c == nil {
				continue
			}
			c.Global = true
			c.size.Add(m.Alloc)
			c.newest.Store(fi.ModTime().UnixMilli())
			s.global[t.Path] = c
			if fi.IsDir() {
				seeds = append(seeds, job{path: t.Path, cand: c, dev: m.Dev})
			} else {
				c.files.Store(1)
			}
		}
	}

	walk.Run(ctx, s.opt.Workers, seeds, s.visit)
	return s.finish(ctx.Err() != nil, tree, roots)
}

func dupRoot(roots []string, r string) bool {
	for _, x := range roots {
		if fsutil.Within(r, x) {
			return true
		}
	}
	return false
}

func containsRoot(p string, roots []string) bool {
	for _, r := range roots {
		if fsutil.Within(r, p) {
			return true
		}
	}
	return false
}

func join(dir, name string) string {
	if strings.HasSuffix(dir, string(filepath.Separator)) {
		return dir + name
	}
	return dir + string(filepath.Separator) + name
}

func (s *Scanner) fail(p string, err error) {
	if s.errs.Add(1) <= 50 {
		s.mu.Lock()
		s.errSamples = append(s.errSamples, err.Error())
		s.mu.Unlock()
	}
}

func (s *Scanner) excluded(p, name string) bool {
	if s.exName[name] {
		return true
	}
	for _, g := range s.exGlob {
		if ok, _ := path.Match(g, name); ok {
			return true
		}
	}
	for _, e := range s.exPath {
		if strings.ContainsAny(e, "*?[") {
			if ok, _ := filepath.Match(e, p); ok {
				return true
			}
		} else if fsutil.Within(p, e) {
			return true
		}
	}
	return false
}

func (s *Scanner) ignored(p string) bool {
	for _, ig := range s.opt.Ignores {
		if fsutil.Within(p, ig) {
			return true
		}
	}
	return false
}

func (s *Scanner) newCand(r *rules.Rule, p, kind string, m fsutil.Meta, project string) *Candidate {
	if s.ignored(p) {
		return nil
	}
	c := &Candidate{
		Path: p, Kind: kind, Rule: r.ID, Name: r.Name, Category: r.Category, Risk: r.Risk,
		Contents: r.Contents, Note: r.Note, Regen: r.Regen, Project: project, Dev: m.Dev, Ino: m.Ino,
	}
	s.mu.Lock()
	s.cands = append(s.cands, c)
	s.mu.Unlock()
	s.found.Add(1)
	return c
}

// linkSeen reports whether this inode was already counted. Only files with
// more than one link are tracked, which keeps the set small.
func (s *Scanner) linkSeen(m fsutil.Meta) bool {
	if !m.HasID || m.Nlink < 2 {
		return false
	}
	k := [2]uint64{m.Dev, m.Ino}
	s.linkMu.Lock()
	_, ok := s.links[k]
	if !ok {
		s.links[k] = struct{}{}
	}
	s.linkMu.Unlock()
	return ok
}

func storeMax(a *atomic.Int64, v int64) {
	for {
		old := a.Load()
		if v <= old || a.CompareAndSwap(old, v) {
			return
		}
	}
}

func (s *Scanner) visit(j job, push func(...job)) {
	p := j.path
	s.cur.Store(&p)
	f, err := os.Open(p)
	if err != nil {
		s.fail(p, err)
		return
	}
	ents, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		s.fail(p, err)
		if len(ents) == 0 {
			return
		}
	}
	s.dirs.Add(1)
	if j.cand != nil {
		s.visitInside(j, ents, push)
	} else {
		s.visitTree(j, ents, push)
	}
}

// visitInside sizes the contents of a candidate.
func (s *Scanner) visitInside(j job, ents []fs.DirEntry, push func(...job)) {
	c := j.cand
	var size, files, newest int64
	var sub []job
	for _, e := range ents {
		t := e.Type()
		if t&fs.ModeSymlink != 0 {
			continue
		}
		cp := join(j.path, e.Name())
		if t.IsDir() {
			if _, ok := s.global[cp]; ok {
				continue // a nested global target is sized on its own
			}
			fi, err := os.Lstat(cp)
			if err != nil {
				s.fail(cp, err)
				continue
			}
			m := fsutil.MetaOf(fi)
			if s.opt.OneFileSystem && m.HasID && m.Dev != j.dev {
				continue
			}
			size += m.Alloc
			if mt := fi.ModTime().UnixMilli(); mt > newest {
				newest = mt
			}
			sub = append(sub, job{path: cp, cand: c, dev: j.dev})
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		m := fsutil.MetaOf(fi)
		if s.linkSeen(m) {
			continue
		}
		size += m.Alloc
		files++
		if mt := fi.ModTime().UnixMilli(); mt > newest {
			newest = mt
		}
	}
	c.size.Add(size)
	c.files.Add(files)
	storeMax(&c.newest, newest)
	s.files.Add(files)
	s.bytes.Add(size)
	push(sub...)
}

// visitTree sizes an ordinary directory and looks for candidates in it.
func (s *Scanner) visitTree(j job, ents []fs.DirEntry, push func(...job)) {
	n := j.node
	keep := j.keep
	if !keep {
		for _, e := range ents {
			if e.Name() == KeepMarker {
				keep = true
				break
			}
		}
	}
	var names map[string]bool
	sibling := func(pat string) bool {
		if !strings.ContainsAny(pat, "*?[") {
			if names == nil {
				names = make(map[string]bool, len(ents))
				for _, e := range ents {
					names[e.Name()] = true
				}
			}
			return names[pat]
		}
		for _, e := range ents {
			if ok, _ := path.Match(pat, e.Name()); ok {
				return true
			}
		}
		return false
	}
	// Project rules never fire in the home directory itself: a stray
	// ~/package.json must not turn ~/.cache or ~/build into a candidate.
	projects := !keep && !j.inert && j.path != s.home
	parentBase := filepath.Base(j.path)

	var self, files, newest int64
	var sub []job
	var made []*Candidate
	hasGit := false
	for _, e := range ents {
		name := e.Name()
		t := e.Type()
		if t&fs.ModeSymlink != 0 {
			continue
		}
		cp := join(j.path, name)
		if t.IsDir() {
			if name == ".git" {
				hasGit = true
			}
			if c, ok := s.global[cp]; ok {
				n.Kids = append(n.Kids, &Node{Name: name, Cand: c})
				continue
			}
			if s.excluded(cp, name) {
				continue
			}
			fi, err := os.Lstat(cp)
			if err != nil {
				s.fail(cp, err)
				continue
			}
			m := fsutil.MetaOf(fi)
			if s.opt.OneFileSystem && m.HasID && m.Dev != j.dev {
				continue // another filesystem is mounted here
			}
			mt := fi.ModTime().UnixMilli()
			if projects {
				child := func(c string) bool { _, err := os.Lstat(join(cp, c)); return err == nil }
				if r := s.opt.Rules.MatchDir(name, sibling, child); r != nil {
					if c := s.newCand(r, cp, "dir", m, j.path); c != nil {
						c.size.Add(m.Alloc)
						storeMax(&c.newest, mt)
						s.bytes.Add(m.Alloc)
						n.Kids = append(n.Kids, &Node{Name: name, Cand: c})
						made = append(made, c)
						sub = append(sub, job{path: cp, cand: c, dev: j.dev})
						continue
					}
				}
			}
			if mt > newest {
				newest = mt
			}
			kid := &Node{Name: name, self: m.Alloc}
			n.Kids = append(n.Kids, kid)
			s.bytes.Add(m.Alloc)
			inert := j.inert || inertDir(name, parentBase) ||
				(j.path == s.home && (strings.HasPrefix(name, ".") || homeInert[name]))
			sub = append(sub, job{path: cp, node: kid, dev: j.dev, keep: keep, inert: inert})
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		m := fsutil.MetaOf(fi)
		if s.linkSeen(m) {
			continue
		}
		self += m.Alloc
		files++
		mt := fi.ModTime().UnixMilli()
		if mt > newest {
			newest = mt
		}
		if !keep && m.Alloc >= s.minFile && fi.Mode().IsRegular() {
			if r := s.opt.Rules.MatchFile(name, m.Alloc, s.opt.LargeFileSize); r != nil {
				if c := s.newCand(r, cp, "file", m, ""); c != nil {
					c.size.Store(m.Alloc)
					c.files.Store(1)
					c.newest.Store(mt)
				}
			}
		}
	}
	n.self += self
	n.nfiles += files
	s.files.Add(files)
	s.bytes.Add(self)

	if len(made) > 0 {
		// The project's last activity: its newest entry other than the
		// candidates themselves, or the last git operation.
		active := newest
		if hasGit {
			if fi, err := os.Stat(join(join(j.path, ".git"), "index")); err == nil {
				if mt := fi.ModTime().UnixMilli(); mt > active {
					active = mt
				}
			}
		}
		for _, c := range made {
			c.Active = active
		}
	}
	push(sub...)
}

// pruneBelow is the smallest directory kept in the explorer tree.
const pruneBelow = 1 << 20

func (s *Scanner) finish(canceled bool, tree *Node, roots []string) *Result {
	for _, c := range s.cands {
		c.Size = c.size.Load()
		c.Files = c.files.Load()
		c.Newest = c.newest.Load()
		c.Touched = c.Newest
		if c.Active > c.Touched {
			c.Touched = c.Active
		}
	}
	for _, r := range tree.Kids {
		total(r)
		tree.Size += r.Size
		tree.Files += r.Files
	}

	res := &Result{
		Started: s.start, Finished: time.Now(), Roots: roots, Canceled: canceled,
		Stats: s.Progress(), Tree: tree, ErrorSamples: s.errSamples,
	}
	res.Stats.Running = false
	res.Stats.Current = ""
	res.Duration = res.Finished.Sub(res.Started).Seconds()

	small := map[string]*Small{}
	for _, c := range s.cands {
		if c.Size >= s.opt.MinItemSize || (c.Category == rules.CatTrash && c.Files > 0) {
			res.Items = append(res.Items, c)
			continue
		}
		if c.Size == 0 {
			continue
		}
		sm := small[c.Rule]
		if sm == nil {
			sm = &Small{Rule: c.Rule, Name: c.Name, Category: c.Category}
			small[c.Rule] = sm
		}
		sm.Count++
		sm.Size += c.Size
	}
	sort.Slice(res.Items, func(a, b int) bool {
		if res.Items[a].Size != res.Items[b].Size {
			return res.Items[a].Size > res.Items[b].Size
		}
		return res.Items[a].Path < res.Items[b].Path
	})
	for i, c := range res.Items {
		c.ID = i + 1
		res.Total += c.Size
	}
	for _, sm := range small {
		res.Small = append(res.Small, *sm)
	}
	sort.Slice(res.Small, func(a, b int) bool { return res.Small[a].Size > res.Small[b].Size })
	res.Summary = Summarize(res.Items)
	return res
}

// Summarize totals items per category, in display order.
func Summarize(items []*Candidate) []CatSum {
	by := map[string]*CatSum{}
	for _, c := range items {
		cs := by[c.Category]
		if cs == nil {
			cs = &CatSum{Category: c.Category}
			by[c.Category] = cs
		}
		cs.Count++
		cs.Size += c.Size
		switch c.Risk {
		case rules.Safe:
			cs.Safe += c.Size
		case rules.Moderate:
			cs.Moderate += c.Size
		default:
			cs.Review += c.Size
		}
	}
	var out []CatSum
	for _, cat := range rules.Categories {
		if cs := by[cat.ID]; cs != nil {
			cs.Label = cat.Label
			out = append(out, *cs)
			delete(by, cat.ID)
		}
	}
	for _, cs := range by { // custom categories
		cs.Label = cs.Category
		out = append(out, *cs)
	}
	return out
}

// total computes subtree sizes bottom-up, folds tiny children into Small,
// and sorts what remains largest first.
func total(n *Node) {
	if n.Cand != nil {
		n.Size, n.Files = n.Cand.size.Load(), n.Cand.files.Load()
		return
	}
	n.Size, n.Files = n.self, n.nfiles
	kept := n.Kids[:0]
	for _, k := range n.Kids {
		total(k)
		n.Size += k.Size
		n.Files += k.Files
		if k.Size < pruneBelow && k.Cand == nil {
			n.Small += k.Size
			n.SmallN++
			continue
		}
		kept = append(kept, k)
	}
	for i := len(kept); i < len(n.Kids); i++ {
		n.Kids[i] = nil
	}
	n.Kids = kept
	sort.Slice(n.Kids, func(a, b int) bool { return n.Kids[a].Size > n.Kids[b].Size })
}

// NodeView is one level of the explorer.
type NodeView struct {
	Path   string     `json:"path"`
	Parent string     `json:"parent,omitempty"`
	Size   int64      `json:"size"`
	Files  int64      `json:"files"`
	Self   int64      `json:"self"`
	Small  int64      `json:"small"`
	SmallN int        `json:"smallN"`
	Kids   []KidView  `json:"kids"`
	Roots  []string   `json:"roots,omitempty"`
	Cand   *Candidate `json:"cand,omitempty"`
}

// KidView is a child row in the explorer.
type KidView struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Files int64  `json:"files"`
	More  bool   `json:"more"`
	Cand  int    `json:"cand,omitempty"` // item ID when this is a reported candidate
	Rule  string `json:"rule,omitempty"`
	Risk  string `json:"risk,omitempty"`
}

// View returns the explorer level at p; "" means the list of roots.
func (r *Result) View(p string) (*NodeView, bool) {
	if r.Tree == nil {
		return nil, false
	}
	var n *Node
	full := ""
	if p == "" {
		if len(r.Tree.Kids) == 1 {
			n, full = r.Tree.Kids[0], r.Tree.Kids[0].Name
		} else {
			n = r.Tree
		}
	} else {
		p = filepath.Clean(p)
		for _, root := range r.Tree.Kids {
			if !fsutil.Within(p, root.Name) {
				continue
			}
			n, full = root, root.Name
			rel, _ := filepath.Rel(root.Name, p)
			if rel != "." {
				for _, part := range strings.Split(rel, string(filepath.Separator)) {
					var next *Node
					for _, k := range n.Kids {
						if k.Name == part {
							next = k
							break
						}
					}
					if next == nil {
						return nil, false
					}
					n, full = next, join(full, part)
				}
			}
			break
		}
		if n == nil {
			return nil, false
		}
	}
	v := &NodeView{Path: full, Size: n.Size, Files: n.Files, Small: n.Small, SmallN: n.SmallN, Self: n.self, Cand: n.Cand}
	if full != "" {
		for _, root := range r.Tree.Kids {
			if full != root.Name && fsutil.Within(full, root.Name) {
				v.Parent = filepath.Dir(full)
			}
		}
	}
	for _, root := range r.Tree.Kids {
		v.Roots = append(v.Roots, root.Name)
	}
	for _, k := range n.Kids {
		kp := k.Name
		if full != "" {
			kp = join(full, k.Name)
		}
		kv := KidView{Name: k.Name, Path: kp, Size: k.Size, Files: k.Files, More: len(k.Kids) > 0 || k.Small > 0}
		if k.Cand != nil {
			kv.Cand, kv.Rule, kv.Risk, kv.More = k.Cand.ID, k.Cand.Name, k.Cand.Risk, false
		}
		v.Kids = append(v.Kids, kv)
	}
	return v, true
}
