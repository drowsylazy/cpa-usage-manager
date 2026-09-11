// ---------- 页签调度 ----------
const loaders = {};
let activeTab = 'overview';
function switchTab(name) {
  activeTab = name;
  // roving tabindex：选中项才进 Tab 序，其余用方向键在 tablist 内移动（ARIA tabs 模式）
  document.querySelectorAll('.tab').forEach(t => {
    const on = t.dataset.tab === name;
    t.setAttribute('aria-selected', String(on));
    t.tabIndex = on ? 0 : -1;
  });
  document.querySelectorAll('.view').forEach(v => { v.hidden = v.id !== 'view-' + name; });
  reloadActive();
}
function reloadActive() {
  const fn = loaders[activeTab];
  if (fn) fn().catch(e => toast(e.message, 'err'));
}
document.querySelectorAll('.tab').forEach(t => t.addEventListener('click', () => switchTab(t.dataset.tab)));
$('tabs').addEventListener('keydown', e => {
  const tabs = [...document.querySelectorAll('.tab')];
  const i = tabs.findIndex(t => t.dataset.tab === activeTab);
  if (i < 0) return;
  let to = -1;
  if (e.key === 'ArrowRight') to = (i + 1) % tabs.length;
  else if (e.key === 'ArrowLeft') to = (i - 1 + tabs.length) % tabs.length;
  else if (e.key === 'Home') to = 0;
  else if (e.key === 'End') to = tabs.length - 1;
  if (to < 0) return;
  e.preventDefault();
  switchTab(tabs[to].dataset.tab);
  tabs[to].focus();
});
// Escape 统一关闭浮层：先关下拉，再关时间范围弹层（原先范围弹层无法用键盘关闭）
document.addEventListener('keydown', e => {
  if (e.key !== 'Escape') return;
  if (openSel) { closeAnySel(true); return; }
  if (!$('settings-pop').hidden) {
    closeSettingsPop();
    $('settings-btn').focus();
    return;
  }
  if (!$('range-pop').hidden) {
    closeRangePop();
    $('range-btn').focus();
  }
});
$('refresh-btn').addEventListener('click', reloadActive);
$('logout-btn').addEventListener('click', logout);
function stamp() { $('stamp').textContent = '更新于 ' + new Date().toLocaleTimeString('zh-CN', { hour12: false }); }

// ---------- 概览 ----------
const GRAINS = [
  { value: 'minute', label: '按分钟' },
  { value: 'hour', label: '按小时' },
  { value: 'day', label: '按天' },
  { value: 'week', label: '按周' },
  { value: 'month', label: '按月' },
];
const trend = { points: [], off: new Set(), grainManual: false, view: 'chart' };

// Token 口径：上游 total 缺失（0）时按计费四类合计兜底；
// 缓存命中取「Claude 口径读写」与「OpenAI/Gemini 口径 cached」的较大者，避免双计。
function effTokens(r) {
  const t = Number(r.total_tokens) || 0;
  if (t > 0) return t;
  return (Number(r.input_tokens) || 0) + (Number(r.output_tokens) || 0)
    + (Number(r.cache_read_tokens) || 0) + (Number(r.cache_creation_tokens) || 0);
}
function cacheHit(r) {
  return Math.max((Number(r.cache_read_tokens) || 0) + (Number(r.cache_creation_tokens) || 0),
    Number(r.cached_tokens) || 0);
}
// cacheReadOf 统一两种上游口径的「缓存读」：Claude 的 cache_read 独立于输入，
// OpenAI/Gemini 的 cached 含在输入内。取较大者，避免双计，与缓存命中率同口径。
function cacheReadOf(r) {
  return Math.max(Number(r.cache_read_tokens) || 0, Number(r.cached_tokens) || 0);
}

loaders.overview = async () => {
  if (!trend.grainManual) trendGrainSel.value = autoGrain();
  const p = rangeParams();
  // 汇率与四路数据并发拉取（原先串行在 Promise.all 之后，多付一个 RTT）。
  const [dimModel, dimKey, points, costs, fx] = await Promise.all([
    api('/usage/dimension?' + new URLSearchParams({ dimension: 'model', limit: '50', ...p })),
    api('/usage/dimension?' + new URLSearchParams({ dimension: 'key_id', limit: '50', ...p })),
    api('/trends?' + new URLSearchParams({ grain: trendGrainSel.value, ...p })),
    api('/costs?' + new URLSearchParams(p)),
    S.fx ? null : api('/exchange-rate').catch(() => null),
  ]);
  if (!S.fx && fx) S.fx = fx;
  trend.points = Array.isArray(points) ? points : [];
  ovCache.models = dimModel.rows || [];
  ovCache.modelCount = dimModel.count;
  ovCache.keys = dimKey.rows || [];
  ovCache.keyCount = dimKey.count;
  renderReadouts(dimModel.total || {}, costs);
  renderModels(ovCache.models);
  renderKeySpend(ovCache.keys);
  renderCostCoverage(costs);
  renderTrend();
  stamp();
};

