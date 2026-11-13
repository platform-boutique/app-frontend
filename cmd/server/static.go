package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Cache policy for /static/*. Nothing is fingerprinted (the URLs carry no content hash), so nothing
// may be "immutable": every response carries validators (ETag, Last-Modified) and the policy is a
// freshness window after which the browser revalidates with a cheap 304.
const (
	// product images and the vendored Faro bundles: file names change when the content does (images)
	// or the files are pinned by hash (vendor); a day is long enough to matter, short enough that a
	// replaced image or an upgraded SDK reaches every visitor the next day.
	staticCacheLong = "public, max-age=86400"
	// our own css/js/svg change with every release and the HTML that uses them changes with them:
	// a short window, then revalidate.
	staticCacheShort = "public, max-age=300, must-revalidate"

	// below this a gzip header costs more than it saves
	gzipMinSize = 256
	// bound of the in-memory cache of compressed files (the shop has about a dozen compressible files)
	gzipMaxEntries = 64
)

// compressible: text types only. Images (webp) are already compressed, compressing them again only
// burns CPU and can make them larger.
var compressible = map[string]bool{
	".css": true, ".js": true, ".mjs": true, ".svg": true, ".json": true,
	".html": true, ".txt": true, ".xml": true, ".map": true,
}

// noDirFS hides directories: a request for one is "not found" (no listing, no index redirect).
type noDirFS struct{ fs http.FileSystem }

func (n noDirFS) Open(name string) (http.File, error) {
	f, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		_ = f.Close()
		return nil, fs.ErrNotExist
	}
	return f, nil
}

type gzipEntry struct {
	size int64
	mod  time.Time
	data []byte
}

// staticServer serves files with Cache-Control, ETag, Last-Modified/304 (via http.ServeContent),
// gzip for text types, no directory listing and never a Set-Cookie.
type staticServer struct {
	fs http.FileSystem

	mu sync.Mutex
	gz map[string]gzipEntry // compressed bodies keyed by cleaned path, valid while size and mtime match
}

func newStaticServer(root http.FileSystem) *staticServer {
	return &staticServer{fs: noDirFS{root}, gz: map[string]gzipEntry{}}
}

// ServeHTTP expects the request path relative to the static root (mount it with http.StripPrefix).
func (s *staticServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.serve(w, r, r.URL.Path)
}

// file returns a handler that always serves one file of the root (the favicon).
func (s *staticServer) file(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serve(w, r, name) })
}

func (s *staticServer) serve(w http.ResponseWriter, r *http.Request, name string) {
	h := w.Header()
	h.Del("Set-Cookie") // a static response must stay cacheable by browsers and CDNs
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.HasSuffix(name, "/") || name == "" {
		http.NotFound(w, r)
		return
	}
	name = path.Clean("/" + name)
	f, err := s.fs.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}

	h.Set("Cache-Control", cacheControlFor(name))
	h.Set("X-Content-Type-Options", "nosniff")
	tag := strconv.FormatInt(st.ModTime().UnixNano(), 36) + "-" + strconv.FormatInt(st.Size(), 36)

	if compressible[strings.ToLower(path.Ext(name))] && st.Size() >= gzipMinSize {
		h.Add("Vary", "Accept-Encoding")
		if acceptsGzip(r.Header.Get("Accept-Encoding")) {
			data, err := s.gzipped(name, st.Size(), st.ModTime(), f)
			if err == nil {
				if ct := contentTypeOf(name); ct != "" {
					h.Set("Content-Type", ct)
				}
				h.Set("Content-Encoding", "gzip")
				h.Set("ETag", `"`+tag+`-gzip"`)
				if r.Header.Get("Range") == "" {
					// ServeContent leaves Content-Length out when Content-Encoding is set; with a Range
					// header the length of the part is its business, a full length would be wrong
					h.Set("Content-Length", strconv.Itoa(len(data)))
				}
				http.ServeContent(w, r, name, st.ModTime(), bytes.NewReader(data))
				return
			}
			// compression failed (cannot happen for an in-memory buffer, but a read error can): serve plain
			if _, err := f.Seek(0, 0); err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
	}
	h.Set("ETag", `"`+tag+`"`)
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func cacheControlFor(name string) string {
	if strings.HasPrefix(name, "/img/") || strings.HasPrefix(name, "/js/vendor/") {
		return staticCacheLong
	}
	return staticCacheShort
}

func contentTypeOf(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json", ".map":
		return "application/json"
	case ".html":
		return "text/html; charset=utf-8"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".xml":
		return "text/xml; charset=utf-8"
	}
	return ""
}

// gzipped returns the compressed body of the file, computed once per (path, size, mtime).
func (s *staticServer) gzipped(name string, size int64, mod time.Time, f http.File) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.gz[name]; ok && e.size == size && e.mod.Equal(mod) {
		return e.data, nil
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	if _, err := io.Copy(zw, f); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if len(s.gz) >= gzipMaxEntries {
		s.gz = map[string]gzipEntry{} // bounded: start over rather than track recency
	}
	data := buf.Bytes()
	s.gz[name] = gzipEntry{size: size, mod: mod, data: data}
	return data, nil
}

// acceptsGzip parses Accept-Encoding: gzip (or *) with a non-zero q, and no "gzip;q=0".
func acceptsGzip(header string) bool {
	star, gz := false, false
	seenGzip := false
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		coding := strings.ToLower(strings.TrimSpace(fields[0]))
		q := 1.0
		for _, p := range fields[1:] {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(p, "q=") || strings.HasPrefix(p, "Q=") {
				if v, err := strconv.ParseFloat(p[2:], 64); err == nil {
					q = v
				} else {
					q = 0
				}
			}
		}
		switch coding {
		case "gzip", "x-gzip":
			seenGzip, gz = true, q > 0
		case "*":
			star = q > 0
		}
	}
	if seenGzip {
		return gz
	}
	return star
}
