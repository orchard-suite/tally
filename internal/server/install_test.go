package server

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestInstallFilesNeedNoSession holds the board to what a device needs to
// install it as an app: it fetches the icons and the manifest with no
// session and no identity, and the page names them. A missing file gives
// the device nothing to draw, and it draws a letter instead.
func TestInstallFilesNeedNoSession(t *testing.T) {
	srv := httptest.NewServer(newServer(t, "Viewer").Session())
	defer srv.Close()
	get := func(path string) (*http.Response, []byte) {
		t.Helper()
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res, body
	}
	size := func(png []byte) string {
		if len(png) < 24 || string(png[1:4]) != "PNG" {
			return "not a PNG"
		}
		return fmt.Sprintf("%dx%d", binary.BigEndian.Uint32(png[16:20]), binary.BigEndian.Uint32(png[20:24]))
	}

	sizes := map[string]string{"/apple-touch-icon.png": "180x180", "/icon-192.png": "192x192", "/icon-512.png": "512x512"}
	for path, want := range sizes {
		res, body := get(path)
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/png" {
			t.Errorf("%s: %d %q", path, res.StatusCode, res.Header.Get("Content-Type"))
		}
		if got := size(body); got != want {
			t.Errorf("%s is %s, want %s", path, got, want)
		}
	}

	res, body := get("/manifest.webmanifest")
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/manifest+json" {
		t.Fatalf("manifest: %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	var m struct {
		StartURL string `json:"start_url"`
		Display  string `json:"display"`
		Icons    []struct {
			Src, Sizes, Purpose string
		} `json:"icons"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m.StartURL != "/" || m.Display != "standalone" {
		t.Errorf("manifest opens %q as %q", m.StartURL, m.Display)
	}
	purposes := map[string]bool{}
	for _, icon := range m.Icons {
		if sizes[icon.Src] != icon.Sizes {
			t.Errorf("manifest says %s is %s", icon.Src, icon.Sizes)
		}
		purposes[icon.Sizes+" "+icon.Purpose] = true
	}
	for _, want := range []string{"192x192 any", "512x512 any", "512x512 maskable"} {
		if !purposes[want] {
			t.Errorf("manifest has no %s icon", want)
		}
	}

	page, _ := staticFS.ReadFile("static/index.html")
	for _, tag := range []string{
		`<link rel="apple-touch-icon" href="/apple-touch-icon.png">`,
		`<link rel="manifest" href="/manifest.webmanifest">`,
		`<meta name="theme-color"`,
		`<meta name="mobile-web-app-capable" content="yes">`,
		`<meta name="apple-mobile-web-app-capable" content="yes">`,
		`<meta name="apple-mobile-web-app-title" content="Tally">`,
		`<meta name="apple-mobile-web-app-status-bar-style"`,
	} {
		if !strings.Contains(string(page), tag) {
			t.Errorf("index.html lacks %s", tag)
		}
	}

	if res, _ := get("/v1/costs"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("the board's data answered %d with no identity", res.StatusCode)
	}
}
