// Command disk-hog finds and reclaims disk space wasted by development
// tooling: dependency folders, build output, package-manager caches, old
// SDKs, VM images, logs, duplicates and large files.
//
// Run without arguments it serves a dashboard on http://localhost:6969
// (change it with `disk-hog serve --port N` or DISK_HOG_PORT).
// Every feature is also available from the command line for servers and
// scripts; see `disk-hog help`.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"disk-hog/internal/advisor"
	"disk-hog/internal/clean"
	"disk-hog/internal/config"
	"disk-hog/internal/docker"
	"disk-hog/internal/dupes"
	"disk-hog/internal/fsutil"
	"disk-hog/internal/rules"
	"disk-hog/internal/scan"
	"disk-hog/internal/server"
	"disk-hog/internal/state"
	"disk-hog/internal/units"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `disk-hog — find and reclaim disk space wasted by dev tooling

Usage:
  disk-hog [serve] [--port N]   run the dashboard (default port 6969)
  disk-hog scan [paths...]      scan and print what can be reclaimed
  disk-hog clean [paths...]     scan, then delete matches (dry run unless --yes)
  disk-hog dupes [paths...]     find duplicate files
  disk-hog system               system-level cleanups (package caches, journal, kernels, snaps)
  disk-hog docker               Docker / Podman storage usage
  disk-hog disks                filesystem usage
  disk-hog rules                list every rule disk-hog knows
  disk-hog config <init|path|show|set KEY VALUE>
  disk-hog token                print the API token of the running server
  disk-hog health               check the running server
  disk-hog version

Filters for scan and clean:
  --category deps,build,cache,ide,toolchain,vm,trash,logs,temp,large
  --risk safe,moderate,review (clean defaults to safe,moderate)
  --rule node_modules,rust-target   --older-than 90d   --min-size 100MB
  --match <substring>               --limit N

Examples:
  disk-hog scan ~/code --older-than 60d
  disk-hog clean --category deps,build --older-than 90d --yes
  disk-hog clean --rule node_modules --trash --yes
  disk-hog scan --json > report.json
`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve", "server", "daemon":
		err = cmdServe(args)
	case "scan", "ls", "list":
		err = cmdScan(args, false)
	case "clean":
		err = cmdScan(args, true)
	case "dupes", "duplicates":
		err = cmdDupes(args)
	case "system", "sys":
		err = cmdSystem(args)
	case "docker", "containers":
		err = cmdDocker(args)
	case "disks", "df":
		err = cmdDisks()
	case "rules":
		err = cmdRules(args)
	case "config":
		err = cmdConfig(args)
	case "token":
		err = cmdToken()
	case "health", "ping", "status":
		err = cmdHealth(args)
	case "version", "--version", "-v":
		fmt.Printf("disk-hog %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "disk-hog: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "disk-hog: "+err.Error())
		os.Exit(1)
	}
}

func loadConfig(path string) (config.Config, string, error) {
	cfg, used, _, err := config.Load(path)
	return cfg, used, err
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	underSystemd := os.Getenv("JOURNAL_STREAM") != "" || os.Getenv("INVOCATION_ID") != ""
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey && underSystemd {
				return slog.Attr{}
			}
			return a
		},
	}))
}

// ---- serve -------------------------------------------------------------------

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "", "configuration file")
	host := fs.String("host", "", "bind address (default 127.0.0.1)")
	port := fs.Int("port", 0, "listen port (default 6969, or DISK_HOG_PORT, or \"port\" in the config file)")
	readOnly := fs.Bool("read-only", false, "disable all deleting")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	fs.Parse(args)
	// A bare number is accepted too: `disk-hog serve 8080`.
	if fs.NArg() == 1 && *port == 0 {
		p, err := config.ParsePort(fs.Arg(0))
		if err != nil {
			return err
		}
		*port = p
	} else if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *port != 0 && (*port < 1 || *port > 65535) {
		return fmt.Errorf("invalid port %d: must be from 1 to 65535", *port)
	}

	cfg, used, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if *host != "" {
		cfg.Host = *host
	}
	if *port != 0 {
		cfg.Port = *port
	}
	if *readOnly {
		cfg.ReadOnly = true
	}
	log := newLogger(*logLevel)
	if cfg.LowPriority {
		fsutil.LowerPriority()
	}
	// The daemon idles most of the time; collect garbage eagerly so the
	// resident set stays small between scans.
	debug.SetGCPercent(50)

	store, err := state.Open(state.Dir())
	if err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	srv, err := server.New(cfg, used, store, log, version)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("disk-hog started", "version", version, "url", srv.URL(), "config", used,
		"state", store.Path(), "readOnly", cfg.ReadOnly, "roots", cfg.ExpandRoots())
	if h := cfg.Host; h != "127.0.0.1" && h != "localhost" && h != "::1" {
		log.Warn("listening beyond loopback: other machines need the token from `disk-hog token`", "host", h)
	}
	return srv.Run(ctx)
}

