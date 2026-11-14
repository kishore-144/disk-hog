// Package fsutil holds the platform-specific filesystem pieces: allocated
// sizes and inode identity, filesystem usage, moving to the trash, forced
// removal, process priority and revealing a path in the file manager.
package fsutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Meta is the part of a file's metadata disk-hog needs.
type Meta struct {
	Dev, Ino uint64
	Nlink    uint64
	// Alloc is the space the file occupies on disk, which for sparse or
	// compressed files differs from its length.
	Alloc int64
	// HasID reports whether Dev and Ino are meaningful on this platform.
	HasID bool
}

// Disk describes one mounted filesystem.
type Disk struct {
	Mount  string `json:"mount"`
	Device string `json:"device"`
	FSType string `json:"fstype"`
	Total  int64  `json:"total"`
	Free   int64  `json:"free"`
	Avail  int64  `json:"avail"`
	Used   int64  `json:"used"`
}

// Within reports whether p is root or inside it. Both must be clean.
func Within(p, root string) bool {
	if p == root {
		return true
	}
	if runtime.GOOS == "windows" {
		p, root = strings.ToLower(p), strings.ToLower(root)
		if p == root {
			return true
		}
	}
	if !strings.HasSuffix(root, string(filepath.Separator)) {
		root += string(filepath.Separator)
	}
	return strings.HasPrefix(p, root)
}

// Reveal opens the system file manager at p, selecting it where supported.
func Reveal(p string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", "-R", p)
	case "windows":
		cmd = exec.Command("explorer", "/select,", p)
	default:
		// The FileManager1 D-Bus interface selects the item in Nautilus,
		// Nemo, Dolphin, Caja and Thunar; xdg-open of the parent works
		// everywhere else.
		if _, err := exec.LookPath("gdbus"); err == nil {
			uri := "file://" + (&urlPath{p}).String()
			c := exec.Command("gdbus", "call", "--session", "--dest", "org.freedesktop.FileManager1",
				"--object-path", "/org/freedesktop/FileManager1", "--method", "org.freedesktop.FileManager1.ShowItems",
				"['"+strings.ReplaceAll(uri, "'", "%27")+"']", "")
			if c.Run() == nil {
				return nil
			}
		}
		dir := p
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			dir = filepath.Dir(p)
		}
		cmd = exec.Command("xdg-open", dir)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// urlPath percent-encodes a filesystem path for a file:// URI, keeping "/".
type urlPath struct{ p string }

func (u *urlPath) String() string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(u.p); i++ {
		c := u.p[i]
		if c == '/' || c == '-' || c == '_' || c == '.' || c == '~' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}

// EscapePath is the encoding the freedesktop trash spec uses for Path=.
func EscapePath(p string) string { return (&urlPath{filepath.ToSlash(p)}).String() }
