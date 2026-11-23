package billed

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/orrery"
)

func load(t *testing.T, name string) []orrery.Record {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var out []orrery.Record
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestShape(t *testing.T) {
	snap, err := Shape(load(t, "periods.json"), load(t, "programs.json"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Months) != 2 || snap.Months[0].Period != "2026-08" || snap.Latest().Period != "2026-09" {
		t.Fatalf("months oldest first: %+v", snap.Months)
	}
	sep := snap.Month("2026-09")
	if sep == nil || !near(sep.AI, 29285.89) || !near(sep.Usage, 84633.24) || sep.Invoice != "reconciled" {
		t.Fatalf("september %+v", sep)
	}
	if len(sep.Programs) != 2 || sep.Programs[0] != (Program{Program: "Beta", AI: 18202.89}) || sep.Programs[1] != (Program{Program: "Alpha", AI: 11083}) {
		t.Fatalf("programs largest first, none without AI: %+v", sep.Programs)
	}
	if sep.AIModels == nil || !near(*sep.AIModels, 27000) {
		t.Fatalf("september's model part %v", sep.AIModels)
	}
	if snap.Month("2026-08").AIModels != nil {
		t.Fatal("a month the pipeline wrote no model part for has none")
	}
	if snap.Month("2026-10") != nil {
		t.Fatal("a month the pipeline has not run is absent")
	}
}

func TestShapeRefusesAPeriodWithoutAI(t *testing.T) {
	periods := load(t, "periods.json")
	delete(periods[0].Fields, "ai_cents")
	if _, err := Shape(periods, nil, time.Now()); err == nil {
		t.Fatal("a period with no AI figure fails the round, never reads as zero")
	}
}
