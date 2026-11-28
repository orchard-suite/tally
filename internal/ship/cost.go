package ship

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/orchard-suite/tally/internal/agents"
)

// RecordCost is a run's cost as a Vantage run record holds it.
type RecordCost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
	Tokens   int64   `json:"tokens"`
}

// CostConfig is where `tally ship cost` asks: the tally this machine posts
// to and the machine's token. TALLY_URL and TALLY_TOKEN or TALLY_TOKEN_FILE
// win; without them it reads the files `make install-ship` writes,
// ship-url and ship-token in ~/.config/tally, since an agent's bridge runs
// it with none of the shipper's settings.
func CostConfig() (url, token string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(home, ".config", "tally")
	url = os.Getenv("TALLY_URL")
	if url == "" {
		url, err = readTrim(filepath.Join(dir, "ship-url"))
		if err != nil {
			return "", "", fmt.Errorf("set TALLY_URL or write it to ~/.config/tally/ship-url: %w", err)
		}
	}
	token = os.Getenv("TALLY_TOKEN")
	if token == "" {
		f := env("TALLY_TOKEN_FILE", filepath.Join(dir, "ship-token"))
		if token, err = readTrim(f); err != nil {
			return "", "", fmt.Errorf("the machine's token: %w", err)
		}
	}
	if url == "" || token == "" {
		return "", "", errors.New("the tally URL or the machine's token is empty")
	}
	return strings.TrimRight(url, "/"), token, nil
}

func readTrim(path string) (string, error) {
	b, err := os.ReadFile(path)
	return strings.TrimSpace(string(b)), err
}

// Cost asks tally what run cost of what this machine posted, and answers it
// as a run record holds a cost.
func Cost(ctx context.Context, base, token, run string) (RecordCost, error) {
	if !agents.ValidRun(run) {
		return RecordCost{}, fmt.Errorf("%q is not a run id", run)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/runs/"+url.PathEscape(run), nil)
	if err != nil {
		return RecordCost{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return RecordCost{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return RecordCost{}, fmt.Errorf("tally answered %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var c agents.RunCost
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&c); err != nil {
		return RecordCost{}, err
	}
	return RecordCost{Amount: c.Amount, Currency: c.Currency, Tokens: c.Tokens}, nil
}