function readout(label, value, sub, alarm) {
  return '<div class="readout' + (alarm ? ' alarm' : '') + '"><div class="readout-top">'
    + '<span class="readout-label">' + label + '</span></div>'
    + '<div class="readout-value">' + value + '</div>'
    + (sub ? '<div class="readout-sub">' + sub + '</div>' : '') + '</div>';
}
// cacheHitRate 缓存命中率 = 命中 token / (输入 + 缓存读 + 缓存写)。
// OpenAI 口径的 cached_tokens 已含在输入内，Claude 口径的 cache_read 独立，分母对两者均成立。
function cacheHitRate(t) {
  const denom = (+t.input_tokens || 0) + (+t.cache_read_tokens || 0) + (+t.cache_creation_tokens || 0);
  return denom > 0 ? cacheHit(t) / denom * 100 : -1;
}
function renderReadouts(total, costs) {
  const failRate = total.requests ? (total.failures / total.requests * 100).toFixed(1) + '%' : '0%';
  const cover = costs.requests ? Math.round(costs.priced_requests / costs.requests * 100) : 0;
  const hitPct = cacheHitRate(total);
  $('ov-readouts').innerHTML =
    readout('请求总数', fmtInt(total.requests),
      '失败 <b>' + fmtInt(total.failures) + '</b> · 失败率 ' + failRate,
      total.requests > 0 && total.failures / total.requests > 0.05)
    + readout('总消耗 Token', effTokens(total).toLocaleString('zh-CN'),
      '输入 <b>' + fmtTok(total.input_tokens) + '</b> · 输出 <b>' + fmtTok(total.output_tokens)
      + '</b> · 缓存命中 <b>' + fmtTok(cacheHit(total)) + '</b>')
    + readout('总费用', fmtCur(total.cost_micro_usd),
      '计价覆盖 <b>' + cover + '%</b>')
    + readout('缓存命中率', hitPct < 0 ? '—' : hitPct.toFixed(1) + '%',
      '读 <b>' + fmtTok(cacheReadOf(total)) + '</b> · 写 <b>' + fmtTok(total.cache_creation_tokens)
      + '</b>' + ((total.cached_tokens || 0) > (total.cache_read_tokens || 0) + (total.cache_creation_tokens || 0)
        ? ' · 含上游缓存口径' : ''));
}

// ---------- 概览占比卡：指标切换（费用 / Token / 请求） ----------
const ovCache = { models: [], modelCount: null, keys: [], keyCount: null };
const ovMetric = {
  models: localStorage.getItem('ov-models-metric') || 'tokens',
  keys: localStorage.getItem('ov-keys-metric') || 'cost',
};
function metricVal(r, m) {
  if (m === 'cost') return Number(r.cost_micro_usd) || 0;
  if (m === 'requests') return Number(r.requests) || 0;
  return effTokens(r);
}
function metricText(v, m) {
  return m === 'cost' ? fmtCur(v) : m === 'tokens' ? fmtTok(v) : fmtInt(v);
}
const METRIC_SUBS = { tokens: '按 Token 计量', cost: '按费用计量', requests: '按请求次数计量' };
function bindMetricSeg(id, key, apply) {
  const seg = $(id);
  seg.querySelectorAll('button').forEach(b =>
    b.classList.toggle('on', b.dataset.m === ovMetric[key]));
  seg.addEventListener('click', e => {
    const b = e.target.closest('button[data-m]');
    if (!b || b.dataset.m === ovMetric[key]) return;
    ovMetric[key] = b.dataset.m;
    savePref(id, b.dataset.m);
    seg.querySelectorAll('button').forEach(x => x.classList.toggle('on', x === b));
    apply();
  });
}
bindMetricSeg('ov-models-metric', 'models', () => renderModels(ovCache.models));
bindMetricSeg('ov-keys-metric', 'keys', () => renderKeySpend(ovCache.keys));

