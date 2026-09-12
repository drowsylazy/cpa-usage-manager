// ---------- 密钥 ----------
const keysView = {
  cache: [], total: 0, statusCounts: {}, page: 0, size: 20, search: '', caller: '', status: '',
  balanceSeq: 0,    // 余额异步响应的竞态守卫：快速切换抽屉对象时丢弃晚到的旧响应
};
// keyLabelOf 由 kid 查密钥标签；无标签或缓存未热时返回空串，由调用方决定回落值。
// keysView.cache 只有当前分页页，先查跨页候选缓存 keyCandidates（/keys/candidates 全量口径）。
// keyCandidateMap 与候选列表同源维护：loadHeld 每 5s 渲染上百行都要查标签，
// 对 2000 条候选线性 find 是持续的 O(行数×候选数) 开销。
let keyCandidates = [];
const keyCandidateMap = new Map();
function setKeyCandidates(items) {
  keyCandidates = items || [];
  keyCandidateMap.clear();
  for (const c of keyCandidates) keyCandidateMap.set(c.kid, c);
}
let keyCandPromise = null;
// loadKeyCandidates 会话级缓存候选：用量页/实时页每次进入都拉 2000 条是
// 重复网络往返。签发/编辑/撤销等密钥操作后经 refreshKeys 失效重拉。
function loadKeyCandidates() {
  if (!keyCandPromise) {
    keyCandPromise = api('/keys/candidates')
      .then(r => { setKeyCandidates(r.items || []); return keyCandidates; })
      .catch(e => { keyCandPromise = null; throw e; });
  }
  return keyCandPromise;
}
function keyLabelOf(kid) {
  const inPage = keysView.cache.find(x => x.kid === kid);
  if (inPage) return inPage.label || '';
  const c = keyCandidateMap.get(kid);
  return c && c.label ? c.label : '';
}

