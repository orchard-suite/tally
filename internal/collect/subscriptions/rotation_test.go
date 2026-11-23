package subscriptions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var order = []rotationAccount{
	{Account: "a@example.org", Token: "sk-ant-oat01-a"},
	{Account: "b@example.org", Token: "sk-ant-oat01-b"},
	{Account: "c@example.org", Token: "sk-ant-oat01-c"},
}

// reading is an account read with its session and weekly percents, and a
// model-scoped weekly limit at scoped.
func reading(email string, session, weekly, scoped float64, resets int64) Reading {
	acct := Account{Provider: "claude", Email: email}
	return Reading{Account: acct, Limits: []Limit{
		{Provider: "claude", Account: email, Window: "session", Percent: session, ResetsAt: resets},
		{Provider: "claude", Account: email, Window: "weekly", Percent: weekly, ResetsAt: resets + 1000},
		{Provider: "claude", Account: email, Window: "weekly", Scope: "Fable", Percent: scoped, ResetsAt: resets + 1000},
	}}
}

func TestPickTakesTheMostPowerNext(t *testing.T) {
	cases := []struct {
		name      string
		readings  []Reading
		current   string
		want      string
		exhausted bool
	}{
		{"the most Fable left wins", []Reading{reading("a@example.org", 10, 20, 60, 100), reading("b@example.org", 10, 50, 30, 100), reading("c@example.org", 0, 0, 45, 100)}, "", "b@example.org", false},
		{"Fable used up: the next with Fable", []Reading{reading("a@example.org", 10, 60, 100, 100), reading("b@example.org", 0, 10, 70, 100)}, "a@example.org", "b@example.org", false},
		{"Fable left but the five hours are spent", []Reading{reading("a@example.org", 96, 20, 0, 100), reading("b@example.org", 0, 10, 80, 100)}, "", "b@example.org", false},
		{"Fable left but the week is spent", []Reading{reading("a@example.org", 0, 97, 0, 100), reading("b@example.org", 0, 10, 80, 100)}, "", "b@example.org", false},
		{"no Fable anywhere: the most-used week that can still run", []Reading{reading("a@example.org", 10, 40, 100, 100), reading("b@example.org", 10, 90, 99, 100), reading("c@example.org", 96, 94, 100, 100)}, "", "b@example.org", false},
		{"the current account stays while it is in the best group", []Reading{reading("a@example.org", 10, 20, 60, 100), reading("b@example.org", 10, 20, 10, 100)}, "a@example.org", "a@example.org", false},
		{"the current account leaves when a better group opens", []Reading{reading("a@example.org", 10, 20, 100, 100), reading("b@example.org", 10, 20, 10, 100)}, "a@example.org", "b@example.org", false},
		{"a failed read of the current account keeps it", []Reading{{Account: Account{Provider: "claude", Email: "a@example.org"}, Err: errors.New("timed out")}, reading("b@example.org", 0, 0, 0, 100)}, "a@example.org", "a@example.org", false},
		{"a failed read is passed over", []Reading{{Account: Account{Provider: "claude", Email: "a@example.org"}, Err: errSignedOut}, reading("b@example.org", 0, 0, 90, 100)}, "a@example.org", "b@example.org", false},
		{"nothing usable: the one that frees first", []Reading{reading("a@example.org", 100, 100, 0, 500), reading("b@example.org", 100, 10, 0, 50), reading("c@example.org", 100, 100, 0, 300)}, "a@example.org", "b@example.org", true},
		{"nothing read: the first stands", nil, "", "a@example.org", true},
		{"nothing read: the current account stands", []Reading{{Account: Account{Provider: "claude", Email: "a@example.org"}, Err: errSignedOut}, {Account: Account{Provider: "claude", Email: "c@example.org"}, Err: errSignedOut}}, "c@example.org", "c@example.org", true},
		{"emails match without regard to case", []Reading{reading("A@Example.org", 0, 0, 0, 100)}, "", "a@example.org", false},
	}
	for _, c := range cases {
		got := pick(order, c.readings, 95, "Fable", c.current, "")
		if got.Account != c.want || got.Exhausted != c.exhausted {
			t.Errorf("%s: picked %s (exhausted %v, %s), want %s (exhausted %v)", c.name, got.Account, got.Exhausted, got.Why, c.want, c.exhausted)
		}
		if got.Token != map[string]string{"a@example.org": "sk-ant-oat01-a", "b@example.org": "sk-ant-oat01-b", "c@example.org": "sk-ant-oat01-c"}[got.Account] {
			t.Errorf("%s: the token is not the picked account's", c.name)
		}
	}
}

