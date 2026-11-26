// Package server answers on two ports: the session port is the board and
// its JSON, behind the front door; the API port is the health
// check. Only Traefik may reach the session port, because it trusts the
// door's identity headers.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/orchard-suite/platformkit/door"
	"github.com/orchard-suite/platformkit/telemetry"

	"github.com/orchard-suite/tally/internal/agents"
	"github.com/orchard-suite/tally/internal/collect"
	"github.com/orchard-suite/tally/internal/collect/promapi"
	"github.com/orchard-suite/tally/internal/estate"
	"github.com/orchard-suite/tally/internal/pool"

	// The board's day is the viewer's, named by zone; the image has no
	// zone files of its own.
	_ "time/tzdata"
)

//go:embed static
var staticFS embed.FS

// installFS is what a device reads to put the board on a Home Screen or
// install it as a desktop app: the icons and the manifest. app-icon.svg is
// their source, rendered with rsvg-convert.
//
//go:embed install/*.png install/manifest.webmanifest
var installFS embed.FS

// installFiles are served at the root to anyone: a device fetches them
// without a session, so they skip the person check. They hold nothing
// about the estate.
var installFiles = map[string]string{
	"/apple-touch-icon.png": "image/png",
	"/icon-192.png":         "image/png",
	"/icon-512.png":         "image/png",
	"/manifest.webmanifest": "application/manifest+json",
}

// Config is the server's settings.
type Config struct {
	// Role is the bare front-door role a person needs (Viewer for the app
	// role tally.Viewer); empty admits anyone the door signed in.
	Role string
	// Prometheus, when set, gives the board its trend: the estate's own
	// metric over the last thirty days.
	Prometheus *promapi.Client
	Version    string
	// Agents, when set, is the agent store: shippers post to it on the API
	// port and the board reads its days.
	Agents *agents.Collector
	// DetailRole is the bare role a person needs to see agent spend by
	// user: each machine's cost, tokens, agent hours, and works. AdminRole
	// is the bare role a person needs to see the work itself: each work and
	// conversation, its title, project, timeline, and child agents. An
	// admin sees by user too. Everyone the gate admits sees totals, the
	// days, and each model. An empty role opens its view to everyone who
	// sees the view below it.
	DetailRole string
	AdminRole  string
	// Zone is where the board's days and months begin, unless a request
	// names another.
	Zone *time.Location
}

// Server is the board.
type Server struct {
	cfg     Config
	sources estate.Sources
	inst    *collect.Instruments
	gate    *door.Gate
	relay   http.Handler
	prom    *promapi.Client
	log     *slog.Logger

	trendMu   sync.Mutex
	trendAt   time.Time
	trendData map[string][]Point

	poolsMu   sync.Mutex
	poolsAt   time.Time
	poolsData []pool.Pool
}

// New builds the server. It refuses a role that is not a bare role.
func New(cfg Config, sources estate.Sources, inst *collect.Instruments, relay http.Handler, log *slog.Logger) (*Server, error) {
	for name, role := range map[string]string{"DETAIL_ROLE": cfg.DetailRole, "ADMIN_ROLE": cfg.AdminRole} {
		if role != "" && strings.ContainsAny(role, ". ,") {
			return nil, fmt.Errorf("%s %q must be the bare role, such as Admin for the app role tally.Admin", name, role)
		}
	}
	var roles []string
	if cfg.Role != "" {
		// Whoever may see more may open the board.
		for _, role := range []string{cfg.Role, cfg.DetailRole, cfg.AdminRole} {
			if role != "" && !slices.Contains(roles, role) {
				roles = append(roles, role)
			}
		}
	}
	gate, err := door.NewGate(log, roles...)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, sources: sources, inst: inst, gate: gate, relay: relay, log: log}
	s.prom = cfg.Prometheus
	return s, nil
}

