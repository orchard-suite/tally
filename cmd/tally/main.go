// Command tally is the estate cost meter: it collects what each workload,
// host, agent, and job costs, exports it over OTLP, and serves one board.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/orchard-suite/platformkit/faro"
	"github.com/orchard-suite/platformkit/telemetry"

	"github.com/orchard-suite/tally/internal/agents"
	"github.com/orchard-suite/tally/internal/catalog"
	"github.com/orchard-suite/tally/internal/collect"
	"github.com/orchard-suite/tally/internal/collect/actions"
	"github.com/orchard-suite/tally/internal/collect/advisor"
	"github.com/orchard-suite/tally/internal/collect/aispend"
	"github.com/orchard-suite/tally/internal/collect/azure"
	"github.com/orchard-suite/tally/internal/collect/billed"
	"github.com/orchard-suite/tally/internal/collect/gateway"
	"github.com/orchard-suite/tally/internal/collect/intune"
	"github.com/orchard-suite/tally/internal/collect/kubernetes"
	"github.com/orchard-suite/tally/internal/collect/machines"
	"github.com/orchard-suite/tally/internal/collect/power"
	"github.com/orchard-suite/tally/internal/collect/promapi"
	"github.com/orchard-suite/tally/internal/collect/runners"
	"github.com/orchard-suite/tally/internal/collect/subscriptions"
	"github.com/orchard-suite/tally/internal/estate"
	"github.com/orchard-suite/tally/internal/server"
	"github.com/orchard-suite/tally/internal/ship"
)

// version is set at build time from VERSION.
var version = "dev"

const help = `tally serve                   collect on a schedule, export, and serve the board (the default)
tally ship                    post this machine's agent usage to tally, every minute (TALLY_URL, TALLY_TOKEN_FILE)
tally ship once               one round, then stop
tally ship reset              withdraw everything this machine posted and forget its place (stop the shipper first)
tally ` + "`check`, `login`, `logout`" + `: the subscriptions collector's commands, as usage-probe had them:
` + subscriptions.Help

