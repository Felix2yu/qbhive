// QBHive — Hive Console 设计系统
// 全站唯一的组件工厂：页面骨架、面板、字段、按钮、标签、表格、状态与图标都在这里定义。
// app.js 只负责数据与事件，禁止再手写一套控件样式。
const UI = (() => {
  const ESC = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };
  const escape = (s) => (s == null ? "" : String(s).replace(/[&<>"']/g, (c) => ESC[c]));

  const ICONS = {
    overview: '<rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/>',
    torrents: '<path d="M9 6h12M9 12h12M9 18h12"/><circle cx="4.5" cy="6" r="1.2"/><circle cx="4.5" cy="12" r="1.2"/><circle cx="4.5" cy="18" r="1.2"/>',
    files: '<path d="M3 7.5A2.5 2.5 0 0 1 5.5 5h3.2l2 2.2h7.8A2.5 2.5 0 0 1 21 9.7v8.8a2.5 2.5 0 0 1-2.5 2.5h-13A2.5 2.5 0 0 1 3 18.5z"/>',
    rss: '<path d="M4 11a9 9 0 0 1 9 9"/><path d="M4 4a16 16 0 0 1 16 16"/><circle cx="5" cy="19" r="1.1"/>',
    logs: '<rect x="3" y="4" width="18" height="16" rx="2"/><path d="m7 9 3 3-3 3M13 15h4"/>',
    settings: '<path d="M6 4v5M6 15v5M12 4v9M12 19v1M18 4v1M18 11v9"/><circle cx="6" cy="12" r="2"/><circle cx="12" cy="16" r="2"/><circle cx="18" cy="8" r="2"/>',
    refresh: '<path d="M20.5 12a8.5 8.5 0 1 1-2.5-6M20.5 3.5V9H15"/>',
    plus: '<path d="M12 5v14M5 12h14"/>',
    play: '<path d="M8 5.4v13.2L18.5 12z"/>',
    pause: '<path d="M9 5v14M15 5v14"/>',
    check: '<path d="m5 12.5 4.3 4.3L19 7"/>',
    "check-circle": '<circle cx="12" cy="12" r="9"/><path d="m8.4 12.2 2.4 2.4 4.8-5.2"/>',
    alert: '<path d="M12 3.2 2.6 20h18.8z"/><path d="M12 9v5M12 17.2v.6"/>',
    info: '<circle cx="12" cy="12" r="9"/><path d="M12 11v5M12 7.8v.6"/>',
    close: '<path d="M6 6l12 12M18 6 6 18"/>',
    trash: '<path d="M4 7h16M9 7V5h6v2M6.5 7l1 13h9l1-13M10 11v5M14 11v5"/>',
    edit: '<path d="M4 20h4L20 8l-4-4L4 16z"/><path d="m13.5 6.5 4 4"/>',
    gauge: '<path d="M4 18a8 8 0 1 1 16 0"/><path d="m12 14 4.2-4.2"/><circle cx="12" cy="18" r="1"/>',
    search: '<circle cx="11" cy="11" r="7"/><path d="m20 20-3.6-3.6"/>',
    download: '<path d="M12 4v11M7 10l5 5 5-5M5 20h14"/>',
    upload: '<path d="M12 20V9M7 14l5-5 5 5M5 4h14"/>',
    sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>',
    moon: '<path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/>',
    monitor: '<rect x="3" y="4" width="18" height="13" rx="2"/><path d="M8 21h8M12 17v4"/>',
    external: '<path d="M14 4h6v6M20 4l-9 9"/><path d="M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/>',
    arrow: '<path d="M5 12h14M13 6l6 6-6 6"/>',
    filter: '<path d="M4 5h16l-6 7v5l-4 2v-7z"/>',
    save: '<path d="M5 3h11l3 3v15H5z"/><path d="M8 3v6h8V3M8 14h8v7H8z"/>',
    database: '<ellipse cx="12" cy="5" rx="7" ry="3"/><path d="M5 5v14c0 1.7 3.1 3 7 3s7-1.3 7-3V5"/><path d="M5 12c0 1.7 3.1 3 7 3s7-1.3 7-3"/>',
    activity: '<path d="M3 12h4l2-7 4 14 2-7h6"/>',
    zap: '<path d="M13 2 4 14h7l-1 8 9-12h-7z"/>',
    tag: '<path d="M3 12V4h8l10 10-8 8z"/><circle cx="7.5" cy="7.5" r="1.2"/>',
    clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
    shield: '<path d="M12 3 5 6v5c0 5 3.5 8.5 7 10 3.5-1.5 7-5 7-10V6z"/><path d="m9 12 2 2 4-4"/>',
    sparkle: '<path d="m12 3 1.8 5.2L19 10l-5.2 1.8L12 17l-1.8-5.2L5 10l5.2-1.8z"/><path d="M19 16l.8 2.2L22 19l-2.2.8L19 22l-.8-2.2L16 19l2.2-.8z"/>',
    list: '<path d="M9 6h12M9 12h12M9 18h12"/><circle cx="4.5" cy="6" r="1.2"/><circle cx="4.5" cy="12" r="1.2"/><circle cx="4.5" cy="18" r="1.2"/>',
    inbox: '<path d="M4 4h16v16H4z"/><path d="M4 13h5l2 3h2l2-3h5"/>',
    link: '<path d="M10 13a5 5 0 0 0 7 0l3-3a5 5 0 0 0-7-7l-1 1"/><path d="M14 11a5 5 0 0 0-7 0l-3 3a5 5 0 0 0 7 7l1-1"/>',
    "chevron-down": '<path d="m6 9 6 6 6-6"/>',
    "chevron-left": '<path d="m15 6-6 6 6 6"/>',
    "chevron-right": '<path d="m9 6 6 6-6 6"/>',
    eye: '<path d="M2.5 12s3.5-6 9.5-6 9.5 6 9.5 6-3.5 6-9.5 6-9.5-6-9.5-6z"/><circle cx="12" cy="12" r="2.6"/>',
    code: '<path d="m8 7-5 5 5 5M16 7l5 5-5 5"/>',
    wifi: '<path d="M4.5 9.5a12 12 0 0 1 15 0M7.5 12.8a8 8 0 0 1 9 0M10.4 16a4 4 0 0 1 3.2 0"/><circle cx="12" cy="19.2" r="1"/>',
  };

  function icon(name, size = 18) {
    const path = ICONS[name];
    if (!path) return "";
    return `<svg class="i" width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${path}</svg>`;
  }

  function page({ eyebrow = "", title = "", desc = "", actions = "" }) {
    return `<header class="view-head">
      <div class="view-heading">
        ${eyebrow ? `<span class="eyebrow">${escape(eyebrow)}</span>` : ""}
        <h1>${title}</h1>
        ${desc ? `<p class="view-desc">${desc}</p>` : ""}
      </div>
      ${actions ? `<div class="view-actions">${actions}</div>` : ""}
    </header>`;
  }

  function panel({ title = "", desc = "", actions = "", body = "", footer = "", id = "", className = "", iconName = "", flush = false }) {
    const head = title || desc || actions
      ? `<header class="panel-head">
          <div class="panel-heading">
            ${iconName ? `<span class="panel-glyph">${icon(iconName, 17)}</span>` : ""}
            <div class="panel-heading-text">
              ${title ? `<h2>${title}</h2>` : ""}
              ${desc ? `<p>${desc}</p>` : ""}
            </div>
          </div>
          ${actions ? `<div class="panel-actions">${actions}</div>` : ""}
        </header>`
      : "";
    return `<section class="panel${className ? ` ${className}` : ""}"${id ? ` id="${id}"` : ""}>
      ${head}
      <div class="panel-body${flush ? " panel-body--flush" : ""}">${body}</div>
      ${footer ? `<footer class="panel-foot">${footer}</footer>` : ""}
    </section>`;
  }

  function section({ title = "", desc = "", actions = "", body = "" }) {
    const head = title || desc || actions
      ? `<div class="section-head">
          <div class="section-heading">
            ${title ? `<h3>${title}</h3>` : ""}
            ${desc ? `<p>${desc}</p>` : ""}
          </div>
          ${actions ? `<div class="section-actions">${actions}</div>` : ""}
        </div>`
      : "";
    return `<div class="section">${head}<div class="section-body">${body}</div></div>`;
  }

  function field(label, control, hint = "") {
    return `<div class="field">
      <div class="field-label${label ? "" : " field-label--empty"}">${label || ""}</div>
      <div class="field-main">${control}${hint ? `<p class="field-hint">${hint}</p>` : ""}</div>
    </div>`;
  }

  function toggle(checked, { id = "", data = "", label = "", sm = false } = {}) {
    const attrs = [id ? `id="${id}"` : "", data, checked ? "checked" : ""].filter(Boolean).join(" ");
    return `<label class="switch${sm ? " switch--sm" : ""}">
      <input type="checkbox" ${attrs}>
      <span class="switch-track" aria-hidden="true"></span>
      ${label ? `<span class="switch-text">${label}</span>` : ""}
    </label>`;
  }

  function btn(label, { variant = "", size = "", iconName = "", attrs = "", id = "", className = "", title = "", type = "button" } = {}) {
    const cls = ["btn", variant ? `btn--${variant}` : "", size ? `btn--${size}` : "", className].filter(Boolean).join(" ");
    return `<button type="${type}" class="${cls}"${id ? ` id="${id}"` : ""}${title ? ` title="${escape(title)}"` : ""}${attrs ? ` ${attrs}` : ""}>${iconName ? icon(iconName, size === "sm" ? 15 : 16) : ""}${label ? `<span>${label}</span>` : ""}</button>`;
  }

  function iconBtn(iconName, { title = "", attrs = "", variant = "", size = "" } = {}) {
    const cls = ["icon-btn", variant ? `icon-btn--${variant}` : "", size ? `icon-btn--${size}` : ""].filter(Boolean).join(" ");
    return `<button type="button" class="${cls}" title="${escape(title)}" aria-label="${escape(title)}"${attrs ? ` ${attrs}` : ""}>${icon(iconName, size === "sm" ? 15 : 16)}</button>`;
  }

  function tag(text, tone = "", { dot = false } = {}) {
    return `<span class="tag${tone ? ` tag--${tone}` : ""}">${dot ? '<span class="tag-dot" aria-hidden="true"></span>' : ""}${text}</span>`;
  }

  function notice(text, tone = "") {
    const ic = tone === "danger" || tone === "warn" ? "alert" : tone === "ok" ? "check-circle" : "info";
    return `<div class="notice${tone ? ` notice--${tone}` : ""}" role="note">${icon(ic, 16)}<div class="notice-body">${text}</div></div>`;
  }

  function empty(text, { iconName = "inbox", action = "" } = {}) {
    return `<div class="empty">
      <span class="empty-glyph">${icon(iconName, 22)}</span>
      <p>${text}</p>
      ${action ? `<div class="empty-action">${action}</div>` : ""}
    </div>`;
  }

  function loading(text = "加载中…") {
    return `<div class="loading" role="status"><span class="spinner" aria-hidden="true"></span><span>${text}</span></div>`;
  }

  function toolbar(items, { className = "" } = {}) {
    return `<div class="toolbar${className ? ` ${className}` : ""}">${items.join("")}</div>`;
  }

  function toolField(label, control, { grow = false } = {}) {
    return `<label class="tool-field${grow ? " tool-field--grow" : ""}">
      <span class="tool-label">${label}</span>
      ${control}
    </label>`;
  }

  function segmented(items, { className = "", ariaLabel = "" } = {}) {
    return `<div class="segmented${className ? ` ${className}` : ""}" role="group"${ariaLabel ? ` aria-label="${escape(ariaLabel)}"` : ""}>
      ${items.map((it) => `<button type="button" class="segmented-item${it.active ? " is-active" : ""}"${it.attrs ? ` ${it.attrs}` : ""}${it.title ? ` title="${escape(it.title)}"` : ""}>${it.iconName ? icon(it.iconName, 15) : ""}<span>${it.label}</span></button>`).join("")}
    </div>`;
  }

  function themeSwitch(mode) {
    return `<div class="segmented theme-switch" role="group" aria-label="主题模式">
      ${[["auto", "跟随系统", "monitor"], ["dark", "深色", "moon"], ["light", "浅色", "sun"]]
        .map(([value, label, ic]) => `<button type="button" class="segmented-item${mode === value ? " is-active" : ""}" data-theme-opt="${value}" title="${label}">${icon(ic, 15)}<span>${label}</span></button>`)
        .join("")}
    </div>`;
  }

  function meter(pct, tone = "") {
    const p = Math.max(0, Math.min(100, Number(pct) || 0));
    return `<div class="meter${tone ? ` meter--${tone}` : ""}" role="progressbar" aria-valuenow="${p}" aria-valuemin="0" aria-valuemax="100"><span style="width:${p}%"></span></div>`;
  }

  function stat({ label, value, meta = "", tone = "", iconName = "" }) {
    return `<div class="stat${tone ? ` stat--${tone}` : ""}">
      <div class="stat-top">
        <span class="stat-label">${label}</span>
        ${iconName ? `<span class="stat-glyph">${icon(iconName, 16)}</span>` : ""}
      </div>
      <div class="stat-value">${value}</div>
      ${meta ? `<div class="stat-meta">${meta}</div>` : ""}
    </div>`;
  }

  function table({ head, rows, className = "" }) {
    const headHtml = `<div class="data-row data-row--head">${head
      .map((h) => {
        if (typeof h === "string") return `<span class="data-cell">${h}</span>`;
        return `<span class="data-cell${h.cls ? ` ${h.cls}` : ""}">${h.label}</span>`;
      })
      .join("")}</div>`;
    return `<div class="table-wrap"><div class="data-table${className ? ` ${className}` : ""}">${headHtml}${rows.join("")}</div></div>`;
  }

  function navList(items, { mobile = false } = {}) {
    let lastGroup = "";
    return items
      .map((it) => {
        let groupHtml = "";
        if (!mobile && it.group && it.group !== lastGroup) {
          lastGroup = it.group;
          groupHtml = `<span class="nav-group">${it.group}</span>`;
        }
        return `${groupHtml}<button type="button" class="nav-item" data-view="${it.view}"${mobile ? ' aria-label="' + escape(it.label) + '"' : ""}>
          <span class="nav-icon">${icon(it.iconName, mobile ? 21 : 19)}</span>
          <span class="nav-text">${it.label}</span>
          <span class="nav-dot" aria-hidden="true"></span>
        </button>`;
      })
      .join("");
  }

  // 长表单底部吸底保存栏：RSS / 设置共用同一结构
  function saveBar(hint, actions) {
    return `<div class="save-bar">
      ${hint ? `<span class="hint">${hint}</span>` : ""}
      <span class="spacer"></span>
      ${actions}
    </div>`;
  }

  return {
    escape,
    icon,
    page,
    panel,
    section,
    field,
    toggle,
    btn,
    iconBtn,
    tag,
    notice,
    empty,
    loading,
    toolbar,
    toolField,
    segmented,
    themeSwitch,
    meter,
    stat,
    table,
    navList,
    saveBar,
  };
})();

window.UI = UI;
