package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/faro"

	"github.com/orchard-suite/tally/internal/agents"
	"github.com/orchard-suite/tally/internal/collect"
	"github.com/orchard-suite/tally/internal/collect/gateway"
	"github.com/orchard-suite/tally/internal/estate"
	"github.com/orchard-suite/tally/internal/pool"
)

func newServer(t *testing.T, role string) *Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	inst, err := collect.NewInstruments()
	if err != nil {
		t.Fatal(err)
	}
	relay, err := faro.NewRelay(faro.Config{Service: collect.Service, Version: "test"}, log)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Role: role, Version: "test"}, estate.Sources{}, inst, relay, log)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAPrefixedRoleIsRefused(t *testing.T) {
	inst, err := collect.NewInstruments()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Role: "tally.Viewer"}, estate.Sources{}, inst, http.NotFoundHandler(), slog.New(slog.DiscardHandler)); err == nil {
		t.Error("tally.Viewer was taken as a role")
	}
}

func TestSessionNeedsTheFrontDoor(t *testing.T) {
	srv := httptest.NewServer(newServer(t, "Viewer").Session())
	defer srv.Close()
	get := func(headers map[string]string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/costs", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := get(nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("no identity: %d", res.StatusCode)
	}
	if res := get(map[string]string{"X-Forwarded-Email": "user@example.com", "X-Forwarded-Groups": "other"}); res.StatusCode != http.StatusForbidden {
		t.Errorf("wrong role: %d", res.StatusCode)
	}
	res := get(map[string]string{"X-Forwarded-Email": "user@example.com", "X-Forwarded-Groups": "x, Viewer"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("right role: %d", res.StatusCode)
	}
	var b Board
	if err := json.NewDecoder(res.Body).Decode(&b); err != nil {
		t.Fatal(err)
	}
	if b.Version != "test" || len(b.Categories) != 3 || b.Trend == nil {
		t.Errorf("board = %+v", b)
	}
}

func TestBoardPageAndAssetsAreServed(t *testing.T) {
	srv := httptest.NewServer(newServer(t, "").Session())
	defer srv.Close()
	for _, path := range []string{"/", "/static/app.css", "/static/app.js", "/static/estate.js", "/static/agents.js"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("X-Forwarded-Email", "user@example.com")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK || len(body) == 0 {
			t.Errorf("%s: %d, %d bytes", path, res.StatusCode, len(body))
		}
		if path == "/" && !strings.Contains(string(body), "tally") {
			t.Errorf("the page is not the board")
		}
	}
}

func TestRelayWithoutAKeyDropsAndAnswers(t *testing.T) {
	srv := httptest.NewServer(newServer(t, "").Session())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/telemetry", strings.NewReader(`{"meta":{},"events":[]}`))
	req.Header.Set("X-Forwarded-Email", "user@example.com")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusAccepted {
		t.Errorf("dropped batch answered %d", res.StatusCode)
	}
}

func TestAgentViewsFollowRoles(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	inst, err := collect.NewInstruments()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("secret"))
	t.Setenv("AGENTS_DB", filepath.Join(t.TempDir(), "agents.db"))
	t.Setenv("SHIP_TOKENS", "laptop="+hex.EncodeToString(sum[:]))
	t.Setenv("MACHINE_OWNERS", "laptop=A user")
	ag, err := agents.FromEnv(log, inst)
	if err != nil {
		t.Fatal(err)
	}
	relay, _ := faro.NewRelay(faro.Config{Service: collect.Service, Version: "test"}, log)
	if _, err := New(Config{Role: "Viewer", AdminRole: "tally.Admin", Agents: ag}, estate.Sources{}, inst, relay, log); err == nil {
		t.Fatal("a prefixed admin role was taken")
	}
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Role: "Viewer", DetailRole: "Detail", AdminRole: "Admin", Agents: ag, Zone: chicago}, estate.Sources{}, inst, relay, log)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(s.API())
	defer api.Close()
	session := httptest.NewServer(s.Session())
	defer session.Close()

	body := `{"works":[{"id":"w1","title":"Polish the board","project":"tally"}],
"conversations":[{"id":"c1","work":"w1","agent":"claude","role":"lead","title":"Polish the board"},{"id":"c2","work":"w1","agent":"claude","role":"child","title":"Look"}],
"buckets":[{"conversation":"c1","model":"claude-opus-5-5","minute":"2026-11-04T16:00:00Z","requests":1,"input":1,"cache_write_5m":0,"cache_write_1h":0,"cache_read":0,"output":1},
{"conversation":"c2","model":"claude-opus-5-5","minute":"2026-11-04T16:01:00Z","requests":1,"input":1,"cache_write_5m":0,"cache_write_1h":0,"cache_read":0,"output":1}]}`
	req, _ := http.NewRequest(http.MethodPost, api.URL+"/v1/usage", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusNoContent {
		t.Fatalf("post: %v %v", res, err)
	}

	day := func(roles string) (int, agents.Report) {
		req, _ := http.NewRequest(http.MethodGet, session.URL+"/v1/agents?day=2026-11-04", nil)
		req.Header.Set("X-Forwarded-Email", "user@example.com")
		req.Header.Set("X-Forwarded-Groups", roles)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var rep agents.Report
		_ = json.NewDecoder(res.Body).Decode(&rep)
		return res.StatusCode, rep
	}
	if code, _ := day(""); code != http.StatusForbidden {
		t.Fatalf("no role: %d", code)
	}
	code, rep := day("Viewer")
	if code != http.StatusOK || rep.ShowsUsers || rep.ShowsWorks || len(rep.Machines) != 0 || len(rep.Works) != 0 || rep.Children.Conversations != 0 {
		t.Fatalf("a viewer sees totals and models only: %d %+v", code, rep)
	}
	if rep.Totals.Total != 4 || len(rep.Models) != 1 {
		t.Fatalf("a viewer sees the totals and the models: %+v", rep)
	}
	code, rep = day("Detail")
	if code != http.StatusOK || !rep.ShowsUsers || rep.ShowsWorks || len(rep.Works) != 0 || rep.Children.Conversations != 0 {
		t.Fatalf("the detail role alone opens the board and sees no work: %d %+v", code, rep)
	}
	if len(rep.Machines) != 1 || rep.Machines[0].Owner != "A user" || rep.Machines[0].Total != 4 {
		t.Fatalf("the detail role sees who spent what: %+v", rep.Machines)
	}
	code, rep = day("Admin")
	if code != http.StatusOK || !rep.ShowsUsers || !rep.ShowsWorks || len(rep.Works) != 1 || rep.Works[0].Title != "Polish the board" || rep.Children.Conversations != 1 || len(rep.Machines) != 1 {
		t.Fatalf("an admin sees the work, its children, and who spent what: %d %+v", code, rep)
	}
	if rep.Zone != "America/Chicago" || !rep.From.Equal(time.Date(2026, 11, 4, 0, 0, 0, 0, chicago)) {
		t.Fatalf("with no tz the day is the board's: %s from %s", rep.Zone, rep.From)
	}
}

