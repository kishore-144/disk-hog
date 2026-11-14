// Package server exposes disk-hog over HTTP: the embedded dashboard, a JSON
// API and a Server-Sent Events stream for live progress.
//
// Security model: the server binds to loopback by default. Every
// state-changing request needs a per-process token that is only handed to
// pages loaded from a loopback host name, so a web page on another origin,
// including a DNS-rebinding attack, cannot trigger a cleanup. When bound to
// a LAN address every API call from elsewhere needs the token, which the
// user copies from `disk-hog token`.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"disk-hog/internal/advisor"
	"disk-hog/internal/clean"
	"disk-hog/internal/config"
	"disk-hog/internal/docker"
	"disk-hog/internal/dupes"
	"disk-hog/internal/fsutil"
	"disk-hog/internal/rules"
	"disk-hog/internal/scan"
	"disk-hog/internal/state"
	"disk-hog/internal/units"
)

// Server is the HTTP daemon.
type Server struct {
	cfg     config.Config
	cfgPath string
	log     *slog.Logger
	version string
	rules   *rules.Set
	store   *state.Store
	token   string
	home    string
	local   bool // bound to loopback only

	ctx context.Context

	mu          sync.Mutex
	scanner     *scan.Scanner
	scanCancel  context.CancelFunc
	scanReason  string
	result      *scan.Result
	finder      *dupes.Finder
	dupesCancel context.CancelFunc
	dupesRes    *dupes.Result
	cleaning    bool

	sysMu    sync.Mutex
	sysItems []advisor.Item
	sysAt    time.Time

	events *hub
	assets *assetHandler
	ln     net.Listener
}

// New prepares a server.
func New(cfg config.Config, cfgPath string, store *state.Store, log *slog.Logger, version string) (*Server, error) {
	s := &Server{
		cfg: cfg, cfgPath: cfgPath, log: log, version: version, store: store,
		rules: cfg.RuleSet(), events: newHub(),
	}
	s.home, _ = os.UserHomeDir()
	s.token = cfg.Token
	if s.token == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		s.token = hex.EncodeToString(b[:])
	}
	_ = os.WriteFile(filepath.Join(store.Path(), "token"), []byte(s.token+"\n"), 0o600)
	s.local = isLoopbackName(cfg.Host)

	var err error
	if s.assets, err = newAssetHandler(); err != nil {
		return nil, err
	}
	var last scan.Result
	if store.LoadBlob("last-scan", &last) == nil && !last.Finished.IsZero() {
		s.result = &last
	}
	var lastDupes dupes.Result
	if store.LoadBlob("last-dupes", &lastDupes) == nil && !lastDupes.Started.IsZero() {
		s.dupesRes = &lastDupes
	}
	return s, nil
}

// Addr is the listen address.
func (s *Server) Addr() string { return net.JoinHostPort(s.cfg.Host, fmt.Sprint(s.cfg.Port)) }

// URL is the dashboard address to show the user.
func (s *Server) URL() string {
	h := s.cfg.Host
	if h == "" || h == "0.0.0.0" || h == "::" {
		h = "localhost"
	}
	return "http://" + net.JoinHostPort(h, fmt.Sprint(s.cfg.Port))
}

// Run serves until ctx is cancelled.
// Listen claims the port. It is separate from Run so the caller only
// announces "started" once the port is really ours, and can explain
// clearly when it is not.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.Addr())
	if err != nil {
		return s.explainListen(err)
	}
	s.ln = ln
	return nil
}

