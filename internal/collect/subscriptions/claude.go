package subscriptions

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const claudeRequestID = "usage-probe"

// readClaude asks the account's claude CLI who is signed in and what its
// usage limits are. The CLI talks to Anthropic with its own login and renews
// it; the probe never reads the credential.
func readClaude(ctx context.Context, a Account) ([]Limit, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	// Signed out exits non-zero with the same JSON, so the output is read
	// either way.
	out, _ := a.command(ctx, "claude", "auth", "status", "--json").Output()
	var status struct {
		LoggedIn         bool   `json:"loggedIn"`
		Email            string `json:"email"`
		SubscriptionType string `json:"subscriptionType"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return nil, fmt.Errorf("claude auth status: %w", err)
	}
	// A long-lived inference token (claude setup-token) is logged in with no
	// email: it cannot read limits, so it counts as signed out.
	if !status.LoggedIn || status.Email == "" {
		return nil, errSignedOut
	}
	if !sameAccount(status.Email, a.Email) {
		return nil, fmt.Errorf("%w: %s", errWrongAccount, status.Email)
	}
	raw, err := claudeUsage(ctx, a)
	if err != nil {
		return nil, err
	}
	return parseClaudeUsage(raw, a.Email, cmp.Or(claudePlanName(a.Home), status.SubscriptionType))
}

// claudePlanName is the plan's name as Claude shows it to the person, such
// as ultra, lowercased; empty when the CLI has not stored one. The CLI's
// subscription type cannot tell Ultra from Max: it says max for both. The
// name comes from the profile the CLI keeps in its own config file, never
// from the credential file beside it.
func claudePlanName(home string) string {
	raw, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return ""
	}
	var config struct {
		OAuthAccount struct {
			PlanDisplayName string `json:"planDisplayName"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(config.OAuthAccount.PlanDisplayName))
}

// claudeUsage sends the get_usage control request over stream-json and
// returns the matching control_response line. --restricted keeps hooks,
// plugins, and MCP servers out of the run: the read makes no model call and
// needs none of them.
func claudeUsage(ctx context.Context, a Account) ([]byte, error) {
	cmd := a.command(ctx, "claude", "-p", "--restricted", "--strict-mcp-config",
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--no-session-persistence")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}
	defer func() {
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}()
	req := fmt.Sprintf(`{"type":"control_request","request_id":%q,"request":{"subtype":"get_usage"}}`+"\n", claudeRequestID)
	if _, err := stdin.Write([]byte(req)); err != nil {
		return nil, fmt.Errorf("write usage request: %w", err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var head struct {
			Type     string `json:"type"`
			Response struct {
				RequestID string `json:"request_id"`
			} `json:"response"`
		}
		if json.Unmarshal(sc.Bytes(), &head) == nil && head.Type == "control_response" && head.Response.RequestID == claudeRequestID {
			return append([]byte(nil), sc.Bytes()...), nil
		}
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("claude did not answer the usage request: %w", ctx.Err())
	}
	return nil, errors.New("claude exited without answering the usage request")
}

type claudeWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

// parseClaudeUsage reads the get_usage answer. The `limits` list is the
// CLI's own summary of every limit that applies; the named windows are the
// fallback for a CLI that predates it. plan names the account's plan; the
// answer's subscription type fills it in only when it is empty.
func parseClaudeUsage(raw []byte, account, plan string) ([]Limit, error) {
	var res struct {
		Response struct {
			Subtype  string `json:"subtype"`
			Error    string `json:"error"`
			Response struct {
				SubscriptionType    string `json:"subscription_type"`
				RateLimitsAvailable bool   `json:"rate_limits_available"`
				RateLimits          *struct {
					Limits []struct {
						Kind     string   `json:"kind"`
						Percent  *float64 `json:"percent"`
						ResetsAt string   `json:"resets_at"`
						Scope    *struct {
							Model *struct {
								DisplayName string `json:"display_name"`
							} `json:"model"`
						} `json:"scope"`
					} `json:"limits"`
					FiveHour    *claudeWindow `json:"five_hour"`
					SevenDay    *claudeWindow `json:"seven_day"`
					ModelScoped []struct {
						DisplayName string `json:"display_name"`
						claudeWindow
					} `json:"model_scoped"`
					Spend *struct {
						Enabled bool     `json:"enabled"`
						Percent *float64 `json:"percent"`
					} `json:"spend"`
				} `json:"rate_limits"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decode claude usage: %w", err)
	}
	if res.Response.Subtype != "success" {
		return nil, fmt.Errorf("claude refused the usage request: %s", firstNonEmpty(res.Response.Error, res.Response.Subtype))
	}
	body := res.Response.Response
	plan = cmp.Or(plan, body.SubscriptionType)
	rl := body.RateLimits
	if !body.RateLimitsAvailable || rl == nil {
		return nil, errors.New("claude reports no usage limits for this login")
	}

	var limits []Limit
	add := func(window, scope string, pct *float64, resetsAt string) {
		if pct == nil {
			return
		}
		limits = append(limits, Limit{
			Provider: "claude", Account: account, Plan: plan,
			Window: window, Scope: scope, Percent: *pct, ResetsAt: unixFromRFC3339(resetsAt),
		})
	}
	if len(rl.Limits) > 0 {
		for _, l := range rl.Limits {
			scope := ""
			if l.Scope != nil && l.Scope.Model != nil {
				scope = l.Scope.Model.DisplayName
			}
			add(claudeWindowName(l.Kind), scope, l.Percent, l.ResetsAt)
		}
	} else {
		if w := rl.FiveHour; w != nil {
			add("session", "", w.Utilization, w.ResetsAt)
		}
		if w := rl.SevenDay; w != nil {
			add("weekly", "", w.Utilization, w.ResetsAt)
		}
		for _, m := range rl.ModelScoped {
			add("weekly", m.DisplayName, m.Utilization, m.ResetsAt)
		}
	}
	if s := rl.Spend; s != nil && s.Enabled {
		add("spend", "", s.Percent, "")
	}
	return limits, nil
}

// claudeWindowName maps the CLI's limit kinds onto the probe's windows. A
// kind it does not know passes through, so a new limit still shows up.
func claudeWindowName(kind string) string {
	switch {
	case kind == "session":
		return "session"
	case strings.HasPrefix(kind, "weekly"):
		return "weekly"
	case strings.HasPrefix(kind, "monthly"):
		return "monthly"
	default:
		return kind
	}
}

func unixFromRFC3339(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
