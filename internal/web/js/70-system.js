// ---------- 系统 ----------
loaders.system = async () => {
  const h = await api('/health');
  const s = h.stats || {};
  S.stats = s;
  $('sys-readouts').innerHTML =
    readout('数据库文件', fmtBytes(s.file_bytes),
      'schema v' + s.schema_version + ' · ' + (s.writable ? '可写' : '只读') + ' · WAL ' + fmtBytes(s.wal_bytes || 0),
      !s.writable)
    + readout('请求明细', fmtInt(s.requests), '逐请求记录')
    + readout('分钟聚合', fmtInt(s.rollups), '趋势与维度查询的数据源')
    + readout('在途预占', fmtInt(s.held_reservations), '未结算的额度预占', s.held_reservations > 0)
    + readout('密钥', fmtInt(s.keys), '含已撤销 / 过期')
    + readout('Caller', fmtInt(s.callers), '归属记录')
    + readout('计价规则', fmtInt(s.pricing_rules), 'manual + models.dev')
    + readout('存储重试', fmtInt(s.io_retries || 0),
      s.io_retries > 0 ? '瞬时 I/O 故障已自动重试；持续增长请把 data_dir 移出杀毒/同步盘' : '本次运行未出现瞬时 I/O 故障',
      s.io_retries > 0);
  $('db-note').textContent = s.writable
    ? '备份为单文件 SQLite 快照；恢复前服务端会做一致性检查。'
    : '当前实例处于只读模式（可能存在跨进程写者），备份可用，恢复不可用。';
  await loadNotify();
  await loadReports();
  updateBadges();
  stamp();
};
// ---------- 实时（进行中请求） ----------
// loadHeld 拉取并渲染「进行中请求」：在途额度预占的实时视图。
// 刷新按钮旁的 5s 自动刷新为该页专属（不走顶栏的全局自动刷新，那个间隔可到 86400s，
// 对实时视图太迟钝），页面隐藏或切走页签时暂停。
async function loadHeld() {
  const rows = $('held-rows'), note = $('held-note');
  let items;
  try {
    const r = await api('/reservations/held');
    items = r.items || [];
  } catch (e) {
    rows.innerHTML = '';
    $('held-count').textContent = '在途额度预占的实时视图：请求结算或心跳超时后被清扫后离开列表';
    note.textContent = '加载失败：' + e.message;
    return;
  }
  const staleN = items.filter(h => h.stale).length;
  $('held-count').textContent = items.length
    ? '共 ' + items.length + ' 条在途' + (staleN ? ' · ' + staleN + ' 条心跳超时' : '')
    : '在途额度预占的实时视图：请求结算或心跳超时后被清扫后离开列表';
  rows.innerHTML = items.map(h => {
    const label = keyLabelOf(h.key_id);
    let state;
    if (h.stale) state = '<span class="pill alarm">心跳超时</span>';
    else if (h.age_sec > 300) state = '<span class="pill warn">超过 5 分钟</span>';
    else state = '<span class="pill live">活跃</span>';
    return '<tr>'
      + '<td class="cell-mono cell-clip" title="' + esc(label ? label + ' · ' + h.key_id : h.key_id || '') + '">'
      + esc(label || h.key_id || '-') + '</td>'
      + '<td class="cell-mono cell-clip" title="' + esc(h.model || '') + '">' + esc(h.model || '-') + '</td>'
      + '<td class="num">' + fmtCur(h.held_micro_usd || 0) + '</td>'
      + '<td class="num">' + fmtTok(h.reserved_tokens || 0) + '</td>'
      + '<td class="num">' + fmtDur(h.age_sec) + '</td>'
      + '<td class="num" title="' + esc(h.heartbeat_at || '') + '">' + (h.heartbeat_at ? rel(h.heartbeat_at) : '-') + '</td>'
      + '<td>' + state + '</td></tr>';
  }).join('');
  note.textContent = items.length
    ? '心跳超时的行会在下一次维护动作或预占清扫时回收。'
    : '当前没有进行中的请求。';
}
// heldTimer 是本页 5s 轮询的定时器句柄；只有本页签可见且勾选自动刷新时才走。
let heldTimer = null;
function setupHeldAuto() {
  if (heldTimer) { clearInterval(heldTimer); heldTimer = null; }
  if (!$('held-auto').checked) return;
  heldTimer = setInterval(() => {
    if (document.hidden || $('app').hidden || activeTab !== 'live') return;
    loadHeld().catch(() => {});
    loadRecent().catch(() => {});
    loadAccuracy().catch(() => {});
    loadDensities().catch(() => {});
  }, 5000);
}
$('held-auto').addEventListener('change', () => {
  localStorage.setItem('held-auto', $('held-auto').checked ? '1' : '0');
  setupHeldAuto();
});
$('held-refresh').addEventListener('click', () => { loadHeld().catch(() => {}); });