// Session is the board: the page, its assets, its JSON, and the browser
// telemetry relay.
func (s *Server) Session() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	files := http.FileServer(http.FS(static))
	handle := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.gate.Func(h)) }
	index, _ := staticFS.ReadFile("static/index.html")
	handle("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Write(index)
	})
	handle("GET /static/{file}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.StripPrefix("/static/", files).ServeHTTP(w, r)
	})
	for path, contentType := range installFiles {
		body, err := installFS.ReadFile("install" + path)
		if err != nil {
			panic(err)
		}
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Write(body)
		})
	}
	handle("GET /v1/costs", s.costs)
	handle("GET /v1/agents", s.agents)
	handle("GET /v1/agents/days", s.agentDays)
	handle("GET /v1/agents/uses", s.agentUses)
	handle("GET /v1/pools", s.pools)
	handle("GET /v1/gateway/keys", s.gatewayKeys)
	mux.Handle("POST /api/telemetry", s.gate.Wrap(s.relay))
	return telemetry.Handler(mux, "session")
}

// API is the machine port: the health check, shippers' posts, and the
// headroom a machine's agents have, each let in by its own bearer token.
func (s *Server) API() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	if s.cfg.Agents != nil {
		mux.HandleFunc("POST /v1/usage", s.cfg.Agents.Ingest)
		mux.HandleFunc("DELETE /v1/usage", s.cfg.Agents.Withdraw)
		mux.HandleFunc("GET /v1/machine/headroom", s.machineHeadroom)
	}
	return telemetry.Handler(mux, "api")
}

// Board is GET /v1/costs: the estate's costs, the trend, and the collectors.
type Board struct {
	estate.Costs
	Version    string             `json:"version"`
	Trend      map[string][]Point `json:"trend"`
	Collectors []collect.Status   `json:"collectors"`
}

// Point is one day of a category's trend.
type Point struct {
	Time  time.Time `json:"t"`
	Value float64   `json:"v"`
}

func (s *Server) costs(w http.ResponseWriter, r *http.Request) {
	b := Board{Costs: estate.Build(s.sources, time.Now()), Version: s.cfg.Version, Trend: s.trend(r.Context()), Collectors: s.inst.Statuses()}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(b)
}

const trendFor = 5 * time.Minute

// trend is each category's cost over the last thirty days, a point a day,
// from the estate's own metric in Prometheus. It is read at most once in
// five minutes and is empty without a Prometheus.
func (s *Server) trend(ctx context.Context) map[string][]Point {
	if s.prom == nil {
		return map[string][]Point{}
	}
	s.trendMu.Lock()
	defer s.trendMu.Unlock()
	if time.Since(s.trendAt) < trendFor && s.trendData != nil {
		return s.trendData
	}
	now := time.Now()
	series, err := s.prom.QueryRange(ctx, `max by (category) (tally_estate_cost_per_month)`, now.AddDate(0, 0, -30), now, 24*time.Hour)
	if err != nil {
		s.log.WarnContext(ctx, "trend not read", "error", err)
		s.inst.Upstream(ctx, "prometheus", collect.Failed)
		return map[string][]Point{}
	}
	s.inst.Upstream(ctx, "prometheus", collect.OK)
	out := map[string][]Point{}
	for _, sr := range series {
		for _, p := range sr.Points {
			out[sr.Labels["category"]] = append(out[sr.Labels["category"]], Point{Time: p.Time, Value: p.Value})
		}
	}
	s.trendAt, s.trendData = now, out
	return out
}