// ---- scan / clean -------------------------------------------------------------

type filters struct {
	categories, risks, ruleIDs map[string]bool
	olderThan                  time.Duration
	minSize                    int64
	match                      string
	limit                      int
}

func splitSet(s string) map[string]bool {
	if s == "" || s == "all" {
		return nil
	}
	m := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			m[p] = true
		}
	}
	return m
}

func (f *filters) keep(c *scan.Candidate, now time.Time) bool {
	if f.categories != nil && !f.categories[c.Category] {
		return false
	}
	if f.risks != nil && !f.risks[c.Risk] {
		return false
	}
	if f.ruleIDs != nil && !f.ruleIDs[c.Rule] {
		return false
	}
	if c.Size < f.minSize {
		return false
	}
	if f.olderThan > 0 && c.Touched > 0 && now.Sub(time.UnixMilli(c.Touched)) < f.olderThan {
		return false
	}
	if f.match != "" && !strings.Contains(strings.ToLower(c.Path), strings.ToLower(f.match)) {
		return false
	}
	return true
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func cmdScan(args []string, cleanMode bool) error {
	name := "scan"
	if cleanMode {
		name = "clean"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cfgPath := fs.String("config", "", "configuration file")
	category := fs.String("category", "", "comma-separated categories")
	risk := fs.String("risk", "", "comma-separated risks: safe, moderate, review, all")
	rule := fs.String("rule", "", "comma-separated rule IDs (see `disk-hog rules`)")
	older := fs.String("older-than", "", "only items untouched for this long (e.g. 90d)")
	minSize := fs.String("min-size", "", "only items at least this big (e.g. 100MB)")
	match := fs.String("match", "", "only paths containing this text")
	limit := fs.Int("limit", 0, "show at most N items")
	large := fs.String("large", "", "large-file threshold (default from config, 512MB)")
	noGlobal := fs.Bool("no-global", false, "skip global caches outside the scanned paths")
	allFS := fs.Bool("cross-fs", false, "descend into other mounted filesystems")
	jsonOut := fs.Bool("json", false, "print JSON")
	csvOut := fs.Bool("csv", false, "print CSV")
	quiet := fs.Bool("quiet", false, "no progress output")
	yes := fs.Bool("yes", false, "clean: actually remove (otherwise a dry run)")
	trash := fs.Bool("trash", false, "clean: move to the trash")
	del := fs.Bool("delete", false, "clean: delete permanently")
	workers := fs.Int("workers", 0, "parallel directory readers (default auto)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(reorder(args))

	cfg, cfgUsed, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	f := filters{categories: splitSet(*category), risks: splitSet(*risk), ruleIDs: splitSet(*rule), match: *match, limit: *limit}
	if cleanMode && *risk == "" && f.ruleIDs == nil {
		f.risks = map[string]bool{rules.Safe: true, rules.Moderate: true}
	}
	if f.olderThan, err = units.ParseDuration(*older); err != nil {
		return err
	}
	if f.minSize, err = units.ParseSize(*minSize); err != nil {
		return err
	}
	if *trash && *del {
		return errors.New("choose one of --trash and --delete")
	}

	store, _ := state.Open(state.Dir())
	opt := scan.Options{
		Roots: cfg.ExpandRoots(), Exclude: cfg.Exclude, OneFileSystem: cfg.OneFileSystem && !*allFS,
		IncludeGlobal: cfg.IncludeGlobal && !*noGlobal, Workers: cfg.Workers,
		MinItemSize: int64(cfg.MinItemSize), LargeFileSize: int64(cfg.LargeFileSize), Rules: cfg.RuleSet(),
	}
	if *workers > 0 {
		opt.Workers = *workers
	}
	if store != nil {
		opt.Ignores = store.Ignores()
	}
	if fs.NArg() > 0 {
		opt.Roots = fs.Args()
	}
	if *large != "" {
		if opt.LargeFileSize, err = units.ParseSize(*large); err != nil {
			return err
		}
	}
	if cfg.LowPriority {
		fsutil.LowerPriority()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sc := scan.New(opt)
	showProgress := !*quiet && isTTY(os.Stderr)
	done := make(chan struct{})
	if showProgress {
		go func() {
			t := time.NewTicker(200 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-done:
					fmt.Fprint(os.Stderr, "\r\033[K")
					return
				case <-t.C:
					p := sc.Progress()
					cur := shorten(p.Current, 50)
					fmt.Fprintf(os.Stderr, "\r\033[Kscanning %s dirs, %s files, %s, %d found  %s",
						comma(p.Dirs), comma(p.Files), units.FormatSize(p.Bytes), p.Found, cur)
				}
			}
		}()
	}
	res := sc.Run(ctx)
	close(done)
	if res.Canceled {
		return errors.New("interrupted")
	}
	if store != nil && fs.NArg() == 0 {
		_ = store.SaveBlob("last-scan", res)
		store.Mark(res.Finished, time.Time{})
	}

	now := time.Now()
	var items []*scan.Candidate
	for _, c := range res.Items {
		if f.keep(c, now) {
			items = append(items, c)
		}
	}
	if f.limit > 0 && len(items) > f.limit {
		items = items[:f.limit]
	}

	if !cleanMode {
		switch {
		case *jsonOut:
			res.Items = items
			res.Summary = scan.Summarize(items)
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(res)
		case *csvOut:
			server.WriteCSV(os.Stdout, items)
			return nil
		}
		printItems(os.Stdout, items, res)
		return nil
	}

	// clean
	if len(items) == 0 {
		fmt.Println("Nothing matches the filters.")
		return nil
	}
	mode := cfg.DefaultAction
	if *trash {
		mode = clean.Trash
	}
	if *del {
		mode = clean.Delete
	}
	printItems(os.Stdout, items, nil)
	var total int64
	for _, c := range items {
		total += c.Size
	}
	if !*yes {
		fmt.Printf("\nDry run: %d items, %s. Add --yes to %s them.\n", len(items), units.FormatSize(total), describeMode(mode))
		return nil
	}
	if cfg.ReadOnly {
		return errors.New("read-only mode is on in " + cfgUsed)
	}
	var exact []string
	for _, c := range res.Items {
		if c.Global {
			exact = append(exact, c.Path)
		}
	}
	home, _ := os.UserHomeDir()
	guard := clean.NewGuard(home, res.Roots, exact, []string{state.Dir(), filepath.Dir(cfgUsed)})
	var freed, moved int64
	var failed int
	var entries []state.Entry
	for _, c := range items {
		m := mode
		if c.Contents {
			m = clean.Delete
		} else if m == "auto" {
			m = clean.Trash
			if c.Risk == rules.Safe {
				m = clean.Delete
			}
		}
		err := clean.Run(guard, clean.Target{Path: c.Path, Kind: c.Kind, Contents: c.Contents, Dev: c.Dev, Ino: c.Ino, Size: c.Size}, m)
		e := state.Entry{Time: time.Now(), Path: c.Path, Rule: c.Rule, Size: c.Size, Mode: m, OK: err == nil}
		if err != nil {
			failed++
			e.Error = err.Error()
			fmt.Fprintf(os.Stderr, "  failed  %s: %v\n", c.Path, err)
		} else if m == clean.Delete {
			freed += c.Size
		} else {
			moved += c.Size
		}
		entries = append(entries, e)
	}
	if store != nil {
		_ = store.Record(entries...)
	}
	fmt.Printf("\nFreed %s", units.FormatSize(freed))
	if moved > 0 {
		fmt.Printf(", moved %s to the trash (empty it to free the space)", units.FormatSize(moved))
	}
	if failed > 0 {
		fmt.Printf(", %d failed", failed)
	}
	fmt.Println(".")
	return nil
}

// reorder lets flags follow positional arguments: `scan ~/code --json`.
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(name, "=") && !boolFlags[name] && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

var boolFlags = map[string]bool{"no-global": true, "cross-fs": true, "json": true, "csv": true, "quiet": true,
	"yes": true, "trash": true, "delete": true, "all": true, "refresh": true, "h": true, "help": true}

func describeMode(m string) string {
	switch m {
	case clean.Trash:
		return "trash"
	case clean.Delete:
		return "permanently delete"
	}
	return "clean (safe items deleted, others trashed)"
}

func comma(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func shorten(p string, n int) string {
	if home, _ := os.UserHomeDir(); home != "" && strings.HasPrefix(p, home) {
		p = "~" + p[len(home):]
	}
	if len(p) > n {
		return "…" + p[len(p)-n+1:]
	}
	return p
}

func age(ms int64, now time.Time) string {
	if ms <= 0 {
		return "-"
	}
	d := now.Sub(time.UnixMilli(ms))
	switch {
	case d < time.Hour:
		return "now"
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d < 2*365*24*time.Hour:
		return fmt.Sprintf("%dmo", int(d.Hours()/24/30))
	}
	return fmt.Sprintf("%dy", int(d.Hours()/24/365))
}

func printItems(w io.Writer, items []*scan.Candidate, res *scan.Result) {
	now := time.Now()
	color := isTTY(os.Stdout) && os.Getenv("NO_COLOR") == ""
	paint := func(code, s string) string {
		if !color {
			return s
		}
		return "\033[" + code + "m" + s + "\033[0m"
	}
	riskColor := map[string]string{rules.Safe: "32", rules.Moderate: "33", rules.Review: "31"}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SIZE\tRISK\tAGE\tWHAT\tPATH")
	for _, c := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", units.FormatSize(c.Size), paint(riskColor[c.Risk], c.Risk),
			age(c.Touched, now), c.Name, shorten(c.Path, 200))
	}
	tw.Flush()
	if res == nil {
		return
	}
	var total int64
	for _, c := range items {
		total += c.Size
	}
	fmt.Fprintf(w, "\n%s in %d items", paint("1", units.FormatSize(total)), len(items))
	if len(items) != len(res.Items) {
		fmt.Fprintf(w, " (of %s in %d)", units.FormatSize(res.Total), len(res.Items))
	}
	fmt.Fprintf(w, ". Scanned %s dirs, %s files, %s in %.1fs",
		comma(res.Stats.Dirs), comma(res.Stats.Files), units.FormatSize(res.Stats.Bytes), res.Duration)
	if res.Stats.Errors > 0 {
		fmt.Fprintf(w, ", %d unreadable", res.Stats.Errors)
	}
	fmt.Fprintln(w, ".")
	sum := scan.Summarize(items)
	if len(sum) > 1 {
		fmt.Fprintln(w)
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, s := range sum {
			fmt.Fprintf(tw, "  %s\t%s\t%d items\n", s.Label, units.FormatSize(s.Size), s.Count)
		}
		tw.Flush()
	}
	if len(res.Small) > 0 {
		var n int
		var b int64
		for _, s := range res.Small {
			n += s.Count
			b += s.Size
		}
		fmt.Fprintf(w, "\nPlus %d smaller items (%s) under the minimum item size.\n", n, units.FormatSize(b))
	}
}

// ---- dupes ------------------------------------------------------------------------

func cmdDupes(args []string) error {
	fs := flag.NewFlagSet("dupes", flag.ExitOnError)
	cfgPath := fs.String("config", "", "configuration file")
	minSize := fs.String("min-size", "", "ignore files smaller than this (default 1MB)")
	all := fs.Bool("all", false, "also look inside node_modules, .git, venvs and similar")
	jsonOut := fs.Bool("json", false, "print JSON")
	limit := fs.Int("limit", 50, "show at most N groups")
	fs.Parse(reorder(args))
	cfg, _, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	opt := dupes.Options{Roots: cfg.ExpandRoots(), Exclude: cfg.Exclude, MinSize: int64(cfg.DupesMinSize),
		OneFileSystem: cfg.OneFileSystem, Workers: cfg.Workers, SkipDirs: dupes.DefaultSkip}
	if fs.NArg() > 0 {
		opt.Roots = fs.Args()
	}
	if *minSize != "" {
		if opt.MinSize, err = units.ParseSize(*minSize); err != nil {
			return err
		}
	}
	if *all {
		opt.SkipDirs = nil
	}
	if cfg.LowPriority {
		fsutil.LowerPriority()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	f := dupes.New(opt)
	done := make(chan struct{})
	if isTTY(os.Stderr) {
		go func() {
			t := time.NewTicker(250 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-done:
					fmt.Fprint(os.Stderr, "\r\033[K")
					return
				case <-t.C:
					p := f.Progress()
					fmt.Fprintf(os.Stderr, "\r\033[K%s: %s files, %d/%d hashed, %s read", p.Phase, comma(p.Files), p.Hashed, p.ToHash, units.FormatSize(p.Bytes))
				}
			}
		}()
	}
	res := f.Run(ctx)
	close(done)
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	for i, g := range res.Groups {
		if i >= *limit {
			fmt.Printf("… %d more groups\n", len(res.Groups)-i)
			break
		}
		fmt.Printf("%s × %d  (%s wasted)\n", units.FormatSize(g.Size), len(g.Files), units.FormatSize(g.Wasted))
		for _, fl := range g.Files {
			fmt.Printf("    %s\n", shorten(fl.Path, 200))
		}
	}
	fmt.Printf("\n%d duplicate groups, %s wasted. Scanned %s files in %.1fs.\n",
		len(res.Groups), units.FormatSize(res.Wasted), comma(res.Stats.Files), res.Duration)
	return nil
}

// ---- system, docker, disks, rules -------------------------------------------------

func cmdSystem(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), advisor.Timeout)
	defer cancel()
	items := advisor.Collect(ctx)
	if len(items) == 0 {
		fmt.Println("No system-level cleanups found.")
		return nil
	}
	for _, it := range items {
		size := "?"
		if it.Size >= 0 {
			size = units.FormatSize(it.Size)
			if it.Approx {
				size = "≥" + size
			}
		}
		fmt.Printf("%-10s %s  [%s]\n", size, it.Title, it.Group)
		if it.Note != "" {
			fmt.Printf("           %s\n", it.Note)
		}
		for _, l := range strings.Split(it.Command, "\n") {
			fmt.Printf("           $ %s\n", l)
		}
		fmt.Println()
	}
	return nil
}

