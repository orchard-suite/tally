package gateway

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/tally/internal/agents"
)

func TestShape(t *testing.T) {
	raw, err := os.ReadFile("testdata/query.json")
	if err != nil {
		t.Fatal(err)
	}
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	rows, err := Shape(res)
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		// Before the gateway tagged tokens with the model.
		{Day: "2026-07-22", Key: "dev--ada--max", Product: "dev_pro_max", Model: "", Tokens: agents.Tokens{Input: 31360269 - 1355, CacheRead: 1355}},
		// Two spellings of one model are one row.
		{Day: "2026-10-12", Key: "dev--bob--pro", Product: "dev_pro", Model: "claude-fable-5", Tokens: agents.Tokens{Requests: 2, Input: 5, CacheWrite5m: 18079, Output: 5}},
		// Claude: the prompt is the uncached input.
		{Day: "2026-11-10", Key: "dev--ada--max", Product: "dev_pro_max", Model: "claude-opus-5-5", Tokens: agents.Tokens{Requests: 593, Input: 1112, CacheRead: 266137380, CacheWrite5m: 12641055, Output: 232551}},
		// Others: the prompt holds the cached part.
		{Day: "2026-11-10", Key: "dev--bob--pro", Product: "dev_pro", Model: "gpt-5.6-terra", Tokens: agents.Tokens{Requests: 3, Input: 400, CacheRead: 600, Output: 50}},
	}
	// The model listing's requests carry no tokens and are not usage.
	if len(rows) != len(want) {
		t.Fatalf("%d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d\n got %+v\nwant %+v", i, rows[i], want[i])
		}
	}
}

func TestShapeRefusesAnAnswerWithoutItsColumns(t *testing.T) {
	var res Result
	_ = json.Unmarshal([]byte(`{"tables":[{"columns":[{"name":"day"}],"rows":[]}]}`), &res)
	if _, err := Shape(res); err == nil {
		t.Fatal("a missing column fails the round")
	}
	if _, err := Shape(Result{}); err == nil {
		t.Fatal("no table fails the round")
	}
}

func TestFromAndQuery(t *testing.T) {
	loc, _ := time.LoadLocation("America/Chicago")
	from := From(time.Date(2026, 1, 15, 12, 0, 0, 0, loc), 6, loc)
	if from != time.Date(2025, 7, 1, 0, 0, 0, 0, loc) {
		t.Fatalf("from %v", from)
	}
	q := Query(from, "America/Chicago")
	for _, part := range []string{"datetime(2025-07-01T05:00:00Z)", `datetime_utc_to_local(TimeGenerated, "America/Chicago")`, `"Prompt Cached Tokens"`, `"ai-request"`} {
		if !strings.Contains(q, part) {
			t.Errorf("query lacks %s", part)
		}
	}
}
