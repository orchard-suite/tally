package kubernetes

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/orchard-suite/tally/internal/collect/promapi"
)

const mi = 1 << 20

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func answers(t *testing.T) map[string][]promapi.Sample {
	t.Helper()
	raw, err := os.ReadFile("testdata/answers.json")
	if err != nil {
		t.Fatal(err)
	}
	var in map[string][]promapi.Sample
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	return in
}

func TestSizeRoundsUpToWhatAPersonWrites(t *testing.T) {
	cases := []struct{ v, floor, step, want float64 }{
		{0.0009, CPUFloor, CPUStep, 0.01},
		{0.065, CPUFloor, CPUStep, 0.065},
		{0.0651, CPUFloor, CPUStep, 0.07},
		{1, MemoryFloor, MemoryStep, MemoryFloor},
		{130 * mi, MemoryFloor, MemoryStep, 144 * mi},
		{144 * mi, MemoryFloor, MemoryStep, 144 * mi},
	}
	for _, c := range cases {
		if got := Size(c.v, c.floor, c.step); !near(got, c.want) {
			t.Errorf("Size(%v) = %v, want %v", c.v, got, c.want)
		}
	}
}

func TestCPUIsSizedToItsP95(t *testing.T) {
	r := DefaultRule
	cases := []struct {
		request, p95 float64
		verdict      string
		recommended  float64
	}{
		{0.5, 0.05, Over, 0.065},
		{0.1, 0.5, Under, 0.65},
		{0, 0.2, Unset, 0.26},
		{1, 1, "", 1.3},
		{2.1, 1, Over, 1.3},
		{0.79, 1, Under, 1.3},
		{0.25, 0, "", 0},
		// Over twice its use, but the floor is no smaller: nothing to free.
		{0.01, 0.001, "", 0.01},
	}
	for _, c := range cases {
		m := r.JudgeCPU(Measure{Request: c.request, P95: c.p95}, 0)
		if m.Verdict != c.verdict || !near(m.Recommended, c.recommended) {
			t.Errorf("request %v p95 %v: %q %v, want %q %v", c.request, c.p95, m.Verdict, m.Recommended, c.verdict, c.recommended)
		}
	}
}

// Memory past its limit is a kill, so it is sized to its peak: a p95 far
// below the peak must not shrink the request under it.
func TestMemoryIsSizedToItsPeak(t *testing.T) {
	m := DefaultRule.JudgeMemory(Measure{Request: 1792 * mi, P95: 63 * mi, Peak: 4500 * mi})
	if m.Verdict != Under || m.Recommended < 4500*mi {
		t.Fatalf("a peak over the request is under, never over: %+v", m)
	}
	m = DefaultRule.JudgeMemory(Measure{Request: 512 * mi, P95: 90 * mi, Peak: 100 * mi})
	if m.Verdict != Over || m.Recommended != 144*mi {
		t.Fatalf("%+v", m)
	}
}

// An advised request over the limit raises the limit with it.
func TestAdviceStaysWithinTheLimit(t *testing.T) {
	c := DefaultRule.JudgeCPU(Measure{Request: 0.05, Limit: 1, P95: 1.36}, 0)
	if c.Verdict != Under || !near(c.Recommended, 1.77) || !near(c.RecommendedLimit, 2.655) {
		t.Errorf("cpu %+v", c)
	}
	// A peak of 400 MiB fits under its 512 MiB limit, but the advised
	// 528 MiB request does not.
	m := DefaultRule.JudgeMemory(Measure{Request: 128 * mi, Limit: 512 * mi, Peak: 400 * mi})
	if m.Verdict != Under || m.Recommended != 528*mi || m.RecommendedLimit != 800*mi {
		t.Errorf("memory %+v", m)
	}
	if m := DefaultRule.JudgeMemory(Measure{Request: 512 * mi, Limit: 1024 * mi, Peak: 100 * mi}); m.RecommendedLimit != 0 {
		t.Errorf("a request under its limit keeps the limit: %+v", m)
	}
}

func TestTroubleIsARisk(t *testing.T) {
	r := DefaultRule
	m := r.JudgeMemory(Measure{Request: 128 * mi, Limit: 512 * mi, Peak: 300 * mi, Kills: 149.3})
	if m.Verdict != Risk || m.Reason != "killed for memory 149 times in the window" || m.RecommendedLimit != 768*mi {
		t.Errorf("killed: %+v", m)
	}
	m = r.JudgeMemory(Measure{Request: 128 * mi, Limit: 256 * mi, Peak: 240 * mi})
	if m.Verdict != Risk || !strings.Contains(m.Reason, "94% of its limit") {
		t.Errorf("near the limit: %+v", m)
	}
	// A risk never shrinks the request.
	m = r.JudgeMemory(Measure{Request: 1024 * mi, Limit: 1024 * mi, Peak: 100 * mi, Kills: 1})
	if m.Verdict != Risk || m.Recommended != 1024*mi || m.Reason != "killed for memory once in the window" {
		t.Errorf("risk keeps the request: %+v", m)
	}
	c := r.JudgeCPU(Measure{Request: 0.05, Limit: 0.5, P95: 0.04, Peak: 0.5, Throttled: 0.6}, 10000)
	if c.Verdict != Risk || c.RecommendedLimit != 0.75 || c.Recommended != 0.055 {
		t.Errorf("throttled: %+v", c)
	}
	// A container that barely ran is not throttled.
	if c := r.JudgeCPU(Measure{Request: 0.05, Limit: 0.5, P95: 0.04, Throttled: 0.9}, 100); c.Verdict == Risk {
		t.Errorf("few periods: %+v", c)
	}
}