func cmdDocker(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	engines, errs := docker.Detect(ctx)
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, "  "+e)
	}
	if len(engines) == 0 {
		fmt.Println("No Docker or Podman engine reachable.")
		return nil
	}
	for _, e := range engines {
		u := e.Usage(ctx)
		fmt.Printf("%s  (%s)\n", u.Engine, u.Socket)
		if u.Error != "" {
			fmt.Println("  error:", u.Error)
			continue
		}
		fmt.Printf("  images      %3d  %10s   unused %d (%s), dangling %d (%s)\n", u.Images, units.FormatSize(u.ImagesSize),
			u.UnusedImages, units.FormatSize(u.UnusedSize), u.Dangling, units.FormatSize(u.DanglingSize))
		fmt.Printf("  containers  %3d               stopped %d (%s)\n", u.Containers, u.Stopped, units.FormatSize(u.StoppedSize))
		fmt.Printf("  volumes     %3d  %10s   unused %d (%s)\n", u.Volumes, units.FormatSize(u.VolumesSize), u.UnusedVolumes, units.FormatSize(u.UnusedVolSize))
		fmt.Printf("  build cache      %10s   reclaimable %s\n", units.FormatSize(u.BuildCache), units.FormatSize(u.BuildReclaim))
	}
	fmt.Println("\nPrune from the dashboard, or: docker system prune [--all] [--volumes]")
	return nil
}