loaders.keys = async () => { await refreshKeys(); };
// 密钥列表走服务端分页：limit/offset/status 都下推到 SQL，避免大基数用户
// 一次拉上千条。status_counts 由后端附带，徽标与「共 N 枚」仍拿得到全量口径。
async function refreshKeys() {
  // 密钥可能刚被增删改：失效会话级候选缓存，下次用时重拉。
  keyCandPromise = null;
  const q = new URLSearchParams({ limit: String(keysView.size), offset: String(keysView.page * keysView.size) });
  if (keysView.search) q.set('search', keysView.search);
  if (keysView.caller) q.set('caller_id', keysView.caller);
  if (keysView.status) q.set('status', keysView.status);
  const r = await api('/keys?' + q);
  keysView.cache = r.items || [];
  keysView.total = r.total || 0;
  keysView.statusCounts = r.status_counts || {};
  const pages = Math.max(1, Math.ceil(keysView.total / keysView.size));
  if (keysView.page >= pages && keysView.page > 0) {
    keysView.page = pages - 1;
    return refreshKeys();
  }
  renderKeys();
  updateBadges();
  // 详情 dialog 开着时同步刷新其内容；对象已被删除则关闭。
  const d = $('key-dialog');
  if (d.open && d.dataset.kid) {
    const nk = keysView.cache.find(x => x.kid === d.dataset.kid);
    if (nk) renderKeyDialog(nk); else animateCloseKeyDialog();
  }
  stamp();
}
function keyStatus(k) {
  if (k.revoked_at) return 'revoked';
  if (!k.enabled) return 'disabled';
  if (k.expires_at && new Date(k.expires_at) <= new Date()) return 'expired';
  return 'active';
}
const STATUS_META = {
  active: { label: '启用中', pill: 'live' },
  disabled: { label: '已禁用', pill: 'warn' },
  revoked: { label: '已撤销', pill: 'alarm' },
  expired: { label: '已过期', pill: '' },
};
function cycleKeysNow(d = new Date()) {
  const y = d.getUTCFullYear(), m = d.getUTCMonth(), day = d.getUTCDate();
  const u = new Date(Date.UTC(y, m, day));
  const dow = (u.getUTCDay() + 6) % 7;
  const thu = new Date(u); thu.setUTCDate(u.getUTCDate() - dow + 3);
  const jan4 = new Date(Date.UTC(thu.getUTCFullYear(), 0, 4));
  const jdow = (jan4.getUTCDay() + 6) % 7;
  jan4.setUTCDate(jan4.getUTCDate() - jdow + 3);
  const week = 1 + Math.round((thu - jan4) / (7 * 864e5));
  return {
    daily: u.toISOString().slice(0, 10),
    weekly: String(thu.getUTCFullYear()).padStart(4, '0') + '-W' + pad2(week),
    monthly: u.toISOString().slice(0, 7),
  };
}
function todaySpent(k) {
  return k.daily_cycle_key === cycleKeysNow().daily ? k.daily_spent_micro_usd : 0;
}
// balRow 配额清单的一行：名称 | 进度条 | 余 X / 上限，单行三列。
// 「不限」/数据缺失行也渲染空轨道：骨架→数据替换时行高不变、进度条不突兀消失。
function balRow(name, limit, remain, fmt) {
  if (!limit || limit <= 0) {
    return '<div class="bal-row bal-off"><span class="bal-name">' + name + '</span>'
      + '<span class="bal-bar"></span>'
      + '<span class="bal-free">不限</span></div>';
  }
  if (remain === null || remain === undefined) {
    return '<div class="bal-row"><span class="bal-name">' + name + '</span>'
      + '<span class="bal-bar"></span>'
      + '<span class="bal-val" title="余量数据缺失">—</span></div>';
  }
  const used = Math.min(limit, Math.max(0, limit - remain));
  const pct = Math.min(100, Math.max(0, used / limit * 100));
  const state = pct >= 95 ? 'alarm' : pct >= 80 ? 'warn' : '';
  return '<div class="bal-row" data-state="' + state + '">'
    + '<span class="bal-name">' + name + '</span>'
    + '<span class="bal-bar"><span style="width:' + pct.toFixed(1) + '%"></span></span>'
    + '<span class="bal-val mono">余 ' + fmt(Math.max(0, remain)) + ' / ' + fmt(limit) + '</span></div>';
}
// burnEtaText 按今日已消耗推算额度触顶时间：日速率 = 今日已用 / 今日已过
// 比例（UTC 口径，与额度周期一致）。今日无消耗、刚开日不足 5%（外推不可
// 靠）或余量已为 0 时返回空串——触顶与否进度条自会示警，不重复播报。
function burnEtaText(lim, used, spentToday) {
  if (lim === null || lim === undefined || lim <= 0) return '';
  if (!(spentToday > 0)) return '';
  const now = new Date();
  const midnight = Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate());
  const elapsed = (Date.now() - midnight) / 864e5;
  if (elapsed < 0.05) return '';
  const remain = lim - used;
  if (remain <= 0) return '';
  const days = remain / (spentToday / elapsed);
  if (days >= 365) return '一年以上';
  if (days < 1) return '今日内';
  return '约 ' + Math.max(1, Math.round(days)) + ' 天';
}
// keyEtaText 汇总金额/Token 两族的触顶预估：档位选取与卡片 usdPick/tokPick
// 同口径（优先总额，其次当前周期未滚动的日/周/月）。
function keyEtaText(k) {
  const c = cycleKeysNow();
  const today = k.daily_cycle_key === c.daily;
  const usdToday = today ? k.daily_spent_micro_usd : 0;
  const tokToday = today ? k.daily_tokens_used : 0;
  const parts = [];
  if (k.quota_micro_usd !== null && k.quota_micro_usd !== undefined) {
    const t = burnEtaText(k.quota_micro_usd, k.spent_micro_usd, usdToday);
    if (t) parts.push('金额 ' + t);
  } else if (k.daily_micro_usd !== null && k.daily_micro_usd !== undefined) {
    const t = burnEtaText(k.daily_micro_usd, usdToday, usdToday);
    if (t) parts.push('金额 ' + t);
  } else if (k.weekly_micro_usd !== null && k.weekly_micro_usd !== undefined) {
    const t = burnEtaText(k.weekly_micro_usd, k.weekly_cycle_key === c.weekly ? k.weekly_spent_micro_usd : 0, usdToday);
    if (t) parts.push('金额 ' + t);
  } else if (k.monthly_micro_usd !== null && k.monthly_micro_usd !== undefined) {
    const t = burnEtaText(k.monthly_micro_usd, k.monthly_cycle_key === c.monthly ? k.monthly_spent_micro_usd : 0, usdToday);
    if (t) parts.push('金额 ' + t);
  }
  if (k.token_limit !== null && k.token_limit !== undefined) {
    const t = burnEtaText(k.token_limit, k.tokens_used, tokToday);
    if (t) parts.push('Token ' + t);
  } else if (k.daily_token_limit !== null && k.daily_token_limit !== undefined) {
    const t = burnEtaText(k.daily_token_limit, tokToday, tokToday);
    if (t) parts.push('Token ' + t);
  } else if (k.weekly_token_limit !== null && k.weekly_token_limit !== undefined) {
    const t = burnEtaText(k.weekly_token_limit, k.weekly_cycle_key === c.weekly ? k.weekly_tokens_used : 0, tokToday);
    if (t) parts.push('Token ' + t);
  } else if (k.monthly_token_limit !== null && k.monthly_token_limit !== undefined) {
    const t = burnEtaText(k.monthly_token_limit, k.monthly_cycle_key === c.monthly ? k.monthly_tokens_used : 0, tokToday);
    if (t) parts.push('Token ' + t);
  }
  return parts.join(' · ');
}
function renderKeys() {
  // 服务端分页：cache 即当前页，total/status_counts 是筛选后的全量口径。
  const rows = keysView.cache;
  const allTotal = Object.values(keysView.statusCounts || {}).reduce((a, b) => a + Number(b) || 0, 0);
  const filtered = keysView.search || keysView.caller || keysView.status;
  $('key-count').textContent = '共 ' + allTotal + ' 枚'
    + (filtered ? ' · 筛选后 ' + keysView.total + ' 枚' : '');
  $('key-rows').innerHTML = rows.map(keyCardHTML).join('')
    || '<div class="empty"><p class="empty-title">没有匹配的密钥</p>'
    + '<p class="empty-hint">调整筛选条件，或点击右上角「签发密钥」</p></div>';

  const pages = Math.max(1, Math.ceil(keysView.total / keysView.size));
  $('key-pager').innerHTML = '<span class="mono">第 ' + (keysView.page + 1) + ' / ' + pages + ' 页</span>'
    + '<span class="grow"></span>'
    + '<button type="button" class="btn small" id="key-prev"' + (keysView.page <= 0 ? ' disabled' : '') + '>上一页</button>'
    + '<button type="button" class="btn small" id="key-next"'
    + (keysView.page + 1 >= pages ? ' disabled' : '') + '>下一页</button>';
  const prev = $('key-prev'), next = $('key-next');
  if (prev) prev.onclick = () => { keysView.page--; refreshKeys().catch(e => toast(e.message, 'err')); };
  if (next) next.onclick = () => { keysView.page++; refreshKeys().catch(e => toast(e.message, 'err')); };
}