// pro is a Pro account's read: its plan on every limit, and no limit on
// the preferred model unless scoped is given.
func pro(email string, session, weekly float64, scoped ...float64) Reading {
	r := reading(email, session, weekly, 0, 100)
	if len(scoped) == 0 {
		r.Limits = r.Limits[:2]
	} else {
		r.Limits[2].Percent = scoped[0]
	}
	for i := range r.Limits {
		r.Limits[i].Plan = "pro"
	}
	return r
}

func TestAReserveAccountRunsOnlyWhenNoLargerOneCan(t *testing.T) {
	cases := []struct {
		name     string
		readings []Reading
		current  string
		want     string
		next     string
	}{
		{"no preferred model is not the most of it left", []Reading{reading("a@example.org", 0, 50, 89, 100), reading("b@example.org", 0, 65, 100, 100), pro("c@example.org", 0, 1)}, "a@example.org", "a@example.org", "b@example.org"},
		{"a larger account's spent Fable beats a reserve with Fable", []Reading{reading("a@example.org", 0, 90, 100, 100), pro("c@example.org", 0, 1, 0)}, "", "a@example.org", "c@example.org"},
		{"a reserve with less of its week used still waits", []Reading{reading("a@example.org", 0, 5, 100, 100), pro("c@example.org", 0, 60)}, "c@example.org", "a@example.org", "c@example.org"},
		{"the reserve runs when every larger week is spent", []Reading{reading("a@example.org", 0, 97, 0, 100), reading("b@example.org", 96, 10, 0, 100), pro("c@example.org", 0, 1)}, "a@example.org", "c@example.org", "b@example.org"},
	}
	for _, c := range cases {
		got := pick(order, c.readings, 95, "Fable", c.current, "")
		if got.Account != c.want || got.Exhausted || len(got.Standings) < 2 || got.Standings[1].Account != c.next {
			t.Errorf("%s: picked %s then %+v (%s), want %s then %s", c.name, got.Account, got.Standings, got.Why, c.want, c.next)
		}
	}
	got := pick(order, []Reading{reading("a@example.org", 0, 50, 89, 100), pro("c@example.org", 0, 1)}, 95, "Fable", "", "")
	if s := got.Standings[1]; s.Account != "c@example.org" || s.PreferredAt != unknownTime || s.FreeAt != 0 {
		t.Fatalf("an account with no Fable: %+v; want it free now and never given Fable", s)
	}
}

func TestAPinHoldsUntilItsAccountHitsALimit(t *testing.T) {
	plenty := []Reading{reading("a@example.org", 10, 20, 10, 100), reading("b@example.org", 10, 20, 100, 100)}
	got := pick(order, plenty, 95, "Fable", "a@example.org", "b@example.org")
	if got.Account != "b@example.org" || got.Pinned != "b@example.org" || got.Unpin {
		t.Fatalf("a usable pinned account: %+v; want it picked, though a has more Fable", got)
	}

	spent := []Reading{reading("a@example.org", 10, 20, 10, 100), reading("b@example.org", 96, 20, 100, 100)}
	got = pick(order, spent, 95, "Fable", "b@example.org", "b@example.org")
	if got.Account != "a@example.org" || got.Pinned != "" || !got.Unpin {
		t.Fatalf("a pinned account at its 5-hour limit: %+v; want the pin released and the automatic pick", got)
	}

	failed := []Reading{reading("a@example.org", 10, 20, 10, 100), {Account: Account{Provider: "claude", Email: "b@example.org"}, Err: errors.New("timed out")}}
	got = pick(order, failed, 95, "Fable", "b@example.org", "b@example.org")
	if got.Account != "b@example.org" || got.Unpin {
		t.Fatalf("a pinned account whose read failed: %+v; want it kept", got)
	}

	signedOut := func(email string) Reading {
		return Reading{Account: Account{Provider: "claude", Email: email}, Err: errSignedOut}
	}
	blind := []Reading{signedOut("a@example.org"), signedOut("b@example.org"), signedOut("c@example.org")}
	got = pick(order, blind, 95, "Fable", "a@example.org", "c@example.org")
	if got.Account != "c@example.org" || got.Pinned != "c@example.org" || got.Unpin {
		t.Fatalf("a pin with no account read: %+v; want it kept", got)
	}

	got = pick(order, plenty, 95, "Fable", "a@example.org", "z@example.org")
	if got.Account != "a@example.org" || !got.Unpin {
		t.Fatalf("a pin on an account not in the rotation: %+v; want it released", got)
	}
}