// ---------- 概览圆环图 ----------
// DONUT_COLORS 是环分段配色（Tableau 定性色板，深浅主题均可辨）；第 6 段起并入「其他」。
const DONUT_COLORS = ['#4e79a7', '#f28e2b', '#59a14f', '#e15759', '#b07aa1'];
const DONUT_OTHER = '#9aa5b1';
function donutEntries(rows, metric) {
  const sorted = rows.filter(r => metricVal(r, metric) > 0)
    .sort((a, b) => metricVal(b, metric) - metricVal(a, metric));
  const top = sorted.slice(0, 5).map((r, i) => ({
    label: r.value || '(空)', color: DONUT_COLORS[i],
    value: metricVal(r, metric), cost: r.cost_micro_usd, requests: r.requests,
  }));
  const rest = sorted.slice(5);
  if (rest.length) {
    top.push({
      label: '其他（' + rest.length + ' 项）', color: DONUT_OTHER,
      value: rest.reduce((a, r) => a + metricVal(r, metric), 0),
      cost: rest.reduce((a, r) => a + (Number(r.cost_micro_usd) || 0), 0),
      requests: rest.reduce((a, r) => a + (Number(r.requests) || 0), 0),
    });
  }
  return { items: top, total: sorted.reduce((a, r) => a + metricVal(r, metric), 0), count: sorted.length };
}
// drawDonut 渲染单圆环（默认前 5 + 其他合并段）。
//
// 入场动画是**顺时针单前沿扫描**：每段的过渡时长与其弧长占比成正比、延迟为
// 前序段时长之和，衔接处速度一致 —— 视觉上只有一个前沿从顶部顺时针推进，
// 走到哪里哪段显色，而不是各段各自冒出来。
const DONUT_ANIM_MS = 550;
function drawDonut(mountId, entries, fmt) {
  const mount = $(mountId);
  const total = entries.total;
  if (!total || !entries.items.length) {
    mount.innerHTML = '<div class="empty"><p class="empty-title">暂无数据</p>'
      + '<p class="empty-hint">所选时间范围内没有可统计的记录</p></div>';
    return;
  }
  const R = 42, C = 2 * Math.PI * R;
  let offset = 0;
  const segs = entries.items.map((it, i) => {
    const frac = it.value / total;
    const seg = '<circle class="donut-seg" data-i="' + i + '" cx="60" cy="60" r="' + R + '" fill="none"'
      + ' stroke="' + it.color + '" stroke-width="17" stroke-linecap="butt"'
      + ' style="stroke-dasharray:0 ' + (C + 10).toFixed(2) + ';stroke-dashoffset:' + (-offset).toFixed(2) + '"'
      + ' tabindex="0"><title>' + esc(it.label + ' · ' + metricText(it.value, fmt.metric)
        + ' · ' + (frac * 100).toFixed(1) + '%') + '</title></circle>';
    offset += frac * C;
    return seg;
  }).join('');
  const legend = entries.items.map((it, i) => {
    const pct = it.value / total * 100;
    return '<button type="button" class="donut-legend-item" data-i="' + i + '">'
      + '<span class="swatch" style="background:' + it.color + '"></span>'
      + '<span class="dl-name" title="' + esc(it.label) + '">' + esc(it.label) + '</span>'
      + '<span class="dl-pct mono">' + (pct < 10 ? pct.toFixed(1) : pct.toFixed(0)) + '%</span></button>';
  }).join('');
  mount.innerHTML = '<div class="donut-flex">'
    + '<div class="donut-ring"><svg viewBox="0 0 120 120" role="img">' + segs + '</svg>'
    + '<div class="donut-center"><div class="donut-center-main"></div><div class="donut-center-sub"></div></div></div>'
    + '<div class="donut-legend">' + legend + '</div></div>';

  const centerMain = mount.querySelector('.donut-center-main');
  const centerSub = mount.querySelector('.donut-center-sub');
  const showTotal = () => {
    centerMain.textContent = metricText(total, fmt.metric);
    centerSub.textContent = fmt.center;
  };
  showTotal();
  const circles = [...mount.querySelectorAll('.donut-seg')];
  // 入场：rAF 两帧后（首帧已绘制空弧）按弧长比例分配时长、以前序累计作延迟，
  // linear 缓动保证段间前沿速度一致；结束后移除内联过渡，交还 hover 效果。
  requestAnimationFrame(() => requestAnimationFrame(() => {
    let acc = 0;
    entries.items.forEach((it, i) => {
      const frac = it.value / total;
      const len = Math.max(frac * C - 1.5, frac > 0 ? 0.6 : 0); // 1.5 单位留缝
      const c = circles[i];
      c.style.transition = 'stroke-dasharray ' + Math.max(frac * DONUT_ANIM_MS, 30).toFixed(0)
        + 'ms linear ' + (acc * DONUT_ANIM_MS).toFixed(0) + 'ms';
      c.style.strokeDasharray = len.toFixed(2) + ' ' + (C - len + 10).toFixed(2);
      acc += frac;
    });
    setTimeout(() => circles.forEach(c => { c.style.transition = ''; }), DONUT_ANIM_MS + 120);
  }));
  const highlight = i => {
    circles.forEach((c, j) => c.classList.toggle('dim', i >= 0 && j !== i));
    if (i >= 0) {
      const it = entries.items[i];
      centerMain.textContent = metricText(it.value, fmt.metric);
      centerSub.textContent = it.label;
    } else showTotal();
  };
  const bindHl = (el, i) => {
    el.addEventListener('mouseenter', () => highlight(i));
    el.addEventListener('mouseleave', () => highlight(-1));
    el.addEventListener('focus', () => highlight(i));
    el.addEventListener('blur', () => highlight(-1));
  };
  circles.forEach((c, i) => bindHl(c, i));
  mount.querySelectorAll('.donut-legend-item').forEach(el => bindHl(el, Number(el.dataset.i)));
}
function renderModels(rows) {
  const m = ovMetric.models;
  const e = donutEntries(rows, m);
  const n = Number.isInteger(ovCache.modelCount) ? ovCache.modelCount : rows.length;
  $('ov-models-sub').textContent = METRIC_SUBS[m] + ' · 前 5 + 其他，共 ' + n + ' 项';
  drawDonut('ov-models', e, { metric: m, center: n + ' 个模型' });
}
function renderKeySpend(rows) {
  const m = ovMetric.keys;
  const withKey = rows.filter(r => r.value).map(r => Object.assign({}, r, {
    value: keyLabelOf(r.value) || '(无标签)',
  }));
  const e = donutEntries(withKey, m);
  const n = Number.isInteger(ovCache.keyCount) ? ovCache.keyCount : rows.length;
  $('ov-keys-sub').textContent = METRIC_SUBS[m] + ' · 前 5 + 其他，共 ' + n + ' 枚';
  drawDonut('ov-keys', e, { metric: m, center: n + ' 枚密钥' });
}