// usdPick / tokPick 选出卡片余量块展示的那一档：优先总额，其次当前周期
// 尚未滚动的日/周/月（cycle key 不匹配即已跨期归零，读数按 0 计）。
// 用 null 判空而非真值判断：0 是「禁用」级真实限额，要照常渲染。
function usdPick(k) {
  const c = cycleKeysNow();
  if (k.quota_micro_usd !== null && k.quota_micro_usd !== undefined) return { lim: k.quota_micro_usd, used: k.spent_micro_usd };
  if (k.daily_micro_usd !== null && k.daily_micro_usd !== undefined) return { lim: k.daily_micro_usd, used: k.daily_cycle_key === c.daily ? k.daily_spent_micro_usd : 0 };
  if (k.weekly_micro_usd !== null && k.weekly_micro_usd !== undefined) return { lim: k.weekly_micro_usd, used: k.weekly_cycle_key === c.weekly ? k.weekly_spent_micro_usd : 0 };
  if (k.monthly_micro_usd !== null && k.monthly_micro_usd !== undefined) return { lim: k.monthly_micro_usd, used: k.monthly_cycle_key === c.monthly ? k.monthly_spent_micro_usd : 0 };
  return null;
}
function tokPick(k) {
  const c = cycleKeysNow();
  if (k.token_limit !== null && k.token_limit !== undefined) return { lim: k.token_limit, used: k.tokens_used };
  if (k.daily_token_limit !== null && k.daily_token_limit !== undefined) return { lim: k.daily_token_limit, used: k.daily_cycle_key === c.daily ? k.daily_tokens_used : 0 };
  if (k.weekly_token_limit !== null && k.weekly_token_limit !== undefined) return { lim: k.weekly_token_limit, used: k.weekly_cycle_key === c.weekly ? k.weekly_tokens_used : 0 };
  if (k.monthly_token_limit !== null && k.monthly_token_limit !== undefined) return { lim: k.monthly_token_limit, used: k.monthly_cycle_key === c.monthly ? k.monthly_tokens_used : 0 };
  return null;
}
// keyQuotaCell 卡片半格：标签、大字「余额 + 已用」一行，细进度条贴底
// 像一条边框带；两半的条同高对齐，读起来就是卡片下缘的一圈刻度。
function keyQuotaCell(q, kind, label, fmt) {
  const remain = Math.max(0, q.lim - q.used);
  const pct = q.lim > 0 ? Math.min(100, q.used / q.lim * 100) : 100;
  const state = q.lim <= 0 || pct >= 95 ? 'alarm' : pct >= 80 ? 'warn' : '';
  return '<div class="ky-cell" data-kind="' + kind + '" data-state="' + state + '"'
    + ' title="已用 ' + fmt(q.used) + ' / 上限 ' + fmt(q.lim) + '">'
    + '<span class="ky-cell-label">' + label + '</span>'
    + '<span class="ky-quota-row"><span class="ky-quota-num mono">余 ' + fmt(remain) + '</span>'
    + '<span class="ky-used mono">已用 ' + fmt(q.used) + '</span></span>'
    + '<span class="ky-bar"><span style="width:' + pct.toFixed(1) + '%"></span></span>'
    + '</div>';
}
const COPY_SVG = '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>';
function kidShort(kid) {
  return kid.length > 12 ? kid.slice(0, 6) + '…' + kid.slice(-4) : kid;
}
function keyCardHTML(k) {
  const meta = STATUS_META[keyStatus(k)];
  const u = usdPick(k), t = tokPick(k);
  const cells =
    (u ? keyQuotaCell(u, 'usd', '金额', fmtCur) : '')
    + (t ? keyQuotaCell(t, 'tok', 'Token', fmtTok) : '');
  // 无任何限额的 Key 没有进度条可画，但保持与限额卡片同构的余额区
  // （标签 + 大数字读数）：网格会把同行卡片拉伸到最高者的高度，只给一行
  // 小字会在卡片下半部留出一大块空白。
  const quotaArea = cells
    ? '<div class="ky-split' + (u && t ? '' : ' ky-single') + '">' + cells + '</div>'
    : '<div class="ky-split ky-single"><div class="ky-cell" data-kind="usd">'
      + '<span class="ky-cell-label">累计金额</span>'
      + '<span class="ky-quota-row"><span class="ky-quota-num mono">' + fmtCur(k.spent_micro_usd) + '</span>'
      + '<span class="ky-used mono">Token 已用 ' + fmtTok(k.tokens_used) + '</span></span>'
      + '</div></div>';
  return '<article class="ky-card" data-kid="' + esc(k.kid) + '" role="listitem" tabindex="0">'
    + '<div class="ky-card-top">'
    + '<span class="pill ' + meta.pill + '">' + meta.label + '</span>'
    + '<span class="ky-when">' + esc(rel(k.last_used_at)) + '</span></div>'
    + '<div class="ky-name-row">'
    + '<h3 class="ky-name">' + (k.label ? esc(k.label) : '<i>无标签</i>') + '</h3>'
    + '<button type="button" class="ky-kid mono" data-copy="' + esc(k.kid) + '" title="点击复制完整 kid：' + esc(k.kid) + '">'
    + '<span>' + esc(kidShort(k.kid)) + '</span>' + COPY_SVG + '</button>'
    + '</div>'
    + quotaArea
    + '<footer class="ky-meta">'
    + '<span class="ky-pair"><b>' + esc(k.caller_id || '-') + '</b>' + (k.caller_scope === 'key' ? '独立计额' : '归属 caller') + '</span>'
    + '<span>并发 ' + (k.max_concurrent_requests > 0 ? '≤ ' + k.max_concurrent_requests : '不限') + '</span>'
    + '</footer></article>';
}

