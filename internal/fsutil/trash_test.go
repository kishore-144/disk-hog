//go:build unix && !darwin

package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrashFreedesktop(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	src := filepath.Join(data, "work", "my dir")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644)
	if err := Trash(src); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "Trash", "files", "my dir", "f")); err != nil {
		t.Fatal("not in trash:", err)
	}
	info, err := os.ReadFile(filepath.Join(data, "Trash", "info", "my dir.trashinfo"))
	if err != nil || !strings.Contains(string(info), "Path="+EscapePath(src)) || !strings.Contains(string(info), "%20") {
		t.Fatalf("bad trashinfo: %q %v", info, err)
	}
	// A second item with the same name gets a unique one.
	os.MkdirAll(src, 0o755)
	if err := Trash(src); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "Trash", "files", "my dir.1")); err != nil {
		t.Fatal("collision not handled:", err)
	}
}
