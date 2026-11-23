package machines

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/orchard-suite/tally/internal/collect/promapi"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestShape(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "samples.json"))
	if err != nil {
		t.Fatal(err)
	}
	var in map[string][]promapi.Sample
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	snap := Shape(Answers{MemTotal: in["mem_total"], MemP95: in["mem_p95"], MemPeak: in["mem_peak"], CPUP95: in["cpu_p95"], Cores: in["cores"], Requested: in["requested"]},
		map[string]bool{"host-physical": true}, 0.3, "7d", time.Now())
	if len(snap.Machines) != 4 {
		t.Fatalf("a skipped host is left out: %+v", snap.Machines)
	}
	by := map[string]Machine{}
	for _, m := range snap.Machines {
		by[m.Host] = m
	}

	big := by["vm-big"]
	// 8 cores at 10% busy is 0.8 cores; 0.8 plus 30% rounds up to 2 cores.
	if big.CPU.Unit != Cores || big.CPU.Verdict != Over || !near(big.CPU.P95, 0.8) || big.CPU.Recommended != 2 {
		t.Errorf("vm-big cpu %+v", big.CPU)
	}
	// 16 GiB with 2 GiB used is over; 2 GiB plus 30% fits.
	if big.Memory.Verdict != Over || !near(big.Memory.Recommended, 2147483648*1.3) {
		t.Errorf("vm-big memory %+v", big.Memory)
	}

	tight := by["vm-tight"]
	// No core count: CPU is judged in percent, which names no size; 95%
	// busy is under.
	if tight.CPU.Unit != Percent || tight.CPU.Size != 100 || tight.CPU.Verdict != Under || tight.CPU.Recommended != 0 {
		t.Errorf("vm-tight cpu %+v", tight.CPU)
	}
	if tight.Memory.Verdict != Under {
		t.Errorf("vm-tight memory uses 95%%, so it is under: %+v", tight.Memory)
	}

	fit := by["vm-fit"]
	if fit.CPU.Verdict != "" || fit.Memory.Verdict != "" {
		t.Errorf("vm-fit fits: %+v", fit)
	}

	// A Kubernetes node uses 1.2 of 12 cores and peaks at 4 GiB of 16, but
	// its pods ask for 5.2 cores and 7 GiB and it holds back 0.5 and 1 GiB:
	// the size that fits holds them.
	node := by["node-1"]
	if node.CPU.Verdict != Over || node.CPU.Requested != 5.7 || node.CPU.Recommended != 6 {
		t.Errorf("node-1 cpu %+v", node.CPU)
	}
	if node.Memory.Verdict != Over || node.Memory.Peak != 4294967296 || node.Memory.Recommended != 8589934592 {
		t.Errorf("node-1 memory %+v", node.Memory)
	}
}

// Memory is judged by its peak: a p95 well under the size must not hide a
// peak near it.
func TestMemoryFitsItsPeak(t *testing.T) {
	m := FitMemory(8, 2, 7.5, 0, 0.3)
	if m.Verdict != Under || m.P95 != 2 || m.Peak != 7.5 {
		t.Fatalf("%+v", m)
	}
}

func TestFitNeverRecommendsGrowingAnOverSize(t *testing.T) {
	// One core used at 40%: more than twice the use, but one core is the
	// least, so it is not over.
	if m := Fit(1, 0.4, 0, 0.3, Cores); m.Verdict != "" || m.Recommended != 1 {
		t.Fatalf("%+v", m)
	}
	if m := Fit(4, 0, 0, 0.3, Cores); m.Verdict != "" {
		t.Fatalf("no reading is no verdict: %+v", m)
	}
}