// ---------- 详情 dialog ----------
// balanceSeq 守卫：快速连续打开时，晚返回的旧余额不得覆盖当前内容。
// balSkeletonRow 余额加载占位行：与 balRow 结构一致，数据到达原位替换不跳动。
function balSkeletonRow(name) {
  return '<div class="bal-row"><span class="bal-name">' + name + '</span>'
    + '<span class="bal-bar"><span class="skel"></span></span>'
    + '<span class="bal-val">—</span></div>';
}
function renderKeyDialog(k) {
  const kid = k.kid;
  const st = keyStatus(k);
  const meta = STATUS_META[st];
  // 只有配了 token 限额的 Key 才显示 token 那组配额，避免未用该功能的 Key
  // 详情里多出四行「不限」的空清单。骨架结构据此预先确定。
  const hasTok = [k.token_limit, k.daily_token_limit, k.weekly_token_limit, k.monthly_token_limit]
    .some(v => v !== null && v !== undefined);
  // 请求次数档独立于金额/Token 二选一，配了才显示。
  const hasReq = [k.daily_requests_limit, k.monthly_requests_limit]
    .some(v => v !== null && v !== undefined);
  const d = $('key-dialog');
  d.dataset.kid = kid;
  d.innerHTML =
    '<header class="kd-head">'
    + '<h3>' + (k.label ? esc(k.label) : '<i>无标签</i>') + '</h3>'
    + '<span class="pill ' + meta.pill + '">' + meta.label + '</span>'
    + '<button type="button" class="ky-kid mono" data-copy="' + esc(kid) + '" title="点击复制完整 kid：' + esc(kid) + '">'
    + '<span>' + esc(kidShort(kid)) + '</span>' + COPY_SVG + '</button>'
    + '<button type="button" class="kd-close" aria-label="关闭详情">'
    + '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>'
    + '</button></header>'
    + '<div class="kd-body">'
    + '<div class="detail-facts">'
    + fact('principal', k.principal || '-')
    + fact('额度口径', k.caller_scope === 'key' ? '独立计额' : '归属 caller')
    + fact('过期时间', k.expires_at ? fmtDT(k.expires_at) : '永不')
    + fact('创建于', fmtDT(k.created_at))
    + fact('周期计数', (k.daily_cycle_key || '-') + ' / ' + (k.weekly_cycle_key || '-') + ' / ' + (k.monthly_cycle_key || '-'))
    + fact('指纹', k.fingerprint || '-')
    + fact('可用模型', (k.allowed_models && k.allowed_models.length) ? k.allowed_models.join(', ') : '不限制')
    + fact('最近使用', rel(k.last_used_at))
    + '</div>'
    // 金额 | Token 对半两块；未配 token 时金额块独占整行
    + '<div class="kd-meters" id="kd-meters">'
    + '<section class="kd-quota-block"><div class="bal-title">金额额度（USD）</div>'
    + balSkeletonRow('总额度') + balSkeletonRow('今日') + balSkeletonRow('本周') + balSkeletonRow('本月')
    + '</section>'
    + (hasTok
      ? '<section class="kd-quota-block"><div class="bal-title">Token 限额</div>'
        + balSkeletonRow('总量') + balSkeletonRow('今日') + balSkeletonRow('本周') + balSkeletonRow('本月')
        + '</section>'
      : '')
    + (hasReq
      ? '<section class="kd-quota-block"><div class="bal-title">请求次数</div>'
        + balSkeletonRow('今日') + balSkeletonRow('本月')
        + '</section>'
      : '')
    + '</div>'
    + '<p class="note" id="kd-note">余额核算中…</p>'
    + '<div class="btn-row">'
    + '<button type="button" class="btn small primary" data-act="edit">编辑</button>'
    + '<button type="button" class="btn small" data-act="rotate">轮换</button>'
    + '<button type="button" class="btn small" data-act="reveal">查看明文</button>'
    + (st !== 'revoked' ? '<button type="button" class="btn small danger" data-act="revoke">撤销</button>' : '')
    + '<button type="button" class="btn small danger" data-act="delete">删除</button>'
    + '</div></div>';

  const wire = act => d.querySelector('[data-act="' + act + '"]');
  wire('edit').onclick = () => editKeySheet(k);
  wire('rotate').onclick = () => rotateSheet(kid);
  wire('reveal').onclick = () => revealSheet(kid);
  const rv = wire('revoke');
  if (rv) rv.onclick = () => confirmSheet('撤销密钥 ' + kid,
    '撤销不可逆，该 Key 将立即无法通过鉴权。历史用量保留。',
    () => post('/keys/revoke', { kid, actor: 'console' }).then(refreshKeys));
  wire('delete').onclick = () => confirmSheet('删除密钥 ' + kid,
    '永久删除该 Key（历史用量保留）。操作不可逆。',
    () => post('/keys/delete', { kid, actor: 'console' }).then(refreshKeys));
  d.querySelector('.kd-close').onclick = animateCloseKeyDialog;

  const seq = ++keysView.balanceSeq;
  api('/balance?key_id=' + encodeURIComponent(kid)).then(b => {
    if (seq !== keysView.balanceSeq || !d.open || d.dataset.kid !== kid) return;
    const wrap = $('kd-meters');
    if (!wrap) return;
    // 字段名必须与 service.Balance 的 JSON tag 一致。结构与骨架一致，原位替换无跳动。
    wrap.innerHTML =
      '<section class="kd-quota-block"><div class="bal-title">金额额度（USD）</div>'
      + balRow('总额度', k.quota_micro_usd, b.total_remaining_micro_usd, fmtCur)
      + balRow('今日', k.daily_micro_usd, b.daily_remaining_micro_usd, fmtCur)
      + balRow('本周', k.weekly_micro_usd, b.weekly_remaining_micro_usd, fmtCur)
      + balRow('本月', k.monthly_micro_usd, b.monthly_remaining_micro_usd, fmtCur)
      + '</section>'
      + (hasTok
        ? '<section class="kd-quota-block"><div class="bal-title">Token 限额</div>'
          + balRow('总量', k.token_limit, b.total_remaining_tokens, fmtTok)
          + balRow('今日', k.daily_token_limit, b.daily_remaining_tokens, fmtTok)
          + balRow('本周', k.weekly_token_limit, b.weekly_remaining_tokens, fmtTok)
          + balRow('本月', k.monthly_token_limit, b.monthly_remaining_tokens, fmtTok)
          + '</section>'
        : '')
      + (hasReq
        ? '<section class="kd-quota-block"><div class="bal-title">请求次数</div>'
          + balRow('今日', k.daily_requests_limit, b.daily_remaining_requests, fmtInt)
          + balRow('本月', k.monthly_requests_limit, b.monthly_remaining_requests, fmtInt)
          + '</section>'
        : '');
    const note = $('kd-note');
    const eta = keyEtaText(k);
    if (note) note.textContent = '在途预占 ' + fmtCur(b.held_micro_usd || 0)
      + (hasTok ? ' / ' + fmtTok(b.held_tokens) + ' token' : '')
      + ' · 当前周期 ' + cycleKeysNow().daily
      + (eta ? ' · 按今日消耗预计触顶：' + eta : '');
  }).catch(() => { /* 余额核算失败不打断详情 */ });
}

