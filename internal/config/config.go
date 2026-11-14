// Package config loads disk-hog's optional JSON configuration file.
//
// Every field has a sensible default, so the file is never required.
// Precedence is: command-line flags, then the file, then defaults.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"disk-hog/internal/rules"
	"disk-hog/internal/units"
)

// Config is the whole configuration.
type Config struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// Token is required for cleanup requests from other machines. Leave it
	// empty to have one generated at startup (printed in the log).
	Token string `json:"token,omitempty"`

	Roots         []string   `json:"roots"`
	Exclude       []string   `json:"exclude"`
	OneFileSystem bool       `json:"oneFileSystem"`
	IncludeGlobal bool       `json:"includeGlobal"`
	Workers       int        `json:"workers"`
	MinItemSize   units.Size `json:"minItemSize"`
	LargeFileSize units.Size `json:"largeFileSize"`
	DupesMinSize  units.Size `json:"dupesMinSize"`

	ReadOnly      bool   `json:"readOnly"`
	LowPriority   bool   `json:"lowPriority"`
	DefaultAction string `json:"defaultAction"` // auto, trash or delete

	Schedule    units.Duration `json:"schedule"`    // e.g. "24h"; empty disables
	NotifyAbove units.Size     `json:"notifyAbove"` // desktop notification threshold; 0 disables

	DisabledRules []string     `json:"disabledRules"`
	Rules         []rules.Rule `json:"rules"`
}

// Defaults returns the built-in configuration.
func Defaults() Config {
	return Config{
		Host:          "127.0.0.1",
		Port:          3002,
		Roots:         []string{"~"},
		Exclude:       []string{},
		OneFileSystem: true,
		IncludeGlobal: true,
		MinItemSize:   1 << 20,
		LargeFileSize: 512 << 20,
		DupesMinSize:  1 << 20,
		LowPriority:   true,
		DefaultAction: "auto",
		DisabledRules: []string{},
		Rules:         []rules.Rule{},
	}
}

// DefaultPath is where the config file lives unless --config says otherwise.
func DefaultPath() string {
	if p := os.Getenv("DISK_HOG_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "disk-hog.json"
	}
	return filepath.Join(dir, "disk-hog", "config.json")
}

// Load reads path (or the default path when empty). A missing file is not
// an error: the defaults are returned with found=false.
func Load(path string) (cfg Config, used string, found bool, err error) {
	cfg = Defaults()
	if path == "" {
		path = DefaultPath()
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, path, false, cfg.validate()
	}
	if err != nil {
		return cfg, path, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, path, true, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, path, true, cfg.validate()
}

func (c *Config) validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port %d out of range", c.Port)
	}
	if len(c.Roots) == 0 {
		c.Roots = []string{"~"}
	}
	switch c.DefaultAction {
	case "", "auto":
		c.DefaultAction = "auto"
	case "trash", "delete":
	default:
		return fmt.Errorf("defaultAction must be auto, trash or delete")
	}
	if _, err := rules.NewSet(c.Rules, c.DisabledRules, ""); err != nil {
		return err
	}
	return nil
}

// RuleSet compiles the configured rules.
func (c *Config) RuleSet() *rules.Set {
	s, _ := rules.NewSet(c.Rules, c.DisabledRules, "")
	return s
}

// ExpandRoots resolves "~" in the roots.
func (c *Config) ExpandRoots() []string {
	home, _ := os.UserHomeDir()
	out := make([]string, 0, len(c.Roots))
	for _, r := range c.Roots {
		out = append(out, Expand(r, home))
	}
	return out
}

// Expand resolves a leading "~" against home.
func Expand(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(home, p[2:])
	}
	return p
}

// Write saves cfg to path, creating the directory.
func Write(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// Set changes one top-level key in the file at path, keeping the rest.
// The value is parsed as JSON when possible, otherwise taken as a string;
// comma-separated values are accepted for list keys.
func Set(path, key, value string) error {
	cfg, _, _, err := Load(path)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(cfg)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	cur, ok := m[key]
	if !ok {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return fmt.Errorf("unknown key %q (known: %s)", key, strings.Join(keys, ", "))
	}
	var raw json.RawMessage
	switch {
	case json.Valid([]byte(value)) && !(len(cur) > 0 && cur[0] == '"' && !strings.HasPrefix(value, `"`)):
		raw = json.RawMessage(value)
	case len(cur) > 0 && cur[0] == '[':
		parts := []string{}
		for _, p := range strings.Split(value, ",") {
			if p = strings.TrimSpace(p); p != "" {
				parts = append(parts, p)
			}
		}
		raw, _ = json.Marshal(parts)
	default:
		raw = json.RawMessage(strconv.Quote(value))
	}
	m[key] = raw
	nb, _ := json.Marshal(m)
	next := Defaults()
	dec := json.NewDecoder(bytes.NewReader(nb))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&next); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	if err := next.validate(); err != nil {
		return err
	}
	return Write(path, next)
}
