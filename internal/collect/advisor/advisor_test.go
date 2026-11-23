package advisor

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func read(t *testing.T, names ...string) [][]byte {
	t.Helper()
	var pages [][]byte
	for _, f := range names {
		raw, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, raw)
	}
	return pages
}

func TestShapeKeepsOpenAdviceOnly(t *testing.T) {
	counted, err := Counted("")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := Shape(read(t, "page1.json", "page2.json"), read(t, "suppressions.json"), counted, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var vms []string
	for _, a := range snap.Advice {
		vms = append(vms, a.VM)
	}
	want := []string{"vm-db-03", "vm-app-01", "vm-fs-04", "vm-idle-02"}
	if len(vms) != len(want) {
		t.Fatalf("closed, dismissed, and suppressed advice are left out, the largest saving first: got %v, want %v", vms, want)
	}
	for i := range want {
		if vms[i] != want[i] {
			t.Fatalf("got %v, want %v", vms, want)
		}
	}
	db := snap.Advice[0]
	if db.Kind != Resize || db.From != "Standard_E8s_v5" || db.To != "Standard_E4s_v5" || db.CPUP95 != 18 || db.MemoryP95 != 38 || db.SavingsPerYear != 4800 || db.Currency != "USD" || db.ResourceGroup != "rg-data" {
		t.Fatalf("resize advice %+v", db)
	}
	if idle := snap.Advice[3]; idle.Kind != Shutdown || idle.To != "" || idle.SavingsPerYear != 360 {
		t.Fatalf("a shutdown advises no size: %+v", idle)
	}
}

func TestShapeNamesAndCountsEachChange(t *testing.T) {
	counted, err := Counted("shrink, shutdown")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := Shape(read(t, "page1.json", "page2.json"), nil, counted, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		change  string
		counted bool
	}{
		"vm-db-03":   {Shrink, true},
		"vm-db-07":   {Shrink, true},
		"vm-app-01":  {Family, false},
		"vm-fs-04":   {Burstable, false},
		"vm-idle-02": {Off, true},
	}
	for _, a := range snap.Advice {
		w, ok := want[a.VM]
		if !ok {
			t.Fatalf("unexpected advice for %s", a.VM)
		}
		if a.Change != w.change || a.Counted != w.counted {
			t.Fatalf("%s: change %s counted %v, want %s %v", a.VM, a.Change, a.Counted, w.change, w.counted)
		}
	}
}

func TestSizeFamily(t *testing.T) {
	for sku, want := range map[string]string{
		"Standard_D8as_v5": "D", "Standard_E4-2ds_v4": "E", "Standard_B2s_v2": "B",
		"Standard_NC6s_v3": "NC", "Standard_DC2s_v3": "DC", "Basic_A1": "A",
	} {
		if got := SizeFamily(sku); got != want {
			t.Fatalf("%s: got %q, want %q", sku, got, want)
		}
	}
}

func TestCountedRefusesAnUnknownKind(t *testing.T) {
	if _, err := Counted("shrink,cheaper"); err == nil {
		t.Fatal("an unknown kind stops tally at startup")
	}
	c, err := Counted("")
	if err != nil || !c[Shrink] || c[Burstable] || c[Family] || c[Off] {
		t.Fatalf("unset counts shrinking only: %v %v", c, err)
	}
}

func TestShapeRefusesAPageItCannotRead(t *testing.T) {
	if _, err := Shape([][]byte{[]byte("not json")}, nil, nil, time.Now()); err == nil {
		t.Fatal("an unreadable page fails the round")
	}
	if _, err := Shape(nil, [][]byte{[]byte("not json")}, nil, time.Now()); err == nil {
		t.Fatal("an unreadable suppressions page fails the round")
	}
}
