/* 管理页逻辑 */
(function () {
  'use strict';

  const tabs = $$('.tab');
  let channelsCache = [];

  tabs.forEach(function (t) {
    t.addEventListener('click', function () { showTab(t.dataset.tab); });
  });

  function showTab(name) {
    tabs.forEach(function (t) { t.classList.toggle('active', t.dataset.tab === name); });
    $$('.panel').forEach(function (p) { p.classList.toggle('active', p.id === 'tab-' + name); });
    if (name === 'overview') { loadOverview(); }
    if (name === 'users') { loadUsers(); }
    if (name === 'source') { loadSource(); }
    if (name === 'channels') { loadChannelsTab(); }
  }

  function stat(label, value, small) {
    return '<div class="stat"><div class="k">' + esc(label) + '</div><div class="v' +
      (small ? ' small' : '') + '">' + value + '</div></div>';
  }

  /* ---------- 运行状态 ---------- */

  async function loadOverview() {
    let d;
    try { d = await api('/admin/api/overview'); } catch (e) { toast(e.message, 'err'); return; }
    const ff = d.ffmpeg || {};
    const m3u = d.m3u || {};
    const ffv = ff.ok
      ? '<span class="badge ok">正常</span> ' + esc(ff.version || '')
      : '<span class="badge err">不可用</span> ' + esc(ff.error || '');
    $('#overviewCards').innerHTML =
      stat('服务版本', 'v' + esc(d.version)) +
      stat('运行时长', esc(d.uptime)) +
      stat('频道', d.channels_enabled + ' / ' + d.channels_total + ' <span class="muted">启用/全部</span>') +
      stat('用户数', d.users) +
      stat('正在转发', d.active_sessions + ' / ' + d.max_sessions) +
      stat('内存', (d.memory_mb || 0).toFixed(1) + ' MB') +
      stat('协程', d.goroutines) +
      stat('ffmpeg', ffv, true) +
      stat('ffmpeg 路径', esc(ff.path || 'ffmpeg'), true) +
      stat('播放列表来源', esc(m3u.source || '未导入'), true) +
      stat('播放列表更新于', esc(m3u.applied_at || '—'), true) +
      stat('数据库', esc(d.db_path || ''), true);

    const rows = (d.sessions || []).map(function (s) {
      const state = s.state === 'ready' ? '<span class="badge ok">播放中</span>'
        : (s.state === 'error' ? '<span class="badge err">失败</span>' : '<span class="badge warn">启动中</span>');
      return '<tr><td>#' + s.channel_id + ' ' + esc(s.name) + '</td><td>' + esc(s.kind) + '</td><td>' + state + '</td>' +
        '<td>' + rateCell(s) + '</td>' +
        '<td>' + s.viewers + '</td><td>' + fmtSec(s.uptime_sec) + '</td><td>' + fmtSec(s.idle_sec) + '</td>' +
        '<td>' + esc((s.error || '') + (s.log ? '\n' + s.log : '')) + '</td>' +
        '<td><button class="btn tiny danger" data-kill="' + s.channel_id + '">停止</button></td></tr>';
    }).join('');
    $('#sessionsTable').innerHTML = rows
      ? '<table class="tbl"><thead><tr><th>频道</th><th>类型</th><th>状态</th><th>源站投递</th><th>观看</th><th>已运行</th><th>空闲</th><th>错误/日志</th><th></th></tr></thead><tbody>' + rows + '</tbody></table>'
      : '<p class="muted">当前没有正在转发的频道。</p>';
  }

  // rateCell：源站投递速率。小于 1 说明源站没按实时给数据，
  // 播放端必然变慢或卡顿，属于源站侧限速/拥堵，面板补不出来。
  function rateCell(s) {
    const rate = (typeof s.rate === 'number') ? s.rate : -1;
    const fps = s.fps || 0;
    const peak = s.peak_fps || 0;
    const pct = (peak > 1 && fps > 0) ? Math.round(fps * 100 / peak) : 0;
    const tip = '窗口帧率 ' + fps.toFixed(1) + ' 帧/秒，本会话峰值 ' + peak.toFixed(1) + ' 帧/秒' +
      (pct ? '（约 ' + pct + '%）' : '') + '。低于 0.9x 说明源站在限速或拥堵';
    if (rate < 0) { return '<span class="muted">—</span>'; }
    let cls = 'ok';
    if (rate < 0.6) { cls = 'err'; } else if (rate < 0.9) { cls = 'warn'; }
    const note = (pct && pct < 95) ? '帧率 ' + pct + '%' : '';
    return '<span class="badge ' + cls + '" title="' + esc(tip) + '">' + rate.toFixed(2) + 'x</span>' +
      (note ? '<div class="muted rate-note">' + note + '</div>' : '');
  }

  function fmtSec(sec) {
    sec = sec || 0;
    if (sec < 60) { return sec + ' 秒'; }
    if (sec < 3600) { return Math.floor(sec / 60) + ' 分 ' + (sec % 60) + ' 秒'; }
    return Math.floor(sec / 3600) + ' 小时 ' + Math.floor((sec % 3600) / 60) + ' 分';
  }

  $('#refreshOverview').addEventListener('click', loadOverview);
  $('#sessionsTable').addEventListener('click', async function (e) {
    const b = e.target.closest('[data-kill]');
    if (!b) { return; }
    try {
      await postJSON('/admin/api/sessions/' + b.getAttribute('data-kill') + '/stop');
      toast('已停止转发', 'ok');
      loadOverview();
    } catch (err) { toast(err.message, 'err'); }
  });

  /* ---------- 用户管理 ---------- */

  async function loadUsers() {
    let d;
    try { d = await api('/admin/api/users'); } catch (e) { toast(e.message, 'err'); return; }
    const rows = (d.users || []).map(function (u) {
      const role = u.role === 'admin' ? '<span class="badge on">管理员</span>' : '<span class="badge">普通</span>';
      const st = u.enabled ? '<span class="badge ok">启用</span>' : '<span class="badge err">停用</span>';
      return '<tr>' +
        '<td>' + esc(u.username) + '</td>' +
        '<td>' + role + '</td>' +
        '<td>' + st + '</td>' +
        '<td>' + esc(u.last_login_at || '—') + '</td>' +
        '<td><span class="url">' + esc(u.stream_token || '—') + '</span></td>' +
        '<td><div class="acts">' +
        '<button class="btn tiny" data-pw="' + u.id + '" data-name="' + esc(u.username) + '">改密码</button>' +
        '<button class="btn tiny" data-token="' + u.id + '" data-name="' + esc(u.username) + '">重置令牌</button>' +
        '<button class="btn tiny" data-role="' + u.id + '" data-role-val="' + (u.role === 'admin' ? 'user' : 'admin') + '">' +
        (u.role === 'admin' ? '降为普通' : '设为管理员') + '</button>' +
        '<button class="btn tiny" data-toggle="' + u.id + '" data-enabled="' + (u.enabled ? '1' : '0') + '">' +
        (u.enabled ? '停用' : '启用') + '</button>' +
        '<button class="btn tiny danger" data-del="' + u.id + '" data-name="' + esc(u.username) + '">删除</button>' +
        '</div></td></tr>';
    }).join('');
    $('#usersTable').innerHTML = '<table class="tbl"><thead><tr><th>用户名</th><th>角色</th><th>状态</th><th>最近登录</th><th>播放令牌</th><th>操作</th></tr></thead><tbody>' + rows + '</tbody></table>';
  }

  $('#userForm').addEventListener('submit', async function (e) {
    e.preventDefault();
    const f = new FormData(e.target);
    try {
      await postJSON('/admin/api/users', {
        username: f.get('username'),
        password: f.get('password'),
        role: f.get('role')
      });
      toast('已创建用户', 'ok');
      e.target.reset();
      loadUsers();
    } catch (err) { toast(err.message, 'err'); }
  });

  $('#usersTable').addEventListener('click', async function (e) {
    const pw = e.target.closest('[data-pw]');
    if (pw) {
      const val = prompt('为「' + pw.getAttribute('data-name') + '」设置新密码（至少 6 位）：');
      if (!val) { return; }
      try {
        await postJSON('/admin/api/users/' + pw.getAttribute('data-pw') + '/password', { password: val });
        toast('密码已更新，该用户需重新登录', 'ok');
      } catch (err) { toast(err.message, 'err'); }
      return;
    }
    const tk = e.target.closest('[data-token]');
    if (tk) {
      if (!confirm('重置「' + tk.getAttribute('data-name') + '」的播放令牌？旧的播放地址会立即失效。')) { return; }
      try {
        const r = await postJSON('/admin/api/users/' + tk.getAttribute('data-token') + '/token');
        toast('新令牌：' + r.stream_token, 'ok');
        loadUsers();
      } catch (err) { toast(err.message, 'err'); }
      return;
    }
    const role = e.target.closest('[data-role]');
    if (role) {
      try {
        await postJSON('/admin/api/users/' + role.getAttribute('data-role') + '/role', { role: role.getAttribute('data-role-val') });
        toast('角色已更新', 'ok');
        loadUsers();
      } catch (err) { toast(err.message, 'err'); }
      return;
    }
    const tg = e.target.closest('[data-toggle]');
    if (tg) {
      const enabled = tg.getAttribute('data-enabled') === '1';
      try {
        await postJSON('/admin/api/users/' + tg.getAttribute('data-toggle') + '/toggle', { enabled: !enabled });
        toast(enabled ? '已停用' : '已启用', 'ok');
        loadUsers();
      } catch (err) { toast(err.message, 'err'); }
      return;
    }
    const del = e.target.closest('[data-del]');
    if (del) {
      if (!confirm('确定删除用户「' + del.getAttribute('data-name') + '」？')) { return; }
      try {
        await delJSON('/admin/api/users/' + del.getAttribute('data-del'));
        toast('已删除', 'ok');
        loadUsers();
      } catch (err) { toast(err.message, 'err'); }
    }
  });

  /* ---------- 直播源 ---------- */

  async function loadSource() {
    try {
      const d = await api('/admin/api/m3u');
      $('#m3uText').value = d.content || '';
      $('#m3uMeta').textContent = d.applied_at
        ? ('来源：' + (d.source || '—') + ' · 更新于 ' + d.applied_at)
        : '还没有导入播放列表';
    } catch (e) { toast(e.message, 'err'); }
  }

  function showResult(label, r) {
    const el = $('#m3uResult');
    el.className = 'alert ok';
    el.textContent = label + '：共 ' + r.total + ' 个频道，新增 ' + r.added + '，更新 ' + r.updated +
      '，删除 ' + r.removed + '，保持不变 ' + r.kept + '。';
    setTimeout(function () { el.classList.add('hidden'); }, 12000);
  }

  $('#m3uSave').addEventListener('click', async function () {
    const content = $('#m3uText').value;
    if (!content.trim()) { toast('请先粘贴播放列表内容', 'err'); return; }
    try {
      const r = await postJSON('/admin/api/m3u', { content: content, source: '面板编辑' });
      showResult('已应用', r.result);
      toast('播放列表已更新', 'ok');
      loadSource();
    } catch (e) { toast(e.message, 'err'); }
  });

  $('#m3uFetch').addEventListener('click', async function () {
    const url = $('#m3uUrlInput').value.trim();
    if (!url) { toast('请填写订阅地址', 'err'); return; }
    toast('正在下载…');
    try {
      const r = await postJSON('/admin/api/m3u/fetch', { url: url });
      showResult('已从地址导入', r.result);
      toast('导入成功', 'ok');
      loadSource();
    } catch (e) { toast(e.message, 'err'); }
  });

  $('#m3uFile').addEventListener('change', async function (e) {
    const file = e.target.files && e.target.files[0];
    if (!file) { return; }
    const fd = new FormData();
    fd.append('file', file);
    try {
      const r = await api('/admin/api/m3u/upload', { method: 'POST', body: fd });
      showResult('已上传并应用', r.result);
      toast('导入成功', 'ok');
      loadSource();
    } catch (err) { toast(err.message, 'err'); }
    e.target.value = '';
  });

  $('#m3uExport').addEventListener('click', function () {
    location.href = '/admin/api/export.m3u';
  });

  /* ---------- 频道列表 ---------- */

  async function fetchChannels() {
    const d = await api('/admin/api/channels');
    channelsCache = d.channels || [];
    renderChannels();
  }

  async function loadChannelsTab() {
    try {
      await fetchChannels();
      const st = await api('/admin/api/probe/status');
      if (st.running) { renderProbe(st); pollProbe(); } else if (st.total) { renderProbe(st); }
    } catch (e) { toast(e.message, 'err'); }
  }

  /* ---------- 一键探测 ---------- */

  let probeTimer = null;

  function renderProbe(st) {
    if (!st || (!st.running && !st.total)) { $('#probeProgress').classList.add('hidden'); return; }
    $('#probeProgress').classList.remove('hidden');
    const pct = st.total ? Math.round((st.done / st.total) * 100) : 0;
    $('#probeBarFill').style.width = pct + '%';
    let text;
    if (st.running) {
      text = '正在探测 ' + st.done + '/' + st.total + '（可用 ' + st.ok + '，失败 ' + st.failed + '）' +
        (st.current ? ' · 当前：' + st.current : '');
    } else {
      text = (st.cancelled ? '已取消：' : '探测完成：') + '共 ' + st.total + ' 个，可用 ' + st.ok + '，失败 ' + st.failed +
        (st.disabled ? '，已自动停用 ' + st.disabled + ' 个' : '') + '（' + (st.finished_at || '') + '）';
    }
    $('#probeText').textContent = text;
    const names = st.failed_names || [];
    $('#probeFailedList').textContent = (!st.running && names.length)
      ? ('不可用的频道：' + names.join('、') + (st.failed > names.length ? ' …共 ' + st.failed + ' 个' : ''))
      : '';
    $('#probeCancel').classList.toggle('hidden', !st.running);
    const btn = $('#probeAll');
    btn.disabled = !!st.running;
    btn.textContent = st.running ? '探测中…' : '一键探测全部';
  }

  function pollProbe() {
    clearInterval(probeTimer);
    let ticks = 0;
    probeTimer = setInterval(async function () {
      let st;
      try { st = await api('/admin/api/probe/status'); } catch (e) { clearInterval(probeTimer); return; }
      renderProbe(st);
      ticks++;
      // 边探边刷，结果实时出现在表格里
      if (st.running && ticks % 3 === 0) { try { await fetchChannels(); } catch (e) { } }
      if (!st.running) {
        clearInterval(probeTimer);
        try { await fetchChannels(); } catch (e) { }
      }
    }, 1200);
  }

  $('#probeAll').addEventListener('click', async function () {
    const withDisabled = $('#probeIncludeDisabled').checked;
    const targets = channelsCache.filter(function (c) { return withDisabled || !c.disabled; }).length;
    if (!confirm('开始逐个探测 ' + targets + ' 个频道' + (withDisabled ? '（包含已停用的，用于复查）' : '（只探启用中的）') + '？\n\n' +
      '⚠ 探测消耗的是你账号的额度，和机顶盒是同一套鉴权：\n' +
      '请求过密会触发 IPTV 平台限流 —— 实测被限流后整个账号（面板＋机顶盒）都无法播放，\n' +
      '提示 RateLimitedExceeded，要等 1 小时。\n\n' +
      '现在是串行 + 按「参数设置 → 探测间隔」，' + targets + ' 个台大约 ' + Math.max(2, Math.round(targets * 0.25)) + ' 分钟。\n' +
      '可以关掉页面去忙别的，请不要重复触发。确认开始？')) { return; }
    try {
      const st = await postJSON('/admin/api/probe/start', {
        auto_disable: $('#probeAutoDisable').checked,
        include_disabled: withDisabled
      });
      renderProbe(st);
      pollProbe();
      toast('已开始后台探测', 'ok');
    } catch (e) { toast(e.message, 'err'); }
  });

  $('#probeCancel').addEventListener('click', async function () {
    try {
      renderProbe(await postJSON('/admin/api/probe/cancel'));
      toast('已请求取消（正在跑的几条会先结束）', 'ok');
    } catch (e) { toast(e.message, 'err'); }
  });

  $('#disableFailed').addEventListener('click', async function () {
    if (!confirm('把所有「已被探测标记为失败」的频道一次性停用？之后可以逐个再启用。')) { return; }
    try {
      const r = await postJSON('/admin/api/channels/disable-failed');
      toast(r.disabled ? ('已停用 ' + r.disabled + ' 个频道') : '没有被标记为失败的频道', r.disabled ? 'ok' : '');
      loadChannelsTab();
    } catch (e) { toast(e.message, 'err'); }
  });

  function renderChannels() {
    const q = ($('#chanSearch').value || '').trim().toLowerCase();
    const list = channelsCache.filter(function (c) {
      if (!q) { return true; }
      return (c.name + ' ' + c.group + ' ' + c.url).toLowerCase().indexOf(q) >= 0;
    });
    const rows = list.map(function (c) {
      const st = c.disabled ? '<span class="badge err">停用</span>' : '<span class="badge ok">启用</span>';
      return '<tr><td class="nowrap">' + esc(c.name) + '</td><td class="nowrap">' + esc(c.group || '—') + '</td><td class="nowrap">' + esc(c.kind) + '</td>' +
        '<td><span class="url" title="' + esc(c.url) + '">' + esc(c.url) + '</span></td>' +
        '<td class="nowrap">' + st + '</td><td class="probe-cell">' + esc(c.probe || '—') + '</td>' +
        '<td><div class="acts">' +
        '<button class="btn tiny" data-probe="' + c.id + '">探测</button>' +
        '<button class="btn tiny" data-chan-toggle="' + c.id + '" data-disabled="' + (c.disabled ? '1' : '0') + '">' +
        (c.disabled ? '启用' : '停用') + '</button>' +
        '</div></td></tr>';
    }).join('');
    $('#channelsTable').innerHTML = rows
      ? '<table class="tbl"><thead><tr><th>名称</th><th>分组</th><th>类型</th><th>地址</th><th>状态</th><th>探测结果</th><th>操作</th></tr></thead><tbody>' + rows + '</tbody></table>'
      : '<p class="muted">没有频道，请先在「直播源」页导入播放列表。</p>';
  }

  $('#chanSearch').addEventListener('input', renderChannels);
  $('#refreshChannels').addEventListener('click', loadChannelsTab);

  $('#channelsTable').addEventListener('click', async function (e) {
    const probe = e.target.closest('[data-probe]');
    if (probe) {
      const btn = probe;
      btn.disabled = true;
      btn.textContent = '探测中…';
      try {
        const r = await postJSON('/admin/api/channels/' + probe.getAttribute('data-probe') + '/probe');
        toast(r.ok ? '探测成功：' + r.summary : '探测失败：' + r.error, r.ok ? 'ok' : 'err');
        loadChannelsTab();
      } catch (err) {
        toast(err.message, 'err');
        btn.disabled = false;
        btn.textContent = '探测';
      }
      return;
    }
    const tg = e.target.closest('[data-chan-toggle]');
    if (tg) {
      const disabled = tg.getAttribute('data-disabled') === '1';
      try {
        await postJSON('/admin/api/channels/' + tg.getAttribute('data-chan-toggle') + '/toggle', { disabled: !disabled });
        toast(disabled ? '已启用' : '已停用', 'ok');
        loadChannelsTab();
      } catch (err) { toast(err.message, 'err'); }
    }
  });

  /* ---------- 参数设置 ---------- */

  // 只有勾选了开关才显示依赖的字段（例如 Turnstile 密钥）
  function syncConditionals() {
    $$('#settingsForm [data-show-if]').forEach(function (el) {
      const box = $('input[type=checkbox][name="' + el.getAttribute('data-show-if') + '"]', $('#settingsForm'));
      el.style.display = (box && box.checked) ? 'flex' : 'none';
    });
  }

  $('#settingsForm').addEventListener('change', syncConditionals);
  syncConditionals();

  $('#settingsForm').addEventListener('submit', async function (e) {
    e.preventDefault();
    const form = e.target;
    const fd = new FormData(form);
    // 未勾选的复选框不会出现在 FormData 里，显式补 0，否则关不掉
    $$('input[type=checkbox][name]', form).forEach(function (box) {
      if (!box.checked) { fd.set(box.name, '0'); }
    });
    const payload = {};
    fd.forEach(function (v, k) { payload[k] = v; });
    try {
      await postJSON('/admin/api/settings', payload);
      toast('设置已保存', 'ok');
      syncConditionals();
    } catch (err) { toast(err.message, 'err'); }
  });

  // 默认打开运行状态
  loadOverview();
})();
