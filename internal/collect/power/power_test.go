package power

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/orchard-suite/tally/internal/collect/promapi"
)

func state(t *testing.T, name string) State {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWattsReadsWAndKW(t *testing.T) {
	if w, err := Watts(state(t, "plug")); err != nil || w != 142.6 {
		t.Errorf("plug = %v, %v", w, err)
	}
	if w, err := Watts(state(t, "ups_kw")); err != nil || w != 380 {
		t.Errorf("ups = %v, %v", w, err)
	}
	if _, err := Watts(state(t, "unavailable")); err == nil {
		t.Error("unavailable read as a number")
	}
}

func TestMappingsAndShape(t *testing.T) {
	mappings, err := ParseMappings("sensor.rack_plug_power=node-1, sensor.ups_load=node-1,sensor.nas_power=nas")
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 3 || mappings[1].Host != "node-1" || mappings[2].Entity != "sensor.nas_power" {
		t.Fatalf("mappings = %+v", mappings)
	}
	hosts := Shape(mappings, map[string]float64{"sensor.rack_plug_power": 142.6, "sensor.ups_load": 380}, nil, 0.25)
	if len(hosts) != 1 {
		t.Fatalf("a host with no reading must be left out: %+v", hosts)
	}
	if hosts[0].Host != "node-1" || hosts[0].Watts != 522.6 {
		t.Errorf("node-1 = %+v", hosts[0])
	}
	if diff := hosts[0].CostPerHour - 0.13065; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost per hour = %v, want 0.13065", hosts[0].CostPerHour)
	}
	if _, err := ParseMappings("sensor.x"); err == nil {
		t.Error("a mapping without a host passed")
	}
	if _, err := ParseMappings(""); err == nil {
		t.Error("an empty list passed")
	}
}

func TestMeteredHostsFromPrometheus(t *testing.T) {
	metered := Metered([]promapi.Sample{
		{Labels: map[string]string{"host": "node-1", "instance": "192.0.2.2:9290"}, Value: 158},
		{Labels: map[string]string{"host": "node-2", "instance": "192.0.2.5:9290"}, Value: 151},
		{Labels: map[string]string{"host": "node-2"}, Value: 9},
		{Labels: map[string]string{"instance": "nowhere"}, Value: 999},
	})
	if len(metered) != 2 || metered["node-1"] != 158 || metered["node-2"] != 160 {
		t.Errorf("metered = %v", metered)
	}
	hosts := Shape(nil, nil, metered, 0.15)
	if len(hosts) != 2 || hosts[0].Host != "node-1" || hosts[0].Watts != 158 || hosts[1].Watts != 160 {
		t.Errorf("hosts = %+v", hosts)
	}
	if diff := hosts[0].CostPerHour - 0.0237; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost per hour = %v, want 0.0237", hosts[0].CostPerHour)
	}
	mixed := Shape([]Mapping{{Entity: "sensor.nas_power", Host: "nas"}}, map[string]float64{"sensor.nas_power": 40}, metered, 0.15)
	if len(mixed) != 3 || mixed[0].Host != "nas" {
		t.Errorf("mixed = %+v", mixed)
	}
}