// loadRecent 拉取并渲染「最近预占」：最近 10 条已完结请求的 预估 vs 实际 token 对照。
// released = 未走到结算即释放（上游错误/无响应/超时清扫），实际消耗无从谈起；
// 实际占比 = 实际消耗 ÷ 预估 token，70%–130% 视为估算健康区间（双向往返）。
async function loadRecent() {
  const rows = $('recent-rows'), note = $('recent-note'), sub = $('recent-sub');
  let items;
  try {
    const r = await api('/reservations/recent');
    items = r.items || [];
  } catch (e) {
    rows.innerHTML = '';
    note.textContent = '加载失败：' + e.message;
    return;
  }
  const settled = items.filter(x => x.status === 'settled');
  const released = items.length - settled.length;
  sub.textContent = items.length
    ? '最近 ' + items.length + ' 条已完结：' + settled.length + ' 条已结算 · ' + released + ' 条未结算即释放'
    : '最近 10 条已完结请求的预估与实际消耗 token 对照：实际占比落在 70%–130% 之间即估算健康';
  rows.innerHTML = items.map(x => {
    const label = keyLabelOf(x.key_id);
    let state, ratio;
    if (x.status === 'settled') {
      state = '<span class="pill">已结算</span>';
      if (x.reserved_tokens > 0 && x.settled_tokens > 0) {
        const pct = Math.round(x.settled_tokens / x.reserved_tokens * 100);
        // 健康区间 70%–130%：双向都算正常（预估略高或略低都无碍）；
        // 偏离区间才示警——偏低黄（预估虚高，长期占用额度）、偏高红
        // （预估不足，真实消耗超出预估、结算超扣）。
        const cls = pct >= 70 && pct <= 130 ? 'pill live' : pct > 130 ? 'pill alarm' : 'pill warn';
        ratio = '<span class="' + cls + '" title="实际 ' + fmtTok(x.settled_tokens) + ' / 预估 ' + fmtTok(x.reserved_tokens) + '">' + pct + '%</span>';
      } else {
        ratio = '<span class="pill" title="预估或实际 token 为 0（含 v15 之前的历史行）">—</span>';
      }
    } else {
      state = '<span class="pill warn" title="未走到结算即释放：上游错误、无响应或超时清扫，未产生扣费">已释放</span>';
      ratio = '<span class="pill" title="预占已全额退回，未扣费">退回</span>';
    }
    // Token 与金额各自合成一列（预估 → 实际），占比徽标并入 Token 列：
    // 原先 10 列排布把「预估/实际/占比」三个强关联读数拆到三列，视线要横跨
    // 整行才能对照，宽屏与窄屏都难读。
    const tok = '<span class="cell-pair">'
      + '<span class="pre">' + fmtTok(x.reserved_tokens || 0) + '</span>'
      + '<span class="arrow">→</span>'
      + '<span class="act">' + (x.status === 'settled' && x.settled_tokens > 0 ? fmtTok(x.settled_tokens) : '—') + '</span>'
      + '</span>';
    const cash = '<span class="cell-pair">'
      + '<span class="pre">' + (x.held_micro_usd > 0 ? fmtCur(x.held_micro_usd) : '—') + '</span>'
      + '<span class="arrow">→</span>'
      + '<span class="act">' + (x.status === 'settled' && x.settled_micro_usd > 0 ? fmtCur(x.settled_micro_usd) : '—') + '</span>'
      + '</span>';
    return '<tr>'
      + '<td class="cell-mono" title="' + esc(x.finished_at || '') + '">' + (x.finished_at ? rel(x.finished_at) : '-') + '</td>'
      + '<td class="cell-mono cell-clip" style="max-width:180px" title="' + esc(label ? label + ' · ' + x.key_id : x.key_id || '') + '">'
      + esc(label || x.key_id || '-') + '</td>'
      + '<td class="cell-mono cell-clip" style="max-width:220px" title="' + esc(x.model || '') + '">' + esc(x.model || '-') + '</td>'
      + '<td>' + state + '</td>'
      + '<td class="num">' + tok + ' ' + ratio + '</td>'
      + '<td class="num">' + cash + '</td>'
      + '<td class="num" title="' + esc(x.created_at || '') + ' 创建">' + (x.age_ms > 0 ? fmtDur(Math.round(x.age_ms / 1000)) : '-') + '</td>'
      + '</tr>';
  }).join('');
  note.textContent = items.length
    ? '实际占比 = 实际消耗 ÷ 预估 Token（Token 列的箭头右侧为实际值）。70%–130% 为健康区间；金额列对照预占与实扣，缓存拆档让两者贴近。'
    : '暂无已完结的预占记录（随保留期清理）。';
}
$('held-refresh').addEventListener('click', () => { loadRecent().catch(() => {}); });