// ---------- 趋势图（内联 SVG 堆叠面积）----------
function niceMax(v) {
  if (v <= 0) return 1;
  const exp = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (v <= m * exp) return m * exp;
  return 10 * exp;
}
function bucketLabel(ts, grain) {
  const d = new Date(ts);
  if (grain === 'month') return d.getFullYear() + '-' + pad2(d.getMonth() + 1);
  if (grain === 'day' || grain === 'week')
    return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate());
  return pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) + ' ' + pad2(d.getHours()) + ':' + pad2(d.getMinutes());
}
function bucketTick(ts, grain) {
  const d = new Date(ts);
  if (grain === 'month') return d.getFullYear() + '-' + pad2(d.getMonth() + 1);
  if (grain === 'day' || grain === 'week') return pad2(d.getMonth() + 1) + '-' + pad2(d.getDate());
  return pad2(d.getHours()) + ':' + pad2(d.getMinutes());
}
function cssVar(name) { return getComputedStyle(document.documentElement).getPropertyValue(name).trim(); }

// 按时间跨度自动选粒度（用户手动选过则不再覆盖）。
function autoGrain() {
  const { from } = computeRange();
  const span = from ? Date.now() - from.getTime() : Infinity;
  if (span < 3 * 36e5) return 'minute';
  if (span < 96 * 36e5) return 'hour';
  if (span < 120 * 864e5) return 'day';
  return 'month';
}
// 与服务端口径一致的桶对齐：分/时/日按 UTC 整除，周为 UTC 周一，月为 UTC 月初。
function bucketStepMs(grain) {
  return { minute: 6e4, hour: 36e5, day: 864e5, week: 6048e5, month: 0 }[grain] || 0;
}
function alignBucket(ms, grain) {
  if (grain === 'week') {
    const d = new Date(ms);
    const u = Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate());
    const dow = (new Date(u).getUTCDay() + 6) % 7;
    return u - dow * 864e5;
  }
  if (grain === 'month') {
    const d = new Date(ms);
    return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), 1);
  }
  const step = bucketStepMs(grain);
  return Math.floor(ms / step) * step;
}
// 在所选时间范围内补零值桶，让面积图连续；桶数超上限时退回原始点。
function fillTrendPoints(raw, grain) {
  const pts = raw.slice().sort((a, b) => new Date(a.bucket) - new Date(b.bucket));
  if (!pts.length) return pts;
  const step = bucketStepMs(grain);
  const { from, to } = computeRange();
  let start = from ? alignBucket(from.getTime(), grain) : alignBucket(new Date(pts[0].bucket).getTime(), grain);
  let end = to ? Math.min(to.getTime(), Date.now()) : Date.now();
  end = alignBucket(end, grain);
  if (!step && grain === 'month') {
    // 按自然月推进
    const sd = new Date(start), ed = new Date(end);
    const months = (ed.getUTCFullYear() - sd.getUTCFullYear()) * 12 + ed.getUTCMonth() - sd.getUTCMonth();
    if (months > 2000) return pts;
  } else if (step && (end - start) / step > 2000) {
    return pts;
  }
  const byKey = new Map(pts.map(p => [alignBucket(new Date(p.bucket).getTime(), grain), p]));
  const out = [];
  const zero = () => ({
    requests: 0, failures: 0, input_tokens: 0, output_tokens: 0, cached_tokens: 0,
    cache_read_tokens: 0, cache_creation_tokens: 0, total_tokens: 0, cost_micro_usd: 0,
  });
  if (grain === 'month') {
    const cur = new Date(start);
    while (cur.getTime() <= end) {
      const k = cur.getTime();
      out.push(Object.assign({ bucket: new Date(k).toISOString() }, byKey.get(k) || zero()));
      cur.setUTCMonth(cur.getUTCMonth() + 1);
    }
  } else {
    for (let t = start; t <= end; t += step)
      out.push(Object.assign({ bucket: new Date(t).toISOString() }, byKey.get(t) || zero()));
  }
  return out.length ? out : pts;
}