// agents is GET /v1/agents?day=2006-01-02&tz=Area/City: one day of agent
// work, in the board's zone unless tz names another. Without a day it is
// today.
func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Agents == nil {
		http.Error(w, "the agents collector is off", http.StatusNotFound)
		return
	}
	loc, err := s.zone(r.URL.Query().Get("tz"))
	if err != nil {
		http.Error(w, "tz is not a zone", http.StatusBadRequest)
		return
	}
	day := time.Now().In(loc)
	if v := r.URL.Query().Get("day"); v != "" {
		if day, err = time.ParseInLocation("2006-01-02", v, loc); err != nil {
			http.Error(w, "day is not a date", http.StatusBadRequest)
			return
		}
	}
	from := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	rep, err := s.cfg.Agents.Report(r.Context(), from, from.AddDate(0, 0, 1))
	if err != nil {
		s.log.ErrorContext(r.Context(), "agents report failed", "error", err)
		http.Error(w, "report failed", http.StatusInternalServerError)
		return
	}
	users, works := s.sees(r)
	rep.Zone = loc.String()
	writeJSON(w, rep.Limit(users, works))
}

// zone is the zone a request names, or the board's.
func (s *Server) zone(name string) (*time.Location, error) {
	if name == "" {
		if s.cfg.Zone == nil {
			return time.UTC, nil
		}
		return s.cfg.Zone, nil
	}
	return time.LoadLocation(name)
}

// sees says what of the agent work the person may see: spend by user, and
// the work itself.
func (s *Server) sees(r *http.Request) (users, works bool) {
	p := door.From(r.Context())
	admin := s.cfg.AdminRole != "" && p.Has(s.cfg.AdminRole)
	users = admin || s.cfg.DetailRole == "" || p.Has(s.cfg.DetailRole)
	works = users && (s.cfg.AdminRole == "" || admin)
	if works {
		s.log.DebugContext(r.Context(), "agent view: works and users", "held", p.Roles)
	} else {
		s.log.InfoContext(r.Context(), "agent view limited", "users", users, "works", works, "held", p.Roles)
	}
	return users, works
}

// agentUses is GET /v1/agents/uses?days=30: the skills, commands, and tools
// agents used over the last days (30 unless days says 1 to 90), with how
// many conversations and machines used each. It names what was used and
// never who, so every tier sees it.
func (s *Server) agentUses(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Agents == nil {
		http.Error(w, "the agents collector is off", http.StatusNotFound)
		return
	}
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 90 {
			http.Error(w, "days is not a number from 1 to 90", http.StatusBadRequest)
			return
		}
		days = n
	}
	to := time.Now().Add(time.Minute)
	uses, err := s.cfg.Agents.Uses(r.Context(), to.AddDate(0, 0, -days), to)
	if err != nil {
		s.log.ErrorContext(r.Context(), "agent uses failed", "error", err)
		http.Error(w, "uses failed", http.StatusInternalServerError)
		return
	}
	if uses == nil {
		uses = []agents.UseSum{}
	}
	writeJSON(w, map[string]any{"days": days, "uses": uses})
}

