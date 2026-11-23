package estate

import (
	"testing"
	"time"

	"github.com/orchard-suite/tally/internal/collect/actions"
	"github.com/orchard-suite/tally/internal/collect/runners"
)

var ciNow = time.Date(2026, 11, 8, 15, 0, 0, 0, time.UTC)

func day(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }

func sharedPool() *runners.Snapshot {
	return &runners.Snapshot{Window: "7d", Hours: 168,
		Pool: runners.Pool{Namespace: "arc-runners", CPUShare: 0.1, MemoryShare: 0.3},
		Usage: []runners.Usage{
			{Forge: "github", Repository: "example/web", PodSeconds: 600, CPUSeconds: 300, MemorySeconds: 100},
			{Forge: "github", Repository: "example/api", PodSeconds: 1200, CPUSeconds: 100, MemorySeconds: 300},
		}}
}

func TestSharedPoolIsItsNamespacesShareOfTheCluster(t *testing.T) {
	ci := BuildCI(sharedPool(), nil, 100, true, ciNow)
	if ci.Pool.PerMonth == nil || !near(*ci.Pool.PerMonth, 20) {
		t.Fatalf("pool = %+v", ci.Pool)
	}
	by := map[string]CIRepository{}
	for _, r := range ci.Repositories {
		by[r.Repository] = r
	}
	// web: CPU 300 of 400, memory 100 of 400: half of 0.75 and half of 0.25.
	if !near(*by["example/web"].SelfHostedPerMonth, 10) || !near(*by["example/api"].SelfHostedPerMonth, 10) {
		t.Errorf("repos = %+v", by)
	}
	if !near(by["example/api"].SelfHostedMinutes, 20) {
		t.Errorf("minutes = %+v", by["example/api"])
	}
	if ci.Hosted {
		t.Error("no bill is read")
	}
}

func TestSharedPoolWithoutPowerIsUnpriced(t *testing.T) {
	ci := BuildCI(sharedPool(), nil, 0, false, ciNow)
	if ci.Pool.PerMonth != nil {
		t.Errorf("a pool whose cluster's cost is unknown has no price, not a zero: %+v", ci.Pool)
	}
	for _, r := range ci.Repositories {
		if r.SelfHostedPerMonth != nil || r.SelfHostedMinutes == 0 {
			t.Errorf("minutes still count, money does not: %+v", r)
		}
	}
}

func TestPoolOnItsOwnNodesIsItsNodeHours(t *testing.T) {
	snap := &runners.Snapshot{Window: "7d", Hours: 168,
		Pool:  runners.Pool{Namespace: "gitlab", Nodes: "aks-ci-.*", NodeHours: 84, PricePerHour: 0.5},
		Usage: []runners.Usage{{Forge: "gitlab", Repository: "group/web", PodSeconds: 60, CPUSeconds: 30, MemorySeconds: 30}}}
	ci := BuildCI(snap, nil, 0, false, ciNow)
	// 84 node hours in 168 is half a node, held for 730 hours.
	if !near(*ci.Pool.PerMonth, 84*0.5*730/168) || !near(*ci.Repositories[0].SelfHostedPerMonth, *ci.Pool.PerMonth) {
		t.Errorf("ci = %+v", ci)
	}
}

func bill() *actions.Snapshot {
	return &actions.Snapshot{Items: []actions.Item{
		{Account: "example", Repository: "example/web", SKU: "Actions Linux", Date: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), Minutes: 100, Gross: 0.6, Net: 0},
		{Account: "example", Repository: "Example/Web", SKU: "Actions Linux", Date: day(3), Minutes: 50, Gross: 0.3, Net: 0.3},
		{Account: "example", Repository: "example/app", SKU: "Actions macOS 3-core", Date: day(7), Minutes: 10, Gross: 0.7, Net: 0.7},
		{Account: "example", Repository: "example/app", SKU: "Actions macOS 3-core", Date: day(8), Minutes: 99, Gross: 9, Net: 9},
		{Account: "example", Repository: "example/web", SKU: "Actions Linux", Date: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), Net: 2},
	}}
}

func TestHostedRepositoriesJoinTheSelfHostedOnes(t *testing.T) {
	ci := BuildCI(sharedPool(), bill(), 100, true, ciNow)
	if !ci.Hosted {
		t.Fatal("the bill is read")
	}
	by := map[string]CIRepository{}
	for _, r := range ci.Repositories {
		by[r.Repository] = r
	}
	web := by["example/web"]
	// October 31 is outside the seven whole days before the 8th; the 3rd is in.
	// Its name's case does not split it from the self-hosted ones.
	if web.HostedMinutes != 50 || web.SelfHostedMinutes != 10 || len(ci.Repositories) != 3 {
		t.Errorf("repos = %+v", ci.Repositories)
	}
	if !near(web.HostedPerMonth, 0.3*730/168) || !near(web.HostedGrossPerMonth, 0.3*730/168) {
		t.Errorf("web = %+v", web)
	}
	// Today is not a whole day yet.
	if app := by["example/app"]; app.HostedMinutes != 10 || app.SelfHostedPerMonth != nil {
		t.Errorf("app = %+v", app)
	}
	if ci.Repositories[0].Repository != "example/web" {
		t.Errorf("costliest first: %+v", ci.Repositories)
	}
}

func TestHostedMonths(t *testing.T) {
	trailing, current, previous := HostedMonths(bill(), ciNow)
	if !near(current, 10) || !near(previous, 2) {
		t.Errorf("current = %v, previous = %v", current, previous)
	}
	if !near(trailing, (2+0.3+0.7)*730/720) {
		t.Errorf("trailing = %v", trailing)
	}
}

func TestHostedBillIsACategoryAndJoinsTheTotal(t *testing.T) {
	c := Build(Sources{Actions: func() *actions.Snapshot { return bill() }}, ciNow)
	var got *Category
	for i := range c.Categories {
		if c.Categories[i].Category == Builds {
			got = &c.Categories[i]
		}
	}
	if got == nil || !got.On || !near(*got.MonthToDate, 10) || !near(c.TotalPerMonth, got.PerMonth) {
		t.Errorf("categories = %+v, total = %v", c.Categories, c.TotalPerMonth)
	}
	if c.CI == nil || c.CI.Pool != nil {
		t.Errorf("ci = %+v", c.CI)
	}
	none := Build(Sources{}, ciNow)
	if none.CI != nil || len(none.Categories) != 3 {
		t.Errorf("with neither source the view and category are absent: %+v", none)
	}
}
