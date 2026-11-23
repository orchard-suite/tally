package azure

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestShapeReadsRowsByColumnName(t *testing.T) {
	raw, err := os.ReadFile("testdata/query.json")
	if err != nil {
		t.Fatal(err)
	}
	costs, err := Shape(raw, "sub-1", Current)
	if err != nil {
		t.Fatal(err)
	}
	if len(costs) != 4 {
		t.Fatalf("got %d rows: %+v", len(costs), costs)
	}
	if costs[0].Service != "Virtual Network" || costs[0].Cost != 48.2 || costs[0].Group != "rg-core" || costs[0].Currency != "USD" || costs[0].Period != Current || costs[0].Account != "sub-1" || costs[0].Provider != "azure" {
		t.Errorf("largest first: %+v", costs[0])
	}
	if costs[3].Group != "(none)" {
		t.Errorf("an empty group is named: %+v", costs[3])
	}
	if _, err := Shape([]byte(`{"properties":{"columns":[{"name":"Cost"}],"rows":[]}}`), "s", Current); err == nil {
		t.Error("an answer without the grouping columns passed")
	}
}

func TestMonthsAndQuery(t *testing.T) {
	cur, prev := Months(time.Date(2026, 3, 15, 10, 0, 0, 0, time.FixedZone("x", -5*3600)))
	if cur.From != time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC) || cur.To != time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC) {
		t.Errorf("current = %+v", cur)
	}
	if prev.From != time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC) || prev.To != time.Date(2026, 2, 28, 23, 59, 59, 0, time.UTC) {
		t.Errorf("previous = %+v", prev)
	}
	// On November 1 the trailing 30 days reach back to October 3, today included.
	tr := TrailingDays(time.Date(2026, 11, 1, 12, 30, 0, 0, time.FixedZone("x", -5*3600)), 30)
	if tr.From != time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC) || tr.To != time.Date(2026, 11, 1, 23, 59, 59, 0, time.UTC) {
		t.Errorf("trailing = %+v", tr)
	}
	var q struct {
		Type       string            `json:"type"`
		Timeframe  string            `json:"timeframe"`
		TimePeriod map[string]string `json:"timePeriod"`
		Dataset    struct {
			Granularity string              `json:"granularity"`
			Grouping    []map[string]string `json:"grouping"`
		} `json:"dataset"`
	}
	if err := json.Unmarshal(Query(cur), &q); err != nil {
		t.Fatal(err)
	}
	if q.Type != "ActualCost" || q.Timeframe != "Custom" || q.TimePeriod["from"] != "2026-03-01T00:00:00Z" || q.Dataset.Granularity != "None" || len(q.Dataset.Grouping) != 2 || q.Dataset.Grouping[1]["name"] != "ServiceName" {
		t.Errorf("query = %+v", q)
	}
}

func TestParseSubscriptions(t *testing.T) {
	ids, err := ParseSubscriptions([]byte(`{"value":[{"id":"/subscriptions/aaa","subscriptionId":"aaa","displayName":"Acme"},{"subscriptionId":"bbb"}],"count":{"type":"Total","value":2}}`))
	if err != nil || len(ids) != 2 || ids[0] != "aaa" || ids[1] != "bbb" {
		t.Errorf("ids = %v, %v", ids, err)
	}
}

func TestRetryAfterReadsTheHeaderOrWaitsAMinute(t *testing.T) {
	h := http.Header{}
	if RetryAfter(h) != time.Minute {
		t.Error("no header: a minute")
	}
	h.Set("Retry-After", "17")
	if RetryAfter(h) != 17*time.Second {
		t.Error("header in seconds")
	}
	h.Set("Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT")
	if RetryAfter(h) != time.Minute {
		t.Error("a date falls back to a minute")
	}
}