// downsampleTrend 把过密的桶按相邻 k 个合并，避免柱状图 DOM 爆炸（仅影响显示）。
function downsampleTrend(pts, maxN) {
  if (pts.length <= maxN) return pts;
  const k = Math.ceil(pts.length / maxN);
  const keys = ['requests', 'failures', 'input_tokens', 'output_tokens', 'cached_tokens',
    'cache_read_tokens', 'cache_creation_tokens', 'total_tokens', 'cost_micro_usd'];
  const out = [];
  for (let i = 0; i < pts.length; i += k) {
    const g = { bucket: pts[i].bucket };
    for (const key of keys) g[key] = 0;
    for (let j = i; j < Math.min(i + k, pts.length); j++)
      for (const key of keys) g[key] += +pts[j][key] || 0;
    out.push(g);
  }
  return out;
}

function trendSeries() {
  const metric = trendMetricSel.value;
  // 「成功/失败」是状态语义（good/bad），保留状态色；其余是身份语义，用图表专用系列色。
  // 次级编码：失败恒在栈顶 + 段间 2px 间隙 + 图例常在，不依赖单一色相区分。
  if (metric === 'requests') return [
    { key: 'ok', label: '成功', color: cssVar('--live'), val: p => Math.max(0, p.requests - p.failures) },
    { key: 'fail', label: '失败', color: cssVar('--alarm'), val: p => p.failures },
  ];
  if (metric === 'cost') return [
    { key: 'cost', label: '费用', color: cssVar('--series-1'), val: p => p.cost_micro_usd, money: true },
  ];
  return [
    // 堆叠必须互不重叠：OpenAI/Gemini 的 cached_tokens 已含在 input_tokens 内，
    // 先从输入中拆出再单列「缓存读」，否则缓存命中被计两遍，
    // 堆叠总量会虚高且与概览「总消耗 Token」（EffectiveTotal 口径）对不上。
    // Claude 口径 cached_tokens 恒为 0，减法无影响。
    { key: 'input', label: '输入', color: cssVar('--series-1'), tok: true,
      val: p => Math.max(0, (+p.input_tokens || 0) - Math.min(+p.cached_tokens || 0, +p.input_tokens || 0)) },
    { key: 'output', label: '输出', color: cssVar('--series-2'), tok: true, val: p => p.output_tokens },
    { key: 'cache-read', label: '缓存读', color: cssVar('--series-3'), tok: true, val: p => cacheReadOf(p) },
    { key: 'cache-creation', label: '缓存写', color: cssVar('--series-4'), tok: true, val: p => p.cache_creation_tokens || 0 },
  ];
}
function renderLegend() {
  // 单系列不需要图例（标题已说明画的是什么），≥2 系列图例常在。
  const defs = trendSeries();
  $('trend-legend').innerHTML = defs.length < 2 ? '' : defs.map(d =>
    '<button type="button" class="legend-item" data-key="' + d.key + '" aria-pressed="'
    + String(!trend.off.has(d.key)) + '"><span class="swatch" style="background:' + d.color + '"></span>'
    + d.label + '</button>').join('');
}
$('trend-legend').addEventListener('click', e => {
  const b = e.target.closest('.legend-item');
  if (!b) return;
  const k = b.dataset.key;
  if (trend.off.has(k)) trend.off.delete(k); else trend.off.add(k);
  if (trend.off.size === trendSeries().length) trend.off.delete(k);
  renderTrend();
});

