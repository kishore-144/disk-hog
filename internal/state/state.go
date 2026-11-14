// Package state persists the small amount of data disk-hog keeps between
// runs: the ignore list, a cleanup history and the last scan result.
package state

import (
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
)

// Entry is one cleanup action.
type Entry struct {
	Time  time.Time `json:"time"`
	Path  string    `json:"path"`
	Rule  string    `json:"rule,omitempty"`
	Size  int64     `json:"size"`
	Mode  string    `json:"mode"`
	OK    bool      `json:"ok"`
	Error string    `json:"error,omitempty"`
}

const maxHistory = 1000

// Store is the persisted state, safe for concurrent use.
type Store struct {
	mu   sync.Mutex
	dir  string
	data struct {
		Ignores    []string  `json:"ignores"`
		History    []Entry   `json:"history"`
		Freed      int64     `json:"freed"`
		LastScan   time.Time `json:"lastScan"`
		LastNotify time.Time `json:"lastNotify"`
	}
}

// Dir is the platform's per-user state directory for disk-hog.
func Dir() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "disk-hog")
		}
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "disk-hog")
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "disk-hog")
	}
	return filepath.Join(home, ".local", "state", "disk-hog")
}

// Open loads the store from dir, creating it on first use.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		_ = json.Unmarshal(b, &s.data)
	}
	return s, nil
}

// Path returns the store's directory.
func (s *Store) Path() string { return s.dir }

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.dir, "state.json"), b)
}

func writeAtomic(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Ignores returns the ignored paths.
func (s *Store) Ignores() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.data.Ignores...)
}

// SetIgnore adds or removes an ignored path.
func (s *Store) SetIgnore(p string, on bool) error {
	p = filepath.Clean(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.data.Ignores[:0]
	for _, x := range s.data.Ignores {
		if x != p {
			out = append(out, x)
		}
	}
	if on {
		out = append(out, p)
		sort.Strings(out)
	}
	s.data.Ignores = out
	return s.saveLocked()
}

// Record appends cleanup entries.
func (s *Store) Record(es ...Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range es {
		if e.OK && e.Mode == "delete" {
			s.data.Freed += e.Size
		}
	}
	s.data.History = append(s.data.History, es...)
	if n := len(s.data.History); n > maxHistory {
		s.data.History = append([]Entry(nil), s.data.History[n-maxHistory:]...)
	}
	return s.saveLocked()
}

// History returns the newest entries first, plus total bytes freed.
func (s *Store) History() ([]Entry, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, len(s.data.History))
	for i, e := range s.data.History {
		out[len(out)-1-i] = e
	}
	return out, s.data.Freed
}

// Mark records the time of a scan or notification.
func (s *Store) Mark(scan, notify time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !scan.IsZero() {
		s.data.LastScan = scan
	}
	if !notify.IsZero() {
		s.data.LastNotify = notify
	}
	_ = s.saveLocked()
}

// Times returns the last scan and notification times.
func (s *Store) Times() (scan, notify time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.LastScan, s.data.LastNotify
}

// SaveBlob writes v as gzipped JSON under name.
func (s *Store) SaveBlob(name string, v any) error {
	p := filepath.Join(s.dir, name+".json.gz")
	f, err := os.OpenFile(p+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	err = json.NewEncoder(zw).Encode(v)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(p + ".tmp")
		return err
	}
	return os.Rename(p+".tmp", p)
}

// LoadBlob reads gzipped JSON written by SaveBlob.
func (s *Store) LoadBlob(name string, v any) error {
	f, err := os.Open(filepath.Join(s.dir, name+".json.gz"))
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()
	return json.NewDecoder(zr).Decode(v)
}
