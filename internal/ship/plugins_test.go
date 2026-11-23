package ship

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/orchard-suite/tally/internal/agents"
)

const installedFile = `{"version":2,"plugins":{
 "orrery@estate":[{"scope":"user","installPath":"/Users/someone/.claude/plugins/cache/estate/orrery/1.2.0","version":"1.2.0"},{"scope":"project","version":"1.3.0"}],
 "vantage@estate":[{"scope":"user","version":"2.0.0"}],
 "gone@estate":[]}}`

func TestReadInstalledReadsNamesMarketplacesAndVersionsOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installed_plugins.json")
	if err := os.WriteFile(path, []byte(installedFile), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadInstalled(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []agents.InstalledPlugin{{Name: "orrery", Marketplace: "estate", Version: "1.3.0"}, {Name: "vantage", Marketplace: "estate", Version: "2.0.0"}}
	if len(got.Plugins) != 2 || got.Plugins[0] != want[0] || got.Plugins[1] != want[1] {
		t.Fatalf("one plugin a machine, its newest version, and none with no install: %+v", got.Plugins)
	}
	none, err := ReadInstalled(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || none.Plugins == nil || len(none.Plugins) != 0 {
		t.Fatalf("a machine with no record has no plugins, and says so: %+v %v", none, err)
	}
}

func TestAPlaceKeptUnderEarlierRulesIsReadAgain(t *testing.T) {
	dir := t.TempDir()
	projects := filepath.Join(dir, "projects")
	recent, old := filepath.Join(projects, "p", "recent.jsonl"), filepath.Join(projects, "p", "old.jsonl")
	if err := os.MkdirAll(filepath.Dir(recent), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{recent, old} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	long := time.Now().Add(-2 * Reread)
	if err := os.Chtimes(old, long, long); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "ship.json")
	// A place with no version: kept before a plugin's use left a route.
	if err := os.WriteFile(state, []byte(`{"files":{"`+recent+`":{"offset":3},"`+old+`":{"offset":3},"/elsewhere/ion.jsonl":{"size":9}},"known":{},"sent":{},"active":{"claude:a":{"seen":[],"buckets":[],"uses":[{"Model":"m","Kind":"skill","Name":"orrery:orrery-model","Minute":60,"count":2}],"last":"2026-11-15T10:00:00Z"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{URL: "http://tally.invalid", Token: "secret", Claude: projects, State: state, Every: time.Minute}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := s.files[recent]; kept {
		t.Error("a transcript written to lately is read again from its start")
	}
	if s.files[old].Offset != 3 || s.files["/elsewhere/ion.jsonl"].Size != 9 {
		t.Errorf("an old transcript and another agent's file keep their place: %+v", s.files)
	}
	if len(s.convs["claude:a"].DirtyUses) != 1 {
		t.Error("an active conversation posts its uses again")
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	again, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if len(again.convs["claude:a"].DirtyUses) != 0 {
		t.Error("a place kept under these rules is read on from where it was")
	}
	_ = context.Background
}
