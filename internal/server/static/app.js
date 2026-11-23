// The board's core: telemetry, formatting, the views' router, the section
// index in the rail, and the costs every view reads. estate.js and agents.js
// each register one view on window.tally.
(function () {
  "use strict";

  // Browser telemetry: Faro, relayed through this server. Console capture is
  // off; spans cover the board's own API only.
  function startFaro() {
    var sdk = window.GrafanaFaroWebSdk;
    if (!sdk) return null;
    var instrumentations = sdk.getWebInstrumentations({ captureConsole: false, enablePerformanceInstrumentation: false });
    var tracing = window.GrafanaFaroWebTracing;
    if (tracing) instrumentations.push(new tracing.TracingInstrumentation({ instrumentationOptions: { ignoreUrls: [/\/api\/telemetry/] } }));
    return sdk.initializeFaro({
      url: "/api/telemetry",
      app: { name: "tally-web" },
      instrumentations: instrumentations,
      sessionTracking: { enabled: true },
      ignoreUrls: [/\/api\/telemetry/]
    });
  }
  var faro = null;
  var opened = performance.now();

  var SVG = "http://www.w3.org/2000/svg";

  function money(v, opts) {
    if (v == null || isNaN(v)) return "n/a";
    // Cents only under $100, where they still say something.
    var whole = (opts && opts.whole) || Math.abs(v) >= 100;
    return new Intl.NumberFormat("en-US", { style: "currency", currency: (opts && opts.currency) || "USD", minimumFractionDigits: whole ? 0 : 2, maximumFractionDigits: whole ? 0 : 2 }).format(v);
  }
  var fmt = {
    money: money,
    // A signed change, such as +$4.10 or −$12.
    delta: function (v) { return (v > 0 ? "+" : v < 0 ? "−" : "") + money(Math.abs(v)); },
    watts: function (w) { return w >= 1000 ? (w / 1000).toFixed(2) + " kW" : Math.round(w) + " W"; },
    cores: function (c) {
      c = Math.abs(c);
      if (c > 0 && c < 0.005) return "<0.01\u00a0cores";
      var n = c >= 10 ? String(Math.round(c)) : c < 0.095 ? c.toFixed(2) : c.toFixed(1).replace(/\.0$/, "");
      return n + (n === "1" ? "\u00a0core" : "\u00a0cores");
    },
    bytes: function (b) {
      b = Math.abs(b);
      var u = ["B", "KiB", "MiB", "GiB", "TiB"], i = 0;
      while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
      return (b >= 9.95 || i === 0 ? Math.round(b) : b.toFixed(1)) + "\u00a0" + u[i];
    },
    tokens: function (n) { return n >= 1e9 ? (n / 1e9).toFixed(1) + "B" : n >= 1e6 ? (n / 1e6).toFixed(1) + "M" : n >= 1e3 ? Math.round(n / 1e3) + "k" : String(Math.round(n)); },
    count: function (n) { return Math.round(n).toLocaleString("en-US"); },
    pct: function (p) { return p > 0 && p < 0.01 ? "<1%" : Math.round(p * 100) + "%"; },
    times: function (x) { return (x >= 10 ? Math.round(x) : x.toFixed(1)) + "×"; },
    ago: function (iso) {
      var s = (Date.now() - new Date(iso).getTime()) / 1000;
      if (s < 60) return "just now";
      if (s < 3600) return Math.round(s / 60) + "m ago";
      if (s < 86400) return Math.round(s / 3600) + "h ago";
      return Math.round(s / 86400) + "d ago";
    },
    // How long until a time, in its largest two units.
    until: function (iso) {
      var ms = new Date(iso).getTime() - Date.now();
      if (ms <= 0) return "now";
      var m = Math.round(ms / 60000), h = Math.floor(m / 60), d = Math.floor(h / 24);
      if (d) return d + "d\u00a0" + (h % 24) + "h";
      if (h) return h + "h\u00a0" + (m % 60) + "m";
      return m + "m";
    },
    // A moment people plan around: today or tomorrow by name, else the weekday.
    when: function (iso) {
      var t = new Date(iso), now = new Date();
      var day = new Date(t.getFullYear(), t.getMonth(), t.getDate()), today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
      var diff = Math.round((day - today) / 86400000);
      // Non-breaking spaces keep a moment on one line.
      var clock = t.toLocaleTimeString("en-US", { hour: "numeric", minute: "2-digit" }).replace(/\s/g, "\u00a0");
      if (diff === 0) return "today\u00a0" + clock;
      if (diff === 1) return "tomorrow\u00a0" + clock;
      if (diff > 1 && diff < 7) return t.toLocaleDateString("en-US", { weekday: "long" }) + "\u00a0" + clock;
      return t.toLocaleDateString("en-US", { month: "short", day: "numeric" }).replace(/\s/g, "\u00a0") + "\u00a0" + clock;
    },
    month: function (period) { var p = period.split("-"); return new Date(+p[0], +p[1] - 1, 1).toLocaleDateString("en-US", { month: "long", year: "numeric" }); },
    monthShort: function (period) { var p = period.split("-"); return new Date(+p[0], +p[1] - 1, 1).toLocaleDateString("en-US", { month: "short" }); },
    plural: function (n, one, many) { return n + "\u00a0" + (n === 1 ? one : (many || one + "s")); },
    // A provider or a Kubernetes kind as its makers write it.
    name: function (s) {
      var known = { openai: "OpenAI", xai: "xAI", claude: "Claude", codex: "Codex", grok: "Grok", anthropic: "Anthropic", github: "GitHub", gitlab: "GitLab", deployment: "Deployment", statefulset: "StatefulSet", daemonset: "DaemonSet", cronjob: "CronJob", job: "Job", runnerset: "RunnerSet", pod: "Pod" };
      return known[(s || "").toLowerCase()] || (s ? s.charAt(0).toUpperCase() + s.slice(1) : s);
    }
  };

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  function svg(tag, attrs) {
    var e = document.createElementNS(SVG, tag);
    for (var k in attrs || {}) e.setAttribute(k, attrs[k]);
    return e;
  }
  // A sentence with figures: parts are strings, or [text, class] for an
  // emphasized figure.
  function say(tag, cls, parts) {
    var e = el(tag, cls);
    parts.forEach(function (p) {
      if (typeof p === "string") e.appendChild(document.createTextNode(p));
      else e.appendChild(el(p[2] || "b", p[1] || null, p[0]));
    });
    return e;
  }

  function getJSON(url) {
    return fetch(url, { headers: { Accept: "application/json" } }).then(function (r) {
      if (r.status === 404) { var e = new Error("off"); e.status = 404; throw e; }
      if (!r.ok) { var f = new Error(url.split("?")[0] + " " + r.status); f.status = r.status; throw f; }
      return r.json();
    });
  }

  // The costs, read once and shared by both views, fresh for a minute.
  var costsAt = 0, costsP = null;
  function costs(force) {
    if (force || !costsP || performance.now() - costsAt > 60000) {
      costsAt = performance.now();
      costsP = getJSON("/v1/costs");
      costsP.then(function (b) {
        document.getElementById("asof").textContent = "As of " + new Date(b.generated_at).toLocaleTimeString("en-US", { hour: "numeric", minute: "2-digit" });
        document.getElementById("version").textContent = "tally " + b.version;
      }, function () {});
    }
    return costsP;
  }

  // A subscription pool's limit over time, as a share of the pool: what was
  // recorded, a "now" rule, the projection at the current burn, and the
  // moments accounts refill or run out. opts.from and opts.to clip it.
  function poolChart(p, opts) {
    opts = opts || {};
    var wrap = el("div", "pchart" + (opts.compact ? " compact" : ""));
    var all = p.recorded.concat(p.projected);
    if (opts.from) all = all.filter(function (q) { return new Date(q.t).getTime() >= opts.from; });
    if (opts.to) all = all.filter(function (q) { return new Date(q.t).getTime() <= opts.to; });
    if (all.length < 2 || !p.capacity) return wrap;
    var nowT = p.projected.length ? new Date(p.projected[0].t).getTime() : Date.now();
    var rec = all.filter(function (q) { return new Date(q.t).getTime() <= nowT; });
    var proj = all.filter(function (q) { return new Date(q.t).getTime() >= nowT; });
    var t0 = new Date(all[0].t).getTime(), t1 = new Date(all[all.length - 1].t).getTime();
    var W = 1000, H = 200, cap = p.capacity;
    var x = function (t) { return (new Date(t).getTime() - t0) / (t1 - t0) * W; };
    var y = function (v) { return H - Math.max(0, Math.min(cap, v)) / cap * H; };
    var path = function (pts) { return pts.map(function (q, i) { return (i ? "L" : "M") + x(q.t).toFixed(1) + " " + y(q.v).toFixed(1); }).join(" "); };
    var g = svg("svg", { viewBox: "0 0 " + W + " " + H, preserveAspectRatio: "none", "aria-hidden": "true" });
    [0, 0.5, 1].forEach(function (f) { g.appendChild(svg("line", { x1: 0, x2: W, y1: y(cap * f), y2: y(cap * f), class: "grid", "vector-effect": "non-scaling-stroke" })); });
    if (rec.length > 1) {
      g.appendChild(svg("path", { d: path(rec) + " L" + x(rec[rec.length - 1].t).toFixed(1) + " " + H + " L" + x(rec[0].t).toFixed(1) + " " + H + " Z", class: "area" }));
      g.appendChild(svg("path", { d: path(rec), class: "rec", "vector-effect": "non-scaling-stroke" }));
    }
    if (proj.length > 1) g.appendChild(svg("path", { d: path(proj), class: "proj", "vector-effect": "non-scaling-stroke" }));
    var nowX = x(new Date(nowT).toISOString());
    g.appendChild(svg("line", { x1: nowX, x2: nowX, y1: 0, y2: H, class: "now", "vector-effect": "non-scaling-stroke" }));
    wrap.appendChild(g);
    function label(text, left, top, cls) {
      var s = el("span", "lbl " + (cls || ""), text);
      s.style.left = (left / W * 100).toFixed(2) + "%";
      s.style.top = (top / H * 100).toFixed(2) + "%";
      wrap.appendChild(s);
    }
    // A mark sits on the line; a refill sits at the top of its step.
    function dot(at, cls, title) {
      var t = new Date(at).getTime();
      if (t < t0 || t > t1) return;
      var d = el("span", "pdot " + cls);
      var v = /refill/.test(cls) ? Math.max(valueAt(all, t), valueAt(all, t + 1200000)) : valueAt(all, t);
      d.style.left = (x(at) / W * 100).toFixed(2) + "%";
      d.style.top = (y(Math.min(cap, v)) / H * 100).toFixed(2) + "%";
      d.title = title;
      wrap.appendChild(d);
    }
    label("100%", 0, 0, "y");
    label("50%", 0, H / 2, "y");
    label("0", 0, H, "y");
    label("now", nowX, 0, "nowlbl");
    // A refill shows only where the line steps up at it.
    (p.refills || []).forEach(function (r) {
      var t = new Date(r.at).getTime();
      var step = valueAt(all, t + 1800000) - valueAt(all, t - 1800000);
      if (step < r.gain * 0.5 || step < cap * 0.05) return;
      dot(r.at, "refill" + (r.projected ? " proj" : ""), "An account refills, " + fmt.when(r.at));
    });
    if (p.first_empty && p.first_empty.at !== p.empty) dot(p.first_empty.at, "out", "An account runs out, " + fmt.when(p.first_empty.at));
    if (p.empty) {
      dot(p.empty, "dry", "The pool runs dry, " + fmt.when(p.empty));
      var te = new Date(p.empty).getTime();
      if (te >= t0 && te <= t1) label("dry " + fmt.when(p.empty), x(p.empty), y(0), "drylbl" + (x(p.empty) > W * 0.6 ? " left" : ""));
    }
    // Days: a faint rule at each midnight, the day's name under its middle.
    var d = new Date(t0);
    d.setHours(0, 0, 0, 0);
    for (var n = 0; d.getTime() < t1; d.setDate(d.getDate() + 1), n++) {
      var mid = new Date(d.getTime()), noon = new Date(d.getTime());
      noon.setHours(12);
      if (mid.getTime() > t0 && Math.abs(mid.getTime() - nowT) > (t1 - t0) * 0.03) g.insertBefore(svg("line", { x1: x(mid.toISOString()), x2: x(mid.toISOString()), y1: 0, y2: H, class: "day", "vector-effect": "non-scaling-stroke" }), g.firstChild);
      var px = x(noon.toISOString());
      if (px < W * 0.04 || px > W * 0.96) continue;
      label(noon.toLocaleDateString("en-US", opts.compact ? { weekday: "short" } : { weekday: "short", day: "numeric" }), px, H, "x" + (n % 2 ? " odd" : ""));
    }
    return wrap;
  }
  function valueAt(points, t) {
    var v = points[0].v;
    for (var i = 0; i < points.length && new Date(points[i].t).getTime() <= t; i++) v = points[i].v;
    return v;
  }
  // The pool chart's key, naming only the marks the pools show.
  function poolLegend(pools, opts) {
    opts = opts || {};
    var has = { refill: false, "refill-proj": false, out: false, dry: false };
    var shown = function (at) { var t = new Date(at).getTime(); return (!opts.from || t >= opts.from) && (!opts.to || t <= opts.to); };
    pools.forEach(function (p) {
      (p.refills || []).forEach(function (r) { if (shown(r.at)) has[r.projected ? "refill-proj" : "refill"] = true; });
      if (p.first_empty && p.first_empty.at !== p.empty && shown(p.first_empty.at)) has.out = true;
      if (p.empty && shown(p.empty)) has.dry = true;
    });
    var lg = el("div", "legend");
    [["rec", "recorded"], ["proj", "projected at the current burn"], ["refill", "an account refilled"], ["refill-proj", "an account will refill"], ["out", "an account runs out"], ["dry", "the pool runs dry"]].forEach(function (k) {
      if (k[0] in has && !has[k[0]]) return;
      var sp = el("span");
      sp.appendChild(el("i", "sw pk-" + k[0]));
      sp.appendChild(document.createTextNode(k[1]));
      lg.appendChild(sp);
    });
    return lg;
  }

  // The rail's index: each section of the open view, with its figure.
  function index(items) {
    var nav = document.getElementById("index"), jump = document.getElementById("jump-list");
    nav.textContent = "";
    jump.textContent = "";
    items.forEach(function (it) {
      // The phone's section menu: the same items, closing once one is picked.
      var j = el("a", "jx" + (it.tone ? " " + it.tone : ""));
      j.href = "#" + it.view + "/" + it.id;
      j.appendChild(el("span", null, it.label));
      j.appendChild(el("span", "ix-v", it.value || ""));
      j.addEventListener("click", function () { document.getElementById("jump").open = false; });
      jump.appendChild(j);
      var a = el("a", "ix" + (it.tone ? " " + it.tone : ""));
      a.href = "#" + it.view + "/" + it.id;
      a.dataset.target = it.id;
      a.appendChild(el("span", "ix-k", it.label));
      a.addEventListener("click", function () { spied(it.id); });
      a.appendChild(el("span", "ix-v", it.value || ""));
      if (it.share != null) {
        var m = el("span", "ix-m");
        var f = el("i");
        f.style.width = Math.max(2, Math.min(100, it.share * 100)).toFixed(1) + "%";
        if (it.color) f.style.background = it.color;
        m.appendChild(f);
        a.appendChild(m);
      }
      nav.appendChild(a);
    });
  }

  // The index marks the section being read.
  var spy = null;
  function spied(id) {
    document.querySelectorAll("#index .ix").forEach(function (a) { if (a.dataset.target === id) a.setAttribute("aria-current", "true"); else a.removeAttribute("aria-current"); });
  }
  function watch(view) {
    if (spy) spy.disconnect();
    if (!("IntersectionObserver" in window)) return;
    spy = new IntersectionObserver(function (entries) {
      entries.forEach(function (e) { if (e.isIntersecting) spied(e.target.id); });
    }, { rootMargin: "-20% 0px -70% 0px" });
    document.querySelectorAll("#view-" + view + " section[id]").forEach(function (s) { spy.observe(s); });
  }

  // Views: #estate (the default) and #agents; #<view>/<section> scrolls to
  // a section of the view.
  var views = {}, current = null;
  function route() {
    var parts = location.hash.replace(/^#/, "").split("/");
    var name = views[parts[0]] ? parts[0] : "estate";
    Object.keys(views).forEach(function (v) {
      document.getElementById("view-" + v).hidden = v !== name;
      var tab = document.getElementById("tab-" + v);
      if (v === name) tab.setAttribute("aria-current", "page"); else tab.removeAttribute("aria-current");
    });
    var first = current !== name;
    current = name;
    var shown = first ? views[name].show() : Promise.resolve();
    Promise.resolve(shown).then(function () {
      watch(name);
      var target = parts[1] && document.getElementById(parts[1]);
      if (target) target.scrollIntoView({ block: "start" });
      else if (first) window.scrollTo(0, 0);
    });
  }

  window.tally = {
    fmt: fmt,
    el: el,
    svg: svg,
    say: say,
    getJSON: getJSON,
    poolChart: poolChart,
    poolLegend: poolLegend,
    costs: costs,
    index: index,
    view: function (name, v) { views[name] = v; },
    current: function () { return current; },
    event: function (name, attrs) { if (faro) faro.api.pushEvent(name, attrs || {}); },
    metric: function (type, values) { if (faro) faro.api.pushMeasurement({ type: type, values: values }); },
    ready: function (type) { if (faro) faro.api.pushMeasurement({ type: type, values: { ready_ms: Math.round(performance.now() - opened) } }); },
    error: function (err) { if (faro) faro.api.pushError(err); }
  };

  // The views register on DOMContentLoaded too; route once they all have.
  document.addEventListener("DOMContentLoaded", function () {
    faro = startFaro();
    window.tally.event("board.open");
    window.addEventListener("hashchange", route);
    setTimeout(route, 0);
  });
})();
