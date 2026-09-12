// ---------- 用量 ----------
const REQ_SIZES = [20, 50, 100];
const reqView = {
  page: 0, size: +(localStorage.getItem('req-size') || 20) || 20,
  sort: 'ts', order: 'desc', model: '', keyId: '', result: '',
};

// 请求明细列定义。
// 默认列刻意收进视口宽度内：13 列独立排布时表格 1322px > 容器 1184px，
// 叠加外层 max-height 的竖向滚动后一个卡片里两个方向都要拖。
// 计量三列与延迟两列合并成复合单元格，其余低频列改为按需开启。
//
// sort 只填后端 ListRequests 白名单里真实存在的键（ts/cost/tokens/latency/model）：
// 其余列不给排序 affordance，避免「看起来能点但点了没反应」。
const REQ_COLS = [
  { id: 'ts', label: '时间', sort: 'ts', fixed: true, cell: x => '<td class="cell-mono">' + fmtDT(x.ts, true) + '</td>' },
  {
    id: 'key', label: '密钥', cell: x => '<td class="cell-mono" title="' + esc(x.key_id || '') + '">'
      + esc(keyLabelOf(x.key_id) || x.key_id || '-')
      // ai_judge 子调用归属到触发它的插件 Key，加徽标与该 Key 的正常流量区分
      + (x.source === 'ai_judge' ? ' <span class="pill trace mono" title="AI 评判子调用（路由规则的 ai_judge）">ai_judge</span>' : '')
      + '</td>',
  },
  {
    id: 'model', label: '模型', sort: 'model', cell: x => '<td class="cell-mono cell-clip" title="' + esc(x.model) + '">'
      + esc(x.model || '-') + '</td>',
  },
  { id: 'provider', label: '提供方', off: true, cell: x => '<td class="cell-dim">' + esc(x.provider || '-') + '</td>' },
  {
    id: 'result', label: '结果', cell: x => '<td><span class="pill '
      + (x.result === 'ok' ? 'live' : x.result === 'blocked' ? 'warn' : 'alarm') + '">' + esc(x.result) + '</span></td>',
  },
  {
    // 失败原因：状态码 + 错误摘要（含路由目标转移轨迹）。低频列，默认收起。
    id: 'cause', label: '原因', off: true,
    tip: '失败请求的上游状态码与错误摘要；路由流量含目标转移轨迹（a→b(原因)）。',
    cell: x => {
      if (x.result === 'ok' && !x.error_note) return '<td class="cell-dim">-</td>';
      const code = +x.status_code ? x.status_code : '';
      const note = x.error_note || '';
      const title = (code ? 'HTTP ' + code + (note ? ' · ' : '') : '') + note;
      return '<td class="cell-mono cell-clip" title="' + esc(title) + '">'
        + (code ? '<span class="pill alarm mono">' + esc(code) + '</span> ' : '')
        + esc(note || (code ? '' : '-')) + '</td>';
    },
  },
  {
    id: 'toks', label: '输入 / 输出 / 缓存读', num: true,
    tip: '三段分别为 输入 / 输出 / 缓存读。' + TIPS.cacheRead,
    cell: x => '<td class="num"><span class="cell-toks" title="输入 ' + esc(fmtTok(x.input_tokens))
      + ' · 输出 ' + esc(fmtTok(x.output_tokens)) + ' · 缓存读 ' + esc(fmtTok(cacheReadOf(x))) + '">'
      + '<span class="tk">' + fmtTok(x.input_tokens) + '</span><span class="sep">/</span>'
      + '<span class="tk out">' + fmtTok(x.output_tokens) + '</span><span class="sep">/</span>'
      + '<span class="tk cr">' + fmtTok(cacheReadOf(x)) + '</span></span></td>',
  },
  {
    id: 'tokens', label: '总 Token', sort: 'tokens', num: true,
    tip: '计费四类合计：输入＋输出＋缓存读＋缓存写。与 Token 限额同一口径。',
    cell: x => '<td class="num">' + reqTokenCell(x) + '</td>',
  },
  { id: 'cost', label: '费用', sort: 'cost', num: true, cell: x => x.currency === 'CNY'
      ? '<td class="num" title="人民币计价规则，按当前实时汇率折算 $' + fmtUSD(x.cost_micro_usd).slice(1) + '">' + fmtMoney(x.cost_native_micro, '¥') + '</td>'
      : '<td class="num">' + fmtCur(x.cost_micro_usd) + '</td>' },
  {
    // 排序键指向 latency（总延迟）。旧版把「首字」表头标成 data-sort="latency"，
    // 而 latency 在后端映射到 latency_ms，点「首字」实际按总延迟排 —— 表头与行为不一致。
    id: 'lat', label: '延迟 首字→总', sort: 'latency', num: true,
    tip: '首字延迟 → 总延迟。首字延迟是收到第一个 token 的耗时，总延迟含整段生成。',
    cell: x => '<td class="num"><span class="cell-lat"><span class="ttft">' + fmtSec(x.ttft_ms)
      + '</span><span class="arrow">→</span><span>' + fmtSec(x.latency_ms) + '</span></span></td>',
  },
  { id: 'tps', label: 'TPS', num: true, off: true, cell: x => '<td class="num">' + fmtTPS(x.tps_milli) + '</td>' },
  { id: 'reasoning', label: '推理', num: true, off: true, cell: x => '<td class="num">' + fmtTok(x.reasoning_tokens) + '</td>' },
  { id: 'tier', label: '档位', off: true, cell: x => '<td class="cell-dim">' + esc(x.tier || '-') + '</td>' },
];
const REQ_COLS_DEFAULT = REQ_COLS.filter(c => !c.off).map(c => c.id);
function loadReqCols() {
  try {
    const saved = JSON.parse(localStorage.getItem('req-cols') || 'null');
    if (Array.isArray(saved) && saved.length) {
      const valid = saved.filter(id => REQ_COLS.some(c => c.id === id));
      if (valid.length) return new Set(valid.concat(REQ_COLS.filter(c => c.fixed).map(c => c.id)));
    }
  } catch (_) { /* 偏好损坏则回默认 */ }
  return new Set(REQ_COLS_DEFAULT);
}
let reqCols = loadReqCols();
const activeReqCols = () => REQ_COLS.filter(c => reqCols.has(c.id));
// reqTokenCell 总 Token 单元格：上游未返回用量但已按预占扣费时给出显式标记。
function reqTokenCell(x) {
  const noUsage = !(+x.input_tokens || 0) && !(+x.output_tokens || 0)
    && !(+x.cache_read_tokens || 0) && !(+x.cache_creation_tokens || 0);
  return noUsage && (+x.cost_micro_usd || 0) > 0
    ? '<span class="pill warn mono" title="上游未返回用量，费用按预占估算扣费">未捕获</span>'
    : '<b>' + fmtTok(effTokens(x)) + '</b>';
}

