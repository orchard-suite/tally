package estate

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"testing"

	"github.com/orchard-suite/tally/internal/agents"
	"github.com/orchard-suite/tally/internal/collect/billed"
	"github.com/orchard-suite/tally/internal/collect/gateway"
)

func fixture(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

var reconPrices = agents.Prices{
	"claude-opus-5-5": {Input: 5e-6, Output: 25e-6},
	"gpt-5.6-terra":   {Input: 1e-6, Output: 8e-6},
}

func lenses(t *testing.T) (*billed.Snapshot, *gateway.Snapshot, []agents.MonthModel) {
	var b billed.Snapshot
	var g gateway.Snapshot
	var s []agents.MonthModel
	fixture(t, "billed.json", &b)
	fixture(t, "gateway.json", &g)
	fixture(t, "shipped.json", &s)
	return &b, &g, s
}

func about(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestReconcile(t *testing.T) {
	b, g, s := lenses(t)
	r := Reconcile(b, g, s, reconPrices.Table())
	var periods []string
	for _, m := range r.Months {
		periods = append(periods, m.Period)
	}
	// July is only the shippers': the gateway did not read it.
	if !reflect.DeepEqual(periods, []string{"2026-08", "2026-09", "2026-10"}) {
		t.Fatalf("months %v", periods)
	}

	aug := r.Months[0]
	if aug.Gateway.Tokens != 5000000 || aug.Gateway.Unpriced != 5000000 || aug.Gateway.Cost != 0 {
		t.Errorf("tokens from before the model tag are counted, not priced: %+v", aug.Gateway)
	}
	if aug.BasisIsModels || *aug.Basis != 50 || *aug.Gap != 50 {
		t.Errorf("without a model part the basis is all of AI: %+v", aug)
	}

	sep := r.Months[1]
	if !about(sep.Gateway.Cost, 12.08) || sep.Gateway.Tokens != 3610000 {
		t.Errorf("gateway %+v", sep.Gateway)
	}
	if !about(sep.Shipped.Cost, 11.5) || sep.Shipped.Tokens != 4300000 {
		t.Errorf("shipped %+v", sep.Shipped)
	}
	if !sep.BasisIsModels || *sep.Basis != 80 || *sep.Billed != 100 || !about(*sep.Gap, 80-12.08) || sep.Invoice != "pending" {
		t.Errorf("the gateway against the model part: %+v", sep)
	}
	if !about(sep.Coverage, 3310000.0/3610000) {
		t.Errorf("coverage counts at most what the gateway measured, per model: %v", sep.Coverage)
	}
	if !reflect.DeepEqual(sep.Over, []string{"gpt-5.6-terra"}) {
		t.Errorf("shipping more than the gateway measured is a finding: %v", sep.Over)
	}
	if sep.Models[0].Model != "claude-opus-5-5" || !about(sep.Models[0].GatewayCost, 10) || !about(sep.Models[0].ShippedCost, 8.5) {
		t.Errorf("models costliest first: %+v", sep.Models)
	}

	oct := r.Months[2]
	if oct.Billed != nil || oct.Gap != nil || oct.Gateway.Unpriced != 110 {
		t.Errorf("a month not billed has no gap; a model with no price is unpriced: %+v", oct)
	}
}

func TestGatewayKeysShareTheBasisByCost(t *testing.T) {
	_, g, _ := lenses(t)
	basis := 80.0
	keys := GatewayKeys(g, "2026-09", reconPrices.Table(), &basis)
	if len(keys) != 3 || keys[0].Key != "dev--ada--max" || keys[1].Key != "dev--bob--pro" || keys[2].Key != "app--1--team--svc--job" {
		t.Fatalf("keys costliest first: %+v", keys)
	}
	var sum float64
	for _, k := range keys {
		sum += *k.Billed
	}
	if !about(sum, 80) || !about(*keys[0].Billed, 80*7.5/12.08) {
		t.Errorf("shares add up to the basis: %v %v", sum, *keys[0].Billed)
	}
	for _, k := range GatewayKeys(g, "2026-10", reconPrices.Table(), nil) {
		if k.Billed != nil || k.Unpriced != 110 {
			t.Errorf("no bill, no share: %+v", k)
		}
	}
	if len(GatewayKeys(nil, "2026-09", reconPrices.Table(), &basis)) != 0 {
		t.Error("no gateway, no keys")
	}
}
