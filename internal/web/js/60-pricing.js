// ---------- 价格 ----------
const pricingCache = { items: [] };
let pricingPage = 0;
const PRICING_PAGE_SIZE = 10;
loaders.pricing = async () => {
  const [r, fx] = await Promise.all([api('/pricing'), api('/exchange-rate')]);
  S.fx = fx;
  $('fx-info').textContent = fx && fx.usd_to_cny_micro
    ? 'USD→CNY ' + (fx.usd_to_cny_micro / 1e6).toFixed(4) + ' · ' + fx.source + (fx.fallback ? '（兜底）' : '') : '';
  pricingCache.items = (r.items || []).slice().sort((a, b) => b.priority - a.priority || a.id - b.id);
  pricingPage = 0;
  renderPricing();
  stamp();
};
function renderPricing() {
  const items = pricingCache.items;
  // 表头带 ⓘ 说明：「缓存读/缓存写」这类术语中文里不自明，悬浮给出上游口径解释
  $('pricing-head').innerHTML =
    '<th class="num">' + labelWithTip('优先级', TIPS.priority) + '</th>'
    + '<th>' + labelWithTip('匹配', TIPS.matchKind) + '</th>'
    + '<th class="w-grow">模式</th><th>状态</th>'
    + '<th class="num">' + labelWithTip('输入', TIPS.input) + '</th>'
    + '<th class="num">' + labelWithTip('输出', TIPS.output) + '</th>'
    + '<th class="num">' + labelWithTip('缓存读', TIPS.cacheRead) + '</th>'
    + '<th class="num">' + labelWithTip('缓存写', TIPS.cacheWrite) + '</th>'
    + '<th>来源</th><th class="w-act"></th>';
  const pages = Math.max(1, Math.ceil(items.length / PRICING_PAGE_SIZE));
  const rows = items.slice(pricingPage * PRICING_PAGE_SIZE, (pricingPage + 1) * PRICING_PAGE_SIZE);
  // 价格列带币种单位：CNY 行 ¥、USD 行 $；CNY 悬浮给出当前实时汇率（美元等值按它折算）。
  const priceCell = (p, v) => p.currency === 'CNY'
    ? '<td class="num" title="按 1 USD = ' + curFxText() + ' CNY 折算美元等值（实时汇率）">' + fmtMoney(v, '¥') + '</td>'
    : '<td class="num">' + fmtPrice(v) + '</td>';
  $('pricing-rows').innerHTML = rows.map(p => '<tr>'
    + '<td class="num cell-mono">' + p.priority + '</td>'
    + '<td><span class="pill signal mono">' + esc(p.match_kind) + '</span></td>'
    + '<td class="cell-mono w-grow" title="' + esc(p.pattern) + '">' + esc(p.pattern) + '</td>'
    + '<td><span class="pill ' + (p.enabled ? 'live' : '') + '">' + (p.enabled ? '启用' : '停用') + '</span></td>'
    + priceCell(p, p.price_input)
    + priceCell(p, p.price_output)
    + priceCell(p, p.price_cache_read)
    + priceCell(p, p.price_cache_creation)
    + '<td class="cell-dim">' + (p.currency === 'CNY' ? '<span class="pill trace" title="按 1 USD = ' + curFxText() + ' CNY 折算美元等值（实时汇率）">CNY</span> ' : '')
    + esc(p.source === 'models_dev' ? 'models.dev' : '手动') + '</td>'
    + '<td class="w-act"><button type="button" class="btn small" data-edit="' + p.id + '">编辑</button>'
    + '<button type="button" class="btn small danger" data-id="' + p.id + '">删除</button></td></tr>').join('')
    || '<tr><td colspan="10"><div class="empty"><p class="empty-title">还没有计价规则</p>'
    + '<p class="empty-hint">新增规则，或在上方搜索 models.dev 后按条添加</p></div></td></tr>';
  $('pricing-pager').innerHTML = pages > 1
    ? '<span class="mono">第 ' + (pricingPage + 1) + ' / ' + pages + ' 页 · 共 ' + fmtInt(items.length) + ' 条</span><span class="grow"></span>'
      + '<button type="button" class="btn small" id="pricing-prev"' + (pricingPage <= 0 ? ' disabled' : '') + '>上一页</button>'
      + '<button type="button" class="btn small" id="pricing-next"'
      + ((pricingPage + 1) * PRICING_PAGE_SIZE >= items.length ? ' disabled' : '') + '>下一页</button>'
    : '';
  const prev = $('pricing-prev'), next = $('pricing-next');
  if (prev) prev.onclick = () => { pricingPage--; renderPricing(); };
  if (next) next.onclick = () => { pricingPage++; renderPricing(); };
}
$('pricing-rows').addEventListener('click', e => {
  const del = e.target.closest('button[data-id]');
  if (!del) return;
  confirmSheet('删除计价规则 #' + del.dataset.id,
    '删除后相关模型将回落到兜底规则（通常免费）。',
    async () => {
      await post('/pricing/delete', { id: parseInt(del.dataset.id, 10), actor: 'console' });
      loaders.pricing().catch(() => {});
    });
});
// micro-USD/百万 token → 弹窗里的 $/M 数值文本。
const priceInputVal = v => String((Number(v) || 0) / 1e6);
// curFxText 当前实时汇率文本（S.fx 未就绪时显示 —）。
function curFxText() { return S.fx && S.fx.usd_to_cny_micro ? (S.fx.usd_to_cny_micro / 1e6).toFixed(4) : '—'; }
function pricingFormBody(p) {
  const sel = (kind, cur) => '<option value="' + kind + '"' + (kind === cur ? ' selected' : '') + '>' + kind + '</option>';
  const kindSel = p
    ? ['exact', 'glob', 'regexp'].map(k => sel(k, p.match_kind)).join('')
    : '<option value="exact">exact 完全匹配</option><option value="glob" selected>glob 通配</option><option value="regexp">regexp 正则</option>';
  const cur = p ? (p.currency || 'USD') : 'USD';
  // 只保留真正参与计算的四档：推理并入输出、cached 并入缓存读，独立档位无处可用。
  return '<div class="form-grid">'
    + fieldRow(labelWithTip('匹配方式', TIPS.matchKind), '<select id="p-kind">' + kindSel + '</select>')
    + fieldRow(labelWithTip('优先级', TIPS.priority), '<input id="p-priority" type="number" value="' + (p ? p.priority : 100) + '">')
    + fieldRow('模式', '<input id="p-pattern" value="' + (p ? esc(p.pattern) : '') + '" placeholder="如 gpt-* 或 claude-sonnet-4" spellcheck="false">')
    + fieldRow('状态', '<select id="p-enabled"><option value="true"' + (!p || p.enabled ? ' selected' : '') + '>启用</option>'
      + '<option value="false"' + (p && !p.enabled ? ' selected' : '') + '>停用</option></select>')
    + fieldRow(labelWithTip('计价币种', '人民币规则价格与原生入账恒为 CNY，按原值记账；美元等值仅是额度扣减与跨币种聚合的换算口径，按当前实时汇率折算（后台每 30 分钟自动刷新，不随规则保存锁定）。'), '<select id="p-cur"><option value="USD"' + (cur === 'USD' ? ' selected' : '') + '>USD 美元</option>'
      + '<option value="CNY"' + (cur === 'CNY' ? ' selected' : '') + '>CNY 人民币</option></select>')
    + '<div class="form-sep wide">单价（每百万 token）</div>'
    + fieldRow(labelWithTip('输入', TIPS.input), '<input id="p-in" inputmode="decimal" value="' + (p ? priceInputVal(p.price_input) : '') + '" placeholder="0">')
    + fieldRow(labelWithTip('输出', TIPS.output), '<input id="p-out" inputmode="decimal" value="' + (p ? priceInputVal(p.price_output) : '') + '" placeholder="0">')
    + fieldRow(labelWithTip('缓存读', TIPS.cacheRead), '<input id="p-cache-read" inputmode="decimal" value="' + (p ? priceInputVal(p.price_cache_read) : '') + '" placeholder="0">')
    + fieldRow(labelWithTip('缓存写', TIPS.cacheWrite), '<input id="p-cache-create" inputmode="decimal" value="' + (p ? priceInputVal(p.price_cache_creation) : '') + '" placeholder="0">')
    + '</div>';
}
function pricingSubmit() {
  const num = id => Math.round((parseFloat($(id).value) || 0) * 1e6);
  const cur = $('p-cur').value;
  return {
    match_kind: $('p-kind').value,
    pattern: $('p-pattern').value.trim() || '*',
    priority: parseInt($('p-priority').value, 10) || 0,
    enabled: $('p-enabled').value === 'true',
    currency: cur,
    price_input: num('p-in'), price_output: num('p-out'),
    price_cache_read: num('p-cache-read'), price_cache_creation: num('p-cache-create'),
    accounting_mode: 'default', billing_mode: 'token', per_image_micro_usd: 0,
    source: 'manual',
  };
}
$('pricing-add').addEventListener('click', () => {
  openSheet({
    title: '新增计价规则', okText: '保存',
    body: pricingFormBody(null),
    note: '单价为每百万 Token 金额，币种可选美元或人民币；人民币按原值入账，美元等值按当前实时汇率折算（额度扣减恒按美元）。同匹配方式同模式重复添加将覆盖原规则。',
    onOk: async () => {
      await post('/pricing', pricingSubmit());
      toast('规则已保存', 'ok');
      loaders.pricing().catch(() => {});
    },
  });
});
$('pricing-rows').addEventListener('click', e => {
  const b = e.target.closest('button[data-edit]');
  if (!b) return;
  const p = pricingCache.items.find(x => x.id === parseInt(b.dataset.edit, 10));
  if (!p) return;
  openSheet({
    title: '编辑计价规则 #' + p.id, okText: '保存',
    body: pricingFormBody(p),
    note: '修改匹配方式或模式会按新键生效；若新键已存在，两条将合并为一条。计价口径与来源保持原值。',
    onOk: async () => {
      const body = pricingSubmit();
      body.id = p.id;
      // 口径字段不在表单里，保留原值避免把按张计价 / models.dev 规则改坏。
      body.accounting_mode = p.accounting_mode;
      body.billing_mode = p.billing_mode;
      body.per_image_micro_usd = p.per_image_micro_usd;
      body.source = p.source;
      body.models_dev_id = p.models_dev_id;
      await post('/pricing', body);
      toast('规则已更新', 'ok');
      loaders.pricing().catch(() => {});
    },
  });
});
// models.dev 搜索后按条添加：目录在服务端缓存 10 分钟，避免整本同步的瞬时 IO。
async function pricingSearchRun() {
  const q = $('pricing-search-input').value.trim();
  if (!q) { toast('先输入模型关键词', 'err'); return; }
  const box = $('pricing-search-results');
  box.hidden = false;
  box.innerHTML = pricingSearchHead() + '<p class="note" style="padding:10px 14px">正在搜索 models.dev…</p>';
  try {
    const list = await api('/pricing/search?' + new URLSearchParams({ q, limit: '20' }));
    box.dataset.items = JSON.stringify(list);
    box.innerHTML = pricingSearchHead()
      + (list.length
        ? '<div class="search-list">' + list.map((c, i) =>
          '<div class="search-item"><div class="si-main">'
          + '<span class="si-name">' + esc(c.name || c.model_id) + '</span>'
          + '<span class="si-id mono">' + esc(c.provider_id) + '/' + esc(c.model_id) + '</span></div>'
          + '<span class="si-price mono">入 ' + fmtPrice(c.price_input) + ' · 出 ' + fmtPrice(c.price_output) + '</span>'
          + '<button type="button" class="btn small primary" data-si="' + i + '">添加</button></div>').join('') + '</div>'
        : '<p class="note" style="padding:10px 14px">没有匹配的模型，换个关键词试试。</p>');
  } catch (e) { box.innerHTML = pricingSearchHead() + '<p class="note" style="padding:10px 14px">' + esc(e.message) + '</p>'; }
}
// pricingSearchHead 搜索结果面板的标题栏，带关闭按钮（结果面板本身没有原生收起入口）。
function pricingSearchHead() {
  return '<div class="search-head"><span>models.dev 搜索结果</span>'
    + '<button type="button" class="btn small" data-search-close>关闭</button></div>';
}
$('pricing-search-results').addEventListener('click', e => {
  if (!e.target.closest('[data-search-close]')) return;
  $('pricing-search-results').hidden = true;
});
$('pricing-search-btn').addEventListener('click', pricingSearchRun);
$('pricing-search-input').addEventListener('keydown', e => {
  if (e.key === 'Enter') { e.preventDefault(); pricingSearchRun(); }
});
$('pricing-reset').addEventListener('click', () => {
  openSheet({
    title: '清空计价规则',
    okText: '清空', danger: true,
    body: '<p>将删除全部自定义计价规则，只保留全模型免费兜底规则（glob:*）。'
      + '密钥额度与用量数据不受影响。此操作不可撤销，确定继续吗？</p>',
    onOk: async () => {
      const r = await post('/pricing/reset', { actor: 'console' });
      toast('已清空 ' + (r.deleted || 0) + ' 条计价规则', 'ok');
      loaders.pricing().catch(() => {});
    },
  });
});
$('pricing-search-results').addEventListener('click', async e => {
  const b = e.target.closest('button[data-si]');
  if (!b) return;
  let list = [];
  try { list = JSON.parse($('pricing-search-results').dataset.items || '[]'); } catch (_) { /* 忽略 */ }
  const c = list[+b.dataset.si];
  if (!c) return;
  b.disabled = true;
  b.textContent = '已添加';
  try {
    await post('/pricing', {
      match_kind: 'exact', pattern: c.pattern, priority: 100, enabled: true,
      price_input: c.price_input, price_output: c.price_output,
      price_cache_read: c.price_cache_read, price_cache_creation: c.price_cache_creation,
      accounting_mode: 'default', billing_mode: 'token', per_image_micro_usd: 0,
      source: c.source, models_dev_id: c.models_dev_id,
    });
    toast('已添加 ' + c.model_id, 'ok');
    loaders.pricing().catch(() => {});
  } catch (err) {
    b.disabled = false;
    b.textContent = '添加';
    toast(err.message, 'err');
  }
});
$('fx-refresh').addEventListener('click', async () => {
  try {
    S.fx = await post('/exchange-rate');
    toast('汇率已刷新', 'ok');
    loaders.pricing().catch(() => {});
  } catch (e) { toast(e.message, 'err'); }
});