func TestShapeKeepsTheWorkloadsThatRunNow(t *testing.T) {
	got, room := Shape(answers(t), DefaultRule, []string{"kube-system"})
	if room.CPU != 16 || room.Memory != 64*1024*mi {
		t.Errorf("allocatable %+v", room)
	}
	by := map[string]Workload{}
	var names []string
	for _, w := range got {
		by[w.Namespace+"/"+w.Workload] = w
		names = append(names, w.Namespace+"/"+w.Workload)
	}
	want := []string{"batch/nightly", "ci/repo-a-runner", "idle/nothing", "kube-system/coredns", "kube-system/kube-proxy-node1", "web/api", "web/portal"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("workloads %v, want %v (a gone deployment, a retired CronJob's run, and a one-off Job are left out)", names, want)
	}

	portal := by["web/portal"]
	if portal.Pods != 2 || len(portal.Containers) != 1 {
		t.Fatalf("portal %+v", portal)
	}
	app := portal.Containers[0]
	if app.Name != "app" || app.CPU.Verdict != Over || app.Memory.Verdict != Over || app.Memory.Recommended != 144*mi {
		t.Errorf("portal app %+v", app)
	}
	if cpu, mem := portal.Requests(); !near(cpu, 1) || mem != 1024*mi {
		t.Errorf("portal requests %v %v", cpu, mem)
	}

	api := by["web/api"].Containers[0]
	if api.Memory.Verdict != Risk || api.Memory.Kills != 3 || api.CPU.Verdict != Risk || !near(api.CPU.Throttled, 0.5) {
		t.Errorf("api %+v", api)
	}

	nightly := by["batch/nightly"]
	if nightly.Kind != CronJob || nightly.Pods != 0 || nightly.Containers[0].CPU.Verdict != Unset {
		t.Errorf("a CronJob's runs fold into it: %+v", nightly)
	}

	runner := by["ci/repo-a-runner"]
	if runner.Kind != RunnerSet || runner.Pods != 1 || len(runner.Containers) != 1 {
		t.Fatalf("runners fold into their set, and an init container with nothing measured is dropped: %+v", runner)
	}
	if r := runner.Containers[0]; r.CPU.P95 != 1.2 || r.CPU.Peak != 2.5 || r.Memory.Peak != 700*mi || r.Memory.Verdict != Unset {
		t.Errorf("the set takes its busiest run: %+v", r)
	}

	if !by["kube-system/kube-proxy-node1"].Managed || !by["kube-system/coredns"].Managed || portal.Managed {
		t.Errorf("managed: static pods and the patterns' namespaces")
	}
	if c := by["idle/nothing"].Containers[0]; c.CPU.Verdict != "" || c.Memory.Verdict != "" {
		t.Errorf("a container that used nothing gets no verdict: %+v", c)
	}
}

func TestRankPutsRiskFirst(t *testing.T) {
	if !(Rank(Risk) < Rank(Under) && Rank(Under) < Rank(Unset) && Rank(Unset) < Rank(Over) && Rank(Over) < Rank("")) {
		t.Fatal("rank order")
	}
}

func TestQueriesTakeTheBusiestPodOfEachWorkload(t *testing.T) {
	q := Queries("7d")
	want := `max by (namespace, workload, workload_type, container) (quantile_over_time(0.95, node_namespace_pod_container:container_cpu_usage_seconds_total:sum_irate[7d]) * on (namespace, pod) group_left (workload, workload_type) max_over_time(namespace_workload_pod:kube_pod_owner:relabel[7d]))`
	if q["cpu_p95"] != want {
		t.Errorf("cpu_p95 =\n%s\nwant\n%s", q["cpu_p95"], want)
	}
	// A pod a rollout replaced still counts its kills toward its workload.
	if !strings.Contains(q["memory_kills"], `max_over_time(namespace_workload_pod:kube_pod_owner:relabel[7d])`) {
		t.Errorf("memory_kills joins the window's owners: %s", q["memory_kills"])
	}
	// A size is the running pods' own, or else the last the window saw.
	if !strings.Contains(q["memory_request"], `group_left (workload, workload_type) namespace_workload_pod:kube_pod_owner:relabel) or max by`) ||
		!strings.Contains(q["memory_request"], `last_over_time(kube_pod_container_resource_requests{resource="memory"}[7d])`) {
		t.Errorf("memory_request: %s", q["memory_request"])
	}
}

// A sidecar's requests are under the init container metrics and hold for
// the pod's life, so they count; an init container that finishes first
// does not.
func TestResourcesCountSidecars(t *testing.T) {
	q := Resources("requests", `namespace="ci"`, "cpu")
	for _, want := range []string{
		`kube_pod_container_resource_requests{namespace="ci", resource="cpu"}`,
		`kube_pod_init_container_resource_requests{namespace="ci", resource="cpu"}`,
		`kube_pod_init_container_info{restart_policy="Always"}`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("%s lacks %s", q, want)
		}
	}
	if q := Resources("limits", "", "memory"); !strings.Contains(q, `kube_pod_container_resource_limits{resource="memory"}`) {
		t.Errorf("no selector: %s", q)
	}
}