func TestStandingsRankEveryAccountAndSayWhenItFrees(t *testing.T) {
	// a has spent its Fable but can run; b and c have Fable left but are at
	// their 5-hour limit, and c's resets first.
	readings := []Reading{reading("a@example.org", 2, 94, 100, 9000), reading("b@example.org", 98, 49, 75, 500), reading("c@example.org", 96, 45, 58, 300)}
	got := pick(order, readings, 95, "Fable", "a@example.org", "")
	want := []standing{
		{Account: "a@example.org", Rank: 1, Read: true, FreeAt: 0, PreferredAt: 10000},
		{Account: "c@example.org", Rank: 2, Read: true, FreeAt: 300, PreferredAt: 300},
		{Account: "b@example.org", Rank: 3, Read: true, FreeAt: 500, PreferredAt: 500},
	}
	if !slices.Equal(got.Standings, want) {
		t.Fatalf("standings = %+v\nwant %+v", got.Standings, want)
	}
	if !strings.HasPrefix(got.Why, "no account usable now has Fable left") {
		t.Fatalf("why = %q", got.Why)
	}

	got = pick(order, []Reading{reading("b@example.org", 0, 0, 0, 100)}, 95, "Fable", "b@example.org", "")
	if len(got.Standings) != 3 || got.Standings[0].Account != "b@example.org" || got.Standings[1].Read || got.Standings[2].Rank != 3 {
		t.Fatalf("unread accounts rank last with no times: %+v", got.Standings)
	}
}

func TestLoadRotation(t *testing.T) {
	dir := t.TempDir()
	if accts, err := loadRotation(filepath.Join(dir, "none.json")); err != nil || accts != nil {
		t.Fatalf("no file = %v, %v; want no rotation", accts, err)
	}
	path := filepath.Join(dir, "rotation.json")
	raw, _ := json.Marshal(order)
	os.WriteFile(path, raw, 0o600)
	if accts, err := loadRotation(path); err != nil || len(accts) != 3 || accts[1].Account != "b@example.org" {
		t.Fatalf("rotation = %v, %v", accts, err)
	}
	os.WriteFile(path, []byte(`[{"account":"a@example.org","token":"not-a-token"}]`), 0o600)
	if _, err := loadRotation(path); err == nil || strings.Contains(err.Error(), "not-a-token") {
		t.Fatalf("a bad token = %v; want an error that does not repeat it", err)
	}
}

// fakeKube stands in for the API server's secrets and config maps in one
// namespace.
type fakeKube struct {
	mu      sync.Mutex
	secrets map[string]secretObject
	maps    map[string]configMapObject
	// writes counts secret writes; pinWrites counts config map writes.
	writes, pinWrites int
}

func (f *fakeKube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer sa-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/usage-probe/")
	kind, name, _ := strings.Cut(rest, "/")
	raw, _ := io.ReadAll(r.Body)
	var found bool
	var obj any
	switch kind {
	case "secrets":
		obj, found = f.secrets[name]
	case "configmaps":
		obj, found = f.maps[name]
	}
	switch r.Method {
	case http.MethodGet:
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(obj)
		return
	case http.MethodPut:
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodPost:
		w.WriteHeader(http.StatusCreated)
	}
	if kind == "secrets" {
		var s secretObject
		json.Unmarshal(raw, &s)
		f.secrets[s.Metadata["name"].(string)] = s
		f.writes++
	} else {
		var m configMapObject
		json.Unmarshal(raw, &m)
		f.maps[m.Metadata["name"].(string)] = m
		f.pinWrites++
	}
}

func (f *fakeKube) pin(account string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.maps["claude-pin"] = configMapObject{Metadata: map[string]any{"name": "claude-pin"}, Data: map[string]string{"account": account}}
}

func (f *fakeKube) active(t *testing.T) active {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := base64.StdEncoding.DecodeString(f.secrets["claude-active"].Data["active"])
	var a active
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("active secret: %v (%q)", err, raw)
	}
	return a
}

