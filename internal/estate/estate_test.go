package estate

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/tally/internal/agents"
	"github.com/orchard-suite/tally/internal/collect/advisor"
	"github.com/orchard-suite/tally/internal/collect/aispend"
	"github.com/orchard-suite/tally/internal/collect/azure"
	"github.com/orchard-suite/tally/internal/collect/billed"
	"github.com/orchard-suite/tally/internal/collect/kubernetes"
	"github.com/orchard-suite/tally/internal/collect/machines"
	"github.com/orchard-suite/tally/internal/collect/power"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func one(cpu, memory kubernetes.Measure) []kubernetes.Container {
	return []kubernetes.Container{{Name: "app", CPU: cpu, Memory: memory}}
}

func workloads() []kubernetes.Workload {
	return []kubernetes.Workload{
		{Namespace: "web", Workload: "portal", Kind: "deployment", Pods: 1, Containers: one(kubernetes.Measure{Request: 1, P95: 0.1, Verdict: kubernetes.Over, Recommended: 0.13}, kubernetes.Measure{Request: 300, Peak: 100})},
		{Namespace: "web", Workload: "worker", Kind: "deployment", Pods: 1, Containers: one(kubernetes.Measure{Request: 1}, kubernetes.Measure{Request: 100})},
		{Namespace: "db", Workload: "postgres", Kind: "statefulset", Pods: 1, Containers: one(kubernetes.Measure{Request: 2}, kubernetes.Measure{Request: 600, Peak: 1000, Verdict: kubernetes.Under, Recommended: 1300})},
		{Namespace: "idle", Workload: "nothing", Kind: "deployment"},
	}
}

func TestAllocateSplitsPowerByRequestedCPUAndMemory(t *testing.T) {
	got := Allocate(workloads(), 100)
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	// CPU: web 2 of 4, db 2 of 4. Memory: web 400 of 1000, db 600 of 1000.
	byNS := map[string]Namespace{}
	var total float64
	for _, n := range got {
		byNS[n.Namespace] = n
		total += n.PerMonth
	}
	if !near(byNS["web"].PerMonth, 45) || !near(byNS["db"].PerMonth, 55) || !near(byNS["idle"].PerMonth, 0) {
		t.Errorf("shares = %+v", byNS)
	}
	if !near(total, 100) {
		t.Errorf("the shares must add up to the cluster's cost: %v", total)
	}
	if got[0].Namespace != "db" {
		t.Errorf("most expensive first: %+v", got)
	}
}

func TestAllocateWithOnlyCPURequested(t *testing.T) {
	ws := []kubernetes.Workload{
		{Namespace: "a", Pods: 3, Containers: one(kubernetes.Measure{Request: 1}, kubernetes.Measure{})},
		{Namespace: "b", Pods: 1, Containers: one(kubernetes.Measure{Request: 1}, kubernetes.Measure{})},
	}
	got := Allocate(ws, 40)
	if !near(got[0].PerMonth, 30) || !near(got[1].PerMonth, 10) {
		t.Errorf("got %+v", got)
	}
	if got := Allocate(nil, 40); len(got) != 0 {
		t.Errorf("no workloads, no namespaces: %+v", got)
	}
}