// loadAccuracy 拉取并渲染「预占精度 · 按模型」：已结算预占的 实结 ÷ 预估
// 分位数聚合（P50 中位 / P95 长尾）。逐条「最近预占」只能看个案，系统性
// 虚占或低估要在分位数上才看得出来；健康带 70%–130% 与单条徽标同口径。
async function loadAccuracy() {
  const rows = $('accuracy-rows'), sub = $('acc-sub');
  let items;
  try {
    const r = await api('/reservations/accuracy');
    items = r.items || [];
  } catch (e) {
    rows.innerHTML = '';
    return;
  }
  const pctPill = (milli, label) => {
    if (!milli) return '<span class="pill" title="无有效样本">—</span>';
    const pct = Math.round(milli / 10);
    const cls = pct >= 70 && pct <= 130 ? 'pill live' : pct > 130 ? 'pill alarm' : 'pill warn';
    return '<span class="' + cls + '" title="' + label + '">' + pct + '%</span>';
  };
  rows.innerHTML = items.map(x => '<tr>'
    + '<td class="cell-mono cell-clip" style="max-width:260px" title="' + esc(x.model || '') + '">' + esc(x.model || '-') + '</td>'
    + '<td class="num">' + fmtInt(x.samples) + '</td>'
    + '<td class="num">' + pctPill(x.p50_ratio_milli, 'P50：半数请求的占比在此以下') + '</td>'
    + '<td class="num">' + pctPill(x.p95_ratio_milli, 'P95：95% 的请求的占比在此以下，长尾上界') + '</td>'
    + '</tr>').join('');
  sub.textContent = items.length
    ? '各模型实际占比分位数（实结 ÷ 预估）：P50 看中位表现，P95 看长尾上界'
    : '暂无已结算预占样本：跑过流量后此处给出各模型的估算精度聚合';
}

// loadDensities 拉取并渲染「估算密度」：各模型学习到的输入密度读数。
// 毫密度 ×1000 → 显示为 X.X 字节/token；口径列给出对照（固定混合密度
// 约为 ASCII 4 / 中文 3 字节每 token），让「学习密度 vs 直觉密度」可读。
// 缓存读/写占比是金额拆档口径：预占输入金额按此份额拆到三档计价。
async function loadDensities() {
  const rows = $('densities-rows'), sub = $('densities-sub');
  let items;
  try {
    const r = await api('/densities');
    items = r.items || [];
  } catch (e) {
    rows.innerHTML = '';
    return;
  }
  rows.innerHTML = items.map(x => {
    let cells;
    if (!x.samples && x.no_body_len) {
      // 盲区行：该模型有活跃流量但全走被动统计路径（不带 body_len），
      // 永远进不了密度样本——预占正按固定混合密度兜底，如实标出。
      cells = '<td class="num">—</td><td class="num">—</td><td class="num">0</td>'
        + '<td><span class="pill warn" title="近 7 天 ' + fmtInt(x.no_body_len)
        + ' 条请求走被动统计路径（无请求体长度），无法进入密度样本；预占按固定混合密度估算">盲区</span></td>';
    } else if (!x.samples) {
      // 重置后不足 3 条新样本：密度读数无意义，显示待学习态（不再触发
      // 漂移/状态列，避免把 0 当成真实读数）。
      cells = '<td class="num">—</td><td class="num">—</td><td class="num">0</td>'
        + '<td><span class="pill warn" title="学习基线已重置，等新流量跑够 3 条自动恢复读数">待学习</span></td>';
    } else {
      const d = (x.milli_density / 1000).toFixed(1);
      const mad = (x.milli_mad / 1000).toFixed(1);
      const rd = x.cache_read_bp > 0 ? (x.cache_read_bp / 100).toFixed(0) + '%' : '—';
      const wr = x.cache_create_bp > 0 ? (x.cache_create_bp / 100).toFixed(0) + '%' : '—';
      // 密度与离散度同列（读数 + ±MAD 小字），缓存读写同列：这两组各自
      // 是「一个读数 + 一个精度/结构说明」，拆成四列后每列都很空。
      cells = '<td class="num"><span class="cell-pair">'
        + '<span class="pre" title="请求体字节 ÷ 完整输入上下文 token 的中位数（最近成功请求）">' + d + ' B/token</span>'
        + '<span class="arrow">±</span><span class="act" title="绝对中位差：样本密度的离散程度，越小越稳定">' + mad + '</span>'
        + '</span></td>'
        + '<td class="num"><span class="cell-pair">'
        + '<span class="pre" title="缓存读占完整输入上下文的份额（token 加权），预占输入按此比例拆到缓存读档计价">读 ' + rd + '</span>'
        + '<span class="arrow">/</span><span class="act" title="缓存写占完整输入上下文的份额（token 加权），按缓存写档计价">写 ' + wr + '</span>'
        + '</span></td>'
        + '<td class="num">' + x.samples + '</td>'
        + '<td>' + (x.drifted
          ? '<span class="pill warn" title="近期请求体构成与历史明显不同（如 compact 后摘要替代原始代码），已改按近期密度折算">已漂移</span>'
          : '<span class="pill live" title="近期样本与历史密度的离散度在正常带内">学习中</span>') + '</td>';
    }
    // 重置过则标出基线时刻（悬浮可见），提示读数只统计基线之后的流量。
    const resetTip = x.reset_at
      ? ' · 学习基线已重置为 ' + fmtDT(x.reset_at, true) + '，只统计此后的流量' : '';
    return '<tr>'
      + '<td class="cell-mono cell-clip" style="max-width:260px" title="' + esc((x.model || '') + resetTip) + '">' + esc(x.model || '-') + '</td>'
      + cells
      + '<td class="w-act"><button type="button" class="btn small" data-den-reset="' + esc(x.model || '') + '">重置</button></td>'
      + '</tr>';
  }).join('');
  sub.textContent = items.length
    ? '各模型输入预占学习到的等效密度与缓存构成：密度决定 token 折算，缓存构成决定金额拆档'
    : '暂无密度样本：新流量跑过几条成功请求后自动学习（历史行不带请求体长度）';
}
// 重置某模型的学习基线：确认后把基线推到此刻，此后按当前请求体构成
// 重新学习（跑够 3 条新流量恢复读数）；请求记录与账本不受影响。
$('densities-rows').addEventListener('click', e => {
  const b = e.target.closest('button[data-den-reset]');
  if (!b) return;
  const model = b.dataset.denReset;
  openSheet({
    title: '重置估算密度', danger: true, okText: '确认重置',
    body: '<p>将把模型 <b class="mono">' + esc(model) + '</b> 的密度学习基线推到此刻。</p>'
      + '<p>此前的密度与缓存占比样本不再参与预占折算；此后按当前请求体构成重新学习，'
      + '跑够 3 条成功请求后恢复读数。请求记录与账本不受影响。</p>',
    note: '适用场景：agent 执行 /compact 之类的请求体构成突变后，旧样本已被污染，不等漂移探测逐步接管而一次性重新起算。',
    onOk: async () => {
      await post('/densities/reset', { model: model });
      toast('已重置「' + model + '」的学习基线，新流量将重新学习', 'ok');
      await loadDensities();
    },
  });
});
$('held-refresh').addEventListener('click', () => { loadDensities().catch(() => {}); });
loaders.live = async () => {
  // 密钥标签走全量候选（keysView.cache 只有当前分页页）。
  api('/keys/candidates')
    .then(r => { keyCandidates = r.items || []; })
    .catch(() => {});
  await loadHeld();
  await loadRecent();
  await loadAccuracy();
  await loadDensities();
  stamp();
};