let kdOpener = null;   // 关闭 dialog 后焦点回到打开它的卡片
function openKeyDialog(kid, opener) {
  const k = keysView.cache.find(x => x.kid === kid);
  if (!k) return;
  if (opener) kdOpener = opener;
  renderKeyDialog(k);
  $('key-dialog').showModal();
}
// 关闭过渡：原生 close() 是瞬时消失，先播退出动画再真正关闭
function animateCloseKeyDialog() {
  const dlg = $('key-dialog');
  if (!dlg.open || dlg.classList.contains('closing')) return;
  dlg.classList.add('closing');
  setTimeout(() => { dlg.classList.remove('closing'); dlg.close(); }, 150);
}

$('key-rows').addEventListener('click', e => {
  const copy = e.target.closest('[data-copy]');
  if (copy) {
    copyText(copy.dataset.copy).then(() => toast('kid 已复制')).catch(() => toast('复制失败', 'err'));
    return;
  }
  const card = e.target.closest('.ky-card');
  if (card) openKeyDialog(card.dataset.kid, card);
});
$('key-rows').addEventListener('keydown', e => {
  if (e.key !== 'Enter' && e.key !== ' ') return;
  const card = e.target.closest('.ky-card');
  if (!card || e.target !== card) return;
  e.preventDefault();
  openKeyDialog(card.dataset.kid, card);
});
// dialog：背板点击关闭；内部 kid 复制行与网格同一套 data-copy 约定。
// Esc 经 cancel 事件接入同一套退出动画（preventDefault 后自行关闭）。
$('key-dialog').addEventListener('click', e => {
  if (e.target === e.currentTarget) { animateCloseKeyDialog(); return; }
  const copy = e.target.closest('[data-copy]');
  if (copy) copyText(copy.dataset.copy).then(() => toast('kid 已复制')).catch(() => toast('复制失败', 'err'));
});
$('key-dialog').addEventListener('cancel', e => {
  e.preventDefault();
  animateCloseKeyDialog();
});
$('key-dialog').addEventListener('close', () => {
  if (kdOpener && document.contains(kdOpener)) kdOpener.focus();
  kdOpener = null;
});