func TestWorkloadsRankRiskFirstThenWhatEachFrees(t *testing.T) {
	gi := float64(1 << 30)
	snap := &kubernetes.Snapshot{Allocatable: kubernetes.Capacity{CPU: 10, Memory: 40 * gi}, Workloads: []kubernetes.Workload{
		// Two pods, each asking 1 core and 4 GiB more than it needs.
		{Namespace: "web", Workload: "big", Kind: "deployment", Pods: 2, Containers: one(
			kubernetes.Measure{Request: 1.5, Recommended: 0.5, Verdict: kubernetes.Over},
			kubernetes.Measure{Request: 6 * gi, Recommended: 2 * gi, Verdict: kubernetes.Over})},
		{Namespace: "web", Workload: "small", Kind: "deployment", Pods: 1, Containers: one(
			kubernetes.Measure{Request: 0.2, Recommended: 0.1, Verdict: kubernetes.Over}, kubernetes.Measure{Request: gi})},
		{Namespace: "app", Workload: "crashy", Kind: "deployment", Pods: 1, Containers: one(
			kubernetes.Measure{Request: 0.1, Recommended: 0.1}, kubernetes.Measure{Request: gi, Recommended: gi, Verdict: kubernetes.Risk, Reason: "killed for memory 3 times in the window"})},
		{Namespace: "app", Workload: "bare", Kind: "deployment", Pods: 1, Containers: one(
			kubernetes.Measure{Recommended: 0.5, Verdict: kubernetes.Unset}, kubernetes.Measure{})},
		{Namespace: "kube-system", Workload: "proxy", Kind: "daemonset", Pods: 3, Managed: true, Containers: one(
			kubernetes.Measure{Request: 0.25, Recommended: 0.01, Verdict: kubernetes.Over}, kubernetes.Measure{})},
		{Namespace: "web", Workload: "fits", Kind: "deployment", Pods: 1, Containers: one(kubernetes.Measure{Request: 0.1}, kubernetes.Measure{Request: gi})},
	}}
	got, cl := Workloads(snap, 100)
	var order []string
	for _, w := range got {
		order = append(order, w.Workload+":"+w.Verdict)
	}
	want := "crashy:risk bare:unset big:over proxy:over small:over"
	if s := strings.Join(order, " "); s != want {
		t.Fatalf("order %s, want %s", s, want)
	}
	big := got[2]
	// Frees 2 cores of 10 and 8 GiB of 40: a fifth of each, so a fifth of
	// the cluster's cost.
	if !near(big.Frees.CPU, 2) || !near(big.Frees.Memory, 8*gi) || !near(big.Share, 0.2) || big.PerMonth == nil || !near(*big.PerMonth, 20) {
		t.Errorf("big %+v", big)
	}
	if b := got[1]; !near(b.Frees.CPU, -0.5) {
		t.Errorf("setting a request adds: %+v", b)
	}
	// The managed proxy counts in neither the requests nor the totals.
	if !near(cl.Requested.CPU, 1.5*2+0.2+0.1+0.1) || !near(cl.Frees.CPU, 2.1) || !near(cl.Adds.CPU, 0.5) || !near(cl.Frees.Memory, 8*gi) || *cl.PerMonth != 100 {
		t.Errorf("cluster %+v", cl)
	}
	if _, cl := Workloads(snap, 0); cl.PerMonth != nil {
		t.Errorf("an unpriced cluster has no price")
	}
}

func TestBuildAddsTheCategoriesUp(t *testing.T) {
	prevCloud := 0.0
	s := Sources{
		Power: func() *power.Snapshot {
			return &power.Snapshot{Hosts: []power.Host{{Host: "node-1", Watts: 100, CostPerHour: 0.02}, {Host: "nas", Watts: 50, CostPerHour: 0.01}}}
		},
		Workloads: func() *kubernetes.Snapshot { return &kubernetes.Snapshot{Window: "7d", Workloads: workloads()} },
		Cloud: func() *azure.Snapshot {
			return &azure.Snapshot{Costs: []azure.Cost{
				{Provider: "azure", Account: "s", Group: "rg", Service: "Storage", Currency: "USD", Period: azure.Trailing, Cost: 10},
				{Provider: "azure", Account: "s", Group: "rg", Service: "Storage", Currency: "USD", Period: azure.Current, Cost: 0.5},
				{Provider: "azure", Account: "s", Group: "rg", Service: "Storage", Currency: "USD", Period: azure.Previous, Cost: 8},
				{Provider: "azure", Account: "s", Group: "rg", Service: "Key Vault", Currency: "USD", Period: azure.Trailing, Cost: 1},
			}}
		},
		AI: func() *aispend.Snapshot {
			return &aispend.Snapshot{Accounts: []aispend.Account{{Provider: "claude", Account: "a@example.org", CostPerMonth: 200}, {Provider: "codex", Account: "b@example.org", CostPerMonth: 20}}}
		},
		ClusterHosts: []string{"node-1"},
	}
	c := Build(s, time.Now())
	_ = prevCloud
	if !near(c.Categories[0].PerMonth, 0.03*HoursPerMonth) || !near(c.Categories[1].PerMonth, 11) || !near(c.Categories[2].PerMonth, 220) {
		t.Errorf("categories = %+v", c.Categories)
	}
	if c.Categories[1].Previous == nil || *c.Categories[1].Previous != 8 || c.Categories[0].Previous != nil {
		t.Errorf("previous = %+v", c.Categories)
	}
	// The headline is the last 30 days; this month so far sits beside it.
	if c.Categories[1].MonthToDate == nil || *c.Categories[1].MonthToDate != 0.5 || c.Categories[0].MonthToDate != nil {
		t.Errorf("month to date = %+v", c.Categories)
	}
	if !near(c.TotalPerMonth, 0.03*HoursPerMonth+11+220) {
		t.Errorf("total = %v", c.TotalPerMonth)
	}
	if len(c.Hosts) != 2 || !c.Hosts[0].Cluster || c.Hosts[1].Cluster {
		t.Errorf("hosts = %+v", c.Hosts)
	}
	// Only node-1's cost (0.02 * 730) is shared among namespaces.
	var shared float64
	for _, n := range c.Namespaces {
		shared += n.PerMonth
	}
	if !near(shared, 0.02*HoursPerMonth) {
		t.Errorf("namespaces share %v, want %v", shared, 0.02*HoursPerMonth)
	}
	if len(c.Cloud) != 2 || c.Cloud[0].Service != "Storage" || c.Cloud[0].Trailing != 10 || c.Cloud[0].Current != 0.5 || c.Cloud[0].Previous != 8 || c.Cloud[1].Trailing != 1 {
		t.Errorf("cloud = %+v", c.Cloud)
	}
	if len(c.Rightsizing.Workloads) != 2 || c.Workloads != 4 || c.Window != "7d" || c.Rightsizing.Window != "7d" {
		t.Errorf("rightsizing %d, workloads %d, window %q", len(c.Rightsizing.Workloads), c.Workloads, c.Window)
	}
}