// fillReqSuggestions 填充模型/密钥筛选框的联想候选（自建组合框，标签与 kid 同时可见）。
function fillReqSuggestions() {
  api('/usage/dimension?' + new URLSearchParams({ dimension: 'model', limit: '200' }))
    .then(r => reqModelCombo.setOptions((r.rows || []).filter(x => x.value)
      .map(x => ({ value: x.value, label: x.value }))))
    .catch(() => {});
  // 密钥候选走 /keys/candidates 轻量接口（全量 kid+标签），不再受 /keys 分页
  // 限制；loadKeyCandidates 会话级缓存，重复进页零网络往返。
  loadKeyCandidates()
    .then(items => reqKeyCombo.setOptions(items.map(k => ({
      value: k.kid, label: k.label || '(无标签)', sub: k.kid,
    }))))
    .catch(() => {});
}

loaders.usage = async () => {
  await Promise.all([loadDim(), loadRoutes().catch(e => { $('route-body').innerHTML = '<div class="empty"><p class="empty-hint">' + esc(e.message) + '</p></div>'; })]);
  await loadRequests();
  fillReqSuggestions();
  stamp();
};

let routeRows = [], routePage = 0, routeModel = '';
const ROUTE_PAGE_SIZE = 5;
// 必须选中本地别名才展示路由（无「全部」态）；未选时 value='' 显示占位符。
const routeModelSel = new Select('route-model', [],
  v => { routeModel = v; routePage = 0; renderRoutes(); }, { head: '按别名筛选', placeholder: '选择本地别名…' });