func cmdDisks() error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MOUNT\tSIZE\tUSED\tAVAIL\tUSE%\tTYPE\tDEVICE")
	for _, d := range fsutil.Disks() {
		pct := 0.0
		if d.Total > 0 {
			pct = float64(d.Used) / float64(d.Total) * 100
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.0f%%\t%s\t%s\n", d.Mount, units.FormatSize(d.Total), units.FormatSize(d.Used),
			units.FormatSize(d.Avail), pct, d.FSType, d.Device)
	}
	return tw.Flush()
}

func cmdRules(args []string) error {
	cfg, _, err := loadConfig("")
	if err != nil {
		return err
	}
	set := cfg.RuleSet()
	list := append([]*rules.Rule(nil), set.All...)
	sort.SliceStable(list, func(a, b int) bool { return list[a].Category < list[b].Category })
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tCATEGORY\tRISK\tKIND\tMATCHES")
	for _, r := range list {
		m := strings.Join(r.Dirs, " ")
		if r.Kind() == "global" {
			m = strings.Join(r.Paths, " ")
		} else if r.Kind() == "file" {
			m = strings.Join(r.Files, " ") + " ≥" + units.FormatSize(int64(r.MinSize))
		}
		if len(r.Siblings) > 0 {
			m += "  (next to " + strings.Join(r.Siblings, "|") + ")"
		}
		if len(m) > 110 {
			m = m[:107] + "..."
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Category, r.Risk, r.Kind(), m)
	}
	return tw.Flush()
}

