package azure

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeIdentity points the identity at a token endpoint that counts its
// sign-ins and names the audience it was asked for.
func fakeIdentity(t *testing.T, signIns *atomic.Int32, life int) Identity {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		n := signIns.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": r.Form.Get("scope") + "#" + string(rune('0'+n)),
			"expires_in":   life,
		})
	}))
	t.Cleanup(srv.Close)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("assertion"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Identity{TenantID: "tenant", ClientID: "client", TokenFile: file, AuthorityHost: srv.URL}
}

func TestTokensSignInOnceWhileValid(t *testing.T) {
	var n atomic.Int32
	next := Tokens(fakeIdentity(t, &n, 3600), Monitor)
	for range 3 {
		tok, err := next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if tok != Monitor+"/.default#1" {
			t.Fatalf("token %q", tok)
		}
	}
	if n.Load() != 1 {
		t.Fatalf("signed in %d times, want 1", n.Load())
	}
}

func TestTokensSignInAgainNearExpiry(t *testing.T) {
	var n atomic.Int32
	next := Tokens(fakeIdentity(t, &n, 60), Monitor)
	for range 2 {
		if _, err := next(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n.Load() != 2 {
		t.Fatalf("signed in %d times, want 2: a token inside its last five minutes is renewed", n.Load())
	}
}

func TestPrometheusSignsInOnlyForAzureMonitor(t *testing.T) {
	var auth atomic.Value
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer prom.Close()

	c, err := Prometheus(prom.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Query(context.Background(), "up"); err != nil {
		t.Fatal(err)
	}
	if got := auth.Load().(string); got != "" {
		t.Fatalf("a plain Prometheus got Authorization %q", got)
	}

	for _, k := range []string{"AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_FEDERATED_TOKEN_FILE"} {
		t.Setenv(k, "")
	}
	if _, err := Prometheus("https://ws-abc.centralus.prometheus.monitor.azure.com"); err == nil || !strings.Contains(err.Error(), "Azure identity") {
		t.Fatalf("an Azure Monitor workspace without an identity: err %v", err)
	}
}