func main() {
	log := telemetry.NewLogger(os.Stdout, collect.Service, version, slog.LevelInfo)
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	args := os.Args[1:]
	if len(args) >= 1 && args[0] == "ship" {
		if err := runShip(ctx, log, args[1:]); err != nil {
			log.Error("tally ship stopped", "error", err)
			os.Exit(1)
		}
		return
	}
	if err := subscriptions.EnsureHome(); err != nil {
		log.Error("tally stopped", "error", err)
		os.Exit(1)
	}
	var err error
	switch {
	case len(args) == 0 || (len(args) == 1 && args[0] == "serve"):
		err = run(ctx, log)
	case len(args) == 1 && args[0] == "check":
		err = subscriptions.Check(ctx, subscriptions.Root())
	case len(args) == 3 && args[0] == "login":
		err = subscriptions.Login(ctx, subscriptions.Root(), args[1], args[2])
	case len(args) == 3 && args[0] == "logout":
		err = subscriptions.Logout(subscriptions.Root(), args[1], args[2])
	default:
		fmt.Fprintln(os.Stderr, help)
		os.Exit(2)
	}
	if err != nil {
		log.Error("tally stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	shutdownTelemetry, err := telemetry.Setup(ctx, collect.Service, version)
	if err != nil {
		return err
	}
	inst, err := collect.NewInstruments()
	if err != nil {
		return err
	}

	var collectors []collect.Collector
	var sources estate.Sources
	k8s, err := kubernetes.FromEnv(log, inst)
	if err != nil {
		return err
	}
	if k8s != nil {
		collectors = append(collectors, k8s)
		sources.Workloads = k8s.Latest
	}
	pw, err := power.FromEnv(log, inst)
	if err != nil {
		return err
	}
	if pw != nil {
		collectors = append(collectors, pw)
		sources.Power = pw.Latest
	}
	az, err := azure.FromEnv(log, inst)
	if err != nil {
		return err
	}
	if az != nil {
		collectors = append(collectors, az)
		sources.Cloud = az.Latest
	}
	ai, err := aispend.FromEnv(log, inst)
	if err != nil {
		return err
	}
	if ai != nil {
		collectors = append(collectors, ai)
		sources.AI = ai.Latest
	}
	if subs := subscriptions.FromEnv(); subs != nil {
		collectors = append(collectors, subs)
	}
	mc, err := machines.FromEnv(log, inst)
	if err != nil {
		return err
	}
	if mc != nil {
		collectors = append(collectors, mc)
		sources.Machines = mc.Latest
	}
	ad, err := advisor.FromEnv(log, inst)
	if err != nil {
		return err
	}
	if ad != nil {
		collectors = append(collectors, ad)
		sources.Advisor = ad.Latest
	}
	bl, err := billed.FromEnv(log, inst)
	if err != nil {
		return err
	}
	if bl != nil {
		collectors = append(collectors, bl)
		sources.Billed = bl.Latest
	}
	rn, err := runners.FromEnv(log, inst)
	if err != nil {
		return err
	}
	if rn != nil {
		collectors = append(collectors, rn)
		sources.Runners = rn.Latest
	}
	gh, err := actions.FromEnv(log, inst)
	if err != nil {
		return err
	}
	if gh != nil {
		collectors = append(collectors, gh)
		sources.Actions = gh.Latest
	}
	ag, err := agents.FromEnv(log, inst)
	if err != nil {
		return err
	}
	if ag != nil {
		collectors = append(collectors, ag)
		sources.Agents = ag.Spend
		sources.Shipped = ag.Shipped
		sources.Prices = ag.Prices
	}
	zone, err := collect.Zone()
	if err != nil {
		return err
	}
	sources.Zone = zone
	gw, err := gateway.FromEnv(log, inst, zone)
	if err != nil {
		return err
	}
	if gw != nil {
		collectors = append(collectors, gw)
		sources.Gateway = gw.Latest
	}
	if v := os.Getenv("CLUSTER_HOSTS"); v != "" {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				sources.ClusterHosts = append(sources.ClusterHosts, h)
			}
		}
	}
	if err := estate.Register(sources); err != nil {
		return err
	}
	if w := catalog.FromEnv(log, inst, sources); w != nil {
		collectors = append(collectors, w)
	}
	var enroller intune.Enroller
	if ag != nil {
		enroller = ag
	}
	enrolled, err := intune.FromEnv(log, inst, enroller)
	if err != nil {
		return err
	}
	if enrolled != nil {
		collectors = append(collectors, enrolled)
	}
	uses, err := catalog.UsesFromEnv(log, inst, ag)
	if err != nil {
		return err
	}
	if uses != nil {
		collectors = append(collectors, uses)
	}
	names := make([]string, 0, len(collectors))
	for _, c := range collectors {
		names = append(names, c.Name())
	}
	inst.Start(ctx, log, collectors)

	var prom *promapi.Client
	if base := os.Getenv("PROMETHEUS_URL"); base != "" {
		if prom, err = azure.Prometheus(base); err != nil {
			return err
		}
	}
	relay, err := faro.NewRelay(faro.Config{Service: collect.Service, Version: version, URL: os.Getenv("FARO_URL"), Key: os.Getenv("FARO_API_KEY")}, log)
	if err != nil {
		return err
	}
	srv, err := server.New(server.Config{Role: os.Getenv("REQUIRED_ROLE"), Prometheus: prom, Version: version, Agents: ag, DetailRole: os.Getenv("DETAIL_ROLE"), AdminRole: os.Getenv("ADMIN_ROLE"), Zone: zone}, sources, inst, relay, log)
	if err != nil {
		return err
	}

	listeners := []struct {
		name, addr string
		handler    http.Handler
	}{
		{"session", env("SESSION_ADDR", ":8080"), srv.Session()},
		{"api", env("API_ADDR", ":8081"), srv.API()},
	}
	errs := make(chan error, len(listeners))
	servers := make([]*http.Server, 0, len(listeners))
	for _, l := range listeners {
		hs := &http.Server{Addr: l.addr, Handler: l.handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
		servers = append(servers, hs)
		go func() {
			if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("%s port: %w", l.name, err)
			}
		}()
	}
	log.Info("tally started", "collectors", strings.Join(names, ","), "session", listeners[0].addr, "api", listeners[1].addr)

	select {
	case <-ctx.Done():
	case err = <-errs:
	}
	log.Info("tally stopping")
	done, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, hs := range servers {
		_ = hs.Shutdown(done)
	}
	return errors.Join(err, shutdownTelemetry(done))
}

// runShip is `tally ship`: this machine's shipper, every minute or once.
func runShip(ctx context.Context, log *slog.Logger, args []string) error {
	if len(args) > 1 || (len(args) == 1 && args[0] != "once" && args[0] != "reset") {
		fmt.Fprintln(os.Stderr, help)
		os.Exit(2)
	}
	cfg, err := ship.FromEnv()
	if err != nil {
		return err
	}
	s, err := ship.New(cfg, log)
	if err != nil {
		return err
	}
	if len(args) == 1 && args[0] == "reset" {
		return s.Reset(ctx)
	}
	log.Info("tally ship started", "url", cfg.URL, "claude", cfg.Claude, "t3", cfg.T3, "route", cfg.Route, "every", cfg.Every.String())
	if len(args) == 1 {
		return s.Round(ctx)
	}
	return s.Run(ctx)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
