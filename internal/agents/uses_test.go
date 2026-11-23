package agents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const usesBatch = `{"uses":[
 {"conversation":"claude:a","model":"m","minute":"2026-11-04T16:00:00Z","kind":"skill","name":"orrery:orrery-model","count":2},
 {"conversation":"claude:a","model":"m","minute":"2026-11-04T16:00:00Z","kind":"command","name":"orrery:orrery-catalog","count":1},
 {"conversation":"claude:a","model":"m","minute":"2026-11-04T16:01:00Z","kind":"tool","name":"mcp__plugin_orrery_orrery__search_entities","count":3},
 {"conversation":"claude:b","model":"m","minute":"2026-11-04T16:02:00Z","kind":"tool","name":"Bash","count":4}
]}`

func TestIngestStoresUsesOnceAndWithdrawTakesThem(t *testing.T) {
	c := testCollector(t, "secret")
	for range 2 {
		if code := post(c, "Bearer secret", usesBatch); code != http.StatusNoContent {
			t.Fatalf("batch: %d", code)
		}
	}
	from := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	sums, err := c.store.UseSums(context.Background(), from, from.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 4 || sums[0].Name != "Bash" || sums[0].Count != 4 || sums[0].Plugin != "" {
		t.Fatalf("a batch sent twice stores once, most used first: %+v", sums)
	}
	for _, s := range sums[1:] {
		if s.Plugin != "orrery" || s.Machines != 1 || s.Conversations != 1 {
			t.Errorf("%+v", s)
		}
	}
	rows, err := c.store.UseRows(context.Background(), from, from.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if p := Plugins(rows)["orrery"]; p.Count != 6 || p.Machines != 1 {
		t.Errorf("a plugin's reach is every use of its parts: %+v", p)
	}
	skills := Skills(rows)
	if len(skills) != 2 || skills["orrery:orrery-model"].Count != 2 || skills["orrery:orrery-catalog"].Count != 1 {
		t.Errorf("a skill is loaded or typed, never a tool: %+v", skills)
	}
	req := httptest.NewRequest(http.MethodDelete, "/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	c.Withdraw(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("withdraw: %d", rec.Code)
	}
	if sums, _ := c.store.UseSums(context.Background(), from, from.AddDate(0, 0, 1)); len(sums) != 0 {
		t.Errorf("a withdraw takes the machine's uses too: %+v", sums)
	}
}

func TestReachCountsAMachineOnce(t *testing.T) {
	rows := []UseRow{
		{Machine: "a", Use: Use{Kind: UseSkill, Name: "p:s", Count: 2}},
		{Machine: "a", Use: Use{Kind: UseCommand, Name: "p:s", Count: 1}},
		{Machine: "b", Use: Use{Kind: UseTool, Name: "mcp__plugin_p_srv__t", Count: 5}},
		{Machine: "b", Use: Use{Kind: UseSkill, Name: "report", Count: 9}},
	}
	if r := Plugins(rows)["p"]; r.Count != 8 || r.Machines != 2 {
		t.Errorf("plugin p: %+v", r)
	}
	if r := Skills(rows)["p:s"]; r.Count != 3 || r.Machines != 1 {
		t.Errorf("skill p:s, loaded and typed on one machine: %+v", r)
	}
	if _, ok := Skills(rows)["report"]; ok {
		t.Error("a skill of no plugin has no catalog entity to reach")
	}
}

func TestUsePlugin(t *testing.T) {
	for _, tc := range []struct {
		u    Use
		want string
	}{
		{Use{Kind: UseSkill, Name: "beacon:consult"}, "beacon"},
		{Use{Kind: UseCommand, Name: "compact"}, ""},
		{Use{Kind: UseTool, Name: "mcp__plugin_vantage_vantage__vantage_save"}, "vantage"},
		{Use{Kind: UseTool, Name: "mcp__acme__tasks_due"}, ""},
		{Use{Kind: UseTool, Name: "Read"}, ""},
	} {
		if got := tc.u.Plugin(); got != tc.want {
			t.Errorf("%s %s: %q, want %q", tc.u.Kind, tc.u.Name, got, tc.want)
		}
	}
}

func TestInstalledIsAMachinesWholeListAndCountsMachinesThatStillShip(t *testing.T) {
	c := testCollector(t, "secret")
	ctx := context.Background()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	say := func(machine string, at time.Time, names ...string) {
		t.Helper()
		in := Installed{Plugins: []InstalledPlugin{}}
		for _, n := range names {
			in.Plugins = append(in.Plugins, InstalledPlugin{Name: n, Marketplace: "estate", Version: "1.0.0"})
		}
		b := Batch{Installed: &in}
		if err := b.Validate(); err != nil {
			t.Fatalf("a batch of only the installed plugins is one tally takes: %v", err)
		}
		if err := c.store.Apply(ctx, machine, b, at); err != nil {
			t.Fatal(err)
		}
	}
	say("a", now, "orrery", "vantage")
	say("b", now, "orrery")
	say("gone", now.AddDate(0, -3, 0), "orrery")
	got, err := c.store.InstalledOn(ctx, now.AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	if got["orrery"] != 2 || got["vantage"] != 1 {
		t.Fatalf("each plugin by the machines that still ship: %v", got)
	}
	// A machine that says its list again is believed whole: what it left out is gone.
	say("a", now)
	if got, _ := c.store.InstalledOn(ctx, now.AddDate(0, 0, -30)); got["orrery"] != 1 || got["vantage"] != 0 {
		t.Fatalf("a list replaces the one before: %v", got)
	}
	if err := (Batch{}).Validate(); err == nil {
		t.Error("a batch that says nothing is refused")
	}
}