// agentDays is GET /v1/agents/days?from=2006-01-02&to=2006-01-02&tz=Area/City:
// every day from from through to, in the board's zone unless tz names
// another, for the heat map.
func (s *Server) agentDays(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Agents == nil {
		http.Error(w, "the agents collector is off", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	loc, err := s.zone(q.Get("tz"))
	if err != nil {
		http.Error(w, "tz is not a zone", http.StatusBadRequest)
		return
	}
	from, err1 := time.ParseInLocation(time.DateOnly, q.Get("from"), loc)
	to, err2 := time.ParseInLocation(time.DateOnly, q.Get("to"), loc)
	if err1 != nil || err2 != nil || to.Before(from) || to.Sub(from) > agents.MaxDays*24*time.Hour {
		http.Error(w, fmt.Sprintf("from and to are dates, from first, at most %d days apart", agents.MaxDays), http.StatusBadRequest)
		return
	}
	days, err := s.cfg.Agents.Days(r.Context(), from, to.AddDate(0, 0, 1), loc)
	if err != nil {
		s.log.ErrorContext(r.Context(), "agent days failed", "error", err)
		http.Error(w, "days failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, days)
}

// GatewayKeys is GET /v1/gateway/keys: what every AI gateway key used in
// one month.
type GatewayKeys struct {
	Month string           `json:"month"`
	Keys  []estate.KeyLine `json:"keys"`
}

// gatewayKeys is GET /v1/gateway/keys?month=2006-01: every gateway key's
// month, in the board's zone, this month without one. A key names a person
// or an app, so it is the detail tier's, as spend by user is.
func (s *Server) gatewayKeys(w http.ResponseWriter, r *http.Request) {
	if s.sources.Gateway == nil {
		http.Error(w, "the gateway collector is off", http.StatusNotFound)
		return
	}
	if users, _ := s.sees(r); !users {
		http.Error(w, "spend by key needs the detail role", http.StatusForbidden)
		return
	}
	month := r.URL.Query().Get("month")
	if month == "" {
		loc, _ := s.zone("")
		month = time.Now().In(loc).Format("2006-01")
	} else if _, err := time.Parse("2006-01", month); err != nil {
		http.Error(w, "month is not a year and month, such as 2026-10", http.StatusBadRequest)
		return
	}
	writeJSON(w, GatewayKeys{Month: month, Keys: s.sources.Keys(month)})
}

const poolsFor = 2 * time.Minute

// readPools is every subscription pool, read from Prometheus at most once
// in two minutes.
func (s *Server) readPools(ctx context.Context) ([]pool.Pool, error) {
	if s.prom == nil {
		return []pool.Pool{}, nil
	}
	s.poolsMu.Lock()
	defer s.poolsMu.Unlock()
	if time.Since(s.poolsAt) >= poolsFor || s.poolsData == nil {
		p, err := pool.Query(ctx, s.prom, time.Now().UTC().Truncate(time.Minute))
		if err != nil {
			s.inst.Upstream(ctx, "prometheus", collect.Failed)
			return nil, err
		}
		s.inst.Upstream(ctx, "prometheus", collect.OK)
		s.poolsAt, s.poolsData = time.Now(), p
	}
	return s.poolsData, nil
}

// pools is GET /v1/pools: every subscription pool, recorded and projected.
func (s *Server) pools(w http.ResponseWriter, r *http.Request) {
	p, err := s.readPools(r.Context())
	if err != nil {
		s.log.WarnContext(r.Context(), "pools not read", "error", err)
		http.Error(w, "pools not read", http.StatusBadGateway)
		return
	}
	writeJSON(w, p)
}

// machineHeadroom is GET /v1/machine/headroom: what a machine's agents can
// draw on from one provider, for the models they run, for a machine that
// shows its shipping token. Tally does the working out, so the app on the
// machine needs no rules of its own.
func (s *Server) machineHeadroom(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	machine := s.cfg.Agents.Machine(r)
	if machine == "" {
		s.log.WarnContext(ctx, "machine headroom refused", "reason", "no token or an unknown token")
		http.Error(w, "unknown token", http.StatusUnauthorized)
		return
	}
	provider := r.URL.Query().Get("provider")
	if provider == "" {
		provider = "claude"
	}
	models := r.URL.Query()["model"]
	pools, err := s.readPools(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "machine headroom not read", "machine", machine, "error", err)
		http.Error(w, "pools not read", http.StatusBadGateway)
		return
	}
	var rot pool.Rotation
	if s.prom != nil {
		rot, err = pool.QueryRotation(ctx, s.prom, provider)
		if err != nil {
			s.inst.Upstream(ctx, "prometheus", collect.Failed)
			s.log.WarnContext(ctx, "machine headroom not read", "machine", machine, "error", err)
			http.Error(w, "rotation not read", http.StatusBadGateway)
			return
		}
		s.inst.Upstream(ctx, "prometheus", collect.OK)
	}
	h := pool.Machine(provider, models, pools, rot)
	s.log.InfoContext(ctx, "machine headroom read", "machine", machine, "provider", provider, "models", models, "used", h.Used, "usable", h.Usable)
	writeJSON(w, h)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
