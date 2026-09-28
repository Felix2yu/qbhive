// QBHive 前端主脚本
const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

const API = window.location.origin + "/api";
const genID = () => Math.random().toString(36).slice(2, 10);

// ---------- 表单模板原语：全站共用，保证字段行 / 开关 / 说明的排布完全一致 ----------
// field(label, control, hint)：一行字段 = 固定宽标签列 + 控件列（控件在上、说明紧跟其下）
function field(label, control, hint = "") {
  return `<div class="field">
    ${label ? `<label>${label}</label>` : `<span></span>`}
    <div class="field-body">${control}${hint ? `<div class="hint">${hint}</div>` : ""}</div>
  </div>`;
}

// toggle：统一结构的开关（原生 checkbox 视觉隐藏，只留轨道）
// id / data 属性通过 opts 传入，label 显示在开关右侧
function toggle(checked, { id = "", data = "", label = "", sm = false } = {}) {
  const attrs = [id && `id="${id}"`, data, checked && "checked"].filter(Boolean).join(" ");
  return `<label class="switch${sm ? " sm" : ""}">
    <input type="checkbox" ${attrs}>
    <span class="track"></span>${label ? `<span class="txt">${label}</span>` : ""}
  </label>`;
}

// notice：成段说明 / 注意事项，取代散落的行内彩色文字
function notice(text, kind = "") {
  return `<div class="notice ${kind}">${text}</div>`;
}

// 完成通知可选字段（与后端 models.NotifyFields 保持一致，顺序即通知正文顺序）
const NOTIFY_FIELDS = [
  ["name", "任务名"], ["size", "文件大小"], ["category", "分类"],
  ["addedOn", "添加时间"], ["completedOn", "完成时间"],
  ["savePath", "保存路径"], ["tags", "标签"], ["hash", "Hash"],
];

// 渲染通知字段复选框组；未配置（空）时默认全选
function renderNotifyFieldChecks(sel) {
  const chosen = new Set(sel && sel.length ? sel : NOTIFY_FIELDS.map(f => f[0]));
  return NOTIFY_FIELDS.map(([k, label]) =>
    `<label class="check"><input type="checkbox" data-nt-field="${k}"${chosen.has(k) ? " checked" : ""}> ${label}</label>`
  ).join("");
}

// escapeHTML 防 XSS：把外部可控文本安全地嵌入 innerHTML / 属性
function escapeHTML(s) {
  if (s == null) return "";
  return String(s).replace(/[&<>"']/g, c => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"
  }[c]));
}

function toast(msg, type = "ok") {
  const el = document.createElement("div");
  el.className = `toast ${type}`;
  el.textContent = msg;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 2500);
}

// ---------- 弹窗：全站统一用这一套，取代原生 prompt / confirm ----------
function openModal(html) {
  const mask = document.createElement("div");
  mask.className = "modal-mask";
  mask.innerHTML = `<div class="modal" role="dialog" aria-modal="true">${html}</div>`;
  document.body.appendChild(mask);
  const onKey = (e) => { if (e.key === "Escape") done(false); };
  function done(v) {
    document.removeEventListener("keydown", onKey);
    mask.remove();
    resolve(v);
  }
  let resolve;
  const result = new Promise(r => resolve = r);
  document.addEventListener("keydown", onKey);
  mask.addEventListener("mousedown", (e) => { if (e.target === mask) done(false); });
  return { mask, done, result };
}

// confirmDialog：破坏性操作确认（默认焦点留在「取消」，避免回车误触）
function confirmDialog(title, message, { danger = true, okText = "确认" } = {}) {
  const { mask, done, result } = openModal(`
    <h3>${escapeHTML(title)}</h3>
    <p>${escapeHTML(message).replace(/\n/g, "<br>")}</p>
    <div class="modal-actions">
      <button class="btn ${danger ? "confirm-danger" : "primary"}" data-yes>${escapeHTML(okText)}</button>
      <button class="btn" data-no>取消</button>
    </div>`);
  mask.querySelector("[data-yes]").onclick = () => done(true);
  mask.querySelector("[data-no]").onclick = () => done(false);
  setTimeout(() => mask.querySelector("[data-no]").focus(), 0);
  return result;
}

// inputDialog：单值输入（如限速），回车提交
function inputDialog(title, message, { value = "", unit = "", type = "number", min, max, placeholder = "" } = {}) {
  const { mask, done, result } = openModal(`
    <h3>${escapeHTML(title)}</h3>
    ${message ? `<p>${escapeHTML(message).replace(/\n/g, "<br>")}</p>` : ""}
    <div class="controls">
      <input class="w-sm" type="${type}" min="${min ?? ""}" max="${max ?? ""}" placeholder="${escapeHTML(placeholder)}" value="${escapeHTML(value)}" data-input />
      ${unit ? `<span class="unit">${escapeHTML(unit)}</span>` : ""}
    </div>
    <div class="modal-actions">
      <button class="btn primary" data-ok>确定</button>
      <button class="btn" data-no>取消</button>
    </div>`);
  const input = mask.querySelector("[data-input]");
  mask.querySelector("[data-ok]").onclick = () => done(input.value);
  mask.querySelector("[data-no]").onclick = () => done(null);
  input.onkeydown = (e) => { if (e.key === "Enter") done(input.value); };
  setTimeout(() => { input.focus(); input.select && input.select(); }, 0);
  return result;
}
// --- 工具函数 ---
function debounce(fn, wait = 300) {
  let t; return (...args) => { clearTimeout(t); t = setTimeout(() => fn(...args), wait); };
}
function chunkRender(items, perFrame, renderItem, container, done) {
  let i = 0; container.innerHTML = "";
  function step() {
    const frag = document.createDocumentFragment();
    const end = Math.min(i + perFrame, items.length);
    for (; i < end; i++) frag.appendChild(renderItem(items[i], i));
    container.appendChild(frag);
    if (i < items.length) requestAnimationFrame(step);
    else done && done();
  }
  requestAnimationFrame(step);
}
function paginate(total, page, pageSize) {
  const pages = Math.max(1, Math.ceil(total / pageSize));
  page = Math.max(1, Math.min(page, pages));
  return { pages, page, start: (page - 1) * pageSize, end: Math.min(page * pageSize, total) };
}
function pageNav(total, page, pageSize, onChange) {
  const { pages, page: cur } = paginate(total, page, pageSize);
  if (pages <= 1) return "";
  const btn = (label, target, cls = "") =>
    `<button class="btn small ${cls}" ${target === cur ? "disabled" : ""} data-p="${target}">${label}</button>`;
  const parts = [btn("«", 1), btn("‹", cur - 1)];
  // 省略号逻辑：只显示前3、后3、当前页附近1页
  const windowSet = new Set([1, 2, 3, pages - 2, pages - 1, pages, cur - 1, cur, cur + 1].filter(x => x >= 1 && x <= pages));
  let last = 0;
  for (const n of [...windowSet].sort((a, b) => a - b)) {
    if (n > last + 1) parts.push(`<span class="text-dim">…</span>`);
    parts.push(btn(String(n), n, n === cur ? "primary" : ""));
    last = n;
  }
  parts.push(btn("›", cur + 1), btn("»", pages));
  return `<div class="pager">${parts.join("")}</div>`;
}

// 绑定 pageNav 生成的分页按钮（此前从未绑定，分页按钮一直是死的）
function bindPageNav(onChange) {
  $$("[data-p]").forEach(b => {
    if (b.__bound) return;
    b.__bound = true;
    b.addEventListener("click", () => onChange(parseInt(b.dataset.p, 10)));
  });
}


async function api(method, path, body) {
  const opts = { method, headers: {}, credentials: "include" };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  let resp = await fetch(API + path, opts);
  if (resp.status === 401) {
    // 需要登录
    const ok = await showLoginOverlay();
    if (!ok) {
      toast("需要 token 才能访问", "err");
      return { success: false, message: "unauthorized" };
    }
    // 登录成功后重试
    resp = await fetch(API + path, opts);
  }
  // 响应可能不是 JSON（如后端 panic 时 gin 返回 500 空 body、反代返回 HTML 错误页），
  // 解析失败必须返回可展示的错误，不能抛异常让页面永远停在"加载中…"
  try {
    return await resp.json();
  } catch {
    return {
      success: false,
      message: resp.ok ? "服务端返回了非 JSON 响应" : `服务端错误 (HTTP ${resp.status})`,
    };
  }
}

// 登录覆盖层：输入 token，成功后 set cookie
function showLoginOverlay() {
  return new Promise(resolve => {
    const existing = document.getElementById("__qbhive_login__");
    if (existing) existing.remove();

    const { mask, done, result } = openModal(`
      <h3>需要访问 token</h3>
      <p>服务端设置了 <code>QBHIVE_TOKEN</code> 环境变量，请输入对应的访问 token。</p>
      <input type="password" data-input placeholder="token" class="mono" />
      <div class="modal-actions">
        <button class="btn primary" data-ok>登录</button>
        <button class="btn" data-no>取消</button>
      </div>`);
    mask.id = "__qbhive_login__";
    const input = mask.querySelector("[data-input]");

    const submit = async () => {
      const token = input.value.trim();
      if (!token) return;
      const r = await fetch(API + "/login", {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ token }),
      });
      done(r.ok);
    };
    mask.querySelector("[data-ok]").onclick = submit;
    mask.querySelector("[data-no]").onclick = () => done(false);
    input.onkeydown = (e) => { if (e.key === "Enter") submit(); };
    setTimeout(() => input.focus(), 0);
    result.then(resolve);
  });
}