// explainListen turns a bind failure into advice. When the port is taken,
// it asks whoever holds it whether it is disk-hog, so the message says
// what is actually going on instead of guessing.
func (s *Server) explainListen(err error) error {
	if !errors.Is(err, syscall.EADDRINUSE) && !strings.Contains(err.Error(), "address already in use") &&
		!strings.Contains(err.Error(), "Only one usage") {
		return fmt.Errorf("cannot listen on %s: %w", s.Addr(), err)
	}
	port := s.cfg.Port
	client := &http.Client{Timeout: 2 * time.Second}
	host := s.cfg.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	resp, herr := client.Get("http://" + net.JoinHostPort(host, fmt.Sprint(port)) + "/api/health")
	if herr == nil {
		var v struct {
			OK      bool   `json:"ok"`
			Version string `json:"version"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&v)
		resp.Body.Close()
		if v.OK {
			return &AlreadyRunning{URL: s.URL(), Version: v.Version, Port: port}
		}
	}
	return fmt.Errorf("port %d is already used by another program.\n"+
		"  Use a different port:  disk-hog serve --port %d\n"+
		"  See what holds it:     ss -ltnp 'sport = :%d'   (macOS: lsof -iTCP:%d -sTCP:LISTEN)",
		port, suggestPort(port), port, port)
}

func suggestPort(p int) int {
	if p < 65535 {
		return p + 1
	}
	return p - 1
}

// AlreadyRunning reports that another disk-hog already serves the port.
type AlreadyRunning struct {
	URL, Version string
	Port         int
}

func (e *AlreadyRunning) Error() string {
	return fmt.Sprintf("disk-hog %s is already running at %s, open that in your browser.\n"+
		"  It is usually the background service. To run one in this terminal instead, stop it first:\n"+
		"    systemctl --user stop disk-hog      (or ./deploy.sh stop in the cloned folder)\n"+
		"  Or run a second copy on another port:  disk-hog serve --port %d",
		e.Version, e.URL, suggestPort(e.Port))
}

// Run serves until ctx is cancelled. It calls Listen if the caller has not.
func (s *Server) Run(ctx context.Context) error {
	s.ctx = ctx
	if s.ln == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}
	ln := s.ln
	srv := &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	if s.cfg.Schedule > 0 {
		go s.scheduler(ctx)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		s.mu.Lock()
		if s.scanCancel != nil {
			s.scanCancel()
		}
		if s.dupesCancel != nil {
			s.dupesCancel()
		}
		s.mu.Unlock()
		s.events.close()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	case err := <-errc:
		return err
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	type handler = func(http.ResponseWriter, *http.Request)
	byPath := map[string]map[string]handler{}
	api := func(method, path string, h handler) {
		if byPath[path] == nil {
			byPath[path] = map[string]handler{}
		}
		byPath[path][method] = h
	}
	api("GET", "/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, map[string]any{"ok": true, "version": s.version})
	})
	api("GET", "/api/info", s.handleInfo)
	api("GET", "/api/events", s.handleEvents)
	api("GET", "/api/disks", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, r, fsutil.Disks()) })
	api("POST", "/api/scan", s.handleScan)
	api("POST", "/api/scan/cancel", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		if s.scanCancel != nil {
			s.scanCancel()
		}
		s.mu.Unlock()
		writeJSON(w, r, map[string]bool{"ok": true})
	})
	api("GET", "/api/results", s.handleResults)
	api("GET", "/api/tree", s.handleTree)
	api("POST", "/api/clean", s.handleClean)
	api("GET", "/api/ignores", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, r, s.store.Ignores()) })
	api("POST", "/api/ignore", s.handleIgnore)
	api("GET", "/api/history", func(w http.ResponseWriter, r *http.Request) {
		h, freed := s.store.History()
		writeJSON(w, r, map[string]any{"entries": h, "freed": freed})
	})
	api("GET", "/api/export", s.handleExport)
	api("POST", "/api/dupes", s.handleDupes)
	api("POST", "/api/dupes/cancel", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		if s.dupesCancel != nil {
			s.dupesCancel()
		}
		s.mu.Unlock()
		writeJSON(w, r, map[string]bool{"ok": true})
	})
	api("GET", "/api/dupes", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		res := s.dupesRes
		s.mu.Unlock()
		writeJSON(w, r, res)
	})
	api("POST", "/api/dupes/clean", s.handleDupesClean)
	api("GET", "/api/system", s.handleSystem)
	api("GET", "/api/docker", s.handleDocker)
	api("POST", "/api/docker/prune", s.handleDockerPrune)
	api("POST", "/api/reveal", s.handleReveal)
	for path, methods := range byPath {
		methods := methods
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			m := r.Method
			if m == http.MethodHead {
				m = http.MethodGet
			}
			h := methods[m]
			if h == nil {
				allow := make([]string, 0, len(methods))
				for k := range methods {
					allow = append(allow, k)
				}
				w.Header().Set("Allow", strings.Join(allow, ", "))
				httpError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			h(w, r)
		})
	}
	mux.Handle("/", s.assets)
	return s.secure(mux)
}

// ---- security --------------------------------------------------------------

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return strings.Trim(h, "[]")
}

func isLoopbackName(h string) bool {
	h = strings.ToLower(hostOnly(h))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// trusted reports whether the request comes from this machine through a
// loopback host name, which is what entitles a page to receive the token.
func (s *Server) trusted(r *http.Request) bool {
	if !isLoopbackName(r.Host) {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) tokenOK(r *http.Request) bool {
	t := r.Header.Get("X-Disk-Hog-Token")
	if t == "" && r.Method == http.MethodGet {
		t = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) == 1
}

func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")

		// A loopback-only server must only answer to loopback host names:
		// that is what defeats DNS rebinding.
		if s.local && !isLoopbackName(r.Host) {
			httpError(w, http.StatusForbidden, "forbidden host")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			open := r.URL.Path == "/api/health" || r.URL.Path == "/api/info"
			mutating := r.Method != http.MethodGet && r.Method != http.MethodHead
			if mutating {
				if o := r.Header.Get("Origin"); o != "" {
					if u, err := url.Parse(o); err != nil || u.Host != r.Host {
						httpError(w, http.StatusForbidden, "cross-origin request refused")
						return
					}
				}
			}
			if (mutating || (!open && !s.trusted(r))) && !s.tokenOK(r) {
				httpError(w, http.StatusUnauthorized, "token required (run: disk-hog token)")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---- info and events -------------------------------------------------------

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	type ruleView struct {
		ID, Name, Category, Risk, Kind, Note, Regen string
	}
	var rl []ruleView
	for _, ru := range s.rules.All {
		rl = append(rl, ruleView{ru.ID, ru.Name, ru.Category, ru.Risk, ru.Kind(), ru.Note, ru.Regen})
	}
	cfg := s.cfg
	cfg.Token = ""
	host, _ := os.Hostname()
	lastScan, _ := s.store.Times()
	info := map[string]any{
		"version": s.version, "os": runtime.GOOS, "arch": runtime.GOARCH, "hostname": host,
		"home": s.home, "roots": cfg.ExpandRoots(), "config": cfg, "configPath": s.cfgPath,
		"statePath": s.store.Path(), "readOnly": cfg.ReadOnly, "defaultAction": cfg.DefaultAction,
		"categories": rules.Categories, "rules": rl, "lastScan": lastScan, "keepMarker": scan.KeepMarker,
		"needsToken": !s.trusted(r),
	}
	if s.trusted(r) {
		info["token"] = s.token
	}
	writeJSON(w, r, info)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	ch := s.events.subscribe()
	defer s.events.unsubscribe(ch)

	fmt.Fprintf(w, "retry: 3000\n\n")
	s.writeEvent(w, "status", s.status())
	fl.Flush()
	keep := time.NewTicker(25 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			w.Write(msg)
			fl.Flush()
		case <-keep.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		}
	}
}

func (s *Server) writeEvent(w http.ResponseWriter, typ string, v any) {
	w.Write(encodeEvent(typ, v))
}

type status struct {
	Scan     *scan.Progress  `json:"scan,omitempty"`
	Dupes    *dupes.Progress `json:"dupes,omitempty"`
	Cleaning bool            `json:"cleaning"`
	Reason   string          `json:"reason,omitempty"`
	HasScan  bool            `json:"hasScan"`
	Summary  *resultSummary  `json:"summary,omitempty"`
	Disks    []fsutil.Disk   `json:"disks,omitempty"`
}

type resultSummary struct {
	Finished time.Time     `json:"finished"`
	Duration float64       `json:"duration"`
	Total    int64         `json:"total"`
	Items    int           `json:"items"`
	Canceled bool          `json:"canceled"`
	Summary  []scan.CatSum `json:"summary"`
	Stats    scan.Progress `json:"stats"`
	Roots    []string      `json:"roots"`
}

func summarize(r *scan.Result) *resultSummary {
	if r == nil {
		return nil
	}
	return &resultSummary{Finished: r.Finished, Duration: r.Duration, Total: r.Total, Items: len(r.Items),
		Canceled: r.Canceled, Summary: r.Summary, Stats: r.Stats, Roots: r.Roots}
}

func (s *Server) status() status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := status{Cleaning: s.cleaning, HasScan: s.result != nil, Summary: summarize(s.result), Reason: s.scanReason}
	if s.scanner != nil {
		p := s.scanner.Progress()
		st.Scan = &p
	}
	if s.finder != nil {
		p := s.finder.Progress()
		st.Dupes = &p
	}
	return st
}

// ---- scanning ----------------------------------------------------------------

type scanRequest struct {
	Roots         []string `json:"roots"`
	IncludeGlobal *bool    `json:"includeGlobal"`
	OneFileSystem *bool    `json:"oneFileSystem"`
	MinItemSize   string   `json:"minItemSize"`
	LargeFileSize string   `json:"largeFileSize"`
}

var errBusy = errors.New("a scan is already running")

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	var req scanRequest
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.StartScan(req, "manual"); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errBusy) {
			code = http.StatusConflict
		}
		httpError(w, code, err.Error())
		return
	}
	writeJSON(w, r, map[string]bool{"ok": true})
}

// StartScan launches a scan in the background.
func (s *Server) StartScan(req scanRequest, reason string) error {
	opt := scan.Options{
		Roots:         s.cfg.ExpandRoots(),
		Exclude:       s.cfg.Exclude,
		Ignores:       s.store.Ignores(),
		OneFileSystem: s.cfg.OneFileSystem,
		IncludeGlobal: s.cfg.IncludeGlobal,
		Workers:       s.cfg.Workers,
		MinItemSize:   int64(s.cfg.MinItemSize),
		LargeFileSize: int64(s.cfg.LargeFileSize),
		Rules:         s.rules,
	}
	if len(req.Roots) > 0 {
		opt.Roots = nil
		for _, root := range req.Roots {
			if root = strings.TrimSpace(root); root != "" {
				opt.Roots = append(opt.Roots, config.Expand(root, s.home))
			}
		}
	}
	if req.IncludeGlobal != nil {
		opt.IncludeGlobal = *req.IncludeGlobal
	}
	if req.OneFileSystem != nil {
		opt.OneFileSystem = *req.OneFileSystem
	}
	if req.MinItemSize != "" {
		v, err := units.ParseSize(req.MinItemSize)
		if err != nil {
			return err
		}
		opt.MinItemSize = v
	}
	if req.LargeFileSize != "" {
		v, err := units.ParseSize(req.LargeFileSize)
		if err != nil {
			return err
		}
		opt.LargeFileSize = v
	}
	for _, root := range opt.Roots {
		if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
			return fmt.Errorf("not a directory: %s", root)
		}
	}

	s.mu.Lock()
	if s.scanner != nil {
		s.mu.Unlock()
		return errBusy
	}
	sc := scan.New(opt)
	ctx, cancel := context.WithCancel(s.ctx)
	s.scanner, s.scanCancel, s.scanReason = sc, cancel, reason
	s.mu.Unlock()
	s.log.Info("scan started", "roots", opt.Roots, "reason", reason)

	go func() {
		defer cancel()
		done := make(chan struct{})
		go func() {
			t := time.NewTicker(300 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					s.events.publish("scan.progress", sc.Progress())
				}
			}
		}()
		res := sc.Run(ctx)
		close(done)

		s.mu.Lock()
		s.scanner, s.scanCancel, s.scanReason = nil, nil, ""
		if !res.Canceled || s.result == nil {
			s.result = res
		}
		s.mu.Unlock()
		if !res.Canceled {
			if err := s.store.SaveBlob("last-scan", res); err != nil {
				s.log.Warn("save scan result", "error", err)
			}
			s.store.Mark(res.Finished, time.Time{})
		}
		s.log.Info("scan finished", "items", len(res.Items), "reclaimable", units.FormatSize(res.Total),
			"dirs", res.Stats.Dirs, "files", res.Stats.Files, "errors", res.Stats.Errors,
			"duration", time.Duration(res.Duration*float64(time.Second)).Round(time.Millisecond).String(), "canceled", res.Canceled)
		s.events.publish("scan.done", summarize(res))
		if reason == "schedule" && !res.Canceled {
			s.maybeNotify(res)
		}
		// Walking a large tree churns through a lot of short-lived memory;
		// hand it back to the OS so the idle daemon stays small.
		debug.FreeOSMemory()
	}()
	return nil
}

func (s *Server) handleResults(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	res := s.result
	s.mu.Unlock()
	if res == nil {
		writeJSON(w, r, nil)
		return
	}
	writeJSON(w, r, res)
}

func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	res := s.result
	s.mu.Unlock()
	if res == nil || res.Tree == nil {
		httpError(w, http.StatusNotFound, "run a scan to browse folders")
		return
	}
	v, ok := res.View(r.URL.Query().Get("path"))
	if !ok {
		httpError(w, http.StatusNotFound, "folder not in the last scan")
		return
	}
	writeJSON(w, r, v)
}

// ---- cleaning ----------------------------------------------------------------

type cleanRequest struct {
	IDs  []int  `json:"ids"`
	Mode string `json:"mode"`
}

type cleanResult struct {
	ID    int    `json:"id"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Mode  string `json:"mode"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (s *Server) beginClean(w http.ResponseWriter) bool {
	if s.cfg.ReadOnly {
		httpError(w, http.StatusForbidden, "disk-hog is in read-only mode (readOnly in config)")
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cleaning {
		httpError(w, http.StatusConflict, "a cleanup is already running")
		return false
	}
	if s.scanner != nil {
		httpError(w, http.StatusConflict, "wait for the scan to finish")
		return false
	}
	s.cleaning = true
	return true
}

func (s *Server) endClean() {
	s.mu.Lock()
	s.cleaning = false
	s.mu.Unlock()
	debug.FreeOSMemory()
}

// resolveMode turns "auto" into a concrete mode for one item: regenerable
// things are deleted, anything that needs a look goes to the trash, and
// "contents" items (the trash itself) can only be emptied.
func resolveMode(mode, risk string, contents bool) string {
	if contents {
		return clean.Delete
	}
	if mode == "" || mode == "auto" {
		if risk == rules.Safe {
			return clean.Delete
		}
		return clean.Trash
	}
	return mode
}

func (s *Server) handleClean(w http.ResponseWriter, r *http.Request) {
	var req cleanRequest
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Mode == "" {
		req.Mode = s.cfg.DefaultAction
	}
	if req.Mode != "auto" && req.Mode != clean.Trash && req.Mode != clean.Delete {
		httpError(w, http.StatusBadRequest, "mode must be auto, trash or delete")
		return
	}
	if !s.beginClean(w) {
		return
	}
	defer s.endClean()

	s.mu.Lock()
	res := s.result
	s.mu.Unlock()
	if res == nil {
		httpError(w, http.StatusConflict, "no scan results")
		return
	}
	byID := make(map[int]*scan.Candidate, len(res.Items))
	var exact []string
	for _, c := range res.Items {
		byID[c.ID] = c
		if c.Global {
			exact = append(exact, c.Path)
		}
	}
	guard := clean.NewGuard(s.home, res.Roots, exact, []string{s.store.Path(), filepath.Dir(s.cfgPath)})

	var out []cleanResult
	var entries []state.Entry
	var freed, moved int64
	removed := map[int]bool{}
	for i, id := range req.IDs {
		c := byID[id]
		if c == nil {
			out = append(out, cleanResult{ID: id, Error: "unknown item (rescan?)"})
			continue
		}
		mode := resolveMode(req.Mode, c.Risk, c.Contents)
		s.events.publish("clean.progress", map[string]any{"done": i, "total": len(req.IDs), "path": c.Path})
		t := clean.Target{Path: c.Path, Kind: c.Kind, Contents: c.Contents, Dev: c.Dev, Ino: c.Ino, Size: c.Size}
		err := clean.Run(guard, t, mode)
		cr := cleanResult{ID: id, Path: c.Path, Size: c.Size, Mode: mode, OK: err == nil}
		if err != nil {
			cr.Error = err.Error()
			s.log.Warn("cleanup failed", "path", c.Path, "error", err)
		} else {
			removed[id] = true
			if mode == clean.Delete {
				freed += c.Size
			} else {
				moved += c.Size
			}
			s.log.Info("cleaned", "path", c.Path, "mode", mode, "size", units.FormatSize(c.Size))
		}
		out = append(out, cr)
		entries = append(entries, state.Entry{Time: time.Now(), Path: c.Path, Rule: c.Rule, Size: c.Size, Mode: mode, OK: err == nil, Error: cr.Error})
	}
	_ = s.store.Record(entries...)

	if len(removed) > 0 {
		s.mu.Lock()
		if s.result == res {
			kept := res.Items[:0]
			for _, c := range res.Items {
				if !removed[c.ID] {
					kept = append(kept, c)
				}
			}
			res.Items = kept
			res.Total = 0
			for _, c := range kept {
				res.Total += c.Size
			}
			res.Summary = scan.Summarize(kept)
		}
		s.mu.Unlock()
		_ = s.store.SaveBlob("last-scan", res)
	}
	s.events.publish("clean.done", map[string]any{"freed": freed, "moved": moved, "count": len(removed)})
	writeJSON(w, r, map[string]any{"results": out, "freed": freed, "moved": moved, "disks": fsutil.Disks()})
}

func (s *Server) handleIgnore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		On   bool   `json:"on"`
	}
	if err := readJSON(r, &req); err != nil || !filepath.IsAbs(req.Path) {
		httpError(w, http.StatusBadRequest, "absolute path required")
		return
	}
	if err := s.store.SetIgnore(req.Path, req.On); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.On {
		s.mu.Lock()
		if res := s.result; res != nil {
			kept := res.Items[:0]
			for _, c := range res.Items {
				if !fsutil.Within(c.Path, filepath.Clean(req.Path)) {
					kept = append(kept, c)
				}
			}
			res.Items = kept
			res.Total = 0
			for _, c := range kept {
				res.Total += c.Size
			}
			res.Summary = scan.Summarize(kept)
		}
		s.mu.Unlock()
	}
	writeJSON(w, r, s.store.Ignores())
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	res := s.result
	s.mu.Unlock()
	if res == nil {
		httpError(w, http.StatusNotFound, "no scan results")
		return
	}
	stamp := res.Finished.Format("2006-01-02-1504")
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="disk-hog-`+stamp+`.csv"`)
		WriteCSV(w, res.Items)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="disk-hog-`+stamp+`.json"`)
	writeJSON(w, r, res)
}

// ---- duplicates ----------------------------------------------------------------

func (s *Server) handleDupes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Roots    []string `json:"roots"`
		MinSize  string   `json:"minSize"`
		SkipDeps *bool    `json:"skipDeps"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	opt := dupes.Options{Roots: s.cfg.ExpandRoots(), Exclude: s.cfg.Exclude, MinSize: int64(s.cfg.DupesMinSize),
		OneFileSystem: s.cfg.OneFileSystem, Workers: s.cfg.Workers, SkipDirs: dupes.DefaultSkip}
	if len(req.Roots) > 0 {
		opt.Roots = nil
		for _, root := range req.Roots {
			if root = strings.TrimSpace(root); root != "" {
				opt.Roots = append(opt.Roots, config.Expand(root, s.home))
			}
		}
	}
	if req.MinSize != "" {
		v, err := units.ParseSize(req.MinSize)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		opt.MinSize = v
	}
	if req.SkipDeps != nil && !*req.SkipDeps {
		opt.SkipDirs = nil
	}
	s.mu.Lock()
	if s.finder != nil {
		s.mu.Unlock()
		httpError(w, http.StatusConflict, "a duplicate search is already running")
		return
	}
	f := dupes.New(opt)
	ctx, cancel := context.WithCancel(s.ctx)
	s.finder, s.dupesCancel = f, cancel
	s.mu.Unlock()

	go func() {
		defer cancel()
		done := make(chan struct{})
		go func() {
			t := time.NewTicker(400 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					s.events.publish("dupes.progress", f.Progress())
				}
			}
		}()
		res := f.Run(ctx)
		close(done)
		s.mu.Lock()
		s.finder, s.dupesCancel = nil, nil
		if !res.Canceled || s.dupesRes == nil {
			s.dupesRes = res
		}
		s.mu.Unlock()
		if !res.Canceled {
			_ = s.store.SaveBlob("last-dupes", res)
		}
		s.log.Info("duplicate search finished", "groups", len(res.Groups), "wasted", units.FormatSize(res.Wasted))
		s.events.publish("dupes.done", map[string]any{"groups": len(res.Groups), "wasted": res.Wasted, "canceled": res.Canceled})
		debug.FreeOSMemory()
	}()
	writeJSON(w, r, map[string]bool{"ok": true})
}

