package ship

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCostAsksForTheRunAsARecordHoldsIt(t *testing.T) {
	const run = "3f0c9a52-1d7e-4b8a-9c11-2b6f8e4d0a71"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.URL.Path != "/v1/runs/"+run {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"run":"`+run+`","amount":1.25,"currency":"USD","tokens":4200,"conversations":2}`)
	}))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TALLY_URL", "")
	t.Setenv("TALLY_TOKEN", "")
	t.Setenv("TALLY_TOKEN_FILE", "")
	if _, _, err := CostConfig(); err == nil {
		t.Fatal("with no URL anywhere there is nothing to ask")
	}
	dir := filepath.Join(home, ".config", "tally")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "ship-url"), []byte(srv.URL+"/\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "ship-token"), []byte("secret\n"), 0o600)
	base, token, err := CostConfig()
	if err != nil || base != srv.URL || token != "secret" {
		t.Fatalf("the files install-ship writes: %q %q %v", base, token, err)
	}
	c, err := Cost(context.Background(), base, token, run)
	if err != nil || c != (RecordCost{Amount: 1.25, Currency: "USD", Tokens: 4200}) {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := Cost(context.Background(), base, "wrong", run); err == nil {
		t.Fatal("a refused token is an error, never a zero cost")
	}
	if _, err := Cost(context.Background(), base, token, "a b"); err == nil {
		t.Fatal("not a run id")
	}
}