// fmtDur 把秒数渲染为可读时长。
function fmtDur(sec) {
  sec = Math.max(0, +sec || 0);
  if (sec < 60) return sec + 's';
  if (sec < 3600) return Math.floor(sec / 60) + 'm' + (sec % 60 ? sec % 60 + 's' : '');
  return Math.floor(sec / 3600) + 'h' + Math.floor((sec % 3600) / 60) + 'm';
}

$('backup-btn').addEventListener('click', async () => {
  try {
    const r = await api('/backup');
    const blob = await r.blob();
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'cpa-usage-manager-backup.db';
    a.click();
    URL.revokeObjectURL(a.href);
    toast('备份已下载（' + fmtBytes(blob.size) + '）', 'ok');
  } catch (e) { toast(e.message, 'err'); }
});
$('restore-file').addEventListener('change', () => {
  const f = $('restore-file').files[0];
  $('restore-name').textContent = f ? f.name + '（' + fmtBytes(f.size) + '）' : '选择备份文件…';
});
$('restore-btn').addEventListener('click', () => {
  const f = $('restore-file').files[0];
  if (!f) { toast('请先选择备份文件', 'err'); return; }
  openSheet({
    title: '整库恢复', danger: true, okText: '确认恢复',
    body: '<p>将用 <b>' + esc(f.name) + '</b> 替换当前数据库的全部内容。'
      + '若备份来自其他机器，必须同时迁移 <span class="mono">data_dir/key-peppers</span>，否则密文无法解密。</p>',
    onOk: async () => {
      const res = await fetch(API + '/restore?actor=console', {
        method: 'POST',
        headers: { Authorization: 'Bearer ' + key, 'X-Confirm-Restore': 'replace' },
        body: f,
      });
      if (!res.ok) {
        let msg = 'HTTP ' + res.status;
        try { const j = await res.json(); if (j.error) msg = j.error; } catch (_) { /* 忽略 */ }
        throw new Error(msg);
      }
      const j = await res.json();
      const rows = j.tables ? Object.values(j.tables).reduce((a, b) => a + b, 0) : 0;
      // 恢复后服务端用当前 pepper 集做过可解密性自检：有问题必须在确认
      // 弹窗里让用户当场看到，不能只给一条绿色成功 toast。
      if (j.pepper_warning) {
        openSheet({
          title: '恢复完成，但密钥解密异常', danger: true, okText: '知道了',
          body: '<p>' + esc(j.pepper_warning) + '</p>'
            + '<p class="note">不可解密密钥数：' + fmtInt(j.undecryptable_keys || 0)
            + '。恢复其他机器的备份时，需要把其 <span class="mono">data_dir/key-peppers</span> 一并复制过来。</p>',
        });
      } else {
        toast('恢复完成：' + fmtBytes(j.bytes || f.size) + ' · ' + rows + ' 行', 'ok');
      }
      loaders.system().catch(() => {});
    },
  });
});
function maintainRun(vacuum) {
  return async () => {
    try {
      const r = await post('/maintain', { vacuum, actor: 'console' });
      const g = k => r[k] !== undefined ? r[k] : r[k.charAt(0).toUpperCase() + k.slice(1)];
      $('maintain-note').textContent = '上次结果：清理请求 ' + fmtInt(g('requests'))
        + ' · 聚合 ' + fmtInt(g('rollups')) + ' · 预占 ' + fmtInt(g('reservations'))
        + ' · 合并重复 ' + fmtInt(g('deduped'))
        + (vacuum ? ' · 已 VACUUM' : '');
      toast(vacuum ? '清理并 VACUUM 完成' : '清理完成', 'ok');
      loaders.system().catch(() => {});
    } catch (e) { toast(e.message, 'err'); }
  };
}
$('maintain-btn').addEventListener('click', maintainRun(false));
$('vacuum-btn').addEventListener('click', maintainRun(true));
$('dedupe-btn').addEventListener('click', async () => {
  const btn = $('dedupe-btn');
  btn.disabled = true;
  try {
    const r = await post('/dedupe', { actor: 'console' });
    const n = r.merged || 0;
    $('maintain-note').textContent = n > 0
      ? '上次对账：合并了 ' + fmtInt(n) + ' 条重复请求行，费用口径不变（保留执行器行的结算金额）。'
      : '上次对账：未发现重复请求行。';
    toast(n > 0 ? '对账完成：合并 ' + fmtInt(n) + ' 条' : '对账完成：无重复', 'ok');
    loaders.system().catch(() => {});
  } catch (e) { toast(e.message, 'err'); } finally { btn.disabled = false; }
});
$('reset-confirm').addEventListener('input', () => {
  $('reset-btn').disabled = $('reset-confirm').value !== 'reset';
});
$('reset-btn').addEventListener('click', () => {
  openSheet({
    title: '重置统计', danger: true, okText: '确认重置',
    body: '<p>将清空逐请求明细、分钟聚合、已终结预占与密钥周期计数器。'
      + '<b>密钥与计价规则保留</b>。</p>',
    onOk: async () => {
      const r = await post('/reset', { confirm: 'reset', actor: 'console' });
      toast('重置完成：请求 ' + fmtInt(r.requests) + ' · 聚合 ' + fmtInt(r.rollups), 'ok');
      $('reset-confirm').value = '';
      $('reset-btn').disabled = true;
      loaders.system().catch(() => {});
    },
  });
});

