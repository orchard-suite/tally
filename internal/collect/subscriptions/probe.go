// The subscriptions collector is usage-probe, moved here whole: it reads the
// usage limits of every AI subscription account signed in under its
// accounts folder, one after another, and pushes them as OTLP metrics to an
// estate's telemetry under its own service name, usage-probe. It also picks
// the active Claude build account (rotation.go, kube.go). Its metric names,
// labels, settings, and commands are a contract with an estate's dashboards
// and alerts that read them.
package subscriptions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Help is the usage of the subscription commands, as usage-probe printed it.
const Help = `serve                   read every account on a schedule and push the limits
check                   read every account once and print the limits; push nothing
login PROVIDER EMAIL    sign an account in (claude, codex, or grok)
logout PROVIDER EMAIL   forget an account

Accounts live in $USAGE_PROBE_ACCOUNTS/<provider>/<email> (default /data/accounts).
The build accounts rotate when $USAGE_PROBE_ROTATION names a rotation file.`

// Root is the accounts folder, from USAGE_PROBE_ACCOUNTS.
func Root() string {
	return env("USAGE_PROBE_ACCOUNTS", "/data/accounts")
}

// Enabled is true when the accounts folder is named in the environment.
func Enabled() bool {
	return os.Getenv("USAGE_PROBE_ACCOUNTS") != ""
}

// EnsureHome makes the CLIs' HOME, which the image puts on the data volume;
// a new volume starts without it.
func EnsureHome() error {
	if home := os.Getenv("HOME"); home != "" {
		return os.MkdirAll(home, 0o700)
	}
	return nil
}

// Serve, Check, Login, and Logout are usage-probe's commands.
func Serve(ctx context.Context, root string, done func(ok bool)) error {
	return serve(ctx, root, done)
}

func Check(ctx context.Context, root string) error { return check(ctx, root) }

func Login(ctx context.Context, root, provider, email string) error {
	return login(ctx, root, provider, email)
}

func Logout(root, provider, email string) error { return logout(root, provider, email) }

// serve reads every account, pushes, and waits for the next round, until
// the pod is stopped. A failed push is logged and the next round tries again.
// done is told after every round whether it read and pushed.
func serve(ctx context.Context, root string, done func(ok bool)) error {
	endpoint := strings.TrimRight(env("USAGE_PROBE_ENDPOINT", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")), "/")
	if endpoint == "" {
		return errors.New("set USAGE_PROBE_ENDPOINT or OTEL_EXPORTER_OTLP_ENDPOINT to the OTLP endpoint the limits are pushed to")
	}
	every, err := time.ParseDuration(env("USAGE_PROBE_EVERY", "3m"))
	if err != nil {
		return fmt.Errorf("USAGE_PROBE_EVERY: %w", err)
	}
	rot, err := newRotator()
	if err != nil {
		return err
	}
	slog.Info("usage-probe serving", "accounts", root, "every", every.String(), "endpoint", endpoint, "rotation", rot.file, "rotateAt", rot.bar, "pin", rot.pin)
	if rot.kube != nil {
		ensureCtx, cancel := context.WithTimeout(ctx, handOnTimeout)
		if err := rot.kube.ensureConfigMap(ensureCtx, rot.pin, pinKey); err != nil {
			slog.Error("build account pin not created; it cannot be set by hand", "configmap", rot.pin, "error", err.Error())
		}
		cancel()
	}
	// A pin set by hand takes effect within pinCheck, on the last round's
	// readings, rather than waiting for the next round.
	pinTick := time.NewTicker(pinCheck)
	defer pinTick.Stop()
	var readings []Reading
	next := time.After(0)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-next:
			readCtx, cancel := context.WithTimeout(ctx, every)
			r, err := readAll(readCtx, root)
			cancel()
			if err != nil {
				slog.Error("accounts could not be read", "error", err.Error())
				done(false)
			} else {
				readings = r
				notePlans(readings)
				done(handOn(ctx, endpoint, readings, rot))
			}
			next = time.After(every)
		case <-pinTick.C:
			if readings != nil && rot.pinChanged(ctx) {
				handOn(ctx, endpoint, readings, rot)
			}
		}
	}
}

