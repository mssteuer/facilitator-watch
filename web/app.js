// Native links work without JavaScript. This also makes the rest of a row clickable.
document.querySelectorAll('tr[data-href]').forEach(row => {
  row.addEventListener('click', event => {
    if (event.target.closest('a, button, details') || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey || window.getSelection().toString()) return;
    window.location.assign(row.dataset.href);
  });
});

// Scheme timeline: aggregated checks per time bucket, fetched from the read-only API.
// Everything that comes from the API is escaped before it reaches innerHTML.
(function () {
  const root = document.getElementById("scheme-timeline");
  if (!root || !root.dataset.api) return;
  const API = root.dataset.api, H = 3600e3, D = 24 * H;
  const RANGES = [["24h", "24h"], ["7d", "7d"], ["30d", "30d"], ["90d", "90d"], ["all", "All"]];
  const MON = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"], p2 = n => String(n).padStart(2, "0");
  const esc = s => String(s).replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
  const num = x => (Number.isFinite(+x) ? +x : 0);
  const ms = s => (s ? Date.parse(s) : NaN);
  const iso = t => new Date(t).toISOString().replace(/\.\d+Z$/, "Z");
  const fmt = (t, o) => {
    const d = new Date(t), m = MON[d.getUTCMonth()];
    return o === "d" ? `${p2(d.getUTCDate())} ${m}` : o === "dy" ? `${p2(d.getUTCDate())} ${m} ${d.getUTCFullYear()}` : `${p2(d.getUTCDate())} ${m} ${p2(d.getUTCHours())}:${p2(d.getUTCMinutes())}`;
  };
  const hm = (t, end) => { const d = new Date(t); return end && t % D === 0 ? "24:00" : `${p2(d.getUTCHours())}:${p2(d.getUTCMinutes())}`; };
  function stepName(s) {
    if (s % (7 * D) === 0) return s === 7 * D ? "1 week" : `${s / (7 * D)} weeks`;
    if (s % D === 0) return s === D ? "1 day" : `${s / D} days`;
    if (s % H === 0) return s === H ? "1 hour" : `${s / H} hours`;
    return `${Math.round(s / 60e3)} min`;
  }

  // ---------- skeleton ----------
  root.innerHTML = `<div class="tl-controls"><div class="tl-ranges" role="group" aria-label="Time range"></div><div class="tl-nav"></div></div>
<p class="tl-rangeline" aria-live="polite"></p>
<div class="tl-state" role="status" hidden></div>
<div class="tl-body"><p class="tl-hint" aria-hidden="true">← scroll for earlier</p>
<div class="tl-scroll" role="region" aria-label="Scheme declaration timeline" aria-describedby="scheme-timeline-kbd"><div class="tl-chart"></div></div>
<p class="sr-only" id="scheme-timeline-kbd">Each lane is a row of time buckets. Use the arrow keys to move between buckets and lanes, Home and End to jump to the first or last bucket, and Enter to zoom into a bucket.</p>
<div class="tl-legend">
  <span><i class="tl-sw tl-s-a"></i>Advertised (height = share of checks)</span>
  <span><i class="tl-sw tl-s-f"></i>Failed check</span>
  <span><i class="tl-sw tl-s-o"></i>Reachable, not listed</span>
  <span><i class="tl-sw tl-mk"></i>First seen</span>
  <span><i class="tl-sw tl-nd"></i>No checks</span>
  <span><i class="tl-sw tl-s-h"></i>Share of failed checks</span>
</div></div>`;
  const q = s => root.querySelector(s);
  const rangesEl = q(".tl-ranges"), navEl = q(".tl-nav"), lineEl = q(".tl-rangeline"), stateEl = q(".tl-state"), bodyEl = q(".tl-body"), scroller = q(".tl-scroll"), chart = q(".tl-chart");
  const subEl = document.getElementById("scheme-timeline-sub"), zoomHint = document.getElementById("scheme-timeline-zoomhint");
  const tip = document.createElement("div");
  tip.className = "tl-tip"; tip.setAttribute("aria-hidden", "true");
  document.body.appendChild(tip);
  rangesEl.innerHTML = RANGES.map(([k, l]) => `<button type="button" aria-pressed="false" data-r="${k}">${l}</button>`).join("");

  // ---------- state + data ----------
  const state = { range: "30d", drill: false, from: 0, to: 0, span: D };
  let model = null, nowMs = 0, firstCheck = NaN, ctl = null, seq = 0;
  let pinEnd = true, pointerCell = null, focusCell = null, touchCell = null, lastPointer = "mouse";

  function parse(env) {
    const d = env && env.data;
    if (!d || !Array.isArray(d.buckets) || !Array.isArray(d.lanes)) throw new Error("unexpected response");
    const buckets = d.buckets.map(b => ({ t0: ms(b.start), t1: ms(b.end), n: num(b.checks), f: num(b.failed), l: [] }));
    const lanes = d.lanes.map(l => ({ name: String(l.scheme), since: ms(l.first_seen) }));
    d.lanes.forEach((l, li) => buckets.forEach((b, i) => {
      const x = (Array.isArray(l.buckets) && l.buckets[i]) || {};
      b.l[li] = { a: num(x.advertised), o: num(x.omitted), f: num(x.failed), n: num(x.before), first: !!x.first_seen };
    }));
    if (buckets.some(b => !Number.isFinite(b.t0) || !Number.isFinite(b.t1))) throw new Error("bad bucket time");
    const from = ms(d.from), to = ms(d.to);
    return { buckets, lanes, step: num(d.step_seconds) * 1000 || H, from: Number.isFinite(from) ? from : (buckets[0] ? buckets[0].t0 : 0), to: Number.isFinite(to) ? to : Date.now(), first: ms(d.first_check) };
  }
  function url() {
    const p = state.drill ? `start=${encodeURIComponent(iso(state.from))}&end=${encodeURIComponent(iso(state.to))}` : `range=${state.range}`;
    return `${API}?${p}`;
  }
  async function load(opt = {}) {
    const my = ++seq;
    if (ctl) ctl.abort();
    ctl = new AbortController();
    root.classList.add("tl-loading"); root.setAttribute("aria-busy", "true");
    if (!model) showState("Loading…");
    try {
      const res = await fetch(url(), { signal: ctl.signal, headers: { Accept: "application/json" } });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const m = parse(await res.json());
      if (my !== seq) return;
      model = m; nowMs = Math.max(nowMs, m.to);
      if (Number.isFinite(m.first)) firstCheck = m.first;
      root.classList.remove("tl-loading"); root.removeAttribute("aria-busy");
      render(opt);
    } catch (err) {
      if (my !== seq || (err && err.name === "AbortError")) return;
      root.classList.remove("tl-loading"); root.removeAttribute("aria-busy");
      model = null; hideTip();
      showState("Couldn’t load the scheme timeline.", true);
      renderNav(); renderRanges();
    }
  }
  function showState(msg, retry) {
    bodyEl.hidden = true; zoomHint.hidden = true;
    stateEl.hidden = false; stateEl.textContent = "";
    const p = document.createElement("p"); p.textContent = msg; stateEl.appendChild(p);
    if (retry) {
      const b = document.createElement("button"); b.type = "button"; b.textContent = "Retry"; b.dataset.retry = "1"; stateEl.appendChild(b);
    }
    lineEl.textContent = "";
  }
  stateEl.addEventListener("click", e => { if (e.target.closest("[data-retry]")) load(); });

  const HEALTH = () => model.lanes.length;
  const rowName = r => (r === HEALTH() ? "check health" : model.lanes[r].name);
  function when(b, step) {
    if (step >= 7 * D) return { date: `${fmt(b.t0, "d")} – ${fmt(b.t1 - 1, "dy")}`, time: "UTC" };
    if (step >= D) return { date: fmt(b.t0, "dy"), time: "UTC day" };
    return { date: fmt(b.t0, "dy"), time: `${hm(b.t0)}–${hm(b.t1, true)} UTC` };
  }
  function bucketText(b, r, step) {
    const w = when(b, step), head = `${rowName(r)}, ${w.date} ${w.time}`;
    if (!b.n) return `${head}: no checks.`;
    if (r === HEALTH()) return `${head}: ${b.n} checks, ${b.f} failed.`;
    const L = b.l[r];
    return `${head}: ${L.a} advertised, ${L.f} failed, ${L.o} not in declaration${L.n ? `, ${L.n} before first seen` : ""}, of ${b.n} checks${L.first ? ". First seen in this bucket" : ""}.`;
  }
  function tipHtml(b, r, step, zoomable) {
    const w = when(b, step), pc = x => Math.round(x / b.n * 100) + "%";
    const head = `<div class="tl-tt-h"><b>${esc(w.date)}</b><span>${esc(w.time)}</span></div><div class="tl-tt-lane">${esc(rowName(r))}</div>`;
    if (!b.n) return head + `<p class="tl-tt-msg">No checks in this bucket.<br>We weren’t checking then.</p>`;
    const row = (cls, label, x) => `<dt class="${x ? "" : "tl-zero"}"><i class="tl-sw ${cls}"></i>${label}</dt><dd class="${x ? "" : "tl-zero"}">${x}</dd><dd class="${x ? "" : "tl-zero"}">${pc(x)}</dd>`;
    let body;
    if (r === HEALTH()) {
      body = `<dl class="tl-tt-rows">${row("tl-s-h", "Failed", b.f)}</dl>` + (b.f ? `<div class="tl-tt-note">Earlier evidence is kept.</div>` : "");
    } else {
      const L = b.l[r];
      body = `<dl class="tl-tt-rows">${row("tl-s-a", "Advertised", L.a)}${row("tl-s-f", "Failed", L.f)}${row("tl-s-o", "Not listed", L.o)}${L.n ? `<dt><i class="tl-sw"></i>Before first seen</dt><dd>${L.n}</dd><dd>${pc(L.n)}</dd>` : ""}</dl>` + (L.first ? `<div class="tl-tt-first"><i class="tl-sw tl-mk"></i>First seen in this bucket</div>` : "");
    }
    return head + body + `<div class="tl-tt-foot">Share of ${b.n.toLocaleString("en")} checks${zoomable ? " · click or Enter to zoom" : ""}</div>`;
  }

  // ---------- render ----------
  function renderRanges() {
    rangesEl.querySelectorAll("button").forEach(b => b.setAttribute("aria-pressed", String(!state.drill && state.range === b.dataset.r)));
  }
  function renderNav() {
    const step = model ? model.step : H, zoomable = step > H;
    if (state.drill) {
      const one = state.span === D, label = one ? fmt(state.from, "dy") : `${fmt(state.from, "d")} – ${fmt(state.to - 1, "dy")}`;
      const prevOff = !Number.isFinite(firstCheck) || state.from <= Math.floor(firstCheck / D) * D, nextOff = state.to >= nowMs;
      navEl.innerHTML = `<div class="tl-stepper" role="group" aria-label="${one ? "Day" : "Period"}"><button type="button" id="scheme-timeline-prev" aria-label="Previous ${one ? "day" : "period"}" title="Previous ${one ? "day" : "period"}" ${prevOff ? "disabled" : ""}>‹</button><span class="tl-day"><b>${esc(label)}</b>${esc(stepName(step))} buckets</span><button type="button" id="scheme-timeline-next" aria-label="Next ${one ? "day" : "period"}" title="Next ${one ? "day" : "period"}" ${nextOff ? "disabled" : ""}>›</button></div><button type="button" class="tl-back" id="scheme-timeline-back">← Back to ${esc(state.range === "all" ? "All" : state.range)}</button>`;
    } else {
      navEl.innerHTML = model && model.lanes.length ? `<span>Bucket <b>${esc(stepName(step))}</b>${zoomable ? " · click to zoom" : ""}</span>` : "";
    }
  }
  function render(opt = {}) {
    const keep = focusKey(), { buckets, lanes, step, from, to } = model, n = buckets.length, zoomable = step > H, HL = lanes.length;
    model.zoomable = zoomable;
    pointerCell = focusCell = touchCell = null; hideTip();
    renderRanges(); renderNav();
    subEl && (subEl.textContent = `Aggregated per ${stepName(step)} / UTC`);
    if (!lanes.length || !n) {
      showState("Nothing to show yet.");
      renderNav();
      return;
    }
    stateEl.hidden = true; bodyEl.hidden = false; zoomHint.hidden = !zoomable;
    const tot = buckets.reduce((a, b) => a + b.n, 0), fail = buckets.reduce((a, b) => a + b.f, 0);
    const edge = t => (step < D ? `${fmt(t, "dy")} ${hm(t)}` : fmt(t, "dy"));
    lineEl.innerHTML = `<b>${esc(edge(from))}</b> → <b>${esc(edge(to))}</b> · ${tot.toLocaleString("en")} checks · ${fail} failed (${tot ? (fail / tot * 100).toFixed(1) : 0}%) · ${n} × ${esc(stepName(step))}`;

    const cell = (b, i, r) => {
      let inner = "";
      if (b.n) {
        if (r === HL) { inner = b.f ? `<span class="tl-s tl-s-h" style="height:${(b.f / b.n * 92 + 8).toFixed(2)}%"></span>` : ""; }
        else {
          const L = b.l[r], h = x => (x / b.n * 100).toFixed(2);
          inner = (L.o ? `<span class="tl-s tl-s-o" style="height:${h(L.o)}%"></span>` : "") + (L.f ? `<span class="tl-s tl-s-f" style="height:${h(L.f)}%"></span>` : "") + (L.a ? `<span class="tl-s tl-s-a" style="height:${h(L.a)}%"></span>` : "");
          if (L.first) inner += `<span class="tl-pin${i > n * .72 ? " tl-flip" : ""}" aria-hidden="true"><span>first seen</span></span>`;
        }
      }
      return `<button type="button" class="tl-cell${b.n ? "" : " tl-nodata"}${zoomable && b.n ? "" : " tl-static"}" tabindex="-1" data-row="${r}" data-i="${i}" aria-label="${esc(bucketText(b, r, step))}">${inner}</button>`;
    };
    const laneSum = li => { let a = 0, t = 0; buckets.forEach(b => { const L = b.l[li]; a += L.a; t += L.a + L.o + L.f; }); return t ? (a / t * 100).toFixed(1) : null; };
    const row = (r, name, sub, val, unit, track) => `<div class="tl-row tl-lane${r === HL ? " tl-health" : ""}"><div class="tl-lane-name"><strong>${esc(name)}</strong><small>${esc(sub)}</small><span class="tl-inline" aria-hidden="true"><b>${esc(val)}</b> ${esc(unit)}</span></div><div class="tl-track${r === HL ? " tl-health" : ""}" role="group" aria-label="${esc(name)}">${track}</div><div class="tl-sum"><b>${esc(val)}</b><small>${esc(unit)}</small></div></div>`;
    const rows = lanes.map((l, li) => {
      const s = laneSum(li);
      return row(li, l.name, l.since <= to ? `first seen ${fmt(l.since, "dy")}` : "not yet observed", s ? s + "%" : "–", s ? "of checks" : "no checks", buckets.map((b, i) => cell(b, i, li)).join(""));
    }).join("");
    const health = row(HL, "check health", "failed GET /supported", `${tot ? (fail / tot * 100).toFixed(1) : 0}%`, "failed", buckets.map((b, i) => cell(b, i, HL)).join(""));
    chart.style.setProperty("--n", n);
    chart.innerHTML = `<div class="tl-row tl-axis" aria-hidden="true"><span class="tl-axl">Scheme</span><div class="tl-axis-track"></div><span class="tl-axr">Advertised</span></div>` + rows + health + `<div class="tl-band" aria-hidden="true"></div>`;

    layout();
    // roving tabindex: one tab stop in the chart, on the most recent bucket of the first lane
    const kc = keep && keep.startsWith("c:") ? keep.split(":") : null;
    cellAt(kc ? Math.min(HL, +kc[1]) : 0, kc ? Math.min(n - 1, +kc[2]) : n - 1).tabIndex = 0;
    restoreFocus(keep, opt);
    pinEnd = true; scroller.scrollLeft = scroller.scrollWidth;   // newest bucket in view on narrow screens
  }
  const cellAt = (r, i) => chart.querySelector(`.tl-cell[data-row="${r}"][data-i="${i}"]`);

  // gap, axis ticks and the narrow-layout header width all depend on measured widths
  function layout() {
    if (!model || bodyEl.hidden || !chart.querySelector(".tl-track")) return;
    const { buckets, step } = model, n = buckets.length;
    chart.style.setProperty("--vw", scroller.clientWidth + "px");
    const track = chart.querySelector(".tl-track"), tw = track.clientWidth, cw = tw / n;
    chart.style.setProperty("--gap", (cw >= 22 ? 3 : cw >= 6 ? 2 : 1) + "px");
    const ticks = pickTicks(tw), ax = chart.querySelector(".tl-axis-track"), ar = ax.getBoundingClientRect();
    ax.innerHTML = ticks.map(i => {
      const t = buckets[i].t0, major = step < D ? t % D === 0 : new Date(t).getUTCDate() === 1;
      return `<span class="tl-tick${major ? " tl-major" : ""}">${step < D && t % D !== 0 ? hm(t) : fmt(t, "d")}</span>`;
    }).join("");
    // sub-day labels name an instant, so they sit on the bucket's left edge; day labels name the whole bucket
    const g = parseFloat(getComputedStyle(chart).getPropertyValue("--gap")) || 0;
    let prevRight = -1e9;
    [...ax.children].forEach((el, k) => {
      const cr = track.children[ticks[k]].getBoundingClientRect(), cx = Math.max(0, (step < D ? cr.left - g / 2 : cr.left + cr.width / 2) - ar.left), w = el.offsetWidth;
      const l = Math.max(0, Math.min(ar.width - w, cx - w / 2));
      if (l < prevRight + 12) { el.remove(); return; }
      prevRight = l + w; el.style.left = l + "px"; el.style.setProperty("--tx", (cx - l).toFixed(1) + "px");
    });
    root.classList.toggle("tl-overflowing", scroller.scrollWidth > scroller.clientWidth + 1);
    if (pinEnd) scroller.scrollLeft = scroller.scrollWidth;
    sync();
  }
  // ticks land on calendar boundaries: hours, midnights, Mondays, 1st/15th, months
  function pickTicks(tw) {
    const { buckets, step } = model, ts = buckets.map(b => b.t0), max = Math.max(2, Math.floor(tw / 80)), dt = t => new Date(t);
    const preds = [];
    if (step < D) [1, 2, 3, 4, 6, 12].forEach(h => { if (h * H >= step) preds.push(t => t % (h * H) === 0); });
    preds.push(t => t % D === 0, t => t % D === 0 && dt(t).getUTCDay() === 1, t => t % D === 0 && [1, 15].includes(dt(t).getUTCDate()), t => t % D === 0 && dt(t).getUTCDate() === 1, t => t % D === 0 && dt(t).getUTCDate() === 1 && dt(t).getUTCMonth() % 3 === 0);
    preds.push((t, i) => i > 0 && dt(t).getUTCMonth() !== dt(ts[i - 1]).getUTCMonth());
    for (const p of preds) { const idx = ts.map((t, i) => (p(t, i) ? i : -1)).filter(i => i >= 0); if (idx.length >= 2 && idx.length <= max) return idx; }
    const k = Math.ceil(ts.length / max); return ts.map((_, i) => i).filter(i => i % k === 0);
  }

  // ---------- hover, focus, tooltip ----------
  function sync() {
    const band = chart.querySelector(".tl-band"), c = pointerCell || focusCell;
    if (!band) return;
    if (!c || !c.isConnected) { band.classList.remove("tl-on", "tl-kbd"); hideTip(); return; }
    const ch = chart.getBoundingClientRect(), cr = c.getBoundingClientRect(), first = chart.querySelector(".tl-row.tl-lane").getBoundingClientRect();
    const g = parseFloat(getComputedStyle(chart).getPropertyValue("--gap")) || 0;
    band.style.left = (cr.left - ch.left - g / 2) + "px"; band.style.width = (cr.width + g) + "px"; band.style.top = (first.top - ch.top) + "px";
    band.classList.add("tl-on"); band.classList.toggle("tl-kbd", c === focusCell && !pointerCell);
    showTip(c);
  }
  function showTip(c) {
    const b = model.buckets[+c.dataset.i], r = +c.dataset.row;
    tip.innerHTML = tipHtml(b, r, model.step, model.zoomable && b.n > 0) + `<i class="tl-arrow"></i>`;
    const cr = c.getBoundingClientRect(), tr = c.parentElement.getBoundingClientRect(), vw = document.documentElement.clientWidth, vh = innerHeight, m = 8;
    const w = tip.offsetWidth, h = tip.offsetHeight, cx = cr.left + cr.width / 2;
    const left = Math.round(Math.max(m, Math.min(vw - w - m, cx - w / 2)));
    let top = tr.top - h - 12, below = false;
    if (top < m) { top = tr.bottom + 12; below = true; if (top + h > vh - m) top = Math.max(m, vh - h - m); }
    tip.style.left = left + "px"; tip.style.top = Math.round(top) + "px";
    tip.style.setProperty("--ax", Math.max(12, Math.min(w - 12, cx - left)) + "px");
    tip.classList.toggle("tl-below", below); tip.classList.add("tl-on");
  }
  function hideTip() { tip.classList.remove("tl-on"); }

  chart.addEventListener("pointerdown", e => { lastPointer = e.pointerType; });
  chart.addEventListener("pointerover", e => {
    if (e.pointerType === "touch") return;
    const c = e.target.closest(".tl-cell");
    if (c) { if (c !== pointerCell) { pointerCell = c; sync(); } }
    else if (!e.target.closest(".tl-track") && pointerCell) { pointerCell = null; sync(); }   // gaps between cells keep the column
  });
  chart.addEventListener("pointerleave", e => { if (e.pointerType !== "touch") { pointerCell = null; sync(); } });
  chart.addEventListener("focusin", e => { const c = e.target.closest(".tl-cell"); focusCell = c && c.matches(":focus-visible") ? c : null; sync(); });
  chart.addEventListener("focusout", e => { if (!(e.relatedTarget && e.relatedTarget.closest && e.relatedTarget.closest(".tl-cell"))) { focusCell = null; sync(); } });
  document.addEventListener("pointerdown", e => { if (touchCell && !chart.contains(e.target)) { touchCell = pointerCell = null; sync(); } });
  scroller.addEventListener("scroll", () => { pinEnd = scroller.scrollLeft >= scroller.scrollWidth - scroller.clientWidth - 2; if (pointerCell || focusCell) sync(); }, { passive: true });
  addEventListener("scroll", () => { if (pointerCell || focusCell) sync(); }, { passive: true });
  let rz = 0; addEventListener("resize", () => { cancelAnimationFrame(rz); rz = requestAnimationFrame(layout); });

  chart.addEventListener("click", e => {
    const c = e.target.closest(".tl-cell"); if (!c || !model) return;
    const kb = e.detail === 0;
    if (!kb && lastPointer === "touch" && touchCell !== c) { touchCell = pointerCell = c; sync(); return; }   // first tap previews, second tap zooms
    const b = model.buckets[+c.dataset.i], r = +c.dataset.row, step = model.step;
    if (!model.zoomable || !b.n) return;
    state.drill = true; state.from = step >= D ? b.t0 : Math.floor(b.t0 / D) * D;
    state.span = step >= 7 * D ? 7 * D : D; state.to = Math.min(state.from + state.span, nowMs);
    load({ focus: kb ? `c:${r}:0` : null }); renderRanges();
  });
  chart.addEventListener("keydown", e => {
    const c = e.target.closest(".tl-cell"); if (!c) return;
    let r = +c.dataset.row, i = +c.dataset.i; const n = model.buckets.length;
    switch (e.key) {
      case "ArrowLeft": i--; break; case "ArrowRight": i++; break; case "ArrowUp": r--; break; case "ArrowDown": r++; break;
      case "Home": i = 0; break; case "End": i = n - 1; break;
      case "Escape": hideTip(); return;
      default: return;
    }
    e.preventDefault();
    const t = cellAt(Math.max(0, Math.min(HEALTH(), r)), Math.max(0, Math.min(n - 1, i)));
    c.tabIndex = -1; t.tabIndex = 0; t.focus();
  });

  rangesEl.addEventListener("click", e => { const b = e.target.closest("button"); if (!b) return; state.drill = false; state.range = b.dataset.r; renderRanges(); load(); });
  navEl.addEventListener("click", e => {
    const b = e.target.closest("button"); if (!b || b.disabled) return;
    if (b.id === "scheme-timeline-back") { state.drill = false; renderRanges(); load({ focus: `r:${state.range}` }); return; }
    const dir = b.id === "scheme-timeline-prev" ? -1 : 1;
    state.from += dir * state.span; state.to = Math.min(state.from + state.span, nowMs);
    load();
  });

  // keep keyboard focus on the equivalent control across re-renders
  function focusKey() {
    const a = document.activeElement; if (!a || !a.closest || !root.contains(a)) return null;
    if (a.dataset.r) return `r:${a.dataset.r}`;
    if (a.classList.contains("tl-cell")) return `c:${a.dataset.row}:${a.dataset.i}`;
    return a.id ? `id:${a.id}` : null;
  }
  function restoreFocus(keep, opt) {
    const k = opt.focus || keep; if (!k) return;
    let el = null;
    if (k.startsWith("r:")) el = rangesEl.querySelector(`[data-r="${k.slice(2)}"]`);
    else if (k.startsWith("c:")) { const [, r, i] = k.split(":"); el = cellAt(+r, Math.min(model.buckets.length - 1, +i)); if (el) { chart.querySelectorAll('.tl-cell[tabindex="0"]').forEach(x => { x.tabIndex = -1; }); el.tabIndex = 0; } }
    else if (k.startsWith("id:")) el = document.getElementById(k.slice(3));
    if (!el || el.disabled) el = document.getElementById("scheme-timeline-back") || rangesEl.querySelector('[aria-pressed="true"]');
    if (el) el.focus({ preventScroll: true });
  }

  renderRanges();
  load();
})();
