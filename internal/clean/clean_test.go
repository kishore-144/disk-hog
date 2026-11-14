package clean

import (
	"os"
	"path/filepath"
	"testing"

	"disk-hog/internal/fsutil"
)

func target(t *testing.T, p string) Target {
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	m := fsutil.MetaOf(fi)
	kind := "file"
	if fi.IsDir() {
		kind = "dir"
	}
	return Target{Path: p, Kind: kind, Dev: m.Dev, Ino: m.Ino}
}

func TestGuard(t *testing.T) {
	root := t.TempDir()
	nm := filepath.Join(root, "proj", "node_modules")
	os.MkdirAll(filepath.Join(nm, "x"), 0o755)
	g := NewGuard("/nonexistent", []string{root}, nil, nil)

	tg := target(t, nm)
	if err := g.Check(tg); err != nil {
		t.Fatalf("valid target refused: %v", err)
	}
	// The root itself and anything outside it are refused.
	if err := g.Check(target(t, root)); err == nil {
		t.Error("root accepted")
	}
	if err := g.Check(Target{Path: "/etc", Kind: "dir"}); err == nil {
		t.Error("/etc accepted")
	}
	if err := g.Check(Target{Path: root + "/proj/../proj/node_modules", Kind: "dir"}); err == nil {
		t.Error("non-canonical path accepted")
	}
	// Swapped for a symlink after the scan. The original is renamed, not
	// deleted, so its inode number cannot be reused by the replacement.
	os.Rename(nm, nm+".orig")
	os.Symlink("/", nm)
	if err := g.Check(tg); err == nil {
		t.Error("symlink swap accepted")
	}
	os.Remove(nm)
	// Replaced by a different directory.
	os.MkdirAll(nm, 0o755)
	if err := g.Check(tg); err == nil {
		t.Error("replaced directory accepted")
	}
	os.RemoveAll(nm)
	if err := Run(g, tg, Delete); err != nil {
		t.Errorf("gone target should be a no-op: %v", err)
	}
}

func TestDeleteReadOnlyTree(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "mod", "pkg@v1")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "f.go"), []byte("x"), 0o444)
	os.Chmod(d, 0o555)
	g := NewGuard("/nonexistent", []string{root}, nil, nil)
	if err := Run(g, target(t, filepath.Join(root, "mod")), Delete); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "mod")); !os.IsNotExist(err) {
		t.Error("read-only tree not removed")
	}
}

func TestEmptyContents(t *testing.T) {
	root := t.TempDir()
	tr := filepath.Join(root, "Trash")
	os.MkdirAll(filepath.Join(tr, "files", "a"), 0o755)
	os.WriteFile(filepath.Join(tr, "info.txt"), []byte("x"), 0o644)
	g := NewGuard("/nonexistent", nil, []string{tr}, nil)
	tg := target(t, tr)
	tg.Contents = true
	if err := Run(g, tg, Delete); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(tr)
	if len(ents) != 0 {
		t.Errorf("trash not emptied: %v", ents)
	}
	if _, err := os.Stat(tr); err != nil {
		t.Error("trash directory itself removed")
	}
}