// ---------- 模型集合（路由别名） ----------
// 集合别名 = 一条规则脚本 + 有序目标链；保存期服务端编译校验并回填 refs。
const routeCache = { items: [], judge: { model: '', timeout_ms: 8000 }, modelsLoaded: false };
// ROUTE_DOC_URL 规则语言的完整手册（仓库 docs/routing.md 的 GitHub 页面）。
const ROUTE_DOC_URL = 'https://github.com/drowsylazy/cpa-usage-manager/blob/main/docs/routing.md';
const TIPS_routeRule = 'when 条件 -> 候选链，自上而下第一条命中生效；末行必须是无条件兜底分支。变量、运算符与示例见「规则手册」链接。';

// unescapeEntities 把上游链路可能注入的 HTML 实体还原为原文（textarea 内层
// 解码法，安全无脚本执行）。与后端保存期 decodeHTMLEntities 配对：存盘已归一，
// 这里兜住「响应途中再被转义」的显示侧。
function unescapeEntities(s) {
  if (!s || s.indexOf('&') < 0) return s;
  const ta = document.createElement('textarea');
  ta.innerHTML = s;
  return ta.value;
}

loaders.routes = async () => {
  const r = await api('/model-routes');
  routeCache.items = (r.items || []).map(it => ({
    ...it,
    alias: unescapeEntities(it.alias),
    rule: unescapeEntities(it.rule),
  }));
  routeCache.judge = r.judge || routeCache.judge;
  if (routeCache.judge) routeCache.judge.model = unescapeEntities(routeCache.judge.model);
  renderRouteJudgeState();
  renderRouteCards();
  await loadRoutesHealth();
  stamp();
};
// loadRoutesHealth 渲染「目标健康」面板：冷却状态 + 近 60 分钟失败统计。
// 排序：冷却中的目标排最前（最值得关注），其次按失败数降序，最后按请求量降序。
async function loadRoutesHealth() {
  const rows = $('rh-rows'), note = $('rh-note');
  let items;
  try {
    const r = await api('/model-routes/health');
    items = (r.items || []).slice().sort((a, b) =>
      (b.cooling - a.cooling) || ((b.fail_60m || 0) - (a.fail_60m || 0)) || ((b.total_60m || 0) - (a.total_60m || 0)));
  } catch (e) {
    rows.innerHTML = '';
    note.textContent = '加载失败：' + e.message;
    return;
  }
  rows.innerHTML = items.map(h =>
    '<tr><td class="cell-mono cell-clip" title="' + esc(h.alias) + '">' + esc(h.alias || '-') + '</td>'
    + '<td class="cell-mono cell-clip" title="' + esc(h.target) + '">' + esc(h.target) + '</td>'
    + '<td>' + (h.cooling
      ? '<span class="pill warn" title="冷却剩余 ' + h.cooldown_remaining_sec + ' 秒">冷却 ' + fmtDur(h.cooldown_remaining_sec) + '</span>'
      : '<span class="pill live">可用</span>')
    + '</td>'
    + '<td class="num">' + fmtInt(h.total_60m || 0) + '</td>'
    + '<td class="num">' + (h.fail_60m > 0
      ? '<span class="pill alarm">' + fmtInt(h.fail_60m) + '</span>'
      : '<span class="cell-dim">0</span>')
    + '</td></tr>').join('');
  note.textContent = items.length
    ? '冷却是进程内启发式：重启丢失、多实例各自独立；失败目标恢复成功后立即回到可用池。'
    : '暂无启用中的集合，或启用集合的规则未引用任何目标。';
}
$('rh-refresh').addEventListener('click', () => { loadRoutesHealth().catch(() => {}); });
function renderRouteJudgeState() {
  const badge = $('route-judge-state');
  if (badge) badge.hidden = !!routeCache.judge.model;
}
function routeCard(r) {
  const refs = (r.refs || []).map(x => '<span class="rt-ref">' + esc(x) + '</span>').join('')
    || '<span class="note">规则未引用任何目标</span>';
  const rulePreview = esc(r.rule || '');
  const modeBadge = r.pricing_mode === 'alias' ? '<span class="pill trace">按别名声价</span>' : '<span class="pill signal">按目标计价</span>';
  return '<div class="rt-card' + (r.enabled ? '' : ' off') + '" role="listitem" data-id="' + r.id + '">'
    + '<div class="rt-top"><span class="rt-alias">' + esc(r.alias) + '</span>'
    + '<span class="pill ' + (r.enabled ? 'live' : '') + '">' + (r.enabled ? '启用' : '停用') + '</span></div>'
    + '<div class="rt-meta">' + modeBadge
    + '<span>冷却 <b>' + (r.cooldown_seconds || 0) + 's</b></span></div>'
    + '<pre class="rt-rule" title="规则脚本">' + rulePreview + '</pre>'
    + '<div class="rt-chips">' + refs + '</div>'
    + '<div class="rt-acts">'
    + '<button type="button" class="btn small" data-route-toggle="' + r.id + '">' + (r.enabled ? '停用' : '启用') + '</button>'
    + '<button type="button" class="btn small" data-route-edit="' + r.id + '">编辑</button>'
    + '<button type="button" class="btn small danger" data-route-del="' + r.id + '">删除</button>'
    + '</div></div>';
}
function renderRouteCards() {
  const items = routeCache.items;
  $('route-rows').innerHTML = items.length
    ? items.map(routeCard).join('')
    : '<div class="empty"><p class="empty-title">还没有模型集合</p>'
      + '<p class="empty-hint">新建集合后，插件 Key 请求别名即按规则路由到健康目标，失败自动转移。</p></div>';
}
async function loadRouteModelList() {
  if (routeCache.modelsLoaded) return;
  try {
    const r = await api('/usage/dimension?' + new URLSearchParams({ dimension: 'model', limit: '200' }));
    $('route-model-list').innerHTML = (r.rows || []).filter(x => x.value)
      .map(x => '<option value="' + esc(x.value) + '">').join('');
    routeCache.modelsLoaded = true;
  } catch (_) { /* 候选列表只是辅助，失败不打断编辑 */ }
}
function routeFormBody(r) {
  return '<div class="form-grid">'
    + fieldRow('别名', '<input id="rt-alias" list="route-model-list" value="' + (r ? esc(r.alias) : '') + '" placeholder="如 auto 或 grp/name（可含 /，撞真实模型名会被拒绝）" spellcheck="false" maxlength="128">')
    + fieldRow('状态', '<select id="rt-enabled"><option value="true"' + (!r || r.enabled ? ' selected' : '') + '>启用</option>'
      + '<option value="false"' + (r && !r.enabled ? ' selected' : '') + '>停用</option></select>')
    + fieldRow('计价模式', '<select id="rt-mode"><option value="target"' + (!r || r.pricing_mode !== 'alias' ? ' selected' : '') + '>按实际目标计价</option>'
      + '<option value="alias"' + (r && r.pricing_mode === 'alias' ? ' selected' : '') + '>按别名自身计价</option></select>')
    + fieldRow(labelWithTip('冷却秒数', '目标失败后的进程内冷却时长；冷却期内该目标被跳过，到期自动恢复。0 为不冷却。'),
      '<input id="rt-cooldown" type="number" min="0" max="86400" value="' + (r ? (r.cooldown_seconds || 0) : 60) + '">')
    + fieldRow('全冷却时', '<select id="rt-policy"><option value="block"' + (!r || r.cooldown_policy !== 'force' ? ' selected' : '') + '>拒绝请求</option>'
      + '<option value="force"' + (r && r.cooldown_policy === 'force' ? ' selected' : '') + '>忽略冷却照打</option></select>')
    + '</div>'
    + fieldRow(labelWithTip('规则脚本', TIPS_routeRule),
      '<textarea id="rt-rule" class="mono-area" spellcheck="false" placeholder=\'-> "gpt-4o-mini"\'>'
      + esc(r ? r.rule : '') + '</textarea>')
    + '<div class="btn-row"><button type="button" class="btn small" data-rt-test="' + (r ? r.id : 0) + '">测试此规则…</button>'
    + '<a class="doc-link" href="' + ROUTE_DOC_URL + '" target="_blank" rel="noopener">规则手册 ↗</a></div>';
}
function collectRouteForm() {
  return {
    alias: $('rt-alias').value.trim(),
    enabled: $('rt-enabled').value === 'true',
    pricing_mode: $('rt-mode').value,
    cooldown_seconds: Math.max(0, Math.min(86400, parseInt($('rt-cooldown').value, 10) || 0)),
    cooldown_policy: $('rt-policy').value,
    rule: $('rt-rule').value,
  };
}
// routeSaveOnOk 生成集合表单的保存闭包。抽出来是为了「测试规则」视图切换：
// 离开编辑器去测试时暂存表单快照，回来后重建同一份保存闭包。
function routeSaveOnOk(id) {
  return async () => {
    const body = id ? { id, actor: 'console', ...collectRouteForm() } : { actor: 'console', ...collectRouteForm() };
    const res = await post('/model-routes/save', body);
    if (res.warning) toast(res.warning, 'err');
    toast(id ? '集合已更新' : '集合已创建', 'ok');
    loaders.routes().catch(() => {});
  };
}
$('route-add').addEventListener('click', () => {
  loadRouteModelList();
  openSheet({
    title: '新建模型集合', okText: '保存',
    body: routeFormBody(null),
    note: TIPS_routeRule,
    onOk: routeSaveOnOk(0),
  });
});
$('route-rows').addEventListener('click', e => {
  const editBtn = e.target.closest('button[data-route-edit]');
  if (editBtn) {
    const r = routeCache.items.find(x => x.id === parseInt(editBtn.dataset.routeEdit, 10));
    if (!r) return;
    loadRouteModelList();
    openSheet({
      title: '编辑集合 · ' + r.alias, okText: '保存',
      body: routeFormBody(r),
      note: TIPS_routeRule,
      onOk: routeSaveOnOk(r.id),
    });
    return;
  }
  const delBtn = e.target.closest('button[data-route-del]');
  if (delBtn) {
    const id = parseInt(delBtn.dataset.routeDel, 10);
    const r = routeCache.items.find(x => x.id === id);
    confirmSheet('删除集合 · ' + (r ? r.alias : '#' + id),
      '删除后请求该别名将不再被接管（宿主按未知模型报错）。历史统计不受影响。',
      async () => {
        await post('/model-routes/delete', { id, actor: 'console' });
        loaders.routes().catch(() => {});
      });
    return;
  }
  const toggleBtn = e.target.closest('button[data-route-toggle]');
  if (toggleBtn) {
    const id = parseInt(toggleBtn.dataset.routeToggle, 10);
    const r = routeCache.items.find(x => x.id === id);
    if (!r) return;
    // 快捷开关复用 save 端点全量提交，避免额外端点。
    post('/model-routes/save', {
      id: r.id, alias: r.alias, rule: r.rule,
      cooldown_seconds: r.cooldown_seconds, pricing_mode: r.pricing_mode,
      cooldown_policy: r.cooldown_policy || 'block',
      enabled: !r.enabled, actor: 'console',
    }).then(() => loaders.routes().catch(() => {}))
      .catch(err => toast(err.message, 'err'));
  }
});
$('route-judge-btn').addEventListener('click', () => {
  const j = routeCache.judge || {};
  openSheet({
    title: 'AI 评判设置', okText: '保存',
    body: '<div class="form-grid">'
      + fieldRow(labelWithTip('评判模型', '执行 ai_judge 时调用的模型名，经宿主正常转发与计费；留空表示未配置，含 ai_judge 的规则将无法保存或回落兜底分支。'),
        '<input id="jd-model" value="' + esc(j.model || '') + '" placeholder="如 gpt-4o-mini" spellcheck="false">')
      + fieldRow(labelWithTip('超时（毫秒）', 'ai_judge 在转发前同步执行，最长等待此时长；超时即回落兜底分支。500~120000。'),
        '<input id="jd-timeout" type="number" min="500" max="120000" step="100" value="' + (j.timeout_ms || 8000) + '">')
      + '</div>',
    note: '发送给评判模型的是脱敏摘要：结构化指标加对话文本前 2000 字符，绝不发送完整请求体。同一输入组合的结论缓存 10 分钟。',
    onOk: async () => {
      const model = $('jd-model').value.trim();
      const timeout_ms = Math.max(500, Math.min(120000, parseInt($('jd-timeout').value, 10) || 8000));
      const saved = await post('/model-routes/judge', { model, timeout_ms });
      routeCache.judge = saved && saved.model !== undefined ? saved : { model, timeout_ms };
      renderRouteJudgeState();
      toast('评判设置已保存', 'ok');
    },
  });
});