$('key-search').addEventListener('input', debounce(() => {
  keysView.search = $('key-search').value.trim();
  keysView.page = 0;
  refreshKeys().catch(e => toast(e.message, 'err'));
}, 350));
$('key-search').addEventListener('keydown', e => { if (e.key === 'Enter') e.preventDefault(); });
// 状态筛选 chips：与 STATUS_META 的文案保持一致
const KEY_STATUS_CHIPS = [
  ['', '全部'], ['active', '启用中'], ['disabled', '已禁用'], ['revoked', '已撤销'], ['expired', '已过期'],
];
function renderStatusChips() {
  $('key-status-chips').innerHTML = KEY_STATUS_CHIPS.map(([v, label]) =>
    '<button type="button" class="ky-chip' + (keysView.status === v ? ' on' : '') + '"'
    + ' data-v="' + v + '" aria-pressed="' + (keysView.status === v) + '">' + label + '</button>').join('');
}
$('key-status-chips').addEventListener('click', e => {
  const b = e.target.closest('button[data-v]');
  if (!b || b.dataset.v === keysView.status) return;
  keysView.status = b.dataset.v;
  keysView.page = 0;
  renderStatusChips();
  refreshKeys().catch(e2 => toast(e2.message, 'err'));
});
renderStatusChips();
const keyCallerSel = new Select('key-caller', [{ value: '', label: '全部 caller' }], v => {
  keysView.caller = v;
  keysView.page = 0;
  refreshKeys().catch(e => toast(e.message, 'err'));
}, { value: '', head: '按 caller 过滤' });
// callersCache 供签发弹窗重建 caller 下拉（原先克隆 select 的 innerHTML，改组件后必须走数据源）
let callersCache = [];
async function loadCallers() {
  try {
    const r = await api('/callers');
    callersCache = r.items || [];
    keyCallerSel.setOptions([{ value: '', label: '全部 caller' }].concat(
      callersCache.map(c => ({ value: c.id, label: (c.display_name || c.id) + (c.enabled ? '' : '（停用）') }))));
  } catch (e) { /* caller 下拉失败不阻塞 */ }
}
// callerOptionsHTML 给弹窗内的原生 <select> 用（弹窗表单保持原生控件）
function callerOptionsHTML() {
  return '<option value="">默认 caller</option>' + callersCache.map(c =>
    '<option value="' + esc(c.id) + '">' + esc(c.display_name || c.id)
    + (c.enabled ? '' : '（停用）') + '</option>').join('');
}

// 签发
$('key-issue-btn').addEventListener('click', () => {
  openSheet({
    title: '签发插件密钥',
    okText: '签发',
    body: '<div class="form-grid">'
      + fieldRow('标签', '<input id="f-label" placeholder="如：张三的测试 Key">')
      + fieldRow('principal', '<input id="f-principal" placeholder="可选，属主标识">')
      + fieldRow('caller', '<select id="f-caller">' + callerOptionsHTML() + '</select>')
      + fieldRow(labelWithTip('额度口径', TIPS.callerScope),
        '<select id="f-scope"><option value="caller">归属 caller 共享</option><option value="key">独立计额</option></select>')
      + fieldRow('过期时间', '<input id="f-expires" type="datetime-local">')
      + fieldRow('最大并发', '<input id="f-conc" type="number" min="0" placeholder="0 为不限">')
      + '<div class="form-sep wide kd-mode-row"><div class="seg" id="f-mode" role="group" aria-label="计费方式">'
      + '<button type="button" data-m="usd">按金额（USD）</button>'
      + '<button type="button" data-m="tok">按 Token</button></div>'
      + '<span class="note">二选一：一个 Key 只能用一种计费方式</span></div>'
      + '<div class="fg-sub" id="g-money">'
      + fieldRow(labelWithTip('总额度', TIPS.moneyLimit), '<input id="f-quota" inputmode="decimal" placeholder="留空为不限">')
      + fieldRow('日限额', '<input id="f-daily" inputmode="decimal" placeholder="留空为不限">')
      + fieldRow('周限额', '<input id="f-weekly" inputmode="decimal" placeholder="留空为不限">')
      + fieldRow('月限额', '<input id="f-monthly" inputmode="decimal" placeholder="留空为不限">')
      + '</div>'
      + '<div class="fg-sub off" id="g-tok">'
      + fieldRow(labelWithTip('总量', TIPS.tokenLimit), '<input id="f-tok" inputmode="numeric" placeholder="留空为不限，支持 500k / 1.5m">')
      + fieldRow('日限额', '<input id="f-tok-daily" inputmode="numeric" placeholder="留空为不限">')
      + fieldRow('周限额', '<input id="f-tok-weekly" inputmode="numeric" placeholder="留空为不限">')
      + fieldRow('月限额', '<input id="f-tok-monthly" inputmode="numeric" placeholder="留空为不限">')
      + '</div>'
      + '<div class="form-sep wide">请求次数限额（可选，独立于计费方式）</div>'
      + '<div class="fg-sub" id="g-req">'
      + fieldRow('日请求数', '<input id="f-req-daily" inputmode="numeric" placeholder="留空为不限">')
      + fieldRow('月请求数', '<input id="f-req-monthly" inputmode="numeric" placeholder="留空为不限">')
      + '</div>'
      + fieldRow('可用模型', '<textarea id="f-models" placeholder="逗号或换行分隔，支持 * 通配；留空不限制"></textarea>', 'wide')
      + '</div>',
    note: '明文只在签发结果里出现一次。计费方式金额/Token 二选一；限额留空为不限。',
    onOk: async () => {
      const mode = document.querySelector('#f-mode button.on').dataset.m;
      const num = id => {
        const v = $(id).value.trim();
        if (!v || v === '-1') return null;
        const n = parseFloat(v);
        if (!isFinite(n) || n < 0) throw new Error('金额限额须为不小于 0 的数字');
        return Math.round(n * 1e6);
      };
      // token 数支持 1000 / 1k / 1.5m / 2b 几种写法，避免手数零
      const tok = id => {
        const v = $(id).value.trim();
        if (!v || v === '-1') return null;
        const n = parseTokens(v);
        if (n < 0) throw new Error('Token 限额不接受负数');
        return n;
      };
      const reqN = id => {
        const v = $(id).value.trim();
        if (!v || v === '-1') return null;
        const n = parseInt(v, 10);
        if (!isFinite(n) || n < 0) throw new Error('请求次数限额须为不小于 0 的整数');
        return n;
      };
      const models = $('f-models').value.split(/[\n,，]/).map(s => s.trim()).filter(Boolean);
      const expires = $('f-expires').value ? new Date($('f-expires').value).toISOString() : null;
      const r = await post('/keys/issue', {
        label: $('f-label').value.trim(),
        principal: $('f-principal').value.trim(),
        caller_id: $('f-caller').value || 'default',
        caller_scope: $('f-scope').value,
        quota_micro_usd: mode === 'usd' ? num('f-quota') : null,
        daily_micro_usd: mode === 'usd' ? num('f-daily') : null,
        weekly_micro_usd: mode === 'usd' ? num('f-weekly') : null,
        monthly_micro_usd: mode === 'usd' ? num('f-monthly') : null,
        token_limit: mode === 'tok' ? tok('f-tok') : null,
        daily_token_limit: mode === 'tok' ? tok('f-tok-daily') : null,
        weekly_token_limit: mode === 'tok' ? tok('f-tok-weekly') : null,
        monthly_token_limit: mode === 'tok' ? tok('f-tok-monthly') : null,
        daily_requests_limit: reqN('f-req-daily'),
        monthly_requests_limit: reqN('f-req-monthly'),
        max_concurrent_requests: parseInt($('f-conc').value, 10) || 0,
        allowed_models: models,
        expires_at: expires,
        actor: 'console',
      });
      $('sheet-title').textContent = '密钥已签发 · ' + r.KID;
      $('sheet-body').innerHTML = secretBlock(r.Key)
        + '<p class="note">指纹 ' + esc(r.Fingerprint) + '</p>';
      $('sheet-note').textContent = '';
      wireSecretCopy();
      staySheet('完成');
      refreshKeys().catch(() => {});
      return false;
    },
  });
  // 计费方式切换（openSheet 同步建好 DOM，这里直接绑）
  const setMode = m => {
    $('g-money').classList.toggle('off', m !== 'usd');
    $('g-tok').classList.toggle('off', m !== 'tok');
    document.querySelectorAll('#f-mode button').forEach(b => b.classList.toggle('on', b.dataset.m === m));
  };
  $('f-mode').addEventListener('click', e => {
    const b = e.target.closest('button[data-m]');
    if (b) setMode(b.dataset.m);
  });
  setMode('usd');
});