async function loadRoutes() {
  const r = await api('/routes?' + new URLSearchParams(rangeParams()));
  routeRows = r.items || [];
  const names = [...new Set(routeRows.flatMap(r => r.models || []))].sort((a, b) => a.localeCompare(b));
  if (routeModel && !names.includes(routeModel)) { routeModel = ''; routeModelSel.value = ''; }
  routeModelSel.setOptions(names.map(n => ({ value: n, label: n })));
  renderRoutes();
}
function renderRoutes() {
  // 表格区与换页栏始终同构渲染：fixed5 撑住高度，换页栏位置不随内容浮动。
  if (!routeModel) {
    $('route-body').innerHTML = '<div class="table-wrap fixed5"><div class="empty">'
      + '<p class="empty-title">未选择本地别名</p>'
      + '<p class="empty-hint">在右上角选择一个本地别名，查看它实际路由到的上游模型</p></div></div>'
      + '<div class="pager"><span class="mono">第 0 / 0 页 · 共 0 条映射</span><span class="grow"></span>'
      + '<button type="button" class="btn small" disabled>上一页</button>'
      + '<button type="button" class="btn small" disabled>下一页</button></div>';
    $('route-count').textContent = '';
    return;
  }
  const rowsAll = routeRows.filter(r => (r.models || []).includes(routeModel));
  const total = rowsAll.reduce((a, r) => a + (Number(r.requests) || 0), 0);
  const maxReq = Math.max(1, ...rowsAll.map(r => Number(r.requests) || 0));
  const shareOf = r => total > 0 ? (Number(r.requests) || 0) / total * 100 : 0;
  const pages = Math.max(1, Math.ceil(rowsAll.length / ROUTE_PAGE_SIZE));
  if (routePage >= pages) routePage = pages - 1;
  const rows = rowsAll.slice(routePage * ROUTE_PAGE_SIZE, (routePage + 1) * ROUTE_PAGE_SIZE);
  // 首列与维度聚合同构：名称行 + 占比 + 条形；行按上游真名聚合，不展示本地别名与提供商。
  $('route-body').innerHTML = '<div class="table-wrap fixed5"><table class="data"><thead><tr>'
    + '<th class="w-grow">上游模型</th>'
    + '<th class="num">请求</th><th class="num">Token</th></tr></thead><tbody>'
    + rows.map(rw => {
      const up = rw.upstream_model || '(未知)';
      const name = '<span class="bar-name">' + esc(up) + '</span>';
      return '<tr>'
      + '<td><div class="bar-cell" title="' + esc(up) + '">'
        + '<div class="bar-top">' + name + '<span class="bar-pct">'
        + (shareOf(rw) < 10 ? shareOf(rw).toFixed(1) : shareOf(rw).toFixed(0)) + '%</span></div>'
        + '<div class="bar-line"><span style="width:'
        + ((Number(rw.requests) || 0) / maxReq * 100).toFixed(1) + '%"></span></div></div></td>'
      + '<td class="num">' + fmtInt(rw.requests) + '</td>'
      + '<td class="num">' + fmtTok(rw.total_tokens) + '</td></tr>';
    }).join('')
    + '</tbody></table></div>'
    + '<div class="pager" id="route-pager"><span class="mono">第 ' + (routePage + 1) + ' / ' + pages + ' 页 · 共 '
      + fmtInt(rowsAll.length) + ' 条映射</span><span class="grow"></span>'
      + '<button type="button" class="btn small" id="route-prev"' + (routePage <= 0 ? ' disabled' : '') + '>上一页</button>'
      + '<button type="button" class="btn small" id="route-next"'
      + ((routePage + 1) * ROUTE_PAGE_SIZE >= rowsAll.length ? ' disabled' : '') + '>下一页</button></div>';
  $('route-count').textContent = fmtInt(total) + ' 次请求 · ' + fmtInt(rowsAll.length) + ' 条映射';
  const prev = $('route-prev'), next = $('route-next');
  if (prev) prev.onclick = () => { routePage--; renderRoutes(); };
  if (next) next.onclick = () => { routePage++; renderRoutes(); };
}
const DIMS = [
  { value: 'model', label: '模型' },
  { value: 'provider', label: '提供方' },
  { value: 'source', label: '来源' },
  { value: 'auth_type', label: '认证类型' },
  { value: 'auth_label', label: '认证账号' },
  { value: 'result', label: '结果' },
  { value: 'key_id', label: '密钥' },
  { value: 'caller_id', label: 'caller' },
];
let dimRows = [], dimPage = 0, dimSort = 'cost', dimDir = 'desc';
const DIM_PAGE_SIZE = 5;
async function loadDim() {
  const dim = dimSel.value;
  // 与概览页口径一致：显式带 limit，后端还有 500 硬上限兜底。
  const r = await api('/usage/dimension?' + new URLSearchParams({ dimension: dim, limit: '50', ...rangeParams() }));
  dimRows = r.rows || [];
  sortDimRows();
  dimPage = 0;
  renderDim();
}
// 排序键由表头点击驱动（与请求明细同款交互），客户端全量重排。
function sortDimRows() {
  const val = r => dimSort === 'requests' ? (Number(r.requests) || 0)
    : dimSort === 'failures' ? (Number(r.failures) || 0)
    : dimSort === 'tokens' ? (Number(effTokens(r)) || 0)
    : (Number(r.cost_micro_usd) || 0);
  const sgn = dimDir === 'asc' ? 1 : -1;
  dimRows.sort((a, b) => sgn * (val(a) - val(b)));
}
function renderDim() {
  const rowsAll = dimRows;
  // 占比 = 本行请求数 / 所有分组请求数之和，各行之和为 100%（四舍五入误差除外）。
  //
  // 曾用「最大行请求数」作分母（v0.3.0 为修 >100% 而引入），那算的是「相对最大值的
  // 比例」而不是占比：最大行恒显示 100%，且各行相加远超 100%（result 维度下
  // ok 100% + error 8% = 108%，provider 维度累计 263%），与列名和常识都不符。
  //
  // 分母用全量行之和而非服务端 total —— 服务端的 total 是**只对返回行**累加的，
  // 带分页后每页只渲染一部分，占比与条长必须按全量数据归一才稳定。
  const denom = rowsAll.reduce((a, row) => a + (Number(row.requests) || 0), 0);
  const maxReq = Math.max(1, ...rowsAll.map(row => Number(row.requests) || 0));
  const shareOf = row => denom > 0 ? (Number(row.requests) || 0) / denom * 100 : 0;
  // 密钥维度的分组值是 kid，显示标签更可读（与请求表/概览/详情弹窗同口径），
  // kid 保留在 title 里。其余维度分组值本身就是可读文本。
  const nameOf = row => {
    const v = row.value || '';
    if (dimSel.value === 'key_id' && v) return keyLabelOf(v) || v;
    return v || '(空)';
  };
  const titleOf = row => {
    const v = row.value || '';
    if (dimSel.value === 'key_id' && v) {
      const label = keyLabelOf(v);
      return label ? label + ' · ' + v : v;
    }
    return v || '(空)';
  };
  const pages = Math.max(1, Math.ceil(rowsAll.length / DIM_PAGE_SIZE));
  if (dimPage >= pages) dimPage = pages - 1;
  const rows = rowsAll.slice(dimPage * DIM_PAGE_SIZE, (dimPage + 1) * DIM_PAGE_SIZE);
  // 数值列表头可点击排序（同请求明细：th.sort + data-dir，点击换键/切向）。
  const th = (label, key, num, wide) => '<th class="' + (wide ? 'w-grow' : num ? 'num' : '')
    + (key ? ' sort"' : '"')
    + (key ? ' data-sort="' + key + '"' : '')
    + (key && dimSort === key ? ' data-dir="' + dimDir + '"' : '') + '>' + label + '</th>';
  $('dim-body').innerHTML = '<div class="table-wrap fixed5"><table class="data"><thead><tr>'
    + th(esc((DIMS.find(d => d.value === dimSel.value) || {}).label || dimSel.value), '', false, true)
    + th('请求', 'requests', true)
    + th('失败', 'failures', true)
    + th('Token', 'tokens', true)
    + th('费用', 'cost', true)
    + th('缓存', '', true)
    + th('平均延迟', '', true)
    + th('TPS', '', true) + '</tr></thead><tbody>'
    + rows.map(row => {
      const share = shareOf(row);
      const hit = cacheHitRate(row);
      return '<tr>'
      + '<td><div class="bar-cell" title="' + esc(titleOf(row)) + '"><div class="bar-top"><span class="bar-name">'
      + esc(nameOf(row)) + '</span><span class="bar-pct">'
      + (share < 10 ? share.toFixed(1) : share.toFixed(0)) + '%</span></div>'
      + '<div class="bar-line"><span style="width:'
      + ((Number(row.requests) || 0) / maxReq * 100).toFixed(1) + '%"></span></div></div></td>'
      + '<td class="num">' + fmtInt(row.requests) + '</td>'
      + '<td class="num">' + (row.failures ? '<span class="pill alarm mono">' + fmtInt(row.failures) + '</span>' : '0') + '</td>'
      + '<td class="num">' + fmtTok(effTokens(row)) + '</td>'
      + '<td class="num">' + fmtCur(row.cost_micro_usd) + '</td>'
      + '<td class="num">' + (hit >= 0 ? hit.toFixed(1) + '%' : '—') + '</td>'
      + '<td class="num">' + fmtSec(row.latency_avg_ms) + '</td>'
      + '<td class="num">' + fmtTPS(row.tps_avg_milli) + '</td></tr>';
    }).join('')
    + '</tbody></table></div>'
    + '<div class="pager" id="dim-pager"><span class="mono">第 ' + (dimPage + 1) + ' / ' + pages + ' 页 · 共 '
      + fmtInt(rowsAll.length) + ' 项</span><span class="grow"></span>'
      + '<button type="button" class="btn small" id="dim-prev"' + (dimPage <= 0 ? ' disabled' : '') + '>上一页</button>'
      + '<button type="button" class="btn small" id="dim-next"'
      + ((dimPage + 1) * DIM_PAGE_SIZE >= rowsAll.length ? ' disabled' : '') + '>下一页</button></div>';
  const prev = $('dim-prev'), next = $('dim-next');
  if (prev) prev.onclick = () => { dimPage--; renderDim(); };
  if (next) next.onclick = () => { dimPage++; renderDim(); };
}
// 表头排序：#dim-body 常驻不重建，事件委托一次绑定即可。
$('dim-body').addEventListener('click', e => {
  const thEl = e.target.closest('th.sort');
  if (!thEl) return;
  const keyName = thEl.dataset.sort;
  if (dimSort === keyName) dimDir = dimDir === 'desc' ? 'asc' : 'desc';
  else { dimSort = keyName; dimDir = 'desc'; }
  sortDimRows();
  dimPage = 0;
  renderDim();
});
// fmtTPS 展示 TPS：超过 3000 token/s 视为宿主缓冲整转产生的坏测量
// （与后端落库上限同口径），v0.3.0 之前入库的历史脏行在展示层一并隐藏。
const maxPlausibleTPS = 3000;
const fmtTPS = milli => milli > 0 && milli / 1000 <= maxPlausibleTPS ? (milli / 1000).toFixed(1) : '-';