// ---------- 规则干跑测试 ----------
// 编辑器内做视图切换而非叠加第二个对话框：规则草稿尚未保存，快照后整块
// 换入测试面板，返回时按快照还原表单与保存闭包，任何路径都不丢编辑内容。

// routeEditSnapshot 是进入测试视图时的编辑器状态；null 表示不在测试视图。
let routeEditSnapshot = null;

function enterRouteTestView(id) {
  const snap = collectRouteForm();
  routeEditSnapshot = { id, snap, title: $('sheet-title').textContent };
  const judgeReady = !!(routeCache.judge && routeCache.judge.model);
  const usesAI = (snap.rule || '').indexOf('ai_judge') >= 0;
  $('sheet-title').textContent = '测试规则 · ' + (snap.alias || '(未命名)');
  $('sheet-body').innerHTML =
    '<div class="btn-row" style="margin-bottom:8px"><button type="button" class="btn small" id="rt-test-back">← 返回编辑</button></div>'
    + '<pre class="rt-rule">' + (snap.rule ? esc(snap.rule) : '(空规则)') + '</pre>'
    + fieldRow(labelWithTip('提示词',
        '合成一条 user 消息参与干跑：body_len / input_tokens 按它计算，含 ai_judge 的规则用它做摘要。'),
      '<textarea id="rt-test-prompt" class="mono-area" rows="4" style="min-height:96px" placeholder="输入一段用户提示词，模拟一次真实请求"></textarea>')
    + '<div class="form-grid">'
    + fieldRow('请求模型名', '<input id="rt-test-model" value="' + esc(snap.alias) + '" spellcheck="false">')
    + fieldRow('入口格式', '<select id="rt-test-source"><option value="openai">openai</option><option value="claude">claude</option><option value="gemini">gemini</option><option value="chat-completions">chat-completions</option></select>')
    + '</div>'
    + '<div class="btn-row rt-test-opts">'
    + '<label class="rt-opt"><input type="checkbox" id="rt-test-stream"> 流式请求</label>'
    + (usesAI
        ? '<label class="rt-opt"><input type="checkbox" id="rt-test-ai"' + (judgeReady ? '' : ' disabled') + '> 执行 ai_judge（真实调用评判模型）' + (judgeReady ? '' : ' —— 需先配置评判模型') + '</label>'
        : '')
    + '</div>'
    + '<p class="note">点右下角「运行测试」。求值为纯干跑：不请求目标模型、不产生计费与统计。</p>'
    + '<div id="rt-test-out"></div>';
  const ok = $('sheet-ok');
  ok.textContent = '运行测试';
  sheetOk = async () => {
    try { await runRouteTest(); }
    finally { ok.disabled = false; }
    return false; // 保持对话框打开，结果就地展示
  };
  const promptBox = $('rt-test-prompt');
  if (promptBox) promptBox.focus();
}

