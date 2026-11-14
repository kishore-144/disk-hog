// Package advisor reports system-level space that disk-hog does not delete
// itself because it needs root or the owning tool: package-manager caches,
// the systemd journal, rotated logs, crash dumps, old kernels and snap
// revisions. Each item comes with the exact command to run.
package advisor

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"disk-hog/internal/fsutil"
	"disk-hog/internal/walk"
)

// Item is one suggestion.
type Item struct {
	ID      string `json:"id"`
	Group   string `json:"group"`
	Title   string `json:"title"`
	Path    string `json:"path,omitempty"`
	Size    int64  `json:"size"` // -1 when unknown
	Approx  bool   `json:"approx,omitempty"`
	Command string `json:"command"`
	Note    string `json:"note,omitempty"`
}

// Collect gathers every suggestion that applies to this machine. It is
// bounded by ctx; sizes that could not be finished are marked approximate.
func Collect(ctx context.Context) []Item {
	var fns []func(context.Context) []Item
	switch runtime.GOOS {
	case "linux":
		fns = []func(context.Context) []Item{packageCaches, journal, rotatedLogs, crashes, kernels, snaps, others}
	case "darwin":
		fns = []func(context.Context) []Item{darwinItems}
	case "windows":
		fns = []func(context.Context) []Item{windowsItems}
	default:
		fns = []func(context.Context) []Item{bsdItems}
	}
	var mu sync.Mutex
	var out []Item
	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Add(1)
		go func(fn func(context.Context) []Item) {
			defer wg.Done()
			items := fn(ctx)
			mu.Lock()
			out = append(out, items...)
			mu.Unlock()
		}(fn)
	}
	wg.Wait()
	sort.SliceStable(out, func(a, b int) bool { return out[a].Size > out[b].Size })
	return out
}

// du sizes a tree, reporting approx when parts were unreadable or the
// context ran out.
func du(ctx context.Context, root string, match func(name string) bool) (size int64, approx, ok bool) {
	fi, err := os.Lstat(root)
	if err != nil {
		return 0, false, false
	}
	if !fi.IsDir() {
		return fsutil.MetaOf(fi).Alloc, false, true
	}
	var total atomic.Int64
	var bad atomic.Bool
	walk.Run(ctx, 4, []string{root}, func(dir string, push func(...string)) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			bad.Store(true)
			return
		}
		for _, e := range ents {
			p := filepath.Join(dir, e.Name())
			if e.Type()&fs.ModeSymlink != 0 {
				continue
			}
			if e.IsDir() {
				push(p)
				continue
			}
			if match != nil && !match(e.Name()) {
				continue
			}
			if info, err := e.Info(); err == nil {
				total.Add(fsutil.MetaOf(info).Alloc)
			}
		}
	})
	return total.Load(), bad.Load() || ctx.Err() != nil, true
}