func (s *Server) handleDupesClean(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs  []int  `json:"ids"`
		Mode string `json:"mode"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Mode != clean.Trash && req.Mode != clean.Delete {
		req.Mode = clean.Trash
	}
	if !s.beginClean(w) {
		return
	}
	defer s.endClean()
	s.mu.Lock()
	res := s.dupesRes
	s.mu.Unlock()
	if res == nil {
		httpError(w, http.StatusConflict, "no duplicate results")
		return
	}
	want := map[int]bool{}
	for _, id := range req.IDs {
		want[id] = true
	}
	guard := clean.NewGuard(s.home, res.Roots, nil, []string{s.store.Path()})
	var out []cleanResult
	var entries []state.Entry
	var freed int64
	for gi := range res.Groups {
		g := &res.Groups[gi]
		n := 0
		for _, f := range g.Files {
			if want[f.ID] {
				n++
			}
		}
		if n == 0 {
			continue
		}
		if n >= len(g.Files) {
			for _, f := range g.Files {
				if want[f.ID] {
					out = append(out, cleanResult{ID: f.ID, Path: f.Path, Error: "refusing to remove every copy"})
				}
			}
			continue
		}
		kept := g.Files[:0]
		for _, f := range g.Files {
			if !want[f.ID] {
				kept = append(kept, f)
				continue
			}
			t := clean.Target{Path: f.Path, Kind: "file", Dev: f.Dev, Ino: f.Ino, Size: g.Size}
			err := clean.Run(guard, t, req.Mode)
			cr := cleanResult{ID: f.ID, Path: f.Path, Size: g.Size, Mode: req.Mode, OK: err == nil}
			if err != nil {
				cr.Error = err.Error()
				kept = append(kept, f)
			} else if req.Mode == clean.Delete {
				freed += g.Size
			}
			out = append(out, cr)
			entries = append(entries, state.Entry{Time: time.Now(), Path: f.Path, Rule: "duplicate", Size: g.Size, Mode: req.Mode, OK: err == nil, Error: cr.Error})
		}
		g.Files = kept
		g.Wasted = g.Size * int64(len(kept)-1)
	}
	_ = s.store.Record(entries...)
	s.mu.Lock()
	groups := res.Groups[:0]
	res.Wasted = 0
	for _, g := range res.Groups {
		if len(g.Files) > 1 {
			groups = append(groups, g)
			res.Wasted += g.Wasted
		}
	}
	res.Groups = groups
	s.mu.Unlock()
	_ = s.store.SaveBlob("last-dupes", res)
	writeJSON(w, r, map[string]any{"results": out, "freed": freed})
}

// ---- system, containers, reveal ------------------------------------------------

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	s.sysMu.Lock()
	defer s.sysMu.Unlock()
	if s.sysItems == nil || time.Since(s.sysAt) > 10*time.Minute || r.URL.Query().Get("refresh") != "" {
		ctx, cancel := context.WithTimeout(r.Context(), advisor.Timeout)
		s.sysItems = advisor.Collect(ctx)
		cancel()
		if s.sysItems == nil {
			s.sysItems = []advisor.Item{}
		}
		s.sysAt = time.Now()
	}
	writeJSON(w, r, map[string]any{"items": s.sysItems, "at": s.sysAt})
}

func (s *Server) handleDocker(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	engines, errs := docker.Detect(ctx)
	var usage []docker.Usage
	for _, e := range engines {
		usage = append(usage, e.Usage(ctx))
	}
	writeJSON(w, r, map[string]any{"engines": usage, "errors": errs})
}

func (s *Server) handleDockerPrune(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ReadOnly {
		httpError(w, http.StatusForbidden, "read-only mode")
		return
	}
	var req struct {
		Socket string `json:"socket"`
		What   string `json:"what"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	engines, _ := docker.Detect(ctx)
	for _, e := range engines {
		if e.Socket != req.Socket {
			continue
		}
		n, err := e.Prune(ctx, req.What)
		_ = s.store.Record(state.Entry{Time: time.Now(), Path: e.Name + " prune " + req.What, Rule: "docker", Size: n, Mode: "delete", OK: err == nil, Error: errString(err)})
		if err != nil {
			httpError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, r, map[string]any{"reclaimed": n})
		return
	}
	httpError(w, http.StatusNotFound, "engine not found")
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) handleReveal(w http.ResponseWriter, r *http.Request) {
	if !s.trusted(r) {
		httpError(w, http.StatusForbidden, "only available on this machine")
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &req); err != nil || !filepath.IsAbs(req.Path) {
		httpError(w, http.StatusBadRequest, "absolute path required")
		return
	}
	p := filepath.Clean(req.Path)
	if _, err := os.Lstat(p); err != nil {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	if err := fsutil.Reveal(p); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, r, map[string]bool{"ok": true})
}