function humanSize(n) {
  if (n == null) return "-";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  let i = 0, v = Number(n);
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(v < 10 && i > 0 ? 2 : 1) + " " + units[i];
}

function humanSpeed(bps) {
  if (!bps) return "0 KB/s";
  return humanSize(bps) + "/s";
}

// 前端轻量校验：返回错误消息字符串，空串表示通过
function validateConfigJS(cfg) {
  const c = cfg || {};
  // qB URL
  if (c.qbittorrent && c.qbittorrent.url && c.qbittorrent.url.trim()) {
    const u = c.qbittorrent.url.trim();
    if (!/^https?:\/\//.test(u)) return "qBittorrent URL 必须以 http:// 或 https:// 开头";
  }
  // 间隔字段
  if (c.rss && c.rss.enabled) {
    if ((c.rss.interval || 0) < 1) return "RSS 刷新间隔必须 ≥ 1 分钟";
  }
  if (c.limiter && c.limiter.enabled) {
    if ((c.limiter.interval || 0) < 1) return "限速器刷新间隔必须 ≥ 1 秒";
  }
  if (c.fileManager && c.fileManager.enabled) {
    if ((c.fileManager.scanInterval || 0) < 1) return "文件管理扫描间隔必须 ≥ 1 秒";
  }
  // Apprise URL 协议
  if (c.notifier) {
    const urls = (c.notifier.appriseUrls || []).map(s => (s || "").trim()).filter(Boolean);
    for (const u of urls) {
      if (!u.includes("://")) return `通知 URL 缺少协议前缀: ${u.slice(0, 60)}`;
    }
    if (c.notifier.enabled && (c.notifier.fields || []).length === 0) {
      return "请至少勾选一个通知字段";
    }
  }
  // RSS feeds
  if (c.rss) {
    for (const feed of (c.rss.feeds || [])) {
      if (feed.url && feed.url.trim() && !feed.url.trim().startsWith("http")) {
        return `RSS 订阅源 "${feed.name || feed.url}" 的 URL 必须以 http 开头`;
      }
      for (const r of (feed.rules || [])) {
        if (r.mode === "regex") {
          if (r.include && r.include.trim()) {
            try { new RegExp(r.include); } catch (e) {
              return `订阅源 ${feed.name || ""} / 规则 ${r.name || ""} 的 include 正则无效: ${e.message}`;
            }
          }
          if (r.exclude && r.exclude.trim()) {
            try { new RegExp(r.exclude); } catch (e) {
              return `订阅源 ${feed.name || ""} / 规则 ${r.name || ""} 的 exclude 正则无效: ${e.message}`;
            }
          }
        }
      }
    }
  }
  // Limiter rules
  for (const r of ((c.limiter && c.limiter.rules) || [])) {
    if (r.match && r.match.trim()) {
      try { new RegExp(r.match); } catch (e) {
        return `限速规则 "${r.name || ""}" 的 match 正则无效: ${e.message}`;
      }
    }
  }
  // 文件管理：清理自定义正则 + AI 通道
  if (c.fileManager) {
    const fm = c.fileManager;
    const rules = fm.cleanRules || [];
    if (rules.length > 32) return "文件名清理自定义规则不能超过 32 条";
    for (let i = 0; i < rules.length; i++) {
      if ((rules[i] || "").length > 256) return `文件名清理规则 ${i + 1} 长度超过 256 字符`;
      if ((rules[i] || "").trim()) {
        try { new RegExp(rules[i]); } catch (e) {
          return `文件名清理规则 ${i + 1} 正则无效: ${e.message}`;
        }
      }
    }
    const chans = fm.aiChannels || [];
    if (chans.length > 8) return "AI 通道不能超过 8 个";
    for (const ch of chans) {
      if (!ch.enabled) continue;
      if (!/^https?:\/\//.test((ch.baseURL || "").trim())) {
        return `AI 通道 "${ch.name || ch.id || ""}" 的 BaseURL 必须以 http:// 或 https:// 开头`;
      }
      if (!(ch.model || "").trim()) {
        return `AI 通道 "${ch.name || ch.id || ""}" 未填写模型名`;
      }
      if ((ch.prompt || "").length > 4000) {
        return `AI 通道 "${ch.name || ch.id || ""}" 的提示词超过 4000 字符`;
      }
    }
  }
  return "";
}

// ---------- 路由 ----------
const views = {
  dashboard: renderDashboard,
  torrents: renderTorrents,
  files: renderFiles,
  rss: renderRSS,
  settings: renderSettings,
};

// 从 location.hash 解析目标视图（#/torrents → torrents）；无效 hash 一律回落到概览
function hashToView() {
  const m = /^#\/([A-Za-z]+)/.exec(location.hash || "");
  return m && views[m[1]] ? m[1] : "dashboard";
}

let currentView = "dashboard";
async function switchView(name, background = false) {
  if (!views[name]) name = "dashboard";
  currentView = name;
  // hash 路由：把 URL 同步成当前页，刷新/分享链接都能停留在原页面；
  // 后台自动刷新（background）不改 URL。赋值会触发 hashchange，
  // 监听器里目标与 currentView 相同则忽略，不会重复渲染。
  if (!background && location.hash !== "#/" + name) {
    location.hash = "#/" + name;
  }
  $$(".tab").forEach(b => b.classList.toggle("active", b.dataset.view === name));
  const root = $("#content");
  root.dataset.view = name;
  // 后台刷新：不清空页面，保留旧数据，只在渲染函数内部更新
  if (!background) {
    root.innerHTML = '<div class="empty">加载中…</div>';
  }
  try {
    await views[name](root, background);
  } catch (e) {
    root.innerHTML = notice(`页面渲染出错：${escapeHTML(e.message || String(e))}`, "danger");
  }
}

$$(".tab").forEach(b => b.addEventListener("click", () => switchView(b.dataset.view)));

// 浏览器前进/后退、手动改 hash 时切换到对应页面
window.addEventListener("hashchange", () => {
  const v = hashToView();
  if (v !== currentView) switchView(v);
});

// ---------- 主题 ----------
// 三种模式：auto（跟随系统）、dark、light；持久化到 localStorage
const THEME_KEY = "qbhive-theme";

function applyTheme(mode) {
  document.documentElement.setAttribute("data-theme", mode);
  // 页面上任何 .theme-switch 里的按钮都同步高亮
  document.querySelectorAll(".theme-switch button").forEach(b => {
    b.classList.toggle("active", b.dataset.themeOpt === mode);
  });
}

function getTheme() {
  let saved = localStorage.getItem(THEME_KEY);
  if (!saved || !["auto", "dark", "light"].includes(saved)) saved = "auto";
  return saved;
}

function setTheme(mode) {
  localStorage.setItem(THEME_KEY, mode);
  applyTheme(mode);
}

// 初始化：页面加载完立即应用一次，避免 FOUC
applyTheme(getTheme());

// ---------- 概览 ----------
let _refreshRunning = false;

async function renderDashboard(root, background = false) {
  // 后台刷新：保留已有的 DOM 结构，只更新数字和列表
  if (background) {
    await refreshDashboard(root);
    return;
  }
  // 首次加载：搭结构
  root.innerHTML = `
    <div class="page-head">
      <div class="titles">
        <h2>概览</h2>
        <p>下载 / 做种的实时状态，每 8 秒自动刷新。</p>
      </div>
    </div>
    <div class="status-bar">
      <div class="status-group" id="dash-status-group"></div>
      <div class="speed-group" id="dash-speed-group"></div>
    </div>
    <div class="card">
      <div class="card-head">
        <h3>最近活跃任务</h3>
        <span class="desc">按添加时间倒序，最多 10 条</span>
      </div>
      <div id="dash-latest"><div class="empty">加载中…</div></div>
    </div>
  `;
  await refreshDashboard(root);
}

async function refreshDashboard(root) {
  // 标记刷新中（状态带右上角小 spinner）
  const sb = root.querySelector(".status-bar");
  if (sb) sb.classList.add("refreshing");

  // stats 与最近活跃列表并发拉取，首屏等待从「二者之和」降到「取最大」
  const [t, top10] = await Promise.all([
    api("GET", "/torrents/stats"),
    api("GET", "/torrents?filter=active&limit=10&sort=added_on&reverse=true"),
  ]);
  const s = t.data || {};
  const dlSpeed = s.dlSpeed || 0, upSpeed = s.upSpeed || 0;
  const dlLimit = s.dlSpeedLimit || 0, upLimit = s.upSpeedLimit || 0;
  const downloading = s.downloadingCount || 0, seeding = s.seedingCount || 0;
  const stalled = s.stalledCount || 0, errored = s.erroredCount || 0;
  const stoppedDl = s.stoppedDL || 0, stoppedUp = s.stoppedUP || 0;

  const speedBar = (cur, limit, kind) => {
    if (cur === 0 && limit === 0) return "";
    const pct = limit > 0 ? Math.min(100, Math.round(cur / limit * 100)) : 0;
    const speedCls = kind === "dl" ? "var(--accent)" : "var(--success)";
    const dirIcon = kind === "dl" ? "▼" : "▲";
    const limitStr = limit > 0 ? humanSpeed(limit) : "∞";
    return `<div class="speed">
      <div class="speed-label"><span style="color:${speedCls}">${dirIcon}</span>
        <span>${humanSpeed(cur)}</span>
        <span class="speed-limit">/ ${limitStr}</span>
      </div>
      <div class="progress-bar"><div style="width:${pct}%;background:${speedCls}"></div></div>
    </div>`;
  };

  const group = root.querySelector("#dash-status-group");
  if (group) group.innerHTML = `
    <span class="status-chip dl">下载中 ${downloading}</span>
    <span class="status-chip up">做种中 ${seeding}</span>
    ${stalled > 0 ? `<span class="status-chip warn">停滞 ${stalled}</span>` : ""}
    ${stoppedDl > 0 ? `<span class="status-chip dim">停止未完成 ${stoppedDl}</span>` : ""}
    ${stoppedUp > 0 ? `<span class="status-chip ok">已完成 ${stoppedUp}</span>` : ""}
    ${errored > 0 ? `<span class="status-chip danger">⚠ 异常 ${errored}</span>` : ""}
  `;
  const sg = root.querySelector("#dash-speed-group");
  if (sg) sg.innerHTML = speedBar(dlSpeed, dlLimit, "dl") + speedBar(upSpeed, upLimit, "up");

  // 任务列表：已与 stats 并发获取，到了再替换
  const list = top10.data || [];
  const slot = root.querySelector("#dash-latest");
  if (slot) {
    slot.innerHTML = list.length === 0
      ? '<div class="empty">当前没有活跃任务</div>'
      : torrentTable(list, false);
  }

  if (sb) sb.classList.remove("refreshing");
}


// 任务状态 → 短标签 + 语义色档（与 .tag 一套色板，不再逐个写行内颜色）
const STATE_TAGS = {
  downloading:   { t: "下载",   c: "busy" },
  forcedDL:      { t: "下载",   c: "busy" },
  stalledDL:     { t: "停滞",   c: "warn" },
  stoppedDL:     { t: "停止",   c: "dim" },
  stoppedUP:     { t: "已完成", c: "ok" },
  stalledUP:     { t: "做种",   c: "warn" },
  uploading:     { t: "做种",   c: "ok" },
  forcedUP:      { t: "做种",   c: "ok" },
  queuedUP:      { t: "排队",   c: "dim" },
  queuedDL:      { t: "排队",   c: "dim" },
  checkingUP:    { t: "校验",   c: "alt" },
  checkingDL:    { t: "校验",   c: "alt" },
  metaDL:        { t: "元数据", c: "alt" },
  forcedMetaDL:  { t: "元数据", c: "alt" },
  moving:        { t: "移动",   c: "alt" },
  checkingResumeData: { t: "恢复", c: "alt" },
  missingFiles:  { t: "缺文件", c: "err" },
  error:         { t: "错误",   c: "err" },
};

function stateTag(s) {
  const v = STATE_TAGS[s] || { t: s, c: "dim" };
  return `<span class="tag ${v.c}">${escapeHTML(v.t)}</span>`;
}

function torrentTable(list, withActions) {
  if (!list || list.length === 0) return '<div class="empty">暂无任务</div>';
  const rows = list.map(t => torrentRow(t, withActions)).join("");
  const head = `
    <div class="torrent-row head${withActions ? "" : " no-actions"}">
      <div class="cell name">名称</div>
      <div class="cell state">状态</div>
      <div class="cell progress">进度</div>
      <div class="cell size">大小</div>
      <div class="cell speed">下速</div>
      <div class="cell speed">上速</div>
      <div class="cell cat">分类</div>
      ${withActions ? '<div class="cell actions">限速</div>' : ''}
    </div>`;
  return `<div class="torrent-list">${head}${rows}</div>`;
}

function torrentRow(t, withActions) {
  const pct = (t.progress * 100).toFixed(1);
  const safeName = escapeHTML(t.name);
  const safeCat = escapeHTML(t.category || "-");
  const safeHash = escapeHTML(t.hash);
  return `<div class="torrent-row${withActions ? "" : " no-actions"}">
    <div class="cell name">
      <div class="torr-name" title="${safeName}">${safeName}</div>
    </div>
    <div class="cell state">${stateTag(t.state)}</div>
    <div class="cell progress">
      <div class="progress-bar"><div style="width:${pct}%"></div></div>
      <span class="progress-text">${pct}%</span>
    </div>
    <div class="cell size">${humanSize(t.downloaded)} <span class="text-dim">/ ${humanSize(t.size)}</span></div>
    <div class="cell speed dl">${humanSpeed(t.dlspeed)}</div>
    <div class="cell speed up">${humanSpeed(t.upspeed)}</div>
    <div class="cell cat" title="${safeCat}">${safeCat}</div>
    ${withActions ? `<div class="cell actions"><button class="btn small" data-limit-btn="${safeHash}">限速</button></div>` : ""}
  </div>`;
}

// 限速按钮弹框绑定
function bindLimitButtons() {
  $$("[data-limit-btn]").forEach(btn => {
    if (btn.__bound) return; btn.__bound = true;
    btn.addEventListener("click", () => openLimitDialog(btn.dataset.limitBtn));
  });
}

async function openLimitDialog(hash) {
  let currentLimit = 0;
  try {
    const r = await api("GET", `/torrents/${hash}/limit`);
    if (r.success && r.data) currentLimit = r.data.uploadLimit || 0;
  } catch {}

  const val = await inputDialog("上传限速", "设为 0 表示不限速。", { value: currentLimit, unit: "KB/s", min: 0 });
  if (val === null) return;
  const v = parseInt(val, 10);
  if (isNaN(v) || v < 0) { toast("请输入 ≥ 0 的数字", "err"); return; }
  const res = await api("POST", `/torrents/${hash}/limit`, { uploadLimit: v });
  toast(res.success ? "已应用限速" : (res.message || "失败"), res.success ? "ok" : "err");
}

function filterList(list, kw) {
  if (!kw) return list;
  return list.filter(t =>
    (t.name || "").toLowerCase().includes(kw) ||
    (t.category || "").toLowerCase().includes(kw) ||
    (t.tags || "").toLowerCase().includes(kw)
  );
}

// ---------- 任务 ----------
// 任务列表状态（模块级，切 filter/limit 不丢）
let _torrentsState = {
  filter: "active",
  limit: 500,
  page: 1,
  pageSize: 50,
  search: "",
  sort: "added_on",
  reverse: "true",
  rawList: [],    // 后端返回的原始列表（未过滤未分页）
};

// 前端友好的窄 filter 选项（带中文标签）
// 值为 qBittorrent v5.0+ 的 torrent state 值，后端会映射到对应的 qB API filter 参数
const torrentFilterOptions = [
  { value: "active",       label: "活跃（下载+做种）" },
  { value: "downloading",  label: "下载中" },
  { value: "stoppedDL",    label: "停止下载（未完成）" },
  { value: "stoppedUP",    label: "已完成历史（停止）" },
  { value: "stalledUP",    label: "历史（做种停滞）" },
  { value: "completed",    label: "所有已完成" },
  { value: "all",          label: "全部任务（数量可能很大）" },
];

async function renderTorrents(root, background = false) {
  // 后台刷新：保留工具栏和旧列表，只重新拉取
  if (background) { await fetchTorrents(); return; }

  root.innerHTML = `
    <div class="page-head">
      <div class="titles">
        <h2>下载任务</h2>
        <p>按状态筛选、搜索与分页；可对单个任务直接下发上传限速。</p>
      </div>
    </div>
    <div class="card">
      <div class="card-head">
        <h3>任务列表</h3>
        <span class="spacer"></span>
        <div class="head-actions">
          <span class="desc" id="tf-progress"></span>
          <button class="btn small" id="tf-reload">刷新</button>
        </div>
      </div>
      <div class="toolbar">
        <label class="tfield"><span>状态</span>
          <select id="tf-filter" class="w-lg">
            ${torrentFilterOptions.map(o =>
              `<option value="${o.value}" ${o.value===_torrentsState.filter?"selected":""}>${o.label}</option>`
            ).join("")}
          </select>
        </label>
        <label class="tfield"><span>数量</span>
          <select id="tf-limit" class="w-md">
            ${[100, 200, 500, 1000, 0].map(n =>
              `<option value="${n}" ${n===_torrentsState.limit?"selected":""}>${n===0?"全量":n}</option>`
            ).join("")}
          </select>
        </label>
        <label class="tfield grow"><span>搜索</span>
          <input type="text" id="tf-search" value="${escapeHTML(_torrentsState.search)}" placeholder="任务名 / 分类 / 标签" />
        </label>
      </div>
      <div class="notice-slot" id="tf-notice"></div>
      <div id="tf-body"><div class="empty">加载中…</div></div>
    </div>
  `;

  // 全量模式警告
  if (_torrentsState.filter === "all" || _torrentsState.filter === "completed") {
    $("#tf-notice").innerHTML = notice(
      _torrentsState.filter === "all"
        ? "全量模式可能加载数千条任务，首次请求较慢；后端返回完整列表后再前端分页。"
        : "「已完成」包含停止、做种、停滞三类，数量可能很多；建议改用「已完成历史（停止）。",
      "warn"
    );
  }

  // 事件绑定
  $("#tf-filter").onchange = e => { _torrentsState.filter = e.target.value; _torrentsState.page = 1; fetchTorrents(); };
  $("#tf-limit").onchange  = e => { _torrentsState.limit  = parseInt(e.target.value, 10); _torrentsState.page = 1; fetchTorrents(); };
  $("#tf-reload").onclick  = fetchTorrents;
  $("#tf-search").oninput  = debounce(e => {
    _torrentsState.search = e.target.value.trim().toLowerCase();
    _torrentsState.page = 1;
    renderPage();
  }, 300);

  fetchTorrents();
}

// 拉取任务 + 渲染列表（模块级，后台刷新可直接调用）
async function fetchTorrents() {
  const progress = $("#tf-progress");
  if (progress) progress.textContent = "拉取中…";
  try {
    const t = await api("GET",
      `/torrents?filter=${_torrentsState.filter}&limit=${_torrentsState.limit}&sort=${_torrentsState.sort}&reverse=${_torrentsState.reverse}`);
    if (!t.success) {
      const body = $("#tf-body");
      if (body) body.innerHTML = notice(`加载失败：${escapeHTML(t.message || "未知错误")}`, "danger");
      if (progress) progress.textContent = "";
      return;
    }
    _torrentsState.rawList = t.data || [];
    if (progress) progress.textContent = `已加载 ${_torrentsState.rawList.length} 条`;
    renderPage();
  } catch (e) {
    const body = $("#tf-body");
    if (body) body.innerHTML = notice(`加载失败：${escapeHTML(e.message || String(e))}`, "danger");
    if (progress) progress.textContent = "";
  }
}

function renderPage() {
  const list = filterList(_torrentsState.rawList, _torrentsState.search);
  const { pages, page, start, end } = paginate(list.length, _torrentsState.page, _torrentsState.pageSize);
  const pageSlice = list.slice(start, end);

  const progress = $("#tf-progress");
  if (progress) progress.textContent =
    `已加载 ${_torrentsState.rawList.length} 条${_torrentsState.search ? `，搜索命中 ${list.length} 条` : ""} · 显示 ${start + 1}-${Math.min(end, list.length)} / ${list.length}`;

  const body = $("#tf-body");
  if (!body) return;
  if (pageSlice.length === 0) {
    body.innerHTML = `<div class="empty">${_torrentsState.search ? "搜索无匹配" : "当前没有任务"}</div>`;
    return;
  }

  body.innerHTML = torrentTable(pageSlice, true) + pageNav(list.length, page, _torrentsState.pageSize);
  bindPageNav(p => { _torrentsState.page = p; renderPage(); });
  bindLimitButtons();
}

// ---------- RSS ----------
// rss 参数可传：首次进入页面或显式传 undefined 时从服务器拉；
// 新增/删除/改规则后传本地已修改的 rss，避免覆盖内存中的改动。
let _rssStatusTimer = null;

async function renderRSS(root, rss) {
  // 第二个参数可能是 switchView 传入的 background 布尔，也可能是内部重渲传入的 rss 配置对象；
  // 只有真正的对象才复用，其余一律从 /config 重新拉取（并兜底请求失败）
  if (!rss || typeof rss !== "object") {
    const cr = await api("GET", "/config");
    if (!cr.success || !cr.data) {
      root.innerHTML = `<div class="empty">载入配置失败：${escapeHTML(cr.message || "未知错误")}</div>`;
      return;
    }
    rss = cr.data.rss || {};
  }
  if (!rss.feeds) rss.feeds = [];

  root.innerHTML = `
    <div class="page-head">
      <div class="titles">
        <h2>RSS 订阅</h2>
        <p>定时拉取订阅源，按规则匹配条目并自动提交下载；条目去重持久化，重启不重复下载。</p>
      </div>
      <div class="head-actions">
        <button class="btn" id="rss-add">+ 添加订阅源</button>
      </div>
    </div>

    <div class="card">
      <div class="card-head">
        <h3>全局设置</h3>
        <span class="spacer"></span>
        <div class="head-actions">
          <button class="btn small" id="rss-force">立即拉取</button>
          <button class="btn small" id="rss-reset-all">全部重新匹配</button>
          <button class="btn small primary" id="rss-save">保存</button>
        </div>
      </div>
      ${field("启用", toggle(rss.enabled, { id: "rss-enabled" }), "关闭后不再拉取任何订阅源，已提交的下载不受影响。")}
      ${field("刷新间隔",
        `<div class="controls"><input type="number" id="rss-interval" class="w-xs" min="1" value="${rss.interval || 15}"><span class="unit">分钟</span></div>`,
        "到点后逐个订阅源拉取。")}
    </div>

    ${rss.feeds.length === 0 ? `<div class="card"><div class="empty">还没有订阅源，点右上角「+ 添加订阅源」开始。</div></div>` :
      rss.feeds.map((f, fi) => renderFeedBlock(f, fi)).join("")}

    <div class="card">
      <div class="card-head">
        <h3>运行状态</h3>
        <span class="desc">每 5 秒自动刷新</span>
        <span class="spacer"></span>
      </div>
      <div id="rss-status-panel"><div class="empty">加载中…</div></div>
    </div>
  `;

  $("#rss-force").onclick = async () => {
    await api("POST", "/rss/force");
    toast("已触发 RSS 拉取");
    refreshRSSStatus(true); // true = 立即拉一次，显示 fetching
  };
  $("#rss-save").onclick = () => saveRSS(rss);
  $("#rss-add").onclick = () => {
    // 先把用户在 DOM 里已填的内容同步回 rss 对象，避免重渲染丢失
    collectRSS(rss);
    const newFeed = { id: genID(), name: "新订阅源", url: "", enabled: true, rules: [] };
    rss.feeds.push(newFeed);
    renderRSS(root, rss);
  };
  $("#rss-reset-all").onclick = async () => {
    const ok = await confirmDialog("全部重新匹配",
      "确定让所有订阅源对历史条目重新匹配规则吗？\n已下载过的不会重复下载。",
      { danger: false, okText: "重置" });
    if (!ok) return;
    const r = await api("POST", "/rss/reset", { feedId: "" });
    toast(r.success ? "已重置，正在重新拉取…" : (r.message || "重置失败"), r.success ? "ok" : "err");
    refreshRSSStatus(true);
  };
  // 事件委托：订阅源状态块里的重置按钮
  $("#rss-status-panel").addEventListener("click", async (e) => {
    const btn = e.target.closest("[data-rss-reset]");
    if (!btn) return;
    const fid = btn.dataset.rssReset;
    const name = btn.dataset.feedName || "该订阅源";
    const ok = await confirmDialog("重新匹配",
      `确定让「${name}」对历史条目重新匹配规则吗？\n已下载过的不会重复下载。`,
      { danger: false, okText: "重置" });
    if (!ok) return;
    const r = await api("POST", "/rss/reset", { feedId: fid });
    toast(r.success ? "已重置，正在重新拉取…" : (r.message || "重置失败"), r.success ? "ok" : "err");
    refreshRSSStatus(true);
  });
  bindFeedEvents(rss);

  // 启动状态自动刷新（先停掉旧的，避免多个 RSS 页并发）
  if (_rssStatusTimer) clearInterval(_rssStatusTimer);
  await refreshRSSStatus(true);
  _rssStatusTimer = setInterval(() => {
    if (currentView !== "rss") { clearInterval(_rssStatusTimer); _rssStatusTimer = null; return; }
    refreshRSSStatus(false);
  }, 5000);
}

function renderFeedBlock(f, fi) {
  const sName = escapeHTML(f.name || "");
  const sUrl  = escapeHTML(f.url || "");
  const rules = f.rules || [];
  return `
  <div class="card feed">
    <div class="item-head">
      <span class="idx">${fi + 1}</span>
      <input type="text" class="name" id="f-${f.id}-name" value="${sName}" placeholder="订阅源名称" />
      ${toggle(f.enabled, { id: `f-${f.id}-enabled`, sm: true })}
      <button class="btn small danger" data-del-feed="${f.id}">删除</button>
    </div>
    ${field("订阅地址", `<input type="url" id="f-${f.id}-url" class="ctrl mono" value="${sUrl}" placeholder="https://…" />`)}

    <div class="section">
      <div class="section-title">
        匹配规则<span class="text-dim">共 ${rules.length} 条</span>
        <span class="spacer"></span>
        <button class="btn small" data-add-rule="${f.id}">+ 添加规则</button>
      </div>
      ${rules.length === 0 ? `<div class="item"><div class="empty">还没有规则，点右上角「+ 添加规则」。</div></div>` :
        rules.map((r, ri) => renderRuleBlock(f.id, r, ri)).join("")}
    </div>
  </div>`;
}

function renderRuleBlock(fid, r, ri) {
  const sName     = escapeHTML(r.name || ("规则 " + (ri + 1)));
  const sInclude  = escapeHTML(r.include || "");
  const sExclude  = escapeHTML(r.exclude || "");
  const sPath     = escapeHTML(r.savePath || "");
  const sCategory = escapeHTML(r.category || "");
  const sTags     = escapeHTML(r.tags || "");
  return `
    <div class="item">
      <div class="item-head">
        <span class="idx">${ri + 1}</span>
        <input type="text" class="name" data-rule-name="${fid}-${ri}" value="${sName}" placeholder="规则名称" />
        ${toggle(r.enabled !== false, { data: `data-rule-enabled="${fid}-${ri}"`, sm: true })}
        <button class="btn small danger" data-del-rule="${fid}-${ri}">删除</button>
      </div>
      ${field("匹配模式",
        `<select class="ctrl" data-rule-mode="${fid}-${ri}">
          <option value="keyword" ${r.mode !== "regex" ? "selected" : ""}>关键字（包含 / 排除表达式）</option>
          <option value="regex" ${r.mode === "regex" ? "selected" : ""}>正则表达式</option>
        </select>`)}
      ${field("包含 Include",
        `<textarea wrap="soft" class="auto-h ctrl" data-rule-include="${fid}-${ri}" placeholder="${r.mode === "regex" ? "正则，需匹配" : "例：ManoJob 720p|MrLucky（| 为 OR，空格为 AND）"}">${sInclude}</textarea>`)}
      ${field("排除 Exclude",
        `<textarea wrap="soft" class="auto-h ctrl" data-rule-exclude="${fid}-${ri}" placeholder="例：1080p|2160p，命中任一则跳过">${sExclude}</textarea>`)}
      ${field("保存路径",
        `<input type="text" class="ctrl mono" data-rule-path="${fid}-${ri}" value="${sPath}" placeholder="留空则用 qBittorrent 默认路径" />`)}
      ${field("分类 / 标签",
        `<div class="split">
          <input type="text" data-rule-category="${fid}-${ri}" value="${sCategory}" placeholder="Category" />
          <input type="text" data-rule-tags="${fid}-${ri}" value="${sTags}" placeholder="Tags（逗号分隔）" />
        </div>`)}
      ${field("上传限速",
        `<div class="controls"><input type="number" class="w-xs" data-rule-upload="${fid}-${ri}" value="${r.uploadLimit || 0}" min="0"><span class="unit">KB/s</span></div>`,
        "0 表示不限速。")}
      ${field("添加后停止", toggle(r.stopped, { data: `data-rule-stopped="${fid}-${ri}"` }),
        "只加入 qBittorrent，不自动开始下载。")}
    </div>`;
}

function bindFeedEvents(rss) {
  $$("[data-del-feed]").forEach(b => b.addEventListener("click", () => {
    // 先同步 DOM 输入 → rss 对象，避免其他正在编辑的 feed 丢值
    collectRSS(rss);
    const id = b.dataset.delFeed;
    rss.feeds = rss.feeds.filter(f => f.id !== id);
    renderRSS(document.getElementById("content"), rss);
  }));
  $$("[data-add-rule]").forEach(b => b.addEventListener("click", () => {
    collectRSS(rss);
    const fid = b.dataset.addRule;
    const feed = rss.feeds.find(f => f.id === fid);
    feed.rules = feed.rules || [];
    feed.rules.push({ id: genID(), name: "规则 " + (feed.rules.length + 1), enabled: true, mode: "keyword", include: "", exclude: "", savePath: "", category: "", tags: "", uploadLimit: 0, stopped: false });
    renderRSS(document.getElementById("content"), rss);
  }));
  $$("[data-del-rule]").forEach(b => b.addEventListener("click", () => {
    collectRSS(rss);
    const [fid, ri] = b.dataset.delRule.split("-");
    const feed = rss.feeds.find(f => f.id === fid);
    feed.rules.splice(parseInt(ri, 10), 1);
    renderRSS(document.getElementById("content"), rss);
  }));
}

// RSS 运行状态面板辅助
function fmtTimeAgo(iso) {
  if (!iso) return "从未";
  const t = new Date(iso);
  if (isNaN(t.getTime())) return "未知";
  const diff = Math.max(0, Math.floor((Date.now() - t.getTime()) / 1000));
  if (diff < 5) return "刚刚";
  if (diff < 60) return `${diff} 秒前`;
  const m = Math.floor(diff / 60);
  if (m < 60) return `${m} 分钟前`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h} 小时前`;
  const d = Math.floor(h / 24);
  return `${d} 天前`;
}

async function refreshRSSStatus(showLoading) {
  const panel = document.getElementById("rss-status-panel");
  if (!panel) return;
  if (showLoading) panel.innerHTML = '<div class="empty">拉取中…</div>';
  const res = await api("GET", "/rss/status");
  if (!res.success) {
    panel.innerHTML = notice(`加载状态失败：${escapeHTML(res.message || "未知错误")}`, "danger");
    return;
  }
  panel.innerHTML = renderRSSStatusList(res.data || []);
}

function renderRSSStatusList(list) {
  if (list.length === 0) {
    return '<div class="empty">暂无可监控的订阅源</div>';
  }
  return list.map(s => {
    const seenLine = `累计已见 <b>${s.seenCount}</b>`;
    let detailLine;
    if (s.lastOk != null) {
      detailLine = s.lastOk
        ? `解析 <b>${s.itemCount}</b> 条 · 命中 <b>${s.matched}</b> · 提交下载 <b>${s.downloaded}</b>${s.failed > 0 ? ` · 失败 <b>${s.failed}</b>` : ""}${s.snapshot ? ` · <span class="tag warn">快照模式</span>` : ""}`
        : `<span class="tag err">上次拉取失败</span> ${escapeHTML(s.lastError || "")}`;
    } else {
      detailLine = `<span class="text-dim">尚未拉取过</span>`;
    }

    const recent = (s.recentItems || []).map(r => renderRecentItem(r)).join("");
    const recentBlock = recent
      ? `<details open>
           <summary>最近 ${(s.recentItems || []).length} 条处理记录</summary>
           <div class="rss-recent">${recent}</div>
         </details>`
      : "";

    return `
    <div class="rss-status-block">
      <div class="rss-status-head">
        <span class="rss-status-name">${escapeHTML(s.feedName)}</span>
        ${renderFeedStatusBadge(s)}
        <span class="rss-status-time">${fmtTimeAgo(s.lastFetchAt)}拉取 · ${seenLine}</span>
        <button class="btn small" data-rss-reset="${s.feedId}" data-feed-name="${escapeHTML(s.feedName)}" ${s.fetching ? "disabled" : ""} title="清空已见过条目，对所有历史条目重新跑匹配规则">重新匹配</button>
      </div>
      <div class="rss-status-detail">${detailLine}</div>
      <div class="rss-status-url mono" title="${escapeHTML(s.url || "")}">${escapeHTML(s.url || "（未填）")}</div>
      ${recentBlock}
    </div>`;
  }).join("");
}

function renderFeedStatusBadge(s) {
  if (!s.enabled) return '<span class="tag dim">已禁用</span>';
  if (s.fetching) return '<span class="tag busy">拉取中</span>';
  if (s.lastOk == null) return '<span class="tag dim">等待首次</span>';
  if (s.lastOk) return '<span class="tag ok">正常</span>';
  return '<span class="tag err">异常</span>';
}

function renderRecentItem(r) {
  const actionMap = {
    downloaded:       { label: "已提交", cls: "ok" },
    skipped_snapshot: { label: "快照跳过", cls: "dim" },
    skipped_no_rule:  { label: "未命中", cls: "dim" },
    failed:           { label: r.error || "失败", cls: "err" },
    seen_duplicate:   { label: "重复", cls: "dim" },
  };
  const a = actionMap[r.action] || { label: r.action || "", cls: "dim" };
  return `<div class="rss-recent-item">
    <span class="rss-recent-title">${escapeHTML(r.title)}</span>
    ${r.ruleName ? `<span class="rss-recent-rule">${escapeHTML(r.ruleName)}</span>` : ""}
    <span class="rss-recent-time">${fmtTimeAgo(r.processedAt)}</span>
    <span class="tag ${a.cls}">${escapeHTML(a.label)}</span>
  </div>`;
}

function collectRSS(rss) {
  rss.enabled = $("#rss-enabled").checked;
  rss.interval = parseInt($("#rss-interval").value || "15", 10);
  rss.feeds.forEach(f => {
    f.enabled = document.getElementById("f-" + f.id + "-enabled").checked;
    f.name = document.getElementById("f-" + f.id + "-name").value;
    f.url  = document.getElementById("f-" + f.id + "-url").value;
    f.rules = f.rules || [];
    f.rules.forEach((r, ri) => {
      const key = `${f.id}-${ri}`;
      r.enabled = document.querySelector(`[data-rule-enabled="${key}"]`).checked;
      r.name = document.querySelector(`[data-rule-name="${key}"]`).value;
      r.mode = document.querySelector(`[data-rule-mode="${key}"]`).value;
      r.include = document.querySelector(`[data-rule-include="${key}"]`).value;
      r.exclude = document.querySelector(`[data-rule-exclude="${key}"]`).value;
      r.savePath = document.querySelector(`[data-rule-path="${key}"]`).value;
      r.category = document.querySelector(`[data-rule-category="${key}"]`).value;
      r.tags = document.querySelector(`[data-rule-tags="${key}"]`).value;
      r.uploadLimit = parseInt(document.querySelector(`[data-rule-upload="${key}"]`).value || "0", 10);
      r.stopped = document.querySelector(`[data-rule-stopped="${key}"]`).checked;
    });
  });
  return rss;
}

async function saveRSS(rss) {
  collectRSS(rss);
  const cfg = (await api("GET", "/config")).data;
  cfg.rss = rss;
  const vmsg = validateConfigJS(cfg);
  if (vmsg) { toast("❌ " + vmsg, "err"); return; }
  const res = await api("POST", "/config", cfg);
  toast(res.success ? "RSS 配置已保存" : (res.message || "保存失败"), res.success ? "ok" : "err");
}

// ---------- 设置 ----------
// settings 页的当前配置对象（模块级，避免每次操作都重建页面丢值）
let _settingsCfg = null;

// 把当前 DOM 里 settings 页所有输入同步回 _settingsCfg
function syncFormToCfg() {
  if (!_settingsCfg) return;
  const c = _settingsCfg;
  c.qbittorrent.url = $("#qb-url").value;
  c.qbittorrent.username = $("#qb-user").value;
  // 掩码值 "********" 不覆盖原值；用户清空密码字段时原样保存（后端会保留原值）
  const pwd = $("#qb-pass").value;
  if (pwd && pwd !== "********") c.qbittorrent.password = pwd;
  const key = $("#qb-apikey").value;
  if (key && key !== "********") c.qbittorrent.apiKey = key;
  c.server.listen = $("#srv-listen").value;
  c.limiter.enabled = $("#lim-enabled").checked;
  c.limiter.interval = parseInt($("#lim-interval").value || "10", 10);
  c.limiter.rules = $$("[data-lm-name]").map(n => {
    const i = n.dataset.lmName;
    return {
      id: c.limiter.rules[i]?.id || genID(),
      enabled: document.querySelector(`[data-lm-enabled="${i}"]`).checked,
      name: n.value,
      match: document.querySelector(`[data-lm-match="${i}"]`).value,
      uploadLimit: parseInt(document.querySelector(`[data-lm-limit="${i}"]`).value || "0", 10),
    };
  });
  c.notifier.enabled = $("#nt-enabled").checked;
  // AppriseURLs：前端保持与后端返回的条目数一致（包含掩码值占位），
  // 掩码项原样传回去让后端按 index 保留真实值，非掩码值作为新值替换对应位置，
  // 空字符串表示用户删除该行。后端 saveConfig 会统一处理这三种情况。
  c.notifier.appriseUrls = $("#nt-urls").value.split("\n").map(s => s.trim());
  c.notifier.fields = $$("[data-nt-field]").filter(n => n.checked).map(n => n.dataset.ntField);
  c.fileManager.enabled = $("#fm-enabled").checked;
  c.fileManager.scanInterval = parseInt($("#fm-interval").value || "15", 10);
  if ($("#fm-clean-enabled")) {
    c.fileManager.cleanEnabled = $("#fm-clean-enabled").checked;
    c.fileManager.cleanRules = ($("#fm-clean-rules").value || "")
      .split("\n").map(s => s.trim()).filter(Boolean);
    c.fileManager.aiEnabled = $("#fm-ai-enabled").checked;
    c.fileManager.aiActive = $("#fm-ai-active").value;
    // AI 通道：掩码值 "********" 不覆盖原 apiKey（后端按 index 保留真实值）
    c.fileManager.aiChannels = $$("[data-ai-name]").map(n => {
      const i = n.dataset.aiName;
      const oldCh = (c.fileManager.aiChannels || [])[i] || {};
      const kv = document.querySelector(`[data-ai-key="${i}"]`).value;
      return {
        id: oldCh.id || genID(),
        enabled: document.querySelector(`[data-ai-enabled="${i}"]`).checked,
        name: n.value,
        baseURL: document.querySelector(`[data-ai-url="${i}"]`).value.trim(),
        apiKey: (kv && kv !== "********") ? kv : (oldCh.apiKey || ""),
        model: document.querySelector(`[data-ai-model="${i}"]`).value.trim(),
        prompt: document.querySelector(`[data-ai-prompt="${i}"]`).value,
      };
    });
  }
}

// 只重渲 limiter 规则那一块（加/删规则时调用，不碰整页）
function renderLimiterRules() {
  if (!_settingsCfg) return;
  const box = $("#limiter-rules");
  if (!box) return;
  const rules = _settingsCfg.limiter.rules || [];
  const count = $("#lim-count");
  if (count) count.textContent = `共 ${rules.length} 条`;
  box.innerHTML = rules.length === 0
    ? `<div class="item"><div class="empty">还没有规则，点上方「+ 添加规则」。</div></div>`
    : rules.map((r, i) => `
      <div class="item">
        <div class="item-head">
          <span class="idx">${i + 1}</span>
          <input type="text" class="name" data-lm-name="${i}" value="${escapeHTML(r.name || "")}" placeholder="规则名称" />
          ${toggle(r.enabled !== false, { data: `data-lm-enabled="${i}"`, sm: true })}
          <button class="btn small danger" data-lm-del="${i}">删除</button>
        </div>
        ${field("匹配正则",
          `<input type="text" class="ctrl mono" data-lm-match="${i}" value="${escapeHTML(r.match || "")}" placeholder="如 \\.mkv$ 或 4k" />`,
          "Go/RE2 语法，对任务名做部分匹配。")}
        ${field("上传限速",
          `<div class="controls"><input type="number" class="w-xs" data-lm-limit="${i}" value="${r.uploadLimit}" min="0"><span class="unit">KB/s</span></div>`,
          "0 表示不限制。")}
      </div>`).join("");
  // 重新绑定删除按钮（checkbox/input 的 change 不丢值，不需要重绑）
  $$("[data-lm-del]", box).forEach(b => b.addEventListener("click", onLimDel));
}

// 只重渲 AI 通道那一块（加/删通道时调用，不碰整页）
function renderAIChannels() {
  if (!_settingsCfg) return;
  const box = $("#ai-channels");
  if (!box) return;
  const chans = _settingsCfg.fileManager.aiChannels || [];
  const count = $("#ai-count");
  if (count) count.textContent = `共 ${chans.length} 个`;
  box.innerHTML = chans.length === 0
    ? `<div class="item"><div class="empty">还没有通道，点上方「+ 添加通道」。可添加本地（Ollama 等）或云端（OpenAI 兼容）通道。</div></div>`
    : chans.map((ch, i) => `
      <div class="item">
        <div class="item-head">
          <span class="idx">${i + 1}</span>
          <input type="text" class="name" data-ai-name="${i}" value="${escapeHTML(ch.name || "")}" placeholder="通道名称，如 本地 Ollama" />
          ${toggle(ch.enabled, { data: `data-ai-enabled="${i}"`, sm: true })}
          <button class="btn small danger" data-ai-del="${i}">删除</button>
        </div>
        ${field("BaseURL",
          `<input type="text" class="ctrl mono" data-ai-url="${i}" value="${escapeHTML(ch.baseURL || "")}" placeholder="http://localhost:11434/v1" />`,
          "任何 OpenAI 兼容接口均可，如本地 Ollama、DeepSeek、OpenAI。")}
        ${field("API Key",
          `<input type="password" class="ctrl mono" data-ai-key="${i}" value="${escapeHTML(ch.apiKey || "")}" placeholder="本地模型可留空" />`)}
        ${field("模型",
          `<input type="text" class="ctrl mono" data-ai-model="${i}" value="${escapeHTML(ch.model || "")}" placeholder="qwen2.5:7b / deepseek-chat / gpt-4o-mini" />`)}
        ${field("提示词",
          `<textarea class="ctrl" data-ai-prompt="${i}" placeholder="留空使用内置默认提示词。支持变量 {files}（文件名 JSON 数组）与 {torrent}（任务名），要求模型只输出 JSON 映射。">${escapeHTML(ch.prompt || "")}</textarea>`)}
      </div>`).join("");
  $$("[data-ai-del]", box).forEach(b => b.addEventListener("click", onAIDel));
  // 当前使用通道下拉
  const sel = $("#fm-ai-active");
  if (sel) {
    const cur = _settingsCfg.fileManager.aiActive;
    sel.innerHTML = chans.map(ch =>
      `<option value="${escapeHTML(ch.id)}" ${ch.id === cur ? "selected" : ""}>${escapeHTML(ch.name || ch.id)}${ch.enabled ? "" : "（未启用）"}</option>`
    ).join("") || `<option value="">（无通道）</option>`;
  }
}

function onAIDel() {
  syncFormToCfg();
  const idx = parseInt(this.dataset.aiDel, 10);
  const removed = _settingsCfg.fileManager.aiChannels.splice(idx, 1)[0];
  // 删除的是当前活动通道时重置选择
  if (removed && _settingsCfg.fileManager.aiActive === removed.id) {
    _settingsCfg.fileManager.aiActive = (_settingsCfg.fileManager.aiChannels.find(ch => ch.enabled) || {}).id || "";
  }
  renderAIChannels();
}

async function renderSettings(root) {
  const cfg = (await api("GET", "/config")).data;
  _settingsCfg = cfg;

  const sQbUrl    = escapeHTML(cfg.qbittorrent.url || "");
  const sQbUser   = escapeHTML(cfg.qbittorrent.username || "");
  const sQbPass   = escapeHTML(cfg.qbittorrent.password || "");
  const sQbKey    = escapeHTML(cfg.qbittorrent.apiKey || "");
  const sListen   = escapeHTML(cfg.server.listen || "");
  const sNotifyUrl = escapeHTML((cfg.notifier.appriseUrls || []).join("\n"));

  root.innerHTML = `
    <div class="page-head">
      <div class="titles">
        <h2>设置</h2>
        <p>所有配置保存在服务端配置文件中，保存后热重载，无需重启。</p>
      </div>
    </div>

    <!-- 外观 -->
    <div class="card">
      <div class="card-head">
        <h3>外观主题</h3>
        <span class="desc">只对当前浏览器生效，保存在本地，不影响其他设备</span>
      </div>
      ${field("主题模式", `
        <div class="theme-switch">
          <button data-theme-opt="auto" title="跟随系统">
            <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="2" y="4" width="20" height="14" rx="2"/><path d="M8 21h8M12 18v3"/></svg>
            <span>跟随系统</span>
          </button>
          <button data-theme-opt="dark" title="深色">
            <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/></svg>
            <span>深色</span>
          </button>
          <button data-theme-opt="light" title="浅色">
            <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>
            <span>浅色</span>
          </button>
        </div>`,
        "「跟随系统」按操作系统的深/浅色设置自动切换。")}
    </div>

    <!-- 连接 -->
    <div class="card">
      <div class="card-head">
        <h3>连接</h3>
        <span class="desc">QBHive 通过 qBittorrent WebUI API 读取与控制任务</span>
        <span class="spacer"></span>
        <div class="head-actions">
          <button class="btn small" id="qb-test">测试连接</button>
        </div>
      </div>
      ${field("WebUI 地址", `<input type="url" id="qb-url" class="ctrl mono" value="${sQbUrl}" placeholder="http://192.168.1.10:8080" />`)}
      ${field("用户名", `<input type="text" id="qb-user" class="w-md" value="${sQbUser}" autocomplete="username" />`)}
      ${field("密码", `<input type="password" id="qb-pass" class="w-md" value="${sQbPass}" placeholder="已隐藏，留空则不修改" autocomplete="current-password" />`)}
      ${field("API Key",
        `<input type="password" id="qb-apikey" class="ctrl mono" value="${sQbKey}" placeholder="已隐藏，留空则不修改" />`,
        "qBittorrent v5.2.0+ 可在 WebUI → 设置 → WebUI 里生成，形如 <code>qbt_xxx</code>。填写后优先使用，直接走 <code>Authorization: Bearer</code>，跳过用户名密码登录。")}

      <div class="section">
        <div class="section-title">QBHive 服务</div>
        ${field("监听地址", `<input type="text" id="srv-listen" class="w-md mono" value="${sListen}" placeholder=":8088" />`,
          "修改后需重启进程才会生效。")}
      </div>
    </div>

    <!-- 限速 -->
    <div class="card">
      <div class="card-head">
        <h3>限速规则</h3>
        <span class="desc">按任务名正则匹配，批量下发上传限速</span>
      </div>
      ${field("启用", toggle(cfg.limiter.enabled, { id: "lim-enabled" }),
        "只对限速值发生变化的任务调用 qB 接口，不会反复下发。")}
      ${field("刷新间隔",
        `<div class="controls"><input type="number" id="lim-interval" class="w-xs" value="${cfg.limiter.interval || 10}" min="1"><span class="unit">秒</span></div>`)}

      <div class="section">
        <div class="section-title">规则<span class="text-dim" id="lim-count"></span><span class="spacer"></span>
          <button class="btn small" id="lim-add">+ 添加规则</button>
        </div>
        <div id="limiter-rules"></div>
      </div>
    </div>

    <!-- 通知 -->
    <div class="card">
      <div class="card-head">
        <h3>完成通知</h3>
        <span class="desc">基于 Apprise-Go，原生支持 Telegram / Discord / Slack / 邮件 / Bark 等上百种渠道</span>
        <span class="spacer"></span>
        <div class="head-actions">
          <button class="btn small" id="nt-test">发送测试通知</button>
        </div>
      </div>
      ${field("启用", toggle(cfg.notifier.enabled, { id: "nt-enabled" }),
        "每 10 秒扫描一次完成事件，已通知记录持久化，重启不重发。")}
      ${field("通知渠道",
        `<textarea id="nt-urls" class="ctrl mono" placeholder="每行一个 Apprise URL，例如：&#10;telegram://BOT_TOKEN/CHAT_ID&#10;discord://WEBHOOK_ID/WEBHOOK_TOKEN&#10;gotify://TOKEN@HOST:PORT">${sNotifyUrl}</textarea>`,
        "格式参见 <a href=\"https://github.com/caronc/apprise/wiki\" target=\"_blank\">Apprise Wiki</a>，渠道用法见 <a href=\"https://github.com/unraid/apprise-go\" target=\"_blank\">Apprise-Go</a>。")}
      ${field("通知字段",
        `<div class="check-group">${renderNotifyFieldChecks(cfg.notifier.fields)}</div>`,
        "勾选通知正文包含的字段，至少一项；标题始终包含任务名。")}
    </div>

    <!-- 文件管理 -->
    <div class="card">
      <div class="card-head">
        <h3>文件管理</h3>
        <span class="desc">归档、文件名清洗与 AI 美化</span>
      </div>

      <div class="section">
        <div class="section-title">单文件自动归档</div>
        ${field("启用", toggle(cfg.fileManager.enabled, { id: "fm-enabled" }),
          "下载完成后，若 <code>savePath/torrentName/</code> 下只有一个文件，自动上移并清理空目录。")}
        ${field("扫描间隔",
          `<div class="controls"><input type="number" id="fm-interval" class="w-xs" value="${cfg.fileManager.scanInterval || 15}" min="1"><span class="unit">秒</span></div>`)}
      </div>

      <div class="section">
        <div class="section-title">文件名自动清理</div>
        ${field("启用", toggle(cfg.fileManager.cleanEnabled, { id: "fm-clean-enabled" }),
          "自动移除站点域名水印与 emoji 装饰，通过 qB 重命名接口执行，不影响做种。")}
        ${field("自定义正则",
          `<textarea id="fm-clean-rules" class="ctrl mono" placeholder="每行一个正则（Go/RE2 语法），匹配内容会被移除，例如：&#10;^【[^】]*】\\s*&#10;\\s*-\\s*4KHDR.*$">${escapeHTML((cfg.fileManager.cleanRules || []).join("\n"))}</textarea>`,
          "内置规则已覆盖 <code>www.xxx.com - </code> 前缀、<code>[xxx.com]</code> / <code>【xxx.com】</code> 括号、<code>@xxx.com</code> 后缀等常见水印；自定义规则在内置规则之后执行。清洗保证保留扩展名、结果非空、不含非法字符，所有改动记录在「文件」页并可回退。")}
      </div>

      <div class="section">
        <div class="section-title">AI 格式化文件名</div>
        ${field("启用", toggle(cfg.fileManager.aiEnabled, { id: "fm-ai-enabled" }),
          "在正则清洗之后执行，每个任务只调用一次（批量传入文件名）；失败自动重试 3 轮，仍失败则降级为仅正则清洗。")}
        ${field("使用通道", `<select id="fm-ai-active" class="ctrl"></select>`)}

        <div class="section-title">通道<span class="text-dim" id="ai-count"></span><span class="spacer"></span>
          <button class="btn small" id="ai-add">+ 添加通道</button>
        </div>
        <div id="ai-channels"></div>
      </div>
    </div>

    <!-- 保存 -->
    <div class="card save-bar">
      <span class="hint">保存前会先做一次本地校验（URL 协议、正则、间隔下限等）。</span>
      <span class="spacer"></span>
      <button class="btn primary" id="save-all">保存全部设置</button>
    </div>
  `;

  // 首次渲染 limiter 规则与 AI 通道
  renderLimiterRules();
  renderAIChannels();

  // --- 主题按钮高亮 + 事件 ---
  applyTheme(getTheme());
  $$(".theme-switch button", root).forEach(b => {
    b.addEventListener("click", () => setTheme(b.dataset.themeOpt));
  });

  // --- 事件 ---
  $("#qb-test").onclick = async () => {
    const r = await api("POST", "/test-qb", {
      url: $("#qb-url").value, username: $("#qb-user").value,
      password: $("#qb-pass").value, apiKey: $("#qb-apikey").value,
    });
    toast(r.success ? "连接成功 ✓" : (r.message || "连接失败"), r.success ? "ok" : "err");
    if (r.success) updateStatus("ok", "qb 已连接");
  };

  $("#lim-add").onclick = () => {
    syncFormToCfg();
    _settingsCfg.limiter.rules.push({ id: genID(), name: `规则 ${(_settingsCfg.limiter.rules.length + 1)}`, enabled: true, match: "", uploadLimit: 0 });
    renderLimiterRules();
  };

  $$("[data-lm-del]").forEach(b => b.addEventListener("click", onLimDel));

  $("#ai-add").onclick = () => {
    syncFormToCfg();
    const chans = _settingsCfg.fileManager.aiChannels;
    const ch = { id: genID(), name: `通道 ${chans.length + 1}`, baseURL: "", apiKey: "", model: "", prompt: "", enabled: true };
    chans.push(ch);
    _settingsCfg.fileManager.aiActive = ch.id;
    renderAIChannels();
  };

  $("#nt-test").onclick = async () => {
    syncFormToCfg();
    const urls = _settingsCfg.notifier.appriseUrls;
    if (urls.length === 0) { toast("请先填写通知 URL", "err"); return; }
    toast("正在发送测试通知...");
    const r = await api("POST", "/notify/test", { enabled: _settingsCfg.notifier.enabled, appriseUrls: urls });
    toast(r.message || (r.success ? "测试通知已发送 ✓" : "失败"), r.success ? "ok" : "err");
  };

  $("#save-all").onclick = async () => {
    syncFormToCfg();
    const out = _settingsCfg; // 掩码值已在 syncFormToCfg 里被忽略，后端会保留原密码/apiKey
    const vmsg = validateConfigJS(out);
    if (vmsg) { toast("❌ " + vmsg, "err"); return; }
    const res = await api("POST", "/config", out);
    toast(res.success ? "设置已保存" : (res.message || "保存失败"), res.success ? "ok" : "err");
    // 保存成功后重新 GET 一次，让密码/API key 回到掩码状态
    if (res.success) {
      const fresh = (await api("GET", "/config")).data;
      _settingsCfg = fresh;
    }
  };
}

function onLimDel() {
  syncFormToCfg();
  const idx = parseInt(this.dataset.lmDel, 10);
  _settingsCfg.limiter.rules.splice(idx, 1);
  renderLimiterRules();
}

// ---------- 文件（重命名审计与回退）----------
const _filesState = { page: 1, pageSize: 20 };

const AUDIT_STATUS = {
  committed:  { t: "已提交", c: "warn" },
  confirmed:  { t: "已生效", c: "ok" },
  failed:     { t: "失败",   c: "err" },
  rolledback: { t: "已回退", c: "dim" },
};

function fmtTime(ts) {
  if (!ts) return "-";
  const d = new Date(ts * 1000);
  const p = n => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

async function renderFiles(root, background = false) {
  root.innerHTML = `
    ${batchCard()}
    <div id="audit-card"><div class="empty">加载审计日志…</div></div>`;
  bindBatchCard(root);
  await renderAudit($("#audit-card", root));
}

// ---------- 批量重命名（模板）----------

const BR_VAR_HINT = "<code>{date}</code> 完成日期 · <code>{type}</code> 类型（video / audio / image / archive / doc / other） · <code>{index}</code> 任务内序号 · <code>{orig}</code> 原文件名 · <code>{title}</code> 任务名";

function batchCard() {
  return `
    <div class="page-head">
      <div class="titles">
        <h2>文件</h2>
        <p>按模板批量重命名，并查看、回退自动改名记录。</p>
      </div>
    </div>
    <div class="card">
      <div class="card-head">
        <h3>批量重命名</h3>
        <span class="desc">先预览再执行；目标同名时自动加后缀兜底，绝不覆盖</span>
        <span class="spacer"></span>
        <div class="head-actions">
          <button class="btn small" id="br-preview">预览</button>
          <button class="btn small primary" id="br-apply" disabled>执行重命名</button>
        </div>
      </div>
      ${field("任务", `<select id="br-torrent" class="ctrl"><option value="">加载中…</option></select>`)}
      ${field("模板",
        `<input type="text" id="br-template" class="ctrl mono" placeholder="{date}_{type}_{index}_{orig}" />`,
        `变量：${BR_VAR_HINT}。仅重命名文件名（扩展名保留），目录名不变；<code>{index}</code> 按任务文件列表从 1 起编，执行时按当前列表重新计算。`)}
      <div id="br-result"></div>
    </div>`;
}

async function bindBatchCard(root) {
  const sel = $("#br-torrent", root);
  const tplInput = $("#br-template", root);
  const resultBox = $("#br-result", root);
  const applyBtn = $("#br-apply", root);
  tplInput.value = localStorage.getItem("qbhive_batch_tpl") || "";
  try {
    const r = await api("GET", "/torrents?filter=all&limit=0");
    const list = r.data || [];
    sel.innerHTML = list.length === 0
      ? `<option value="">暂无任务</option>`
      : `<option value="">— 选择任务 —</option>` + list.map(t =>
          `<option value="${escapeHTML(t.hash)}">${escapeHTML(t.name)}</option>`).join("");
  } catch {
    sel.innerHTML = `<option value="">任务列表加载失败</option>`;
  }

  let lastPlan = null;
  $("#br-preview", root).onclick = async () => {
    const hash = sel.value, tpl = tplInput.value.trim();
    if (!hash) return toast("请先选择任务", "err");
    if (!tpl) return toast("请输入模板", "err");
    localStorage.setItem("qbhive_batch_tpl", tpl);
    resultBox.innerHTML = `<div class="empty">生成预览中…</div>`;
    const r = await api("POST", "/filemgr/batch/preview", { hash, template: tpl });
    if (!r.success) {
      lastPlan = null; applyBtn.disabled = true;
      resultBox.innerHTML = notice(`预览失败：${escapeHTML(r.message || "未知错误")}`, "danger");
      return;
    }
    lastPlan = r.data.plan || [];
    if (lastPlan.length === 0) {
      applyBtn.disabled = true;
      resultBox.innerHTML = `<div class="empty">没有需要重命名的文件（模板产出与现名一致，或任务无文件）。</div>`;
      return;
    }
    const rows = lastPlan.map(p => `
      <div class="cell" title="${escapeHTML(p.old)}">${escapeHTML(p.old)}</div>
      <div class="cell" title="${escapeHTML(p.new)}"><span class="arrow">→</span> <b>${escapeHTML(p.new)}</b></div>`).join("");
    resultBox.innerHTML = `
      <div class="section">
        <div class="section-title">预览结果<span class="text-dim">共 ${lastPlan.length} 条</span></div>
        <div class="rename-preview">
          <div class="rp-head">当前文件名</div>
          <div class="rp-head">新文件名</div>
          ${rows}
        </div>
      </div>`;
    applyBtn.disabled = false;
  };

  applyBtn.onclick = async () => {
    const hash = sel.value, tpl = tplInput.value.trim();
    if (!hash || !tpl) return;
    if (!lastPlan || lastPlan.length === 0) return toast("请先预览", "err");
    const ok = await confirmDialog("执行重命名",
      `确认按模板重命名 ${lastPlan.length} 个文件？\n执行时将按任务当前文件列表重新计算；改动会写入审计日志，可回退。`,
      { danger: false, okText: "执行" });
    if (!ok) return;
    applyBtn.disabled = true;
    const r = await api("POST", "/filemgr/batch/apply", { hash, template: tpl });
    toast(r.message || (r.success ? "已提交 ✓" : "执行失败"), r.success ? "ok" : "err");
    if (r.success) {
      lastPlan = null;
      resultBox.innerHTML = "";
      renderAudit($("#audit-card", root));
    } else {
      applyBtn.disabled = false;
    }
  };
}

async function renderAudit(root) {
  const offset = (_filesState.page - 1) * _filesState.pageSize;
  const r = await api("GET", `/filemgr/audit?limit=${_filesState.pageSize}&offset=${offset}`);
  if (!r.success) {
    root.innerHTML = notice(`加载审计日志失败：${escapeHTML(r.message || "")}`, "danger");
    return;
  }
  // 解构默认值只对 undefined 生效：后端无审计记录时 entries 会是 null（Go nil slice → JSON null），
  // 必须显式兜底，否则 null.length 在 Safari 报 "null is not an object"
  const data = r.data || {};
  const entries = Array.isArray(data.entries) ? data.entries : [];
  const total = Number.isFinite(data.total) ? data.total : 0;

  const rows = entries.map(e => {
    const st = AUDIT_STATUS[e.status] || { t: e.status, c: "dim" };
    const canRollback = e.status === "committed" || e.status === "confirmed";
    return `
      <div class="audit-row">
        <span class="cell time">${fmtTime(e.ts)}</span>
        <span class="cell" title="${escapeHTML(e.torrent)}">${escapeHTML(e.torrent)}</span>
        <span class="cell change" title="${escapeHTML(e.old)} → ${escapeHTML(e.new)}">
          ${escapeHTML(e.old)} <span class="arrow">→</span> <b>${escapeHTML(e.new)}</b>
        </span>
        <span class="cell via text-dim">${e.via === "ai" ? "AI" : "正则"}</span>
        <span class="status"><span class="tag ${st.c}">${st.t}</span></span>
        <span class="act">${canRollback
          ? `<button class="btn small" data-audit-rollback="${escapeHTML(e.id)}">回退</button>`
          : ""}</span>
      </div>`;
  }).join("");

  root.innerHTML = `
    <div class="card">
      <div class="card-head">
        <h3>文件名审计日志</h3>
        <span class="desc">自动与批量改名记录，保留最近 2000 条</span>
        <span class="spacer"></span>
        <div class="head-actions">
          <span class="desc">共 ${total} 条</span>
          <button class="btn small" id="audit-reload">刷新</button>
        </div>
      </div>
      ${entries.length === 0
        ? `<div class="empty">暂无重命名记录。启用「设置 → 文件管理 → 文件名自动清理」后，改动会记录在这里。</div>`
        : `<div class="audit-wrap">
             <div class="audit-list">
               <div class="audit-row head">
                 <span>时间</span><span>任务</span><span>文件名变更</span><span>方式</span><span>状态</span><span></span>
               </div>
               ${rows}
             </div>
           </div>
           ${pageNav(total, _filesState.page, _filesState.pageSize)}`}
    </div>`;

  bindPageNav(p => { _filesState.page = p; renderAudit(root); });
  $("#audit-reload", root).onclick = () => renderAudit(root);
  $$("[data-audit-rollback]", root).forEach(b => {
    b.onclick = async () => {
      const id = b.dataset.auditRollback;
      const ok = await confirmDialog("回退重命名", "文件名将恢复为修改前的名称。", { okText: "回退" });
      if (!ok) return;
      b.disabled = true;
      const res = await api("POST", "/filemgr/audit/rollback", { id });
      toast(res.message || (res.success ? "回退已提交 ✓" : "回退失败"), res.success ? "ok" : "err");
      renderAudit(root);
    };
  });
}

// ---------- 状态 ----------
async function updateStatus() {
  try {
    const r = await api("GET", "/torrents/stats");
    const el = $("#qb-status");
    if (r.success) {
      const s = r.data || {};
      el.textContent = "qb 已连接 · 活跃 " + (s.activeCount || 0) + " · 历史 " + (s.stoppedUP || 0);
      el.className = "status ok";
    } else {
      el.textContent = "qb 未连接"; el.className = "status bad";
    }
  } catch {
    const el = $("#qb-status");
    el.textContent = "服务异常"; el.className = "status bad";
  }
}


// ---------- 启动 ----------
// 初始页面：按 URL hash 决定（刷新不回首页）；hash 缺失/无效时用 replaceState 规范化，不产生多余历史
const _initView = hashToView();
if (location.hash !== "#/" + _initView) history.replaceState(null, "", "#/" + _initView);
switchView(_initView);
updateStatus();
setInterval(updateStatus, 30000);
// 概览/任务页自动刷新（防抖；间隔 8s；后台刷新不清空页面）
setInterval(async () => {
  if (_refreshRunning) return;
  if (currentView === "dashboard" || currentView === "torrents") {
    _refreshRunning = true;
    try { await switchView(currentView, true); } finally { _refreshRunning = false; }
  }
}, 8000);
