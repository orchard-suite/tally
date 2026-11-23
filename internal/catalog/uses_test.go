package catalog

import (
	"testing"
	"time"

	"github.com/orchard-suite/tally/internal/agents"
)

func TestParseTargets(t *testing.T) {
	ts, err := ParseTargets("agent_plugin=claude-code/{plugin}, agent_skill=claude-code/{plugin}/{skill}")
	if err != nil || len(ts) != 2 || ts[0].Skill() || !ts[1].Skill() {
		t.Fatalf("%+v %v", ts, err)
	}
	if ts[1].KeyOf("orrery:orrery-model") != "claude-code/orrery/orrery-model" || ts[0].KeyOf("orrery") != "claude-code/orrery" {
		t.Errorf("keys: %q %q", ts[1].KeyOf("orrery:orrery-model"), ts[0].KeyOf("orrery"))
	}
	if _, err := ParseTargets("agent_skill=claude-code/x"); err == nil {
		t.Error("a template without {plugin} is refused")
	}
}

func TestUsesEntitiesWriteEveryExistingEntityAndZeroForUnused(t *testing.T) {
	targets, _ := ParseTargets("agent_plugin=claude-code/{plugin},agent_skill=claude-code/{plugin}/{skill}")
	rows := []agents.UseRow{
		{Machine: "a", Use: agents.Use{Kind: agents.UseSkill, Name: "orrery:orrery-model", Count: 2}},
		{Machine: "b", Use: agents.Use{Kind: agents.UseTool, Name: "mcp__plugin_orrery_orrery__get_entity", Count: 3}},
		{Machine: "b", Use: agents.Use{Kind: agents.UseSkill, Name: "elsewhere:thing", Count: 7}},
	}
	existing := map[string][]string{
		"agent_plugin": {"claude-code/orrery", "claude-code/vantage", "copilot/t_x"},
		"agent_skill":  {"claude-code/orrery/orrery-model", "claude-code/orrery/orrery-catalog"},
	}
	got := map[string]map[string]any{}
	installed := map[string]int64{"orrery": 3, "orr": 9, "elsewhere": 1}
	for _, e := range UsesEntities(targets, rows, installed, existing, time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)) {
		got[e.Blueprint+"/"+e.Key] = e.Fields
	}
	if len(got) != 4 {
		t.Fatalf("one write per entity of the target's shape the hub has, and none for one it lacks: %v", got)
	}
	if _, ok := got["agent_plugin/copilot/t_x"]; ok {
		t.Error("an entity of another shape is not one tally counts, so it is left alone")
	}
	check := func(id string, uses, computers int64) {
		t.Helper()
		f := got[id]
		if f[UsesField] != uses || f[ComputersField] != computers || f[UsedField] != "2026-11-15T12:00:00Z" {
			t.Errorf("%s: %v, want %d uses on %d computers", id, f, uses, computers)
		}
	}
	// A plugin's installs are its own, and each of its skills takes them; a plugin whose name another starts with is not it.
	for id, want := range map[string]int64{"agent_plugin/claude-code/orrery": 3, "agent_plugin/claude-code/vantage": 0, "agent_skill/claude-code/orrery/orrery-model": 3} {
		if got[id][InstalledField] != want {
			t.Errorf("%s installed on %v, want %d", id, got[id][InstalledField], want)
		}
	}
	check("agent_plugin/claude-code/orrery", 5, 2)
	check("agent_plugin/claude-code/vantage", 0, 0)
	check("agent_skill/claude-code/orrery/orrery-model", 2, 1)
	check("agent_skill/claude-code/orrery/orrery-catalog", 0, 0)
}

func TestUsesBlueprintsAreOffersTheHubTakes(t *testing.T) {
	targets, _ := ParseTargets("agent_plugin=claude-code/{plugin},agent_skill=claude-code/{plugin}/{skill}")
	for _, b := range UsesBlueprints(targets) {
		if id, _ := b["id"].(string); id == "" {
			t.Errorf("an offer names its id: %v", b)
		}
		if title, _ := b["title"].(string); title == "" {
			t.Errorf("an offer names a title, or the hub refuses it: %v", b["id"])
		}
		fields, _ := b["fields"].(map[string]any)
		if len(fields) != 4 {
			t.Errorf("%v offers the usage fields and the installs: %v", b["id"], fields)
		}
	}
}