function exitRouteTestView() {
  const s = routeEditSnapshot;
  if (!s) return;
  routeEditSnapshot = null;
  $('sheet-title').textContent = s.title;
  $('sheet-body').innerHTML = routeFormBody({ ...s.snap, id: s.id });
  const ok = $('sheet-ok');
  ok.textContent = '保存';
  ok.className = 'btn primary';
  ok.disabled = false;
  sheetOk = routeSaveOnOk(s.id);
  loadRouteModelList();
  const rule = $('rt-rule');
  if (rule) rule.focus();
}

async function runRouteTest() {
  const s = routeEditSnapshot;
  if (!s) return;
  return post('/model-routes/test', {
    id: s.id,
    alias: s.snap.alias,
    rule: s.snap.rule,
    model: $('rt-test-model').value.trim(),
    stream: $('rt-test-stream').checked,
    source: $('rt-test-source').value,
    prompt: $('rt-test-prompt').value,
    run_ai: !!($('rt-test-ai') && $('rt-test-ai').checked),
  }).then(renderRouteTestOut.bind(null, $('rt-test-out')));
}

// renderRouteTestOut 展示干跑结论：最终目标突出显示，回退链、被冷却摘除的
// 目标与本次使用的变量值一并给出，便于核对规则分支是否按预期命中。
function renderRouteTestOut(box, r) {
  let html = '';
  if (r.error) {
    box.innerHTML = '<div class="rt-test-msg err"><b>未能求值</b>\n' + esc(unescapeEntities(r.error)) + '</div>';
    return;
  }
  const chain = (r.chain || []).map(m => esc(unescapeEntities(m)));
  if (!chain.length) {
    html += '<div class="rt-test-msg warn">所有候选目标都在冷却中——实际请求将收到「全部冷却」错误，稍后再试或调短冷却秒数。</div>';
  } else {
    html += '<div class="rt-test-hit"><span>最终目标</span><code>' + chain[0] + '</code></div>';
    if (chain.length > 1) html += '<p class="note">回退链：' + chain.join(' → ') + '</p>';
  }
  for (const sk of r.skipped || []) {
    html += '<p class="note">已跳过冷却中的目标 <b>' + esc(unescapeEntities(sk.target)) + '</b>（至 ' + fmtDT(sk.until, true) + '）</p>';
  }
  if (r.fell_back) {
    html += '<p class="note">结果来自兜底分支'
      + (r.ai_skipped ? '（本次未执行 ai_judge；勾选「执行 ai_judge」可真实评判）' : '（ai_judge 执行失败）')
      + '</p>';
  } else if (r.ai_skipped) {
    html += '<p class="note">条件分支里含 ai_judge 的未参与本次判定（未勾选执行）。</p>';
  }
  const v = r.vars || {};
  html += '<p class="rt-test-vars">input_tokens=' + esc(String(v.input_tokens ?? '-'))
    + ' · body_len=' + esc(String(v.body_len ?? '-'))
    + ' · model=' + esc(unescapeEntities(String(v.model ?? '')))
    + ' · stream=' + (v.stream ? 'true' : 'false')
    + ' · thinking_effort=' + esc(String(v.thinking_effort || '(空)'))
    + ' · source=' + esc(unescapeEntities(String(v.source || '')))
    + '</p>';
  box.innerHTML = html;
}
$('sheet-body').addEventListener('click', e => {
  if (e.target.closest('[data-rt-test]')) {
    enterRouteTestView(parseInt(e.target.closest('[data-rt-test]').dataset.rtTest, 10));
    return;
  }
  if (e.target.closest('#rt-test-back')) exitRouteTestView();
});

