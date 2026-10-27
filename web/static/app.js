// QBHive 前端主脚本
const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

const API = window.location.origin + "/api";
const genID = () => Math.random().toString(36).slice(2, 10);

// ---------- 公共组件 ----------
// 所有控件结构都来自 ui.js（Hive Console 设计系统），app.js 只负责数据与事件。
const field = UI.field;
const toggle = UI.toggle;
const notice = UI.notice;

// 名单类输入的分隔符：与后端 postListSeparators 同一套（半角/全角逗号、顿号、
// 分号、竖线、换行都算），前端校验按它拆项，才不会把「动漫，剧集」当成一项拦下来
const LIST_SEP = /[,，、;；｜|\r\n]+/;

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

// 外部可控文本统一走设计系统的转义函数
const escapeHTML = UI.escape;

function toast(msg, type = "ok") {
  const el = document.createElement("div");
  el.className = `toast ${type === "err" ? "toast--err" : "toast--ok"}`;
  el.setAttribute("role", "status");
  el.innerHTML = `${UI.icon(type === "err" ? "alert" : "check-circle", 16)}<span>${escapeHTML(msg)}</span>`;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 2600);
}

// ---------- 弹窗：全站统一用这一套，取代原生 prompt / confirm ----------
function openModal(html) {
  const prevFocus = document.activeElement;
  const mask = document.createElement("div");
  mask.className = "modal-mask";
  mask.innerHTML = `<div class="modal" role="dialog" aria-modal="true">${html}</div>`;
  document.body.appendChild(mask);
  const focusables = () => $$("button, input, textarea, select, a[href]", mask)
    .filter(el => !el.disabled && el.offsetParent !== null);
  const onKey = (e) => {
    if (e.key === "Escape") { done(false); return; }
    if (e.key !== "Tab") return;
    const list = focusables();
    if (!list.length) return;
    const first = list[0], last = list[list.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  };
  function done(v) {
    document.removeEventListener("keydown", onKey);
    mask.remove();
    if (prevFocus && prevFocus.focus) prevFocus.focus();
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
      <button class="btn" data-no>取消</button>
      <button class="btn ${danger ? "btn--danger" : "btn--primary"}" data-yes>${escapeHTML(okText)}</button>
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
      <button class="btn" data-no>取消</button>
      <button class="btn btn--primary" data-ok>确定</button>
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
// downloadText：把内存里的文本存成文件（日志导出用），用完立刻释放 blob URL
function downloadText(filename, text) {
  const url = URL.createObjectURL(new Blob([text], { type: "text/plain;charset=utf-8" }));
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 0);
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
  const btn = (label, target, primary = false) =>
    UI.btn(label, { size: "sm", variant: primary ? "primary" : "", attrs: `data-p="${target}"${target === cur ? " disabled" : ""}` });
  const parts = [btn("«", 1), btn("‹", cur - 1)];
  // 省略号逻辑：只显示前3、后3、当前页附近1页
  const windowSet = new Set([1, 2, 3, pages - 2, pages - 1, pages, cur - 1, cur, cur + 1].filter(x => x >= 1 && x <= pages));
  let last = 0;
  for (const n of [...windowSet].sort((a, b) => a - b)) {
    if (n > last + 1) parts.push(`<span class="text-dim">…</span>`);
    parts.push(btn(String(n), n, n === cur));
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
        <button class="btn" data-no>取消</button>
        <button class="btn btn--primary" data-ok>登录</button>
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
  // 代理配置
  if (c.proxy) {
    const pmodes = ["system", "manual", "off", ""];
    if (!pmodes.includes(c.proxy.mode)) return "代理模式非法";
    const purl = (c.proxy.url || "").trim();
    if (purl && !["http://", "https://", "socks5://", "socks5h://"].some(s => purl.startsWith(s))) {
      return "代理地址需以 http:// https:// socks5:// 或 socks5h:// 开头";
    }
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
      // 正则语法一律交给后端校验：规则按 Go/RE2 语法编写（例如 (?i) 内联标志），
      // 浏览器 RegExp 不认这类写法，在这里拦截会把合法规则误判成"正则无效"。
      // 后端 validateConfig 用 regexp.Compile 编译，非法时保存请求返回具体原因。
    }
  }
  // Limiter rules
  for (const r of ((c.limiter && c.limiter.rules) || [])) {
    // match 正则的语法校验同样交给后端 regexp.Compile（支持 (?i) 等 RE2 写法）
    if ((r.ratioTarget || 0) > 0 && !(r.uploadLimit > 0)) {
      return `限速规则 "${r.name || ""}" 启用了比率目标，上传限速基准必须大于 0：不限速没有可压降的基准值`;
    }
  }
  // 文件管理：清理自定义正则 + AI 通道
  if (c.fileManager) {
    const fm = c.fileManager;
    const rules = fm.cleanRules || [];
    if (rules.length > 32) return "文件名清理自定义规则不能超过 32 条";
    for (let i = 0; i < rules.length; i++) {
      if ((rules[i] || "").length > 256) return `文件名清理规则 ${i + 1} 长度超过 256 字符`;
    }
    // 正则语法不在这里校验：清理规则用 Go/RE2 语法，(?i) 等内联标志浏览器
    // RegExp 不认，前端拦截会把合法规则误判成"正则无效"。后端
    // validateConfig 用 regexp.Compile 编译，非法时保存请求返回具体原因。
    const pp = fm.postProcess || {};
    if (pp.enabled) {
      if (!((pp.command || "").trim())) return "后处理已启用，请填写要执行的命令";
      if (pp.timeout < 0) return "后处理超时不能为负数";
      for (const e of (pp.extensions || "").split(LIST_SEP)) {
        const t = e.trim();
        if (t && !/^[A-Za-z0-9]+$/.test(t)) return `后处理扩展名白名单含非法项：${t}`;
      }
      for (const [label, list] of [
        ["分类包含", pp.categoryInclude], ["分类排除", pp.categoryExclude],
        ["标签包含", pp.tagInclude], ["标签排除", pp.tagExclude],
      ]) {
        const v = list || "";
        if (v.length > 256) return `后处理${label}名单不能超过 256 字符`;
        if (v.split(LIST_SEP).filter(t => t.trim()).length > 32) return `后处理${label}名单不能超过 32 项`;
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
  logs: renderLogs,
  settings: renderSettings,
};

// 导航只有一份定义：桌面侧栏与移动底栏共用同一组件
const NAV_ITEMS = [
  { view: "dashboard", label: "概览", iconName: "overview", group: "工作区" },
  { view: "torrents", label: "任务", iconName: "torrents" },
  { view: "files", label: "文件", iconName: "files" },
  { view: "rss", label: "RSS", iconName: "rss", group: "自动化" },
  { view: "logs", label: "日志", iconName: "logs", group: "系统" },
  { view: "settings", label: "设置", iconName: "settings" },
];
$("#nav-desktop").innerHTML = UI.navList(NAV_ITEMS);
$("#nav-mobile").innerHTML = UI.navList(NAV_ITEMS, { mobile: true });

// 从 location.hash 解析目标视图（#/torrents → torrents）；无效 hash 一律回落到概览
function hashToView() {
  const m = /^#\/([A-Za-z]+)/.exec(location.hash || "");
  return m && views[m[1]] ? m[1] : "dashboard";
}

let currentView = "dashboard";

// ---------- 未保存守卫 ----------
// RSS 与设置两页都是「编辑本地对象 → 手动点保存」，而切换视图原本直接清空 DOM：
// 用户填完一整页配置、点一下标签栏就全没了，且没有任何提示。
// 这里统一标脏 → 离开前拦截 → 关闭/刷新前拦截。
const VIEW_LABEL = { rss: "RSS 订阅", settings: "设置" };
const _dirty = { rss: false, settings: false };
// 上次「与服务端一致」时的表单快照。判断是否脏必须靠快照比对，不能只看事件：
// 文本框一旦被改过，之后任意一次失焦都会再补发一次 change，纯事件置位会在
// 「保存成功后点标签栏」这个最常见的动作上误报「有未保存的修改」。
const _savedSnap = { rss: "", settings: "" };

// 把当前页所有表单控件的值拼成一个可比较的字符串
function formSnapshot() {
  const parts = [];
  $$("#content input, #content textarea, #content select").forEach((el, i) => {
    let v;
    if (el.type === "checkbox" || el.type === "radio") v = el.checked ? "1" : "0";
    else if (el.type === "file") v = "";
    else v = el.value;
    parts.push(i + ":" + v);
  });
  return parts.join("\u0001");
}

// 显式标脏：程序化增删规则/通道不触发 input 事件，由处理函数自己调用
function markDirty(view) {
  if (!VIEW_LABEL[view]) return;
  _dirty[view] = true;
  syncDirtyUI();
}

// 按快照重新计算脏状态（表单改动触发）
function refreshDirty(view) {
  if (!VIEW_LABEL[view]) return;
  _dirty[view] = formSnapshot() !== _savedSnap[view];
  syncDirtyUI();
}

// 与服务端对齐：保存成功或刚从服务端载入时调用
function clearDirty(view) {
  if (!VIEW_LABEL[view]) return;
  _savedSnap[view] = formSnapshot();
  _dirty[view] = false;
  syncDirtyUI();
}

// 未保存提示：导航项与保存按钮上各挂一个小圆点
function syncDirtyUI() {
  Object.keys(VIEW_LABEL).forEach(v => {
    document.querySelectorAll(`.nav-item[data-view="${v}"]`).forEach(el => el.classList.toggle("dirty", _dirty[v]));
    document.querySelectorAll(`[data-save="${v}"]`).forEach(b => b.classList.toggle("unsaved", _dirty[v]));
  });
}

// 离开当前视图前的确认。返回 true 表示允许离开。
async function guardUnsaved(next) {
  if (!_dirty[currentView]) return true;
  const staying = next === currentView;
  const ok = await confirmDialog("有未保存的修改",
    `「${VIEW_LABEL[currentView]}」页有尚未保存的修改，${staying ? "重新加载" : "离开"}后会丢失。`,
    { danger: true, okText: "放弃修改" });
  if (!ok) return false;
  _dirty[currentView] = false;
  syncDirtyUI();
  return true;
}

async function switchView(name, background = false) {
  if (!views[name]) name = "dashboard";
  // 后台自动刷新是同一视图内的数据更新，不触发守卫
  if (!background && !(await guardUnsaved(name))) {
    // 守卫拒绝了切换：若 hash 已变则还原，避免地址栏与页面内容不一致
    if (location.hash === "#/" + name) history.replaceState(null, "", "#/" + currentView);
    return;
  }
  currentView = name;
  // hash 路由：把 URL 同步成当前页，刷新/分享链接都能停留在原页面；
  // 后台自动刷新（background）不改 URL。赋值会触发 hashchange，
  // 监听器里目标与 currentView 相同则忽略，不会重复渲染。
  if (!background && location.hash !== "#/" + name) {
    location.hash = "#/" + name;
  }
  $$(".nav-item").forEach(b => {
    const active = b.dataset.view === name;
    b.classList.toggle("active", active);
    if (active) b.setAttribute("aria-current", "page");
    else b.removeAttribute("aria-current");
  });
  const root = $("#content");
  root.dataset.view = name;
  // 后台刷新：不清空页面，保留旧数据，只在渲染函数内部更新
  if (!background) {
    root.innerHTML = UI.loading();
  }
  try {
    await views[name](root, background);
  } catch (e) {
    root.innerHTML = notice(`页面渲染出错：${escapeHTML(e.message || String(e))}`, "danger");
  }
}

$$(".nav-item").forEach(b => b.addEventListener("click", () => switchView(b.dataset.view)));

// 表单一改动就重新比对快照（覆盖输入、下拉、开关）。程序化改动（增删规则/通道）
// 不触发事件，由各处理函数显式调用 markDirty。
["input", "change"].forEach(evt => {
  document.addEventListener(evt, (e) => {
    if (!VIEW_LABEL[currentView]) return;
    if (e.target && e.target.closest && e.target.closest("#content")) refreshDirty(currentView);
  });
});

// 关闭标签页 / 刷新前的最后一道拦截
window.addEventListener("beforeunload", (e) => {
  if (_dirty.rss || _dirty.settings) {
    e.preventDefault();
    e.returnValue = "";
  }
});

// 浏览器前进/后退、手动改 hash 时切换到对应页面
window.addEventListener("hashchange", () => {
  const v = hashToView();
  if (v !== currentView) switchView(v);
});

// 自适应高度文本域（.auto-h）：内容变化即时撑高，超过 CSS max-height 后内部滚动。
// 用 document 级事件委托，动态重渲染出的文本域无需重新绑定。
document.addEventListener("input", (e) => {
  if (e.target && e.target.classList && e.target.classList.contains("auto-h")) autosize(e.target);
});

// ---------- 主题 ----------
// 三种模式：auto（跟随系统）、dark、light；持久化到 localStorage
const THEME_KEY = "qbhive-theme";

function applyTheme(mode) {
  document.documentElement.setAttribute("data-theme", mode);
  // 页面上任何主题分段控件都同步高亮
  document.querySelectorAll(".theme-switch [data-theme-opt]").forEach(b => {
    b.classList.toggle("is-active", b.dataset.themeOpt === mode);
  });
  syncThemeColor();
}

// 移动端地址栏 / 状态栏跟随当前主题底色（--canvas 已按 auto 模式解析成实际色值）
function syncThemeColor() {
  const bg = getComputedStyle(document.documentElement).getPropertyValue("--canvas").trim();
  if (!bg) return;
  let meta = document.querySelector('meta[name="theme-color"]');
  if (!meta) {
    meta = document.createElement("meta");
    meta.name = "theme-color";
    document.head.appendChild(meta);
  }
  meta.content = bg;
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
  const skeletonStats = Array.from({ length: 6 }, () => UI.stat({ label: "加载中", value: "–" })).join("");
  root.innerHTML = `
    ${UI.page({
      eyebrow: "Overview",
      title: "概览",
      desc: "下载与做种的实时状态，每 8 秒自动刷新。",
      actions: UI.btn("刷新", { size: "sm", iconName: "refresh", id: "dash-refresh" }),
    })}
    <div class="stat-grid" id="dash-stats">${skeletonStats}</div>
    ${UI.panel({
      title: "实时吞吐",
      desc: "全局上下行速度与限速占用",
      iconName: "activity",
      className: "throughput-panel",
      body: `<div class="throughput" id="dash-speed">${UI.loading()}</div>`,
    })}
    ${UI.panel({
      title: "最近活跃任务",
      desc: "按添加时间倒序 · 最多 10 条",
      iconName: "torrents",
      flush: true,
      actions: UI.btn("查看全部", { size: "sm", variant: "ghost", iconName: "arrow", attrs: 'data-goto="torrents"' }),
      body: `<div id="dash-latest">${UI.loading()}</div>`,
    })}
  `;
  $("#dash-refresh").onclick = () => refreshDashboard(root);
  $$("[data-goto]", root).forEach(b => b.onclick = () => switchView(b.dataset.goto));
  await refreshDashboard(root);
}

async function refreshDashboard(root) {
  const statsEl = root.querySelector("#dash-stats");
  const speedEl = root.querySelector("#dash-speed");
  if (statsEl) statsEl.classList.add("is-refreshing");
  if (speedEl) speedEl.classList.add("is-refreshing");

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

  const speedCard = (kind, cur, limit) => {
    const pct = limit > 0 ? Math.min(100, Math.round(cur / limit * 100)) : 0;
    const label = kind === "down" ? "下载" : "上传";
    return `<div class="speed-card speed-card--${kind}">
      <div class="speed-top">
        <span class="speed-kind">${UI.icon(kind === "down" ? "download" : "upload", 16)}${label}</span>
        <span>${limit > 0 ? `限速 ${humanSpeed(limit)}` : "不限速"}</span>
      </div>
      <div class="speed-value">${humanSpeed(cur)}</div>
      ${UI.meter(pct, kind)}
      <div class="speed-foot">${limit > 0 ? `已用 ${pct}% 限速` : "全局未设置限速"}</div>
    </div>`;
  };

  if (statsEl) {
    const statDefs = [
      { label: "下载中", value: downloading, meta: "正在传输", tone: downloading > 0 ? "down" : "muted", iconName: "download" },
      { label: "做种中", value: seeding, meta: "有上传速度", tone: seeding > 0 ? "up" : "muted", iconName: "upload" },
      { label: "停滞", value: stalled, meta: "无对端活动", tone: stalled > 0 ? "warn" : "muted", iconName: "clock" },
      { label: "停止未完成", value: stoppedDl, meta: "等待继续", tone: stoppedDl > 0 ? "accent" : "muted", iconName: "pause" },
      { label: "已完成", value: stoppedUp, meta: "停止做种", tone: stoppedUp > 0 ? "up" : "muted", iconName: "check-circle" },
      { label: "异常", value: errored, meta: "需要处理", tone: errored > 0 ? "bad" : "muted", iconName: "alert" },
    ];
    statsEl.innerHTML = statDefs.map(d => UI.stat(d)).join("");
    statsEl.classList.remove("is-refreshing");
  }
  if (speedEl) {
    speedEl.innerHTML = speedCard("down", dlSpeed, dlLimit) + speedCard("up", upSpeed, upLimit);
    speedEl.classList.remove("is-refreshing");
  }

  // 任务列表：已与 stats 并发获取，到了再替换
  const list = top10.data || [];
  const slot = root.querySelector("#dash-latest");
  if (slot) {
    slot.innerHTML = list.length === 0
      ? UI.empty("当前没有活跃任务", { iconName: "check-circle" })
      : torrentTable(list, false);
  }
}


// 任务状态 → 短标签 + 语义色档（与全站 tag 共用一套色板）
const STATE_TAGS = {
  downloading:   { t: "下载",   c: "down" },
  forcedDL:      { t: "下载",   c: "down" },
  stalledDL:     { t: "停滞",   c: "warn" },
  stoppedDL:     { t: "停止",   c: "muted" },
  stoppedUP:     { t: "已完成", c: "good" },
  stalledUP:     { t: "做种",   c: "warn" },
  uploading:     { t: "做种",   c: "up" },
  forcedUP:      { t: "做种",   c: "up" },
  queuedUP:      { t: "排队",   c: "muted" },
  queuedDL:      { t: "排队",   c: "muted" },
  checkingUP:    { t: "校验",   c: "violet" },
  checkingDL:    { t: "校验",   c: "violet" },
  metaDL:        { t: "元数据", c: "violet" },
  forcedMetaDL:  { t: "元数据", c: "violet" },
  moving:        { t: "移动",   c: "violet" },
  checkingResumeData: { t: "恢复", c: "violet" },
  missingFiles:  { t: "缺文件", c: "bad" },
  error:         { t: "错误",   c: "bad" },
};

function stateTag(s) {
  const v = STATE_TAGS[s] || { t: s, c: "muted" };
  return UI.tag(escapeHTML(v.t), v.c, { dot: true });
}

// 是否处于「可恢复」的停止态：stopped*/paused* 都算，missingFiles/error 不算
function isStoppedState(s) {
  return /^(stopped|paused)/.test(s || "");
}

function torrentTable(list, withActions) {
  if (!list || list.length === 0) return UI.empty("暂无任务", { iconName: "torrents" });
  const head = [
    { label: "任务", cls: "data-cell--name" },
    { label: "状态", cls: "data-cell--state" },
    { label: "进度", cls: "data-cell--progress" },
    { label: "大小", cls: "data-cell--size" },
    { label: "下载", cls: "data-cell--speed" },
    { label: "上传", cls: "data-cell--speed" },
    { label: "分类", cls: "data-cell--cat" },
  ];
  if (withActions) head.push({ label: "操作", cls: "data-cell--actions" });
  return UI.table({
    head,
    rows: list.map(t => torrentRow(t, withActions)),
    className: `table--torrents${withActions ? "" : " table--no-actions"}`,
  });
}

function torrentRow(t, withActions) {
  const pctNum = Math.max(0, Math.min(100, (t.progress || 0) * 100));
  const pct = pctNum.toFixed(1);
  const safeName = escapeHTML(t.name);
  const safeCat = escapeHTML(t.category || "-");
  const safeHash = escapeHTML(t.hash);
  const stopped = isStoppedState(t.state);
  return `<div class="data-row torrent-row">
    <div class="data-cell data-cell--name" title="${safeName}">
      <div class="torrent-name">${safeName}</div>
    </div>
    <div class="data-cell data-cell--state">${stateTag(t.state)}</div>
    <div class="data-cell data-cell--progress">
      <div class="torrent-progress">
        ${UI.meter(pctNum, pctNum >= 100 ? "up" : "down")}
        <span class="progress-text">${pct}%</span>
      </div>
    </div>
    <div class="data-cell data-cell--size">${humanSize(t.downloaded)} <span class="size-total">/ ${humanSize(t.size)}</span></div>
    <div class="data-cell data-cell--speed speed-dl${t.dlspeed ? "" : " is-idle"}">${humanSpeed(t.dlspeed)}</div>
    <div class="data-cell data-cell--speed speed-up${t.upspeed ? "" : " is-idle"}">${humanSpeed(t.upspeed)}</div>
    <div class="data-cell data-cell--cat" title="${safeCat}">${safeCat}</div>
    ${withActions ? `<div class="data-cell data-cell--actions row-actions">
      ${UI.iconBtn(stopped ? "play" : "pause", { title: stopped ? "继续任务" : "暂停任务", attrs: `data-torrent-action="${safeHash}" data-act="${stopped ? "resume" : "pause"}"` })}
      ${UI.iconBtn("refresh", { title: "强制校验", attrs: `data-torrent-recheck="${safeHash}"` })}
      ${UI.iconBtn("edit", { title: "修改分类与标签", attrs: `data-torrent-meta="${safeHash}"` })}
      ${UI.iconBtn("gauge", { title: "上传限速", attrs: `data-limit-btn="${safeHash}"` })}
      ${UI.iconBtn("trash", { title: "移除任务", variant: "danger", attrs: `data-torrent-del="${safeHash}" data-name="${safeName}"` })}
    </div>` : ""}
  </div>`;
}

// 任务行内操作（暂停/继续、强制校验、改分类、限速、删除）。
// 一次绑定全部，避免列表重渲后漏绑某个按钮。
function bindTorrentRowActions() {
  $$("[data-limit-btn]").forEach(btn => {
    if (btn.__bound) return; btn.__bound = true;
    btn.addEventListener("click", () => openLimitDialog(btn.dataset.limitBtn));
  });
  $$("[data-torrent-action]").forEach(btn => {
    if (btn.__bound) return; btn.__bound = true;
    btn.addEventListener("click", () => runTorrentAction(btn.dataset.torrentAction, btn.dataset.act));
  });
  $$("[data-torrent-recheck]").forEach(btn => {
    if (btn.__bound) return; btn.__bound = true;
    btn.addEventListener("click", () => runTorrentAction(btn.dataset.torrentRecheck, "recheck"));
  });
  $$("[data-torrent-meta]").forEach(btn => {
    if (btn.__bound) return; btn.__bound = true;
    btn.addEventListener("click", () => openMetaDialog(btn.dataset.torrentMeta));
  });
  $$("[data-torrent-del]").forEach(btn => {
    if (btn.__bound) return; btn.__bound = true;
    btn.addEventListener("click", () => openDeleteTorrentDialog(btn.dataset.torrentDel, btn.dataset.name || ""));
  });
}

// 单任务操作：pause / resume / recheck
async function runTorrentAction(hash, action) {
  const labels = { pause: "已暂停", resume: "已恢复", recheck: "已开始校验" };
  const res = await api("POST", `/torrents/${encodeURIComponent(hash)}/action`, { action });
  toast(res.success ? (res.message || labels[action] || "已执行") : (res.message || "操作失败"),
    res.success ? "ok" : "err");
  if (res.success) fetchTorrents();
}

// 改分类 / 标签：只提交用户在弹窗里真正改动的字段
async function openMetaDialog(hash) {
  const t = (_torrentsState.rawList || []).find(x => x.hash === hash) || {};
  const cat = await inputDialog("修改分类", "留空表示取消分类。只改分类，不移动文件。",
    { value: t.category || "", unit: "", type: "text", placeholder: "如 电影" });
  if (cat === null) return;
  const tags = await inputDialog("修改标签", "多个标签用英文逗号分隔；留空表示清空标签。",
    { value: t.tags || "", unit: "", type: "text", placeholder: "如 国语,4K" });
  if (tags === null) return;
  const body = {};
  if ((cat || "") !== (t.category || "")) body.category = cat;
  if ((tags || "") !== (t.tags || "")) body.tags = tags;
  if (Object.keys(body).length === 0) { toast("没有改动", "ok"); return; }
  const res = await api("POST", `/torrents/${encodeURIComponent(hash)}/meta`, body);
  toast(res.success ? "已更新" : (res.message || "更新失败"), res.success ? "ok" : "err");
  if (res.success) fetchTorrents();
}

// 删除任务：默认只从 qB 移除、保留磁盘文件；勾选后才连文件一起删（不可逆）
function openDeleteTorrentDialog(hash, name) {
  const { mask, done, result } = openModal(`
    <h3>移除任务</h3>
    <p>${escapeHTML(name || hash)}<br>仅从 qBittorrent 移除任务记录，磁盘文件保留。</p>
    <label class="check-row">
      <input type="checkbox" data-del-files>
      <span>同时删除磁盘文件（不可逆，无法回退）</span>
    </label>
    <div class="modal-actions">
      <button class="btn" data-no>取消</button>
      <button class="btn btn--danger" data-yes>移除</button>
    </div>`);
  const cb = mask.querySelector("[data-del-files]");
  // 勾选删除文件时按钮文案同步变红更醒目，避免用户误以为只是移除任务
  cb.addEventListener("change", () => {
    mask.querySelector("[data-yes]").textContent = cb.checked ? "删除任务和文件" : "移除";
  });
  mask.querySelector("[data-yes]").onclick = () => done({ ok: true, deleteFiles: cb.checked });
  mask.querySelector("[data-no]").onclick = () => done({ ok: false });
  setTimeout(() => mask.querySelector("[data-no]").focus(), 0);
  return result.then(async (r) => {
    if (!r || !r.ok) return;
    const q = r.deleteFiles ? "?deleteFiles=true" : "";
    const res = await api("DELETE", `/torrents/${encodeURIComponent(hash)}${q}`);
    toast(res.success ? (res.message || "已移除") : (res.message || "删除失败"), res.success ? "ok" : "err");
    if (res.success) fetchTorrents();
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
    ${UI.page({
      eyebrow: "Transfers",
      title: "任务",
      desc: "按状态筛选、搜索与分页；每行可直接暂停/恢复、强制校验、改分类与标签、改上传限速或移除任务。",
      actions: UI.btn("刷新", { size: "sm", iconName: "refresh", id: "tf-reload" }),
    })}
    ${UI.panel({
      title: "任务列表",
      desc: "行内操作即时下发到 qBittorrent",
      iconName: "torrents",
      actions: `<span class="meta-text" id="tf-progress"></span>`,
      body: `
        ${UI.toolbar([
          UI.toolField("状态", `<select id="tf-filter" class="control">
            ${torrentFilterOptions.map(o =>
              `<option value="${o.value}" ${o.value===_torrentsState.filter?"selected":""}>${o.label}</option>`
            ).join("")}
          </select>`),
          UI.toolField("数量", `<select id="tf-limit" class="control w-sm">
            ${[100, 200, 500, 1000, 0].map(n =>
              `<option value="${n}" ${n===_torrentsState.limit?"selected":""}>${n===0?"全量":n}</option>`
            ).join("")}
          </select>`),
          UI.toolField("搜索", `<input type="text" id="tf-search" class="control" value="${escapeHTML(_torrentsState.search)}" placeholder="任务名 / 分类 / 标签" />`, { grow: true }),
        ])}
        <div class="notice-slot" id="tf-notice"></div>
        <div id="tf-body">${UI.loading()}</div>
      `,
    })}
  `;

  // 全量模式警告
  if (_torrentsState.filter === "all" || _torrentsState.filter === "completed") {
    $("#tf-notice").innerHTML = notice(
      _torrentsState.filter === "all"
        ? "全量模式可能加载数千条任务，首次请求较慢；后端返回完整列表后再前端分页。"
        : "「已完成」包含停止、做种、停滞三类，数量可能很多；建议改用「已完成历史（停止）」。",
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
    body.innerHTML = UI.empty(_torrentsState.search ? "搜索无匹配" : "当前没有任务", { iconName: _torrentsState.search ? "search" : "torrents" });
    return;
  }

  body.innerHTML = torrentTable(pageSlice, true) + pageNav(list.length, page, _torrentsState.pageSize);
  bindPageNav(p => { _torrentsState.page = p; renderPage(); });
  bindTorrentRowActions();
}

// ---------- RSS ----------
// rss 参数可传：首次进入页面或显式传 undefined 时从服务器拉；
// 新增/删除/改规则后传本地已修改的 rss，避免覆盖内存中的改动。
let _rssStatusTimer = null;

// autosize：把 .auto-h 文本域高度撑到内容高度，封顶到 CSS 的 max-height（超出内部滚动）。
// <textarea> 是替换元素，纯 CSS 无法按内容增高，必须 JS 设 height。
function autosize(t) {
  t.style.height = "auto";
  const cs = getComputedStyle(t);
  const min = parseFloat(cs.minHeight) || 0;
  const max = parseFloat(cs.maxHeight) || 320;
  // +1：scrollHeight 向下取整，补足舍入误差，避免最后一行差 1px 触发内滚动
  t.style.height = Math.min(Math.max(t.scrollHeight + 1, min), max) + "px";
}
function autosizeAll(root) {
  (root || document).querySelectorAll("textarea.auto-h").forEach(autosize);
}

async function renderRSS(root, rss) {
  // 第二个参数可能是 switchView 传入的 background 布尔，也可能是内部重渲传入的 rss 配置对象；
  // 只有真正的对象才复用，其余一律从 /config 重新拉取（并兜底请求失败）
  let freshLoad = false;
  if (!rss || typeof rss !== "object") {
    const cr = await api("GET", "/config");
    if (!cr.success || !cr.data) {
      root.innerHTML = `
        ${UI.page({ eyebrow: "Automation", title: "RSS 订阅" })}
        ${UI.panel({ body: notice(`载入配置失败：${escapeHTML(cr.message || "未知错误")}`, "danger") })}
      `;
      return;
    }
    rss = cr.data.rss || {};
    freshLoad = true;
  }
  if (!rss.feeds) rss.feeds = [];

  root.innerHTML = `
    ${UI.page({
      eyebrow: "Automation",
      title: "RSS 订阅",
      desc: "定时拉取订阅源，按规则匹配条目并自动提交下载；条目去重持久化，重启不重复下载。",
      actions: UI.btn("添加订阅源", { size: "sm", variant: "primary", iconName: "plus", id: "rss-add" }),
    })}

    ${UI.panel({
      title: "全局设置",
      desc: "订阅源与规则的总体开关",
      iconName: "rss",
      actions: `${UI.btn("立即拉取", { size: "sm", iconName: "refresh", id: "rss-force" })}${UI.btn("全部重新匹配", { size: "sm", variant: "ghost", iconName: "shield", id: "rss-reset-all" })}`,
      body: `
        ${field("启用", toggle(rss.enabled, { id: "rss-enabled" }), "关闭后不再拉取任何订阅源，已提交的下载不受影响。")}
        ${field("刷新间隔",
          `<div class="controls"><input type="number" id="rss-interval" class="w-xs" min="1" value="${rss.interval || 15}"><span class="unit">分钟</span></div>`,
          "到点后逐个订阅源拉取。")}
      `,
    })}

    ${rss.feeds.length === 0
      ? UI.panel({ body: UI.empty("还没有订阅源，点右上角「添加订阅源」开始。", { iconName: "rss" }) })
      : rss.feeds.map((f, fi) => renderFeedBlock(f, fi)).join("")}

    ${UI.panel({
      title: "运行状态",
      desc: "每 5 秒自动刷新，记录最近一次拉取与命中情况",
      iconName: "activity",
      body: `<div id="rss-status-panel">${UI.loading()}</div>`,
    })}

    ${UI.saveBar(
      "订阅源与规则的改动保存后才生效；匹配规则发生变化时会自动对历史条目重新匹配。",
      UI.btn("保存", { variant: "primary", iconName: "save", id: "rss-save", attrs: 'data-save="rss"' })
    )}
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
    markDirty("rss"); // 增删不触发 input 事件，需显式标脏
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
  // 刚从服务端载入：把当前 DOM 记成基准快照，避免把「加载」误判成「有改动」
  if (freshLoad) clearDirty("rss");
  else syncDirtyUI(); // 重渲后重建了保存按钮，未保存标记要重新贴一遍
  autosizeAll(root); // 初始渲染：按已填内容撑高 include/exclude，长规则不再被截断

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
  <section class="panel feed-card">
    <div class="feed-head">
      <span class="index-badge">${fi + 1}</span>
      <input type="text" class="control feed-name" id="f-${f.id}-name" value="${sName}" placeholder="订阅源名称" />
      ${toggle(f.enabled, { id: `f-${f.id}-enabled`, sm: true, label: "启用" })}
      ${UI.iconBtn("trash", { title: "删除订阅源", variant: "danger", attrs: `data-del-feed="${f.id}"` })}
    </div>
    <div class="feed-body">
      ${field("订阅地址", `<input type="url" id="f-${f.id}-url" class="ctrl mono" value="${sUrl}" placeholder="https://…" />`)}

      ${UI.section({
        title: "匹配规则",
        desc: `共 ${rules.length} 条`,
        actions: UI.btn("添加规则", { size: "sm", iconName: "plus", attrs: `data-add-rule="${f.id}"` }),
        body: rules.length === 0
          ? `<div class="rule-empty">还没有规则，点右上角「添加规则」。</div>`
          : rules.map((r, ri) => renderRuleBlock(f.id, r, ri)).join(""),
      })}
    </div>
  </section>`;
}

function renderRuleBlock(fid, r, ri) {
  const sName     = escapeHTML(r.name || ("规则 " + (ri + 1)));
  const sInclude  = escapeHTML(r.include || "");
  const sExclude  = escapeHTML(r.exclude || "");
  const sPath     = escapeHTML(r.savePath || "");
  const sCategory = escapeHTML(r.category || "");
  const sTags     = escapeHTML(r.tags || "");
  return `
    <div class="rule-card">
      <div class="rule-head">
        <span class="index-badge">${ri + 1}</span>
        <input type="text" class="control rule-name" data-rule-name="${fid}-${ri}" value="${sName}" placeholder="规则名称" />
        ${toggle(r.enabled !== false, { data: `data-rule-enabled="${fid}-${ri}"`, sm: true, label: "启用" })}
        ${UI.iconBtn("trash", { title: "删除规则", variant: "danger", attrs: `data-del-rule="${fid}-${ri}"` })}
      </div>
      <div class="rule-body">
        ${field("匹配模式",
          `<select class="ctrl" data-rule-mode="${fid}-${ri}">
            <option value="keyword" ${r.mode !== "regex" ? "selected" : ""}>关键字（包含 / 排除表达式）</option>
            <option value="regex" ${r.mode === "regex" ? "selected" : ""}>正则表达式</option>
          </select>`)}
        ${field("包含 Include",
          `<textarea wrap="soft" class="auto-h ctrl mono" data-rule-include="${fid}-${ri}" placeholder="${r.mode === "regex" ? "正则，需匹配" : "例：ManoJob 720p|MrLucky（| 为 OR，空格为 AND）"}">${sInclude}</textarea>`)}
        ${field("排除 Exclude",
          `<textarea wrap="soft" class="auto-h ctrl mono" data-rule-exclude="${fid}-${ri}" placeholder="例：1080p|2160p，命中任一则跳过">${sExclude}</textarea>`)}
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
      </div>
    </div>`;
}

function bindFeedEvents(rss) {
  $$("[data-del-feed]").forEach(b => b.addEventListener("click", () => {
    // 先同步 DOM 输入 → rss 对象，避免其他正在编辑的 feed 丢值
    collectRSS(rss);
    const id = b.dataset.delFeed;
    rss.feeds = rss.feeds.filter(f => f.id !== id);
    markDirty("rss");
    renderRSS(document.getElementById("content"), rss);
  }));
  $$("[data-add-rule]").forEach(b => b.addEventListener("click", () => {
    collectRSS(rss);
    const fid = b.dataset.addRule;
    const feed = rss.feeds.find(f => f.id === fid);
    feed.rules = feed.rules || [];
    feed.rules.push({ id: genID(), name: "规则 " + (feed.rules.length + 1), enabled: true, mode: "keyword", include: "", exclude: "", savePath: "", category: "", tags: "", uploadLimit: 0, stopped: false });
    markDirty("rss");
    renderRSS(document.getElementById("content"), rss);
  }));
  $$("[data-del-rule]").forEach(b => b.addEventListener("click", () => {
    collectRSS(rss);
    const [fid, ri] = b.dataset.delRule.split("-");
    const feed = rss.feeds.find(f => f.id === fid);
    feed.rules.splice(parseInt(ri, 10), 1);
    markDirty("rss");
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
  if (showLoading) panel.innerHTML = UI.loading("正在拉取订阅状态…");
  const res = await api("GET", "/rss/status");
  if (!res.success) {
    panel.innerHTML = notice(`加载状态失败：${escapeHTML(res.message || "未知错误")}`, "danger");
    return;
  }
  panel.innerHTML = renderRSSStatusList(res.data || []);
}

function renderRSSStatusList(list) {
  if (list.length === 0) {
    return UI.empty("暂无可监控的订阅源", { iconName: "inbox" });
  }
  return `<div class="status-list">${list.map(s => {
    const seenLine = `累计已见 <b>${s.seenCount}</b>`;
    let detailLine;
    if (s.lastOk != null) {
      detailLine = s.lastOk
        ? `解析 <b>${s.itemCount}</b> 条 · 命中 <b>${s.matched}</b> · 提交下载 <b>${s.downloaded}</b>${s.failed > 0 ? ` · 失败 <b>${s.failed}</b>` : ""}${s.snapshot ? ` · ${UI.tag("快照模式", "warn")}` : ""}`
        : `${UI.tag("上次拉取失败", "bad")} <span>${escapeHTML(s.lastError || "")}</span>`;
    } else {
      detailLine = `<span class="text-dim">尚未拉取过</span>`;
    }

    const recent = (s.recentItems || []).map(r => renderRecentItem(r)).join("");
    const recentBlock = recent
      ? `<details open>
           <summary>最近 ${(s.recentItems || []).length} 条处理记录</summary>
           <div class="status-recent">${recent}</div>
         </details>`
      : "";
    const dot = !s.enabled ? "" : s.fetching ? "busy" : s.lastOk === false ? "err" : s.lastOk ? "ok" : "";

    return `
    <div class="status-item">
      <div class="status-head">
        <span class="status-dot ${dot}"></span>
        <span class="status-name">${escapeHTML(s.feedName)}</span>
        ${renderFeedStatusBadge(s)}
        <span class="status-time">${fmtTimeAgo(s.lastFetchAt)} · ${seenLine}</span>
        ${UI.btn("重新匹配", {
          size: "sm",
          variant: "ghost",
          iconName: "refresh",
          attrs: `data-rss-reset="${s.feedId}" data-feed-name="${escapeHTML(s.feedName)}"${s.fetching ? " disabled" : ""}`,
        })}
      </div>
      <div class="status-detail">${detailLine}</div>
      <div class="status-url mono" title="${escapeHTML(s.url || "")}">${escapeHTML(s.url || "（未填）")}</div>
      ${recentBlock}
    </div>`;
  }).join("")}</div>`;
}

function renderFeedStatusBadge(s) {
  if (!s.enabled) return UI.tag("已禁用", "muted", { dot: true });
  if (s.fetching) return UI.tag("拉取中", "info", { dot: true });
  if (s.lastOk == null) return UI.tag("等待首次", "muted", { dot: true });
  if (s.lastOk) return UI.tag("正常", "good", { dot: true });
  return UI.tag("异常", "bad", { dot: true });
}

function renderRecentItem(r) {
  const actionMap = {
    downloaded:       { label: "已提交", cls: "good" },
    skipped_snapshot: { label: "快照跳过", cls: "muted" },
    skipped_no_rule:  { label: "未命中", cls: "muted" },
    failed:           { label: r.error || "失败", cls: "bad" },
    seen_duplicate:   { label: "重复", cls: "muted" },
  };
  const a = actionMap[r.action] || { label: r.action || "", cls: "muted" };
  return `<div class="recent-item">
    <span class="recent-title">${escapeHTML(r.title)}</span>
    ${r.ruleName ? `<span class="recent-rule">${escapeHTML(r.ruleName)}</span>` : ""}
    <span class="recent-time">${fmtTimeAgo(r.processedAt)}</span>
    ${UI.tag(escapeHTML(a.label), a.cls)}
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
  // only=rss：本页只负责 rss 段，其余段沿用服务端当前值，
  // 否则设置页刚保存的改动会被这次全量提交静默回滚。
  const res = await api("POST", "/config?only=rss", cfg);
  if (res.success) clearDirty("rss");
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
  if (!c.proxy) c.proxy = { mode: "system", url: "" };
  c.proxy.mode = $("#proxy-mode").value;
  c.proxy.url = $("#proxy-url").value.trim();
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
      ratioTarget: parseFloat(document.querySelector(`[data-lm-ratio="${i}"]`).value || "0") || 0,
      minUploaded: parseInt(document.querySelector(`[data-lm-minup="${i}"]`).value || "0", 10) || 0,
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
  if ($("#fm-pp-enabled")) {
    const pp = c.fileManager.postProcess || (c.fileManager.postProcess = {});
    pp.enabled = $("#fm-pp-enabled").checked;
    pp.command = $("#fm-pp-cmd").value;
    pp.extensions = ($("#fm-pp-exts").value || "").trim();
    pp.categoryInclude = ($("#fm-pp-cat-inc").value || "").trim();
    pp.categoryExclude = ($("#fm-pp-cat-exc").value || "").trim();
    pp.tagInclude = ($("#fm-pp-tag-inc").value || "").trim();
    pp.tagExclude = ($("#fm-pp-tag-exc").value || "").trim();
    pp.timeout = parseInt($("#fm-pp-timeout").value || "0", 10) || 0;
    pp.sizePrune = $("#fm-pp-prune").checked;
    pp.verify = $("#fm-pp-verify").checked;
    // 留空存成 null：语义是「自动探测」，存成空串会被当成显式指定一个坏路径
    const probe = ($("#fm-pp-probe").value || "").trim();
    pp.probePath = probe === "" ? null : probe;
  }
  if ($("#ac-enabled")) {
    const ac = c.autoCategory || (c.autoCategory = {});
    ac.enabled = $("#ac-enabled").checked;
    ac.interval = parseInt($("#ac-interval").value || "60", 10) || 60;
    ac.onlyIfEmpty = $("#ac-only-empty").checked;
    ac.fallback = ($("#ac-fallback").value || "").trim();
    ac.rules = collectCategoryRules();
  }
}

// ---------- 自动分类 ----------

// 条件类型，与后端 models 里的常量一一对应
const CAT_COND = { code: "code", latinCount: "latinCount", cjkCount: "cjkCount", regex: "regex" };

const CAT_COND_TYPES = [
  [CAT_COND.code, "编号式命名（字母-数字）"],
  [CAT_COND.latinCount, "英文字母数量"],
  [CAT_COND.cjkCount, "汉字数量"],
  [CAT_COND.regex, "正则匹配"],
];

// 从 DOM 收集当前规则。试算与保存共用：试算必须反映表单里还没保存的改动，
// 否则用户改完规则点「试算」看到的仍是旧结论，比不试算更误导。
function collectCategoryRules() {
  const cur = (_settingsCfg && _settingsCfg.autoCategory && _settingsCfg.autoCategory.rules) || [];
  return $$("[data-cat-name]").map((n) => {
    const ri = parseInt(n.dataset.catName, 10);
    const old = cur[ri] || {};
    const when = $$(`[data-cat-ctype="${ri}"]`).map((sel) => {
      const ci = parseInt(sel.dataset.catCi, 10);
      const type = sel.value;
      const q = (k) => document.querySelector(`[data-cat-c${k}="${ri}:${ci}"]`) || {};
      if (type === CAT_COND.regex) return { type, pattern: q("pattern").value || "" };
      if (type === CAT_COND.code) {
        return {
          type,
          minLetters: parseInt(q("letters").value || "0", 10) || 0,
          minDigits: parseInt(q("digits").value || "0", 10) || 0,
          anchored: q("anchored").checked !== false,
        };
      }
      return { type, min: parseInt(q("min").value || "0", 10) || 0 };
    });
    return {
      id: old.id || genID(),
      name: n.value,
      enabled: (document.querySelector(`[data-cat-enabled="${ri}"]`) || {}).checked !== false,
      category: (document.querySelector(`[data-cat-cat="${ri}"]`) || {}).value || "",
      when,
    };
  });
}

// 条件类型对应的参数控件。计数类刻意不翻译成正则：语义相同但慢几十倍且几乎不可读
function catCondArgs(ri, ci, c) {
  const at = (k) => `data-cat-c${k}="${ri}:${ci}"`;
  switch (c.type) {
    case CAT_COND.code:
      return `<div class="controls">
        <span class="unit">字母段≥</span><input type="number" class="w-xs" ${at("letters")} value="${c.minLetters || 2}" min="1">
        <span class="unit">数字段≥</span><input type="number" class="w-xs" ${at("digits")} value="${c.minDigits || 2}" min="1">
        <label class="inline-check"><input type="checkbox" ${at("anchored")} ${c.anchored !== false ? "checked" : ""}><span>只在名称开头</span></label>
      </div>`;
    case CAT_COND.latinCount:
    case CAT_COND.cjkCount:
      return `<div class="controls"><span class="unit">数量≥</span><input type="number" class="w-xs" ${at("min")} value="${c.min || 0}" min="1"></div>`;
    case CAT_COND.regex:
      return `<input type="text" class="ctrl mono" ${at("pattern")} value="${escapeHTML(c.pattern || "")}" placeholder="Go/RE2 语法，如 ^\\[字幕组\\]" />`;
    default:
      return "";
  }
}

function catCondRow(ri, ci, c) {
  return `<div class="cond-row">
    <select class="ctrl w-sm" data-cat-ctype="${ri}" data-cat-ci="${ci}">
      ${CAT_COND_TYPES.map(([v, l]) => `<option value="${v}"${c.type === v ? " selected" : ""}>${l}</option>`).join("")}
    </select>
    <div class="cond-args">${catCondArgs(ri, ci, c)}</div>
    ${UI.iconBtn("trash", { title: "删除条件", variant: "danger", size: "sm", attrs: `data-cat-cond-del="${ri}:${ci}"` })}
  </div>`;
}

// 只重渲规则那一块（加/删/改条件时调用，不碰整页）
function renderCategoryRules() {
  if (!_settingsCfg) return;
  const box = $("#cat-rules");
  if (!box) return;
  const ac = _settingsCfg.autoCategory || (_settingsCfg.autoCategory = { rules: [] });
  const rules = ac.rules || [];
  const count = $("#cat-count");
  if (count) count.textContent = `共 ${rules.length} 条`;
  box.innerHTML = rules.length === 0
    ? `<div class="rule-empty">还没有规则，点上方「添加规则」或「恢复内置三条」。</div>`
    : rules.map((r, ri) => `
      <div class="rule-card">
        <div class="rule-head">
          <span class="index-badge">${ri + 1}</span>
          <input type="text" class="control rule-name" data-cat-name="${ri}" value="${escapeHTML(r.name || "")}" placeholder="规则名称" />
          <span class="text-dim">→</span>
          <input type="text" class="control w-sm" data-cat-cat="${ri}" value="${escapeHTML(r.category || "")}" placeholder="分类名" />
          ${toggle(r.enabled !== false, { data: `data-cat-enabled="${ri}"`, sm: true, label: "启用" })}
          ${UI.iconBtn("chevron-down", { title: "上移", size: "sm", attrs: `data-cat-up="${ri}"` })}
          ${UI.iconBtn("chevron-down", { title: "下移", size: "sm", attrs: `data-cat-down="${ri}"` })}
          ${UI.iconBtn("trash", { title: "删除规则", variant: "danger", size: "sm", attrs: `data-cat-del="${ri}"` })}
        </div>
        <div class="rule-body">
          ${catCondSection(ri, r)}
        </div>
      </div>`).join("");
}

// 内置的编号式 → 英文长名 → 中文长名三条。分类名是占位，需按实际使用的分类改
function catDefaultRules() {
  return [
    { id: genID(), name: "编号式命名", enabled: true, category: "编号", when: [{ type: CAT_COND.code, minLetters: 2, minDigits: 2, anchored: true }] },
    { id: genID(), name: "英文长名", enabled: true, category: "外文", when: [{ type: CAT_COND.latinCount, min: 15 }] },
    { id: genID(), name: "中文长名", enabled: true, category: "中文", when: [{ type: CAT_COND.cjkCount, min: 10 }] },
  ];
}

function catCondSection(ri, r) {
  const conds = r.when || [];
  return UI.section({
    title: "匹配条件",
    desc: conds.length === 0 ? "无条件时这条规则不会命中任何任务" : "全部满足才算命中",
    actions: UI.btn("添加条件", { size: "sm", iconName: "plus", attrs: `data-cat-cond-add="${ri}"` }),
    body: `<div class="cond-list">${conds.map((c, ci) => catCondRow(ri, ci, c)).join("")}</div>`,
  });
}

// 规则增删改都走事件委托：规则区会被反复重渲，逐个按钮绑定在重渲后就失效了
function bindCategoryEvents(root) {
  const catMutate = (fn) => { syncFormToCfg(); fn(); markDirty("settings"); renderCategoryRules(); };
  root.addEventListener("click", (e) => {
    const el = e.target.closest("[data-cat-del],[data-cat-up],[data-cat-down],[data-cat-cond-add],[data-cat-cond-del]");
    if (!el) return;
    if (el.dataset.catDel !== undefined) {
      const i = parseInt(el.dataset.catDel, 10);
      catMutate(() => _settingsCfg.autoCategory.rules.splice(i, 1));
      return;
    }
    if (el.dataset.catUp !== undefined) {
      const i = parseInt(el.dataset.catUp, 10);
      if (i <= 0) return;
      catMutate(() => {
        const rs = _settingsCfg.autoCategory.rules;
        [rs[i - 1], rs[i]] = [rs[i], rs[i - 1]];
      });
      return;
    }
    if (el.dataset.catDown !== undefined) {
      const i = parseInt(el.dataset.catDown, 10);
      catMutate(() => {
        const rs = _settingsCfg.autoCategory.rules;
        if (i >= rs.length - 1) return;
        [rs[i + 1], rs[i]] = [rs[i], rs[i + 1]];
      });
      return;
    }
    if (el.dataset.catCondAdd !== undefined) {
      const i = parseInt(el.dataset.catCondAdd, 10);
      catMutate(() => {
        const r = _settingsCfg.autoCategory.rules[i];
        r.when = r.when || [];
        r.when.push({ type: CAT_COND.latinCount, min: 15 });
      });
      return;
    }
    if (el.dataset.catCondDel !== undefined) {
      const [ri, ci] = el.dataset.catCondDel.split(":").map(Number);
      catMutate(() => { _settingsCfg.autoCategory.rules[ri].when.splice(ci, 1); });
    }
  });
  // 切换条件类型：参数控件完全不同，收集后整块重渲
  root.addEventListener("change", (e) => {
    if (!e.target.matches("[data-cat-ctype]")) return;
    const ri = parseInt(e.target.dataset.catCtype, 10);
    const ci = parseInt(e.target.dataset.catCi, 10);
    syncFormToCfg();
    const cond = ((_settingsCfg.autoCategory.rules[ri] || {}).when || [])[ci];
    if (!cond) return;
    // 类型换了就给新类型一套合理默认，避免留下旧类型的残留参数
    if (cond.type === CAT_COND.code) { cond.minLetters = 2; cond.minDigits = 2; cond.anchored = true; delete cond.min; delete cond.pattern; }
    else if (cond.type === CAT_COND.regex) { cond.pattern = ""; delete cond.min; delete cond.minLetters; delete cond.minDigits; delete cond.anchored; }
    else { cond.min = cond.type === CAT_COND.latinCount ? 15 : 10; delete cond.pattern; delete cond.minLetters; delete cond.minDigits; delete cond.anchored; }
    markDirty("settings");
    renderCategoryRules();
  });
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
    ? `<div class="rule-empty">还没有规则，点上方「添加规则」。</div>`
    : rules.map((r, i) => `
      <div class="rule-card">
        <div class="rule-head">
          <span class="index-badge">${i + 1}</span>
          <input type="text" class="control rule-name" data-lm-name="${i}" value="${escapeHTML(r.name || "")}" placeholder="规则名称" />
          ${toggle(r.enabled !== false, { data: `data-lm-enabled="${i}"`, sm: true, label: "启用" })}
          ${UI.iconBtn("trash", { title: "删除规则", variant: "danger", attrs: `data-lm-del="${i}"` })}
        </div>
        <div class="rule-body">
          ${field("匹配正则",
            `<input type="text" class="ctrl mono" data-lm-match="${i}" value="${escapeHTML(r.match || "")}" placeholder="如 \\.mkv$ 或 4k" />`,
            "Go/RE2 语法，对任务名做部分匹配。")}
          ${field("上传限速",
            `<div class="controls"><input type="number" class="w-xs" data-lm-limit="${i}" value="${r.uploadLimit}" min="0"><span class="unit">KB/s</span></div>`,
            "0 表示不限制；启用比率目标时必须大于 0。")}
          ${field("比率目标",
            `<div class="controls"><input type="number" class="w-xs" data-lm-ratio="${i}" value="${r.ratioTarget || 0}" min="0" step="0.1"><span class="unit">分享率</span></div>`,
            "0 表示不按比率压速。超过目标后限速按「基准 × 目标 ÷ 实际比率」连续下降，最低 1 KB/s；变化不足 10% 时不重复下发。")}
          ${field("最低上传量",
            `<div class="controls"><input type="number" class="w-xs" data-lm-minup="${i}" value="${r.minUploaded || 0}" min="0"><span class="unit">MB</span></div>`,
            "已上传量没到这个数就不看比率，防止小体积种子刚传一点就被压速。0 表示不设门槛。")}
        </div>
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
    ? `<div class="rule-empty">还没有通道，点上方「添加通道」。可添加本地（Ollama 等）或云端（OpenAI 兼容）通道。</div>`
    : chans.map((ch, i) => `
      <div class="rule-card">
        <div class="rule-head">
          <span class="index-badge">${i + 1}</span>
          <input type="text" class="control rule-name" data-ai-name="${i}" value="${escapeHTML(ch.name || "")}" placeholder="通道名称，如 本地 Ollama" />
          ${toggle(ch.enabled, { data: `data-ai-enabled="${i}"`, sm: true, label: "启用" })}
          ${UI.iconBtn("trash", { title: "删除通道", variant: "danger", attrs: `data-ai-del="${i}"` })}
        </div>
        <div class="rule-body">
          ${field("BaseURL",
            `<input type="text" class="ctrl mono" data-ai-url="${i}" value="${escapeHTML(ch.baseURL || "")}" placeholder="http://localhost:11434/v1" />`,
            "任何 OpenAI 兼容接口均可，如本地 Ollama、DeepSeek、OpenAI。")}
          ${field("API Key",
            `<input type="password" class="ctrl mono" data-ai-key="${i}" value="${escapeHTML(ch.apiKey || "")}" placeholder="本地模型可留空" />`)}
          ${field("模型",
            `<input type="text" class="ctrl mono" data-ai-model="${i}" value="${escapeHTML(ch.model || "")}" placeholder="qwen2.5:7b / deepseek-chat / gpt-4o-mini" />`)}
          ${field("提示词",
            `<textarea class="ctrl mono" data-ai-prompt="${i}" placeholder="留空使用内置默认提示词。支持变量 {files}（文件名 JSON 数组）与 {torrent}（任务名），要求模型只输出 JSON 映射。">${escapeHTML(ch.prompt || "")}</textarea>`)}
        </div>
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
  markDirty("settings");
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
  // 转码后处理：后端恒定返回该对象（无 omitempty），这里兜底成空对象免得取属性报错
  const pp = (cfg.fileManager && cfg.fileManager.postProcess) || {};
  // 自动分类：老配置文件可能还没有这一段，兜底成默认结构
  const ac = cfg.autoCategory || { enabled: false, interval: 60, onlyIfEmpty: true, fallback: "", rules: [] };

  root.innerHTML = `
    ${UI.page({
      eyebrow: "Preferences",
      title: "设置",
      desc: "所有配置保存在服务端配置文件中，保存后热重载，无需重启。",
    })}

    <!-- 外观 -->
    ${UI.panel({
      title: "外观主题",
      desc: "只对当前浏览器生效，保存在本地，不影响其他设备",
      iconName: "sun",
      body: field("主题模式", UI.themeSwitch(getTheme()), "「跟随系统」按操作系统的深/浅色设置自动切换。"),
    })}

    <!-- 连接 -->
    ${UI.panel({
      title: "连接",
      desc: "QBHive 通过 qBittorrent WebUI API 读取与控制任务",
      iconName: "link",
      actions: UI.btn("测试连接", { size: "sm", iconName: "wifi", id: "qb-test" }),
      body: `
        ${field("WebUI 地址", `<input type="url" id="qb-url" class="ctrl mono" value="${sQbUrl}" placeholder="http://192.168.1.10:8080" />`)}
        ${field("用户名", `<input type="text" id="qb-user" class="w-md" value="${sQbUser}" autocomplete="username" />`)}
        ${field("密码", `<input type="password" id="qb-pass" class="w-md" value="${sQbPass}" placeholder="已隐藏，留空则不修改" autocomplete="current-password" />`)}
        ${field("API Key",
          `<input type="password" id="qb-apikey" class="ctrl mono" value="${sQbKey}" placeholder="已隐藏，留空则不修改" />`,
          "qBittorrent v5.2.0+ 可在 WebUI → 设置 → WebUI 里生成，形如 <code>qbt_xxx</code>。填写后优先使用，直接走 <code>Authorization: Bearer</code>，跳过用户名密码登录。")}
        ${UI.section({
          title: "QBHive 服务",
          body: field("监听地址", `<input type="text" id="srv-listen" class="w-md mono" value="${sListen}" placeholder=":8088" />`,
            "修改后需重启进程才会生效。"),
        })}
      `,
    })}

    <!-- 网络代理 -->
    ${UI.panel({
      title: "网络代理",
      desc: "RSS 拉取、通知、AI 美化等所有出站请求统一走代理；qBittorrent 与 Ollama 等本机回环地址始终直连",
      iconName: "wifi",
      body: `
        ${field("模式", `
          <select id="proxy-mode" class="ctrl">
            <option value="system"${(cfg.proxy && cfg.proxy.mode ? cfg.proxy.mode : "system") === "system" ? " selected" : ""}>跟随 macOS 系统代理</option>
            <option value="manual"${cfg.proxy && cfg.proxy.mode === "manual" ? " selected" : ""}>手动指定</option>
            <option value="off"${cfg.proxy && cfg.proxy.mode === "off" ? " selected" : ""}>直连（忽略系统代理与环境变量）</option>
          </select>`,
          "「跟随系统」自动读取 macOS「系统设置 → 网络 → 代理」，无显式代理（仅 PAC）时回退环境变量。<code>HTTP_PROXY</code>/<code>HTTPS_PROXY</code>/<code>NO_PROXY</code> 亦生效。")}
        ${field("代理地址", `<input type="text" id="proxy-url" class="ctrl mono" value="${escapeHTML((cfg.proxy && cfg.proxy.url) || "")}" placeholder="http://127.0.0.1:7890 或 socks5://127.0.0.1:7890" />`,
          "仅「手动指定」模式生效。支持 <code>http://</code> <code>https://</code> <code>socks5://</code> <code>socks5h://</code>；留空则直连。")}
        <div class="hint" id="proxy-status">${escapeHTML((cfg.proxy && cfg.proxy.mode === "off") ? "当前模式：直连，代理已关闭。" : "保存后立即对后续请求生效，无需重启。")}</div>
      `,
    })}

    <!-- 限速 -->
    ${UI.panel({
      title: "限速规则",
      desc: "按任务名正则匹配，批量下发上传限速；可选按分享比率连续压速",
      iconName: "gauge",
      body: `
        ${field("启用", toggle(cfg.limiter.enabled, { id: "lim-enabled" }),
          "只对限速值发生变化的任务调用 qB 接口，不会反复下发。")}
        ${field("刷新间隔",
          `<div class="controls"><input type="number" id="lim-interval" class="w-xs" value="${cfg.limiter.interval || 10}" min="1"><span class="unit">秒</span></div>`)}
        ${UI.section({
          title: "规则",
          desc: `<span id="lim-count"></span>`,
          actions: UI.btn("添加规则", { size: "sm", iconName: "plus", id: "lim-add" }),
          body: `<div id="limiter-rules"></div>`,
        })}
      `,
    })}

    <!-- 自动分类 -->
    ${UI.panel({
      title: "自动分类",
      desc: "按规则给种子任务打上 qB 分类，规则自上而下求值，先命中先得",
      iconName: "tag",
      body: `
        ${field("启用", toggle(ac.enabled, { id: "ac-enabled" }),
          "每隔一个扫描间隔按规则给任务设置分类。<b>默认只处理尚未分类的任务</b>，不会冲掉你手动整理的结果。")}
        ${field("扫描间隔",
          `<div class="controls"><input type="number" id="ac-interval" class="w-xs" value="${ac.interval || 60}" min="1"><span class="unit">秒</span></div>`,
          "分类是低频动作，60 秒足够；任务多、想更快生效可调小。")}
        ${field("只处理未分类任务", toggle(ac.onlyIfEmpty !== false, { id: "ac-only-empty" }),
          "<b>建议保持开启。</b>关掉后，已经打好分类的任务也会被规则重新判定并覆盖——包括你手动设的分类。")}
        ${field("兜底分类",
          `<input type="text" id="ac-fallback" class="ctrl w-sm" value="${escapeHTML(ac.fallback || "")}" placeholder="留空表示不动" />`,
          "所有规则都没命中时归入这个分类，<b>留空则保持不动</b>。qB 里该分类不存在时由 qB 自动创建。")}
        ${UI.notice("若 qB 端开启了「自动管理种子」，改分类会触发 qB 移动文件，请确认这是你想要的。", "warn")}
        ${UI.section({
          title: "规则",
          desc: `<span id="cat-count"></span> · 自上而下求值，命中即停`,
          actions: UI.btn("恢复内置三条", { size: "sm", attrs: 'id="cat-default"' }) + UI.btn("添加规则", { size: "sm", iconName: "plus", id: "cat-add" }),
          body: `<div id="cat-rules"></div>`,
        })}
        ${UI.section({
          title: "试算",
          desc: "按上面<b>当前填写</b>的规则求值，不会改动任何任务，也不用先保存",
          body: `
            ${field("任务名",
              `<textarea id="cat-preview-names" class="ctrl mono auto-h" placeholder="每行一个任务名；留空则直接取 qB 里未分类的任务试算"></textarea>`)}
            <div class="controls">${UI.btn("试算", { size: "sm", id: "cat-preview" })}</div>
            <div id="cat-preview-out"></div>
          `,
        })}
      `,
    })}

    <!-- 通知 -->
    ${UI.panel({
      title: "完成通知",
      desc: "基于 Apprise-Go，原生支持 Telegram / Discord / Slack / 邮件 / Bark 等上百种渠道",
      iconName: "zap",
      actions: UI.btn("发送测试通知", { size: "sm", iconName: "zap", id: "nt-test" }),
      body: `
        ${field("启用", toggle(cfg.notifier.enabled, { id: "nt-enabled" }),
          "每 10 秒扫描一次完成事件，已通知记录持久化，重启不重发。")}
        ${field("通知渠道",
          `<textarea id="nt-urls" class="ctrl mono auto-h" placeholder="每行一个 Apprise URL，例如：&#10;telegram://BOT_TOKEN/CHAT_ID&#10;discord://WEBHOOK_ID/WEBHOOK_TOKEN&#10;gotify://TOKEN@HOST:PORT">${sNotifyUrl}</textarea>`,
          "格式参见 <a href=\"https://github.com/caronc/apprise/wiki\" target=\"_blank\">Apprise Wiki</a>，渠道用法见 <a href=\"https://github.com/unraid/apprise-go\" target=\"_blank\">Apprise-Go</a>。")}
        ${field("通知字段",
          `<div class="check-group">${renderNotifyFieldChecks(cfg.notifier.fields)}</div>`,
          "勾选通知正文包含的字段，至少一项；标题始终包含任务名。")}
      `,
    })}

    <!-- 文件管理 -->
    ${UI.panel({
      title: "文件管理",
      desc: "归档、文件名清洗与 AI 美化",
      iconName: "files",
      body: `
        ${UI.section({
          title: "单文件自动归档",
          body: `
            ${field("启用", toggle(cfg.fileManager.enabled, { id: "fm-enabled" }),
              "下载完成后，若 <code>savePath/torrentName/</code> 下只有一个文件，自动上移并清理空目录。")}
            ${field("扫描间隔",
              `<div class="controls"><input type="number" id="fm-interval" class="w-xs" value="${cfg.fileManager.scanInterval || 15}" min="1"><span class="unit">秒</span></div>`)}
          `,
        })}

        ${UI.section({
          title: "文件名自动清理",
          body: `
            ${field("启用", toggle(cfg.fileManager.cleanEnabled, { id: "fm-clean-enabled" }),
              "自动移除站点域名水印与 emoji 装饰，通过 qB 重命名接口执行，不影响做种。")}
            ${field("自定义正则",
              `<textarea id="fm-clean-rules" class="ctrl mono auto-h" placeholder="每行一个正则（Go/RE2 语法），匹配内容会被移除，例如：&#10;^【[^】]*】\\s*&#10;\\s*-\\s*4KHDR.*$">${escapeHTML((cfg.fileManager.cleanRules || []).join("\n"))}</textarea>`,
              "内置规则已覆盖 <code>www.xxx.com - </code> 前缀、<code>[xxx.com]</code> / <code>【xxx.com】</code> 括号、<code>@xxx.com</code> 后缀、<code>www.xxx.com@@</code> 这类域名带 @ 尾巴的水印；自定义规则在内置规则之后执行。清洗保证保留扩展名、结果非空、不含非法字符，所有改动记录在「文件」页并可回退。")}
          `,
        })}

        ${UI.section({
          title: "AI 格式化文件名",
          body: `
            ${field("启用", toggle(cfg.fileManager.aiEnabled, { id: "fm-ai-enabled" }),
              "在正则清洗之后执行，每个任务只调用一次（批量传入文件名）；失败自动重试 3 轮，仍失败则降级为仅正则清洗。")}
            ${field("使用通道", `<select id="fm-ai-active" class="ctrl"></select>`)}
            <div class="section-head">
              <div class="section-heading"><h3>通道<span class="text-dim" id="ai-count"></span></h3></div>
              <div class="section-actions">${UI.btn("添加通道", { size: "sm", iconName: "plus", id: "ai-add" })}</div>
            </div>
            <div id="ai-channels"></div>
          `,
        })}

        ${UI.section({
          title: "转码后处理",
          body: `
            ${field("启用", toggle(!!(pp.enabled), { id: "fm-pp-enabled" }),
              `下载完成后按 qB 报告的任务文件列表逐个转码：命中扩展名白名单的<b>每个</b>视频文件都会跑一次命令（多文件种子、文件散在共享分类根也一样）。全服务同时<b>只跑一条</b>命令，其余文件等下一轮扫描排队；已派发过的文件不会重复跑，重启也不会。归档关掉也照常触发——只要本项开着就会扫描。`)}
            ${field("命令",
              `<textarea id="fm-pp-cmd" class="ctrl mono auto-h" placeholder="/usr/bin/shortcuts run &#34;Permute HEVC 50%缩放&#34; -i &#34;$FILE&#34;">${escapeHTML(pp.command || "")}</textarea>`,
              `单行 shell 命令（走 <code>sh -c</code>），可以自己写引号、管道。占位符 <code>{file}</code> <code>{dir}</code> <code>{name}</code> <code>{category}</code> <code>{tags}</code> <code>{savepath}</code> 会被替换成自动加好单引号的字面量；也可以直接用环境变量 <code>$FILE</code> <code>$DIR</code> <code>$NAME</code> <code>$CATEGORY</code> <code>$TAGS</code> <code>$SAVEPATH</code>。<code>$TAGS</code> 是任务在 qB 里的逗号分隔标签原值（无标签时为空串），脚本里可自行按标签挑快捷指令。示例中的 <code>$FILE</code> 一定要带双引号，路径含空格时才能拆开。<b>快捷指令名必须与 <code>shortcuts list</code> 输出逐字符一致</b>，差一个空格就会「找不到快捷指令」。首次接入建议先用下面的「测试运行」验证快捷指令名与参数能不能跑通。`)}
            ${field("扩展名白名单",
              `<input type="text" id="fm-pp-exts" class="ctrl w-sm" value="${escapeHTML(pp.extensions || "")}" placeholder="mov,m4v,mkv,mp4">`,
              "多条用逗号分隔、不写点（<code>, ， 、 ; ； |</code> 与换行都当分隔符）；只有命中这些扩展名的文件才执行，留空表示不过滤（非视频也会跑命令）。")}
            ${field("分类过滤",
              `<div class="split">
                <input type="text" id="fm-pp-cat-inc" value="${escapeHTML(pp.categoryInclude || "")}" placeholder="只转这些分类，例：动漫, 剧集">
                <input type="text" id="fm-pp-cat-exc" value="${escapeHTML(pp.categoryExclude || "")}" placeholder="这些分类不转，例：电影">
              </div>`,
              "<b>左框只转、右框不转</b>。按<b>整个任务</b>的 qB 分类判定，不区分大小写；多条之间用逗号分隔（<code>, ， 、 ; ； |</code> 与换行都算分隔符）；两边都留空即不过滤。包含名单里<b>分类要逐字符对上</b>（<code>动漫</code> 与 <code>动漫组</code> 是两条不同分类）。<b>未分类的任务永远不命中包含名单</b>——要连没打分类的一起转，就把左框留空、只用右框排除。同时命中左右两框时<b>右框优先</b>（不转）。判定结果会持久化，但<b>改了名单就会重扫历史任务</b>：之前被挡掉、漏转的会补转，已经转过的不会重转（按文件去重），不用重启也不用清状态。")}
            ${field("标签过滤",
              `<div class="split">
                <input type="text" id="fm-pp-tag-inc" value="${escapeHTML(pp.tagInclude || "")}" placeholder="只转带这些标签的，例：转码">
                <input type="text" id="fm-pp-tag-exc" value="${escapeHTML(pp.tagExclude || "")}" placeholder="带这些标签的不转，例：真人">
              </div>`,
              "<b>左框只转、右框不转</b>。任务带多个标签时<b>命中任意一个即算</b>，不要求全部命中；<b>无标签的任务不命中左框</b>。典型用法是反向 opt-in：左框填 <code>转码</code>，只有手动打了 <code>转码</code> 标签的任务才会转。其余语义、优先级与分类过滤一致。")}
            ${field("超时",
              `<div class="controls"><input type="number" id="fm-pp-timeout" class="w-xs" value="${pp.timeout || 0}" min="0"><span class="unit">秒</span></div>`,
              "0 表示不限。转码动辄几十分钟，默认不限即可；命令失败会退避重试 2 次后放弃。")}
            ${field("按大小清理产物", toggle(!!(pp.sizePrune), { id: "fm-pp-prune" }),
              "命令成功后：产物比原片<b>小</b>就删掉原片保留产物，产物比原片<b>大</b>就保留原片、删掉产物。产物必须同时满足「同目录、与原片<b>同名</b>（只换扩展名）、且不早于命令开始时刻」才算数——文件散在共享分类根时，只按「同目录新文件」认产物会把别的任务刚下完的片子当成产物删掉。找不到产物就不动任何文件。删除会写进「文件」页审计日志，<b>不可回退</b>；删掉原片会让 qB 认为种子缺文件。")}
            ${field("删除前校验产物", toggle(pp.verify !== false, { id: "fm-pp-verify" }),
              "开启后（默认），删原片之前先用 <code>ffprobe</code> 确认产物真能解出视频流：<b>命令返回 0 不等于产物可用</b>——转码被中断、封装损坏留下的小文件照样会被当成成功。校验不通过、或本机找不到 ffprobe，就一个文件都不删。关掉则退回纯按大小判定。")}
            ${field("ffprobe 路径",
              `<input type="text" id="fm-pp-probe" class="ctrl mono" value="${escapeHTML(pp.probePath || "")}" placeholder="/opt/homebrew/bin/ffprobe">`,
              "留空则按 <code>PATH → /opt/homebrew/bin → /usr/local/bin → /usr/bin</code> 的顺序自动探测。Homebrew 装的 ffmpeg 若不在你启动本程序的 shell 的 PATH 里，就把绝对路径填在这里。")}
            ${field("测试运行",
              `<div class="controls">
                <input type="text" id="fm-pp-test-path" class="ctrl" placeholder="待转码文件的绝对路径，含中文/空格也要能跑">
                <input type="text" id="fm-pp-test-cat" class="w-sm" placeholder="模拟分类，例：动漫">
                <input type="text" id="fm-pp-test-tags" class="w-sm" placeholder="模拟标签，例：转码,1080p">
                <button class="btn" id="fm-pp-test">运行一次</button>
              </div>`,
              "用当前命令对指定文件跑一次，展示退出码与输出尾部。<b>不会</b>触发上面的大小清理，纯粹验证命令本身。分类/标签是选填的模拟值：会原样交给命令的 <code>$CATEGORY</code> / <code>$TAGS</code>，方便验证脚本里按分类、标签分派的分支；结果里另外回报<b>按上面的名单这一组会不会被转码</b>（只是回报，本次命令照样跑；名单改动要先保存才参与判定）。")}
            <pre class="pp-out" id="fm-pp-out"></pre>
          `,
        })}
      `,
    })}

    <!-- 保存 -->
    ${UI.saveBar(
      "保存前会先做一次本地校验（URL 协议、正则、间隔下限等）。",
      UI.btn("保存全部设置", { variant: "primary", iconName: "save", id: "save-all", attrs: 'data-save="settings"' })
    )}
  `;

  // 首次渲染 limiter 规则与 AI 通道
  renderLimiterRules();
  renderAIChannels();
  renderCategoryRules();
  bindCategoryEvents(root);
  syncDirtyUI(); // 重渲后重建了保存按钮，未保存标记要重新贴一遍

  // --- 主题按钮高亮 + 事件 ---
  applyTheme(getTheme());
  $$("[data-theme-opt]", root).forEach(b => {
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

  // 代理：仅手动模式启用地址输入，实时提示生效范围
  const proxyMode = $("#proxy-mode"), proxyUrl = $("#proxy-url"), proxyStatus = $("#proxy-status");
  const syncProxyUI = () => {
    const m = proxyMode.value;
    proxyUrl.disabled = m !== "manual";
    proxyStatus.textContent = m === "off"
      ? "直连：忽略系统代理与环境变量。"
      : m === "system"
        ? "跟随 macOS 系统代理，保存后立即对后续请求生效（本机回环地址始终直连）。"
        : "手动代理，保存后立即对后续请求生效（本机回环地址始终直连）。";
  };
  proxyMode.addEventListener("change", syncProxyUI);
  syncProxyUI();

  $("#lim-add").onclick = () => {
    syncFormToCfg();
    _settingsCfg.limiter.rules.push({ id: genID(), name: `规则 ${(_settingsCfg.limiter.rules.length + 1)}`, enabled: true, match: "", uploadLimit: 0, ratioTarget: 0, minUploaded: 0 });
    markDirty("settings");
    renderLimiterRules();
  };

  $$("[data-lm-del]").forEach(b => b.addEventListener("click", onLimDel));

  $("#cat-add").onclick = () => {
    syncFormToCfg();
    const rs = _settingsCfg.autoCategory.rules;
    rs.push({ id: genID(), name: `规则 ${rs.length + 1}`, enabled: true, category: "", when: [] });
    markDirty("settings");
    renderCategoryRules();
  };

  $("#cat-default").onclick = () => {
    syncFormToCfg();
    _settingsCfg.autoCategory.rules = catDefaultRules();
    markDirty("settings");
    renderCategoryRules();
    toast("已载入内置三条：编号式 → 英文长名 → 中文长名，分类名请改成你在用的分类");
  };

  $("#cat-preview").onclick = async () => {
    syncFormToCfg();
    const names = ($("#cat-preview-names").value || "").split("\n").map(s => s.trim()).filter(Boolean);
    const out = $("#cat-preview-out");
    out.innerHTML = UI.loading("试算中…");
    // 带上 config：让后端按表单里尚未保存的规则求值
    const r = await api("POST", "/category/preview", { names, limit: 20, config: _settingsCfg.autoCategory });
    if (!r.success) {
      out.innerHTML = UI.notice(escapeHTML(r.message || "试算失败"), "warn");
      return;
    }
    const items = r.data || [];
    out.innerHTML = items.length === 0
      ? UI.notice("没有可试算的任务", "")
      : UI.table({
          className: "cat-preview",
          head: ["任务名", "当前分类", "命中规则", "将设为", "说明"],
          rows: items.map(it => `<div class="data-row">
            <span class="data-cell" title="${escapeHTML(it.name)}">${escapeHTML(it.name)}</span>
            <span class="data-cell">${escapeHTML(it.current || "-")}</span>
            <span class="data-cell">${escapeHTML(it.rule || "-")}</span>
            <span class="data-cell">${it.target ? escapeHTML(it.target) : "-"}</span>
            <span class="data-cell text-dim">${escapeHTML(it.reason || "")}</span>
          </div>`),
        });
  };

  $("#fm-pp-test").onclick = async () => {
    const path = ($("#fm-pp-test-path").value || "").trim();
    if (!path) { toast("请先填写测试文件的绝对路径", "err"); return; }
    const out = $("#fm-pp-out");
    out.textContent = "执行中…";
    const r = await api("POST", "/filemgr/postprocess/test", {
      path,
      category: ($("#fm-pp-test-cat").value || "").trim(),
      tags: ($("#fm-pp-test-tags").value || "").trim(),
    });
    const d = r.data || {};
    out.textContent = `${d.filter || ""}\n退出码：${d.exitCode || "-"}\n${d.output || r.message || ""}`;
    toast(r.success ? "测试执行成功" : (r.message || "测试执行失败"), r.success ? "ok" : "err");
  };

  $("#ai-add").onclick = () => {
    syncFormToCfg();
    const chans = _settingsCfg.fileManager.aiChannels;
    const ch = { id: genID(), name: `通道 ${chans.length + 1}`, baseURL: "", apiKey: "", model: "", prompt: "", enabled: true };
    chans.push(ch);
    _settingsCfg.fileManager.aiActive = ch.id;
    markDirty("settings");
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
    const res = await api("POST", "/config?only=qbittorrent,server,notifier,limiter,fileManager,proxy,autoCategory", out);
    if (res.success) clearDirty("settings");
    toast(res.success ? "设置已保存" : (res.message || "保存失败"), res.success ? "ok" : "err");
    // 保存成功后重新 GET 一次，让密码/API key 回到掩码状态
    if (res.success) {
      const fresh = (await api("GET", "/config")).data;
      _settingsCfg = fresh;
    }
  };

  // 本页每次都从服务端重新载入，载入完成即基准状态
  clearDirty("settings");
}

function onLimDel() {
  syncFormToCfg();
  const idx = parseInt(this.dataset.lmDel, 10);
  _settingsCfg.limiter.rules.splice(idx, 1);
  markDirty("settings");
  renderLimiterRules();
}

// ---------- 文件（重命名审计与回退）----------
const _filesState = { page: 1, pageSize: 20 };

const AUDIT_STATUS = {
  committed:  { t: "已提交", c: "warn" },
  confirmed:  { t: "已生效", c: "good" },
  failed:     { t: "失败",   c: "bad" },
  rolledback: { t: "已回退", c: "muted" },
  pruned:    { t: "已删除", c: "bad" },
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
    <div id="audit-card">${UI.loading("加载审计日志…")}</div>`;
  bindBatchCard(root);
  await renderAudit($("#audit-card", root));
}

// ---------- 批量重命名（模板）----------

const BR_VAR_HINT = "<code>{date}</code> 完成日期 · <code>{type}</code> 类型（video / audio / image / archive / doc / other） · <code>{index}</code> 任务内序号 · <code>{orig}</code> 原文件名 · <code>{title}</code> 任务名";

function batchCard() {
  return `
    ${UI.page({
      eyebrow: "Library",
      title: "文件",
      desc: "按模板批量重命名，并查看、回退自动改名记录。",
    })}
    ${UI.panel({
      title: "批量重命名",
      desc: "先预览再执行；目标同名时自动加后缀兜底，绝不覆盖",
      iconName: "edit",
      actions: `${UI.btn("预览", { size: "sm", iconName: "eye", id: "br-preview" })}${UI.btn("执行重命名", { size: "sm", variant: "primary", iconName: "check", id: "br-apply", attrs: "disabled" })}`,
      body: `
        ${field("任务", `<select id="br-torrent" class="ctrl"><option value="">加载中…</option></select>`)}
        ${field("模板",
          `<input type="text" id="br-template" class="ctrl mono" placeholder="{date}_{type}_{index}_{orig}" />`,
          `变量：${BR_VAR_HINT}。仅重命名文件名（扩展名保留），目录名不变；<code>{index}</code> 按任务文件列表从 1 起编，执行时按当前列表重新计算。`)}
        <div id="br-result"></div>
      `,
    })}`;
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
    resultBox.innerHTML = UI.loading("生成预览中…");
    const r = await api("POST", "/filemgr/batch/preview", { hash, template: tpl });
    if (!r.success) {
      lastPlan = null; applyBtn.disabled = true;
      resultBox.innerHTML = notice(`预览失败：${escapeHTML(r.message || "未知错误")}`, "danger");
      return;
    }
    lastPlan = r.data.plan || [];
    if (lastPlan.length === 0) {
      applyBtn.disabled = true;
      resultBox.innerHTML = UI.empty("没有需要重命名的文件（模板产出与现名一致，或任务无文件）。", { iconName: "check-circle" });
      return;
    }
    const rows = lastPlan.map(p => `
      <div class="rename-row">
        <span class="rename-old" title="${escapeHTML(p.old)}">${escapeHTML(p.old)}</span>
        <span class="rename-new" title="${escapeHTML(p.new)}"><span class="rename-arrow">→</span>${escapeHTML(p.new)}</span>
      </div>`).join("");
    resultBox.innerHTML = `
      <div class="section">
        <div class="section-head">
          <div class="section-heading"><h3>预览结果<span class="text-dim">共 ${lastPlan.length} 条</span></h3></div>
        </div>
        <div class="rename-list">
          <div class="rename-head"><span>当前文件名</span><span>新文件名</span></div>
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
    const st = AUDIT_STATUS[e.status] || { t: e.status, c: "muted" };
    const canRollback = e.status === "committed" || e.status === "confirmed";
    return `
      <div class="data-row audit-row">
        <span class="data-cell data-cell--time">${fmtTime(e.ts)}</span>
        <span class="data-cell" title="${escapeHTML(e.torrent)}">${escapeHTML(e.torrent)}</span>
        <span class="data-cell audit-change" title="${escapeHTML(e.old)} → ${escapeHTML(e.new)}">
          ${escapeHTML(e.old)}<span class="audit-arrow">→</span><b>${escapeHTML(e.new)}</b>
        </span>
        <span class="data-cell data-cell--via text-dim">${e.via === "ai" ? "AI" : e.via === "prune" ? "后处理清理" : e.via === "local-move" ? "本地归档" : e.via === "manual" ? "手动" : "正则"}</span>
        <span class="data-cell data-cell--status">${UI.tag(st.t, st.c)}</span>
        <span class="data-cell data-cell--act">${canRollback
          ? UI.btn("回退", { size: "xs", attrs: `data-audit-rollback="${escapeHTML(e.id)}"` })
          : ""}</span>
      </div>`;
  });

  const body = entries.length === 0
    ? UI.empty("暂无重命名记录。启用「设置 → 文件管理 → 文件名自动清理」后，改动会记录在这里。", { iconName: "files" })
    : `${UI.table({
        head: [
          { label: "时间", cls: "data-cell--time" },
          { label: "任务" },
          { label: "文件名变更" },
          { label: "方式", cls: "data-cell--via" },
          { label: "状态", cls: "data-cell--status" },
          { label: "", cls: "data-cell--act" },
        ],
        rows,
        className: "table--audit",
      })}${pageNav(total, _filesState.page, _filesState.pageSize)}`;

  root.innerHTML = UI.panel({
    title: "文件名审计日志",
    desc: "自动与批量改名记录，保留最近 2000 条",
    iconName: "clock",
    actions: `<span class="meta-text">共 ${total} 条</span>${UI.btn("刷新", { size: "sm", iconName: "refresh", id: "audit-reload" })}`,
    body,
  });

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

// ---------- 日志 ----------
// 后端只有一份内存环形缓冲（logger.RingSize），级别与关键字筛选在这里做，
// 免得切换筛选时漏掉别的级别。
const LOG_CAP = 2000;
const LOG_LEVELS = [
  { v: "", t: "全部" },
  { v: "debug", t: "调试" },
  { v: "info", t: "信息" },
  { v: "warn", t: "警告" },
  { v: "error", t: "错误" },
];
const LOG_LEVEL_NAME = { debug: "DEBUG", info: "INFO", warn: "WARN", error: "ERROR" };

const _logsState = { lines: [], cursor: 0, boot: "", level: "", q: "", follow: true, busy: false, timer: null };

function logMatches(e) {
  return (!_logsState.level || e.level === _logsState.level) &&
    (!_logsState.q || (e.msg || "").toLowerCase().includes(_logsState.q));
}

// fmtLogTime：日志行里的时间戳。列表按秒对齐已经够用（行序由 cursor 保证），
// 只到「日」是为了长列表里每行宽度一致，不用反复读年份。
function fmtLogTime(ts) {
  const d = new Date(ts * 1000);
  if (isNaN(d.getTime())) return "-";
  const p = n => String(n).padStart(2, "0");
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function logRow(e) {
  const name = LOG_LEVEL_NAME[e.level] || String(e.level || "").toUpperCase();
  return `<div class="log-line lv-${escapeHTML(e.level)}">
    <span class="log-time">${fmtLogTime(e.ts)}</span>
    <span class="log-level">${escapeHTML(name)}</span>
    <span class="log-msg">${escapeHTML(e.msg)}</span>
  </div>`;
}

function paintLogs() {
  const box = $("#lg-body");
  if (!box) return;
  const rows = _logsState.lines.filter(logMatches);
  const keep = box.scrollTop;
  box.innerHTML = rows.length === 0
    ? UI.empty(_logsState.lines.length ? "没有匹配的日志" : "暂无日志", { iconName: "logs" })
    : rows.map(logRow).join("");
  box.scrollTop = _logsState.follow ? box.scrollHeight : keep;
  const count = $("#lg-count");
  if (count) count.textContent = `${rows.length} / ${_logsState.lines.length} 条`;
}

// pullLogs：full=true 重新拉整个缓冲，否则从上次 cursor 往后增量拉。
// 服务重启后 Seq 从头计数，增量拉会拿回空列表，所以 BootID 一变就整份重拉。
async function pullLogs(full) {
  if (_logsState.busy) return;
  _logsState.busy = true;
  let redo = false;
  try {
    const r = await api("GET", `/logs?since=${full ? 0 : _logsState.cursor}&limit=${LOG_CAP}`);
    const slot = $("#lg-notice");
    if (!r.success) {
      if (slot) slot.innerHTML = notice(`日志加载失败：${escapeHTML(r.message || "未知错误")}`, "danger");
      return;
    }
    if (slot) slot.innerHTML = "";
    const d = r.data || {};
    const entries = Array.isArray(d.entries) ? d.entries : [];
    const cursor = Number(d.cursor) || 0;
    const boot = d.boot || "";
    if (full) {
      _logsState.lines = entries;
    } else if (boot !== _logsState.boot) {
      redo = true;
    } else {
      _logsState.lines = _logsState.lines.concat(entries);
    }
    _logsState.boot = boot;
    _logsState.cursor = cursor;
    // 本地列表不会超过服务端缓冲，除了增量拉之间缓冲已溢出淘汰的头部几条
    if (_logsState.lines.length > LOG_CAP) {
      _logsState.lines.splice(0, _logsState.lines.length - LOG_CAP);
    }
    if ((full || entries.length) && !redo) paintLogs();
  } finally {
    _logsState.busy = false;
  }
  if (redo) await pullLogs(true);
}

async function renderLogs(root) {
  if (_logsState.timer) { clearInterval(_logsState.timer); _logsState.timer = null; }
  const levels = UI.segmented(LOG_LEVELS.map(o => ({
    label: o.t,
    active: o.v === _logsState.level,
    attrs: `data-log-level="${o.v}"`,
  })), { className: "log-levels", ariaLabel: "日志级别" });

  root.innerHTML = `
    ${UI.page({
      eyebrow: "System",
      title: "日志",
      desc: `服务进程内存中的运行日志，最多保留最近 ${LOG_CAP} 条；重启服务后从头开始。`,
      actions: `${UI.btn("刷新", { size: "sm", iconName: "refresh", id: "lg-reload" })}${UI.btn("导出", { size: "sm", variant: "ghost", iconName: "download", id: "lg-export" })}`,
    })}
    ${UI.panel({
      title: "运行日志",
      desc: "每 2 秒增量拉取，仅保留进程内存中的最近记录",
      iconName: "logs",
      body: `
        ${UI.toolbar([
          `<div class="tool-field"><span class="tool-label">级别</span>${levels}</div>`,
          UI.toolField("搜索", `<input type="text" id="lg-q" class="control" value="${escapeHTML(_logsState.q)}" placeholder="日志内容关键字" />`, { grow: true }),
          `<div class="tool-field"><span class="tool-label">跟随</span>${toggle(_logsState.follow, { id: "lg-follow", label: "滚到底部", sm: true })}</div>`,
        ])}
        <div class="notice-slot" id="lg-notice"></div>
        <div class="console">
          <div class="console-head">
            <span class="live-dot" aria-hidden="true"></span>
            <span class="console-title">live stream</span>
            <span class="console-count" id="lg-count"></span>
          </div>
          <div class="console-body" id="lg-body">${UI.loading("连接日志流…")}</div>
        </div>
      `,
    })}`;

  $$("[data-log-level]", root).forEach(b => b.onclick = () => {
    _logsState.level = b.dataset.logLevel;
    $$("[data-log-level]", root).forEach(x => x.classList.toggle("is-active", x === b));
    paintLogs();
  });
  $("#lg-q", root).oninput = debounce(e => {
    _logsState.q = e.target.value.trim().toLowerCase();
    paintLogs();
  }, 200);
  $("#lg-follow", root).onchange = e => {
    _logsState.follow = e.target.checked;
    if (_logsState.follow) paintLogs();
  };
  $("#lg-reload", root).onclick = () => pullLogs(true);
  $("#lg-export", root).onclick = () => {
    const rows = _logsState.lines.filter(logMatches);
    if (rows.length === 0) return toast("没有可导出的日志", "err");
    const text = rows.map(e =>
      `${fmtTime(e.ts)} ${(LOG_LEVEL_NAME[e.level] || (e.level || "").toUpperCase()).padEnd(5)} ${e.msg}`
    ).join("\n");
    downloadText(`qbhive-日志-${new Date().toISOString().slice(0, 10)}.txt`, text);
  };

  await pullLogs(true);
  _logsState.timer = setInterval(() => {
    if (currentView !== "logs") { clearInterval(_logsState.timer); _logsState.timer = null; return; }
    pullLogs(false);
  }, 2000);
}

// ---------- 状态 ----------
function setConnState(tone, text, pillText) {
  const card = $("#conn-card"), state = $("#qb-status"), pill = $("#mobile-status");
  if (card) card.dataset.tone = tone;
  if (state) state.textContent = text;
  if (pill) {
    pill.dataset.tone = tone;
    pill.textContent = pillText;
  }
}

async function updateStatus() {
  try {
    const r = await api("GET", "/torrents/stats");
    if (r.success) {
      const s = r.data || {};
      const active = s.activeCount || 0;
      setConnState("ok", `已连接 · 活跃 ${active}`, `活跃 ${active}`);
    } else {
      setConnState("bad", "未连接", "未连接");
    }
  } catch {
    setConnState("bad", "服务异常", "服务异常");
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
