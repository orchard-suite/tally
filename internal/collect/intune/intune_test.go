package intune

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orchard-suite/tally/internal/collect"
)

const (
	h1 = "364adac65056e7302a554f7b0c4d695abb1a173b1e49ef3afe881f424dd3c4c5"
	h2 = "1111111111111111111111111111111111111111111111111111111111111111"
	h3 = "2222222222222222222222222222222222222222222222222222222222222222"
)

func dev(name string) *struct {
	DeviceName string `json:"deviceName"`
} {
	return &struct {
		DeviceName string `json:"deviceName"`
	}{name}
}

func TestParseScripts(t *testing.T) {
	s, err := ParseScripts("mac:abc, windows:def")
	if err != nil || len(s) != 2 || s[0] != (Script{"mac", "abc"}) || s[1] != (Script{"windows", "def"}) {
		t.Fatalf("%v %v", s, err)
	}
	if _, err := ParseScripts("linux:x"); err == nil {
		t.Error("an unknown kind is refused")
	}
}

func TestHashIn(t *testing.T) {
	if got := HashIn("installed\ntally-ship=" + strings.ToUpper(h1) + "\n"); got != h1 {
		t.Errorf("a hash on its line, lowercased: %q", got)
	}
	if got := HashIn("acme-agents: synced; tally-ship=" + h2 + " (new)"); got != h2 {
		t.Errorf("a hash inside a line: %q", got)
	}
	if HashIn("tally-ship=nothex") != "" || HashIn("nothing here") != "" || HashIn("tally-ship=abcd") != "" {
		t.Error("only a whole sha256 is a hash")
	}
}

func TestMachines(t *testing.T) {
	got := Machines([]RunState{
		{ResultMessage: "tally-ship=" + h1, ManagedDevice: dev("STUDIO-MAC")},
		{PreRemediationDetectionOutput: "tally-ship=" + h3, PostRemediationDetectionOutput: "tally-ship=" + h2, ManagedDevice: dev("BUILD-BOX")},
		{ResultMessage: "Claude Code is not installed; nothing to do", ManagedDevice: dev("WEB-01")},
		{ResultMessage: "tally-ship=" + h1, ManagedDevice: nil},
	})
	want := map[string]string{h1: "studio-mac", h2: "build-box"}
	if len(got) != len(want) || got[h1] != want[h1] || got[h2] != want[h2] {
		t.Fatalf("machines %v, want %v", got, want)
	}
	clash := Machines([]RunState{
		{ResultMessage: "tally-ship=" + h1, ManagedDevice: dev("a")},
		{ResultMessage: "tally-ship=" + h1, ManagedDevice: dev("b")},
	})
	if len(clash) != 0 {
		t.Errorf("a token two machines report names neither: %v", clash)
	}
}

type enrolled struct{ got map[string]string }

func (e *enrolled) Enroll(m map[string]string) { e.got = m }

func TestRoundReadsEveryPageOfEveryScript(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "deviceShellScripts/mac1") && r.URL.Query().Get("page") == "":
			io.WriteString(w, `{"value":[{"resultMessage":"tally-ship=`+h1+`","managedDevice":{"deviceName":"Mac-A"}}],"@odata.nextLink":"`+srv.URL+r.URL.Path+`?page=2"}`)
		case strings.Contains(r.URL.Path, "deviceShellScripts/mac1"):
			io.WriteString(w, `{"value":[{"resultMessage":"tally-ship=`+h3+`","managedDevice":{"deviceName":"Mac-B"}}]}`)
		case strings.Contains(r.URL.Path, "deviceHealthScripts/win1"):
			io.WriteString(w, `{"value":[{"preRemediationDetectionScriptOutput":"tally-ship=`+h2+`","managedDevice":{"deviceName":"PC-C"}}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	e := &enrolled{}
	inst, err := collect.NewInstruments()
	if err != nil {
		t.Fatal(err)
	}
	c := &Collector{scripts: []Script{{"mac", "mac1"}, {"windows", "win1"}}, token: func(context.Context) (string, error) { return "t", nil },
		agents: e, inst: inst, client: srv.Client(), base: srv.URL, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := c.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.got) != 3 || e.got[h1] != "mac-a" || e.got[h3] != "mac-b" || e.got[h2] != "pc-c" {
		t.Fatalf("enrolled %v", e.got)
	}
	e.got = nil
	c.scripts = append(c.scripts, Script{"mac", "gone"})
	if err := c.Round(context.Background()); err == nil || e.got != nil {
		t.Fatal("a failed read enrolls nothing and keeps the last machines")
	}
}