// ---- config, token, health ----------------------------------------------------------

func cmdConfig(args []string) error {
	path := config.DefaultPath()
	if len(args) == 0 {
		args = []string{"show"}
	}
	switch args[0] {
	case "path":
		fmt.Println(path)
	case "init":
		force := len(args) > 1 && args[1] == "--force"
		if _, err := os.Stat(path); err == nil && !force {
			fmt.Println("exists:", path)
			return nil
		}
		if err := config.Write(path, config.Defaults()); err != nil {
			return err
		}
		fmt.Println("wrote", path)
	case "show":
		cfg, used, err := loadConfig("")
		if err != nil {
			return err
		}
		cfg.Token = ""
		fmt.Fprintln(os.Stderr, "# "+used)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(cfg)
	case "set":
		if len(args) != 3 {
			return errors.New("usage: disk-hog config set KEY VALUE")
		}
		if err := config.Set(path, args[1], args[2]); err != nil {
			return err
		}
		fmt.Printf("set %s in %s (restart the service to apply)\n", args[1], path)
	default:
		return fmt.Errorf("unknown config command %q", args[0])
	}
	return nil
}

func cmdToken() error {
	b, err := os.ReadFile(filepath.Join(state.Dir(), "token"))
	if err != nil {
		return errors.New("no token yet: start the server first")
	}
	fmt.Print(string(b))
	return nil
}

func cmdHealth(args []string) error {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	url := fs.String("url", "", "server URL (default from config)")
	port := fs.Int("port", 0, "check this port instead of the configured one")
	wait := fs.Duration("wait", 0, "keep retrying for this long")
	fs.Parse(args)
	if *url == "" {
		cfg, _, err := loadConfig("")
		if err != nil {
			return err
		}
		if *port != 0 {
			cfg.Port = *port
		}
		host := cfg.Host
		if host == "0.0.0.0" || host == "::" || host == "" {
			host = "127.0.0.1"
		}
		*url = fmt.Sprintf("http://%s:%d", host, cfg.Port)
		if strings.Contains(host, ":") {
			*url = fmt.Sprintf("http://[%s]:%d", host, cfg.Port)
		}
	}
	deadline := time.Now().Add(*wait)
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		resp, err := client.Get(*url + "/api/health")
		if err == nil {
			var v struct {
				OK      bool
				Version string
			}
			json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
			if v.OK {
				fmt.Printf("ok: disk-hog %s at %s\n", v.Version, *url)
				return nil
			}
			err = fmt.Errorf("unexpected response %s", resp.Status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not reachable at %s: %v", *url, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
