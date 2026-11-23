package runners

import (
	"math"
	"os"
	"strings"
	"testing"

	"github.com/orchard-suite/tally/internal/collect/promapi"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func vector(t *testing.T, file string) []promapi.Sample {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	s, err := promapi.ParseVector(raw)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sum(samples []promapi.Sample, prefix string) float64 {
	var v float64
	for _, s := range samples {
		if strings.HasPrefix(s.Labels["pod"], prefix) {
			v += s.Value
		}
	}
	return v
}

// Invented runner pods in the shape a Prometheus answers with: each runner
// set's pods add up under its repository, named from the listeners' gauges.
func TestGitHubRunnerPodsAddUpByRepository(t *testing.T) {
	answers := map[string][]promapi.Sample{
		"pod_seconds":    vector(t, "gh-pod-seconds.json"),
		"cpu_seconds":    vector(t, "gh-cpu-seconds.json"),
		"memory_seconds": vector(t, "gh-memory-seconds.json"),
	}
	names := RunnerSetNames(vector(t, "gh-runner-sets.json"))
	if names["example-web-app"] != "example/web.app" {
		t.Fatalf("names = %v", names)
	}
	got := Name(GitHub, Keys(GitHub, answers), names)
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	by := map[string]Usage{}
	for _, u := range got {
		by[u.Repository] = u
	}
	api := by["example/api"]
	if !near(api.PodSeconds, sum(answers["pod_seconds"], "example-api-runner-")) || api.PodSeconds == 0 {
		t.Errorf("api = %+v", api)
	}
	// A runner asks for at least half a core and 1 GiB.
	if api.CPUSeconds < api.PodSeconds/2 || api.MemorySeconds < api.PodSeconds*(1<<30) {
		t.Errorf("api requests = %+v", api)
	}
	for i := 1; i < len(got); i++ {
		if got[i].PodSeconds > got[i-1].PodSeconds {
			t.Errorf("longest first: %+v", got)
		}
	}
}

// Invented GitLab job pods in the shape a Prometheus answers with: the project id in
// each pod's name is the key, and a project with no name shows by id.
func TestGitLabJobPodsAddUpByProject(t *testing.T) {
	answers := map[string][]promapi.Sample{
		"pod_seconds":    vector(t, "gl-pod-seconds.json"),
		"cpu_seconds":    vector(t, "gl-cpu-seconds.json"),
		"memory_seconds": vector(t, "gl-memory-seconds.json"),
	}
	// A project that ran one short job beside the others.
	answers["pod_seconds"] = append(answers["pod_seconds"], promapi.Sample{Labels: map[string]string{"pod": "runner-abcd1234x-project-11-concurrent-0-aaaa1111"}, Value: 60})
	keys := Keys(GitLab, answers)
	if len(keys) != 2 || keys["4242"] == nil {
		t.Fatalf("keys = %v", keys)
	}
	got := Name(GitLab, keys, map[string]string{"4242": "group/web"})
	by := map[string]Usage{}
	for _, u := range got {
		by[u.Repository] = u
	}
	if !near(by["group/web"].PodSeconds, sum(answers["pod_seconds"], "runner-abcd1234x-project-4242-")) {
		t.Errorf("web = %+v", by["group/web"])
	}
	if _, ok := by["project 11"]; !ok {
		t.Errorf("an unnamed project shows by id: %+v", got)
	}
}

func TestKeysSkipPodsThatAreNotJobs(t *testing.T) {
	answers := map[string][]promapi.Sample{"pod_seconds": {
		{Labels: map[string]string{"pod": "gitlab-webservice-default-6d9f-abcde"}, Value: 600},
		{Labels: map[string]string{"pod": "runner-abcd1234x-project-7-concurrent-0-zzzz1111"}, Value: 60},
	}}
	keys := Keys(GitLab, answers)
	if len(keys) != 1 || keys["7"].PodSeconds != 60 {
		t.Errorf("keys = %v", keys)
	}
}

func TestPoolOnItsOwnNodes(t *testing.T) {
	cfg := Config{Namespace: "gitlab", Nodes: "aks-ci-.*", PricePerHour: 0.4}
	p, err := PoolFrom(cfg, map[string][]promapi.Sample{"node_hours": vector(t, "gl-node-hours.json")})
	if err != nil {
		t.Fatal(err)
	}
	if p.NodeHours <= 0 || p.PricePerHour != 0.4 {
		t.Errorf("pool = %+v", p)
	}
}

func TestPoolSharingTheCluster(t *testing.T) {
	one := func(v float64) []promapi.Sample { return []promapi.Sample{{Labels: map[string]string{}, Value: v}} }
	p, err := PoolFrom(Config{Namespace: "arc-runners"}, map[string][]promapi.Sample{
		"ns_cpu": one(0.5), "all_cpu": one(10), "ns_memory": one(1), "all_memory": one(40),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !near(p.CPUShare, 0.05) || !near(p.MemoryShare, 0.025) {
		t.Errorf("pool = %+v", p)
	}
	if _, err := PoolFrom(Config{Namespace: "arc-runners"}, map[string][]promapi.Sample{
		"ns_cpu": one(0), "all_cpu": one(0), "ns_memory": one(0), "all_memory": one(0),
	}); err == nil {
		t.Error("a cluster that requested nothing is a failed read, not a zero share")
	}
}

func TestQueriesAskOnlyWhatThePoolNeeds(t *testing.T) {
	own := Queries(Config{Namespace: "gitlab", Forge: GitLab, Nodes: "aks-ci-.*", Window: "7d", Hours: 168})
	if _, ok := own["pod_seconds/27"]; !ok {
		t.Error("a week is 28 slices")
	}
	if _, ok := own["node_hours"]; !ok {
		t.Error("a pool on its own nodes reads its node hours")
	}
	if _, ok := own["ns_cpu"]; ok {
		t.Error("a pool on its own nodes needs no share of the cluster")
	}
	shared := Queries(Config{Namespace: "arc-runners", Forge: GitHub, Window: "36h", Hours: 36})
	if _, ok := shared["all_memory"]; !ok {
		t.Error("a shared pool reads the cluster's requests")
	}
	// 36 hours is six slices of six hours, newest first, with no overlap.
	if q := shared["cpu_seconds/0"]; !strings.Contains(q, `[6h:60s]) * 60`) {
		t.Errorf("cpu_seconds/0 = %s", q)
	}
	if q := shared["cpu_seconds/5"]; !strings.Contains(q, `[6h:60s] offset 30h)`) {
		t.Errorf("cpu_seconds/5 = %s", q)
	}
	if _, ok := shared["cpu_seconds/6"]; ok {
		t.Error("six slices cover 36 hours")
	}
}

func TestWindowHours(t *testing.T) {
	for in, want := range map[string]float64{"7d": 168, "36h": 36, "1d": 24} {
		if got, err := WindowHours(in); err != nil || got != want {
			t.Errorf("%s = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0d", "90m", "week"} {
		if _, err := WindowHours(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestProjectPath(t *testing.T) {
	raw, err := os.ReadFile("testdata/gitlab-project.json")
	if err != nil {
		t.Fatal(err)
	}
	if p, err := ProjectPath(raw); err != nil || p != "group/web" {
		t.Errorf("path = %q, %v", p, err)
	}
}
