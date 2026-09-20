/* 播放页逻辑：频道列表 + HLS 播放 + 转发会话管理 */
(function () {
  'use strict';

  const video = $('#video');
  const overlay = $('#overlay');
  const overlayText = $('#overlayText');
  const overlayHint = $('#overlayHint');
  const spinner = $('#spinner');
  const retryBtn = $('#retryBtn');
  const channelList = $('#channelList');
  const statusEl = $('#npMeta');
  const srcWarn = $('#srcWarn');

  const state = {
    channels: [],
    favs: new Set(),
    visible: [],
    current: null,
    hls: null,
    mode: null,
    query: '',
    collapsed: new Set(),
    retry: 0,
    statusTimer: null,
    waitTimer: null
  };

  /* ---------- 频道列表渲染 ---------- */

  function itemHTML(c) {
    const fav = state.favs.has(c.id);
    const playing = state.current && state.current.id === c.id;
    return '<div class="chan-item' + (playing ? ' playing' : '') + '" data-play="' + c.id + '" title="' + esc(c.name) + '">' +
      '<span class="chan-name">' + esc(c.name) + '</span>' +
      (c.kind ? '<span class="chan-kind">' + esc(c.kind) + '</span>' : '') +
      '<button class="fav' + (fav ? ' on' : '') + '" data-fav="' + c.id + '" title="收藏">' + (fav ? '★' : '☆') + '</button>' +
      '</div>';
  }

  function groupHTML(name, items, key) {
    const collapsed = state.collapsed.has(key);
    return '<div class="group' + (collapsed ? ' collapsed' : '') + '" data-group="' + esc(key) + '">' +
      '<button class="group-head" data-toggle="' + esc(key) + '"><span class="arrow">▾</span>' +
      '<span>' + esc(name) + '</span><span class="count">' + items.length + '</span></button>' +
      '<div class="group-body">' + items.map(itemHTML).join('') + '</div></div>';
  }

  function render() {
    const q = state.query.trim().toLowerCase();
    const list = state.channels.filter(function (c) {
      if (!q) { return true; }
      return (c.name + ' ' + c.group + ' ' + c.kind).toLowerCase().indexOf(q) >= 0;
    });
    state.visible = list;

    const favs = list.filter((c) => state.favs.has(c.id));
    const rest = list.filter((c) => !state.favs.has(c.id));
    const groups = new Map();
    rest.forEach(function (c) {
      const g = c.group || '未分组';
      if (!groups.has(g)) { groups.set(g, []); }
      groups.get(g).push(c);
    });

    let html = '';
    if (favs.length) { html += groupHTML('★ 我的收藏', favs, '__fav__'); }
    groups.forEach(function (items, g) { html += groupHTML(g, items, g); });
    channelList.innerHTML = html || '<p class="muted" style="padding:12px">没有匹配的频道</p>';
    $('#chanCount').textContent = list.length + ' / ' + state.channels.length + ' 个频道';
  }

  async function loadChannels() {
    try {
      const data = await api('/api/channels');
      state.channels = data.channels || [];
      state.favs = new Set(state.channels.filter((c) => c.favorite).map((c) => c.id));
      render();
      restoreLast();
    } catch (e) {
      channelList.innerHTML = '<p class="muted" style="padding:12px">' + esc(e.message) + '</p>';
    }
  }

  async function toggleFav(id) {
    try {
      const r = await postJSON('/api/favorite/' + id);
      if (r.favorite) { state.favs.add(id); } else { state.favs.delete(id); }
      const ch = state.channels.find((c) => c.id === id);
      if (ch) { ch.favorite = r.favorite; }
      render();
    } catch (e) {
      toast(e.message, 'err');
    }
  }

  /* ---------- 播放 ---------- */

  function showOverlay(text, busy, hint) {
    overlay.classList.remove('hidden');
    overlayText.textContent = text;
    overlayHint.textContent = hint || '';
    spinner.classList.toggle('hidden', !busy);
    retryBtn.classList.add('hidden');
  }

  function hideOverlay() {
    overlay.classList.add('hidden');
    spinner.classList.add('hidden');
    retryBtn.classList.add('hidden');
    clearInterval(state.waitTimer);
    state.waitTimer = null;
  }

  function showError(text) {
    overlay.classList.remove('hidden');
    overlayText.textContent = text;
    overlayHint.textContent = '';
    spinner.classList.add('hidden');
    retryBtn.classList.remove('hidden');
  }

  // 源站投递速率告警：源站没按实时给数据时，画面必然变慢或卡顿，
  // 这跟本地面板、浏览器都没关系，得明说，不要让大家去查错方向。
  function updateSrcWarn(st) {
    if (!srcWarn) { return; }
    const rate = (st && typeof st.rate === 'number') ? st.rate : -1;
    if (rate < 0 || rate >= 0.9) {
      srcWarn.classList.add('hidden');
      srcWarn.textContent = '';
      return;
    }
    const fps = st.fps || 0;
    const peak = st.peak_fps || 0;
    const pct = (peak > 1 && fps > 0) ? Math.round(fps * 100 / peak) : 0;
    srcWarn.textContent = '⚠ 源站投递不足：' + rate.toFixed(2) + 'x 实时' +
      (pct ? '（帧率约为峰值的 ' + pct + '%）' : '') +
      '，画面会变慢或卡顿。这是源站侧限速/拥堵，不是面板的问题。';
    srcWarn.classList.remove('hidden');
  }

  function setNow(ch) {
    $('#npName').textContent = ch ? ch.name : '未选择频道';
    statusEl.textContent = ch ? (ch.group || '') + (ch.kind ? ' · ' + ch.kind : '') : '';
  }

  function destroyPlayer() {
    stopStatusPoll();
    updateSrcWarn(null);
    if (state.hls) {
      try { state.hls.destroy(); } catch (e) { }
      state.hls = null;
    }
    video.removeAttribute('src');
    try { video.load(); } catch (e) { }
  }

  async function play(ch) {
    state.current = ch;
    state.retry = 0;
    render();
    setNow(ch);
    destroyPlayer();
    try { localStorage.setItem('tvhub-last', String(ch.id)); } catch (e) { }
    showOverlay('正在建立直播流…', true, '首次点播需要 3-8 秒，之后换台会很快');
    const started = Date.now();
    clearInterval(state.waitTimer);
    state.waitTimer = setInterval(function () {
      const s = Math.floor((Date.now() - started) / 1000);
      if (s >= 2) { overlayHint.textContent = '已等待 ' + s + ' 秒…'; }
    }, 1000);
    try {
      const r = await postJSON('/api/stream/' + ch.id + '/prepare');
      clearInterval(state.waitTimer);
      if (!r || r.state !== 'ready') {
        showError('无法播放该频道\n' + ((r && r.error) || '转发失败'));
        return;
      }
      attach(r, ch);
    } catch (e) {
      clearInterval(state.waitTimer);
      showError('无法播放该频道\n' + e.message);
    }
  }

  function attach(r, ch) {
    state.mode = r.method;
    if (r.method === 'proxy') {
      video.src = r.playlist;
      video.play().catch(function () { });
      startStatusPoll(ch.id);
      return;
    }
    if (window.Hls && window.Hls.isSupported()) {
      const hls = new window.Hls({
        liveDurationInfinity: true,
        maxBufferLength: 12,
        liveSyncDurationCount: 3,
        manifestLoadingTimeOut: 20000,
        manifestLoadingMaxRetry: 2,
        fragLoadingTimeOut: 25000,
        fragLoadingMaxRetry: 4
      });
      state.hls = hls;
      hls.on(window.Hls.Events.ERROR, function (evt, data) { onHlsError(data); });
      hls.on(window.Hls.Events.MANIFEST_PARSED, function () { video.play().catch(function () { }); });
      hls.loadSource(r.playlist);
      hls.attachMedia(video);
      startStatusPoll(ch.id);
      return;
    }
    if (video.canPlayType('application/vnd.apple.mpegurl')) {
      video.src = r.playlist;
      video.addEventListener('loadedmetadata', function () { video.play().catch(function () { }); }, { once: true });
      startStatusPoll(ch.id);
      return;
    }
    showError('当前浏览器不支持 HLS 播放\n建议使用 Chrome / Edge / Firefox 最新版');
  }

  function onHlsError(data) {
    if (!data || !data.fatal) { return; }
    if (data.type === window.Hls.ErrorTypes.MEDIA_ERROR) {
      try { state.hls.recoverMediaError(); } catch (e) { }
      return;
    }
    state.retry++;
    if (state.retry <= 3) {
      showOverlay('流中断，正在重连（第 ' + state.retry + ' 次）…', true, '');
      setTimeout(function () {
        try { state.hls.startLoad(); } catch (e) { }
      }, 1500);
      return;
    }
    showError('播放中断：' + (data.details || data.type) + '\n可以点击重试，或换一个频道');
  }

  function startStatusPoll(id) {
    stopStatusPoll();
    state.statusTimer = setInterval(async function () {
      if (!state.current || state.current.id !== id) { return; }
      try {
        const st = await api('/api/stream/' + id + '/status');
        if (st.state === 'idle') {
          showError('转发已在服务端停止，请重新点播');
          return;
        }
        if (st.state === 'error') {
          showError('转发失败：' + (st.error || '未知错误') + (st.log ? '\n' + st.log : ''));
          return;
        }
        updateSrcWarn(st);
        if (state.current) {
          statusEl.textContent = (state.current.group || '') + ' · ' +
            (st.viewers > 1 ? st.viewers + ' 人观看 · ' : '') + humanTime(st.uptime_sec);
        }
      } catch (e) { /* 忽略轮询错误 */ }
    }, 10000);
  }

  function stopStatusPoll() {
    if (state.statusTimer) { clearInterval(state.statusTimer); state.statusTimer = null; }
  }

  function humanTime(sec) {
    sec = sec || 0;
    if (sec < 60) { return sec + ' 秒'; }
    if (sec < 3600) { return Math.floor(sec / 60) + ' 分'; }
    return Math.floor(sec / 3600) + ' 小时 ' + Math.floor((sec % 3600) / 60) + ' 分';
  }

  function zap(step) {
    if (!state.visible.length) { return; }
    let idx = state.visible.findIndex((c) => state.current && c.id === state.current.id);
    if (idx < 0) { idx = 0; } else { idx = (idx + step + state.visible.length) % state.visible.length; }
    play(state.visible[idx]);
  }

  function restoreLast() {
    let id = 0;
    try { id = parseInt(localStorage.getItem('tvhub-last') || '0', 10); } catch (e) { }
    const ch = state.channels.find((c) => c.id === id);
    if (ch) {
      state.current = ch;
      render();
      setNow(ch);
      showOverlay('上次观看：' + ch.name + '\n点击左侧频道或这里开始播放', false);
    }
  }

  /* ---------- 事件绑定 ---------- */

  channelList.addEventListener('click', function (e) {
    const favBtn = e.target.closest('[data-fav]');
    if (favBtn) {
      e.stopPropagation();
      toggleFav(parseInt(favBtn.getAttribute('data-fav'), 10));
      return;
    }
    const head = e.target.closest('[data-toggle]');
    if (head) {
      const key = head.getAttribute('data-toggle');
      if (state.collapsed.has(key)) { state.collapsed.delete(key); } else { state.collapsed.add(key); }
      render();
      return;
    }
    const item = e.target.closest('[data-play]');
    if (item) {
      const id = parseInt(item.getAttribute('data-play'), 10);
      const ch = state.channels.find((c) => c.id === id);
      if (ch) { play(ch); }
    }
  });

  overlay.addEventListener('click', function (e) {
    if (e.target === overlay && state.current) { play(state.current); }
  });
  retryBtn.addEventListener('click', function () { if (state.current) { play(state.current); } });

  $('#search').addEventListener('input', function (e) {
    state.query = e.target.value;
    render();
  });
  $('#collapseAll').addEventListener('click', function () {
    const anyOpen = Array.from(document.querySelectorAll('.group')).some((g) => !g.classList.contains('collapsed'));
    state.collapsed = anyOpen
      ? new Set(Array.from(document.querySelectorAll('.group')).map((g) => g.getAttribute('data-group')))
      : new Set();
    render();
  });
  $('#prevBtn').addEventListener('click', function () { zap(-1); });
  $('#nextBtn').addEventListener('click', function () { zap(1); });
  $('#stopBtn').addEventListener('click', async function () {
    const ch = state.current;
    destroyPlayer();
    if (ch) { try { await postJSON('/api/stream/' + ch.id + '/stop'); } catch (e) { } }
    showOverlay('已断开转发', false);
    setNow(null);
    render();
  });

  video.addEventListener('playing', hideOverlay);
  video.addEventListener('error', function () {
    if (state.mode === 'proxy') { showError('源站播放失败，可能是编码不受浏览器支持'); }
  });

  document.addEventListener('keydown', function (e) {
    const tag = (e.target.tagName || '').toLowerCase();
    if (tag === 'input' || tag === 'textarea') { return; }
    if (e.key === 'ArrowDown' || e.key === 'PageDown') { e.preventDefault(); zap(1); }
    if (e.key === 'ArrowUp' || e.key === 'PageUp') { e.preventDefault(); zap(-1); }
  });

  window.addEventListener('beforeunload', destroyPlayer);

  loadChannels();
})();
