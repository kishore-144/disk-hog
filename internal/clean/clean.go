// Package clean deletes or trashes what a scan found, after re-checking that
// the path on disk is still the thing that was scanned.
//
// The browser never names a path to delete: it sends the ID of an item from
// the last scan, the server looks up the recorded target, and Guard
// validates it again here. That rules out path injection from a malicious
// page and catches the race where a directory was swapped for a symlink
// between scanning and cleaning.
package clean

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"disk-hog/internal/fsutil"
)

// Modes.
const (
	Trash  = "trash"
	Delete = "delete"
)

// ErrGone means the target no longer exists, which is not a failure.
var ErrGone = errors.New("already gone")

// Target is a path recorded by a scan.
type Target struct {
	Path     string
	Kind     string // "dir" or "file"
	Contents bool   // empty the directory instead of removing it
	Dev, Ino uint64
	Size     int64
}

// Guard decides whether a target may be removed.
type Guard struct {
	roots     []string        // targets must lie strictly inside one of these...
	exact     map[string]bool // ...or be one of these exact paths (global caches)
	protected []string
}

// NewGuard builds a guard. roots are the scan roots, exact the global rule
// paths that were scanned, extra any additional protected paths.
func NewGuard(home string, roots, exact, extra []string) *Guard {
	g := &Guard{exact: map[string]bool{}}
	for _, r := range roots {
		g.roots = append(g.roots, filepath.Clean(r))
	}
	for _, e := range exact {
		g.exact[filepath.Clean(e)] = true
	}
	prot := []string{string(filepath.Separator)}
	if runtime.GOOS == "windows" {
		prot = append(prot, os.Getenv("SystemDrive")+`\`, os.Getenv("SystemRoot"), os.Getenv("ProgramFiles"),
			os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramData"), os.Getenv("APPDATA"), os.Getenv("LOCALAPPDATA"))
	} else {
		for _, d := range []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/libx32", "/nix",
			"/opt", "/proc", "/root", "/run", "/sbin", "/snap", "/srv", "/sys", "/usr", "/var", "/tmp",
			"/System", "/Applications", "/Library", "/private", "/Users", "/home", "/Volumes", "/mnt", "/media"} {
			prot = append(prot, d)
		}
	}
	if home != "" {
		prot = append(prot, home)
		for _, d := range []string{"Desktop", "Documents", "Downloads", "Music", "Pictures", "Videos", "Movies",
			"Public", "Templates", "Library", "AppData", ".ssh", ".gnupg", ".config", ".local", ".local/share",
			".local/state", ".cache", "snap", ".var", ".var/app", "go", ".cargo", ".rustup", ".npm", ".m2", ".gradle",
			".android", ".docker", ".kube", ".aws", ".password-store", "workspace", "Workspace", "WorkSpace",
			"Projects", "projects", "repos", "Repos", "git", "src", "code", "dev"} {
			prot = append(prot, filepath.Join(home, filepath.FromSlash(d)))
		}
	}
	if c, err := os.UserConfigDir(); err == nil {
		prot = append(prot, c)
	}
	if c, err := os.UserCacheDir(); err == nil {
		prot = append(prot, c)
	}
	prot = append(prot, extra...)
	for _, p := range prot {
		if p != "" {
			g.protected = append(g.protected, filepath.Clean(p))
		}
	}
	return g
}

// Check validates t against the guard and the live filesystem.
func (g *Guard) Check(t Target) error {
	p := t.Path
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return fmt.Errorf("refusing non-canonical path %q", p)
	}
	for _, q := range g.protected {
		if p == q || (runtime.GOOS == "windows" && strings.EqualFold(p, q)) {
			// A protected path may still be emptied (the Trash dir) but
			// never removed itself.
			if t.Contents && g.exact[p] {
				break
			}
			return fmt.Errorf("refusing to remove protected path %s", p)
		}
		if fsutil.Within(q, p) && q != p {
			return fmt.Errorf("refusing to remove %s: it contains %s", p, q)
		}
	}
	allowed := g.exact[p]
	for _, r := range g.roots {
		if p != r && fsutil.Within(p, r) {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("refusing to remove %s: outside the scanned locations", p)
	}
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return ErrGone
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to remove %s: it is now a symlink", p)
	}
	if (t.Kind == "dir") != fi.IsDir() {
		return fmt.Errorf("refusing to remove %s: its type changed since the scan", p)
	}
	if m := fsutil.MetaOf(fi); m.HasID && t.Ino != 0 && (m.Dev != t.Dev || m.Ino != t.Ino) {
		return fmt.Errorf("refusing to remove %s: it was replaced since the scan", p)
	}
	return nil
}

// Apply removes t in the given mode. Call Check first.
func Apply(t Target, mode string) error {
	switch mode {
	case Trash:
		if t.Contents {
			return errors.New("this item can only be emptied permanently")
		}
		return fsutil.Trash(t.Path)
	case Delete:
		if t.Contents {
			return fsutil.RemoveContents(t.Path)
		}
		if t.Kind == "dir" {
			return fsutil.RemoveAll(t.Path)
		}
		if err := os.Remove(t.Path); err != nil {
			return fsutil.RemoveAll(t.Path)
		}
		return nil
	}
	return fmt.Errorf("unknown mode %q", mode)
}

// Run checks and applies in one step. A target that is already gone counts
// as done.
func Run(g *Guard, t Target, mode string) error {
	if err := g.Check(t); err != nil {
		if errors.Is(err, ErrGone) {
			return nil
		}
		return err
	}
	return Apply(t, mode)
}