const pinCheck = 15 * time.Second

// handOn picks the build account from the readings and pushes them. It has
// time of its own: a slow read must not cost the round its rotation and its
// metrics. It reports whether the push went through.
func handOn(ctx context.Context, endpoint string, readings []Reading, rot *rotator) bool {
	ctx, cancel := context.WithTimeout(ctx, handOnTimeout)
	defer cancel()
	payload, points := buildPayload(readings, rot.rotate(ctx, readings), time.Now())
	if payload == nil {
		slog.Warn("no accounts signed in; nothing to push")
		return true
	}
	if err := push(ctx, endpoint, payload); err != nil {
		slog.Error("push failed", "endpoint", endpoint, "error", err.Error())
		return false
	}
	slog.Info("limits pushed", "points", points, "accounts", len(readings))
	return true
}

const handOnTimeout = 30 * time.Second

// rotator hands the active build account on after every round: it picks
// from the rotation file and writes the pick to a secret in the probe's
// namespace, which the cluster pushes to Key Vault for every consumer.
type rotator struct {
	file string
	bar  float64
	// prefer is the model whose weekly limit decides between usable
	// accounts: the most capable one.
	prefer string
	secret string
	// pin is the config map that holds the account picked by hand.
	pin  string
	kube *kube
	off  bool
	// pinned is the pin the last pick saw.
	pinned string
}

func newRotator() (*rotator, error) {
	bar, err := strconv.ParseFloat(env("USAGE_PROBE_ROTATE_AT", "95"), 64)
	if err != nil || bar <= 0 || bar > 100 {
		return nil, fmt.Errorf("USAGE_PROBE_ROTATE_AT: want a percent, got %q", os.Getenv("USAGE_PROBE_ROTATE_AT"))
	}
	r := &rotator{file: env("USAGE_PROBE_ROTATION", "/etc/usage-probe/rotation.json"), bar: bar, prefer: env("USAGE_PROBE_PREFER", "Fable"), secret: env("USAGE_PROBE_ACTIVE_SECRET", "claude-active"), pin: env("USAGE_PROBE_PIN", "claude-pin")}
	if k, err := inCluster(); err == nil {
		r.kube = k
	} else {
		slog.Warn("the active build account is not handed on", "reason", err.Error())
	}
	return r, nil
}

// rotate picks the active build account from this round's readings and
// hands it on when it changed. It returns what the metrics report, or nil
// when there is no rotation.
func (r *rotator) rotate(ctx context.Context, readings []Reading) *rotationReport {
	order, err := loadRotation(r.file)
	if err != nil {
		slog.Error("rotation file unreadable; the active build account stands", "error", err.Error())
		return nil
	}
	if len(order) == 0 {
		if !r.off {
			slog.Info("no rotation file; build accounts do not rotate", "file", r.file)
			r.off = true
		}
		return nil
	}
	r.off = false
	if r.kube == nil {
		return newRotationReport(order, r.prefer, pick(order, readings, r.bar, r.prefer, "", ""))
	}
	pinned, err := r.kube.configMapValue(ctx, r.pin, pinKey)
	if err != nil {
		slog.Error("build account picked by hand unreadable; the active build account stands", "configmap", r.pin, "error", err.Error())
		return nil
	}
	pinned = strings.TrimSpace(pinned)
	current, ok, err := r.kube.secretValue(ctx, r.secret, "active")
	if err != nil {
		slog.Error("active build account unreadable", "secret", r.secret, "error", err.Error())
		return nil
	}
	var prev active
	if ok {
		_ = json.Unmarshal(current, &prev)
	}
	ch := pick(order, readings, r.bar, r.prefer, prev.Account, pinned)
	r.pinned = pinned
	if ch.Unpin {
		if err := r.kube.putConfigMap(ctx, r.pin, pinKey, ""); err != nil {
			slog.Error("build account picked by hand not released", "configmap", r.pin, "error", err.Error())
		} else {
			r.pinned = ""
			slog.Info("build account picked by hand released; the pick is automatic", "account", pinned)
		}
	}
	report := newRotationReport(order, r.prefer, ch)
	value, err := json.Marshal(ch.active)
	if err != nil {
		slog.Error("active build account not encoded", "error", err.Error())
		return report
	}
	if ok && bytes.Equal(current, value) {
		return report
	}
	was := prev.Account
	if err := r.kube.putSecret(ctx, r.secret, "active", value); err != nil {
		slog.Error("active build account not handed on", "secret", r.secret, "error", err.Error())
		return report
	}
	slog.Info("build account switched", "from", was, "to", ch.Account, "why", ch.Why, "exhausted", ch.Exhausted)
	return report
}