func TestBuildWithNothingOn(t *testing.T) {
	c := Build(Sources{}, time.Now())
	if c.TotalPerMonth != 0 || len(c.Categories) != 3 || c.Categories[0].On || c.Categories[2].On {
		t.Errorf("got %+v", c)
	}
}

func TestBuildCountsAgentWorkBilledByTheToken(t *testing.T) {
	plans := &aispend.Snapshot{Accounts: []aispend.Account{{Provider: "claude", Account: "user@example.com", CostPerMonth: 200}}}
	spend := &agents.Spend{Trailing: 300, MonthToDate: 120, Previous: 280}
	c := Build(Sources{AI: func() *aispend.Snapshot { return plans }, Agents: func() *agents.Spend { return spend }}, time.Now())
	ai := c.Categories[2]
	if !ai.On || !near(ai.PerMonth, 500) || ai.MonthToDate == nil || !near(*ai.MonthToDate, 120) || ai.Previous == nil || !near(*ai.Previous, 280) {
		t.Fatalf("ai is the plans plus the last 30 days of agent work: %+v", ai)
	}
	if !near(c.TotalPerMonth, 500) || c.AgentSpend == nil {
		t.Fatalf("total %v, agent spend %v", c.TotalPerMonth, c.AgentSpend)
	}

	// Work a plan pays for is not spend.
	c = Build(Sources{Agents: func() *agents.Spend { return nil }}, time.Now())
	if c.Categories[2].On || c.TotalPerMonth != 0 || c.AgentSpend != nil {
		t.Fatalf("no agent spend: %+v", c.Categories[2])
	}
}

func TestBuildTakesTheBillAsTruth(t *testing.T) {
	snap := &billed.Snapshot{Months: []billed.Month{
		{Period: "2026-08", AI: 37837.36, Usage: 93731.16},
		{Period: "2026-09", AI: 29285.89, Usage: 84633.24, Programs: []billed.Program{{Program: "Alpha", AI: 20000}}},
	}}
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	spend := &agents.Spend{Trailing: 8391, MonthToDate: 3060, Previous: 9466}
	src := Sources{Billed: func() *billed.Snapshot { return snap }, Agents: func() *agents.Spend { return spend }, Zone: chicago}

	c := Build(src, time.Date(2026, 11, 4, 12, 0, 0, 0, chicago))
	cloud, ai := c.Categories[1], c.Categories[2]
	if !ai.On || !near(ai.PerMonth, 29285.89) || ai.Label != "September 2026, billed" || ai.MonthToDate != nil || ai.Previous != nil {
		t.Fatalf("ai is the newest billed month; October and November are not billed yet: %+v", ai)
	}
	if !cloud.On || !near(cloud.PerMonth, 84633.24-29285.89) {
		t.Fatalf("cloud is the bill's usage less its AI: %+v", cloud)
	}
	if !near(c.TotalPerMonth, 84633.24) || c.AgentSpend == nil {
		t.Fatalf("collected agent work stays beside the bill, never added: total %v", c.TotalPerMonth)
	}
	if len(c.Headline) != 2 || c.Headline[0] != (Figure{Label: "September 2026", Amount: 84633.24, Note: "billed"}) || c.Headline[1].Label != "August 2026" {
		t.Fatalf("headline: the newest billed months, newest first: %+v", c.Headline)
	}
	if len(c.Billed) != 2 || len(c.Billed[1].Programs) != 1 {
		t.Fatalf("billed months %+v", c.Billed)
	}

	// Mid-October, September is last month.
	c = Build(src, time.Date(2026, 10, 15, 12, 0, 0, 0, chicago))
	if p := c.Categories[2].Previous; p == nil || !near(*p, 29285.89) {
		t.Fatalf("last month billed: %v", p)
	}
}