func has(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

func pathItem(ctx context.Context, id, group, title, p, cmd, note string) []Item {
	size, approx, ok := du(ctx, p, nil)
	if !ok || size < 1<<20 {
		return nil
	}
	return []Item{{ID: id, Group: group, Title: title, Path: p, Size: size, Approx: approx, Command: cmd, Note: note}}
}

func packageCaches(ctx context.Context) []Item {
	type pm struct{ id, bin, title, path, cmd, note string }
	list := []pm{
		{"apt", "apt-get", "APT package cache", "/var/cache/apt/archives", "sudo apt-get clean && sudo apt-get autoremove --purge", "Downloaded .deb files and orphaned packages."},
		{"dnf", "dnf", "DNF cache", "/var/cache/dnf", "sudo dnf clean all && sudo dnf autoremove", ""},
		{"yum", "yum", "YUM cache", "/var/cache/yum", "sudo yum clean all", ""},
		{"pacman", "pacman", "pacman package cache", "/var/cache/pacman/pkg", "sudo paccache -rk1   # pacman-contrib; or: sudo pacman -Sc", "Keeps the latest version of each package."},
		{"zypper", "zypper", "zypper package cache", "/var/cache/zypp/packages", "sudo zypper clean --all", ""},
		{"apk", "apk", "apk cache", "/var/cache/apk", "sudo apk cache clean", ""},
		{"xbps", "xbps-remove", "XBPS cache", "/var/cache/xbps", "sudo xbps-remove -Oo", "Removes obsolete packages and orphans."},
		{"portage", "emerge", "Portage distfiles", "/var/cache/distfiles", "sudo eclean-dist --deep", "Needs app-portage/gentoolkit."},
		{"eopkg", "eopkg", "eopkg cache", "/var/cache/eopkg/packages", "sudo eopkg delete-cache", ""},
	}
	var out []Item
	for _, m := range list {
		if !has(m.bin) {
			continue
		}
		out = append(out, pathItem(ctx, m.id, "Packages", m.title, m.path, m.cmd, m.note)...)
	}
	if has("flatpak") {
		out = append(out, Item{ID: "flatpak", Group: "Packages", Title: "Unused Flatpak runtimes", Size: -1,
			Command: "flatpak uninstall --unused && flatpak uninstall --user --unused",
			Note:    "Runtimes no installed app needs any more."})
	}
	if _, err := os.Stat("/nix/store"); err == nil {
		out = append(out, Item{ID: "nix", Group: "Packages", Title: "Nix store garbage", Path: "/nix/store", Size: -1,
			Command: "nix-collect-garbage --delete-older-than 14d   # add sudo for system profiles",
			Note:    "Removes old generations and unreferenced store paths."})
	}
	return out
}

var usageRe = regexp.MustCompile(`take up ([0-9.]+)([KMGTP]?)`)

func journal(ctx context.Context) []Item {
	if _, err := os.Stat("/var/log/journal"); err != nil {
		return nil
	}
	size, approx, _ := du(ctx, "/var/log/journal", nil)
	if out, err := exec.CommandContext(ctx, "journalctl", "--disk-usage").Output(); err == nil {
		if m := usageRe.FindStringSubmatch(string(out)); m != nil {
			v, _ := strconv.ParseFloat(m[1], 64)
			mult := map[string]float64{"": 1, "K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40, "P": 1 << 50}[m[2]]
			size, approx = int64(v*mult), false
		}
	}
	if size == 0 {
		return nil
	}
	return []Item{{ID: "journal", Group: "Logs", Title: "systemd journal", Path: "/var/log/journal", Size: size, Approx: approx,
		Command: "sudo journalctl --vacuum-size=200M --vacuum-time=4weeks",
		Note:    "Permanent cap: set SystemMaxUse=500M in /etc/systemd/journald.conf."}}
}

var rotated = regexp.MustCompile(`\.([0-9]+|gz|xz|bz2|zst|old)$`)

func rotatedLogs(ctx context.Context) []Item {
	size, approx, ok := du(ctx, "/var/log", func(n string) bool { return rotated.MatchString(n) })
	if !ok || size < 1<<20 {
		return nil
	}
	return []Item{{ID: "rotated-logs", Group: "Logs", Title: "Rotated logs in /var/log", Path: "/var/log", Size: size, Approx: approx,
		Command: `sudo find /var/log -type f -regextype posix-extended -regex '.*\.([0-9]+|gz|xz|bz2|zst|old)$' -delete`,
		Note:    "Old, already-rotated log files. Current logs are untouched."}}
}

func crashes(ctx context.Context) []Item {
	var out []Item
	out = append(out, pathItem(ctx, "var-crash", "Crash dumps", "Crash reports (/var/crash)", "/var/crash",
		"sudo rm -f /var/crash/*", "")...)
	out = append(out, pathItem(ctx, "coredumps", "Crash dumps", "systemd-coredump files", "/var/lib/systemd/coredump",
		"sudo rm -f /var/lib/systemd/coredump/*", "List them first with: coredumpctl list")...)
	return out
}

func kernels(ctx context.Context) []Item {
	cur, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	current := strings.TrimSpace(string(cur))
	dir := "/lib/modules"
	if _, err := os.Stat(dir); err != nil {
		dir = "/usr/lib/modules"
	}
	ents, err := os.ReadDir(dir)
	if err != nil || current == "" {
		return nil
	}
	var old []string
	var total int64
	approx := false
	for _, e := range ents {
		if !e.IsDir() || e.Name() == current {
			continue
		}
		// Skip directories that only hold leftover extra modules.
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "modules.dep")); err != nil {
			continue
		}
		old = append(old, e.Name())
		s, a, _ := du(ctx, filepath.Join(dir, e.Name()), nil)
		total += s
		approx = approx || a
		boot, _ := filepath.Glob("/boot/*-" + e.Name())
		for _, b := range boot {
			if fi, err := os.Stat(b); err == nil {
				total += fi.Size()
			}
		}
	}
	// Distributions keep one fallback kernel on purpose; only suggest a
	// cleanup when there are two or more old ones.
	if len(old) < 2 {
		return nil
	}
	cmd := "sudo apt autoremove --purge"
	switch {
	case has("dnf"):
		cmd = "sudo dnf remove --oldinstallonly --setopt installonly_limit=2"
	case has("zypper"):
		cmd = "sudo zypper purge-kernels"
	case has("pacman"):
		cmd = "# pacman keeps one kernel per package; remove unused kernel packages with sudo pacman -Rns <pkg>"
	}
	return []Item{{ID: "kernels", Group: "Kernels", Title: strconv.Itoa(len(old)) + " old kernels", Size: total, Approx: approx,
		Command: cmd, Note: "Running " + current + ". Old: " + strings.Join(old, ", ") + ". Keep one fallback."}}
}

