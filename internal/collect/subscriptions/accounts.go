package subscriptions

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Account is one subscription sign-in: a provider and the email it belongs
// to. Its CLI keeps the login in Home, a folder of its own, so any number of
// accounts of one provider sit side by side.
type Account struct {
	Provider string
	Email    string
	Home     string
	homeEnv  string
}

type provider struct {
	name string
	// homeEnv points the CLI at an account's folder instead of its default.
	homeEnv string
	read    func(ctx context.Context, a Account) ([]Limit, error)
	// login is the CLI's sign-in for a terminal with no browser.
	login func(a Account) []string
}

var providers = []provider{
	{
		name: "claude", homeEnv: "CLAUDE_CONFIG_DIR", read: readClaude,
		login: func(a Account) []string { return []string{"claude", "auth", "login", "--claudeai", "--email", a.Email} },
	},
	{
		name: "codex", homeEnv: "CODEX_HOME", read: readCodex,
		login: func(Account) []string { return []string{"codex", "login", "--device-auth"} },
	},
	{
		name: "grok", homeEnv: "GROK_HOME", read: readGrok,
		login: func(Account) []string { return []string{"grok", "login", "--device-auth"} },
	},
}

func providerNamed(name string) (provider, error) {
	for _, p := range providers {
		if p.name == name {
			return p, nil
		}
	}
	return provider{}, fmt.Errorf("unknown provider %q (want claude, codex, or grok)", name)
}

// account names the folder of one sign-in: <root>/<provider>/<email>.
func account(root, providerName, email string) (Account, error) {
	p, err := providerNamed(providerName)
	if err != nil {
		return Account{}, err
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") || strings.ContainsAny(email, `/\`) || strings.HasPrefix(email, ".") {
		return Account{}, fmt.Errorf("%q is not an email address", email)
	}
	return Account{Provider: providerName, Email: email, Home: filepath.Join(root, providerName, email), homeEnv: p.homeEnv}, nil
}

// accounts lists every account folder under root, by provider then email.
func accounts(root string) ([]Account, error) {
	var out []Account
	for _, p := range providers {
		entries, err := os.ReadDir(filepath.Join(root, p.name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			a, err := account(root, p.name, e.Name())
			if err != nil {
				continue
			}
			// Stat, not the entry's type, so a linked folder counts too.
			if info, err := os.Stat(a.Home); err != nil || !info.IsDir() {
				continue
			}
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Provider+"/"+out[i].Email < out[j].Provider+"/"+out[j].Email
	})
	return out, nil
}

// command builds a run of a provider's CLI pointed at the account's folder.
func (a Account) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), a.homeEnv+"="+a.Home)
	cmd.Dir = os.TempDir()
	// A CLI killed at its deadline can leave a child holding its output
	// open; without a bound, the read would wait for that child too.
	cmd.WaitDelay = commandWaitDelay
	return cmd
}

const commandWaitDelay = 2 * time.Second

func (a Account) read(ctx context.Context) Reading {
	p, _ := providerNamed(a.Provider)
	limits, err := p.read(ctx, a)
	return Reading{Account: a, Limits: limits, Err: err}
}