// ---------- 通知（shoutrrr 多端点） ----------
let notifyCache = { settings: null, endpoints: [] };
async function loadNotify() {
  const r = await api('/notify');
  notifyCache = { settings: r.settings || null, endpoints: r.endpoints || [] };
  renderNotify();
}
function renderNotify() {
  const st = notifyCache.settings || {};
  $('nt-enabled').checked = !!st.enabled;
  $('nt-errors').checked = !!st.error_alerts;
  $('nt-warn').value = st.warn_pct ?? 20;
  $('nt-single').checked = !!st.single_cost_alert;
  $('nt-single-usd').value = st.single_cost_micro_usd > 0 ? (st.single_cost_micro_usd / 1e6) : '';
  $('nt-single-tok').value = st.single_token_threshold > 0 ? st.single_token_threshold : '';
  $('nt-errrate').checked = !!st.error_rate_alert;
  $('nt-errrate-win').value = st.error_rate_window_min ?? 10;
  $('nt-errrate-pct').value = st.error_rate_pct ?? 50;
  $('nt-expire-days').value = st.expire_warn_days > 0 ? st.expire_warn_days : '';
  const eps = notifyCache.endpoints;
  if (!eps.length) {
    $('nt-list').innerHTML = '<p class="note">尚未配置通知端点，点右上角「新增端点」开始。</p>';
    return;
  }
  $('nt-list').innerHTML = eps.map(e => {
    const scheme = (String(e.url).split('://')[0] || '?').toLowerCase();
    let status;
    if (e.last_error) status = '<span class="nt-status err" title="' + esc(e.last_error) + '">✗ 发送失败</span>';
    else if (e.last_ok_at) status = '<span class="nt-status ok" title="上次成功 ' + esc(fmtDT(e.last_ok_at)) + '">✓ 正常</span>';
    else status = '<span class="nt-status dim">从未发送</span>';
    return '<div class="nt-row' + (e.enabled ? '' : ' off') + '">'
      + '<span class="nt-scheme">' + esc(scheme) + '</span>'
      + '<div class="nt-main">'
      + '<div class="nt-line1"><b class="nt-name">' + esc(e.label || '未命名端点') + '</b>'
      + (e.enabled ? '' : '<span class="pill">停用</span>')
      + status + '</div>'
      + '<div class="nt-line2">'
      + '<span class="nt-url mono" title="' + esc(e.url) + '">' + esc(e.url) + '</span>'
      + '<span class="nt-ops">'
      + '<button type="button" class="btn" data-nt-test="' + e.id + '">测试</button>'
      + '<button type="button" class="btn" data-nt-edit="' + e.id + '">编辑</button>'
      + '<button type="button" class="btn danger" data-nt-del="' + e.id + '">删除</button>'
      + '</span></div></div></div>';
  }).join('');
}
function openEndpointSheet(ep) {
  openSheet({
    title: ep ? '编辑通知端点' : '新增通知端点',
    okText: ep ? '保存' : '添加',
    body:
      fieldRow('标签', '<input id="f-nt-label" placeholder="如：飞书值班群" value="' + esc(ep ? ep.label : '') + '">')
      + fieldRow('shoutrrr URL',
        '<textarea id="f-nt-url" rows="3" spellcheck="false" autocomplete="off" '
        + 'placeholder="telegram://… / discord://… / lark://… / generic://…"'
        + '>' + esc(ep ? ep.url : '') + '</textarea>')
      + '<label class="check-row"><input type="checkbox" id="f-nt-enabled"'
      + (!ep || ep.enabled ? ' checked' : '') + '> 启用该端点</label>',
    note: 'URL 里通常带 bot token / webhook secret，仅存本机数据库并加密；完整服务列表见 shoutrrr 文档。',
    onOk: async () => {
      await post('/notify/endpoint/save', {
        id: ep ? ep.id : 0,
        label: $('f-nt-label').value.trim(),
        url: $('f-nt-url').value.trim(),
        enabled: $('f-nt-enabled').checked,
        actor: 'console',
      });
      toast(ep ? '端点已更新' : '端点已添加', 'ok');
      await loadNotify();
    },
  });
}
$('nt-add-btn').addEventListener('click', () => openEndpointSheet(null));
$('nt-save-btn').addEventListener('click', async () => {
  const warn = Math.round(Number($('nt-warn').value));
  const su = Number($('nt-single-usd').value);
  const stk = Number($('nt-single-tok').value);
  const erw = Math.round(Number($('nt-errrate-win').value));
  const erp = Math.round(Number($('nt-errrate-pct').value));
  const ewd = Math.round(Number($('nt-expire-days').value));
  try {
    const r = await post('/notify/settings', {
      enabled: $('nt-enabled').checked,
      error_alerts: $('nt-errors').checked,
      warn_pct: Number.isFinite(warn) && warn > 0 ? warn : 20,
      single_cost_alert: $('nt-single').checked,
      single_cost_micro_usd: Number.isFinite(su) && su > 0 ? Math.round(su * 1e6) : 0,
      single_token_threshold: Number.isFinite(stk) && stk > 0 ? Math.round(stk) : 0,
      error_rate_alert: $('nt-errrate').checked,
      error_rate_window_min: Number.isFinite(erw) && erw > 0 ? erw : 10,
      error_rate_pct: Number.isFinite(erp) && erp > 0 ? erp : 50,
      expire_warn_days: Number.isFinite(ewd) && ewd > 0 ? ewd : 0,
      actor: 'console',
    });
    notifyCache.settings = r;
    toast('通知设置已保存', 'ok');
  } catch (e) { toast(e.message, 'err'); }
});
$('nt-list').addEventListener('click', ev => {
  const t = ev.target.closest('button[data-nt-test],button[data-nt-edit],button[data-nt-del]');
  if (!t) return;
  const id = Number(t.dataset.ntTest || t.dataset.ntEdit || t.dataset.ntDel);
  const ep = (notifyCache.endpoints || []).find(x => x.id === id);
  if (!ep) return;
  if (t.dataset.ntTest !== undefined) {
    (async () => {
      try {
        await post('/notify/endpoint/test', { id, actor: 'console' });
        toast('测试消息已发送，请到对应渠道查收', 'ok');
        renderNotify();
      } catch (e) { toast(e.message, 'err'); renderNotify(); }
    })();
  } else if (t.dataset.ntEdit !== undefined) {
    openEndpointSheet(ep);
  } else {
    openSheet({
      title: '删除通知端点', danger: true, okText: '删除',
      body: '<p>删除端点「<b>' + esc(ep.label || ep.url) + '</b>」？该操作不可撤销。</p>',
      onOk: async () => {
        await post('/notify/endpoint/delete', { id, actor: 'console' });
        toast('端点已删除', 'ok');
        await loadNotify();
      },
    });
  }
});

