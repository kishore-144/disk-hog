// Package units parses and formats the human-friendly sizes and durations
// used in configuration files and on the command line.
//
// Sizes are binary: 1 KB = 1024 bytes, the same convention du and most file
// managers on Linux use.
package units

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Size is a byte count that reads and writes as "512MB", "1.5GB" or a plain
// number in JSON.
type Size int64

// ParseSize accepts "1024", "64K", "64KB", "64KiB", "1.5G" and so on.
func ParseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return 0, nil
	}
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil || num < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	unit := strings.TrimSpace(s[i:])
	unit = strings.TrimSuffix(unit, "B")
	unit = strings.TrimSuffix(unit, "I")
	var mult float64
	switch unit {
	case "":
		mult = 1
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	case "P":
		mult = 1 << 50
	default:
		return 0, fmt.Errorf("invalid size unit in %q", s)
	}
	return int64(num * mult), nil
}

// FormatSize renders n as "0 B", "12.3 MB", "1.50 GB".
func FormatSize(n int64) string {
	if n < 0 {
		return "-" + FormatSize(-n)
	}
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	suffix := []string{"KB", "MB", "GB", "TB", "PB"}[exp]
	switch {
	case v >= 100:
		return fmt.Sprintf("%.0f %s", v, suffix)
	case v >= 10:
		return fmt.Sprintf("%.1f %s", v, suffix)
	default:
		return fmt.Sprintf("%.2f %s", v, suffix)
	}
}

// compact renders a size for config files: "512MB", "1GB", "1536KB".
func compact(n int64) string {
	for _, u := range []struct {
		s string
		v int64
	}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}} {
		if n >= u.v && n%u.v == 0 {
			return strconv.FormatInt(n/u.v, 10) + u.s
		}
	}
	return strconv.FormatInt(n, 10)
}

func (s Size) String() string { return FormatSize(int64(s)) }

// MarshalJSON writes the size in its compact human form.
func (s Size) MarshalJSON() ([]byte, error) { return json.Marshal(compact(int64(s))) }

// UnmarshalJSON accepts either a number of bytes or a size string.
func (s *Size) UnmarshalJSON(b []byte) error {
	var n int64
	if err := json.Unmarshal(b, &n); err == nil {
		*s = Size(n)
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return fmt.Errorf("size must be a number or a string like \"512MB\"")
	}
	v, err := ParseSize(str)
	if err != nil {
		return err
	}
	*s = Size(v)
	return nil
}

// Duration is a time.Duration that also understands days ("30d") and weeks
// ("2w"), and reads and writes as a string in JSON.
type Duration time.Duration

// ParseDuration accepts anything time.ParseDuration does plus "d" and "w".
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "0" {
		return 0, nil
	}
	if n := len(s) - 1; s[n] == 'd' || s[n] == 'w' {
		v, err := strconv.ParseFloat(s[:n], 64)
		if err != nil || v < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		day := 24 * time.Hour
		if s[n] == 'w' {
			day *= 7
		}
		return time.Duration(v * float64(day)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q (use e.g. 90d, 12h, 2w)", s)
	}
	return d, nil
}

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string {
	td := time.Duration(d)
	if td == 0 {
		return ""
	}
	if td%(24*time.Hour) == 0 {
		return strconv.FormatInt(int64(td/(24*time.Hour)), 10) + "d"
	}
	return td.String()
}

// MarshalJSON writes the duration as a string such as "24h" or "7d".
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON accepts a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return fmt.Errorf("duration must be a string like \"24h\" or \"7d\"")
	}
	v, err := ParseDuration(str)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}
