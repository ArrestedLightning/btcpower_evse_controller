// Package web serves the status UI, settings UI and JSON API.
package web

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ArrestedLightning/btcpower_evse_controller/internal/app"
	"github.com/ArrestedLightning/btcpower_evse_controller/internal/config"
	"github.com/ArrestedLightning/btcpower_evse_controller/internal/station"
)

//go:embed assets
var assets embed.FS

// maxLogoBytes limits uploaded logo size.
const maxLogoBytes = 1 << 20

// logoTypes lists accepted logo file extensions and content types, in the
// order they are looked for in the data directory (a fixed order, in case
// more than one logo file exists).
var logoTypes = []struct{ ext, contentType string }{
	{".png", "image/png"},
	{".jpg", "image/jpeg"},
	{".jpeg", "image/jpeg"},
	{".svg", "image/svg+xml"},
	{".webp", "image/webp"},
}

// Server is the HTTP front end.
type Server struct {
	App     *app.App
	Version string
	Log     *slog.Logger
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(assets, "assets")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /{$}", s.page(static, "index.html"))
	mux.HandleFunc("GET /settings", s.admin(s.page(static, "settings.html")))
	mux.HandleFunc("GET /logo", s.getLogo)

	mux.HandleFunc("GET /api/v1/status", s.getStatus)
	mux.HandleFunc("GET /api/v1/ports/{id}", s.getPort)
	mux.HandleFunc("POST /api/v1/ports/{id}/enable", s.admin(s.setEnabled(true)))
	mux.HandleFunc("POST /api/v1/ports/{id}/disable", s.admin(s.setEnabled(false)))
	mux.HandleFunc("GET /api/v1/sessions", s.getSessions)
	mux.HandleFunc("GET /api/v1/config", s.admin(s.getConfig))
	mux.HandleFunc("PUT /api/v1/config", s.admin(s.putConfig))
	mux.HandleFunc("PUT /api/v1/logo", s.admin(s.putLogo))
	mux.HandleFunc("DELETE /api/v1/logo", s.admin(s.deleteLogo))
	mux.HandleFunc("POST /api/v1/sim/ports/{id}/{action}", s.simAction)
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'")
		h.ServeHTTP(w, r)
	})
}

// page serves an HTML page with the configured theme and clock visibility
// applied, so it renders correctly before the first status poll.
func (s *Server) page(static fs.FS, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(static, name)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		d := s.App.Config().Display
		html := strings.Replace(string(data), "<html lang=\"en\">", `<html lang="en" class="theme-`+d.Theme+`">`, 1)
		if !d.ClockVisible() {
			html = strings.Replace(html, `<div id="clock" class="clock">`, `<div id="clock" class="clock" hidden>`, 1)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		io.WriteString(w, html)
	}
}