// pinKey is the config map key that names the account picked by hand.
const pinKey = "account"

// pinChanged is true when the pin differs from the one the last pick saw.
func (r *rotator) pinChanged(ctx context.Context) bool {
	if r.kube == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, handOnTimeout)
	defer cancel()
	pinned, err := r.kube.configMapValue(ctx, r.pin, pinKey)
	if err != nil {
		slog.Warn("build account picked by hand unreadable", "configmap", r.pin, "error", err.Error())
		return false
	}
	return strings.TrimSpace(pinned) != r.pinned
}

// readAll reads every account back to back and logs one line for each.
func readAll(ctx context.Context, root string) ([]Reading, error) {
	accts, err := accounts(root)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	readings := make([]Reading, 0, len(accts))
	for _, a := range accts {
		r := a.read(ctx)
		if r.Err != nil {
			slog.Warn("account read failed", "provider", a.Provider, "account", a.Email, "reason", reason(r.Err), "error", r.Err.Error())
		} else if len(r.Limits) == 0 {
			slog.Info("account signed in; its CLI reports no limits", "provider", a.Provider, "account", a.Email)
		} else {
			slog.Info("account read", "provider", a.Provider, "account", a.Email, "limits", len(r.Limits))
		}
		readings = append(readings, r)
	}
	return readings, nil
}

func check(ctx context.Context, root string) error {
	readings, err := readAll(ctx, root)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	for _, r := range readings {
		if len(r.Limits) > 0 {
			enc.Encode(r.Limits)
		}
	}
	order, err := loadRotation(env("USAGE_PROBE_ROTATION", "/etc/usage-probe/rotation.json"))
	if err != nil {
		return err
	}
	if len(order) > 0 {
		bar, err := strconv.ParseFloat(env("USAGE_PROBE_ROTATE_AT", "95"), 64)
		if err != nil {
			return fmt.Errorf("USAGE_PROBE_ROTATE_AT: %w", err)
		}
		ch := pick(order, readings, bar, env("USAGE_PROBE_PREFER", "Fable"), "", "")
		enc.Encode(map[string]any{"active": ch.Account, "exhausted": ch.Exhausted, "why": ch.Why})
	}
	return nil
}

// login runs the provider's own sign-in in the account's folder, on this
// terminal, then reads the account once to prove it.
func login(ctx context.Context, root, providerName, email string) error {
	a, err := account(root, providerName, email)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(a.Home, 0o700); err != nil {
		return err
	}
	p, _ := providerNamed(a.Provider)
	argv := p.login(a)
	cmd := a.command(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s sign-in: %w", a.Provider, err)
	}
	r := a.read(ctx)
	if r.Err != nil {
		return fmt.Errorf("%s %s signed in, but the read failed: %w", a.Provider, a.Email, r.Err)
	}
	fmt.Printf("%s %s: signed in, %d limits\n", a.Provider, a.Email, len(r.Limits))
	return nil
}

func logout(root, providerName, email string) error {
	a, err := account(root, providerName, email)
	if err != nil {
		return err
	}
	if _, err := os.Stat(a.Home); err != nil {
		return errors.New("no such account")
	}
	if err := os.RemoveAll(a.Home); err != nil {
		return err
	}
	fmt.Printf("%s %s: forgotten\n", a.Provider, a.Email)
	return nil
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
