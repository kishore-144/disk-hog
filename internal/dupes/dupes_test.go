package dupes

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDupes(t *testing.T) {
	root := t.TempDir()
	same := make([]byte, 100<<10)
	for i := range same {
		same[i] = byte(i)
	}
	diffEnd := append([]byte(nil), same...)
	diffEnd[50<<10] ^= 1 // same size, same head and tail, different middle
	os.WriteFile(filepath.Join(root, "a"), same, 0o644)
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "sub", "b"), same, 0o644)
	os.WriteFile(filepath.Join(root, "c"), diffEnd, 0o644)
	os.Link(filepath.Join(root, "a"), filepath.Join(root, "hardlink"))
	os.MkdirAll(filepath.Join(root, "node_modules"), 0o755)
	os.WriteFile(filepath.Join(root, "node_modules", "d"), same, 0o644)

	res := New(Options{Roots: []string{root}, MinSize: 1024, SkipDirs: DefaultSkip}).Run(context.Background())
	if len(res.Groups) != 1 {
		t.Fatalf("want 1 group, got %d: %+v", len(res.Groups), res.Groups)
	}
	if n := len(res.Groups[0].Files); n != 2 {
		t.Fatalf("want 2 files (hard link and node_modules excluded), got %d: %+v", n, res.Groups[0].Files)
	}
	if res.Wasted != int64(len(same)) {
		t.Errorf("wasted %d", res.Wasted)
	}
}