// ---------- 定期报告（日/周/月报） ----------
let reportsCache = [];
async function loadReports() {
  const r = await api('/reports');
  reportsCache = r.items || [];
  renderReports();
}
function renderReports() {
  const el = $('rp-list');
  if (!reportsCache.length) {
    el.innerHTML = '<p class="note">尚未配置定期报告。</p>';
    return;
  }
  const freqName = { daily: '日报', weekly: '周报', monthly: '月报' };
  const epName = id => {
    const e = (notifyCache.endpoints || []).find(x => x.id === id);
    return e ? (e.label || e.url) : '#' + id;
  };
  el.innerHTML = reportsCache.map(c => {
    let sched = '每天 ' + c.time_of_day;
    if (c.frequency === 'weekly') sched = '每周' + '一二三四五六日'[c.weekday - 1] + ' ' + c.time_of_day;
    if (c.frequency === 'monthly') sched = '每月 ' + c.monthday + ' 日 ' + c.time_of_day;
    const tz = c.tz_offset_min ? ' · UTC' + (c.tz_offset_min > 0 ? '+' : '') + Math.round(c.tz_offset_min / 60 * 10) / 10 : ' · UTC';
    const eps = (c.endpoint_ids || []).map(epName).join('、') || '无端点';
    let status;
    if (c.last_error) status = '<span class="nt-status err" title="' + esc(c.last_error) + '">✗ 发送失败</span>';
    else if (c.last_sent_at) status = '<span class="nt-status ok" title="上次发送 ' + esc(fmtDT(c.last_sent_at)) + '">✓ 正常</span>';
    else status = '<span class="nt-status dim">从未发送</span>';
    const freqClass = { daily: 'daily', weekly: 'weekly', monthly: 'monthly' }[c.frequency] || '';
    return '<div class="nt-row' + (c.enabled ? '' : ' off') + '">'
      + '<span class="nt-freq ' + freqClass + '">' + esc(freqName[c.frequency] || c.frequency) + '</span>'
      + '<div class="nt-main">'
      + '<div class="nt-line1"><b class="nt-name">' + esc(c.name || '未命名报告') + '</b>'
      + (c.enabled ? '' : '<span class="pill">停用</span>')
      + status + '</div>'
      + '<div class="nt-line2">'
      + '<span class="nt-desc">' + esc(sched + tz + ' · 发往 ' + eps) + '</span>'
      + '<span class="nt-ops">'
      + '<button type="button" class="btn" data-rp-test="' + c.id + '">测试</button>'
      + '<button type="button" class="btn" data-rp-edit="' + c.id + '">编辑</button>'
      + '<button type="button" class="btn danger" data-rp-del="' + c.id + '">删除</button>'
      + '</span></div></div></div>';
  }).join('');
}
const RP_METRICS = [['cost', '费用'], ['tokens', 'Token'], ['requests', '请求数']];
function rpMetricSel(id, cur) {
  return '<select id="' + id + '" class="rp-select">'
    + RP_METRICS.map(m => '<option value="' + m[0] + '"' + (cur === m[0] ? ' selected' : '') + '>' + m[1] + '</option>').join('')
    + '</select>';
}
function openReportSheet(c) {
  const eps = notifyCache.endpoints || [];
  if (!eps.length) { toast('请先在「通知」面板配置至少一个端点', 'err'); return; }
  const s = (c && c.sections) || {};
  const bm = s.by_model || { on: !c, top: 5, metric: 'cost' };
  const bk = s.by_key || { on: false, top: 5, metric: 'cost' };
  const bc = s.by_caller || { on: false, top: 5, metric: 'cost' };
  const ids = (c && c.endpoint_ids) || [];
  const epChecks = eps.map(e =>
    '<label class="check-row"><input type="checkbox" class="rp-ep" value="' + e.id + '"'
    + (ids.includes(e.id) ? ' checked' : '') + '> ' + esc(e.label || e.url) + '</label>').join('');
  const topBlock = (key, label, t) =>
    '<div class="rp-top-row">'
    + '<label class="check-row"><input type="checkbox" id="rp-' + key + '-on"' + (t.on ? ' checked' : '') + '> ' + label + ' Top</label>'
    + '<input type="number" id="rp-' + key + '-top" min="1" max="20" value="' + (t.top || 5) + '">'
    + rpMetricSel('rp-' + key + '-metric', t.metric || 'cost')
    + '</div>';
  openSheet({
    title: c ? '编辑报告 · ' + (c.name || '') : '新增定期报告',
    okText: c ? '保存' : '添加',
    body:
      fieldRow('名称', '<input id="rp-name" placeholder="如：每日用量日报" value="' + esc(c ? c.name : '') + '">')
      + '<div class="form-grid">'
      + fieldRow('频率', '<select id="rp-freq" class="rp-select">'
        + [['daily', '日报'], ['weekly', '周报'], ['monthly', '月报']].map(f =>
          '<option value="' + f[0] + '"' + ((c ? c.frequency : 'daily') === f[0] ? ' selected' : '') + '>' + f[1] + '</option>').join('') + '</select>')
      + fieldRow('发送时刻', '<input id="rp-time" type="time" value="' + esc(c ? c.time_of_day : '09:00') + '">')
      + '<span id="rp-weekday-row">' + fieldRow('每周几（周报）', '<select id="rp-weekday" class="rp-select">'
        + ['周一', '周二', '周三', '周四', '周五', '周六', '周日'].map((d, i) =>
          '<option value="' + (i + 1) + '"' + ((c ? c.weekday : 1) === i + 1 ? ' selected' : '') + '>' + d + '</option>').join('') + '</select>')
      + '</span>'
      + '<span id="rp-monthday-row">' + fieldRow('每月几号（月报）', '<input type="number" id="rp-monthday" min="1" max="28" value="' + (c ? c.monthday : 1) + '">') + '</span>'
      + fieldRow('时区偏移（分钟，北京 +480）', '<input type="number" id="rp-tz" min="-840" max="840" step="15" value="' + (c ? c.tz_offset_min : 0) + '">')
      + '</div>'
      + '<div class="form-sep">内容板块</div>'
      + '<div class="rp-secs">'
      + '<label class="check-row"><input type="checkbox" id="rp-summary"' + (s.summary || !c ? ' checked' : '') + '> 汇总行（请求 / 费用 / Token / 成功率 / 缓存命中）</label>'
      + '<label class="check-row"><input type="checkbox" id="rp-failures"' + (s.failures ? ' checked' : '') + '> 失败请求明细</label>'
      + topBlock('by_model', '模型', bm)
      + topBlock('by_key', '密钥', bk)
      + topBlock('by_caller', '归属', bc)
      + '</div>'
      + '<div class="form-sep">发送端点</div>'
      + '<div class="rp-eps">' + epChecks + '</div>'
      + '<label class="check-row"><input type="checkbox" id="rp-enabled"' + (!c || c.enabled ? ' checked' : '') + '> 启用该报告</label>',
    note: '报告覆盖上一个已完成周期；测试按钮按同一周期立即生成发送，不影响计划。',
    onOk: async () => {
      const endpointIDs = [...document.querySelectorAll('.rp-ep:checked')].map(x => Number(x.value));
      if (!endpointIDs.length) throw new Error('至少选择一个发送端点');
      await post('/reports/save', {
        id: c ? c.id : 0,
        name: $('rp-name').value.trim(),
        enabled: $('rp-enabled').checked,
        frequency: $('rp-freq').value,
        time_of_day: $('rp-time').value || '09:00',
        weekday: Number($('rp-weekday').value),
        monthday: Number($('rp-monthday').value),
        tz_offset_min: Number($('rp-tz').value) || 0,
        sections: {
          summary: $('rp-summary').checked,
          failures: $('rp-failures').checked,
          by_model: { on: $('rp-by_model-on').checked, top: Number($('rp-by_model-top').value) || 5, metric: $('rp-by_model-metric').value },
          by_key: { on: $('rp-by_key-on').checked, top: Number($('rp-by_key-top').value) || 5, metric: $('rp-by_key-metric').value },
          by_caller: { on: $('rp-by_caller-on').checked, top: Number($('rp-by_caller-top').value) || 5, metric: $('rp-by_caller-metric').value },
        },
        endpoint_ids: endpointIDs,
        actor: 'console',
      });
      toast(c ? '报告已更新' : '报告已添加', 'ok');
      await loadReports();
    },
  });
  const freqSel = $('rp-freq');
  const syncFreq = () => {
    $('rp-weekday-row').hidden = freqSel.value !== 'weekly';
    $('rp-monthday-row').hidden = freqSel.value !== 'monthly';
  };
  freqSel.addEventListener('change', syncFreq);
  syncFreq();
}
$('rp-add-btn').addEventListener('click', () => openReportSheet(null));
$('rp-list').addEventListener('click', ev => {
  const t = ev.target.closest('button[data-rp-test],button[data-rp-edit],button[data-rp-del]');
  if (!t) return;
  const id = Number(t.dataset.rpTest || t.dataset.rpEdit || t.dataset.rpDel);
  const cfg = (reportsCache || []).find(x => x.id === id);
  if (!cfg) return;
  if (t.dataset.rpTest !== undefined) {
    (async () => {
      try {
        await post('/reports/test', { id, actor: 'console' });
        toast('测试报告已发送，请到对应渠道查收', 'ok');
        renderReports();
      } catch (e) { toast(e.message, 'err'); renderReports(); }
    })();
  } else if (t.dataset.rpEdit !== undefined) {
    openReportSheet(cfg);
  } else {
    openSheet({
      title: '删除定期报告', danger: true, okText: '删除',
      body: '<p>删除报告「<b>' + esc(cfg.name || cfg.frequency) + '</b>」？该操作不可撤销。</p>',
      onOk: async () => {
        await post('/reports/delete', { id, actor: 'console' });
        toast('报告已删除', 'ok');
        await loadReports();
      },
    });
  }
});

