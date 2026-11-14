package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"disk-hog/internal/scan"
	"disk-hog/web"
)

// writeJSON encodes v, gzipping larger bodies for clients that accept it.
func writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	if len(b) > 1400 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		zw, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
		zw.Write(b)
		zw.Close()
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.Write(b)
}

// WriteCSV writes scan items as CSV.
func WriteCSV(w io.Writer, items []*scan.Candidate) {
	cw := csv.NewWriter(w)
	cw.Write([]string{"path", "bytes", "size", "rule", "category", "risk", "files", "last_touched", "project"})
	for _, c := range items {
		touched := ""
		if c.Touched > 0 {
			touched = time.UnixMilli(c.Touched).Format(time.RFC3339)
		}
		cw.Write([]string{c.Path, strconv.FormatInt(c.Size, 10), formatSize(c.Size), c.Rule, c.Category, c.Risk,
			strconv.FormatInt(c.Files, 10), touched, c.Project})
	}
	cw.Flush()
}

func formatSize(n int64) string {
	const u = 1024
	if n < u {
		return strconv.FormatInt(n, 10) + " B"
	}
	v, i := float64(n)/u, 0
	for v >= u && i < 4 {
		v /= u
		i++
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " " + []string{"KB", "MB", "GB", "TB", "PB"}[i]
}

// ---- server-sent events --------------------------------------------------------

type hub struct {
	mu     sync.Mutex
	subs   map[chan []byte]struct{}
	closed bool
}

func newHub() *hub { return &hub{subs: map[chan []byte]struct{}{}} }

func encodeEvent(typ string, v any) []byte {
	b, _ := json.Marshal(v)
	var buf bytes.Buffer
	buf.Grow(len(b) + len(typ) + 16)
	buf.WriteString("event: ")
	buf.WriteString(typ)
	buf.WriteString("\ndata: ")
	buf.Write(b)
	buf.WriteString("\n\n")
	return buf.Bytes()
}

func (h *hub) subscribe() chan []byte {
	ch := make(chan []byte, 32)
	h.mu.Lock()
	if h.closed {
		close(ch)
	} else {
		h.subs[ch] = struct{}{}
	}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// publish never blocks: a client that falls behind misses progress ticks,
// which the next tick makes up for.
func (h *hub) publish(typ string, v any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) == 0 {
		return
	}
	msg := encodeEvent(typ, v)
	for ch := range h.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for ch := range h.subs {
		delete(h.subs, ch)
		close(ch)
	}
}

// ---- embedded dashboard --------------------------------------------------------

type asset struct {
	ctype string
	etag  string
	plain []byte
	gz    []byte
}

type assetHandler struct{ files map[string]*asset }

// newAssetHandler loads the embedded files once, precomputing gzip and
// ETags so serving them costs nothing.
func newAssetHandler() (*assetHandler, error) {
	h := &assetHandler{files: map[string]*asset{}}
	err := fs.WalkDir(web.Assets, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".go") {
			return err
		}
		b, err := fs.ReadFile(web.Assets, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		a := &asset{ctype: mime.TypeByExtension(path.Ext(p)), etag: `"` + hex.EncodeToString(sum[:8]) + `"`, plain: b}
		if a.ctype == "" {
			a.ctype = "application/octet-stream"
		}
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		zw.Write(b)
		zw.Close()
		if buf.Len() < len(b) {
			a.gz = buf.Bytes()
		}
		h.files["/"+p] = a
		return nil
	})
	return h, err
}

func (h *assetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/" {
		p = "/index.html"
	}
	a := h.files[p]
	if a == nil {
		http.NotFound(w, r)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", a.ctype)
	hd.Set("ETag", a.etag)
	hd.Set("Cache-Control", "no-cache")
	hd.Set("Vary", "Accept-Encoding")
	if r.Header.Get("If-None-Match") == a.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := a.plain
	if a.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		hd.Set("Content-Encoding", "gzip")
		body = a.gz
	}
	hd.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	w.Write(body)
}
