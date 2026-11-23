package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// readGrok checks that the account's grok CLI is still signed in. The CLI
// reports no usage limits, so a Grok account has a sign-in status and no
// limits. `grok models` renews the login when it is close to expiring, which
// keeps a sign-in alive between uses.
func readGrok(ctx context.Context, a Account) ([]Limit, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	out, err := a.command(ctx, "grok", "models").CombinedOutput()
	if strings.Contains(string(out), "not authenticated") {
		return nil, errSignedOut
	}
	if err != nil {
		return nil, fmt.Errorf("grok models: %w: %s", err, firstLine(out))
	}
	raw, err := os.ReadFile(filepath.Join(a.Home, "auth.json"))
	if os.IsNotExist(err) {
		return nil, errSignedOut
	}
	if err != nil {
		return nil, err
	}
	email, err := grokEmail(raw)
	if err != nil {
		return nil, err
	}
	if !sameAccount(email, a.Email) {
		return nil, fmt.Errorf("%w: %s", errWrongAccount, email)
	}
	return nil, nil
}

// grokEmail reads the signed-in email from grok's auth.json, which keys each
// login by its issuer and client. Only the email is read.
func grokEmail(raw []byte) (string, error) {
	var logins map[string]struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(raw, &logins); err != nil {
		return "", fmt.Errorf("decode grok auth.json: %w", err)
	}
	for _, l := range logins {
		if l.Email != "" {
			return l.Email, nil
		}
	}
	return "", errSignedOut
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return s
}
