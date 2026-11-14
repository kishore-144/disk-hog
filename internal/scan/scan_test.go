package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"disk-hog/internal/rules"
)

func write(t *testing.T, p string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) string {
	root := t.TempDir()
	// JS project with node_modules and a build dir.
	write(t, filepath.Join(root, "web", "package.json"), 10)
	write(t, filepath.Join(root, "web", "node_modules", "a", "index.js"), 2<<20)
	write(t, filepath.Join(root, "web", "node_modules", "a", "node_modules", "b", "x.js"), 100<<10)
	write(t, filepath.Join(root, "web", ".next", "cache.bin"), 50<<10)
	// Rust project.
	write(t, filepath.Join(root, "rs", "Cargo.toml"), 10)
	write(t, filepath.Join(root, "rs", "target", "debug", "app"), 300<<10)
	// "target" without Cargo.toml must NOT match.
	write(t, filepath.Join(root, "notes", "target", "plan.txt"), 300<<10)
	// Python.
	write(t, filepath.Join(root, "py", "pyproject.toml"), 10)
	write(t, filepath.Join(root, "py", ".venv", "pyvenv.cfg"), 10)
	write(t, filepath.Join(root, "py", ".venv", "lib", "x.so"), 150<<10)
	write(t, filepath.Join(root, "py", "pkg", "__pycache__", "m.pyc"), 20<<10)
	// "venv" without pyvenv.cfg must NOT match.
	write(t, filepath.Join(root, "py", "venv", "readme.md"), 10)
	// Protected by the keep marker.
	write(t, filepath.Join(root, "kept", KeepMarker), 0)
	write(t, filepath.Join(root, "kept", "app", "package.json"), 10)
	write(t, filepath.Join(root, "kept", "app", "node_modules", "z.js"), 100<<10)
	// Installed editor extension: its node_modules is part of the program.
	write(t, filepath.Join(root, ".vscode", "extensions", "ext", "package.json"), 10)
	write(t, filepath.Join(root, ".vscode", "extensions", "ext", "node_modules", "dep.js"), 100<<10)
	// Files: a large file and a core dump.
	write(t, filepath.Join(root, "big.iso"), 2<<20)
	write(t, filepath.Join(root, "crash", "core.1234"), 1<<20+1)
	return root
}

func run(t *testing.T, root string, ignores ...string) *Result {
	t.Helper()
	set, err := rules.NewSet(nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{
		Roots: []string{root}, Rules: set, OneFileSystem: true, MinItemSize: 1,
		LargeFileSize: 1 << 20, Ignores: ignores,
		Env: rules.Env{Home: "/nonexistent-home", Getenv: func(string) string { return "" }},
	})
	return s.Run(context.Background())
}

func byRule(res *Result) map[string][]*Candidate {
	m := map[string][]*Candidate{}
	for _, c := range res.Items {
		m[c.Rule] = append(m[c.Rule], c)
	}
	return m
}

func TestScanFindsProjectArtifacts(t *testing.T) {
	root := fixture(t)
	res := run(t, root)
	m := byRule(res)

	expect := map[string]string{
		"node_modules": filepath.Join(root, "web", "node_modules"),
		"js-cache":     filepath.Join(root, "web", ".next"),
		"rust-target":  filepath.Join(root, "rs", "target"),
		"python-venv":  filepath.Join(root, "py", ".venv"),
		"pycache":      filepath.Join(root, "py", "pkg", "__pycache__"),
		"large-file":   filepath.Join(root, "big.iso"),
		"core-dumps":   filepath.Join(root, "crash", "core.1234"),
	}
	for rule, path := range expect {
		got := m[rule]
		if len(got) != 1 || got[0].Path != path {
			t.Errorf("rule %s: want %s, got %+v", rule, path, got)
		}
	}
	if len(m["node_modules"]) == 1 && m["node_modules"][0].Size < 300<<10 {
		t.Errorf("node_modules should include nested modules, size %d", m["node_modules"][0].Size)
	}
	for _, c := range res.Items {
		switch {
		case c.Path == filepath.Join(root, "notes", "target"):
			t.Errorf("target without Cargo.toml matched")
		case c.Path == filepath.Join(root, "py", "venv"):
			t.Errorf("venv without pyvenv.cfg matched")
		case filepath.Dir(c.Path) == filepath.Join(root, "kept", "app"):
			t.Errorf("keep marker ignored: %s", c.Path)
		case filepath.Base(filepath.Dir(c.Path)) == "ext":
			t.Errorf("editor extension flagged: %s", c.Path)
		}
	}
	if res.Tree == nil || res.Tree.Size < 1<<20 {
		t.Fatalf("tree not sized: %+v", res.Tree)
	}
}

func TestIgnore(t *testing.T) {
	root := fixture(t)
	res := run(t, root, filepath.Join(root, "web"))
	for _, c := range res.Items {
		if filepath.Dir(c.Path) == filepath.Join(root, "web") {
			t.Errorf("ignored path reported: %s", c.Path)
		}
	}
}

func TestHardLinksCountOnce(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a", "f"), 1<<20)
	if err := os.Link(filepath.Join(root, "a", "f"), filepath.Join(root, "b")); err != nil {
		t.Skip("hard links unsupported")
	}
	res := run(t, root)
	if res.Tree.Size > 1<<20+64<<10 {
		t.Errorf("hard link counted twice: %d", res.Tree.Size)
	}
}

func TestView(t *testing.T) {
	root := fixture(t)
	res := run(t, root)
	v, ok := res.View(filepath.Join(root, "web"))
	if !ok {
		t.Fatal("view not found")
	}
	found := false
	for _, k := range v.Kids {
		if k.Name == "node_modules" && k.Cand > 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("node_modules not shown as candidate in explorer: %+v", v.Kids)
	}
}