// admin requires HTTP Basic auth with the configured admin password (any
// user name). With no password configured, admin functions are disabled.
func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := s.App.Config().AdminPassword
		if want == "" {
			httpError(w, http.StatusForbidden, "admin functions are disabled: set admin_password in the config file")
			return
		}
		_, pw, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pw), []byte(want)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="EVSE settings", charset="UTF-8"`)
			httpError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

type statusResponse struct {
	station.Status
	Version     string         `json:"version"`
	LogoVersion int64          `json:"logo_version"`
	Display     displayOptions `json:"display"`
}

type displayOptions struct {
	Theme           string `json:"theme"`
	ShowClock       bool   `json:"show_clock"`
	ShowLastSession bool   `json:"show_last_session"`
}

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request) {
	d := s.App.Config().Display
	resp := statusResponse{
		Status:  s.App.Station().Snapshot(),
		Version: s.Version,
		Display: displayOptions{Theme: d.Theme, ShowClock: d.ClockVisible(), ShowLastSession: d.LastSessionVisible()},
	}
	if p, _ := s.logoPath(); p != "" {
		if fi, err := os.Stat(p); err == nil {
			resp.LogoVersion = fi.ModTime().UnixNano()
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) portID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid port id")
		return 0, false
	}
	return id, true
}

func (s *Server) getPort(w http.ResponseWriter, r *http.Request) {
	id, ok := s.portID(w, r)
	if !ok {
		return
	}
	snap := s.App.Station().Snapshot()
	if id < 1 || id > len(snap.Ports) {
		httpError(w, http.StatusNotFound, "no such port")
		return
	}
	writeJSON(w, http.StatusOK, snap.Ports[id-1])
}

func (s *Server) setEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.portID(w, r)
		if !ok {
			return
		}
		st := s.App.Station()
		if err := st.SetEnabled(id, enabled); err != nil {
			httpError(w, http.StatusNotFound, err.Error())
			return
		}
		s.Log.Info("port enable changed", "port", id, "enabled", enabled, "remote", r.RemoteAddr)
		writeJSON(w, http.StatusOK, st.Snapshot().Ports[id-1])
	}
}

func (s *Server) getSessions(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			httpError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	sessions := s.App.Station().Sessions(limit)
	if sessions == nil {
		sessions = []station.Session{}
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.App.Config()
	cfg.AdminPassword = "" // write-only
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	cur := s.App.Config()
	var cfg config.Config
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if cfg.AdminPassword == "" {
		cfg.AdminPassword = cur.AdminPassword
	}
	if err := s.App.ApplyConfig(&cfg); err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.Log.Info("configuration updated", "remote", r.RemoteAddr)
	saved := s.App.Config()
	saved.AdminPassword = ""
	writeJSON(w, http.StatusOK, saved)
}

// logoPath returns the uploaded logo file and its content type, if any.
func (s *Server) logoPath() (string, string) {
	dir := s.App.DataDir()
	for _, t := range logoTypes {
		p := filepath.Join(dir, "logo"+t.ext)
		if _, err := os.Stat(p); err == nil {
			return p, t.contentType
		}
	}
	return "", ""
}

func (s *Server) getLogo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	// Defuse any script in an uploaded SVG opened directly.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:")
	p, ct := s.logoPath()
	if p == "" {
		w.Header().Set("Content-Type", "image/svg+xml")
		http.ServeFileFS(w, r, assets, "assets/default-logo.svg")
		return
	}
	f, err := os.Open(p)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "cannot read logo")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "cannot read logo")
		return
	}
	w.Header().Set("Content-Type", ct)
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// detectLogoType identifies an uploaded image by content.
func detectLogoType(data []byte) (string, error) {
	switch http.DetectContentType(data) {
	case "image/png":
		return ".png", nil
	case "image/jpeg":
		return ".jpg", nil
	case "image/webp":
		return ".webp", nil
	}
	head := strings.ToLower(string(data[:min(len(data), 1024)]))
	if strings.Contains(head, "<svg") {
		return ".svg", nil
	}
	return "", errors.New("logo must be a PNG, JPEG, WebP or SVG image")
}

func (s *Server) putLogo(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLogoBytes))
	if err != nil {
		httpError(w, http.StatusRequestEntityTooLarge, "logo must be at most 1 MB")
		return
	}
	ext, err := detectLogoType(data)
	if err != nil {
		httpError(w, http.StatusUnsupportedMediaType, err.Error())
		return
	}
	dir := s.App.DataDir()
	if err := config.WriteFileAtomic(filepath.Join(dir, "logo"+ext), data, 0o644); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, t := range logoTypes {
		if t.ext != ext {
			os.Remove(filepath.Join(dir, "logo"+t.ext))
		}
	}
	s.Log.Info("logo updated", "type", ext, "bytes", len(data), "remote", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated": time.Now()})
}

func (s *Server) deleteLogo(w http.ResponseWriter, r *http.Request) {
	dir := s.App.DataDir()
	for _, t := range logoTypes {
		os.Remove(filepath.Join(dir, "logo"+t.ext))
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// simAction drives simulated vehicles; only available in simulate mode.
func (s *Server) simAction(w http.ResponseWriter, r *http.Request) {
	id, ok := s.portID(w, r)
	if !ok {
		return
	}
	sim := s.App.Station().Sim(id)
	if sim == nil {
		httpError(w, http.StatusNotFound, "not simulating, or no such port")
		return
	}
	switch r.PathValue("action") {
	case "plug":
		sim.Plug()
	case "unplug":
		sim.Unplug()
	case "request":
		sim.SetRequesting(true)
	case "stop":
		sim.SetRequesting(false)
	case "fault":
		sim.SetFault(true)
	case "clear":
		sim.SetFault(false)
	case "auto":
		sim.SetAuto(true)
	case "manual":
		sim.SetAuto(false)
	default:
		httpError(w, http.StatusBadRequest, "action must be plug, unplug, request, stop, fault, clear, auto or manual")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