// 编辑
function editKeySheet(k) {
  const MONEY_FIELDS = [
    ['e-quota', 'quota_micro_usd', '总额度'],
    ['e-daily', 'daily_micro_usd', '日限额'],
    ['e-weekly', 'weekly_micro_usd', '周限额'],
    ['e-monthly', 'monthly_micro_usd', '月限额'],
  ];
  const TOKEN_FIELDS = [
    ['e-tok', 'token_limit', '总量'],
    ['e-tok-daily', 'daily_token_limit', '日限额'],
    ['e-tok-weekly', 'weekly_token_limit', '周限额'],
    ['e-tok-monthly', 'monthly_token_limit', '月限额'],
  ];
  const REQ_FIELDS = [
    ['e-req-daily', 'daily_requests_limit', '日请求数'],
    ['e-req-monthly', 'monthly_requests_limit', '月请求数'],
  ];
  // 全量回填当前生效值：不限显示 -1，用户在现有基础上直接改。
  // 提交时所有字段原样发回（-1 由后端归一为不限），不再有「留空=不改」的隐式语义。
  const curMoney = f => k[f] === null || k[f] === undefined ? '-1' : String(k[f] / 1e6);
  const curTok = f => k[f] === null || k[f] === undefined ? '-1' : String(k[f]);
  openSheet({
    title: '编辑密钥 ' + k.kid,
    okText: '保存',
    body: '<div class="form-grid">'
      + fieldRow('标签', '<input id="e-label" value="' + esc(k.label || '') + '">')
      + fieldRow('启用', '<select id="e-enabled"><option value="true"' + (k.enabled ? ' selected' : '') + '>是</option>'
        + '<option value="false"' + (!k.enabled ? ' selected' : '') + '>否</option></select>')
      + fieldRow('过期时间', '<input id="e-expires" type="datetime-local" value="'
        + (k.expires_at ? toLocalInput(new Date(k.expires_at)) : '') + '">')
      + fieldRow('最大并发', '<input id="e-conc" type="number" min="0" value="' + (k.max_concurrent_requests || 0) + '">')
      + '<div class="form-sep wide kd-mode-row"><div class="seg" id="e-mode" role="group" aria-label="计费方式">'
      + '<button type="button" data-m="usd">按金额（USD）</button>'
      + '<button type="button" data-m="tok">按 Token</button></div>'
      + '<span class="note">二选一：切换后另一族限额会被清除</span></div>'
      + '<div class="fg-sub" id="g-money">'
      + MONEY_FIELDS.map(([id, field, label]) =>
        fieldRow(label, '<input id="' + id + '" inputmode="decimal" value="'
          + esc(curMoney(field)) + '">')).join('')
      + '</div>'
      + '<div class="fg-sub off" id="g-tok">'
      + TOKEN_FIELDS.map(([id, field, label]) =>
        fieldRow(label, '<input id="' + id + '" inputmode="numeric" value="'
          + esc(curTok(field)) + '">')).join('')
      + '</div>'
      + '<div class="form-sep wide">请求次数限额（独立于计费方式）</div>'
      + '<div class="fg-sub" id="g-req">'
      + REQ_FIELDS.map(([id, field, label]) =>
        fieldRow(label, '<input id="' + id + '" inputmode="numeric" value="'
          + esc(curTok(field)) + '">')).join('')
      + '</div>'
      + fieldRow('可用模型', '<textarea id="e-models" placeholder="留空表示不限制模型">'
        + esc((k.allowed_models || []).join(', ')) + '</textarea>', 'wide')
      + '</div>',
    note: '字段已按当前值回填，改完保存即可。计费方式金额/Token 二选一，切换后另一族清除。限额：正数=上限，0=禁用，-1=不限；Token 也接受 500k / 1.5m 写法。过期时间留空表示永不过期。',
    onOk: async () => {
      const mode = $('g-tok').classList.contains('off') ? 'usd' : 'tok';
      const body = { kid: k.kid, actor: 'console' };
      body.label = $('e-label').value.trim();
      body.enabled = $('e-enabled').value === 'true';
      const exp = $('e-expires').value;
      body.expires_at = exp ? new Date(exp).toISOString() : null;
      body.max_concurrent_requests = parseInt($('e-conc').value, 10) || 0;
      for (const [id, field] of TOKEN_FIELDS) {
        if (mode !== 'tok') { body[field] = -1; continue; }
        const raw = $(id).value.trim();
        if (raw === '' || raw === '-1') { body[field] = -1; continue; }
        const n = parseTokens(raw); // 非法写法直接抛错，由 sheet 捕获成提示
        if (n < 0) throw new Error('Token 限额不接受负数；-1 表示不限');
        body[field] = n;
      }
      for (const [id, field] of MONEY_FIELDS) {
        if (mode !== 'usd') { body[field] = -1; continue; }
        const raw = $(id).value.trim();
        if (raw === '' || raw === '-1') { body[field] = -1; continue; }
        const numv = parseFloat(raw);
        if (!isFinite(numv) || numv < 0) throw new Error('金额限额须为不小于 0 的数字（-1 表示不限）');
        body[field] = Math.round(numv * 1e6);
      }
      for (const [id, field] of REQ_FIELDS) {
        const raw = $(id).value.trim();
        if (raw === '' || raw === '-1') { body[field] = -1; continue; }
        const n = parseInt(raw, 10);
        if (!isFinite(n) || n < 0) throw new Error('请求次数限额须为不小于 0 的整数（-1 表示不限）');
        body[field] = n;
      }
      const modelsRaw = $('e-models').value.trim();
      body.allowed_models = modelsRaw
        ? modelsRaw.split(/[\n,，]/).map(s => s.trim()).filter(Boolean)
        : [];
      await post('/keys/update', body);
      toast('密钥已更新', 'ok');
      refreshKeys().catch(() => {});
    },
  });
  // 计费方式切换：默认沿用该 Key 现有口径（配了 token 即 tok），切换后另一族提交 -1 清除
  const setMode = m => {
    $('g-money').classList.toggle('off', m !== 'usd');
    $('g-tok').classList.toggle('off', m !== 'tok');
    document.querySelectorAll('#e-mode button').forEach(b => b.classList.toggle('on', b.dataset.m === m));
  };
  $('e-mode').addEventListener('click', e => {
    const b = e.target.closest('button[data-m]');
    if (b) setMode(b.dataset.m);
  });
  setMode(tokPick(k) ? 'tok' : 'usd');
}
function rotateSheet(kid) {
  openSheet({
    title: '轮换 ' + kid, danger: true, okText: '轮换',
    body: '<p>旧 Key 立即失效，并生成新明文（仅展示一次）。</p>',
    onOk: async () => {
      const r = await post('/keys/rotate', { kid, actor: 'console' });
      $('sheet-title').textContent = '已轮换 · ' + r.KID;
      $('sheet-body').innerHTML = secretBlock(r.Key);
      wireSecretCopy();
      staySheet('完成');
      refreshKeys().catch(() => {});
      return false;
    },
  });
}
function revealSheet(kid) {
  openSheet({
    title: '查看明文 ' + kid, okText: '解密',
    body: '<p>解密该 Key 的明文用于配置客户端。</p>',
    onOk: async () => {
      const r = await post('/keys/reveal', { kid, actor: 'console' });
      $('sheet-title').textContent = '明文 · ' + kid;
      $('sheet-body').innerHTML = secretBlock(r.key);
      wireSecretCopy();
      staySheet('关闭');
      return false;
    },
  });
}