func TestGatewayKeysAreTheDetailTiers(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	inst, err := collect.NewInstruments()
	if err != nil {
		t.Fatal(err)
	}
	relay, _ := faro.NewRelay(faro.Config{Service: collect.Service, Version: "test"}, log)
	snap := &gateway.Snapshot{Rows: []gateway.Row{{Day: "2026-10-03", Key: "dev--ada--max", Product: "dev_pro_max", Model: "claude-opus-5-5", Tokens: agents.Tokens{Input: 1000}}}}
	sources := estate.Sources{
		Gateway: func() *gateway.Snapshot { return snap },
		Prices:  func() agents.Table { return agents.Prices{"claude-opus-5-5": {Input: 1e-3}}.Table() },
	}
	s, err := New(Config{Role: "Viewer", DetailRole: "Detail", AdminRole: "Admin"}, sources, inst, relay, log)
	if err != nil {
		t.Fatal(err)
	}
	session := httptest.NewServer(s.Session())
	defer session.Close()
	get := func(roles, query string) (int, GatewayKeys) {
		req, _ := http.NewRequest(http.MethodGet, session.URL+"/v1/gateway/keys"+query, nil)
		req.Header.Set("X-Forwarded-Email", "user@example.com")
		req.Header.Set("X-Forwarded-Groups", roles)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out GatewayKeys
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	if code, _ := get("Viewer", "?month=2026-10"); code != http.StatusForbidden {
		t.Fatalf("a viewer never sees a key: %d", code)
	}
	if code, _ := get("Detail", "?month=October"); code != http.StatusBadRequest {
		t.Fatalf("a month that is not one: %d", code)
	}
	code, out := get("Detail", "?month=2026-10")
	if code != http.StatusOK || out.Month != "2026-10" || len(out.Keys) != 1 || out.Keys[0].Key != "dev--ada--max" || out.Keys[0].Cost != 1 {
		t.Fatalf("the detail role sees every key: %d %+v", code, out)
	}
	if code, _ := get("Admin", ""); code != http.StatusOK {
		t.Fatalf("an admin sees keys too: %d", code)
	}
}

func TestAMachineReadsItsHeadroomByItsToken(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	inst, err := collect.NewInstruments()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("secret"))
	t.Setenv("AGENTS_DB", filepath.Join(t.TempDir(), "agents.db"))
	t.Setenv("SHIP_TOKENS", "laptop="+hex.EncodeToString(sum[:]))
	ag, err := agents.FromEnv(log, inst)
	if err != nil {
		t.Fatal(err)
	}
	relay, _ := faro.NewRelay(faro.Config{Service: collect.Service, Version: "test"}, log)
	s, err := New(Config{Role: "Viewer", Agents: ag}, estate.Sources{}, inst, relay, log)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(s.API())
	defer api.Close()
	get := func(token string) (int, []byte) {
		req, _ := http.NewRequest(http.MethodGet, api.URL+"/v1/machine/headroom?model=opus", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	if code, _ := get(""); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := get("wrong"); code != http.StatusUnauthorized {
		t.Fatalf("an unknown token: %d", code)
	}
	// With no Prometheus nothing is read, and the answer says so.
	code, body := get("secret")
	var h pool.Headroom
	if err := json.Unmarshal(body, &h); err != nil || code != http.StatusOK {
		t.Fatalf("the machine's token: %d %s", code, body)
	}
	if h.Provider != "claude" || !h.Usable || h.Used != 0 || h.Binding != "no limit read" {
		t.Fatalf("nothing read: %+v", h)
	}
}

func TestARunIsPricedForItsMachineAndRolledUp(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	inst, err := collect.NewInstruments()
	if err != nil {
		t.Fatal(err)
	}
	mine, theirs := sha256.Sum256([]byte("secret")), sha256.Sum256([]byte("other"))
	prices := filepath.Join(t.TempDir(), "prices.yaml")
	if err := os.WriteFile(prices, []byte("models:\n  claude-opus-5-5:\n    - {from: 2026-11-01, input: 1, output: 1000000, cache_read: 0, cache_write: 0}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTS_DB", filepath.Join(t.TempDir(), "agents.db"))
	t.Setenv("SHIP_TOKENS", "laptop-1="+hex.EncodeToString(mine[:])+",laptop-2="+hex.EncodeToString(theirs[:]))
	t.Setenv("PRICES_FILE", prices)
	ag, err := agents.FromEnv(log, inst)
	if err != nil {
		t.Fatal(err)
	}
	relay, _ := faro.NewRelay(faro.Config{Service: collect.Service, Version: "test"}, log)
	s, err := New(Config{Role: "Viewer", DetailRole: "Detail", Agents: ag}, estate.Sources{}, inst, relay, log)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(s.API())
	defer api.Close()
	session := httptest.NewServer(s.Session())
	defer session.Close()

	const run = "3f0c9a52-1d7e-4b8a-9c11-2b6f8e4d0a71"
	body := `{"conversations":[{"id":"claude:` + run + `","work":"w1","agent":"claude","role":"lead"},{"id":"claude:` + run + `/agent-a1","work":"w1","agent":"claude","role":"child","parent":"claude:` + run + `"}],
"buckets":[{"conversation":"claude:` + run + `","model":"claude-opus-5-5","minute":"2026-11-20T15:05:00Z","requests":1,"input":0,"cache_write_5m":0,"cache_write_1h":0,"cache_read":0,"output":2},
{"conversation":"claude:` + run + `/agent-a1","model":"claude-opus-5-5","minute":"2026-11-20T15:06:00Z","requests":1,"input":0,"cache_write_5m":0,"cache_write_1h":0,"cache_read":0,"output":3}]}`
	req, _ := http.NewRequest(http.MethodPost, api.URL+"/v1/usage", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusNoContent {
		t.Fatalf("post: %v %v", res, err)
	}
	price := func(token, id string) (int, agents.RunCost) {
		req, _ := http.NewRequest(http.MethodGet, api.URL+"/v1/runs/"+id, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var c agents.RunCost
		_ = json.NewDecoder(res.Body).Decode(&c)
		return res.StatusCode, c
	}
	if code, c := price("secret", run); code != http.StatusOK || c.Amount != 5 || c.Currency != "USD" || c.Tokens != 5 || c.Conversations != 2 {
		t.Fatalf("the machine's run at the estate's price: %d %+v", code, c)
	}
	if code, _ := price("other", run); code != http.StatusNotFound {
		t.Fatalf("another machine prices only its own runs: %d", code)
	}
	if code, _ := price("", run); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := price("secret", "a%20b"); code != http.StatusBadRequest {
		t.Fatalf("not a run id: %d", code)
	}

	at := time.Date(2026, 11, 20, 15, 4, 0, 0, time.UTC)
	if err := ag.Store().TakeFeed(context.Background(), []agents.Claim{{Run: run, Item: "ACME-7", Agent: "claude-code on laptop-1", At: at}}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := ag.Store().SetItemScope(context.Background(), "ACME-7", "billing", at); err != nil {
		t.Fatal(err)
	}
	spend := func(roles string) agents.SpendReport {
		req, _ := http.NewRequest(http.MethodGet, session.URL+"/v1/agents/spend?from=2026-11-20&to=2026-11-20&tz=UTC", nil)
		req.Header.Set("X-Forwarded-Email", "user@example.com")
		req.Header.Set("X-Forwarded-Groups", roles)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("spend: %d", res.StatusCode)
		}
		var rep agents.SpendReport
		_ = json.NewDecoder(res.Body).Decode(&rep)
		return rep
	}
	rep := spend("Viewer")
	if rep.ShowsHosts || len(rep.Items) != 1 || rep.Items[0].Item != "ACME-7" || rep.Items[0].Cost != 5 || len(rep.Agents) != 1 || rep.Agents[0].Agent != "claude-code" {
		t.Fatalf("a viewer sees items and agents by name: %+v", rep)
	}
	if rep := spend("Detail"); !rep.ShowsHosts || rep.Agents[0].Agent != "claude-code on laptop-1" {
		t.Fatalf("the detail role sees an agent's host: %+v", rep.Agents)
	}
}