func TestHeadlineWithoutABill(t *testing.T) {
	now, prev := 3.0, 9.0
	months := []Category{{Category: AI, On: true, PerMonth: 8, MonthToDate: &now, Previous: &prev}, {Category: Cloud}}
	if h := headline(months, 8); len(h) != 2 || h[0] != (Figure{Label: "This month so far", Amount: 3}) || h[1] != (Figure{Label: "Last month", Amount: 9}) {
		t.Fatalf("every category on knows its months: %+v", h)
	}
	rate := append(months, Category{Category: Power, On: true, PerMonth: 50})
	if h := headline(rate, 58); len(h) != 1 || h[0].Label != "Estate per month" || h[0].Amount != 58 {
		t.Fatalf("a category without months makes it the rate: %+v", h)
	}
}

func TestBuildListsEverySourcesSizing(t *testing.T) {
	ms := &machines.Snapshot{Machines: []machines.Machine{
		{Host: "vm-a", CPU: machines.Fit(8, 0.8, 0, 0.3, machines.Cores), Memory: machines.FitMemory(16, 12, 15, 0, 0.3)},
		{Host: "vm-b", CPU: machines.Fit(2, 1.5, 0, 0.3, machines.Cores), Memory: machines.FitMemory(8, 5, 5, 0, 0.3)},
	}}
	adv := &advisor.Snapshot{Advice: []advisor.Advice{
		{VM: "vm-x", ResourceGroup: "rg-app", Kind: advisor.Resize, From: "D4", To: "E2", CPUP95: 29, MemoryP95: 61, SavingsPerYear: 1500, Change: advisor.Family},
		{VM: "vm-y", ResourceGroup: "rg-lab", Kind: advisor.Shutdown, From: "B2", SavingsPerYear: 360, Change: advisor.Off, Counted: true},
	}}
	c := Build(Sources{
		Workloads: func() *kubernetes.Snapshot { return &kubernetes.Snapshot{Workloads: workloads(), Window: "7d"} },
		Machines:  func() *machines.Snapshot { return ms },
		Advisor:   func() *advisor.Snapshot { return adv },
	}, time.Now())
	rs := c.Rightsizing
	if len(rs.Workloads) != 2 || rs.Workloads[0].Workload != "postgres" || rs.Workloads[1].Workload != "portal" {
		t.Fatalf("workloads %+v", rs.Workloads)
	}
	var got []string
	for _, r := range append(rs.Machines, rs.Advisor...) {
		got = append(got, r.Source+":"+r.Workload+":"+r.Resource+":"+r.Verdict)
	}
	want := "machines:vm-a:cpu:over machines:vm-a:memory:under advisor:vm-x:size:over advisor:vm-y:size:shutdown"
	if s := strings.Join(got, " "); s != want {
		t.Fatalf("got %s, want %s", s, want)
	}
	if r := rs.Machines[0]; r.Unit != machines.Cores || r.Request != 8 || r.Recommended != 2 {
		t.Fatalf("machine cpu %+v", r)
	}
	if r := rs.Machines[1]; r.Peak != 15 || r.P95 != 12 {
		t.Fatalf("machine memory is sized to its peak: %+v", r)
	}
	if r := rs.Advisor[1]; r.Change != advisor.Off || r.Decide {
		t.Fatalf("a counted change needs no decision: %+v", r)
	}
	if r := rs.Advisor[0]; r.Namespace != "rg-app" || r.From != "D4" || r.To != "E2" || r.SavingsPerYear != 1500 || r.MemoryP95 != 61 || r.Change != advisor.Family || !r.Decide {
		t.Fatalf("advice %+v", r)
	}
}
