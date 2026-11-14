// Package dupes finds files with identical contents.
//
// It works in three narrowing passes so that most files are never read:
// group by size, then by a hash of the first and last 16 KiB, and only then
// by a full SHA-256. Hard links to the same inode are not duplicates (they
// take no extra space) and are collapsed before hashing.
package dupes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"disk-hog/internal/fsutil"
	"disk-hog/internal/walk"
)

// Options configures a duplicate search.
type Options struct {
	Roots         []string
	Exclude       []string
	MinSize       int64
	OneFileSystem bool
	Workers       int
	// SkipDirs are directory names never descended into (node_modules and
	// friends are full of legitimately duplicated files nobody cares about).
	SkipDirs []string
}

// File is one copy within a group.
type File struct {
	ID    int    `json:"id"`
	Path  string `json:"path"`
	Mtime int64  `json:"mtime"`
	Dev   uint64 `json:"dev,omitempty"`
	Ino   uint64 `json:"ino,omitempty"`
}

// Group is a set of files with identical contents.
type Group struct {
	ID     int    `json:"id"`
	Size   int64  `json:"size"`
	Hash   string `json:"hash"`
	Wasted int64  `json:"wasted"`
	Files  []File `json:"files"`
}

// Progress is a live view of a running search.
type Progress struct {
	Running bool    `json:"running"`
	Phase   string  `json:"phase"`
	Files   int64   `json:"files"`
	Hashed  int64   `json:"hashed"`
	ToHash  int64   `json:"toHash"`
	Bytes   int64   `json:"bytes"`
	Errors  int64   `json:"errors"`
	Elapsed float64 `json:"elapsed"`
}

// Result is a finished search.
type Result struct {
	Started  time.Time `json:"started"`
	Duration float64   `json:"duration"`
	Roots    []string  `json:"roots"`
	MinSize  int64     `json:"minSize"`
	Canceled bool      `json:"canceled"`
	Groups   []Group   `json:"groups"`
	Wasted   int64     `json:"wasted"`
	Stats    Progress  `json:"stats"`
}

// DefaultSkip lists directories that are skipped unless configured
// otherwise: their duplicates are managed by package managers.
var DefaultSkip = []string{"node_modules", ".git", ".venv", "venv", "__pycache__", "target", ".gradle", ".m2", "go", ".cargo", ".rustup"}

// Finder runs one search.
type Finder struct {
	opt   Options
	start time.Time
	phase atomic.Pointer[string]

	files, hashed, toHash, bytes, errs atomic.Int64
	running                            atomic.Bool
}

// New prepares a search.
func New(opt Options) *Finder {
	if opt.MinSize <= 0 {
		opt.MinSize = 1 << 20
	}
	return &Finder{opt: opt}
}

func (f *Finder) setPhase(p string) { f.phase.Store(&p) }

// Progress reports the current state.
func (f *Finder) Progress() Progress {
	p := Progress{Running: f.running.Load(), Files: f.files.Load(), Hashed: f.hashed.Load(),
		ToHash: f.toHash.Load(), Bytes: f.bytes.Load(), Errors: f.errs.Load()}
	if ph := f.phase.Load(); ph != nil {
		p.Phase = *ph
	}
	if !f.start.IsZero() {
		p.Elapsed = time.Since(f.start).Seconds()
	}
	return p
}

type ref struct {
	path  string
	mtime int64
	dev   uint64
	ino   uint64
}

type job struct {
	path string
	dev  uint64
}