func snaps(ctx context.Context) []Item {
	files, _ := filepath.Glob("/var/lib/snapd/snaps/*.snap")
	var lines []string
	var total int64
	for _, f := range files {
		base := strings.TrimSuffix(filepath.Base(f), ".snap")
		i := strings.LastIndexByte(base, '_')
		if i < 0 {
			continue
		}
		name, rev := base[:i], base[i+1:]
		cur, err := os.Readlink(filepath.Join("/snap", name, "current"))
		if err != nil || cur == rev {
			continue
		}
		if fi, err := os.Stat(f); err == nil {
			total += fi.Size()
			lines = append(lines, "sudo snap remove "+name+" --revision="+rev)
		}
	}
	var out []Item
	if len(lines) > 0 {
		out = append(out, Item{ID: "snap-revisions", Group: "Snap", Title: strconv.Itoa(len(lines)) + " disabled snap revisions",
			Size: total, Command: strings.Join(lines, "\n"),
			Note: "Keep fewer in future: sudo snap set system refresh.retain=2"})
	}
	out = append(out, pathItem(ctx, "snap-cache", "Snap", "snapd download cache", "/var/lib/snapd/cache",
		"sudo find /var/lib/snapd/cache -type f -delete", "")...)
	return out
}

func others(ctx context.Context) []Item {
	var out []Item
	if has("podman") {
		out = append(out, Item{ID: "podman", Group: "Containers", Title: "Podman images & containers", Size: -1,
			Command: "podman system df && podman system prune --all", Note: "Also listed in the Containers panel when the Podman socket is enabled."})
	}
	return out
}

func darwinItems(ctx context.Context) []Item {
	home, _ := os.UserHomeDir()
	out := []Item{{ID: "tm-snapshots", Group: "System", Title: "Time Machine local snapshots", Size: -1,
		Command: "tmutil listlocalsnapshots / && sudo tmutil thinlocalsnapshots / 999999999999 4",
		Note:    "macOS frees these automatically under pressure, but they inflate used space."}}
	if has("brew") {
		out = append(out, Item{ID: "brew", Group: "Packages", Title: "Homebrew old versions & downloads", Size: -1,
			Command: "brew cleanup --prune=all && brew autoremove"})
	}
	if has("xcrun") {
		out = append(out, Item{ID: "simctl", Group: "Xcode", Title: "Unavailable simulators", Size: -1,
			Command: "xcrun simctl delete unavailable"})
	}
	out = append(out, pathItem(ctx, "ios-backups", "Devices", "iPhone / iPad backups",
		filepath.Join(home, "Library", "Application Support", "MobileSync", "Backup"),
		"open ~/Library/Application\\ Support/MobileSync/Backup", "Delete old device backups from Finder > device > Manage Backups.")...)
	return out
}

func windowsItems(ctx context.Context) []Item {
	root := os.Getenv("SystemRoot")
	out := []Item{
		{ID: "cleanmgr", Group: "System", Title: "Windows Disk Cleanup", Size: -1, Command: "cleanmgr /lowdisk"},
		{ID: "winsxs", Group: "System", Title: "Component store (WinSxS)", Size: -1,
			Command: "Dism.exe /Online /Cleanup-Image /StartComponentCleanup", Note: "Run in an elevated terminal."},
	}
	out = append(out, pathItem(ctx, "wu-download", "System", "Windows Update downloads",
		filepath.Join(root, "SoftwareDistribution", "Download"),
		"net stop wuauserv && rd /s /q %SystemRoot%\\SoftwareDistribution\\Download && net start wuauserv", "Run as administrator.")...)
	vhds, _ := filepath.Glob(filepath.Join(os.Getenv("LOCALAPPDATA"), "Packages", "*", "LocalState", "ext4.vhdx"))
	for _, v := range vhds {
		if fi, err := os.Stat(v); err == nil {
			out = append(out, Item{ID: "wsl-" + filepath.Base(filepath.Dir(filepath.Dir(v))), Group: "WSL", Title: "WSL disk image",
				Path: v, Size: fi.Size(), Command: "wsl --shutdown; Optimize-VHD -Path \"" + v + "\" -Mode Full",
				Note: "WSL disks grow but never shrink on their own. Optimize-VHD needs Hyper-V tools; otherwise use diskpart compact vdisk."})
		}
	}
	return out
}

func bsdItems(ctx context.Context) []Item {
	var out []Item
	if has("pkg") {
		out = append(out, pathItem(ctx, "pkg", "Packages", "pkg cache", "/var/cache/pkg", "sudo pkg clean -a && sudo pkg autoremove", "")...)
	}
	return out
}

// Timeout bounds a Collect call made on behalf of a browser.
const Timeout = 45 * time.Second
