# tally

Cost as a visible signal for a mixed estate: bare metal and VMs, a
Kubernetes cluster, cloud accounts, AI subscriptions. tally says what each
workload, host, agent, and job costs, in money and in capacity, prints the
right-sizing instead of leaving it as a chore, exports everything as metrics
(OTLP to Prometheus), and serves one board. Every collector is optional and
turns on from configuration, so it fits any organization.

## Collectors

| Collector | Turns on with | Reads | Exports |
| --- | --- | --- | --- |
| `kubernetes` | `PROMETHEUS_URL` | Per workload that runs now and container, from the Kubernetes recording rules (kube-prometheus-stack's, or Managed Prometheus's defaults): CPU and memory requested and limited, used at p95 and at peak over `K8S_WINDOW` (7d) in its busiest pod, out-of-memory kills, and CPU throttling | `tally.k8s.cpu.request_cores`, `tally.k8s.cpu.p95_cores`, `tally.k8s.cpu.limit_cores`, `tally.k8s.memory.request_bytes`, `tally.k8s.memory.p95_bytes`, `tally.k8s.memory.peak_bytes`, `tally.k8s.memory.limit_bytes` by `namespace`, `workload`, `container`; `tally.k8s.recommendation` (1, with `kind` risk, under, unset, or over, and `resource`) |
| `power` | `PROMETHEUS_URL` with `POWER_PROMETHEUS_QUERY` (a query whose series carry a `host` label, in watts, such as `ipmi_dcmi_power_consumption_watts{job="ipmi"}` from a host's own IPMI meter), or `HASS_URL`, `HASS_TOKEN`, `HASS_POWER_ENTITIES` (`entity_id=host,...`), or both | Each host's watts from its meter in Prometheus or its Home Assistant sensor (W or kW), priced at `ELECTRICITY_PRICE_PER_KWH` | `tally.power.watts`, `tally.power.cost_per_hour` by `host` |
| `azure` | `AZURE_COST=on` and the Azure identity's environment (`AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_FEDERATED_TOKEN_FILE`), with `AZURE_SUBSCRIPTIONS` to narrow it | Cost Management, by resource group and service, for the last 30 days (the run rate the board leads with), this month so far, and last month, for every subscription the identity can see. Workload identity only; no client secret. | `tally.cloud.cost` by `provider`, `account`, `group`, `service`, `currency`, `period` (trailing, current, or previous) |
| `machines` | `PROMETHEUS_URL` with `MACHINES=on`, and `MACHINES_SKIP` (`host,...`) to leave out physical hosts and people's own machines | Each server's memory total, its memory used at p95 and at peak, and its CPU used at p95 over `MACHINES_WINDOW` (7d), from the device vitals (`device_*{role="server"}`), with the core count node-exporter gives where it runs; for a Kubernetes node, what its pods ask for and what it holds back | `tally.machines.recommendation` (1, with `kind` over or under) by `host`, `resource` |
| `advisor` | `ADVISOR=on` and the Azure identity's environment; the identity needs Reader where the VMs are | Azure Advisor's open advice to resize or shut down underused virtual machines, through Resource Graph, for every subscription the identity can see: the size now, the size advised, the kind of change, CPU and memory at p95, and the saving a year | `tally.advisor.recommendations`, `tally.advisor.savings` by `change` and `counted` |
| `ai` | `PLAN_PRICES` (`provider:plan=price,...`, applied to every account the `subscriptions` collector sees on that plan, such as `claude:max=200`), `SUBSCRIPTION_PRICES` (`provider:account=price,...`, a price by name, which wins), `OPENAI_ADMIN_KEY`, `ANTHROPIC_ADMIN_KEY` | Each subscription's monthly price, and month-to-date tokens from a provider's organization usage API where there is one. An account on a plan with no price is listed unpriced. | `tally.ai.cost_per_month` by `provider`, `account`; `tally.ai.tokens` by `provider`, `account`, `direction` |
| `agents` | `AGENTS_DB` (the store's path), with `SHIP_TOKENS` (`machine=<sha256 of its token>,...`) | What every machine's shipper posts to `POST /v1/usage`: each agent conversation's tokens by model and minute, and the work it belongs to. Prices from LiteLLM's public table (`PRICES_URL`), read once a day | `tally.agents.cost` (this month at API prices) by `machine`, `model`, `role`; `tally.agents.tokens` by those and `kind`; `tally.agents.last_post` by `machine`; `tally.agents.batches` by `outcome` |
| `billed` | `ORRERY_URL` with `ORRERY_READ_TOKEN`, an Orrery access token that reads | The bill month by month, as the estate's billing pipeline pushes it into Orrery: each `finops_billing_period`'s AI, the part of it billed for model use (`ai_models_cents`, when the pipeline writes it), and whole usage, and each `finops_program_portfolio_cost`'s AI by program. A month appears once the pipeline has run it, after the month closes | `tally.billed.ai`, `tally.billed.usage` for the newest billed month |
| `gateway` | `GATEWAY_WORKSPACE`, the id of the Log Analytics workspace an AI gateway's token metrics land in, read with the Azure identity | What the gateway measured: every key's tokens and requests by day, product tier, and model, from its `llm-emit-token-metric` and `ai-request` metrics in `AppMetrics`, over this month and the `GATEWAY_MONTHS` (6) before it. A Claude model's prompt tokens leave out the cache and every other model's include it; both become input, cache reads, and cache writes as agent work counts them | `tally.gateway.tokens` (this month) by `model` and `kind`; `tally.gateway.requests` by `model`. A key is never a label |
| `subscriptions` | `USAGE_PROBE_ACCOUNTS` | The usage limits of every signed-in AI subscription account, through the provider's CLI, and the Claude build-account rotation. This is usage-probe, moved whole; see below. | `llm_quota_*` and `llm_rotation_*`, pushed under the service name `usage-probe` |
| `runners` | `PROMETHEUS_URL` with `RUNNERS_NAMESPACE` (where the job pods run) and `RUNNERS_FORGE` (`github` for Actions Runner Controller, `gitlab` for GitLab's Kubernetes executor); `RUNNERS_NODES` (a pattern naming the pool's own nodes, such as `aks-ci-.*`) with `RUNNERS_NODE_PRICE` (one node an hour) when the pool has its own nodes; `GITLAB_URL` and `GITLAB_TOKEN` (`read_api`) to name GitLab projects | Over `RUNNERS_WINDOW` (7d): each repository's job pods, how long they ran and what CPU and memory they requested (sidecars included), the repository named from the runner set's listener gauges or the GitLab project; and the pool: its own nodes' hours, or its namespace's average share of the cluster's requests | Feeds the CI view below |
| `actions` | `GITHUB_BILLING` (`user:<name>,org:<name>,...`) with `GITHUB_BILLING_TOKEN`, a token that reads those accounts' billing | GitHub's billing usage for Actions, by day, repository, and SKU, this month and last, every `GITHUB_BILLING_INTERVAL` (2h): minutes, list price, and what is owed after included minutes | Feeds the CI view below |

From those, `tally.estate.cost_per_month` by `category` (`power`, `cloud`,
`ai`, and `ci`, the hosted runners' bill) and `tally.estate.namespace_cost_per_month` by `namespace`: the
cluster hosts' power cost (`CLUSTER_HOSTS`, or every powered host) shared
out half by requested CPU and half by requested memory.

### Builds

With `runners` or `actions` on, the Statement's Hosted CI section and `ci` in
`GET /v1/costs` show each repository's builds over the runners' window,
held for a month:

- **On self-hosted runners.** The pool's cost a month is its own nodes'
  hours at `RUNNERS_NODE_PRICE`, or, for a pool that shares the cluster,
  its namespace's share of the cluster hosts' power, half by requested CPU
  and half by requested memory, as namespaces share it. Each repository
  takes its part of the pool the same way, by what its job pods requested
  for as long as they ran. Without the hosts' power a shared pool is
  unpriced, and its minutes still show. This money is already in `power` or
  `cloud`; the view splits it, never adds it.
- **On hosted runners.** What GitHub bills over the window's whole days,
  net of included minutes, with the list price beside it. This money is
  in no other category, so it is the `ci` category: the last 30 whole
  days, this month so far, and last month, in UTC as GitHub bills.

`tally.ci.cost_per_month` and `tally.ci.minutes` by `forge`, `repository`,
and `runner` (`self-hosted` or `hosted`), and `tally.ci.pool_cost_per_month` by
`namespace`, carry the same numbers. A pool with no price, and a bill not
yet read, export nothing rather than a zero.

### Agent work, from the machines

`tally ship` runs on each machine where agents work. It reads Claude Code's
transcripts (`~/.claude/projects`), Ion's conversations
(`~/.ion/conversations`, with its session bindings and tab titles), and T3
Code's thread tree (`~/.t3/userdata/statev2.sqlite`, read-only) and posts,
every minute, what changed: each conversation's tokens by model and minute, which work it
belongs to, whether it is the lead or a child, and what it used: each skill
an agent loaded, each slash command a person typed, and each tool an agent
called, by name and count. It sends ids, titles, names, and counts, never a
prompt, a reply, a file, or what a tool was asked.

- **Work.** A T3 thread a person started, with every thread it delegated
  and every subagent inside them, is one work. T3 runs Claude Code by its
  session id and Ion through an ACP session that Ion's bindings name. An
  agent Ion dispatches is a child of its work, and a Claude Code session an
  Ion conversation ran itself shares its place. Anything else is a work of
  its own.
- **Counted once.** A reply is counted once by its message and request ids.
  Claude Code writes a reply once per content block, a moved project leaves
  a copy of every transcript behind, and a forked subagent repeats its
  parent's replies; each still counts once, for the conversation that ran it.
  Ion rewrites a conversation file whole, so a changed file is read again
  and its replies counted by entry id. A conversation cut from or forked off
  another counts only its own replies. From 2026-09-05 Ion copies the usage
  of a run it handed to Claude Code onto its own replies; Claude Code's
  transcript has that run, so an Ion reply from then on counts only with the
  stop reason Ion's own API runs stamp.
- **Whole minutes.** Every post carries a minute's full totals and replaces
  what tally had, so a post sent twice stores once. The shipper saves its
  place (`SHIP_STATE`, default `~/Library/Application Support/tally/ship.json`
  on a Mac) only after tally took everything; a failed post is sent again.
- **The machine is the token's.** `SHIP_TOKENS` holds each machine's token
  as a sha256, so the list can live in git. The token itself stays on the
  machine (`TALLY_TOKEN_FILE`).
- **Only the gateway's work, on a route.** With `SHIP_ROUTE` set to an AI
  gateway's host, the shipper posts only replies that gateway served: an
  Ion reply whose model names a provider (`provider/model`) whose base URL
  in Ion's `engine.json` is that host, and a Claude Code reply (a bare
  model) when Claude Code sends its requests to that host:
  `ANTHROPIC_BASE_URL` in the `env` of its managed settings (the macOS
  preference profile, or `managed-settings.json`), then the person's own
  `settings.local.json` and `settings.json`, then the shipper's own
  environment. Claude Code signed in to its own subscription, and a bare
  model Ion handed to such a CLI, stay on the machine, titles included.
  Two things leave whatever the route, since a catalog counts them and
  neither says what the work was: how often a plugin's skills, commands,
  and tools were used, under a digest in place of the conversation, and the
  plugins the machine has installed (name, marketplace, version). A use of
  no plugin stays with its replies.
  A conversation and its work are posted only once one of their replies
  went through the gateway. A tally that meters an employer's gateway takes
  only the employer's work this way.
- **Starting over.** `tally ship reset` (with the shipper stopped) asks
  tally to delete everything the machine ever posted (`DELETE /v1/usage`)
  and forgets the shipper's place, so the next round posts again from the
  start under the rules it holds now.

The cost is at API list prices. Under `AGENTS_BILLING=subscription` (the
default) it is what the work would have cost, not what was paid; the plans'
prices are the `ai` category. Under `AGENTS_BILLING=api` the work is billed
by the token, so it is spend: the last 30 days of it join the `ai`
category.

The board's Agent work view opens on a sentence: this month's work at API
prices, its multiple of the plans, last month, and where this month ends at
its pace. Then the last 13 weeks: a square a day, shaded by the day's cost
in four even groups of the active days, the busiest day marked, beside each
month's total (this month with its pace) and a sentence of agent hours and
active days. Picking a square opens that day below. `GET /v1/agents/days?from=2026-08-09&to=2026-11-04&tz=America/Chicago`
is the same as JSON, at most 400 days.

Who sees what:

| Who | Sees |
| --- | --- |
| Everyone the board admits | The totals, the 13 weeks, and cost by model |
| `DETAIL_ROLE` | Also spend by user: each machine's cost, tokens, agent hours, and works, named by its owner from `MACHINE_OWNERS` |
| `ADMIN_ROLE` | Also the work itself: each work and conversation, its title, project, timeline, and child agents |

The day view shows one day in the viewer's zone: the estimate, agent run
time (agents working at once each count), the token mix, each work on a
timeline of lead and child activity zoomed to the hours with work, and the
cost by user and by model.
`GET /v1/agents?day=2026-11-04&tz=America/Chicago` is the same as JSON.

Below it, each provider's subscription pool: every account's weekly (or
monthly) limit added up, each account 100%, recorded from the
`subscriptions` collector's metrics in Prometheus and projected seven days
ahead at the last three hours' burn, with each account refilling at its
reset. `GET /v1/pools` is the same as JSON.

```bash
make install-ship TALLY_URL=https://tally.example.com   # this Mac: build into ~/.local/bin, (re)start the launch agent tally.ship (SHIP_LABEL)
make install-ship TALLY_URL=https://tally.example.com SHIP_ROUTE=ai.example.com   # an employer's tally: only its gateway's work
tally ship once     # one round by hand (TALLY_URL, TALLY_TOKEN_FILE)
tally ship reset    # withdraw this machine's posts and start over (stop the launch agent first)
```

To add a machine: make a token (`openssl rand -base64 32`), put it in
`~/.config/tally/ship-token` there, add `name=<its sha256>` to
`SHIP_TOKENS`, and install the shipper.

### A managed fleet

A fleet installs the shipper silently and enrolls itself; nobody signs in
and nobody keeps a list. Each release publishes the shipper for macOS and
Windows as one signed artifact, `ghcr.io/orchard-suite/tally:ship-<version>`
(`tally-darwin-arm64`, `tally-darwin-amd64`, `tally-windows-amd64.exe`, and
`SHA256SUMS`), signed by this repository's release workflow, so an estate's
forge can verify it and serve it. A script the fleet's management runs on
each machine installs it, makes the machine's token there (kept on the
machine, readable by its user alone), starts the shipper, and prints one
line, `tally-ship=<sha256 of the token>`. With `SHIP_INTUNE` set to those
scripts (`mac:<shell script id>`, `windows:<remediation id>`), the `intune`
collector reads each machine's last run of them through Microsoft Graph,
every `SHIP_INTUNE_INTERVAL` (15m), and takes posts from every machine they
name, by its Intune device name in lowercase. The Azure identity needs
Graph's `DeviceManagementScripts.Read.All` and
`DeviceManagementManagedDevices.Read.All`. A machine Intune stops managing
stops posting; a token two machines report is neither's; `SHIP_TOKENS`
wins over Intune for a hash it names.

### Billed, measured, and seen

Three lenses on AI, each the truth for one thing.

- **Billed** is the bill, from the `billed` collector: the truth for what
  was paid, a month at a time, after the month closes. When the bill is
  read, cloud is the bill's usage less its AI, AI is the bill's AI, and the
  header shows the newest billed months. Nothing below it changes a billed
  figure.
- **Measured** is the AI gateway, from the `gateway` collector: the truth
  for what was used, because every call through it is counted, by every
  key. Its tokens are priced at the same API prices as agent work.
- **Seen** is what the machines' shippers posted: detail on top of the
  gateway (which conversation, which work), never a total. Fewer shipped
  tokens than the gateway measured is work no shipper saw. More is a
  finding, shown and never corrected.

The gateway is compared with the bill's model part when the pipeline
writes one, since the rest of AI (an assistant product billed apart, for example) makes no gateway call,
else with all of AI. The difference is the gap, shown, never spread.
Tokens with no model, or no price for it, are counted and never priced.

The Statement's AI section shows each month's three lenses, then the
newest billed month taken apart (billed, measured, the gap, and how much
the machines saw), its models, and its AI by program. Without the gateway
it shows each billed month beside what the machines collected.
`GET /v1/costs` carries it as `reconciliation`.

What each gateway key used in a month, and its share of the bill's basis in
proportion to what its calls cost at API prices, is
`GET /v1/gateway/keys?month=2026-10`. A key names a person or an app, so
only `DETAIL_ROLE` sees it, on the board and on the wire.

### Cost on the catalog

With `ORRERY_URL`, `ORRERY_TOKEN` (an Orrery source token), and `ORRERY_CLUSTER` set, and the
`kubernetes` and `power` collectors on, the `catalog` writer builds the
estate every `CATALOG_INTERVAL` (15m) and writes each namespace's
`cost_per_month` (a number, USD) and `cost_updated` (a datetime) onto its
`k8s_namespace` entity, keyed `<ORRERY_CLUSTER>/<namespace>`. It first offers the
two fields on the `k8s_namespace` blueprint; the hub adds only the fields
the blueprint lacks. Every write is an incremental run of the scope `tally`
in enrich mode, so tally adds fields to namespaces the hub already has and
never creates one or keeps one alive. The source the token belongs to must
be registered for `k8s_namespace`.

A round whose inputs are stale writes nothing and fails: when `kubernetes`
or `power` has not finished a round, or its last round failed, or no
cluster host's power was read. A failed read is never written as a zero.

### Use on the catalog

With `ORRERY_USES`, `ORRERY_URL`, `ORRERY_TOKEN` (a source token, to
write), and `ORRERY_READ_TOKEN` (to list), and the `agents` collector on,
the `uses` writer writes, every `CATALOG_INTERVAL`, how much each plugin and
each plugin skill was used over the last 30 days onto its entity:
`uses_30d` (times used), `computers_30d` (on how many shipping machines),
`installed_computers` (on how many machines that still ship it is
installed), and `usage_updated`. `ORRERY_USES` names the blueprints and how each keys
an entity, such as
`agent_plugin=claude-code/{plugin},agent_skill=claude-code/{plugin}/{skill}`.
A plugin's use is every use of its skills, commands, and MCP tools; a
skill's is the times an agent loaded it and a person typed it. It offers the
three fields on each blueprint first, then writes every entity of the
template's shape the hub has, in enrich mode as the scope `tally-uses`: one
nothing used reads zero, and an entity of another shape is left alone.
`GET /v1/agents/uses?days=30` lists every skill, command, and tool used, and
`tally.agents.uses` counts this month's by kind, plugin, and name.
Each hub request is counted in `tally.upstream.requests` with
`upstream="orrery"`, and each round in `tally.collector.run` with
`collector="catalog"`. Without the settings the writer is off and says so
once in the log.

Every collector runs on its own interval (`K8S_INTERVAL` 15m,
`POWER_INTERVAL` 1m, `AZURE_INTERVAL` 6h, `MACHINES_INTERVAL` 15m,
`ADVISOR_INTERVAL` 6h, `AI_INTERVAL` 1h,
`USAGE_PROBE_EVERY` 3m, `AGENTS_INTERVAL` 5m) and counts every round in `tally.collector.run` by
`collector` and `outcome`, with `tally.collector.last_success` in unix
seconds and `tally.collector.interval`, its interval in seconds, both by
`collector`, so an alert can say "no good round in two intervals". A failed
round is tried again sooner than its interval: after 15 minutes or a quarter
of the interval, whichever is shorter, doubling on each failure in a row up
to half the interval, and back to the interval after a round that works.
Each retry is logged as `collector retry scheduled` with its wait. Every paid or rate-limited upstream is counted in
`tally.upstream.requests` by `upstream` and `outcome` (`ok`, `refused`,
`failed`).

### The right-sizing rule

A workload is one a person sizes: a deployment, statefulset, daemonset, a
CronJob with its Jobs folded in, an Actions Runner Controller runner set
with its runners folded in, or a static pod. Only workloads that run now
are judged; a CronJob between runs counts while it exists, a runner set
while it ran in the window. Sizes are one pod's, per container, as the
manifest says them, and use is the container's busiest pod's.

CPU is sized to its p95: a busy moment past it only slows the container.
Memory is sized to its peak: past its limit the container is killed. The
request that fits is that use plus `K8S_HEADROOM` (0.3), never under 10m
or 32 MiB, rounded up to 5m or 16 MiB. Each container resource gets one
verdict, the most urgent first:

- `risk`: in trouble now. Memory was killed for running out, or peaked at
  90% of its limit; or a CPU limit held it back in a quarter of its
  periods. The advice raises the limit to 1.5x the larger of the limit and
  the use, and never shrinks the request.
- `under`: the request is under 0.8x the use.
- `unset`: it uses the resource with no request.
- `over`: the request is over 2x the use and the size that fits is smaller.

An advised request over its limit raises the limit with it. A workload's
change frees or adds its containers' differences times its pods now, and
its share is that part of the cluster's allocatable room, half by CPU and
half by memory, the split that prices namespaces. Workloads in a
`K8S_MANAGED` namespace (`kube-system,aks-*`) and static pods are judged
but kept apart: the platform sizes them.

The list holds more than workloads:

- `machines`: a server's own size. CPU is judged at p95 and memory at its
  peak. It is `over` when the size is more than 2x its use and the size
  that fits (the use plus `K8S_HEADROOM`, whole cores for CPU) is
  smaller; `under` when the use reaches 90% of the size. A Kubernetes
  node's size that fits never drops under what its pods ask for and what
  it holds back from them. A server with no core count is judged in
  percent and names no size.
- `advisor`: Azure Advisor decides. Each row shows the size now, CPU and
  memory at p95, the size it advises or a shutdown, and the saving a
  year. Only open advice is listed: advice Advisor has closed, or a person
  dismissed, postponed, or suppressed in Advisor, is left out, so saying
  no to one VM happens in Advisor. Tally names the kind of change by the
  sizes' families: `shrink` (a smaller size in the same family),
  `burstable` (a step down to the B family), `family` (another family,
  such as D to E), or `shutdown`. `ADVISOR_COUNT` names the kinds whose
  savings count; the rest show as waiting on a decision, with their
  savings beside the total, not in it.

## The board

One page at the session port, phone-first, light and dark, behind the
front door, with two views. On a desktop a rail holds the view
switch and an index of the open view's sections with each one's figure; on
a phone the rail is a top bar with a sections menu.

**Statement** (`#estate`) reads the month like a statement. A sentence
gives the figure the server heads with (the rate a month, this month so far
and last month, or the newest billed months), the biggest category's share,
and what the agents did on the AI money last month; a second line says
whether the run rate moved over the trend's days, what is not counted, and
the work's multiple of its cost. A red line names a pool that runs dry in
the next two days and the workloads in trouble now. One proportional bar
splits the month by category, each key with its share, its change, and its
trend line. Coming up lists the next seven days: accounts and pools that
run out, limits that reset, and what is in trouble now. Then a section
per category, biggest first, with Capacity right after the first: each
section a sentence and line items whose bars are shares of the section's
total, long tails folded behind their sum. AI plans adds what the plans
bought (agent work a month at API prices against the plans' cost, this
month's pace as a ghost bar) and the runway, each pool's limit a day back
and seven days ahead. Hosted CI sets the 30-day bill beside the last
window's pace, split by repository. Capacity sets what the cluster asks now
against what it would ask after both changes and the room, then the
workloads by verdict, each opening to a bar per container resource (use
from p95 to peak, the request, the limit, and the size advised, on one
scale per resource) and the manifest's resources block to copy; then the
servers. Azure Advisor's VMs sit beside Cloud when Cloud shows. Collectors
close the page. `GET /v1/costs` is the same as JSON.

**Agent work** (`#agents`) is described above. A section shows only where
the collector it needs runs, and a category with no source is left out, so
a deployment never shows what it does not collect. The
page's telemetry goes through `POST /api/telemetry` as the Faro app
`tally-web`. The front-door gate, the browser relay, the JSON logs, OTLP,
`tally.build_info`, and the process metrics come from platformkit
(`github.com/orchard-suite/platformkit`).

## Settings

| Variable | Default | What |
| --- | --- | --- |
| `PROMETHEUS_URL` | off | The Prometheus the `kubernetes`, `machines`, and `power` collectors and the board's trend read. An Azure Monitor workspace's query endpoint (`https://<name>.<region>.prometheus.monitor.azure.com`) is read as the Azure identity, which needs Monitoring Data Reader on the workspace |
| `SESSION_ADDR`, `API_ADDR` | `:8080`, `:8081` | The board; and the health check and the shippers' posts |
| `REQUIRED_ROLE` | empty | The bare front-door role a person needs (`Viewer` for the app role `tally.Viewer`); empty admits anyone the door signed in. A role with the app's part still on it stops tally at startup |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_SERVICE_NAME` | off, `tally` | Spans and metrics |
| `FARO_URL`, `FARO_API_KEY` | off | Where browser telemetry is relayed; without them batches are accepted and dropped |
| `TIMEZONE` | `UTC` | The IANA zone tally's days and months begin in, such as `America/Chicago`: the estate's calendar months, and the board's days and months, wherever the viewer is |
| `CLUSTER_HOSTS` | every powered host | The hosts whose power the namespaces share |
| `AZURE_COST` | off | `on` reads Cost Management with the Azure identity |
| `K8S_WINDOW`, `K8S_HEADROOM`, `K8S_INTERVAL` | `7d`, `0.3`, `15m` | The window workloads' use is read over; the headroom above use a size keeps (servers too); how often the workloads are read |
| `K8S_MANAGED` | `kube-system,aks-*` | Namespace patterns the platform sizes, comma-separated: judged, but kept apart and out of the totals |
| `MACHINES`, `MACHINES_SKIP`, `MACHINES_WINDOW` | off, empty, `7d` | `on` right-sizes servers from Prometheus; the hosts to leave out; the window their use is over |
| `ADVISOR` | off | `on` reads Azure Advisor's VM right-sizing with the Azure identity |
| `ADVISOR_COUNT` | `shrink` | The kinds of change whose savings count: any of `shrink`, `burstable`, `family`, `shutdown`, comma-separated |
| `ORRERY_URL`, `ORRERY_TOKEN` | off | The Orrery hub's machine surface and a source token, for cost on the catalog |
| `ORRERY_READ_TOKEN` | off | An Orrery access token whose role holds no permission; with `ORRERY_URL`, the `billed` collector reads the bill |
| `BILLED_INTERVAL` | `1h` | How often the bill is read |
| `GATEWAY_WORKSPACE` | off | The Log Analytics workspace id an AI gateway's token metrics land in; the `gateway` collector reads it with the Azure identity (any role that may query it, such as Reader) |
| `GATEWAY_MONTHS`, `GATEWAY_INTERVAL` | `6`, `1h` | How many whole months before this one the gateway is read over, and how often |
| `ORRERY_CLUSTER` | off | The cluster name the catalog keys namespaces under (`<cluster>/<namespace>`, as Orrery's Kubernetes pusher writes them), such as `cluster-a`; the writer is off without it |
| `CATALOG_INTERVAL` | `15m` | How often the namespace costs and the uses are written to the catalog |
| `ORRERY_USES` | off | `blueprint=template,...`: where the `uses` writer counts plugins and skills, each template naming `{plugin}`, and a skill's also `{skill}` |
| `AGENTS_DB` | off | The agent store, one SQLite file; the `agents` collector and `POST /v1/usage` are off without it |
| `SHIP_TOKENS` | empty | `machine=<sha256 hex of its token>,...`: who may post agent usage, and as which machine |
| `DETAIL_ROLE` | empty | The bare front-door role a person needs to see agent spend by user. Empty shows it to everyone the board admits. This role alone also opens the board |
| `ADMIN_ROLE` | empty | The bare front-door role a person needs to see the work itself: each work and conversation, with titles, projects, timelines, and child agents. An admin sees spend by user too. Empty shows it to everyone who sees spend by user. This role alone also opens the board |
| `AGENTS_BILLING` | `subscription` | How agent work is paid for. `subscription`: a plan pays, and its cost at API prices is what it would have cost. `api`: it is billed by the token, so the last 30 days of it join the `ai` category as spend, with this month and last month beside it |
| `MACHINE_OWNERS` | empty | `machine=owner,...`: who each machine belongs to, as the board's By user names them |
| `AGENTS_INTERVAL` | `5m` | How often the month's agent totals are exported |
| `PRICES_URL` | LiteLLM's table | Where model prices are read, once a day |
| `TALLY_URL`, `TALLY_TOKEN` or `TALLY_TOKEN_FILE` | | `tally ship`: where to post, and this machine's token |
| `CLAUDE_PROJECTS`, `ION_DIR`, `T3_STATE`, `SHIP_STATE`, `SHIP_EVERY` | `~/.claude/projects`, `~/.ion`, `~/.t3/userdata/statev2.sqlite`, the user config folder, `1m` | `tally ship`: what it reads, where it keeps its place, and how often |
| `SHIP_ROUTE` | empty | `tally ship`: an AI gateway's host, such as `ai.example.com`. Only replies that gateway served are posted; empty posts every reply |
| `SHIP_INTUNE` | off | `mac:<id>,windows:<id>`: the Intune scripts whose output names each fleet machine's token hash; the `intune` collector enrolls them |
| `SHIP_INTUNE_INTERVAL` | `15m` | How often the Intune scripts' runs are read |
| `ANTHROPIC_BASE_URL` | empty | `tally ship`: where Claude Code sends its requests, for a machine that sets it only in the shell; Claude Code's settings win over it |

Plus each collector's variables above, and usage-probe's `USAGE_PROBE_*`
(`ACCOUNTS`, `ENDPOINT`, `EVERY`, `ROTATION`, `ROTATE_AT`, `PREFER`,
`ACTIVE_SECRET`, `PIN`), unchanged.

## The subscriptions collector, from usage-probe

`internal/collect/subscriptions` is usage-probe, moved whole:
every metric name and label, every setting, the rotation, its
reads and writes of the `claude-active` secret and the `claude-pin` config
map, its tests, and its commands.

```bash
tally check                    # read every account once and print the limits; push nothing
tally login claude <email>     # sign an account in (claude, codex, or grok)
tally logout codex <email>     # forget an account
CLAUDE_ROTATION_VAULT=kv-example ./claude-rotation list   # the build accounts in Key Vault, the active one, and the pin
./claude-rotation pin ada@example.com   # use this account until it hits a limit
./claude-rotation unpin                 # let tally pick again
```

`pin` writes the account to the `claude-pin` config map, the same one an
estate's own page may write. The collector checks it every 15 seconds,
switches to it while it is usable, and empties it when the account hits a
limit. `pin` returns once Key Vault hands the account out. Every command, `list`
too, then runs `CLAUDE_ROTATION_AFTER`, so a computer that copies the active
account on a schedule takes it at once. `make install-rotation` puts the command in `~/.local/bin`; it reads
its settings (`CLAUDE_ROTATION_*`, listed at the top of the script) from
the environment or from `~/.config/tally/rotation.env`.

The image also answers to `usage-probe`, so
`kubectl exec ... usage-probe login` lines written for it keep working. The CLIs it asks are
pinned in the `Dockerfile`.

## Run it here

```bash
make check                     # gofmt, vet, every test
kubectl -n monitoring port-forward svc/prometheus 19090:9090 &
make run                       # the board on :8080 against that Prometheus
curl -H 'X-Forwarded-Email: you@example.com' localhost:8080/v1/costs
```

## Deploy

An estate deploys the image `ghcr.io/orchard-suite/tally:<version>`,
`linux/amd64`, built and signed by the Release workflow on every `feat` or
`fix` on `main`, with its own manifests, alerts, and dashboards. Verify it
with cosign against the identity
`^https://github.com/orchard-suite/tally/`.
