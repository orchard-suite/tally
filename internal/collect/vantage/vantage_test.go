package vantage

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/orchard-suite/tally/internal/agents"
	"github.com/orchard-suite/tally/internal/collect"
)

const run = "3f0c9a52-1d7e-4b8a-9c11-2b6f8e4d0a71"

func page(t *testing.T) Page {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "changes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p Page
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFoldTakesAgentRunsClaims(t *testing.T) {
	claims, stale := Fold(page(t))
	want := []agents.Claim{
		{Run: run, Item: "ACME-7", Agent: "claude-code on laptop-1", At: time.Date(2026, 11, 20, 15, 4, 30, 0, time.UTC)},
		{Run: run, Item: "ACME-9", Agent: "claude-code on laptop-1", At: time.Date(2026, 11, 20, 15, 30, 10, 0, time.UTC)},
	}
	if len(claims) != len(want) || claims[0] != want[0] || claims[1] != want[1] {
		t.Fatalf("a person's claim is no run: %+v", claims)
	}
	if len(stale) != 2 || stale[0] != "ACME-7" || stale[1] != "ACME-9" {
		t.Fatalf("made and scoped items are read again: %v", stale)
	}
}

func TestRoundFollowsTheHub(t *testing.T) {
	changes, _ := os.ReadFile(filepath.Join("testdata", "changes.json"))
	scopes, _ := os.ReadFile(filepath.Join("testdata", "scopes.json"))
	var afters []string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer vpt_reader" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/changes":
			afters = append(afters, r.URL.Query().Get("after"))
			if r.URL.Query().Get("after") == "0" {
				w.Write(changes)
				return
			}
			io.WriteString(w, `{"changes":[],"next":17}`)
		case "/api/v1/items/ACME-7":
			io.WriteString(w, `{"card":{"scope":"billing"}}`)
		case "/api/v1/items/ACME-9":
			io.WriteString(w, `{"card":{"scope":"payments"}}`)
		case "/api/v1/scopes":
			w.Write(scopes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()
	inst, err := collect.NewInstruments()
	if err != nil {
		t.Fatal(err)
	}
	store, err := agents.OpenStore(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	c := New(hub.URL, "vpt_reader", store, slog.New(slog.NewTextHandler(io.Discard, nil)), inst, time.Minute)
	if err := c.Round(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Round(ctx); err != nil {
		t.Fatal(err)
	}
	if len(afters) != 2 || afters[0] != "0" || afters[1] != "17" {
		t.Fatalf("each round carries on from the last position: %v", afters)
	}
	l, err := store.Links(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Claims) != 2 || l.Scopes["ACME-7"] != "billing" || l.Scopes["ACME-9"] != "payments" || len(l.Tree) != 4 {
		t.Fatalf("links %+v", l)
	}
	for _, n := range l.Tree {
		if n.ID == "billing" && (n.Parent != "finance" || n.Kind != "program") {
			t.Fatalf("tree %+v", l.Tree)
		}
	}

	bad := New(hub.URL, "vpt_wrong", store, slog.New(slog.NewTextHandler(io.Discard, nil)), inst, time.Minute)
	if err := bad.Round(ctx); err == nil {
		t.Fatal("a hub that refuses the token fails the round")
	}
	if p, _ := store.FeedPosition(ctx); p != 17 {
		t.Fatalf("a failed round keeps what was taken: %d", p)
	}
}
