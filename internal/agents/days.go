package agents

import (
	"context"
	"time"
)

// Day is one day's agent work, for the heat map above the day view.
type Day struct {
	// Day is the date in the viewer's zone.
	Day        string  `json:"day"`
	Total      int64   `json:"total"`
	Cost       float64 `json:"cost"`
	AgentHours float64 `json:"agent_hours"`
	Works      int     `json:"works"`
}

// MaxDays is the longest span the heat map asks for.
const MaxDays = 400

// BuildDays adds rows up by day in loc, one entry for every day from from
// up to to, empty days included.
func BuildDays(rows []Row, prices Prices, from, to time.Time, loc *time.Location) []Day {
	type key struct{ machine, id string }
	type convMinute struct {
		conv   key
		minute int64
	}
	out := []Day{}
	index := map[string]int{}
	for d := from.In(loc); d.Before(to); d = d.AddDate(0, 0, 1) {
		k := d.Format(time.DateOnly)
		index[k] = len(out)
		out = append(out, Day{Day: k})
	}
	minutes := make([]map[convMinute]struct{}, len(out))
	works := make([]map[key]struct{}, len(out))
	for _, r := range rows {
		i, ok := index[r.Minute.In(loc).Format(time.DateOnly)]
		if !ok {
			continue
		}
		d := &out[i]
		d.Total += r.Total()
		if rate, ok := prices[Model(r.Model)]; ok {
			d.Cost += rate.Cost(r.Tokens)
		}
		if minutes[i] == nil {
			minutes[i], works[i] = map[convMinute]struct{}{}, map[key]struct{}{}
		}
		minutes[i][convMinute{key{r.Machine, r.Conversation}, r.Minute.Unix()}] = struct{}{}
		works[i][key{r.Machine, r.Work}] = struct{}{}
	}
	for i := range out {
		out[i].AgentHours = float64(len(minutes[i])) / 60
		out[i].Works = len(works[i])
	}
	return out
}

// Days is every day from from up to to in loc, at the prices last read.
func (c *Collector) Days(ctx context.Context, from, to time.Time, loc *time.Location) ([]Day, error) {
	rows, err := c.store.Rows(ctx, from, to)
	if err != nil {
		return nil, err
	}
	return BuildDays(rows, c.Prices(), from, to, loc), nil
}
