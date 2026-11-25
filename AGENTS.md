# tally

The estate cost meter: what each workload, host, agent, and job costs, in
money and in capacity, as metrics and one board. Read `README.md` first.

This repository is public and estate-neutral. Every name in a test, an
example, or a doc comes from the fixture world:
https://github.com/orchard-suite/hedge/blob/main/docs/fixtures.md.

## Shape

- `cmd/tally` is the one binary. `tally serve` (the default) runs every
  configured collector and the board; `tally check|login|logout` are the
  subscriptions collector's commands, as usage-probe had them.
- `internal/collect` is the frame: the service name, the one meter, the
  `Collector` interface, the runner that
  counts `tally.collector.run` by collector and outcome, and the budget
  counter `tally.upstream.requests` for every paid or rate-limited upstream.
  `collect.Every` runs a round on an interval as a root span. Every
  collector names its `Interval`, exported as `tally.collector.interval`.
- `internal/collect/<name>` is one collector each. Every one turns on from
  its own environment variables and is off without them. Each keeps its
  last snapshot and exports it as `tally.<name>.*` observable gauges:
  `kubernetes` (Prometheus), `runners` (CI job pods and their pool, in
  Prometheus), `actions` (GitHub's Actions bill),
  `machines` (servers' vitals in Prometheus),
  `power` (Home Assistant), `azure` (Cost Management), `advisor` (Azure
  Advisor through Resource Graph), `aispend` (prices and usage APIs),
  `billed` (the bill as
  the billing pipeline pushes it into Orrery, read with an access token),
  `gateway` (an AI gateway's token metrics in Log Analytics, by day, key,
  product, and model), and `subscriptions`. When `billed` has a month,
  `estate` takes it as the truth for cloud and AI; collected agent work
  stays beside it.
- `internal/collect/subscriptions` is usage-probe, moved here whole. Its
  metric names (`llm_*`), labels, `USAGE_PROBE_*` settings, OTLP push under
  the service name `usage-probe`, the rotation, and its Kubernetes reads and
  writes are a contract with the dashboards and alerts an estate builds on
  them. Its
  rules in short: ask the provider's CLI, never its API; never log, push, or
  print a token; a failed read is `llm_quota_probe_success = 0`, never a
  zero percent; parsing is pure and tested on fixtures captured from the
  real CLI; CLI versions are pinned in the `Dockerfile`; no dependencies
  beyond the standard library in that package.
- `internal/agents` is agent work: the batch a shipper posts
  (`wire.go`, an upsert contract: a bucket carries a whole minute), the
  SQLite store at `AGENTS_DB` that only tally opens, the price table, the
  pure day report, and the `agents` collector, which also answers
  `POST /v1/usage` on the API port by bearer token (`SHIP_TOKENS`, hashes
  only). Cost is a conversation's own and never split; model names are
  bare (`agents.Model`) before they are priced, grouped, or exported.
- `internal/estate/reconcile.go` lines AI up through three lenses, each
  the truth for one thing: the bill for what was paid, the gateway for
  what was used, the shippers for what it was for. Nothing below the bill
  changes a billed figure, nothing below the gateway sets a token total,
  and a gap or a shipper over the gateway is shown, never spread or
  corrected. A gateway key names a person or an app: it is never a metric
  label, and `GET /v1/gateway/keys` is the detail tier's.
- `internal/ship` is `tally ship`, the shipper that runs on a machine:
  it reads Claude Code's transcripts (`claude.go`), Ion's conversations,
  bindings, and tab titles (`ion.go`), and T3 Code's state database
  (read-only, `t3.go`), counts each reply once, places every conversation
  in its work (`resolve.go`), and posts whole minutes. It also counts what a
  Claude Code conversation used (`agents.Use`): a tool call once by its id,
  a skill a `Skill` call loads, and a slash command a person typed once by
  its line, under the model of the conversation's newest reply. It reads a
  tool call's name and nothing of its input but the skill a `Skill` call
  names. A use goes where its replies go on a route. What each format
  promises, and why a rule exists, is in the comments of its reader. It
  never sends a prompt, a reply, or a file's content. Parsing is pure and
  tested on `testdata/`. With `SHIP_ROUTE` (`route.go`) it posts only the
  replies an AI gateway served, and a conversation and its work only once
  one of their replies did; everything else never leaves the machine, but
  for two things a catalog counts and neither says what the work was: a
  plugin's use (`Use.Plugin`), posted bare under a digest of its
  conversation's id (`agents.Bare`) with no conversation or work beside it,
  and the plugins the machine has installed (`plugins.go`, read from Claude
  Code's own record: name, marketplace, and version), said whole whenever
  the list changes. A use of no plugin stays with its replies. A place kept
  under earlier rules is read again (`stateVersion`, `reread`): every
  transcript written to within `Reread`, so a change to what is posted
  fills in the window a count covers. Claude Code's
  replies count as the gateway's when its settings send it there
  (`ClaudeBaseURL`, read each round from the files `ClaudeSettings` names,
  managed first); a test passes its own paths, never the machine's.
  `tally ship reset` withdraws all the machine posted (`DELETE /v1/usage`)
  and starts over.
- `internal/collect/intune` enrolls a managed fleet: it reads the run
  states of the Intune scripts `SHIP_INTUNE` names through Graph, takes the
  `tally-ship=<sha256>` line each machine's run printed, and hands the
  agents collector the machines by hash (`Collector.Enroll`). A failed read
  keeps the last machines; a hash two machines report is neither's.
  `SHIP_TOKENS` wins for a hash it names. The release's `ship` job
  publishes the shipper binaries this is for (`ship-<version>`).
- `internal/pool` adds the subscriptions collector's limits into one pool
  per provider and scope and projects it; pure, read from Prometheus.
- `internal/estate` adds the collectors up: cost per month by category, each
  namespace's share of the cluster hosts' power (half by requested CPU, half
  by requested memory), right-sizing (the workloads by verdict and what
  each change frees or adds against the cluster's room, by the same
  split; then servers and Advisor's VMs), and the CI view
  (`ci.go`): each repository's share of the self-hosted runners' pool, by the
  same split, beside what hosted runners bill. The self-hosted share is
  already in `power` or `cloud`; only the hosted bill is the `ci`
  category. A request is what `kubernetes.Resources` reads, sidecars
  included; every query of requests or limits goes through it.
- `internal/catalog` writes each namespace's `cost_per_month` and
  `cost_updated` onto its `k8s_namespace` entity in Orrery, through
  platformkit's `orrery` client, when `ORRERY_URL`, `ORRERY_TOKEN`, and `ORRERY_CLUSTER` are
  set. It runs as the collector `catalog`. Every write is enrich mode
  (`EnrichOnly`): tally never creates, owns, or keeps alive a namespace. A
  round whose inputs are stale (`Stale`) writes nothing. `uses.go` is the
  second writer, `uses`: each plugin's and plugin skill's use over 30 days
  onto the entities `ORRERY_USES` names, listed with the read token and
  written in enrich mode, beside `installed_computers`: on how many
  machines that still ship the plugin is installed, a skill taking its
  plugin's. It writes every entity of a template's shape, a
  zero for one nothing used, and never one of another shape. A plugin and a
  skill are found from a use's name alone (`Use.Plugin`).
- `orrery.yaml` at the root says what this repository builds: the service,
  its pillars, the APIs it provides and calls, and its logo, `logo.svg`.
  The forge reader reads it from the repository and writes it into the
  catalog; the app never writes it itself.
- `internal/server` answers on two ports. The session port (8080) is the
  board, `GET /v1/costs`, `GET /v1/agents`, `GET /v1/agents/days`,
  `GET /v1/pools`, `GET /v1/gateway/keys`, and the
  browser telemetry relay; it trusts the front door's `X-Forwarded-Email`
  and `X-Forwarded-Groups`, so only Traefik may reach it. The API port
  (8081) is `/healthz`, `POST /v1/usage`, and `DELETE /v1/usage`, by
  which a shipper withdraws all its machine posted; both trust only a
  shipper's bearer token. What of agent work a person sees is
  `Report.Limit`: spend by user needs `DETAIL_ROLE`, the work and its
  conversations need `ADMIN_ROLE`, and everyone else gets totals and
  models. A new view of agent data decides which tier it belongs to.
- `internal/server/static` is the board, plain scripts with no build:
  `app.js` is the core (formatting, the router, the rail's index, the
  shared pool chart, telemetry), `estate.js` the Statement view, and
  `agents.js` the Agent work view; each view registers itself on
  `window.tally`. The board keeps one ledger rule: a list's bars are
  shares of its section's total, a money column takes one precision
  (whole dollars once any line reaches $100), and a time keeps its parts
  on one line.
- `openapi.yaml` at the root defines every `/v1` route, and `orrery.yaml`
  names it as the `spec` of both the costs and the usage APIs. Tests in
  `internal/server` fail while it and the routes `server.go` registers
  differ, or while `orrery.yaml` names another spec or a missing logo;
  change them together.
- platformkit (`github.com/orchard-suite/platformkit`) is the shared plumbing:
  `door` gates every session route on `REQUIRED_ROLE`, the bare role
  (`Viewer`); `telemetry` is JSON logs with `trace_id`, OTLP,
  `tally.build_info`, the process metrics, and the instrumented ports;
  `faro` is the browser relay and `tally.browser.batches`. tally keeps no
  copy of any of them. Metric names are a contract with the dashboards and
  alerts an estate builds on them; a rename is a breaking change.
- `deploy/macos/tally-ship.plist` is the shipper's launch agent, written by
  `make install-ship`.

## Rules

- A collector exports low-cardinality labels only: a namespace, a workload,
  a host, a provider, an account, a resource group, a service. Never a pod,
  a path with an id, or a timestamp.
- A collector that cannot read its source fails its round and keeps its
  last snapshot; it never exports a zero as a reading.
- Right-sizing judges what a person sizes and what runs now: a pod's
  owner folded up to its template (a CronJob, a runner set), never a pod
  or a single run. Memory is sized to its peak and CPU to its p95; trouble
  (kills, a limit's grip) outranks every size. Advice is a size a manifest
  can hold: floored, rounded, and never a request over its limit.
- A new upstream that is paid or rate-limited counts every request through
  `Instruments.Upstream` with `ok`, `refused`, or `failed`.
- Every shaping function is pure and tested on a fixture under `testdata/`.
  No real names or emails in fixtures.
- Never let a header set identity on the API port, never add a session
  route that skips the person check.
- Logs are JSON lines on stdout. Log both branches of a decision about who
  is let in.
- No continuously repainting CSS animations. No em dashes or en dashes in
  prose.
- `VERSION` is the version of record; never edit it, the manifest, or
  `CHANGELOG.md` by hand.

## Delivery

Agents push to main.

Commit on `main` and land with `make land` (`make land BRANCH=<branch>` from
a worktree). It runs `make check`, then `release-damnit land`. CI runs the
same check on every push and pull request (`test.yml`); the release calls
it first, then versions every `feat` and `fix`, tags it, and builds
`ghcr.io/orchard-suite/tally:<version>` for `linux/amd64` and the shipper
artifact `ghcr.io/orchard-suite/tally:ship-<version>`.

A Mac runs the shipper as a launch agent (`make install-ship`, label
`SHIP_LABEL`, default `tally.ship`). After a change to `internal/ship`,
`internal/agents/wire.go`, or `cmd/tally`, run `make install-ship` once the
release that carries it is live, so the Mac posts with the new build. Its
log is `~/Library/Logs/tally-ship.log`.

## Checks

```bash
make check
```

Run it before every commit; `make land` runs it again before it lands, and
CI runs it on every push. It needs Go (see `go.mod`).
