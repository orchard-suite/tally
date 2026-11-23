package catalog

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/orrery"

	"github.com/orchard-suite/tally/internal/collect"
	"github.com/orchard-suite/tally/internal/collect/kubernetes"
	"github.com/orchard-suite/tally/internal/collect/power"
	"github.com/orchard-suite/tally/internal/estate"
)

func fixture(t *testing.T) estate.Costs {
	t.Helper()
	raw, err := os.ReadFile("testdata/costs.json")
	if err != nil {
		t.Fatal(err)
	}
	var c estate.Costs
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func ok(names ...string) []collect.Status {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	out := make([]collect.Status, 0, len(names))
	for _, n := range names {
		out = append(out, collect.Status{Name: n, Rounds: 1, LastRun: &at, LastSuccess: &at})
	}
	return out
}

func TestEntitiesEnrichEveryNamespace(t *testing.T) {
	got := Entities(fixture(t), "home")
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	want := map[string]float64{"home/db": 33, "home/web": 27, "home/idle": 0}
	for _, e := range got {
		if e.Blueprint != Blueprint || e.Title != "" || len(e.Relations) != 0 {
			t.Errorf("an enrich write carries the cost fields only: %+v", e)
		}
		if e.Fields[CostField] != want[e.Key] {
			t.Errorf("%s cost = %v, want %v", e.Key, e.Fields[CostField], want[e.Key])
		}
		if e.Fields[UpdateField] != "2026-11-01T12:00:00Z" {
			t.Errorf("%s updated = %v", e.Key, e.Fields[UpdateField])
		}
	}
}

func TestStale(t *testing.T) {
	c := fixture(t)
	if r := Stale(ok("kubernetes", "power"), c); r != "" {
		t.Fatalf("fresh inputs judged stale: %s", r)
	}
	failed := ok("kubernetes", "power")
	later := failed[1].LastSuccess.Add(time.Minute)
	failed[1].LastRun = &later
	if r := Stale(failed, c); r != "power's last round failed" {
		t.Errorf("failed power: %q", r)
	}
	if r := Stale(ok("power"), c); r != "kubernetes is off" {
		t.Errorf("no kubernetes: %q", r)
	}
	never := ok("kubernetes", "power")
	never[0].LastSuccess = nil
	if r := Stale(never, c); r != "kubernetes has not finished a round" {
		t.Errorf("kubernetes never worked: %q", r)
	}
	noCluster := fixture(t)
	noCluster.Hosts[0].Cluster = false
	if r := Stale(ok("kubernetes", "power"), noCluster); r != "no cluster host's power was read" {
		t.Errorf("no cluster power: %q", r)
	}
}

// hub is a fake Orrery hub that records what it was sent.
type hub struct {
	mu       sync.Mutex
	paths    []string
	run      map[string]string
	entities []orrery.Entity
	offered  []map[string]any
}

func (h *hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.paths = append(h.paths, r.Method+" "+r.URL.Path)
	body, _ := io.ReadAll(r.Body)
	switch r.Method + " " + r.URL.Path {
	case "PUT /v1/ingest/blueprints":
		var in struct {
			Blueprints []map[string]any `json:"blueprints"`
		}
		_ = json.Unmarshal(body, &in)
		h.offered = in.Blueprints
		_, _ = w.Write([]byte(`{"extended":["k8s_namespace"]}`))
	case "POST /v1/ingest/runs":
		_ = json.Unmarshal(body, &h.run)
		_, _ = w.Write([]byte(`{"id":"r1"}`))
	case "PUT /v1/ingest/runs/r1/entities":
		var in struct {
			Entities []orrery.Entity `json:"entities"`
		}
		_ = json.Unmarshal(body, &in)
		h.entities = append(h.entities, in.Entities...)
		_, _ = w.Write([]byte(`{"accepted":2}`))
	case "POST /v1/ingest/runs/r1/commit":
		_, _ = w.Write([]byte(`{"id":"r1","state":"committed","counts":{"updated":2}}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writer(t *testing.T, h *hub, statuses []collect.Status) *Writer {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	inst, err := collect.NewInstruments()
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sources := estate.Sources{
		Power: func() *power.Snapshot {
			return &power.Snapshot{Hosts: []power.Host{{Host: "node-a", CostPerHour: 0.1}}}
		},
		Workloads: func() *kubernetes.Snapshot {
			return &kubernetes.Snapshot{Workloads: []kubernetes.Workload{
				{Namespace: "web", Pods: 1, Containers: []kubernetes.Container{{Name: "app", CPU: kubernetes.Measure{Request: 1}, Memory: kubernetes.Measure{Request: 100}}}},
				{Namespace: "db", Pods: 1, Containers: []kubernetes.Container{{Name: "app", CPU: kubernetes.Measure{Request: 3}, Memory: kubernetes.Measure{Request: 300}}}},
			}}
		},
	}
	w := New(orrery.New(srv.URL, "source-token", orrery.WithRetries(0), orrery.WithLogger(log)), log, inst, sources, "home", time.Minute)
	w.statuses = func() []collect.Status { return statuses }
	return w
}

func TestRoundWritesCostsInEnrichMode(t *testing.T) {
	h := &hub{}
	w := writer(t, h, ok("kubernetes", "power"))
	if err := w.Round(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(h.offered) != 1 || h.offered[0]["id"] != Blueprint {
		t.Fatalf("offered %+v", h.offered)
	}
	fields, _ := h.offered[0]["fields"].(map[string]any)
	if _, ok := fields[CostField]; !ok {
		t.Errorf("the blueprint offer lacks %s: %+v", CostField, fields)
	}
	if h.run["mode"] != "incremental" || h.run["write"] != "enrich" || h.run["scope"] != Scope {
		t.Errorf("run = %+v, want an incremental enrich run of %s", h.run, Scope)
	}
	got := map[string]float64{}
	for _, e := range h.entities {
		if e.Blueprint != Blueprint {
			t.Errorf("wrote %s", e.ID())
		}
		got[e.Key], _ = e.Fields[CostField].(float64)
		if _, ok := e.Fields[UpdateField].(string); !ok {
			t.Errorf("%s has no %s", e.Key, UpdateField)
		}
	}
	// 73 a month: web has a quarter of the CPU and the memory, db the rest.
	if len(got) != 2 || got["home/web"] < 18.24 || got["home/web"] > 18.26 || got["home/db"] < 54.74 || got["home/db"] > 54.76 {
		t.Errorf("costs = %+v", got)
	}

	// The blueprint is offered once.
	if err := w.Round(t.Context()); err != nil {
		t.Fatal(err)
	}
	offers := 0
	for _, p := range h.paths {
		if p == "PUT /v1/ingest/blueprints" {
			offers++
		}
	}
	if offers != 1 {
		t.Errorf("offered %d times: %v", offers, h.paths)
	}
}

func TestStaleRoundWritesNothing(t *testing.T) {
	h := &hub{}
	statuses := ok("kubernetes", "power")
	later := statuses[1].LastSuccess.Add(time.Minute)
	statuses[1].LastRun = &later
	w := writer(t, h, statuses)
	if err := w.Round(t.Context()); err == nil {
		t.Fatal("a stale round must fail")
	}
	if len(h.paths) != 0 {
		t.Errorf("a stale round sent %v", h.paths)
	}
}
