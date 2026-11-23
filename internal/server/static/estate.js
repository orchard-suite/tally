// The Statement: the estate's month read like a statement. A sentence says
// what it costs, where it goes, and whether it moves; one bar splits it;
// what comes up next sits under it; then each category's line items, the
// cluster's room, and the collectors.
(function () {
  "use strict";
  var T, fmt, el, svg, say;
  var NAMES = { ai: "AI plans", power: "Power", cloud: "Cloud", ci: "Hosted CI" };
  var LOWER = { ai: "AI plans", power: "power", cloud: "cloud", ci: "hosted CI" };
  var COLORS = { ai: "var(--c-ai)", power: "var(--c-power)", cloud: "var(--c-cloud)", ci: "var(--c-ci)" };
  var host;

  function cap(s) { return fmt.name(s); }
  function handle(account) { return (account || "").split("@")[0]; }
  function today() { var d = new Date(); return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()); }
  function pad(n) { return (n < 10 ? "0" : "") + n; }
  function monthKey(offset) { var d = new Date(); d = new Date(d.getFullYear(), d.getMonth() + offset, 1); return d.getFullYear() + "-" + pad(d.getMonth() + 1); }
  // A refill as people count it: whole accounts, or a share of one.
  function refills(gain) { var n = gain / 100; return Math.abs(n - Math.round(n)) < 0.05 && n >= 0.95 ? fmt.plural(Math.round(n), "account") + " refill" + (Math.round(n) === 1 ? "s" : "") : "+" + Math.round(gain) + "% of an account"; }
  function sum(list, f) { return list.reduce(function (s, x) { return s + (f(x) || 0); }, 0); }

  function show() {
    var tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
    var soft = function (p) { return p.catch(function (e) { if (e.status !== 404) T.error(e); return null; }); };
    return Promise.all([
      T.costs(),
      soft(T.getJSON("/v1/pools")),
      soft(T.getJSON("/v1/agents/days?from=" + monthKey(-3) + "-01&to=" + today() + "&tz=" + encodeURIComponent(tz)))
    ]).then(function (r) {
      render(r[0], r[1] || [], r[2]);
      T.ready("board_ready");
    }).catch(function (err) {
      host.textContent = "";
      host.appendChild(say("h1", "lede", ["The costs could not be read."]));
      host.appendChild(el("p", "say", "The board will try again in five minutes."));
      T.error(err);
    });
  }

  // ---------- derived figures

  // Each category's change over the trend's days: now less the first point.
  function moves(b) {
    var out = {};
    (b.categories || []).forEach(function (c) {
      var t = (b.trend || {})[c.category];
      if (c.on && t && t.length > 1) out[c.category] = { delta: c.per_month - t[0].v, days: Math.round((Date.now() - new Date(t[0].t)) / 86400000) };
    });
    return out;
  }

  function agentMonths(days) {
    var m = {};
    (days || []).forEach(function (d) { var k = d.day.slice(0, 7); m[k] = (m[k] || 0) + d.cost; });
    Object.defineProperty(m, "first", { value: days && days.length ? days[0].day : "", enumerable: false });
    return m;
  }

  // What comes up in the next seven days: accounts and pools that run out,
  // limits that reset, and what is in trouble now.
  function upcoming(pools, risks) {
    var horizon = Date.now() + 7 * 86400000, byKey = {};
    function add(at, kind, name, detail) {
      var t = new Date(at).getTime();
      if (t > horizon) return;
      var key = kind + "|" + Math.round(t / 60000);
      if (byKey[key]) { byKey[key].names.push(name); if (kind === "reset") byKey[key].detail = byKey[key].detail.replace(/ refills?$/, "") + " refills in each"; return; }
      byKey[key] = { at: Math.max(t, Date.now()), kind: kind, names: [name], detail: detail };
    }
    pools.forEach(function (p) {
      var name = cap(p.provider) + (p.scope ? " " + p.scope : "") + (p.window !== "weekly" ? " " + p.window : "");
      if (p.empty) add(p.empty, "dry", name, fmt.pct(p.remaining / p.capacity) + " left, burning " + (p.burn / p.capacity * 100).toFixed(1) + "% of the pool an hour");
      if (p.first_empty && p.first_empty.at !== p.empty) add(p.first_empty.at, "account", name, handle(p.first_empty.account) + ", the first of " + p.accounts);
      if (p.next_reset) add(p.next_reset.at, "reset", name, refills(p.next_reset.gain));
    });
    var list = Object.keys(byKey).map(function (k) { return byKey[k]; });
    if (risks) list.push({ at: Date.now() - 1, kind: "risk", names: [fmt.plural(risks, "workload")], detail: "out of memory, near a limit, or throttled" });
    return list.sort(function (a, b) { return a.at - b.at; });
  }

  // ---------- render

  function render(b, pools, days) {
    host.textContent = "";
    var cur = b.currency || "USD";
    var money = function (v, o) { return fmt.money(v, Object.assign({ currency: cur }, o || {})); };
    var running = {};
    (b.collectors || []).forEach(function (c) { running[c.name] = c; });
    var cats = (b.categories || []).filter(function (c) { return c.on; }).sort(function (x, y) { return y.per_month - x.per_month; });
    var total = sum(cats, function (c) { return c.per_month; });
    var mv = moves(b);
    var months = agentMonths(days);
    var plans = sum(b.ai || [], function (a) { return a.cost_per_month; });
    var lastMonth = monthKey(-1), thisMonth = monthKey(0);
    var workloads = (b.rightsizing && b.rightsizing.workloads) || [];
    var own = workloads.filter(function (w) { return !w.managed; });
    var risks = own.filter(function (w) { return w.verdict === "risk"; }).length;

    // The lede: the figure, the biggest share, what the AI money bought.
    // The server's headline is the rate a month, this month so far and
    // last month, or the newest billed months.
    var head = (b.headline || [])[0], second = (b.headline || [])[1];
    var top = cats[0];
    var lede = el("header", "lede-wrap");
    var parts, then = null;
    if (head && head.note === "billed") {
      parts = [head.label.split(" ")[0] + "'s bill came to ", [money(head.amount), "fig"], "."];
      if (second) then = second.label.split(" ")[0] + "'s was " + money(second.amount) + ".";
    } else if (head && head.label === "This month so far") {
      parts = ["The estate has cost ", [money(head.amount), "fig"], " so far this month."];
      if (second) then = "Last month came to " + money(second.amount) + "; it runs " + money(total) + " a month now.";
    } else parts = ["The estate runs ", [money(total), "fig"], " a month."];
    if (top && total > 0) parts.push(" " + NAMES[top.category] + (top.category === "ai" ? " are " : " is "), [fmt.pct(top.per_month / total), "fig c-" + top.category], " of it.");
    if (months[lastMonth] && plans > 0) parts.push(" Agents did ", [money(months[lastMonth], { whole: true }), "fig c-ai"], " of work on the plans in " + fmt.monthShort(lastMonth) + ".");
    if (!total) parts = ["Nothing is metered yet."];
    lede.appendChild(say("h1", "lede", parts));

    var sub = then ? [then] : [];
    var deltas = Object.keys(mv).map(function (k) { return { k: k, d: mv[k].delta, days: mv[k].days }; });
    var net = sum(deltas, function (x) { return x.d; });
    if (deltas.length) {
      var most = deltas.slice().sort(function (x, y) { return Math.abs(y.d) - Math.abs(x.d); })[0];
      var span = Math.max.apply(null, deltas.map(function (x) { return x.days; }));
      if (Math.abs(net) < Math.max(1, total * 0.005)) sub.push("The run rate held steady over the last " + span + " days.");
      else sub.push("The run rate is " + (net > 0 ? "up " : "down ") + money(Math.abs(net)) + " a month over the last " + span + " days" + (Math.abs(most.d) > Math.abs(net) * 0.5 ? ", mostly " + LOWER[most.k] : "") + ".");
    }
    var cloud = (b.categories || []).filter(function (c) { return c.category === "cloud"; })[0];
    var cloudTrend = (b.trend || {}).cloud;
    if (cloud && !cloud.on && cloudTrend && cloudTrend.length) sub.push("Cloud is not counted; it was " + money(cloudTrend[cloudTrend.length - 1].v) + " a month when it last read.");
    var billedLast = (b.billed || []).filter(function (m) { return m.period === lastMonth && m.ai > 0; })[0];
    var basis = billedLast ? { cost: billedLast.ai, word: "the bill" } : { cost: plans, word: "the plans" };
    if (months[lastMonth] && plans > 0) sub.push("At API prices that work was " + fmt.times(months[lastMonth] / basis.cost) + " " + (billedLast ? "its AI bill." : "what the plans cost."));
    if (sub.length) lede.appendChild(el("p", "lede-sub", sub.join(" ")));

    // What needs the owner soon, said in the lede: a pool that runs dry in
    // the next two days, and what is in trouble now.
    var next = upcoming(pools, b.rightsizing && b.rightsizing.cluster ? risks : 0);
    var urgent = next.filter(function (e) { return e.kind === "dry" && e.at - Date.now() < 48 * 3600000; })[0];
    if (urgent || risks) {
      var alert = el("a", "lede-alert");
      alert.href = "#estate/next";
      alert.appendChild(el("i", "dot d-risk"));
      var said = [];
      if (urgent) said.push(urgent.names.join(" and ") + " runs dry " + fmt.when(new Date(urgent.at).toISOString()));
      if (risks) said.push(fmt.plural(risks, "workload") + (risks === 1 ? " is" : " are") + " in trouble now");
      alert.appendChild(el("span", null, "Needs you: " + said.join("; ") + "."));
      lede.appendChild(alert);
    }

    // One bar splits the month.
    if (total > 0) {
      var split = el("div", "split");
      split.setAttribute("role", "img");
      split.setAttribute("aria-label", cats.map(function (c) { return NAMES[c.category] + " " + money(c.per_month); }).join(", "));
      cats.forEach(function (c) {
        var s = el("span", "split-seg c-" + c.category);
        s.style.flexGrow = String(c.per_month);
        s.style.flexBasis = "0";
        if (c.per_month / total > 0.12) s.appendChild(el("span", null, NAMES[c.category]));
        split.appendChild(s);
      });
      lede.appendChild(split);
      var keys = el("div", "split-keys");
      cats.forEach(function (c) {
        var k = el("a", "split-key");
        k.href = "#estate/" + c.category;
        var name = el("span", "sk-name");
        name.appendChild(el("i", "sw c-" + c.category));
        name.appendChild(document.createTextNode(NAMES[c.category]));
        k.appendChild(name);
        k.appendChild(el("span", "sk-amt", money(c.per_month)));
        var m = mv[c.category];
        var line2 = el("span", "sk-sub", fmt.pct(c.per_month / total) + (m ? " · " + (Math.abs(m.delta) >= 0.01 ? fmt.delta(m.delta) + " in " + m.days + "d" : "steady " + m.days + "d") : ""));
        if (c.category === "ai" && months[lastMonth] && plans > 0) line2.appendChild(el("span", "sk-value", " · did " + fmt.times(months[lastMonth] / basis.cost) + " " + basis.word + " in " + fmt.monthShort(lastMonth)));
        k.appendChild(line2);
        var tr = (b.trend || {})[c.category];
        if (tr && tr.length > 1) k.appendChild(spark(tr.concat([{ t: b.generated_at, v: c.per_month }]), c.category));
        keys.appendChild(k);
      });
      if (cloud && !cloud.on && cloudTrend && cloudTrend.length) {
        var gk = el("a", "split-key ghost");
        gk.href = "#estate/cloud";
        var gn = el("span", "sk-name");
        gn.appendChild(el("i", "sw ghost"));
        gn.appendChild(document.createTextNode("Cloud"));
        gk.appendChild(gn);
        gk.appendChild(el("span", "sk-amt", "not counted"));
        gk.appendChild(el("span", "sk-sub", "was " + money(cloudTrend[cloudTrend.length - 1].v, { whole: true }) + " a month"));
        keys.appendChild(gk);
      }
      lede.appendChild(keys);
    }
    host.appendChild(lede);

    // What comes up next.
    if (next.length) {
      var up = el("section", "upnext");
      up.id = "next";
      up.setAttribute("aria-labelledby", "h-next");
      up.appendChild(el("h2", "eyebrow", "Coming up")).id = "h-next";
      var list = el("ol", "events");
      var WORDS = { dry: "runs dry", account: "runs out on one account", reset: "resets", risk: "in trouble" };
      next.slice(0, 6).forEach(function (e) {
        var li = el("li", "ev ev-" + e.kind);
        li.appendChild(el("span", "ev-when", e.kind === "risk" ? "Now" : cap(fmt.when(new Date(e.at).toISOString()))));
        var what = el("span", "ev-what");
        var names = e.names.length > 1 ? e.names.slice(0, -1).join(", ") + " and " + e.names[e.names.length - 1] : e.names[0];
        if (e.kind === "risk") {
          var a = el("a", null, names + " in trouble");
          a.href = "#estate/capacity";
          what.appendChild(a);
        } else {
          what.appendChild(el("b", null, names));
          what.appendChild(document.createTextNode(" " + WORDS[e.kind]));
        }
        li.appendChild(what);
        li.appendChild(el("span", "ev-detail", e.detail + (e.kind !== "risk" ? " · in\u00a0" + fmt.until(new Date(e.at).toISOString()) : "")));
        list.appendChild(li);
      });
      up.appendChild(list);
      host.appendChild(up);
    }

    // The sections in the order of the jobs: the biggest category first,
    // then what to fix, then the rest of the money, biggest first.
    var hasCapacity = b.rightsizing && (b.rightsizing.cluster || (b.rightsizing.machines || []).length || (b.rightsizing.advisor || []).length);
    var order = cats.map(function (c) { return c.category; });
    if (hasCapacity) order.splice(Math.min(1, order.length), 0, "capacity");
    var items = [];
    order.forEach(function (k) {
      var c = cats.filter(function (x) { return x.category === k; })[0];
      if (k === "capacity") {
        capacitySection(b.rightsizing, money, { advisorInCloud: order.indexOf("cloud") >= 0 });
        items.push({ view: "estate", id: "capacity", label: "Capacity", value: risks ? risks + " at risk" : "fits", tone: risks ? "bad" : "" });
        return;
      }
      if (k === "ai") aiSection(b, c, mv.ai, months, plans, pools, money);
      else if (k === "power") powerSection(b, c, mv.power, money);
      else if (k === "cloud") { cloudSection(b, c, mv.cloud, money); if (b.rightsizing) advisorBlock(document.getElementById("cloud"), b.rightsizing.advisor || [], money); }
      else if (k === "ci") ciSection(b, c, mv.ci, money);
      items.push({ view: "estate", id: k, label: NAMES[k], value: money(c.per_month, { whole: true }), share: total ? c.per_month / total : 0, color: COLORS[k] });
    });
    if (cloud && !cloud.on && running.azure) {
      offSection("cloud", "Cloud", cloudTrend, running.azure, money);
      items.push({ view: "estate", id: "cloud", label: "Cloud", value: "not counted" });
    }
    collectorsSection(b.collectors || []);

    var failing = (b.collectors || []).filter(function (c) { return c.rounds > 0 && !(c.last_success && c.last_success >= c.last_run); }).length;
    items.push({ view: "estate", id: "collectors", label: "Collectors", value: failing ? failing + " failing" : (b.collectors || []).length + " reading", tone: failing ? "bad" : "" });
    T.index(items);
  }

  // ---------- building blocks

  // A category's last days as a small line, its last point now.
  function spark(points, cat) {
    var W = 120, H = 28;
    var vs = points.map(function (p) { return p.v; });
    var lo = Math.min.apply(null, vs), hi = Math.max.apply(null, vs);
    if (hi - lo < Math.max(0.01, hi * 0.002)) { lo -= 1; hi += 1; }
    var x = function (i) { return i / (points.length - 1) * W; };
    var y = function (v) { return H - 3 - (v - lo) / (hi - lo) * (H - 6); };
    var d = points.map(function (p, i) { return (i ? "L" : "M") + x(i).toFixed(1) + " " + y(p.v).toFixed(1); }).join(" ");
    var g = svg("svg", { viewBox: "0 0 " + W + " " + H, class: "sk-spark", "aria-hidden": "true", preserveAspectRatio: "none" });
    g.appendChild(svg("path", { d: d, class: "c-" + cat, "vector-effect": "non-scaling-stroke" }));
    g.appendChild(svg("circle", { cx: x(points.length - 1), cy: y(vs[vs.length - 1]), r: 2.5, class: "c-" + cat }));
    return g;
  }

  function section(id, title, amount, move, sentence) {
    var s = el("section", "sec");
    s.id = id;
    var h = el("div", "sec-head");
    var h2 = el("h2", null, title);
    h2.id = "h-" + id;
    s.setAttribute("aria-labelledby", h2.id);
    h.appendChild(h2);
    var right = el("div", "sec-amt");
    if (amount) right.appendChild(el("span", null, amount));
    if (move && Math.abs(move.delta) >= 0.01) right.appendChild(el("span", "chip " + (move.delta > 0 ? "up" : "down"), fmt.delta(move.delta) + " in " + move.days + "d"));
    h.appendChild(right);
    s.appendChild(h);
    if (sentence) s.appendChild(typeof sentence === "string" ? el("p", "say", sentence) : sentence);
    host.appendChild(s);
    return s;
  }

  function sub(parent, title, note) {
    var h = el("div", "sub-head");
    h.appendChild(el("h3", null, title));
    if (note) h.appendChild(el("span", "note", note));
    parent.appendChild(h);
    return h;
  }

  // A line item: a name with a muted line under it, a bar of its share, and
  // its amount with a muted line under it.
  function line(parent, o) {
    var r = el("div", "li" + (o.cls ? " " + o.cls : ""));
    var n = el("div", "li-name");
    var name = el("div", "li-k" + (o.id ? " id" : ""));
    if (o.prefix) name.appendChild(el("span", "dim", o.prefix));
    name.appendChild(document.createTextNode(o.name));
    n.appendChild(name);
    if (o.sub) n.appendChild(el("div", "li-sub", o.sub));
    r.appendChild(n);
    var bar = el("div", "li-bar" + (o.share ? "" : " none"));
    if (o.share) {
      var f = el("i");
      f.style.width = (o.share > 0 ? Math.max(0.6, Math.min(1, o.share) * 100) : 0).toFixed(2) + "%";
      if (o.color) f.style.background = o.color;
      bar.appendChild(f);
    }
    r.appendChild(bar);
    var v = el("div", "li-v");
    v.appendChild(el("div", "li-amt", o.value));
    if (o.vsub) v.appendChild(el("div", "li-sub", o.vsub));
    r.appendChild(v);
    parent.appendChild(r);
    return r;
  }

  // The first few lines, then the rest folded behind their sum.
  function lines(parent, items, shown, make, rest) {
    items.slice(0, shown).forEach(function (it) { line(parent, make(it)); });
    if (items.length > shown) {
      var more = el("details", "more");
      more.appendChild(el("summary", null, rest(items.slice(shown))));
      more.addEventListener("toggle", function once() {
        more.removeEventListener("toggle", once);
        items.slice(shown).forEach(function (it) { line(more, make(it)); });
      });
      parent.appendChild(more);
    }
  }

  // ---------- AI

  function aiSection(b, c, move, months, plans, pools, money) {
    var accounts = (b.ai || []).slice().sort(function (x, y) { return (y.cost_per_month || 0) - (x.cost_per_month || 0); });
    var groups = {};
    accounts.forEach(function (a) { var k = cap(a.provider) + (a.plan ? " " + cap(a.plan) : ""); groups[k] = groups[k] || { n: 0, cost: 0 }; groups[k].n++; groups[k].cost += a.cost_per_month || 0; });
    var biggest = Object.keys(groups).sort(function (x, y) { return groups[y].cost - groups[x].cost; })[0];
    var sentence = b.billed && b.billed.length ? "The AI part of the bill. The plans below list " + money(plans) + " a month; the bill is the truth." :
      fmt.plural(accounts.length, "account") + (biggest && plans ? ". " + (groups[biggest].n > 1 ? groups[biggest].n + " " + biggest + " seats carry " : biggest + " carries ") + fmt.pct(groups[biggest].cost / plans) + " of the plan money." : ".");
    var s = section("ai", NAMES.ai, money(c.per_month) + " a month", move, sentence);
    if (c.label) s.querySelector(".sec-amt span").textContent = money(c.per_month) + " in " + c.label;

    // Every list's bars are shares of its section's total; a column takes
    // one precision, whole dollars once any line reaches $100.
    var whole = accounts.some(function (a) { return (a.cost_per_month || 0) >= 100; });
    accounts.forEach(function (a) {
      var sub = handle(a.account);
      if (a.tokens) sub += " · " + fmt.tokens(a.tokens.input || 0) + " in, " + fmt.tokens(a.tokens.output || 0) + " out this month";
      line(s, {
        name: cap(a.provider) + (a.plan ? " " + cap(a.plan) : ""),
        sub: sub,
        share: (a.cost_per_month || 0) / (plans || 1),
        color: "var(--c-ai)",
        value: a.cost_per_month ? money(a.cost_per_month, { whole: whole }) : a.cost_per_month === 0 ? "free" : "unpriced"
      });
    });

    if (b.billed && b.billed.length) billedBlock(s, b, months, money);
    if (Object.keys(months).length && plans > 0) valueBlock(s, months, plans, money, b.billed || []);
    if (pools.length) runwayBlock(s, pools);
  }

  // What the plans bought: agent work a month at API prices, against the
  // plans' price as a line across the bars.
  function valueBlock(s, months, plans, money, billed) {
    // A billed month is measured against its bill's AI; the rest against
    // what the plans list.
    var cost = function (k) { var m = billed.filter(function (x) { return x.period === k; })[0]; return m && m.ai > 0 ? m.ai : plans; };
    // The oldest month counts only when the days reach back to its start.
    var keys = Object.keys(months).sort().filter(function (k, i) { return i > 0 || months.first <= k + "-01"; }).slice(-4);
    var thisMonth = monthKey(0);
    var h = sub(s, "What the plans bought", "agent work a month at API prices");
    var link = el("a", "goto", "Open agent work");
    link.href = "#agents";
    h.appendChild(link);
    var top = Math.max.apply(null, keys.map(function (k) { return months[k]; }).concat([plans])) * 1.4;
    var chart = el("div", "value");
    chart.setAttribute("role", "img");
    chart.setAttribute("aria-label", keys.map(function (k) { return fmt.month(k) + " " + money(months[k], { whole: true }); }).join(", ") + "; the plans cost " + money(plans, { whole: true }) + " a month");
    var plot = el("div", "vplot"), names = el("div", "vnames");
    plot.style.setProperty("--cols", keys.length);
    names.style.setProperty("--cols", keys.length);
    // This month on its pace so far: what it ends near if the days ahead
    // look like the days behind.
    var now = new Date(), dom = now.getDate(), dim = new Date(now.getFullYear(), now.getMonth() + 1, 0).getDate();
    var pace = months[thisMonth] && dom < dim ? months[thisMonth] / dom * dim : null;
    if (pace) top = Math.max(top, pace * 1.15);
    keys.forEach(function (k) {
      var col = el("div", "vcol" + (k === thisMonth ? " partial" : ""));
      var bar = el("div", "vbar");
      bar.style.height = (months[k] / top * 100).toFixed(2) + "%";
      if (k === thisMonth && pace) {
        var ghost = el("div", "vghost");
        ghost.style.height = (pace / top * 100).toFixed(2) + "%";
        ghost.appendChild(el("span", null, "pace " + money(pace, { whole: true })));
        col.appendChild(ghost);
      }
      var lbl = el("div", "vlbl");
      lbl.appendChild(el("b", null, money(months[k], { whole: true })));
      lbl.appendChild(el("span", null, fmt.times(months[k] / cost(k)) + (billed.length ? (cost(k) !== plans ? " the bill" : " the plans") : "")));
      col.appendChild(lbl);
      col.appendChild(bar);
      plot.appendChild(col);
      names.appendChild(el("div", "vname", fmt.monthShort(k) + (k === thisMonth ? ", so far" : "")));
    });
    var rule = el("div", "vplan");
    rule.style.bottom = (plans / top * 100).toFixed(2) + "%";
    plot.appendChild(rule);
    chart.appendChild(plot);
    chart.appendChild(names);
    var lg = el("div", "legend");
    var a = el("span"); a.appendChild(el("i", "sw c-ai")); a.appendChild(document.createTextNode("agent work at API prices")); lg.appendChild(a);
    var b = el("span"); b.appendChild(el("i", "sw dash")); b.appendChild(document.createTextNode("the plans, " + money(plans, { whole: true }) + " a month")); lg.appendChild(b);
    if (months[thisMonth]) { var hs = el("span"); hs.appendChild(el("i", "sw vpart-sw")); hs.appendChild(document.createTextNode("this month so far")); lg.appendChild(hs); }
    if (pace) { var c = el("span"); c.appendChild(el("i", "sw vghost-sw")); c.appendChild(document.createTextNode("on pace")); lg.appendChild(c); }
    chart.appendChild(lg);
    s.appendChild(chart);
  }

  // The runway: each pool's limit from a day ago through the next seven
  // days, recorded then projected at the current burn, with the moments it
  // runs out or refills; the same chart Agent work shows in full.
  function runwayBlock(s, pools) {
    var h = sub(s, "Runway", "each pool's limit, a day back and seven days ahead");
    var link = el("a", "goto", "All pools");
    link.href = "#agents/pools";
    h.appendChild(link);
    var from = Date.now() - 86400000, to = Date.now() + 7 * 86400000;
    var wrap = el("div", "runway");
    pools.forEach(function (p) {
      var row = el("div", "rw-row");
      var name = el("div", "rw-name");
      name.appendChild(el("b", null, cap(p.provider) + (p.scope ? " " + p.scope : "")));
      var low = p.remaining / p.capacity < 0.2;
      name.appendChild(el("div", "rw-left" + (low ? " bad" : ""), fmt.pct(p.remaining / p.capacity) + " left"));
      name.appendChild(el("div", "li-sub", p.empty ? "dry " + fmt.when(p.empty).replace(/ /g, "\u00a0") : p.first_empty ? "one out " + fmt.when(p.first_empty.at).replace(/ /g, "\u00a0") : "lasts the week"));
      name.appendChild(el("div", "li-sub", fmt.plural(p.accounts, "account") + " · " + p.window));
      row.appendChild(name);
      row.appendChild(T.poolChart(p, { compact: true, from: from, to: to }));
      wrap.appendChild(row);
    });
    wrap.appendChild(T.poolLegend(pools, { from: from, to: to }));
    s.appendChild(wrap);
  }

  function levelAt(pts, t) {
    var v = pts[0].v;
    for (var i = 0; i < pts.length && new Date(pts[i].t).getTime() <= t; i++) v = pts[i].v;
    return v;
  }

  // The bill's AI month by month, beside what the machines collected for it.
  // With the gateway read, three lenses instead: what was paid, what the
  // gateway measured, and what the machines saw of it.
  function billedBlock(s, b, months, money) {
    var rows = b.billed.slice().reverse();
    if (b.reconciliation && b.reconciliation.months.length) {
      lensBlock(s, b.reconciliation.months.slice().reverse(), money);
    } else {
      sub(s, "Billed and collected", "the bill is the truth; collected is what the machines sent, at API prices");
      // Each month's bill beside what the machines collected for it, on one
      // scale across the months.
      var most = Math.max.apply(null, rows.map(function (m) { return Math.max(m.ai, months[m.period] || 0); }).concat([1]));
      rows.forEach(function (m) {
        var got = months[m.period];
        var r = line(s, { name: fmt.month(m.period), sub: m.invoice ? "invoice " + m.invoice : null, value: money(m.ai), vsub: got != null ? money(got) + " collected" : "not collected" });
        bars(r, [["billed", m.ai], ["got", got || 0]], most);
      });
      legend(s, [["billed", "billed AI"], ["got", "collected from the machines, at API prices"]]);
    }
    var latest = rows[0];
    if (latest.programs && latest.programs.length) {
      sub(s, "AI by program", fmt.month(latest.period));
      latest.programs.slice().sort(function (x, y) { return y.ai - x.ai; }).forEach(function (p) {
        line(s, { name: p.program, share: p.ai / (latest.ai || 1), color: "var(--c-ai)", value: money(p.ai), vsub: latest.ai > 0 ? fmt.pct(p.ai / latest.ai) + " of AI" : null });
      });
    }
  }

  // A line's bar as several bars on one scale.
  function bars(r, parts, most) {
    var bar = r.querySelector(".li-bar");
    bar.className = "li-bar pair" + (parts.length > 2 ? " trio" : "");
    bar.textContent = "";
    parts.forEach(function (p) {
      var i = el("i", p[0]);
      i.style.width = (p[1] / most * 100).toFixed(2) + "%";
      bar.appendChild(i);
    });
  }

  function legend(s, parts) {
    var bk = el("div", "legend");
    parts.forEach(function (p) { var sp = el("span"); sp.appendChild(el("i", "sw pair-" + p[0])); sp.appendChild(document.createTextNode(p[1])); bk.appendChild(sp); });
    s.appendChild(bk);
  }

  // The three lenses, newest month first: the bill is what was paid, the
  // gateway what was used, the machines what it was for. Then the newest
  // billed month taken apart, its models, and, for the detail role, its keys.
  function lensBlock(s, rows, money) {
    sub(s, "Billed, measured, and seen", "the bill is what was paid; the gateway measured every call, at API prices; the machines saw part of it");
    var most = Math.max.apply(null, rows.map(function (m) { return Math.max(m.basis || 0, m.gateway.cost, m.shipped.cost); }).concat([1]));
    var whole = rows.some(function (m) { return Math.max(m.billed || 0, m.gateway.cost) >= 100; });
    var newestBilled = rows.filter(function (x) { return x.billed != null; }).map(function (x) { return x.period; })[0] || "";
    rows.forEach(function (m) {
      var billed = m.billed != null;
      // Tokens with no priced model are counted, never shown as $0.
      var priced = m.gateway.cost > 0;
      var value = billed ? money(m.basis, { whole: whole }) : priced ? money(m.gateway.cost, { whole: whole }) + " measured" : fmt.tokens(m.gateway.unpriced) + " tokens";
      var vsub = [];
      if (billed && priced) vsub.push(money(m.gateway.cost, { whole: whole }) + " measured");
      if (m.gateway.unpriced) vsub.push(billed || priced ? fmt.tokens(m.gateway.unpriced) + " tokens unpriced" : "unpriced");
      vsub.push(money(m.shipped.cost, { whole: whole }) + " seen");
      var r = line(s, {
        name: fmt.month(m.period),
        sub: billed ? "invoice " + (m.invoice || "pending") : m.period < newestBilled ? "no bill read" : "not billed yet",
        value: value,
        vsub: vsub.join(" · ")
      });
      bars(r, [["billed", m.basis || 0], ["gw", m.gateway.cost], ["got", m.shipped.cost]], most);
    });
    var modelsBasis = rows.some(function (m) { return m.basis_is_models; });
    legend(s, [["billed", modelsBasis ? "billed for models" : "billed AI"], ["gw", "measured at the gateway"], ["got", "seen by the machines"]]);

    var m = rows.filter(function (x) { return x.billed != null; })[0] || rows[0];
    sub(s, "Where " + fmt.month(m.period) + " lands", m.billed != null ? "the gap is what the gateway's prices do not explain" : "the bill has not come yet");
    var parts = [];
    if (m.billed != null) {
      if (m.basis_is_models) {
        parts.push({ name: "Billed for models", sub: "Foundry models and marketplace SaaS", value: m.basis });
        parts.push({ name: "Other AI", sub: "Assistants and the rest, no gateway call", value: m.billed - m.basis });
      } else {
        parts.push({ name: "Billed AI", sub: "the bill does not split out models yet", value: m.billed });
      }
    }
    parts.push({ name: "Measured at the gateway", sub: fmt.tokens(m.gateway.tokens) + " tokens at API prices", value: m.gateway.cost });
    if (m.gap != null) parts.push({ name: "Gap", sub: m.gap >= 0 ? "billed above API prices" : "billed below API prices", value: m.gap });
    parts.push({ name: "Seen by the machines", sub: fmt.pct(m.coverage) + " of the gateway's tokens", value: m.shipped.cost });
    var top = Math.max.apply(null, parts.map(function (p) { return Math.abs(p.value); }).concat([1]));
    var wholeParts = parts.some(function (p) { return Math.abs(p.value) >= 100; });
    parts.forEach(function (p) { line(s, { name: p.name, sub: p.sub, share: Math.abs(p.value) / top, color: "var(--c-ai)", value: money(p.value, { whole: wholeParts }) }); });
    var notes = [];
    if (m.gateway.unpriced) notes.push(fmt.tokens(m.gateway.unpriced) + " gateway tokens carry no priced model, so they are counted and not priced. Tokens from before the gateway tagged them with their model have none.");
    if (m.over && m.over.length) notes.push("The machines posted more than the gateway measured for " + m.over.join(", ") + ". That is a finding, not a correction.");
    notes.forEach(function (n) { s.appendChild(el("p", "note", n)); });

    var models = m.models.filter(function (x) { return x.gateway > 0 || x.shipped > 0; });
    if (models.length) {
      sub(s, "By model", fmt.month(m.period) + ", at the gateway");
      var mostModel = Math.max.apply(null, models.map(function (x) { return x.gateway_cost; }).concat([1]));
      var wholeModels = models.some(function (x) { return x.gateway_cost >= 100; });
      lines(s, models, 6, function (x) {
        return {
          name: x.model || "no model tag", id: true,
          sub: fmt.tokens(x.gateway) + " tokens · " + (x.gateway ? fmt.pct(Math.min(x.shipped, x.gateway) / x.gateway) + " seen" : fmt.tokens(x.shipped) + " seen, none measured"),
          share: x.gateway_cost / mostModel, color: "var(--c-ai)",
          value: x.priced ? money(x.gateway_cost, { whole: wholeModels }) : "unpriced"
        };
      }, function (rest) { return fmt.plural(rest.length, "more model"); });
    }

    var keys = el("div");
    s.appendChild(keys);
    T.getJSON("/v1/gateway/keys?month=" + m.period).then(function (k) {
      if (!k || !k.keys.length) return;
      sub(keys, "By key", fmt.month(m.period) + (m.billed != null ? ", the bill shared by what each key's calls cost" : ", at API prices"));
      var mostKey = Math.max.apply(null, k.keys.map(function (x) { return x.billed != null ? x.billed : x.cost; }).concat([1]));
      var wholeKeys = k.keys.some(function (x) { return (x.billed != null ? x.billed : x.cost) >= 100; });
      lines(keys, k.keys, 8, function (x) {
        var v = x.billed != null ? x.billed : x.cost;
        return {
          name: x.key, id: true,
          sub: x.product + " · " + fmt.tokens(x.tokens) + " tokens" + (x.unpriced ? ", " + fmt.tokens(x.unpriced) + " unpriced" : ""),
          share: v / mostKey, color: "var(--c-ai)",
          value: money(v, { whole: wholeKeys }),
          vsub: x.billed != null ? money(x.cost, { whole: wholeKeys }) + " at API prices" : null
        };
      }, function (rest) { return fmt.plural(rest.length, "more key"); });
    }).catch(function (e) { if (e.status !== 403 && e.status !== 404) T.error(e); });
  }

  // ---------- power

  function powerSection(b, c, move, money) {
    var hosts = (b.hosts || []).slice().sort(function (x, y) { return y.per_month - x.per_month; });
    var watts = sum(hosts, function (h) { return h.watts; });
    var ns = (b.namespaces || []).slice().sort(function (x, y) { return (y.per_month || 0) - (x.per_month || 0); });
    var sentence = hosts.length ? fmt.plural(hosts.length, "host") + " draw " + fmt.watts(watts) + " now; a month of that at the meter's price is " + money(c.per_month) + "." + (ns.length ? " The cluster's share splits across " + ns.length + " namespaces, half by the CPU they ask for and half by the memory." : "") : "No host is metered. Set POWER_PROMETHEUS_QUERY or HASS_POWER_ENTITIES.";
    var s = section("power", NAMES.power, money(c.per_month) + " a month", move, sentence);
    var most = hosts.reduce(function (s, h) { return s + h.per_month; }, 0) || 1;
    hosts.forEach(function (h) {
      line(s, { name: h.host, id: true, sub: fmt.watts(h.watts) + " now" + (h.cluster ? " · runs the cluster" : ""), share: h.per_month / most, color: "var(--c-power)", value: money(h.per_month) });
    });
    if (ns.length) {
      sub(s, "By namespace", "each one's share of the cluster hosts' power");
      var top = 1;
      lines(s, ns, 6, function (n) {
        var asks = [n.cpu_request_cores > 0 ? fmt.cores(n.cpu_request_cores) : "", n.memory_request_bytes > 0 ? fmt.bytes(n.memory_request_bytes) : ""].filter(Boolean).join(", ");
        return { name: n.namespace, id: true, sub: asks ? "asks " + asks : "asks nothing", share: n.share / top, color: "var(--c-power)", value: money(n.per_month), vsub: fmt.pct(n.share) + " of the cluster" };
      }, function (rest) { return rest.length + " more namespaces · " + money(sum(rest, function (n) { return n.per_month; })) + " together"; });
    }
  }

  // ---------- cloud

  function cloudSection(b, c, move, money) {
    var cl = (b.cloud || []).slice().sort(function (x, y) { return y.trailing - x.trailing; });
    var sentence = b.billed && b.billed.length ? "The bill's usage, less its AI." : "Azure Cost Management's actual cost: the last 30 days" + (c.month_to_date != null ? ", " + money(c.month_to_date) + " this month so far, " + money(c.previous) + " last month." : ".");
    var s = section("cloud", NAMES.cloud, money(c.per_month) + " a month", move, sentence);
    if (!cl.length) { s.appendChild(el("p", "empty", b.billed ? "The bill is read by month; lines by service come from the azure collector." : "No cloud account is read.")); return; }
    sub(s, "By service", "Azure Cost Management, the last 30 days" + (b.billed && b.billed.length ? "; read apart from the bill" : ""));
    var top = cl.reduce(function (s, l) { return s + l.trailing; }, 0) || 1;
    var cw = cl.some(function (l) { return l.trailing >= 100; });
    lines(s, cl, 8, function (l) {
      return { name: l.service, prefix: l.group + " · ", sub: l.account, share: l.trailing / top, color: "var(--c-cloud)", value: money(l.trailing, { currency: l.currency, whole: cw }), vsub: money(l.current, { currency: l.currency, whole: true }) + " now · " + money(l.previous, { currency: l.currency, whole: true }) + " last" };
    }, function (rest) { return rest.length + " more lines · " + money(sum(rest, function (l) { return l.trailing; })) + " together"; });
  }

  // A category whose collector runs but does not count, said plainly.
  function offSection(id, title, trend, collector, money) {
    var last = trend && trend.length ? trend[trend.length - 1] : null;
    var s = section(id, title, null, null, null);
    s.classList.add("off");
    s.querySelector(".sec-amt").appendChild(el("span", "chip", "not counted"));
    var ok = collector.last_success && collector.last_run && collector.last_success >= collector.last_run;
    s.appendChild(el("p", "say", "Not in the total: the " + collector.name + " collector " + (ok ? "reads, but finds no cost to count" : "is failing" + (collector.last_success ? ", last read " + fmt.ago(collector.last_success) : "")) + "." +
      (last ? " It was " + money(last.v) + " a month when it last counted." : "")));
  }

  // ---------- hosted CI

  function ciSection(b, c, move, money) {
    var ci = b.ci || {};
    var repos = (ci.repositories || []).slice().sort(function (x, y) { return (y.hosted_per_month || 0) - (x.hosted_per_month || 0) || (y.self_hosted_minutes || 0) - (x.self_hosted_minutes || 0); });
    var parts = [];
    parts.push("GitHub billed " + money(c.per_month) + " over the last 30 days." + (c.month_to_date != null ? " This month so far: " + money(c.month_to_date) + "; last month: " + money(c.previous) + "." : ""));
    if (ci.hosted && ci.hosted_per_month != null) {
      var lead = repos[0];
      var hotter = ci.hosted_per_month > c.per_month * 1.15;
      parts.push((hotter ? "The last " + ci.window + " ran hotter: at that pace it is " : "At the last " + ci.window + "'s pace it is ") + money(ci.hosted_per_month) + " a month" + (lead && ci.hosted_per_month > 0 && lead.hosted_per_month / ci.hosted_per_month > 0.25 ? ", and " + lead.repository + " is " + fmt.pct(lead.hosted_per_month / ci.hosted_per_month) + " of it." : "."));
    }
    var s = section("ci", NAMES.ci, money(c.per_month) + " a month", move, parts.join(" ") || "What hosted runners bill, after included minutes.");
    if (!repos.length) { s.appendChild(el("p", "empty", "No builds in the window.")); return; }
    if (ci.hosted && ci.hosted_per_month != null) {
      // The bill and the pace on one scale, so the lines below (parts of
      // the pace) never look bigger than the figure above them.
      var scale = el("div", "pace");
      var most = Math.max(c.per_month, ci.hosted_per_month) || 1;
      var paid = repos.filter(function (r) { return r.hosted_per_month > 0; });
      [["billed", "Billed, the last 30 days", c.per_month], ["pace", "At the last " + ci.window + "'s pace", ci.hosted_per_month]].forEach(function (r) {
        var row = el("div", "pace-row " + r[0]);
        row.appendChild(el("span", "pace-k", r[1]));
        var bar = el("div", "pace-bar");
        if (r[0] === "pace") {
          // The pace split by repository: the biggest three, then the rest.
          var shown = 0;
          paid.slice(0, 3).forEach(function (p, i) {
            var seg = el("i", "seg-" + i);
            seg.style.width = (p.hosted_per_month / most * 100).toFixed(2) + "%";
            seg.title = p.repository + ", " + money(p.hosted_per_month);
            bar.appendChild(seg);
            shown += p.hosted_per_month;
          });
          var rest = el("i", "seg-rest");
          rest.style.width = (Math.max(0, r[2] - shown) / most * 100).toFixed(2) + "%";
          bar.appendChild(rest);
        } else {
          var f = el("i");
          f.style.width = (r[2] / most * 100).toFixed(2) + "%";
          bar.appendChild(f);
        }
        row.appendChild(bar);
        row.appendChild(el("span", "pace-v", money(r[2]) + " a month"));
        scale.appendChild(row);
      });
      var keys = el("div", "legend pace-keys");
      paid.slice(0, 3).forEach(function (p, i) { var sp = el("span"); sp.appendChild(el("i", "sw seg-" + i)); sp.appendChild(document.createTextNode(p.repository.split("/").pop() + " " + fmt.pct(p.hosted_per_month / ci.hosted_per_month))); keys.appendChild(sp); });
      if (paid.length > 3) { var sp = el("span"); sp.appendChild(el("i", "sw seg-rest")); sp.appendChild(document.createTextNode(paid.length - 3 + " more with hosted cost")); keys.appendChild(sp); }
      scale.appendChild(keys);
      s.appendChild(scale);
    }
    sub(s, "By repository", "each one's part of the " + (ci.window || "7d") + " pace, after free minutes; \"at list\" is before them");
    // The lines share the pace's scale, so each bar is its part of the pace.
    var top = ci.hosted_per_month || repos[0].hosted_per_month || 1;
    lines(s, repos, 6, function (r) {
      var m = [];
      if (r.hosted_minutes) m.push(fmt.count(r.hosted_minutes) + " min hosted");
      if (r.self_hosted_minutes) m.push(fmt.count(r.self_hosted_minutes) + " min self-hosted");
      return {
        name: r.repository, prefix: r.forge !== "github" ? r.forge + " · " : null, sub: m.join(" · "),
        share: (r.hosted_per_month || 0) / top, color: "var(--c-ci)", id: true,
        value: r.hosted_per_month ? money(r.hosted_per_month) : "none hosted",
        vsub: r.hosted_gross_per_month > r.hosted_per_month ? money(r.hosted_gross_per_month) + " at list" : r.self_hosted_per_month >= 0.005 ? money(r.self_hosted_per_month) + " of the pool" : null
      };
    }, function (rest) { var paidRest = rest.filter(function (r) { return r.hosted_per_month > 0; }).length; return rest.length + " more, " + paidRest + " with hosted cost · " + money(sum(rest, function (r) { return r.hosted_per_month; })); });
    if (ci.pool) {
      var p = ci.pool;
      s.appendChild(el("p", "note", "Over the " + (ci.window || "week") + ", the self-hosted runners in " + p.namespace + " " + (p.nodes ? "ran on their own nodes for " + Math.round(p.node_hours) + " node hours" : "averaged " + fmt.pct(p.share) + " of the cluster's requests while jobs ran") +
        (p.per_month != null ? ": " + money(p.per_month) + " a month, already counted in " + (p.nodes ? "cloud" : "power") + "." + (p.nodes ? "" : " Power's namespace list weighs what the namespace asks for now, so its figure differs.") : ", unpriced without the hosts' power.")));
    }
  }

  // ---------- capacity

  var VERDICTS = [
    { key: "risk", title: "At risk", note: "In trouble now: killed for memory, near a memory limit, or held back by a CPU limit. Fix these first." },
    { key: "under", title: "Asks too little", note: "Uses more than it asks for, so the scheduler can place it where it does not fit." },
    { key: "unset", title: "Asks nothing", note: "Uses CPU or memory with no request, so the scheduler does not count it." },
    { key: "over", title: "Asks too much", note: "Shrinking these frees room, the most first." }
  ];
  var SHOWN = 8;

  function amount(v, resource) { return resource === "cpu" ? fmt.cores(v) : fmt.bytes(v); }
  // "13 of 32 cores": the unit once when both sides share it.
  function of(a, b, f) {
    var x = f(a), y = f(b), ux = x.split("\u00a0").slice(1).join(" "), uy = y.split("\u00a0").slice(1).join(" ");
    return ux && ux === uy ? x.split("\u00a0")[0] + " of " + y : x + " of " + y;
  }
  function capacity(c) {
    var p = [];
    if (Math.abs(c.cpu) >= 0.0005) p.push(fmt.cores(c.cpu));
    if (Math.abs(c.memory) >= 1048576) p.push(fmt.bytes(c.memory));
    return p.join(" and ") || "nothing";
  }

  function capacitySection(rs, money, opts) {
    opts = opts || {};
    var cl = rs.cluster;
    var workloads = rs.workloads || [];
    var own = workloads.filter(function (w) { return !w.managed; });
    var risks = own.filter(function (w) { return w.verdict === "risk"; }).length;
    var sentence = "";
    if (cl) {
      var after = { cpu: cl.requested.cpu + cl.adds.cpu - cl.frees.cpu, memory: cl.requested.memory + cl.adds.memory - cl.frees.memory };
      var fits = after.cpu <= cl.allocatable.cpu && after.memory <= cl.allocatable.memory;
      sentence = "The cluster asks for " + of(cl.requested.cpu, cl.allocatable.cpu, fmt.cores) + " and " + of(cl.requested.memory, cl.allocatable.memory, fmt.bytes) + ". " +
        "Fixing what asks for too little adds " + capacity(cl.adds) + ". Shrinking what asks for too much frees " + capacity(cl.frees) + ". " +
        (fits ? "Both together still fit." : "Both together run past the room by " + [after.cpu > cl.allocatable.cpu ? fmt.cores(after.cpu - cl.allocatable.cpu) : "", after.memory > cl.allocatable.memory ? fmt.bytes(after.memory - cl.allocatable.memory) : ""].filter(Boolean).join(" and ") + ".") + " What the platform sizes is left out.";
    }
    var s = section("capacity", "Capacity", null, null, sentence);
    var amt = s.querySelector(".sec-amt");
    amt.appendChild(el("span", risks ? "chip bad" : "chip", risks ? risks + " at risk" : "nothing at risk"));
    if (rs.window) s.appendChild(el("p", "note", "Use over the last " + rs.window + ": CPU at p95, memory at its peak, each sized with headroom."));

    if (cl) {
      var room = el("div", "room");
      [["cpu", "CPU", fmt.cores], ["memory", "Memory", fmt.bytes]].forEach(function (r) {
        var k = r[0], all = cl.allocatable[k] || 1;
        var row = el("div", "room-row");
        row.appendChild(el("span", "room-k", r[1]));
        // Two bars on one scale: what is asked now, and what would be asked
        // after both changes; the rule is the cluster's room.
        var keep = Math.max(0, cl.requested[k] - cl.frees[k]);
        var after = keep + cl.adds[k];
        var scale = Math.max(all, cl.requested[k], after);
        var bars = el("div", "room-bars");
        [["now", [["now", cl.requested[k]]]], ["after", [["keep", keep], ["adds", cl.adds[k]]]]].forEach(function (b) {
          var bar = el("div", "room-bar " + b[0]);
          var segs = el("div", "room-segs");
          b[1].forEach(function (p) {
            var seg = el("i", "rs-" + p[0]);
            seg.style.width = Math.max(0, p[1] / scale * 100).toFixed(2) + "%";
            segs.appendChild(seg);
          });
          bar.appendChild(segs);
          var edge = el("span", "room-edge");
          edge.style.left = (all / scale * 100).toFixed(2) + "%";
          bar.appendChild(edge);
          bars.appendChild(bar);
        });
        row.appendChild(bars);
        var v = el("span", "room-v");
        v.appendChild(el("span", null, of(cl.requested[k], all, r[2]) + " now"));
        v.appendChild(el("span", after > all ? "bad" : null, after > all ? r[2](after - all) + " past the room after" : of(after, all, r[2]) + " after"));
        row.appendChild(v);
        room.appendChild(row);
      });
      var lg = el("div", "legend");
      [["now", "asked now"], ["keep", "asked after shrinking"], ["adds", "added by fixing"], ["edge", "the cluster's room"]].forEach(function (p) {
        var sp = el("span");
        sp.appendChild(el("i", "sw rs-" + p[0]));
        sp.appendChild(document.createTextNode(p[1]));
        lg.appendChild(sp);
      });
      room.appendChild(lg);
      s.appendChild(room);

      if (!own.length) s.appendChild(el("p", "empty", "Every workload's requests fit its use."));
      // The verdicts at a glance, worst first; each opens its group.
      var groups = [];
      VERDICTS.forEach(function (v) {
        var list = own.filter(function (w) { return w.verdict === v.key; });
        if (list.length) groups.push([v, list]);
      });
      var managed = workloads.filter(function (w) { return w.managed; });
      if (managed.length) groups.push([{ key: "managed", title: "Sized by the platform", note: "Static pods and the namespaces the platform owns. Their sizes come from the cluster's own configuration." }, managed]);
      groups.forEach(function (gv) { s.appendChild(group(gv[0], gv[1], money)); });
    }

    var machines = rs.machines || [];
    if (machines.length) {
      var hostsN = {};
      machines.forEach(function (r) { hostsN[r.workload] = 1; });
      var big = machines.filter(function (r) { return r.verdict === "over"; }).length, small = machines.length - big;
      var sg = el("details", "vgroup g-servers");
      sg.id = "servers";
      var ss = el("summary");
      var st = el("span", "vg-title");
      st.appendChild(el("i", "dot d-servers"));
      st.appendChild(document.createTextNode("Servers"));
      st.appendChild(el("span", "vg-count", String(Object.keys(hostsN).length)));
      ss.appendChild(st);
      ss.appendChild(el("span", "vg-eff", [big ? fmt.plural(big, "size") + " too big" : "", small ? fmt.plural(small, "size") + " too small" : ""].filter(Boolean).join("\u00a0· ")));
      sg.appendChild(ss);
      sg.appendChild(el("p", "note", "Each server's size against its use, a resource at a time."));
      var shost = el("div", "vg-body");
      sg.appendChild(shost);
      s.appendChild(sg);
      var sized = function (v, unit) { return unit === "bytes" ? fmt.bytes(v) : unit === "percent" ? Math.round(v) + "%" : fmt.cores(v); };
      var byHost = {}, hostOrder = [];
      machines.forEach(function (r) { if (!byHost[r.workload]) { byHost[r.workload] = []; hostOrder.push(r.workload); } byHost[r.workload].push(r); });
      var box = el("div", "servers");
      hostOrder.forEach(function (h) {
        var row = el("div", "srv");
        row.appendChild(el("div", "li-k id", h));
        byHost[h].forEach(function (r) {
          // One scale per resource: the size, what Kubernetes holds of it,
          // the use from p95 to peak, and the size to set.
          var pctUnit = r.unit === "percent";
          var cell = el("div", "srv-r v-" + r.verdict);
          var what = (r.resource === "cpu" ? "CPU" : "Memory") + (r.verdict === "over" ? " too big" : " too small");
          var head = el("div", "rg-head");
          head.appendChild(el("span", "srv-what", what));
          var words = [pctUnit ? Math.round(r.p95) + "% busy at p95" : "is " + sized(r.request, r.unit) + ", uses " + (r.peak ? sized(r.peak, r.unit) + " at peak" : sized(r.p95, r.unit) + " at p95")];
          if (r.requested) words.push("Kubernetes holds " + sized(r.requested, r.unit));
          words.push(pctUnit ? "core count unknown" : "set to " + sized(r.recommended, r.unit));
          head.appendChild(el("span", "rg-nums", words.join(" · ")));
          cell.appendChild(head);
          var top = pctUnit ? 100 : Math.max(r.request || 0, r.peak || 0, r.p95 || 0, r.requested || 0, r.recommended || 0) || 1;
          var at = function (v) { return Math.min(100, v / top * 100).toFixed(2) + "%"; };
          var bar = el("div", "rg-bar");
          var lo = Math.min(r.p95 || r.peak || 0, r.peak || r.p95 || 0), hi = Math.max(r.peak || 0, r.p95 || 0);
          var use = el("span", "use");
          use.style.left = at(pctUnit ? 0 : lo);
          use.style.width = "max(3px, calc(" + at(hi) + " - " + at(pctUnit ? 0 : lo) + "))";
          bar.appendChild(use);
          function tick(v, cls) { if (!v) return; var t = el("span", "mk " + cls); t.style.left = at(v); bar.appendChild(t); }
          if (!pctUnit) { tick(r.request, "size"); tick(r.requested, "holds"); tick(r.recommended, "set"); }
          cell.appendChild(bar);
          row.appendChild(cell);
        });
        box.appendChild(row);
      });
      shost.appendChild(box);
      var slg = el("div", "legend");
      [["use", "use, p95 to peak"], ["size", "the server's size"], ["holds", "what Kubernetes holds"], ["set", "set to"]].forEach(function (k) {
        var sp = el("span");
        sp.appendChild(el("i", "mk " + k[0]));
        sp.appendChild(document.createTextNode(k[1]));
        slg.appendChild(sp);
      });
      shost.appendChild(slg);
    }

    if (!opts.advisorInCloud) advisorBlock(s, rs.advisor || [], money);
  }

  // Azure Advisor's advice on VMs: beside Cloud when Cloud shows, else
  // under Capacity.
  function advisorBlock(s, advisor, money) {
    if (advisor.length) {
      var saves = sum(advisor.filter(function (r) { return !r.decide; }), function (r) { return r.savings_per_year; });
      var waits = sum(advisor.filter(function (r) { return r.decide; }), function (r) { return r.savings_per_year; });
      sub(s, "Azure Advisor", money(saves / 12) + " a month counted" + (waits ? ", " + money(waits / 12) + " more waits on a decision" : ""));
      var most = sum(advisor, function (r) { return r.savings_per_year; }) || 1;
      advisor.forEach(function (r) {
        line(s, {
          name: r.workload, prefix: r.namespace ? r.namespace + " / " : null,
          sub: cap(r.change) + (r.decide ? " (needs a decision)" : "") + " · " + r.from + (r.to ? " to " + r.to : "") + " · CPU " + Math.round(r.p95) + "%" + (r.memory_p95 ? ", memory " + Math.round(r.memory_p95) + "%" : "") + " at p95",
          share: (r.savings_per_year || 0) / most, color: "var(--c-cloud)", value: r.savings_per_year ? money(r.savings_per_year / 12) + " a month" : "", vsub: r.savings_per_year ? money(r.savings_per_year) + " a year" : null
        });
      });
    }
    }

  function group(v, list, money) {
    var g = el("details", "vgroup g-" + v.key);
    if (v.key === "risk") g.open = true;
    var sm = el("summary");
    var t = el("span", "vg-title");
    t.appendChild(el("i", "dot d-" + v.key));
    t.appendChild(document.createTextNode(v.title));
    t.appendChild(el("span", "vg-count", String(list.length)));
    sm.appendChild(t);
    var adds = { cpu: 0, memory: 0 }, frees = { cpu: 0, memory: 0 };
    list.forEach(function (w) {
      if (w.frees.cpu < 0) adds.cpu -= w.frees.cpu; else frees.cpu += w.frees.cpu;
      if (w.frees.memory < 0) adds.memory -= w.frees.memory; else frees.memory += w.frees.memory;
    });
    var eff = [];
    if (adds.cpu > 0.0005 || adds.memory > 1048576) eff.push("adds " + capacity(adds));
    if (frees.cpu > 0.0005 || frees.memory > 1048576) eff.push("frees " + capacity(frees));
    sm.appendChild(el("span", "vg-eff", eff.join("\u00a0· ")));
    g.appendChild(sm);
    g.appendChild(el("p", "note", v.note));
    var body = el("div", "vg-body");
    var fill = function () {
      list.slice(0, SHOWN).forEach(function (w) { body.appendChild(workload(w, money)); });
      if (list.length > SHOWN) {
        var more = el("details", "more");
        more.appendChild(el("summary", null, (list.length - SHOWN) + " more"));
        more.addEventListener("toggle", function once() {
          more.removeEventListener("toggle", once);
          list.slice(SHOWN).forEach(function (w) { more.appendChild(workload(w, money)); });
        });
        body.appendChild(more);
      }
    };
    if (g.open) fill();
    else g.addEventListener("toggle", function once() { g.removeEventListener("toggle", once); fill(); });
    g.appendChild(body);
    return g;
  }

  function workload(w, money) {
    var box = el("details", "wl");
    var sm = el("summary", "wl-head");
    var who = el("div", "wl-who");
    var name = el("div", "wl-name id");
    name.appendChild(el("span", "dim", w.namespace + " / "));
    name.appendChild(document.createTextNode(w.workload));
    who.appendChild(name);
    who.appendChild(el("div", "li-sub", fmt.name(w.kind) + " · " + (w.pods ? fmt.plural(w.pods, "pod") + " now" : "between runs")));
    sm.appendChild(who);
    var f = w.frees, out = f.cpu > 0.0005 || f.memory > 1048576, into = f.cpu < -0.0005 || f.memory < -1048576;
    var eff = el("div", "wl-eff");
    if (out || into) {
      var said = [];
      if (into) said.push("adds " + capacity({ cpu: Math.max(0, -f.cpu), memory: Math.max(0, -f.memory) }));
      if (out) said.push("frees " + capacity({ cpu: Math.max(0, f.cpu), memory: Math.max(0, f.memory) }));
      eff.appendChild(el("div", null, said.join(" · ")));
      eff.appendChild(el("div", "li-sub", w.per_month != null && Math.abs(w.per_month) > 0.005 ? money(Math.abs(w.per_month)) + " a month of the cluster" : "under a cent a month"));
    } else {
      eff.appendChild(el("div", "li-sub", "changes a limit only"));
    }
    sm.appendChild(eff);
    box.appendChild(sm);
    box.addEventListener("toggle", function once() {
      box.removeEventListener("toggle", once);
      var body = el("div", "wl-body");
      // One scale per resource across the workload's containers, so a
      // sidecar's sliver reads as a sliver beside the main container.
      var scale = { cpu: 0, memory: 0 };
      (w.containers || []).forEach(function (c) {
        [["cpu", c.cpu], ["memory", c.memory]].forEach(function (r) {
          var m = r[1];
          if (m) scale[r[0]] = Math.max(scale[r[0]], m.request || 0, m.limit || 0, m.peak || 0, m.p95 || 0, m.recommended || 0, m.recommended_limit || 0);
        });
      });
      (w.containers || []).forEach(function (c) {
        [["cpu", c.cpu], ["memory", c.memory]].forEach(function (r) {
          var m = r[1];
          if (!m || (!m.request && !m.p95 && !m.peak && !m.limit)) return;
          body.appendChild(range(c.name, r[0], m, scale[r[0]] * 1.08));
        });
      });
      var lg = el("div", "legend");
      var marks = { use: 1, ask: 1, lim: 0, set: 0, newlim: 0 };
      (w.containers || []).forEach(function (c) { [c.cpu, c.memory].forEach(function (m) { if (!m) return; if (m.limit) marks.lim = 1; if (m.verdict) marks.set = 1; if (m.verdict && m.recommended_limit) marks.newlim = 1; }); });
      [["use", "use, p95 to peak"], ["ask", "asks now"], ["lim", "limit now"], ["set", "set to"], ["newlim", "new limit"]].forEach(function (k) {
        if (!marks[k[0]]) return;
        var sp = el("span");
        sp.appendChild(el("i", "mk " + k[0]));
        sp.appendChild(document.createTextNode(k[1]));
        lg.appendChild(sp);
      });
      body.appendChild(lg);
      var change = el("div", "change");
      var text = patch(w);
      var bar = el("div", "change-head");
      bar.appendChild(el("span", null, "The change"));
      var copy = el("button", "copy", "Copy");
      copy.type = "button";
      copy.addEventListener("click", function () {
        (navigator.clipboard ? navigator.clipboard.writeText(text) : Promise.reject()).then(function () { copy.textContent = "Copied"; setTimeout(function () { copy.textContent = "Copy"; }, 1600); }, function () { copy.textContent = "Select and copy"; });
        T.event("rightsizing.copy", { workload: w.namespace + "/" + w.workload });
      });
      bar.appendChild(copy);
      change.appendChild(bar);
      change.appendChild(el("pre", null, text));
      body.appendChild(change);
      box.appendChild(body);
    });
    return box;
  }

  // One container's resource on one scale: use from p95 to peak, what it
  // asks, its limit, and what to set.
  function range(container, resource, m, scale) {
    var row = el("div", "rg" + (m.verdict ? " v-" + m.verdict : ""));
    var head = el("div", "rg-head");
    head.appendChild(el("span", "rg-what", container + " · " + (resource === "cpu" ? "CPU" : "memory")));
    var words = [m.request ? "asks " + amount(m.request, resource) : "asks nothing", "uses " + amount(m.p95, resource) + " at p95, " + amount(m.peak, resource) + " at peak"];
    if (m.limit) words.push("limit " + amount(m.limit, resource));
    if (m.verdict) words.push("set to " + amount(m.recommended, resource) + (m.recommended_limit ? (m.limit && Math.abs(m.recommended_limit - m.limit) / m.limit < 0.01 ? ", keep its limit" : " with a limit of " + amount(m.recommended_limit, resource)) : ""));
    head.appendChild(el("span", "rg-nums", words.join(" · ")));
    row.appendChild(head);
    var top = scale || Math.max(m.request || 0, m.limit || 0, m.peak || 0, m.p95 || 0, m.recommended || 0, m.recommended_limit || 0) * 1.08 || 1;
    var at = function (v) { return Math.min(100, v / top * 100).toFixed(2) + "%"; };
    var bar = el("div", "rg-bar");
    var lo = Math.min(m.p95 || m.peak, m.peak || m.p95), hi = Math.max(m.peak || 0, m.p95 || 0);
    var use = el("span", "use");
    use.style.left = at(lo);
    use.style.width = "max(3px, calc(" + at(hi) + " - " + at(lo) + "))";
    bar.appendChild(use);
    function tick(v, cls) { if (!v) return; var t = el("span", "mk " + cls); t.style.left = at(v); bar.appendChild(t); }
    tick(m.limit, "lim");
    tick(m.request, "ask");
    if (m.verdict) { if (m.recommended_limit && Math.abs(m.recommended_limit - (m.limit || 0)) > top * 0.005) tick(m.recommended_limit, "newlim"); tick(m.recommended, "set"); }
    row.appendChild(bar);
    row.appendChild(el("div", "rg-end", amount(top, resource)));
    if (m.reason) row.appendChild(el("div", "rg-reason", m.reason));
    return row;
  }

  // A size as a manifest says it: millicores or cores, Mi or Gi.
  var quantity = {
    cpu: function (c) { var m = Math.round(c * 1000); return m % 1000 === 0 ? String(m / 1000) : m + "m"; },
    memory: function (b) { var mi = Math.round(b / 1048576); return mi % 1024 === 0 && mi > 0 ? mi / 1024 + "Gi" : mi + "Mi"; }
  };

  // The change as the manifest's resources block, every container in full:
  // the advised size where one is advised, the size it has elsewhere.
  function patch(w) {
    var where = { CronJob: "spec.jobTemplate.spec.template.spec", RunnerSet: "the runner template's spec" }[w.kind] || "spec.template.spec";
    var out = ["# " + w.namespace + "/" + w.workload + ", " + where, "containers:"];
    (w.containers || []).forEach(function (c) {
      out.push("  - name: " + c.name, "    resources:");
      var req = [], lim = [];
      [["cpu", c.cpu], ["memory", c.memory]].forEach(function (r) {
        var m = r[1], q = quantity[r[0]];
        if (!m) return;
        var ask = m.verdict ? m.recommended : m.request;
        if (ask) req.push("        " + r[0] + ": " + q(ask));
        var cap = m.verdict && m.recommended_limit ? m.recommended_limit : m.limit;
        if (cap) lim.push("        " + r[0] + ": " + q(cap));
      });
      if (req.length) out.push("      requests:", req.join("\n"));
      if (lim.length) out.push("      limits:", lim.join("\n"));
    });
    return out.join("\n");
  }

  // ---------- collectors

  function collectorsSection(list) {
    var s = section("collectors", "Collectors", null, null, null);
    if (!list.length) { s.appendChild(el("p", "empty", "No collector is on.")); return; }
    var grid = el("div", "collectors");
    list.forEach(function (c) {
      var ok = c.last_success && c.last_run && c.last_success >= c.last_run;
      var state = c.rounds === 0 ? "wait" : ok ? "ok" : "bad";
      var it = el("div", "col " + state);
      it.appendChild(el("i", "dot"));
      it.appendChild(el("span", "col-name", c.name));
      it.appendChild(el("span", "col-when", state === "wait" ? "waiting" : state === "ok" ? fmt.ago(c.last_success) : "failing" + (c.last_success ? ", last read " + fmt.ago(c.last_success) : "")));
      grid.appendChild(it);
    });
    s.appendChild(grid);
  }

  document.addEventListener("DOMContentLoaded", function () {
    T = window.tally; fmt = T.fmt; el = T.el; svg = T.svg; say = T.say;
    host = document.getElementById("view-estate");
    T.view("estate", { show: show });
    setInterval(function () {
      if (T.current() === "estate" && !document.hidden) { T.costs(true); show(); }
    }, 5 * 60 * 1000);
  });
})();
