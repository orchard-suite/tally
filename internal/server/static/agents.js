// Agent work: what the machines' agents did, at API prices. A sentence says
// the month and what it is worth against the plans; the quarter is a square
// a day; a picked day opens below with its works on a timeline; then each
// subscription pool's limit, recorded and projected.
(function () {
  "use strict";
  var T, fmt, el, svg, say;
  var host;
  // The board's zone, as the server reports it with a day; days and months
  // begin there, wherever the viewer is.
  var tz = null;
  var day = null; // YYYY-MM-DD, or null for today
  var WEEKS = 13;
  var quarter = null, plans = 0, built = false;
  var parts = {};

  function pad(n) { return (n < 10 ? "0" : "") + n; }
  function iso(d) { return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()); }
  function today() {
    if (!tz) return iso(new Date());
    return new Intl.DateTimeFormat("en-CA", { timeZone: tz, year: "numeric", month: "2-digit", day: "2-digit" }).format(new Date());
  }
  function shift(s, n) { var p = s.split("-"); return iso(new Date(+p[0], +p[1] - 1, +p[2] + n)); }
  function local(s) { var p = s.split("-"); return new Date(+p[0], +p[1] - 1, +p[2]); }
  function zoned(o) { if (tz) o.timeZone = tz; return o; }
  function clock(t) { return new Date(t).toLocaleTimeString("en-US", zoned({ hour: "numeric", minute: "2-digit" })); }
  function hours(h) { return h >= 10 ? Math.round(h) + "h" : h >= 1 ? h.toFixed(1) + "h" : Math.round(h * 60) + "m"; }
  function money(v, o) { return fmt.money(v, o); }
  function cap(s) { return fmt.name(s); }
  // This month's total if the rest of it runs like the days so far.
  function paceOf(v) { var d = local(today()), dim = new Date(d.getFullYear(), d.getMonth() + 1, 0).getDate(); return v / d.getDate() * dim; }
  function ordinal(n) { var s = ["th", "st", "nd", "rd"], v = n % 100; return n + (s[(v - 20) % 10] || s[v] || s[0]); }
  function tzq() { return tz ? "&tz=" + encodeURIComponent(tz) : ""; }

  // ---------- skeleton

  function build() {
    host.textContent = "";
    parts.lede = el("header", "lede-wrap");
    host.appendChild(parts.lede);

    var q = el("section", "sec");
    q.id = "quarter";
    var qh = el("div", "sec-head");
    qh.appendChild(el("h2", null, "The last 13 weeks")).id = "h-quarter";
    q.setAttribute("aria-labelledby", "h-quarter");
    qh.appendChild(el("div", "sec-amt"));
    q.appendChild(qh);
    q.appendChild(el("p", "say", "A square a day, shaded by its cost at API prices in four even groups of the active days. Pick one to open it below."));
    parts.quarter = el("div", "quarter");
    q.appendChild(parts.quarter);
    host.appendChild(q);

    var d = el("section", "sec");
    d.id = "day";
    var nav = el("div", "day-nav");
    var prev = el("button", "step", "‹");
    prev.type = "button";
    prev.setAttribute("aria-label", "The day before");
    prev.addEventListener("click", function () { pick(shift(day || today(), -1)); });
    var title = el("h2", "day-title", "Today");
    title.id = "h-day";
    d.setAttribute("aria-labelledby", "h-day");
    var next = el("button", "step", "›");
    next.type = "button";
    next.setAttribute("aria-label", "The day after");
    next.addEventListener("click", function () { pick(shift(day || today(), 1)); });
    var back = el("button", "pill", "Today");
    back.type = "button";
    back.addEventListener("click", function () { pick(today()); });
    nav.appendChild(title);
    nav.appendChild(prev);
    nav.appendChild(next);
    nav.appendChild(back);
    d.appendChild(nav);
    parts.dayTitle = title;
    parts.next = next;
    parts.back = back;
    parts.day = el("div", "day");
    d.appendChild(parts.day);
    host.appendChild(d);

    var p = el("section", "sec");
    p.id = "pools";
    p.hidden = true;
    var ph = el("div", "sec-head");
    ph.appendChild(el("h2", null, "Subscription pools")).id = "h-pools";
    p.setAttribute("aria-labelledby", "h-pools");
    p.appendChild(ph);
    p.appendChild(el("p", "say", "Every account's limit added up, each account 100%: recorded, then projected seven days at the last three hours' burn, with each account refilling at its reset."));
    parts.pools = el("div", "pools");
    p.appendChild(parts.pools);
    parts.poolSection = p;
    host.appendChild(p);
    built = true;
  }

  function show() {
    if (!built) build();
    figures();
    var started = performance.now();
    return loadDay().then(function () {
      T.metric("agents_ready", { ready_ms: Math.round(performance.now() - started) });
      return Promise.all([loadQuarter(), loadPools(), T.costs().then(function (b) { plans = (b.ai || []).reduce(function (s, a) { return s + (a.cost_per_month || 0); }, 0); }, function () {})]);
    }).then(lede);
  }

  // The rail's index, with each section's figure once it is read.
  var fig = { quarter: "", day: "", pools: "", poolsTone: "" };
  function figures() {
    T.index([
      { view: "agents", id: "quarter", label: "13 weeks", value: fig.quarter },
      { view: "agents", id: "day", label: day ? "The day" : "Today", value: fig.day },
      { view: "agents", id: "pools", label: "Pools", value: fig.pools, tone: fig.poolsTone }
    ]);
  }

  // ---------- the lede

  function lede() {
    var L = parts.lede;
    L.textContent = "";
    if (!quarter) return;
    var t = today(), thisMonth = t.slice(0, 7), lastMonth = iso(new Date(local(t).getFullYear(), local(t).getMonth() - 1, 1)).slice(0, 7);
    var by = {};
    quarter.forEach(function (d) { var k = d.day.slice(0, 7); by[k] = (by[k] || 0) + d.cost; });
    var now = by[thisMonth] || 0, before = by[lastMonth] || 0;
    var p = ["Agents did ", [money(now, { whole: true }), "fig c-ai"], " of work this month at API prices"];
    if (plans > 0 && now > 0) p.push(", ", [fmt.times(now / plans), "fig c-ai"], " what the plans cost.");
    else p.push(".");
    L.appendChild(say("h1", "lede", p));
    var dom = local(t).getDate(), dim = new Date(local(t).getFullYear(), local(t).getMonth() + 1, 0).getDate();
    var sub = [];
    if (before) sub.push(fmt.month(lastMonth) + " came to " + money(before, { whole: true }) + (plans > 0 ? ", " + fmt.times(before / plans) + " the plans" : "") + ".");
    if (now && dom < dim) sub.push("At this pace, " + fmt.month(thisMonth).split(" ")[0] + " ends near " + money(now / dom * dim, { whole: true }) + ".");
    if (sub.length) L.appendChild(el("p", "lede-sub", sub.join(" ")));
  }

  // ---------- the quarter

  function quarterRange() {
    var now = local(today());
    var start = new Date(now.getFullYear(), now.getMonth(), now.getDate() - now.getDay() - (WEEKS - 1) * 7);
    return { from: iso(start), to: today() };
  }

  function loadQuarter() {
    var r = quarterRange();
    return T.getJSON("/v1/agents/days?from=" + r.from + "&to=" + r.to + tzq())
      .then(function (days) {
        quarter = days;
        fig.quarter = money(days.reduce(function (s, d) { return s + d.cost; }, 0), { whole: true });
        renderQuarter();
        if (T.current() === "agents") figures();
      })
      .catch(function (err) {
        parts.quarter.textContent = "";
        parts.quarter.appendChild(el("p", "empty", "The last 13 weeks could not be read."));
        T.error(err);
      });
  }

  // levels splits the active days' costs into four even groups, so one huge
  // day does not wash the rest out.
  function levels(days) {
    var vs = days.map(function (d) { return d.cost; }).filter(function (v) { return v > 0; }).sort(function (a, b) { return a - b; });
    if (!vs.length) return function () { return 0; };
    var q = [0.25, 0.5, 0.75].map(function (f) { return vs[Math.min(vs.length - 1, Math.floor(f * vs.length))]; });
    return function (v) { return v <= 0 ? 0 : v <= q[0] ? 1 : v <= q[1] ? 2 : v <= q[2] ? 3 : 4; };
  }

  function renderQuarter() {
    var days = quarter, level = levels(days), shown = day || today(), t = today();
    var top = Math.max.apply(null, days.map(function (d) { return d.cost; }).concat([0]));
    var box = parts.quarter;
    box.textContent = "";
    var heat = el("div", "heat");
    heat.setAttribute("role", "grid");
    heat.setAttribute("aria-label", "Agent work a day");
    heat.style.setProperty("--weeks", WEEKS);
    heat.appendChild(el("span"));
    var last = -1;
    for (var w = 0; w < WEEKS; w++) {
      var first = days[w * 7];
      var m = first ? +first.day.split("-")[1] : -1;
      var label = el("span", "hm-month", m !== last && first ? local(first.day).toLocaleDateString("en-US", { month: "short" }) : "");
      label.style.gridColumn = String(w + 2);
      label.style.gridRow = "1";
      heat.appendChild(label);
      last = m;
    }
    ["", "Mon", "", "Wed", "", "Fri", ""].forEach(function (n, i) {
      var wd = el("span", "hm-wd");
      wd.appendChild(el("span", "long", n));
      wd.appendChild(el("span", "short", n.charAt(0)));
      wd.style.gridColumn = "1";
      wd.style.gridRow = String(i + 2);
      heat.appendChild(wd);
    });
    for (var i = 0; i < WEEKS * 7; i++) {
      var d = days[i], cell;
      if (!d) cell = el("span", "cell future");
      else {
        cell = el("button", "cell l" + level(d.cost) + (d.day === t ? " today" : "") + (d.day === shown ? " selected" : "") + (d.cost > 0 && d.cost === top ? " busiest" : ""));
        cell.type = "button";
        var text = local(d.day).toLocaleDateString("en-US", { weekday: "short", month: "short", day: "numeric" }) + ": " + (d.total ? money(d.cost) + ", " + fmt.tokens(d.total) + " tokens, " + hours(d.agent_hours) + " of agent time, " + fmt.plural(d.works, "work") : "no agent work");
        cell.title = text;
        cell.setAttribute("aria-label", text);
        cell.dataset.day = d.day;
        cell.addEventListener("click", function (e) { pick(e.currentTarget.dataset.day); document.getElementById("day").scrollIntoView({ block: "start", behavior: "smooth" }); });
      }
      cell.style.gridColumn = String(Math.floor(i / 7) + 2);
      cell.style.gridRow = String((i % 7) + 2);
      heat.appendChild(cell);
    }
    var left = el("div", "hm");
    left.appendChild(heat);
    var lg = el("div", "legend hm-legend");
    lg.appendChild(el("span", null, "Less"));
    for (var l = 0; l < 5; l++) lg.appendChild(el("i", "cell l" + l));
    lg.appendChild(el("span", null, "More"));
    var sel = el("span", "hm-key"); sel.appendChild(el("i", "cell l2 selected-key")); sel.appendChild(document.createTextNode("open day")); lg.appendChild(sel);
    var bu = el("span", "hm-key"); bu.appendChild(el("i", "cell l4 busiest")); bu.appendChild(document.createTextNode("busiest")); lg.appendChild(bu);
    left.appendChild(lg);
    box.appendChild(left);

    // Beside it: the months, the hours, the busiest day.
    var side = el("div", "hm-side");
    var by = {}, order = [];
    days.forEach(function (d) { var k = d.day.slice(0, 7); if (!(k in by)) { by[k] = 0; order.push(k); } by[k] += d.cost; });
    var most = Math.max.apply(null, order.map(function (k) { return k === t.slice(0, 7) ? paceOf(by[k]) : by[k]; }).concat([1]));
    var months = el("div", "hm-months");
    order.forEach(function (k) {
      var r = el("div", "hm-mrow" + (k === t.slice(0, 7) ? " partial" : ""));
      var firstDay = days.filter(function (d) { return d.day.slice(0, 7) === k; })[0].day;
      var mk = el("span", "hm-mk", fmt.monthShort(k) + (k === t.slice(0, 7) ? ", so far" : firstDay.slice(8) !== "01" ? ", from the " + ordinal(+firstDay.slice(8)) : ""));
      if (k === t.slice(0, 7) && paceOf(by[k]) > by[k]) mk.appendChild(el("span", "hm-pace", "pace " + money(paceOf(by[k]), { whole: true })));
      r.appendChild(mk);
      var b = el("span", "hm-mbar");
      if (k === t.slice(0, 7) && paceOf(by[k]) > by[k]) {
        var gh = el("i", "ghost");
        gh.style.width = Math.min(100, paceOf(by[k]) / most * 100).toFixed(1) + "%";
        gh.title = "on pace for " + money(paceOf(by[k]), { whole: true });
        b.appendChild(gh);
      }
      var f = el("i");
      f.style.width = (by[k] / most * 100).toFixed(1) + "%";
      b.appendChild(f);
      r.appendChild(b);
      r.appendChild(el("span", "hm-mv", money(by[k], { whole: true })));
      months.appendChild(r);
    });
    side.appendChild(months);
    var active = days.filter(function (d) { return d.total > 0; });
    var hrs = active.reduce(function (s, d) { return s + d.agent_hours; }, 0);
    var busiest = active.slice().sort(function (a, b) { return b.cost - a.cost; })[0];
    // The quarter in a sentence; the busiest day opens from it.
    var said = el("p", "hm-said");
    said.appendChild(document.createTextNode(Math.round(hrs) + " hours of agent time on " + active.length + " of " + days.length + " days" + (active.length ? ", " + hours(hrs / active.length) + " an active day" : "") + "."));
    if (busiest) {
      said.appendChild(document.createTextNode(" The busiest was "));
      var bb = el("button", "linkish", local(busiest.day).toLocaleDateString("en-US", { weekday: "short", month: "short", day: "numeric" }) + ", " + money(busiest.cost, { whole: true }));
      bb.type = "button";
      bb.addEventListener("click", function () { pick(busiest.day); document.getElementById("day").scrollIntoView({ block: "start", behavior: "smooth" }); });
      said.appendChild(bb);
      said.appendChild(document.createTextNode(", " + fmt.tokens(busiest.total) + " tokens."));
    }
    side.appendChild(said);
    box.appendChild(side);
  }

  // ---------- the day

  function pick(d) {
    day = d >= today() ? null : d;
    if (quarter) renderQuarter();
    loadDay();
  }

  function loadDay() {
    return T.getJSON("/v1/agents?" + (tz ? "tz=" + encodeURIComponent(tz) : "") + (day ? "&day=" + day : ""))
      .then(renderDay)
      .catch(function (err) {
        parts.day.textContent = "";
        parts.day.appendChild(el("p", "empty", err.status === 404 ? "The agents collector is off. Set AGENTS_DB." : "The agent usage could not be read."));
        if (err.status !== 404) T.error(err);
      });
  }

  function renderDay(rep) {
    if (rep.zone) tz = rep.zone;
    var shown = day || today();
    var title = new Date(rep.from).toLocaleDateString("en-US", zoned({ weekday: "long", month: "short", day: "numeric" }));
    parts.dayTitle.textContent = (shown === today() ? "Today, " : "") + title;
    parts.next.disabled = shown >= today();
    parts.back.hidden = shown === today();
    var box = parts.day;
    box.textContent = "";
    var t = rep.totals;
    fig.day = money(t.cost, { whole: t.cost >= 100 });
    if (T.current() === "agents") figures();
    if (!t.total) { box.appendChild(el("p", "empty", "No agent work on this day.")); return; }

    var billed = rep.billing === "api";
    var top = (rep.models || [])[0];
    box.appendChild(say("p", "day-lede", [[money(t.cost), "fig"], " of work " + (billed ? "billed by the token" : "at API prices") + ", ", [hours(t.agent_hours), "fig"], " of agent time" + (rep.works.length ? " on " + fmt.plural(rep.works.length, "task") : "") + "."]));
    var bits = [fmt.tokens(t.total) + " tokens, " + fmt.pct(t.cache_read / t.total) + " of them read back from cache."];
    if (t.first) bits.push("Agent time counts each agent working at once. From " + clock(t.first) + " to " + clock(t.last) + ", at least one was working for " + hours(t.active_hours) + ".");
    if (top && top.share > 0.5) bits.push(fmt.pct(top.share) + " of the cost is " + top.model.replace(/-/g, "\u2011") + ".");
    if (bits.length) box.appendChild(el("p", "say", bits.join(" ")));

    // The tokens, by kind.
    var fresh = t.input + t.cache_write_5m + t.cache_write_1h;
    var mix = el("div", "mix");
    mix.setAttribute("role", "img");
    mix.setAttribute("aria-label", fmt.tokens(t.cache_read) + " cache reads, " + fmt.tokens(fresh) + " new input, " + fmt.tokens(t.output) + " output");
    [["cache", t.cache_read], ["fresh", fresh], ["out", t.output]].forEach(function (p) {
      var seg = el("i", "seg " + p[0]);
      seg.style.width = (p[1] / t.total * 100).toFixed(2) + "%";
      mix.appendChild(seg);
    });
    box.appendChild(mix);
    var lg = el("div", "legend");
    [["cache", fmt.tokens(t.cache_read) + " cache reads"], ["fresh", fmt.tokens(fresh) + " new input"], ["out", fmt.tokens(t.output) + " output"]].forEach(function (p) {
      var sp = el("span");
      sp.appendChild(el("i", "sw seg " + p[0]));
      sp.appendChild(document.createTextNode(p[1]));
      lg.appendChild(sp);
    });
    box.appendChild(lg);

    // The works on the day's clock. Admins only, as the server decides.
    if (rep.shows_works && rep.works.length) worksBlock(box, rep);

    if (rep.shows_users && rep.machines.length) {
      var h = el("div", "sub-head");
      var named = rep.machines.some(function (m) { return m.owner; });
      h.appendChild(el("h3", null, named ? "By user" : "By machine"));
      h.appendChild(el("span", "note", named ? "each person's machines, at API prices" : "each machine's agent work, at API prices"));
      box.appendChild(h);
      var uw = rep.machines.some(function (m) { return m.cost >= 100; });
      rep.machines.forEach(function (m) {
        line(box, m.owner || m.machine, (m.owner ? m.machine + " · " : "") + fmt.plural(m.works, "task") + " · " + hours(m.agent_hours), m.cost / (t.cost || 1), money(m.cost, { whole: uw }), fmt.tokens(m.total) + " tokens");
      });
    }

    var mh = el("div", "sub-head");
    mh.appendChild(el("h3", null, "By model"));
    if (rep.unpriced && rep.unpriced.length) mh.appendChild(el("span", "note", "no price for " + rep.unpriced.join(", ")));
    box.appendChild(mh);
    var mw = rep.models.some(function (m) { return m.cost >= 100; });
    rep.models.forEach(function (m) {
      line(box, m.model, fmt.tokens(m.total) + " tokens", m.priced ? m.share : 0, m.priced ? money(m.cost, { whole: mw }) : "unpriced", m.priced ? fmt.pct(m.share) + " of the day" : null);
    });
    var c = rep.children;
    if (rep.shows_works && c && c.conversations) box.appendChild(el("p", "note", "Child agents: " + fmt.plural(c.conversations, "conversation") + ", " + fmt.tokens(c.total) + " tokens, " + money(c.cost) + ", counted inside their works above."));
  }

  function line(parent, name, sub, share, value, vsub) {
    var r = el("div", "li");
    var n = el("div", "li-name");
    n.appendChild(el("div", "li-k", name));
    if (sub) n.appendChild(el("div", "li-sub", sub));
    r.appendChild(n);
    var bar = el("div", "li-bar");
    var f = el("i");
    f.style.width = (share > 0 ? Math.max(0.6, share * 100) : 0).toFixed(2) + "%";
    f.style.background = "var(--c-ai)";
    bar.appendChild(f);
    r.appendChild(bar);
    var v = el("div", "li-v");
    v.appendChild(el("div", "li-amt", value));
    if (vsub) v.appendChild(el("div", "li-sub", vsub));
    r.appendChild(v);
    parent.appendChild(r);
  }

  function worksBlock(box, rep) {
    var h = el("div", "sub-head");
    h.appendChild(el("h3", null, "Tasks"));
    var lg = el("span", "legend inline");
    [["lead", "lead agent working"], ["child", "child agents working"], ["span", "first to last"]].forEach(function (k) {
      var sp = el("span");
      sp.appendChild(el("i", "sw lg-" + k[0]));
      sp.appendChild(document.createTextNode(k[1]));
      lg.appendChild(sp);
    });
    h.appendChild(lg);
    box.appendChild(h);
    // The axis spans the hours with work, whole hours, at least six.
    var full = (new Date(rep.to) - new Date(rep.from)) / 60000;
    var lo = full, hi = 0;
    rep.works.forEach(function (w) {
      lo = Math.min(lo, (new Date(w.first) - new Date(rep.from)) / 60000);
      hi = Math.max(hi, (new Date(w.last) - new Date(rep.from)) / 60000);
    });
    lo = Math.max(0, Math.floor(lo / 60) * 60);
    hi = Math.min(full, Math.ceil(hi / 60) * 60);
    if (hi - lo < 360) { var pad = (360 - (hi - lo)) / 2; lo = Math.max(0, lo - pad); hi = Math.min(full, lo + 360); lo = Math.max(0, hi - 360); lo = Math.floor(lo / 60) * 60; }
    var span = { lo: lo, hi: hi };
    var total = hi - lo;
    var machines = {};
    rep.works.forEach(function (w) { machines[w.machine] = true; });
    var several = Object.keys(machines).length > 1;
    var list = el("div", "works");
    var axis = el("div", "wk-row wk-axis");
    axis.setAttribute("aria-hidden", "true");
    axis.appendChild(el("span"));
    var ticks = el("div", "wk-ticks");
    var step = total > 12 * 60 ? 180 : total > 6 * 60 ? 120 : 60;
    for (var m = Math.ceil(lo / step) * step; m <= hi; m += step) {
      var tk = el("span", null, new Date(new Date(rep.from).getTime() + m * 60000).toLocaleTimeString("en-US", zoned({ hour: "numeric" })));
      tk.style.left = ((m - lo) / total * 100).toFixed(2) + "%";
      if (m === lo) tk.className = "first";
      else if (m === hi) tk.className = "last";
      ticks.appendChild(tk);
    }
    ticks.style.setProperty("--step", (step / total * 100).toFixed(3) + "%");
    ticks.style.setProperty("--offset", ((Math.ceil(lo / step) * step - lo) / total * 100).toFixed(3) + "%");
    axis.appendChild(ticks);
    axis.appendChild(el("span"));
    list.appendChild(axis);
    rep.works.forEach(function (w) {
      var row = el("article", "wk-row");
      var who = el("div", "wk-who");
      who.appendChild(el("div", "wk-title", w.title));
      var meta = [w.project, several ? w.machine : "", clock(w.first) + " to " + clock(w.last), hours(w.agent_hours)];
      if (w.children) meta.push(fmt.plural(w.children, "child", "children"));

      who.appendChild(el("div", "li-sub", meta.filter(Boolean).join("\u00a0· ")));
      row.appendChild(who);
      var lanes = el("div", "wk-lanes");
      lanes.setAttribute("aria-hidden", "true");
      lanes.style.setProperty("--step", ticks.style.getPropertyValue("--step"));
      lanes.style.setProperty("--offset", ticks.style.getPropertyValue("--offset"));
      var start = (new Date(w.first) - new Date(rep.from)) / 60000 - span.lo, end = (new Date(w.last) - new Date(rep.from)) / 60000 - span.lo + 1;
      var faint = el("span", "wk-span");
      place(faint, start, end, total);
      lanes.appendChild(faint);
      [["lead", w.lead], ["child", w.child]].forEach(function (l) {
        var lane = el("div", "wk-lane " + l[0]);
        (l[1] || []).forEach(function (s) { var b = el("span", "run"); place(b, s.s - span.lo, s.e - span.lo, total); lane.appendChild(b); });
        lanes.appendChild(lane);
      });
      row.appendChild(lanes);
      var v = el("div", "wk-cost");
      v.appendChild(el("div", "li-amt", money(w.cost)));
      if (w.ongoing) v.appendChild(el("span", "chip live", "ongoing"));
      row.appendChild(v);
      list.appendChild(row);
    });
    box.appendChild(list);
  }

  function place(node, s, e, total) {
    node.style.left = (Math.max(0, s) / total * 100).toFixed(3) + "%";
    node.style.width = Math.max(0.25, (Math.min(total, e) - Math.max(0, s)) / total * 100).toFixed(3) + "%";
  }

  // ---------- pools

  function loadPools() {
    return T.getJSON("/v1/pools").then(function (pools) {
      parts.poolSection.hidden = false;
      var box = parts.pools;
      box.textContent = "";
      if (!pools.length) box.appendChild(el("p", "empty", "No subscription limits are recorded. Set PROMETHEUS_URL and USAGE_PROBE_ACCOUNTS."));
      pools.forEach(function (p) { box.appendChild(pool(p)); });
      if (pools.length) box.appendChild(T.poolLegend(pools));
      var low = pools.filter(function (p) { return p.remaining / p.capacity < 0.2; }).length;
      fig.pools = low ? low + " low" : pools.length + " fine";
      fig.poolsTone = low ? "bad" : "";
      if (T.current() === "agents") figures();
    }).catch(function (err) {
      if (err.status === 404) return;
      parts.poolSection.hidden = false;
      parts.pools.textContent = "";
      parts.pools.appendChild(el("p", "empty", "The subscription pools could not be read."));
      T.error(err);
    });
  }

  function pool(p) {
    var card = el("article", "pool");
    var head = el("div", "pool-head");
    var name = el("div");
    name.appendChild(el("h3", null, cap(p.provider) + (p.scope ? " " + p.scope : "")));
    name.appendChild(el("div", "li-sub", fmt.plural(p.accounts, "account") + " · " + p.window + " limits"));
    head.appendChild(name);
    var low = p.remaining / p.capacity < 0.2;
    var big = el("div", "pool-big");
    big.appendChild(el("b", low ? "bad" : null, fmt.pct(p.remaining / p.capacity)));
    big.appendChild(el("span", null, " left"));
    head.appendChild(big);
    card.appendChild(head);
    var said = [];
    said.push("Burning " + (p.burn / p.capacity * 100).toFixed(1) + "% of the pool an hour" + (p.accounts > 1 ? ", about " + (p.remaining / 100).toFixed(1) + " accounts' worth left" : "") + ".");
    var end = p.projected.length ? p.projected[p.projected.length - 1].t : null;
    if (p.empty) said.push("The pool runs dry " + fmt.when(p.empty) + ", in " + fmt.until(p.empty) + ".");
    else if (end) said.push("It lasts past " + new Date(end).toLocaleDateString("en-US", { weekday: "long" }) + ".");
    if (p.first_empty && !(p.empty && p.first_empty.at === p.empty)) said.push(new Date(p.first_empty.at) <= Date.now() ? p.first_empty.account.split("@")[0] + " is out now." : (p.accounts > 1 ? "The first account, " + p.first_empty.account.split("@")[0] + ", runs out " : "It runs out ") + fmt.when(p.first_empty.at) + ".");
    if (p.next_reset) said.push("Next reset " + fmt.when(p.next_reset.at) + ", when " + (Math.round(p.next_reset.gain / 100) === 1 ? "an account refills" : Math.round(p.next_reset.gain / 100) + " accounts refill") + ".");
    card.appendChild(el("p", "say", said.join(" ")));
    card.appendChild(T.poolChart(p));
    return card;
  }

  document.addEventListener("DOMContentLoaded", function () {
    T = window.tally; fmt = T.fmt; el = T.el; svg = T.svg; say = T.say;
    host = document.getElementById("view-agents");
    T.view("agents", { show: show });
    setInterval(function () {
      if (T.current() === "agents" && !day && !document.hidden) { loadDay(); loadQuarter(); loadPools(); }
    }, 60 * 1000);
  });
})();
