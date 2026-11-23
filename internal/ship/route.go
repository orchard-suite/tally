package ship

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Route is which replies a shipper may post: those an AI gateway served.
// A tally that meters one estate's gateway takes only the work that went
// through it, so work a person ran on their own subscription never leaves
// the machine, not even as a title.
//
// The route is a gateway's host. Ion names the provider that served a
// reply as the prefix of its model ("acme-gateway/claude-opus-5-5"), and
// its engine.json gives each provider's base URL; a reply counts when its
// provider's host is the route's. A bare model is one Ion handed to a
// signed-in CLI, and a Claude Code transcript names no route at all: both
// ran on the machine's own subscription, so neither counts. An empty
// route counts every reply.
type Route struct {
	Host      string
	providers map[string]bool
	// claude is whether Claude Code sends its requests to Host: then its
	// replies, which name a bare model, went through the gateway.
	claude bool
}

// Allows says whether a reply from model may be posted.
func (r Route) Allows(model string) bool {
	if r.Host == "" {
		return true
	}
	provider, _, ok := strings.Cut(model, "/")
	if !ok {
		return model != "" && r.claude
	}
	return r.providers[provider]
}

// Through is the route to host, with the providers engine.json (Ion's
// settings) points at it. A missing engine.json is no provider.
func Through(host, engine, claude string) (Route, error) {
	r := Route{Host: host, providers: map[string]bool{}}
	if host == "" {
		return r, nil
	}
	if u, err := url.Parse(claude); err == nil && claude != "" && strings.EqualFold(u.Hostname(), host) {
		r.claude = true
	}
	b, err := os.ReadFile(engine)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	for id, base := range IonProviders(b) {
		if u, err := url.Parse(base); err == nil && strings.EqualFold(u.Hostname(), host) {
			r.providers[id] = true
		}
	}
	return r, nil
}

// IonProviders reads each provider's base URL from Ion's engine.json, by
// provider id. It reads nothing else: the file also holds credentials.
func IonProviders(engine []byte) map[string]string {
	var v struct {
		Providers map[string]struct {
			BaseURL string `json:"baseURL"`
		} `json:"providers"`
	}
	out := map[string]string{}
	if json.Unmarshal(engine, &v) != nil {
		return out
	}
	for id, p := range v.Providers {
		if p.BaseURL != "" {
			out[id] = p.BaseURL
		}
	}
	return out
}

// ClaudeSettings are where Claude Code reads its settings, in the order a
// setting wins: the managed settings an administrator deploys (a file, or
// on macOS the preference profile), then the person's own under home, the
// folder that holds Claude Code's projects. A missing one is skipped.
func ClaudeSettings(home, user string) []string {
	var managed []string
	switch runtime.GOOS {
	case "darwin":
		managed = []string{
			"/Library/Managed Preferences/" + user + "/com.anthropic.claudecode.plist",
			"/Library/Managed Preferences/com.anthropic.claudecode.plist",
			"/Library/Application Support/ClaudeCode/managed-settings.json",
		}
	case "windows":
		managed = []string{`C:\Program Files\ClaudeCode\managed-settings.json`}
	default:
		managed = []string{"/etc/claude-code/managed-settings.json"}
	}
	return append(managed, filepath.Join(home, "settings.local.json"), filepath.Join(home, "settings.json"))
}

// ClaudeBaseURL is where Claude Code sends its requests: ANTHROPIC_BASE_URL
// from the first of paths that sets it in its env, else fallback, the
// shipper's own environment's. A plist is read through plutil.
func ClaudeBaseURL(paths []string, fallback string) string {
	for _, p := range paths {
		b, err := readSettings(p)
		if err != nil {
			continue
		}
		var v struct {
			Env map[string]string `json:"env"`
		}
		if json.Unmarshal(b, &v) == nil && v.Env["ANTHROPIC_BASE_URL"] != "" {
			return v.Env["ANTHROPIC_BASE_URL"]
		}
	}
	return fallback
}

func readSettings(path string) ([]byte, error) {
	if !strings.HasSuffix(path, ".plist") {
		return os.ReadFile(path)
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return exec.Command("plutil", "-convert", "json", "-o", "-", path).Output()
}
