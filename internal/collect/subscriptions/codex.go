package subscriptions

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// readCodex asks the account's `codex app-server` who is signed in and what
// its usage limits are, over the app server's JSON-RPC on stdin and stdout.
func readCodex(ctx context.Context, a Account) ([]Limit, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	answers, err := codexCall(ctx, a)
	if err != nil {
		return nil, err
	}
	acct, found, err := parseCodexAccount(answers[codexAccountID])
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errSignedOut
	}
	if !sameAccount(acct.Email, a.Email) {
		return nil, fmt.Errorf("%w: %s", errWrongAccount, acct.Email)
	}
	acct.Email = a.Email
	return parseCodexLimits(answers[codexLimitsID], acct)
}

const (
	codexAccountID = 2
	codexLimitsID  = 3
)

// codexCall runs the handshake and both reads, and returns each answer's
// raw line by request id.
func codexCall(ctx context.Context, a Account) (map[int][]byte, error) {
	cmd := a.command(ctx, "codex", "app-server")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex: %w", err)
	}
	defer func() {
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}()
	requests := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"usage-probe","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"initialized"}`,
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"account/read","params":{"refreshToken":false}}`, codexAccountID),
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"account/rateLimits/read"}`, codexLimitsID),
	}
	for _, req := range requests {
		if _, err := stdin.Write([]byte(req + "\n")); err != nil {
			return nil, fmt.Errorf("write to codex: %w", err)
		}
	}
	answers := map[int][]byte{}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var head struct {
			ID *int `json:"id"`
		}
		if json.Unmarshal(sc.Bytes(), &head) != nil || head.ID == nil {
			continue
		}
		answers[*head.ID] = append([]byte(nil), sc.Bytes()...)
		if answers[codexAccountID] != nil && answers[codexLimitsID] != nil {
			return answers, nil
		}
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("codex did not answer: %w", ctx.Err())
	}
	return nil, errors.New("codex exited before answering")
}

type codexAccount struct {
	Email string
	Plan  string
}

// parseCodexAccount reads account/read. Only a ChatGPT login has a
// subscription; an API key does not.
func parseCodexAccount(raw []byte) (codexAccount, bool, error) {
	var res struct {
		Result *struct {
			Account *struct {
				Type     string `json:"type"`
				Email    string `json:"email"`
				PlanType string `json:"planType"`
			} `json:"account"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return codexAccount{}, true, fmt.Errorf("decode codex account: %w", err)
	}
	if res.Error != nil {
		return codexAccount{}, true, fmt.Errorf("codex account/read: %s", res.Error.Message)
	}
	if res.Result == nil || res.Result.Account == nil || res.Result.Account.Type != "chatgpt" || res.Result.Account.Email == "" {
		return codexAccount{}, false, nil
	}
	return codexAccount{Email: res.Result.Account.Email, Plan: res.Result.Account.PlanType}, true, nil
}

type codexWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins *int64  `json:"windowDurationMins"`
	ResetsAt           *int64  `json:"resetsAt"`
}

type codexSnapshot struct {
	LimitID   string       `json:"limitId"`
	LimitName *string      `json:"limitName"`
	Primary   *codexWindow `json:"primary"`
	Secondary *codexWindow `json:"secondary"`
	PlanType  string       `json:"planType"`
}

// parseCodexLimits reads account/rateLimits/read. An account can have
// several limits (rateLimitsByLimitId), each with a short and a long window.
func parseCodexLimits(raw []byte, account codexAccount) ([]Limit, error) {
	var res struct {
		Result *struct {
			RateLimits          codexSnapshot            `json:"rateLimits"`
			RateLimitsByLimitID map[string]codexSnapshot `json:"rateLimitsByLimitId"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decode codex limits: %w", err)
	}
	if res.Error != nil {
		return nil, fmt.Errorf("codex account/rateLimits/read: %s", res.Error.Message)
	}
	if res.Result == nil {
		return nil, errors.New("codex sent no limits")
	}
	snapshots := res.Result.RateLimitsByLimitID
	if len(snapshots) == 0 {
		snapshots = map[string]codexSnapshot{res.Result.RateLimits.LimitID: res.Result.RateLimits}
	}
	ids := make([]string, 0, len(snapshots))
	for id := range snapshots {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var limits []Limit
	for _, id := range ids {
		s := snapshots[id]
		plan := firstNonEmpty(s.PlanType, account.Plan)
		name := id
		if s.LimitName != nil && *s.LimitName != "" {
			name = *s.LimitName
		}
		// The account-wide limit is the plain one; name only the others.
		if id == "codex" {
			name = ""
		}
		for i, w := range []*codexWindow{s.Primary, s.Secondary} {
			if w == nil {
				continue
			}
			window := "session"
			if i == 1 {
				window = "weekly"
			}
			if w.WindowDurationMins != nil {
				window = windowFromMinutes(*w.WindowDurationMins)
			}
			l := Limit{Provider: "codex", Account: account.Email, Plan: plan, Window: window, Name: name, Percent: w.UsedPercent}
			if w.ResetsAt != nil {
				l.ResetsAt = *w.ResetsAt
			}
			limits = append(limits, l)
		}
	}
	return limits, nil
}
