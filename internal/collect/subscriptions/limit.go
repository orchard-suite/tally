package subscriptions

import (
	"errors"
	"strings"
)

// Limit is one usage limit of one subscription account, as its CLI reported it.
type Limit struct {
	Provider string  `json:"provider"`        // claude or codex
	Account  string  `json:"account"`         // the signed-in email
	Plan     string  `json:"plan"`            // the subscription, such as ultra, pro, or plus
	Window   string  `json:"window"`          // session, weekly, monthly, or spend
	Scope    string  `json:"scope,omitempty"` // the model a limit is scoped to; empty for the whole account
	Name     string  `json:"limit,omitempty"` // the provider's own name for a limit when it has several
	Percent  float64 `json:"percent"`
	ResetsAt int64   `json:"resets_at,omitempty"` // unix seconds; 0 when unknown
}

// Reading is what one account's CLI said on one run.
type Reading struct {
	Account Account
	Limits  []Limit
	Err     error
}

// Why a read failed, as the `reason` label of llm_quota_probe_failure.
var (
	errSignedOut    = errors.New("signed out")
	errWrongAccount = errors.New("signed in as another account")
)

const (
	reasonSignedOut    = "signed_out"
	reasonWrongAccount = "wrong_account"
	reasonReadFailed   = "read_failed"
)

func reason(err error) string {
	switch {
	case errors.Is(err, errSignedOut):
		return reasonSignedOut
	case errors.Is(err, errWrongAccount):
		return reasonWrongAccount
	default:
		return reasonReadFailed
	}
}

// sameAccount compares the email a CLI reports with the account folder's name.
func sameAccount(reported, email string) bool {
	return strings.EqualFold(strings.TrimSpace(reported), email)
}

// windowFromMinutes names a window by its length.
func windowFromMinutes(mins int64) string {
	switch {
	case mins <= 24*60:
		return "session"
	case mins <= 8*24*60:
		return "weekly"
	default:
		return "monthly"
	}
}
