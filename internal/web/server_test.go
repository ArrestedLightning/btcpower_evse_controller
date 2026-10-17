package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArrestedLightning/btcpower_evse_controller/internal/app"
	"github.com/ArrestedLightning/btcpower_evse_controller/internal/config"
)

const password = "pw"

func newServer(t *testing.T, ports int) (*httptest.Server, *app.App, string) {
	t.Helper()
	return newServerPW(t, ports, password)
}

func newServerPW(t *testing.T, ports int, pw string) (*httptest.Server, *app.App, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{AdminPassword: pw, DataDir: filepath.Join(dir, "data")}
	for i := 0; i < ports; i++ {
		cfg.Chargers = append(cfg.Chargers, config.Charger{Serial: "/dev/sim" + string(rune('0'+i)), MaxAmps: 32})
	}
	cfg.ApplyDefaults()
	path := filepath.Join(dir, "config.yaml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := app.New(path, cfg, true, false, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.Start(ctx)
	srv := httptest.NewServer((&Server{App: a, Version: "test", Log: log}).Handler())
	t.Cleanup(func() { srv.Close(); cancel(); a.Stop() })
	return srv, a, path
}

func do(t *testing.T, srv *httptest.Server, method, path string, body io.Reader, auth bool) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, body)
	if auth {
		req.SetBasicAuth("admin", password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestStatusAndPages(t *testing.T) {
	for _, n := range []int{1, 4} {
		srv, _, _ := newServer(t, n)
		resp, body := do(t, srv, "GET", "/api/v1/status", nil, false)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		var st struct {
			Version string `json:"version"`
			Ports   []struct {
				ID    int    `json:"id"`
				State string `json:"state"`
			} `json:"ports"`
		}
		if err := json.Unmarshal(body, &st); err != nil || len(st.Ports) != n || st.Version != "test" {
			t.Fatalf("status body %s (%v)", body, err)
		}
		for _, p := range []string{"/", "/static/app.js", "/static/style.css", "/logo"} {
			if resp, _ := do(t, srv, "GET", p, nil, false); resp.StatusCode != 200 {
				t.Errorf("%s: %d", p, resp.StatusCode)
			}
		}
		if resp, _ := do(t, srv, "GET", "/api/v1/ports/1", nil, false); resp.StatusCode != 200 {
			t.Errorf("port 1: %d", resp.StatusCode)
		}
		if resp, _ := do(t, srv, "GET", "/api/v1/ports/9", nil, false); resp.StatusCode != 404 {
			t.Errorf("port 9: %d", resp.StatusCode)
		}
	}
}

func TestAdminRequiresAuth(t *testing.T) {
	srv, _, _ := newServer(t, 2)
	for _, r := range []struct{ method, path string }{
		{"GET", "/settings"}, {"GET", "/api/v1/config"}, {"PUT", "/api/v1/config"},
		{"POST", "/api/v1/ports/1/disable"}, {"PUT", "/api/v1/logo"}, {"DELETE", "/api/v1/logo"},
	} {
		if resp, _ := do(t, srv, r.method, r.path, nil, false); resp.StatusCode != 401 {
			t.Errorf("%s %s without auth: %d", r.method, r.path, resp.StatusCode)
		}
	}
	if resp, _ := do(t, srv, "GET", "/settings", nil, true); resp.StatusCode != 200 {
		t.Errorf("settings with auth: %d", resp.StatusCode)
	}
}

func TestEnableDisable(t *testing.T) {
	srv, _, _ := newServer(t, 2)
	resp, body := do(t, srv, "POST", "/api/v1/ports/2/disable", nil, true)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"enabled": false`) {
		t.Fatalf("disable: %d %s", resp.StatusCode, body)
	}
	_, body = do(t, srv, "GET", "/api/v1/ports/2", nil, false)
	if !strings.Contains(string(body), `"enabled": false`) {
		t.Fatalf("not disabled: %s", body)
	}
}

func TestConfigRoundTripAndReload(t *testing.T) {
	srv, a, path := newServer(t, 2)
	_, body := do(t, srv, "GET", "/api/v1/config", nil, true)
	if strings.Contains(string(body), password) {
		t.Fatal("password exposed")
	}
	var cfg config.Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}

	// Invalid: rejected, nothing changes.
	bad := cfg
	bad.Chargers = nil
	b, _ := json.Marshal(bad)
	if resp, _ := do(t, srv, "PUT", "/api/v1/config", bytes.NewReader(b), true); resp.StatusCode != 422 {
		t.Fatalf("invalid config: %d", resp.StatusCode)
	}

	// Valid: add a third charger and enable sharing.
	cfg.Chargers = append(cfg.Chargers, config.Charger{Name: "Port 3", Serial: "/dev/sim9", MaxAmps: 16})
	cfg.LoadSharing.Enabled, cfg.LoadSharing.SystemMaxAmps = true, 48
	b, _ = json.Marshal(cfg)
	if resp, body := do(t, srv, "PUT", "/api/v1/config", bytes.NewReader(b), true); resp.StatusCode != 200 {
		t.Fatalf("save: %d %s", resp.StatusCode, body)
	}
	if n := len(a.Station().Snapshot().Ports); n != 3 {
		t.Fatalf("station not reloaded: %d ports", n)
	}
	saved, err := config.Load(path)
	if err != nil || len(saved.Chargers) != 3 || saved.AdminPassword != password {
		t.Fatalf("saved config: %+v %v", saved, err)
	}
}

func TestDisplayOptions(t *testing.T) {
	srv, a, _ := newServer(t, 1)
	_, body := do(t, srv, "GET", "/", nil, false)
	if !strings.Contains(string(body), `class="theme-dark"`) || strings.Contains(string(body), `clock" hidden`) {
		t.Fatalf("default page: %s", body)
	}
	cfg := a.Config()
	hide := false
	cfg.Display = config.Display{Theme: config.ThemeLight, ShowClock: &hide, ShowLastSession: &hide}
	if err := a.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	_, body = do(t, srv, "GET", "/", nil, false)
	if !strings.Contains(string(body), `class="theme-light"`) || !strings.Contains(string(body), `<div id="clock" class="clock" hidden>`) {
		t.Fatalf("light page: %s", body)
	}
	_, body = do(t, srv, "GET", "/api/v1/status", nil, false)
	if !strings.Contains(string(body), `"theme": "light"`) || !strings.Contains(string(body), `"show_clock": false`) ||
		!strings.Contains(string(body), `"show_last_session": false`) {
		t.Fatalf("status display: %s", body)
	}
}

func TestLogoUpload(t *testing.T) {
	srv, _, _ := newServer(t, 1)
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	if resp, body := do(t, srv, "PUT", "/api/v1/logo", bytes.NewReader(png), true); resp.StatusCode != 200 {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}
	resp, body := do(t, srv, "GET", "/logo", nil, false)
	if resp.Header.Get("Content-Type") != "image/png" || !bytes.Equal(body, png) {
		t.Fatalf("logo: %s %d bytes", resp.Header.Get("Content-Type"), len(body))
	}
	if resp, _ := do(t, srv, "PUT", "/api/v1/logo", strings.NewReader("not an image"), true); resp.StatusCode != 415 {
		t.Fatalf("bad upload: %d", resp.StatusCode)
	}
	big := bytes.Repeat([]byte{0}, maxLogoBytes+1)
	if resp, _ := do(t, srv, "PUT", "/api/v1/logo", bytes.NewReader(big), true); resp.StatusCode != 413 {
		t.Fatalf("big upload: %d", resp.StatusCode)
	}
	do(t, srv, "DELETE", "/api/v1/logo", nil, true)
	if resp, _ := do(t, srv, "GET", "/logo", nil, false); resp.Header.Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("default logo type %s", resp.Header.Get("Content-Type"))
	}
}

func TestNoPasswordDisablesAdmin(t *testing.T) {
	srv, _, _ := newServerPW(t, 1, "")
	if resp, _ := do(t, srv, "GET", "/settings", nil, true); resp.StatusCode != 403 {
		t.Fatalf("settings without configured password: %d", resp.StatusCode)
	}
}