func TestRotateHandsOnTheActiveAccountWhenItChanges(t *testing.T) {
	fake := &fakeKube{secrets: map[string]secretObject{}, maps: map[string]configMapObject{}}
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "rotation.json")
	raw, _ := json.Marshal(order)
	os.WriteFile(file, raw, 0o600)
	r := &rotator{file: file, bar: 95, prefer: "Fable", secret: "claude-active", pin: "claude-pin", kube: &kube{base: srv.URL, namespace: "usage-probe", token: "sa-token", http: srv.Client()}}
	ctx := context.Background()

	report := r.rotate(ctx, []Reading{reading("a@example.org", 10, 10, 0, 100)})
	if got := fake.active(t); got.Account != "a@example.org" || got.Token != "sk-ant-oat01-a" || fake.writes != 1 {
		t.Fatalf("first round: %+v after %d writes", got, fake.writes)
	}
	if report == nil || report.Active != "a@example.org" || len(report.Accounts) != 3 {
		t.Fatalf("report = %+v", report)
	}

	r.rotate(ctx, []Reading{reading("a@example.org", 50, 10, 0, 100)})
	if fake.writes != 1 {
		t.Fatalf("an unchanged pick wrote again: %d writes", fake.writes)
	}

	r.rotate(ctx, []Reading{reading("a@example.org", 96, 10, 0, 100), reading("b@example.org", 0, 0, 0, 100)})
	if got := fake.active(t); got.Account != "b@example.org" || fake.writes != 2 {
		t.Fatalf("after a's session hit the bar: %+v after %d writes", got, fake.writes)
	}

	fake.pin("a@example.org")
	if !r.pinChanged(ctx) {
		t.Fatal("a new pin went unseen")
	}
	report = r.rotate(ctx, []Reading{reading("a@example.org", 10, 10, 100, 100), reading("b@example.org", 0, 0, 0, 100)})
	if got := fake.active(t); got.Account != "a@example.org" || report.Pinned != "a@example.org" || r.pinChanged(ctx) {
		t.Fatalf("after a was picked by hand: %+v, report pinned %q", got, report.Pinned)
	}
	r.rotate(ctx, []Reading{reading("a@example.org", 97, 10, 100, 100), reading("b@example.org", 0, 0, 0, 100)})
	if got := fake.active(t); got.Account != "b@example.org" || fake.maps["claude-pin"].Data["account"] != "" || fake.pinWrites != 1 {
		t.Fatalf("after the pinned a hit its limit: %+v, pin %q", got, fake.maps["claude-pin"].Data["account"])
	}

	os.Remove(file)
	if report := r.rotate(ctx, nil); report != nil || fake.writes != 4 {
		t.Fatalf("with no rotation file: report %+v, %d writes; want nothing touched", report, fake.writes)
	}
}

func TestPayloadReportsTheActiveBuildAccount(t *testing.T) {
	rot := newRotationReport(order, "Fable", choice{active: active{Account: "b@example.org"}, Pinned: "b@example.org", Standings: []standing{
		{Account: "b@example.org", Rank: 1, Read: true},
		{Account: "a@example.org", Rank: 2, Read: true, FreeAt: 300, PreferredAt: 900},
		{Account: "c@example.org", Rank: 3},
	}})
	raw, _ := buildPayload([]Reading{reading("a@example.org", 1, 1, 1, 100)}, rot, time.Unix(1795000000, 0))
	var body payloadBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, m := range body.ResourceMetrics[0].ScopeMetrics[0].Metrics {
		for _, p := range m.Gauge.DataPoints {
			a := attrs(p)
			got[m.Name+" "+a["account"]] = p.AsDouble
		}
	}
	want := map[string]float64{
		metricActive + " a@example.org": 0, metricActive + " b@example.org": 1, metricActive + " c@example.org": 0,
		metricExhausted + " ":           0,
		metricPinned + " b@example.org": 1, metricPinned + " a@example.org": 0,
		metricRank + " b@example.org": 1, metricRank + " a@example.org": 2, metricRank + " c@example.org": 3,
		metricFreeAt + " a@example.org": 300, metricPreferredAt + " a@example.org": 900, metricFreeAt + " b@example.org": 0,
	}
	if _, ok := got[metricFreeAt+" c@example.org"]; ok {
		t.Error("an unread account sent a time it does not know")
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %v (present %v), want %v", k, g, ok, v)
		}
	}
	if strings.Contains(string(raw), "sk-ant-") {
		t.Fatal("a token reached the metrics")
	}
}