function renderTrend() {
  const grain = trendGrainSel.value;
  const filled = fillTrendPoints(trend.points, grain);
  const pts = downsampleTrend(filled, 1200);
  const box = $('trend-chart');
  renderLegend();
  const grainText = (GRAINS.find(g => g.value === grain) || {}).label || '';
  $('trend-sub').textContent = '按' + grainText.replace('按', '')
    + '堆叠 · ' + rangeLabel() + ' · ' + pts.length + ' 个桶'
    + (pts.length < filled.length ? '（过密已合并显示）' : '');
  const defs = trendSeries().map(d => Object.assign(d, { on: !trend.off.has(d.key) }));
  trend.view === 'table' ? renderTrendTable(pts, defs, grain) : $('trend-table').hidden = true;
  if (trend.view === 'table') { box.hidden = true; return; }
  box.hidden = false;
  if (!pts.length) {
    box.innerHTML = '<div class="empty" style="height:100%"><p class="empty-title">暂无趋势数据</p>'
      + '<p class="empty-hint">所选时间范围与粒度下没有聚合记录</p></div>';
    return;
  }
  // 用像素级 width/height 而非 preserveAspectRatio=none，避免坐标轴文字被拉伸。
  const W = Math.max(320, Math.floor(box.clientWidth || 800));
  const H = Math.max(240, Math.floor(box.clientHeight || 300));
  const padL = 56, padR = 14, padT = 14, padB = 26;
  const iw = W - padL - padR, ih = H - padT - padB;
  const stacks = pts.map(p => defs.reduce((acc, d) => d.on ? acc + Math.max(0, d.val(p)) : acc, 0));
  const ymax = niceMax(Math.max(...stacks, 1));
  const y = v => padT + ih - (v / ymax) * ih;
  const n = pts.length;
  const slot = iw / n;                 // 每个桶的槽宽
  // 细粒度（分钟/小时）桶多，柱宽上限 24px 即可；天/周/月桶少槽宽大，
  // 改为「槽宽减去固定间隙」，避免柱子孤零零缩在槽中央、两侧大片留白。
  const coarse = grain === 'day' || grain === 'week' || grain === 'month';
  const barW = coarse
    ? Math.max(2, Math.min(96, slot - Math.min(12, slot * 0.18)))
    : Math.max(2, Math.min(24, slot * 0.7));
  const cx = i => padL + slot * i + slot / 2;

  let grid = '', labels = '';
  const isMoney = defs.some(d => d.money);
  const isTokens = defs.some(d => d.tok);
  const fmtAxis = v => isMoney ? fmtCur(v) : isTokens ? fmtTok(v) : fmtInt(v);
  for (let g = 0; g <= 4; g++) {
    const gv = ymax * g / 4, gy = y(gv);
    grid += '<line class="gridline" x1="' + padL + '" y1="' + gy.toFixed(1) + '" x2="' + (W - padR) + '" y2="' + gy.toFixed(1) + '"/>';
    labels += '<text class="axis-text" x="' + (padL - 8) + '" y="' + (gy + 3.5).toFixed(1) + '" text-anchor="end">'
      + fmtAxis(gv) + '</text>';
  }
  const tickStep = Math.max(1, Math.ceil(n / Math.floor(iw / 64)));
  let lastTickX = -1e9;
  for (let i = 0; i < n; i++) {
    const tx = cx(i);
    const isLast = i === n - 1;
    if ((i % tickStep !== 0 && !isLast) || tx - lastTickX < 40) continue;
    lastTickX = tx;
    labels += '<text class="axis-text" x="' + tx.toFixed(1) + '" y="' + (H - 8) + '" text-anchor="middle">'
      + esc(bucketTick(pts[i].bucket, grain)) + '</text>';
  }

  // 堆叠柱：自下而上逐系列叠加。
  // 段间留 2px 表面色间隙（用间隙分隔，不画描边）：间隙统一开在每段的**顶边**，
  // 这样最底段仍然坐在基线上（柱体从单一基线生长，基线端方角）。
  // 高度不足以让出间隙的薄段照原高绘制、不加间隙 —— 宁可少一条分隔，
  // 也不能把数据段整段丢掉或抬高（精确值另有 tooltip 与表格视图承载）。
  const GAP = 2, R = 4;
  const on = defs.filter(d => d.on);
  let bars = '', running = new Array(n).fill(0);
  // 先定位每根柱子最上面的可见段，供圆角判定
  const topSeg = new Array(n).fill(-1);
  for (let i = 0; i < n; i++)
    for (let k = 0; k < on.length; k++)
      if (Math.max(0, on[k].val(pts[i])) > 0) topSeg[i] = k;
  for (let k = 0; k < on.length; k++) {
    const d = on[k];
    for (let i = 0; i < n; i++) {
      const v = Math.max(0, d.val(pts[i]));
      if (v <= 0) continue;
      const yBase = y(running[i]);
      const yTop = y(running[i] + v);
      running[i] += v;
      const rawH = yBase - yTop;
      if (rawH <= 0) continue;
      const isTop = k === topSeg[i];
      const gap = (!isTop && rawH > GAP + 1) ? GAP : 0;
      // 非零段至少画 1px：亚像素高度等于画了个看不见的东西，
      // 「有但极小」比「看不到」更诚实（精确值在 tooltip 与表格视图里）。
      const h = Math.max(1, rawH - gap);
      const yDraw = yTop + gap;
      const x = cx(i) - barW / 2;
      bars += isTop
        ? '<path d="' + topRoundedBar(x, yDraw, barW, h, R) + '" fill="' + d.color + '"/>'
        : '<rect x="' + x.toFixed(1) + '" y="' + yDraw.toFixed(1) + '" width="' + barW.toFixed(1)
          + '" height="' + h.toFixed(1) + '" fill="' + d.color + '"/>';
    }
  }
  // 悬停/聚焦热区（置于柱体之下、网格之上）。tabindex 让键盘也能读到数值。
  let hover = '';
  for (let i = 0; i < n; i++)
    hover += '<rect class="bar-hover" tabindex="0" role="button" data-i="' + i + '"'
      + ' aria-label="' + esc(bucketLabel(pts[i].bucket, grain) + '，合计 ' + fmtAxis(stacks[i])) + '"'
      + ' x="' + (padL + slot * i).toFixed(1) + '" y="' + padT + '" width="' + slot.toFixed(1)
      + '" height="' + ih + '"/>';

  box.innerHTML = '<svg width="' + W + '" height="' + H + '" viewBox="0 0 ' + W + ' ' + H
    + '" role="img" aria-label="用量趋势图">' + grid + hover + bars + labels + '</svg>'
    + '<div class="chart-tip" id="trend-tip" hidden></div>';

  const svg = box.querySelector('svg'), tip = $('trend-tip');
  let hotIdx = -1;
  function setHot(i) {
    if (i === hotIdx) return;
    hotIdx = i;
    svg.querySelectorAll('.bar-hover').forEach(r =>
      r.classList.toggle('on', +r.dataset.i === i));
  }
  // showTip 鼠标与键盘共用：tooltip 上挂在柱顶，空间不足时下翻。
  function showTip(idx) {
    setHot(idx);
    const p = pts[idx];
    let rows = defs.filter(d => d.on).map(d =>
      '<div class="tip-row"><span class="swatch" style="background:' + d.color + '"></span><span>' + d.label
      + '</span><b>' + (d.money ? fmtCur(d.val(p)) : d.tok ? fmtTok(d.val(p)) : fmtInt(d.val(p))) + '</b></div>').join('');
    rows += '<div class="tip-row tip-total"><span></span><span>合计</span><b>' + fmtAxis(stacks[idx]) + '</b></div>';
    tip.innerHTML = '<div class="tip-head">' + esc(bucketLabel(p.bucket, grain)) + '</div>' + rows;
    tip.hidden = false;
    const rect = svg.getBoundingClientRect();
    const sx = cx(idx) / W * rect.width;
    const topPx = y(stacks[idx]) / H * rect.height;
    const wantAbove = topPx - tip.offsetHeight - 8 >= 0;
    tip.classList.toggle('below', !wantAbove);
    tip.style.left = Math.max(90, Math.min(rect.width - 90, sx)) + 'px';
    tip.style.top = (wantAbove ? topPx - 8 : topPx + 8) + 'px';
  }
  function hideTip() { tip.hidden = true; setHot(-1); }
  svg.addEventListener('mousemove', ev => {
    const rect = svg.getBoundingClientRect();
    const sx = (ev.clientX - rect.left) * (W / rect.width);
    let idx = Math.floor((sx - padL) / slot);
    showTip(Math.max(0, Math.min(n - 1, idx)));
  });
  svg.addEventListener('mouseleave', hideTip);
  // 键盘：Tab 进入热区即出 tooltip，左右键在桶之间移动
  svg.addEventListener('focusin', e => {
    const r = e.target.closest('.bar-hover');
    if (r) showTip(+r.dataset.i);
  });
  svg.addEventListener('focusout', e => {
    if (!svg.contains(e.relatedTarget)) hideTip();
  });
  svg.addEventListener('keydown', e => {
    const r = e.target.closest('.bar-hover');
    if (!r) return;
    const i = +r.dataset.i;
    const to = e.key === 'ArrowRight' ? i + 1 : e.key === 'ArrowLeft' ? i - 1 : -1;
    if (to < 0 || to >= n) return;
    e.preventDefault();
    svg.querySelector('.bar-hover[data-i="' + to + '"]').focus();
  });
}
// topRoundedBar 顶端圆角、基线方角的柱体路径。
function topRoundedBar(x, y, w, h, r) {
  const R = Math.max(0, Math.min(r, w / 2, h));
  return 'M' + x.toFixed(1) + ' ' + (y + h).toFixed(1)
    + 'V' + (y + R).toFixed(1)
    + 'a' + R.toFixed(1) + ' ' + R.toFixed(1) + ' 0 0 1 ' + R.toFixed(1) + ' -' + R.toFixed(1)
    + 'h' + (w - 2 * R).toFixed(1)
    + 'a' + R.toFixed(1) + ' ' + R.toFixed(1) + ' 0 0 1 ' + R.toFixed(1) + ' ' + R.toFixed(1)
    + 'V' + (y + h).toFixed(1) + 'Z';
}
// renderTrendTable 图表的表格孪生视图：数值不再只能靠悬停读取。
function renderTrendTable(pts, defs, grain) {
  const host = $('trend-table');
  host.hidden = false;
  const on = defs.filter(d => d.on);
  const fmtOf = d => v => d.money ? fmtCur(v) : d.tok ? fmtTok(v) : fmtInt(v);
  if (!pts.length) {
    host.innerHTML = '<div class="empty"><p class="empty-title">暂无趋势数据</p>'
      + '<p class="empty-hint">所选时间范围与粒度下没有聚合记录</p></div>';
    return;
  }
  const rows = pts.slice().reverse();
  host.innerHTML = '<table class="data"><thead><tr><th>时间桶</th>'
    + on.map(d => '<th class="num">' + esc(d.label) + '</th>').join('')
    + '<th class="num">合计</th></tr></thead><tbody>'
    + rows.map(p => {
      const total = on.reduce((a, d) => a + Math.max(0, d.val(p)), 0);
      const f = on.length ? fmtOf(on[0]) : fmtInt;
      return '<tr><td class="cell-mono">' + esc(bucketLabel(p.bucket, grain)) + '</td>'
        + on.map(d => '<td class="num">' + fmtOf(d)(Math.max(0, d.val(p))) + '</td>').join('')
        + '<td class="num"><b>' + f(total) + '</b></td></tr>';
    }).join('')
    + '</tbody></table>';
}
const trendMetricSel = new Select('trend-metric', [
  { value: 'tokens', label: 'Token' },
  { value: 'requests', label: '请求' },
  { value: 'cost', label: '费用' },
], () => { trend.off.clear(); renderTrend(); }, { value: 'tokens', head: '趋势指标' });
const trendGrainSel = new Select('trend-grain', GRAINS,
  () => { trend.grainManual = true; reloadActive(); }, { value: 'day', head: '聚合粒度' });
// 图表 / 表格切换：表格孪生视图让数值不必依赖悬停
$('trend-view').addEventListener('click', e => {
  const b = e.target.closest('button[data-v]');
  if (!b || b.dataset.v === trend.view) return;
  trend.view = b.dataset.v;
  $('trend-view').querySelectorAll('button').forEach(x => x.classList.toggle('on', x === b));
  renderTrend();
});
window.addEventListener('resize', debounce(() => { if (activeTab === 'overview') renderTrend(); }, 200));