function kv(name, value) { return '<div class="kv-row"><dt>' + name + '</dt><dd>' + value + '</dd></div>'; }
// renderCostCoverage 渲染概览第三卡（计价覆盖）；costs 由 overview loader 统一拉取。
function renderCostCoverage(costs) {
  const cover = costs.requests ? Math.round(costs.priced_requests / costs.requests * 100) : 0;
  let html = '<div class="kv">'
    + kv('请求总数', fmtInt(costs.requests))
    + kv('已计价请求', fmtInt(costs.priced_requests))
    + kv('价格覆盖率', '<span class="pill ' + (cover >= 90 ? 'live' : cover >= 60 ? 'warn' : 'alarm') + ' mono">' + cover + '%</span>')
    + kv('总费用', '<b class="mono">' + fmtCur(costs.cost_micro_usd) + '</b>')
    + '</div><p class="note" style="margin-top:10px">未命中价格的请求不计费用；金额按顶栏显示币种折算。</p>';
  $('ov-cost-body').innerHTML = html;
}
async function loadRequests() {
  const q = new URLSearchParams({
    limit: String(reqView.size), offset: String(reqView.page * reqView.size),
    sort: reqView.sort, order: reqView.order, ...rangeParams(),
  });
  if (reqView.model) q.set('model', reqView.model);
  if (reqView.keyId) q.set('key_id', reqView.keyId);
  if (reqView.result) q.set('result', reqView.result);
  const r = await api('/requests?' + q);
  const items = r.items || [], total = r.total || 0;
  const pages = Math.max(1, Math.ceil(total / reqView.size));
  const cols = activeReqCols();
  $('req-count').textContent = '共 ' + fmtInt(total) + ' 条 · 第 ' + (reqView.page + 1) + ' / ' + pages + ' 页';
  // 表头随列偏好重建；只有后端支持的排序键才带 .sort
  $('req-head').innerHTML = cols.map(c => '<th' + (c.num ? ' class="num' + (c.sort ? ' sort' : '') + '"'
    : (c.sort ? ' class="sort"' : '')) + (c.sort ? ' data-sort="' + c.sort + '"' : '')
    + (c.sort && reqView.sort === c.sort ? ' data-dir="' + reqView.order + '"' : '')
    + '>' + (c.tip ? labelWithTip(c.label, c.tip) : esc(c.label)) + '</th>').join('');
  $('req-rows').innerHTML = items.map(x =>
    '<tr class="row" data-id="' + esc(x.id) + '">' + cols.map(c => c.cell(x)).join('') + '</tr>').join('')
    || '<tr><td colspan="' + cols.length + '"><div class="empty"><p class="empty-title">没有匹配的请求</p>'
    + '<p class="empty-hint">调整筛选条件或时间范围</p></div></td></tr>';
  $('req-rows').dataset.items = JSON.stringify(items);
  // 分页：75 页时只有上/下一页不够，补每页条数与跳页
  $('req-pager').innerHTML = '<span class="jump">每页'
    + REQ_SIZES.map(s => '<button type="button" class="btn small" data-size="' + s + '"'
      + (s === reqView.size ? ' disabled' : '') + '>' + s + '</button>').join('') + '</span>'
    + '<span class="grow"></span>'
    + '<button type="button" class="btn small" id="req-prev"' + (reqView.page <= 0 ? ' disabled' : '') + '>上一页</button>'
    + '<span class="jump"><input type="number" id="req-jump" min="1" max="' + pages + '" value="' + (reqView.page + 1)
    + '" aria-label="跳转到页码"><span class="mono">/ ' + pages + '</span></span>'
    + '<button type="button" class="btn small" id="req-next"'
    + ((reqView.page + 1) * reqView.size >= total ? ' disabled' : '') + '>下一页</button>';
  const go = () => loadRequests().catch(e => toast(e.message, 'err'));
  const prev = $('req-prev'), next = $('req-next'), jump = $('req-jump');
  if (prev) prev.onclick = () => { reqView.page--; go(); };
  if (next) next.onclick = () => { reqView.page++; go(); };
  $('req-pager').querySelectorAll('[data-size]').forEach(b => b.onclick = () => {
    reqView.size = +b.dataset.size;
    savePref('req-size', String(reqView.size));
    reqView.page = 0;
    go();
  });
  if (jump) {
    const apply = () => {
      const p = Math.max(1, Math.min(pages, parseInt(jump.value, 10) || 1));
      if (p - 1 === reqView.page) { jump.value = String(p); return; }
      reqView.page = p - 1;
      go();
    };
    jump.onchange = apply;
    jump.onkeydown = e => { if (e.key === 'Enter') { e.preventDefault(); apply(); } };
  }
}
$('req-table').querySelector('thead').addEventListener('click', e => {
  const th = e.target.closest('th.sort');
  if (!th) return;
  const keyName = th.dataset.sort;
  if (reqView.sort === keyName) reqView.order = reqView.order === 'desc' ? 'asc' : 'desc';
  else { reqView.sort = keyName; reqView.order = 'desc'; }
  reqView.page = 0;
  loadRequests().catch(err => toast(err.message, 'err'));
});
$('req-rows').addEventListener('click', e => {
  const tr = e.target.closest('tr.row');
  if (!tr) return;
  let items = [];
  try { items = JSON.parse($('req-rows').dataset.items || '[]'); } catch (_) { /* 忽略 */ }
  const x = items.find(i => i.id === tr.dataset.id);
  if (!x) return;
  openSheet({
    title: '请求明细 · ' + fmtDT(x.ts, true),
    okText: '关闭', noFocus: true,
    body: '<div class="detail-facts">'
      + fact('模型', x.model || '-') + fact('提供方', x.provider || '-')
      + fact('来源', x.source || '-') + fact('结果', x.result)
      + fact('密钥', x.key_id ? (keyLabelOf(x.key_id) || x.key_id) : '-') + fact('caller', x.caller_id || '-')
      + fact('认证账号', x.auth_label || x.auth_id || '-') + fact('认证类型', x.auth_type || '-')
      + fact('档位', x.tier || '-') + fact('思考强度', x.thinking_intensity || '-')
      + fact('输入 Token', fmtTok(x.input_tokens)) + fact('输出 Token', fmtTok(x.output_tokens))
      + fact('推理 Token', fmtTok(x.reasoning_tokens)) + fact('缓存读', fmtTok(cacheReadOf(x)))
      + fact('缓存写', fmtTok(x.cache_creation_tokens)) + fact('总 Token', fmtTok(effTokens(x)))
      + (!(+x.input_tokens || 0) && !(+x.output_tokens || 0) && (+x.cost_micro_usd || 0) > 0
        ? fact('用量捕获', '上游未返回用量，费用按预占估算扣费') : '')
      + fact('首字延迟', fmtSec(x.ttft_ms))
      + fact('生成耗时', fmtSec(x.generation_ms))
      + fact('TPS', fmtTPS(x.tps_milli))
      + fact('总延迟', fmtSec(x.latency_ms))
      + fact('费用', x.currency === 'CNY' ? fmtMoney(x.cost_native_micro, '¥') + '（≈' + fmtCur(x.cost_micro_usd) + '）' : fmtCur(x.cost_micro_usd))
    + fact('计价币种', x.currency === 'CNY' ? '人民币（CNY）' : '美元（USD）')
    + fact('命中计价', x.priced ? '是' : '否')
      + (x.reservation_id ? fact('预占 ID', x.reservation_id) : '')
      + '</div>',
  });
});
const reqFilterChanged = () => {
  reqView.model = reqModelCombo.value;
  reqView.keyId = reqKeyCombo.value;
  reqView.result = reqResultSel.value;
  reqView.page = 0;
  loadRequests().catch(e => toast(e.message, 'err'));
};
const reqModelCombo = new Combo('req-model', reqFilterChanged);
const reqKeyCombo = new Combo('req-key', reqFilterChanged);
const reqResultSel = new Select('req-result', [
  { value: '', label: '全部结果' },
  { value: 'ok', label: '成功 ok' },
  { value: 'error', label: '失败 error' },
], reqFilterChanged, { value: '', head: '按结果过滤' });
const dimSel = new Select('dim', DIMS, () => loadDim().catch(e => toast(e.message, 'err')),
  { value: 'model', head: '聚合维度' });