// Run searches until done or cancelled.
func (f *Finder) Run(ctx context.Context) *Result {
	f.start = time.Now()
	f.running.Store(true)
	defer f.running.Store(false)
	f.setPhase("scanning")

	skip := map[string]bool{}
	for _, d := range f.opt.SkipDirs {
		skip[d] = true
	}
	var exGlob []string
	for _, e := range f.opt.Exclude {
		if strings.ContainsAny(e, "*?[") || strings.ContainsAny(e, `/\`) {
			exGlob = append(exGlob, e)
		} else {
			skip[e] = true
		}
	}
	home, _ := os.UserHomeDir()

	var mu sync.Mutex
	bySize := map[int64][]ref{}
	inodes := map[[2]uint64]bool{}

	var seeds []job
	var roots []string
	for _, r := range f.opt.Roots {
		if r == "~" || strings.HasPrefix(r, "~/") {
			r = filepath.Join(home, strings.TrimPrefix(r, "~"))
		}
		r, _ = filepath.Abs(filepath.Clean(r))
		fi, err := os.Stat(r)
		if err != nil || !fi.IsDir() {
			f.errs.Add(1)
			continue
		}
		roots = append(roots, r)
		seeds = append(seeds, job{r, fsutil.MetaOf(fi).Dev})
	}

	walk.Run(ctx, f.opt.Workers, seeds, func(j job, push func(...job)) {
		d, err := os.Open(j.path)
		if err != nil {
			f.errs.Add(1)
			return
		}
		ents, _ := d.ReadDir(-1)
		d.Close()
		var sub []job
		var local []ref
		var sizes []int64
		for _, e := range ents {
			name := e.Name()
			t := e.Type()
			if t&fs.ModeSymlink != 0 {
				continue
			}
			p := filepath.Join(j.path, name)
			if excluded(p, name, exGlob) {
				continue
			}
			if t.IsDir() {
				if skip[name] {
					continue
				}
				fi, err := os.Lstat(p)
				if err != nil {
					continue
				}
				m := fsutil.MetaOf(fi)
				if f.opt.OneFileSystem && m.HasID && m.Dev != j.dev {
					continue
				}
				sub = append(sub, job{p, j.dev})
				continue
			}
			if !t.IsRegular() {
				continue
			}
			fi, err := e.Info()
			if err != nil || fi.Size() < f.opt.MinSize {
				continue
			}
			f.files.Add(1)
			m := fsutil.MetaOf(fi)
			local = append(local, ref{p, fi.ModTime().UnixMilli(), m.Dev, m.Ino})
			sizes = append(sizes, fi.Size())
		}
		if len(local) > 0 {
			mu.Lock()
			for i, r := range local {
				if r.ino != 0 {
					k := [2]uint64{r.dev, r.ino}
					if inodes[k] {
						continue
					}
					inodes[k] = true
				}
				bySize[sizes[i]] = append(bySize[sizes[i]], r)
			}
			mu.Unlock()
		}
		push(sub...)
	})
	inodes = nil

	res := &Result{Started: f.start, Roots: roots, MinSize: f.opt.MinSize}
	if ctx.Err() != nil {
		return f.done(res, true)
	}

	// Pass 2: head+tail hash for every size shared by two or more files.
	var groups []sizeGroup
	for size, refs := range bySize {
		if len(refs) > 1 {
			groups = append(groups, sizeGroup{size, refs})
			f.toHash.Add(int64(len(refs)))
		}
	}
	bySize = nil
	f.setPhase("comparing")
	partial := f.hashAll(ctx, groups, true)
	if ctx.Err() != nil {
		return f.done(res, true)
	}

	// Pass 3: full hash where the quick hash still matches.
	f.hashed.Store(0)
	f.toHash.Store(0)
	var full []sizeGroup
	for _, g := range partial {
		if g.size <= 2*edge {
			continue // the quick hash already covered the whole file
		}
		full = append(full, sizeGroup{g.size, g.refs})
		f.toHash.Add(int64(len(g.refs)))
	}
	f.setPhase("verifying")
	verified := f.hashAll(ctx, full, false)
	if ctx.Err() != nil {
		return f.done(res, true)
	}
	for _, g := range partial {
		if g.size <= 2*edge {
			verified = append(verified, g)
		}
	}

	id := 0
	for _, g := range verified {
		gr := Group{Size: g.size, Hash: g.hash, Wasted: g.size * int64(len(g.refs)-1)}
		sort.Slice(g.refs, func(a, b int) bool { return g.refs[a].mtime < g.refs[b].mtime })
		for _, r := range g.refs {
			id++
			gr.Files = append(gr.Files, File{ID: id, Path: r.path, Mtime: r.mtime, Dev: r.dev, Ino: r.ino})
		}
		res.Groups = append(res.Groups, gr)
	}
	sort.Slice(res.Groups, func(a, b int) bool { return res.Groups[a].Wasted > res.Groups[b].Wasted })
	if len(res.Groups) > 5000 {
		res.Groups = res.Groups[:5000]
	}
	for i := range res.Groups {
		res.Groups[i].ID = i + 1
		res.Wasted += res.Groups[i].Wasted
	}
	return f.done(res, false)
}

func (f *Finder) done(res *Result, canceled bool) *Result {
	res.Canceled = canceled
	res.Duration = time.Since(f.start).Seconds()
	res.Stats = f.Progress()
	res.Stats.Running = false
	res.Stats.Phase = "done"
	return res
}

func excluded(p, name string, globs []string) bool {
	for _, g := range globs {
		if strings.ContainsAny(g, `/\`) {
			if ok, _ := filepath.Match(g, p); ok || fsutil.Within(p, g) {
				return true
			}
		} else if ok, _ := path.Match(g, name); ok {
			return true
		}
	}
	return false
}

const edge = 16 << 10

type sizeGroup struct {
	size int64
	refs []ref
}

type hashed struct {
	size int64
	hash string
	refs []ref
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 256<<10); return &b }}

// hashAll hashes every file of every group and regroups by hash, keeping
// only hashes shared by two or more files.
func (f *Finder) hashAll(ctx context.Context, groups []sizeGroup, quick bool) []hashed {
	workers := runtime.NumCPU()
	if workers > 4 {
		workers = 4 // disk-bound: more readers only add seeking
	}
	type item struct {
		g, i int
	}
	hashes := make([][]string, len(groups))
	var items []item
	for gi, g := range groups {
		hashes[gi] = make([]string, len(g.refs))
		for i := range g.refs {
			items = append(items, item{gi, i})
		}
	}
	walk.Run(ctx, workers, items, func(it item, _ func(...item)) {
		g := groups[it.g]
		h, err := hashFile(g.refs[it.i].path, g.size, quick)
		if err != nil {
			f.errs.Add(1)
		}
		hashes[it.g][it.i] = h
		f.hashed.Add(1)
		if quick {
			f.bytes.Add(min(g.size, 2*edge))
		} else {
			f.bytes.Add(g.size)
		}
	})
	var out []hashed
	for gi, g := range groups {
		by := map[string][]ref{}
		for i, h := range hashes[gi] {
			if h != "" {
				by[h] = append(by[h], g.refs[i])
			}
		}
		for h, refs := range by {
			if len(refs) > 1 {
				out = append(out, hashed{g.size, h, refs})
			}
		}
	}
	return out
}

func hashFile(p string, size int64, quick bool) (string, error) {
	fh, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	h := sha256.New()
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	if quick && size > 2*edge {
		if _, err := io.ReadFull(fh, buf[:edge]); err != nil {
			return "", err
		}
		h.Write(buf[:edge])
		if _, err := fh.ReadAt(buf[:edge], size-edge); err != nil && err != io.EOF {
			return "", err
		}
		h.Write(buf[:edge])
	} else if _, err := io.CopyBuffer(h, fh, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