// ---- schedule and notifications --------------------------------------------------

func (s *Server) scheduler(ctx context.Context) {
	every := s.cfg.Schedule.D()
	if every < time.Minute {
		every = time.Minute
	}
	last, _ := s.store.Times()
	wait := time.Until(last.Add(every))
	if wait < time.Minute {
		wait = time.Minute // let the machine finish booting
	}
	s.log.Info("scheduled scans enabled", "every", s.cfg.Schedule.String(), "next", time.Now().Add(wait).Format(time.RFC3339))
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.StartScan(scanRequest{}, "schedule"); err != nil && !errors.Is(err, errBusy) {
				s.log.Warn("scheduled scan", "error", err)
			}
			t.Reset(every)
		}
	}
}

func (s *Server) maybeNotify(res *scan.Result) {
	if s.cfg.NotifyAbove <= 0 {
		return
	}
	var easy int64
	for _, c := range res.Items {
		if c.Risk != rules.Review {
			easy += c.Size
		}
	}
	if easy < int64(s.cfg.NotifyAbove) {
		return
	}
	if _, last := s.store.Times(); time.Since(last) < 20*time.Hour {
		return
	}
	msg := units.FormatSize(easy) + " can be reclaimed. Open " + s.URL()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("osascript", "-e", fmt.Sprintf("display notification %q with title \"disk-hog\"", msg))
	case "windows":
		return
	default:
		cmd = exec.Command("notify-send", "--app-name=disk-hog", "--icon=drive-harddisk", "disk-hog", msg)
	}
	if err := cmd.Run(); err != nil {
		s.log.Debug("notification failed", "error", err)
		return
	}
	s.store.Mark(time.Time{}, time.Now())
}

// ---- helpers ---------------------------------------------------------------------

func readJSON(r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
