package rules

import (
	"path/filepath"
	"testing"
)

func TestBuiltinRulesValid(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows", "freebsd"} {
		if _, err := NewSet(nil, nil, goos); err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
	}
}

func TestExpand(t *testing.T) {
	env := Env{Home: "/h", Cache: "/h/.cache", Getenv: func(k string) string {
		return map[string]string{"GOPATH": "/gp:/other"}[k]
	}}
	cases := map[string]string{
		"$GOMODCACHE|$GOPATH/pkg/mod|{home}/go/pkg/mod": "/gp/pkg/mod",
		"$NOPE|{cache}/pip":                             "/h/.cache/pip",
		"{localappdata}/x":                              "",
	}
	for in, want := range cases {
		if got := env.expand(in); got != filepath.FromSlash(want) && !(want == "" && got == "") {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}

func TestMatchFile(t *testing.T) {
	s, _ := NewSet(nil, nil, "linux")
	if r := s.MatchFile("server.LOG", 60<<20, 0); r == nil || r.ID != "big-logs" {
		t.Errorf("log not matched: %v", r)
	}
	if r := s.MatchFile("server.log", 1<<20, 0); r != nil {
		t.Errorf("small log matched: %v", r.ID)
	}
	if r := s.MatchFile("movie.mkv", 2<<30, 1<<30); r == nil || r.ID != "large-file" {
		t.Errorf("large file not matched")
	}
	if r := s.MatchFile("core.42", 2<<20, 0); r == nil || r.ID != "core-dumps" {
		t.Errorf("core dump not matched")
	}
}

func TestCustomRuleOverridesAndDisable(t *testing.T) {
	s, err := NewSet([]Rule{{ID: "my-out", Name: "out", Category: CatBuild, Risk: Safe, Dirs: []string{"out"}}}, []string{"node_modules"}, "linux")
	if err != nil {
		t.Fatal(err)
	}
	yes := func(string) bool { return true }
	if r := s.MatchDir("out", yes, yes); r == nil || r.ID != "my-out" {
		t.Errorf("custom rule not used: %v", r)
	}
	if r := s.MatchDir("node_modules", yes, yes); r != nil {
		t.Errorf("disabled rule matched")
	}
}
