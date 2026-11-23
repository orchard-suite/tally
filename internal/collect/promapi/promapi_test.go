package promapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithTokenSendsBearer(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, WithToken(func(context.Context) (string, error) { return "abc", nil }))
	if _, err := c.Query(context.Background(), "up"); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer abc" {
		t.Fatalf("Authorization %q", got)
	}
}

func TestWithTokenFailureAsksNothing(t *testing.T) {
	asked := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { asked = true }))
	defer srv.Close()
	c := New(srv.URL, WithToken(func(context.Context) (string, error) { return "", errors.New("no") }))
	if _, err := c.Query(context.Background(), "up"); err == nil {
		t.Fatal("want the sign-in error")
	}
	if asked {
		t.Fatal("asked Prometheus without a token")
	}
}
