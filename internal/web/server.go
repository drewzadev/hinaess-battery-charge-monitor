// Package web is Slice 3's embedded HTTP server: a single-page dashboard plus a
// read-only JSON API, started inside `serve` alongside the poll loop and reading
// the same SQLite database the loop writes (FR-3). It opens no port of its own
// beyond cfg.Web.Listen, sets no CORS headers (same-origin LAN only, Non-goal 1),
// and never writes the database. This file owns the constructor, routing, and
// lifecycle; api.go adds the /api/* handlers.
package web

import (
	"context"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"

	"bitbucket.org/andrewburnsza/hinaess-battery-charge-monitor/internal/store"
)

// Server holds the dependencies of the dashboard's HTTP handlers and owns the
// underlying *http.Server's lifecycle. pollInterval feeds /api/health's
// staleness threshold (2× the interval, FR-7); it is carried here so the health
// handler need not reach back into config.
type Server struct {
	store        *store.Store
	pollInterval time.Duration
	log          *slog.Logger
	httpSrv      *http.Server
}

// New builds the route mux, wraps it in a request-logging middleware, and
// constructs the http.Server bound to listen. It does not bind the listener or
// serve — call Start for that (FR-3). The mux serves GET / as the dashboard
// page, /static/ from the embedded asset sub-FS, and the /api/* JSON endpoints.
func New(st *store.Store, pollInterval time.Duration, listen string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		store:        st,
		pollInterval: pollInterval,
		log:          log,
	}

	mux := http.NewServeMux()
	s.routes(mux)

	s.httpSrv = &http.Server{
		Addr:    listen,
		Handler: s.logRequests(mux),
	}
	return s
}

// routes registers the dashboard, static-asset, and API handlers on mux. Only
// GET is accepted; the Go 1.22+ method-prefixed patterns make any other method
// fall through to a 405 (FR-4). The /api/* handlers live in api.go.
func (s *Server) routes(mux *http.ServeMux) {
	// Static assets are served from a sub-FS rooted at "assets" so request paths
	// are /static/uplot.min.js rather than /static/assets/uplot.min.js. fs.Sub
	// cannot fail for a path embedded at build time; the error is ignored only
	// because a build-time-constant subtree is guaranteed present.
	sub, _ := fs.Sub(assetsFS, "assets")
	fileServer := http.FileServerFS(sub)
	mux.Handle("GET /static/", http.StripPrefix("/static/", fileServer))

	// GET / is handled explicitly (not via the file server) so the root path
	// returns index.html rather than a directory listing (FR-3/FR-4).
	mux.HandleFunc("GET /{$}", s.handleIndex)

	// favicon.ico is requested by browsers before/instead of honoring the
	// in-page <link rel="icon">. Serve the embedded SVG so the request is a 200
	// rather than a logged 404. Content-Type is the SVG type even though the path
	// ends in .ico — browsers dispatch on the response type, not the URL suffix.
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)

	// The /api/* JSON handlers (FR-5/FR-6/FR-7) live in api.go.
	s.apiRoutes(mux)
}

// handleIndex serves the embedded assets/index.html as the dashboard page with
// an explicit text/html content type (FR-4).
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		// The file is embedded at build time, so a read failure is a programmer
		// error, not a runtime condition; surface it as a 500.
		http.Error(w, "index unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleFavicon serves the embedded assets/favicon.svg at /favicon.ico so the
// default browser favicon request resolves to a 200 with an explicit SVG
// content type rather than falling through to a 404 (mirrors handleIndex).
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	data, err := assetsFS.ReadFile("assets/favicon.svg")
	if err != nil {
		// The file is embedded at build time, so a read failure is a programmer
		// error, not a runtime condition; surface it as a 500.
		http.Error(w, "favicon unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// logRequests wraps h with a minimal middleware that logs each request's method,
// path, status, and duration at DEBUG. Status is captured via a small
// ResponseWriter wrapper since net/http exposes no read-back of the written code.
func (s *Server) logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		s.log.Debug("web: request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status)
	})
}

// statusRecorder captures the response status code for the logging middleware.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Start binds the listener on s.httpSrv.Addr synchronously so a bind failure
// (e.g. the port is already in use) is returned to the caller immediately,
// letting `serve` treat it as fatal (FR-3/FR-8). On a successful bind it serves
// in a background goroutine and returns nil. A post-bind Serve error other than
// http.ErrServerClosed is logged at ERROR but cannot re-trigger the fatal path,
// since the caller has already moved on.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.httpSrv.Addr)
	if err != nil {
		return err
	}
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.log.Error("web: serve", "err", err)
		}
	}()
	return nil
}

// Shutdown gracefully stops the HTTP server, waiting for in-flight handlers to
// return (bounded by ctx) before the caller closes the store's read handle
// (FR-8 shutdown ordering).
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}