// 列偏好：默认列收进视口，低频列按需开启（偏好存 localStorage）
new MultiSelect('req-cols', REQ_COLS.map(c => ({ value: c.id, label: c.label, fixed: c.fixed })),
  reqCols, sel => {
    reqCols = new Set(sel);
    savePref('req-cols', JSON.stringify([...sel]));
    loadRequests().catch(e => toast(e.message, 'err'));
  }, { text: '列', head: '显示列', defaults: REQ_COLS_DEFAULT });
$('req-export').addEventListener('click', async () => {
  try {
    await downloadFile('/export/csv', {
      kind: 'requests', limit: 100000,
      filter: Object.assign({ model: reqView.model, key_id: reqView.keyId, result: reqView.result }, rangeParams()),
    }, 'cpa-usage-manager-requests.csv');
    toast('CSV 已导出', 'ok');
  } catch (e) { toast(e.message, 'err'); }
});
// 趋势导出：PNG 由服务端渲染（与面板主题、缩放无关），CSV 为聚合数据。
// 均按当前时间范围与所选粒度/指标取数。
$('trend-export').addEventListener('click', () => {
  openSheet({
    title: '导出趋势数据',
    okText: '关闭', noFocus: true,
    body: '<p class="note" style="margin-top:0">按当前时间范围与所选指标导出；PNG 由服务端渲染，与面板主题无关。</p>'
      + '<div class="btn-row"><button type="button" class="btn" data-exp="png">图表 PNG</button>'
      + '<button type="button" class="btn" data-exp="csv">数据 CSV</button></div>',
  });
  $('sheet-body').querySelectorAll('[data-exp]').forEach(b => {
    b.onclick = async () => {
      const fmt = b.dataset.exp === 'png' ? 'png' : 'csv';
      try {
        await downloadFile(b.dataset.exp === 'png' ? '/export/png' : '/export/csv', {
          kind: 'trends', grain: trendGrainSel.value, metric: trendMetricSel.value, filter: rangeParams(),
        }, 'cpa-usage-manager-trends.' + fmt);
        toast('已导出', 'ok');
        animateCloseSheet();
      } catch (e) { toast(e.message, 'err'); }
    };
  });
});
$('dim-export').addEventListener('click', async () => {
  try {
    await downloadFile('/export/csv', {
      kind: 'dimension', dimension: dimSel.value, limit: 100000, filter: rangeParams(),
    }, 'cpa-usage-manager-dimension.csv');
    toast('CSV 已导出', 'ok');
  } catch (e) { toast(e.message, 'err'); }
});

