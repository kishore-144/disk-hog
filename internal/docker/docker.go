// Package docker talks to the Docker Engine API (or Podman's compatible
// API) over its Unix socket, using only the standard library, to report and
// prune container storage.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Engine is one reachable container engine.
type Engine struct {
	Name   string `json:"name"`
	Socket string `json:"socket"`
	http   *http.Client
}

// Sockets lists the places engines usually listen, most specific first.
func Sockets() []string {
	home, _ := os.UserHomeDir()
	run := os.Getenv("XDG_RUNTIME_DIR")
	var out []string
	if h := os.Getenv("DOCKER_HOST"); strings.HasPrefix(h, "unix://") {
		out = append(out, strings.TrimPrefix(h, "unix://"))
	}
	if run != "" {
		out = append(out, filepath.Join(run, "docker.sock"), filepath.Join(run, "podman", "podman.sock"))
	}
	out = append(out, "/var/run/docker.sock", "/run/podman/podman.sock",
		filepath.Join(home, ".docker", "run", "docker.sock"), filepath.Join(home, ".docker", "desktop", "docker.sock"),
		filepath.Join(home, ".colima", "default", "docker.sock"), filepath.Join(home, ".orbstack", "run", "docker.sock"),
		filepath.Join(home, ".rd", "docker.sock"))
	return out
}

// Detect returns every engine that answers on a known socket. Sockets that
// exist but refuse the connection are returned with an error.
func Detect(ctx context.Context) ([]*Engine, []string) {
	if runtime.GOOS == "windows" {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []*Engine
	var errs []string
	for _, s := range Sockets() {
		real, err := filepath.EvalSymlinks(s)
		if err != nil || seen[real] {
			continue
		}
		seen[real] = true
		e := newEngine(s)
		var v struct {
			Version    string
			Components []struct{ Name string }
		}
		if err := e.do(ctx, "GET", "/version", &v); err != nil {
			if strings.Contains(err.Error(), "permission denied") {
				errs = append(errs, s+": permission denied (add yourself to the docker group: sudo usermod -aG docker $USER, then log in again)")
			} else {
				errs = append(errs, s+": "+err.Error())
			}
			continue
		}
		e.Name = "Docker " + v.Version
		for _, c := range v.Components {
			if strings.Contains(strings.ToLower(c.Name), "podman") {
				e.Name = "Podman " + v.Version
			}
		}
		out = append(out, e)
	}
	return out, errs
}

func newEngine(sock string) *Engine {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		MaxIdleConns: 1,
	}
	return &Engine{Socket: sock, http: &http.Client{Transport: tr, Timeout: 5 * time.Minute}}
}

func (e *Engine) do(ctx context.Context, method, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://engine"+path, nil)
	if err != nil {
		return err
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var m struct{ Message string }
		if json.Unmarshal(b, &m) == nil && m.Message != "" {
			return fmt.Errorf("%s", m.Message)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if v == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// Usage summarises an engine's storage.
type Usage struct {
	Engine        string `json:"engine"`
	Socket        string `json:"socket"`
	Images        int    `json:"images"`
	ImagesSize    int64  `json:"imagesSize"`
	UnusedImages  int    `json:"unusedImages"`
	UnusedSize    int64  `json:"unusedSize"`
	Dangling      int    `json:"dangling"`
	DanglingSize  int64  `json:"danglingSize"`
	Containers    int    `json:"containers"`
	Stopped       int    `json:"stopped"`
	StoppedSize   int64  `json:"stoppedSize"`
	Volumes       int    `json:"volumes"`
	VolumesSize   int64  `json:"volumesSize"`
	UnusedVolumes int    `json:"unusedVolumes"`
	UnusedVolSize int64  `json:"unusedVolSize"`
	BuildCache    int64  `json:"buildCache"`
	BuildReclaim  int64  `json:"buildReclaim"`
	Error         string `json:"error,omitempty"`
}

// Usage queries /system/df.
func (e *Engine) Usage(ctx context.Context) Usage {
	u := Usage{Engine: e.Name, Socket: e.Socket}
	var df struct {
		Images []struct {
			RepoTags   []string
			Size       int64
			SharedSize int64
			Containers int64
		}
		Containers []struct {
			State  string
			SizeRw int64
		}
		Volumes []struct {
			UsageData struct {
				Size     int64
				RefCount int64
			}
		}
		BuildCache []struct {
			Size  int64
			InUse bool
		}
	}
	if err := e.do(ctx, "GET", "/system/df", &df); err != nil {
		u.Error = err.Error()
		return u
	}
	for _, im := range df.Images {
		u.Images++
		unique := im.Size
		if im.SharedSize > 0 {
			unique -= im.SharedSize
		}
		u.ImagesSize += im.Size
		if im.Containers == 0 {
			u.UnusedImages++
			u.UnusedSize += unique
		}
		dangling := len(im.RepoTags) == 0
		for _, t := range im.RepoTags {
			if t == "<none>:<none>" {
				dangling = true
			}
		}
		if dangling {
			u.Dangling++
			u.DanglingSize += unique
		}
	}
	for _, c := range df.Containers {
		u.Containers++
		if c.State != "running" && c.State != "paused" && c.State != "restarting" {
			u.Stopped++
			u.StoppedSize += c.SizeRw
		}
	}
	for _, v := range df.Volumes {
		u.Volumes++
		if v.UsageData.Size > 0 {
			u.VolumesSize += v.UsageData.Size
		}
		if v.UsageData.RefCount == 0 {
			u.UnusedVolumes++
			if v.UsageData.Size > 0 {
				u.UnusedVolSize += v.UsageData.Size
			}
		}
	}
	for _, b := range df.BuildCache {
		u.BuildCache += b.Size
		if !b.InUse {
			u.BuildReclaim += b.Size
		}
	}
	return u
}

// PruneTargets are the supported prune operations.
var PruneTargets = map[string]string{
	"containers": "/containers/prune",
	"dangling":   "/images/prune",
	"images":     "/images/prune?filters=" + url.QueryEscape(`{"dangling":["false"]}`),
	"build":      "/build/prune",
	"volumes":    "/volumes/prune?filters=" + url.QueryEscape(`{"all":["true"]}`),
}

// Prune runs one prune operation and returns the bytes reclaimed.
func (e *Engine) Prune(ctx context.Context, what string) (int64, error) {
	path, ok := PruneTargets[what]
	if !ok {
		return 0, fmt.Errorf("unknown prune target %q", what)
	}
	var r struct{ SpaceReclaimed int64 }
	err := e.do(ctx, "POST", path, &r)
	if err != nil && what == "volumes" {
		// Engines older than API 1.42 do not know the "all" filter and
		// prune every unused volume by default anyway.
		err = e.do(ctx, "POST", "/volumes/prune", &r)
	}
	return r.SpaceReclaimed, err
}
