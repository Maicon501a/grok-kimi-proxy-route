import "./style.css";
import "./app.css";

import { marked } from "marked";
import DOMPurify from "dompurify";

import {
  GetBootstrap,
  ListModels,
  ListAccountsForProvider,
  ListProviders,
  SetActiveAccount,
  RemoveAccount,
  RenameAccount,
  StartDeviceLogin,
  CancelDeviceLogin,
  OpenExternal,
  UpdateSettings,
  SendChat,
  CancelChat,
  GetStats,
  StartAutoSignup,
  CancelAutoSignup,
  IsSignupRunning,
  SetAutoCreateOnExhausted,
  GetAutoCreateOnExhausted,
  GetGoogleCredentials,
  SetGoogleCredentials,
  GetAccountGoogleCredentials,
  SetAccountGoogleCredentials,
  StartKimiBrowserLogin,
  StartKimiStealthLoginNewAccount,
  AddKimiFromJWT,
  AddKimiAPIKey,
  LogoffKimiAccount,
} from "../wailsjs/go/main/App";
import { openStatsModal } from "./stats.js";
import { EventsOn } from "../wailsjs/runtime/runtime";

// Markdown like ChatGPT: GFM, breaks for soft newlines
marked.setOptions({
  gfm: true,
  breaks: true,
  pedantic: false,
});

const state = {
  settings: {},
  accounts: [],
  models: [],
  usage: {},
  activeRequest: null,
  proxyBase: "",
  dataDir: "",
  messages: [],
  streaming: false,
  lastResponseId: null,
  device: null,
  shellBuilt: false,
  sessionCost: 0,
  sessionLat: null,
  logs: [],
  // custom dropdowns
  picks: {
    effort: "high",
    api: "chat",
    model: "grok-4.5",
    cEffort: "high",
    cApi: "chat",
    cModel: "grok-4.5",
  },
  menus: {},
};

/** Short label for long model ids (Ollie full paths, aliases, etc.). */
function shortModelLabel(name, id) {
  let s = String(name || id || "").trim();
  if (!s) return "—";
  // "OllieChat alias → accounts/.../foo" → prefer the id short form
  const arrow = s.indexOf("→");
  if (arrow >= 0) s = s.slice(arrow + 1).trim();
  // accounts/euromodels/models/claude-fable-5 → claude-fable-5
  if (s.includes("/")) {
    const parts = s.split("/").filter(Boolean);
    s = parts[parts.length - 1] || s;
  }
  // drop noisy prefixes
  s = s.replace(/^models\//i, "");
  // keep chip readable
  if (s.length > 28) s = s.slice(0, 26) + "…";
  return s;
}

/** Custom dark menu — replaces native <select> (white OS list on Windows). */
function mountMenu(root, { id, options, value, prefix, onChange, chip }) {
  root.className = "dd" + (chip ? " seg dd-chip" : "");
  root.dataset.menuId = id;
  const optList = () =>
    typeof options === "function" ? options() : options;

  const render = () => {
    const opts = optList();
    const cur = opts.find((o) => o.value === root._value) || opts[0];
    if (cur && root._value !== cur.value) root._value = cur.value;
    const label = cur?.label || root._value || "—";
    const title = cur?.value && cur.value !== label ? `${label} (${cur.value})` : label;
    root.innerHTML = `
      <button type="button" class="dd-trigger" aria-haspopup="listbox" title="${escapeHtml(title)}">
        <span class="dd-value">${prefix ? `<span class="dd-label">${escapeHtml(prefix)}</span> ` : ""}${escapeHtml(label)}</span>
        <span class="dd-chev"></span>
      </button>
      <div class="dd-menu" role="listbox"></div>
    `;
    const menu = root.querySelector(".dd-menu");
    opts.forEach((o) => {
      const item = document.createElement("button");
      item.type = "button";
      item.className = "dd-item" + (o.value === root._value ? " active" : "");
      item.role = "option";
      const itemTitle = o.value && o.value !== o.label ? o.value : o.label;
      item.title = itemTitle;
      item.innerHTML = `<span>${escapeHtml(o.label)}</span><span class="check">✓</span>`;
      item.onclick = (e) => {
        e.stopPropagation();
        root._value = o.value;
        root.classList.remove("open");
        render();
        onChange?.(o.value);
      };
      menu.appendChild(item);
    });
    root.querySelector(".dd-trigger").onclick = (e) => {
      e.stopPropagation();
      const was = root.classList.contains("open");
      closeAllMenus();
      if (!was) {
        // open upward if near bottom
        const rect = root.getBoundingClientRect();
        const spaceBelow = window.innerHeight - rect.bottom;
        menu.classList.toggle("drop-up", spaceBelow < 220);
        root.classList.add("open");
      }
    };
  };

  root._value = value;
  root.getValue = () => root._value;
  root.setValue = (v) => {
    root._value = v;
    // For account menu, display email if we can resolve it
    render();
  };
  root.setOptions = (next) => {
    if (typeof options !== "function") options = next;
    render();
  };
  root.refresh = render;
  // Account chip: show email and list all accounts to switch active
  if (id === "c-account") {
    root.refresh = () => {
      const opts = optList();
      const cur = opts.find((o) => o.value === root._value) || opts[0];
      const acc = state.accounts.find((a) => a.id === root._value);
      const display =
        acc?.email || acc?.label || cur?.label || "escolher conta";
      root.innerHTML = `
        <button type="button" class="dd-trigger" title="Clique para alternar a conta da request">
          <span class="dd-value"><span class="dd-label">conta</span> ${escapeHtml(display)}</span>
          <span class="dd-chev"></span>
        </button>
        <div class="dd-menu" role="listbox"></div>
      `;
      const menu = root.querySelector(".dd-menu");
      opts.forEach((o) => {
        const a = state.accounts.find((x) => x.id === o.value);
        const item = document.createElement("button");
        item.type = "button";
        item.className = "dd-item" + (o.value === root._value ? " active" : "");
        item.setAttribute("role", "option");
        const title = a?.email || o.label;
        const sub = a?.label && a.label !== a.email ? a.label : a?.active ? "em uso agora" : "clique para usar";
        item.innerHTML = `<span style="min-width:0"><span style="display:block;overflow:hidden;text-overflow:ellipsis">${escapeHtml(title)}</span><span style="display:block;font-size:10.5px;color:rgba(255,255,255,0.35);margin-top:2px">${escapeHtml(sub)}</span></span><span class="check">✓</span>`;
        item.onclick = (e) => {
          e.stopPropagation();
          root._value = o.value;
          root.classList.remove("open");
          root.refresh();
          onChange?.(o.value);
        };
        menu.appendChild(item);
      });
      root.querySelector(".dd-trigger").onclick = (e) => {
        e.stopPropagation();
        const was = root.classList.contains("open");
        closeAllMenus();
        if (!was) {
          const rect = root.getBoundingClientRect();
          const spaceBelow = window.innerHeight - rect.bottom;
          menu.classList.toggle("drop-up", spaceBelow < 220);
          root.classList.add("open");
        }
      };
    };
    root.setValue = (v) => {
      root._value = v;
      root.refresh();
    };
  }
  root.refresh();
  state.menus[id] = root;
  return root;
}

function closeAllMenus() {
  document.querySelectorAll(".dd.open").forEach((el) => el.classList.remove("open"));
}

document.addEventListener("click", () => closeAllMenus());
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") closeAllMenus();
});

function $(sel, root = document) {
  return root.querySelector(sel);
}

function fmt(n) {
  if (n == null) return "0";
  return Number(n).toLocaleString("en-US");
}

function fmtUSD(n) {
  const v = Number(n) || 0;
  if (v > 0 && v < 0.0001) return "<$0.0001";
  return "$" + v.toFixed(v >= 1 ? 2 : 4);
}

function fmtMs(n) {
  if (n == null || n <= 0) return "—";
  if (n < 1000) return Math.round(n) + " ms";
  return (n / 1000).toFixed(2) + " s";
}

function shortPath(p) {
  if (!p) return "";
  const s = String(p);
  return s.length <= 36 ? s : "…" + s.slice(-34);
}

function initials(s) {
  if (!s) return "?";
  const p = String(s).split(/[\s@._-]+/).filter(Boolean);
  return ((p[0]?.[0] || "?") + (p[1]?.[0] || "")).toUpperCase();
}

function escapeHtml(s) {
  return String(s ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

/** Detect upstream HTML error pages (e.g. Google robot 404) so we never paint them. */
function looksLikeHTML(s) {
  const t = String(s ?? "").trim();
  if (t.length < 12) return false;
  const head = t.slice(0, 240).toLowerCase();
  if (head.startsWith("<!doctype") || head.startsWith("<html") || head.startsWith("<head")) return true;
  if (head.includes("that's an error") || head.includes("robots.txt")) return true;
  if (t.length > 1500 && /<\/?(html|body|script|style|svg)\b/i.test(t)) return true;
  return false;
}

function safeErrorText(err) {
  const raw = String(err ?? "erro desconhecido");
  if (looksLikeHTML(raw)) {
    return "Erro: o provedor devolveu uma página HTML (não é resposta do modelo). Verifique ADC, projeto Vertex e o id do model.";
  }
  // Cap runaway error bodies
  return raw.length > 800 ? raw.slice(0, 800) + "…" : raw;
}

/** Render markdown safely for chat bubbles (assistant + optional user). */
function renderMarkdown(text) {
  const raw = String(text ?? "");
  if (!raw.trim()) return "";
  // Never paint HTML error pages through marked + innerHTML (robot page bug).
  if (looksLikeHTML(raw)) {
    return `<p class="err">${escapeHtml(safeErrorText(raw))}</p>`;
  }
  try {
    const html = marked.parse(raw, { async: false });
    return DOMPurify.sanitize(html, {
      USE_PROFILES: { html: true },
      ADD_ATTR: ["target", "rel"],
      FORBID_TAGS: ["style", "iframe", "object", "embed", "form"],
      FORBID_ATTR: ["style"],
    });
  } catch {
    return `<p>${escapeHtml(raw)}</p>`;
  }
}

function enhanceMarkdownRoot(root) {
  if (!root) return;
  // External links open safely
  root.querySelectorAll("a[href]").forEach((a) => {
    a.setAttribute("target", "_blank");
    a.setAttribute("rel", "noopener noreferrer");
  });
  // Copy button on code blocks
  root.querySelectorAll("pre").forEach((pre) => {
    if (pre.querySelector(".code-copy")) return;
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "code-copy";
    btn.textContent = "Copiar";
    btn.onclick = async (e) => {
      e.stopPropagation();
      const code = pre.querySelector("code")?.innerText || pre.innerText;
      try {
        await navigator.clipboard.writeText(code);
        btn.textContent = "Copiado";
        setTimeout(() => (btn.textContent = "Copiar"), 1200);
      } catch (_) {}
    };
    pre.appendChild(btn);
  });
}

function globalUsage() {
  return (
    state.usage?._global || {
      prompt_tokens: 0,
      completion_tokens: 0,
      reasoning_tokens: 0,
      total_tokens: 0,
      requests: 0,
    }
  );
}

function activeAccount() {
  return state.accounts.find((a) => a.active) || state.accounts[0] || null;
}

function ensureShell() {
  if (state.shellBuilt) return;
  const app = $("#app");
  app.innerHTML = `
    <div class="shell">
      <aside class="rail">
        <div class="brand">
          <div class="logo"></div>
          <div>
            <h1>Grok</h1>
            <span>Desktop proxy</span>
          </div>
        </div>

        <div>
          <div class="accounts-head">
            <div class="rail-label">Contas do provedor</div>
            <span class="accounts-count" id="accounts-count">0</span>
          </div>
          <div class="provider-mode" id="provider-mode">—</div>
          <div class="accounts" id="accounts"></div>
          <div class="rail-actions" style="margin-top:10px; gap:8px; display:flex; flex-direction:column">
            <button class="btn btn-solid" id="btn-add">+ Adicionar</button>
            <button class="btn btn-quiet" id="btn-accounts">Ver contas</button>
          </div>
        </div>

        <div class="rail-block">
          <div class="rail-label">Uso</div>
          <div class="stats">
            <div class="stat"><label>Total</label><b id="u-total">0</b></div>
            <div class="stat"><label>Custo</label><b id="u-cost">$0</b></div>
            <div class="stat"><label>Prompt</label><b id="u-prompt">0</b></div>
            <div class="stat"><label>Out</label><b id="u-comp">0</b></div>
            <div class="stat"><label>Think</label><b id="u-reason">0</b></div>
            <div class="stat"><label>Lat. méd</label><b id="u-lat">—</b></div>
          </div>
          <div class="rail-actions" style="margin-top:10px">
            <button class="btn btn-quiet" id="btn-stats">Ver mais da API</button>
          </div>
        </div>

        <div class="rail-block">
          <div class="rail-label">Global</div>
          <div class="field">
            <span class="field-label">Provedor</span>
            <div id="set-provider"></div>
          </div>
          <div class="field">
            <span class="field-label">Raciocínio</span>
            <div id="set-effort"></div>
          </div>
          <div class="field">
            <span class="field-label">API</span>
            <div id="set-api"></div>
          </div>
          <div class="field">
            <span class="field-label">Modelo</span>
            <div id="set-model"></div>
          </div>
          <div class="field" id="qwen-fields" style="display:none">
            <span class="field-label">QwenBridge URL</span>
            <input id="qwen-upstream" type="text" placeholder="http://127.0.0.1:3000/v1"
              style="width:100%;box-sizing:border-box;padding:7px 9px;border-radius:8px;border:1px solid rgba(255,255,255,.12);background:rgba(0,0,0,.3);color:#eee;font-size:12px;margin-bottom:8px" />
            <span class="field-label">Qwen API key</span>
            <input id="qwen-api-key" type="password" placeholder="API_KEY do QwenBridge"
              style="width:100%;box-sizing:border-box;padding:7px 9px;border-radius:8px;border:1px solid rgba(255,255,255,.12);background:rgba(0,0,0,.3);color:#eee;font-size:12px;margin-bottom:8px" />
            <button class="btn btn-quiet" id="qwen-save" type="button">Salvar Qwen</button>
          </div>
        </div>

        <div class="rail-foot">
          <b>Provedor</b>
          <span id="provider-label">—</span><br /><br />
          <b>Proxy</b>
          <span id="proxy-url">—</span><br /><br />
          <b>AppData</b>
          <span id="data-dir">—</span>
        </div>
      </aside>

      <main class="stage">
        <header class="top">
          <div class="status" id="status">
            <span class="dot-live"></span>
            <span id="status-text">Pronto</span>
          </div>
          <div class="token-live">
            <span>in <b id="sess-in">0</b></span>
            <span>out <b id="sess-out">0</b></span>
            <span>think <b id="sess-think">0</b></span>
            <span class="cost" id="sess-cost">$0</span>
            <span id="sess-lat" style="display:none"></span>
            <button class="icon-btn" id="btn-logs" type="button">Logs</button>
            <button class="icon-btn" id="btn-stats-top" type="button">API</button>
          </div>
        </header>

        <div class="stream" id="stream">
          <div class="stream-inner" id="stream-inner"></div>
        </div>

        <div class="dock">
          <div class="composer">
            <textarea id="prompt" rows="1" placeholder="Pergunte qualquer coisa…"></textarea>
            <div class="composer-row">
              <div class="tools">
                <div id="c-account"></div>
                <div id="c-model"></div>
                <div id="c-effort"></div>
                <div id="c-api"></div>
                <span class="tool-hint" title="Pesquisa nativa xAI (web + X) via Responses">search: xAI</span>
              </div>
              <button class="send" id="send" title="Enviar">↑</button>
            </div>
          </div>
        </div>
      </main>
    </div>
  `;

  $("#btn-add").onclick = showAddAccountChooser;
  $("#btn-accounts").onclick = openAccountsModal;
  $("#btn-stats").onclick = openStatsModal;
  $("#btn-stats-top").onclick = openStatsModal;
  $("#btn-logs").onclick = openLogsModal;

  const effortOpts = [
    { value: "low", label: "Low" },
    { value: "medium", label: "Medium" },
    { value: "high", label: "High" },
    { value: "xhigh", label: "xHigh" },
  ];
  const providerOpts = [
    { value: "xai", label: "Grok · Auth" },
    { value: "kimi_work", label: "Kimi Work · Auth" },
    { value: "ollie", label: "OllieChat · API key" },
    { value: "gemini", label: "Gemini · API key" },
    { value: "qwen", label: "Qwen · API key" },
  ];
  const apiOpts = [
    { value: "responses", label: "Responses ★" },
    { value: "chat", label: "Chat" },
  ];
  const fallbackModels = (prov) => {
    const p = (prov || state.settings?.provider || "xai").toLowerCase();
    if (p === "qwen" || p === "qwenbridge") {
      return [{ id: "qwen3.8", name: "qwen3.8" }];
    }
    if (p === "ollie") {
      return [
        { id: "claude-sonnet-5", name: "claude-sonnet-5" },
        { id: "claude-fable-5", name: "claude-fable-5" },
        { id: "claude-opus-4-8", name: "claude-opus-4-8" },
        { id: "deepseek-v4-flash-free", name: "deepseek-v4-flash-free" },
      ];
    }
    if (p === "gemini" || p === "google" || p === "vertex") {
      return [
        { id: "gemini-3.1-pro-preview", name: "gemini-3.1-pro-preview" },
        { id: "gemini-3-flash-preview", name: "gemini-3-flash-preview" },
        { id: "gemini-3.5-flash", name: "gemini-3.5-flash" },
        { id: "gemini-3.1-flash-lite", name: "gemini-3.1-flash-lite" },
        { id: "gemini-3.1-flash-image", name: "gemini-3.1-flash-image" },
        { id: "gemini-3-pro-image", name: "gemini-3-pro-image" },
        { id: "gemini-2.5-pro", name: "gemini-2.5-pro" },
        { id: "gemini-2.5-flash", name: "gemini-2.5-flash" },
        { id: "gemini-2.5-flash-lite", name: "gemini-2.5-flash-lite" },
        { id: "gemini-2.0-flash-001", name: "gemini-2.0-flash-001" },
        { id: "gemini-2.0-flash-lite-001", name: "gemini-2.0-flash-lite-001" },
        { id: "gemini-1.5-pro-002", name: "gemini-1.5-pro-002" },
      ];
    }
    if (p === "kimi_work" || p === "kimi" || p === "kimi-work") {
      return [
        { id: "kimi-for-coding", name: "Kimi For Coding" },
        { id: "k3-agent", name: "K3 Max (Work)" },
        { id: "k3-agent-low", name: "K3 Max — Low Think" },
        { id: "k3-agent-medium", name: "K3 Max — Medium Think" },
        { id: "k3-agent-high", name: "K3 Max — High Think" },
        { id: "k3-agent-xhigh", name: "K3 Max — Extra High Think" },
        { id: "k2d6-agent", name: "K2.6 Agent (Work)" },
      ];
    }
    return [
      { id: "grok-4.5", name: "Grok 4.5" },
      { id: "grok-4.5-responses", name: "Grok 4.5 (Responses)" },
    ];
  };
  const modelOpts = () =>
    (state.models.length ? state.models : fallbackModels()).map((m) => ({
      value: m.id,
      label: shortModelLabel(m.name || m.id, m.id),
    }));

  async function switchProvider(v) {
    // One shot: backend resets model+upstream for the provider.
    await saveGlobal({ provider: v });
    try {
      state.models = (await ListModels()) || [];
    } catch {
      state.models = fallbackModels(v);
    }
    const prefer =
      state.settings.default_model ||
      fallbackModels(v)[0]?.id ||
      "default";
    state.picks.model = prefer;
    state.picks.cModel = prefer;
    state.menus["set-model"]?.refresh?.();
    state.menus["c-model"]?.refresh?.();
    state.menus["set-model"]?.setValue(prefer);
    state.menus["c-model"]?.setValue(prefer);
    // Grok: Responses. Kimi Work: chat/completions only (no responses on agent-gw).
    const isKimi = v === "kimi_work" || v === "kimi" || v === "kimi-work";
    const api = v === "xai" ? "responses" : "chat";
    if (state.settings.api_mode !== api) {
      await saveGlobal({ api_mode: api });
    }
    state.menus["set-api"]?.setValue(api);
    state.menus["c-api"]?.setValue(api);
    state.picks.api = api;
    state.picks.cApi = api;
    if (isKimi) {
      state.menus["set-api"]?.setValue("chat");
      state.menus["c-api"]?.setValue("chat");
    }
    updateProviderChrome();
    await refreshBootstrap(false);
  }

  mountMenu($("#set-provider"), {
    id: "set-provider",
    options: providerOpts,
    value: state.settings?.provider || "xai",
    onChange: (v) => switchProvider(v),
  });

  mountMenu($("#set-effort"), {
    id: "set-effort",
    options: effortOpts,
    value: state.picks.effort,
    onChange: (v) => {
      state.picks.effort = v;
      state.picks.cEffort = v;
      state.menus["c-effort"]?.setValue(v);
      saveGlobal({ reasoning_effort: v });
    },
  });
  mountMenu($("#set-api"), {
    id: "set-api",
    options: apiOpts,
    value: state.picks.api,
    onChange: (v) => {
      state.picks.api = v;
      state.picks.cApi = v;
      state.menus["c-api"]?.setValue(v);
      saveGlobal({ api_mode: v });
    },
  });
  mountMenu($("#set-model"), {
    id: "set-model",
    options: modelOpts,
    value: state.picks.model,
    onChange: (v) => {
      state.picks.model = v;
      state.picks.cModel = v;
      state.menus["c-model"]?.setValue(v);
      saveGlobal({ default_model: v });
    },
  });

  // QwenBridge settings (visible only when provider=qwen — see updateProviderChrome).
  const qwenSave = $("#qwen-save");
  if (qwenSave) {
    qwenSave.onclick = async () => {
      const base = ($("#qwen-upstream")?.value || "").trim();
      const key = ($("#qwen-api-key")?.value || "").trim();
      const patch = {};
      if (base) patch.qwen_upstream = base;
      // Only send the key when the user typed a new one — the masked sentinel
      // in state.settings.qwen_api_key is never echoed back.
      if (key) patch.qwen_api_key = key;
      if (!Object.keys(patch).length) return;
      await saveGlobal(patch);
      const keyInput = $("#qwen-api-key");
      if (keyInput) keyInput.value = "";
      await refreshBootstrap(false);
    };
  }

  // Composer account switcher: click email chip → pick another account
  const accountOpts = () => {
    if (!state.accounts.length) {
      return [{ value: "", label: "sem conta — adicione à esquerda" }];
    }
    return state.accounts.map((a) => {
      const base = a.email || a.label || a.id;
      let mark = a.active ? "● " : "";
      if (a.exhausted) mark = "⛔ ";
      else if (a.expired) mark = "⚠ ";
      else if (a.active) mark = "● ";
      return { value: a.id, label: mark + base };
    });
  };

  mountMenu($("#c-account"), {
    id: "c-account",
    options: accountOpts,
    value: activeAccount()?.id || "",
    prefix: "conta",
    chip: true,
    onChange: async (v) => {
      if (!v) return;
      if (v === activeAccount()?.id) return;
      await SetActiveAccount(v);
      await refreshBootstrap(false);
    },
  });
  mountMenu($("#c-model"), {
    id: "c-model",
    options: modelOpts,
    value: state.picks.cModel,
    prefix: "model",
    chip: true,
    onChange: (v) => {
      state.picks.cModel = v;
    },
  });
  mountMenu($("#c-effort"), {
    id: "c-effort",
    options: effortOpts.map((o) => ({ ...o, label: o.value })),
    value: state.picks.cEffort,
    prefix: "think",
    chip: true,
    onChange: (v) => {
      state.picks.cEffort = v;
    },
  });
  mountMenu($("#c-api"), {
    id: "c-api",
    options: apiOpts.map((o) => ({ ...o, label: o.value })),
    value: state.picks.cApi,
    prefix: "api",
    chip: true,
    onChange: (v) => {
      state.picks.cApi = v;
    },
  });

  const prompt = $("#prompt");
  prompt.addEventListener("input", () => autoGrow(prompt));
  prompt.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      if (!state.streaming) submit();
    }
  });
  $("#send").onclick = () => {
    if (state.streaming) CancelChat();
    else submit();
  };

  state.shellBuilt = true;
}

function autoGrow(ta) {
  ta.style.height = "auto";
  ta.style.height = Math.min(160, ta.scrollHeight) + "px";
}

function fillModels() {
  // custom menus re-render options via refresh
  state.menus["set-model"]?.refresh?.();
  state.menus["c-model"]?.refresh?.();
  const prefer = state.settings.default_model || state.picks.model || "grok-4.5";
  if (state.menus["set-model"]) state.menus["set-model"].setValue(prefer);
  if (state.menus["c-model"] && !state.picks.cModelTouched) {
    state.menus["c-model"].setValue(prefer);
    state.picks.cModel = prefer;
  }
}

function isKimiProvider(p) {
  const v = (p || state.settings?.provider || "").toLowerCase();
  return v === "kimi_work" || v === "kimi" || v === "kimi-work" || v.startsWith("kimi");
}

/** Yes/No confirm sheet (replaces window.confirm for delete account). */
function confirmYesNo({ title, message, yesLabel = "Sim", noLabel = "Não", danger = true }) {
  return new Promise((resolve) => {
    const overlay = document.createElement("div");
    overlay.className = "overlay overlay-glass";
    overlay.innerHTML = `
      <div class="sheet sheet-confirm">
        <h3>${escapeHtml(title || "Confirmar")}</h3>
        <p class="confirm-msg">${message || ""}</p>
        <div class="sheet-actions confirm-actions">
          <button type="button" class="btn btn-quiet" data-ans="no">${escapeHtml(noLabel)}</button>
          <button type="button" class="btn ${danger ? "btn-danger" : "btn-solid"}" data-ans="yes">${escapeHtml(yesLabel)}</button>
        </div>
      </div>`;
    const finish = (v) => {
      overlay.remove();
      resolve(v);
    };
    overlay.addEventListener("click", (e) => {
      if (e.target === overlay) finish(false);
    });
    overlay.querySelector('[data-ans="no"]').onclick = () => finish(false);
    overlay.querySelector('[data-ans="yes"]').onclick = () => finish(true);
    document.body.appendChild(overlay);
  });
}

async function confirmAndLogoffKimi(account) {
  const name = account.label || account.email || account.id;
  const hasWeb = !!account.has_web_session || !!account.has_refresh;
  const msg = hasWeb
    ? `Deletar a conta <b>${escapeHtml(name)}</b> no <b>kimi.com</b>?<br/><br/>Isso apaga a conta de verdade (irreversível) e remove do proxy.`
    : `A conta <b>${escapeHtml(name)}</b> não tem sessão web (só sk-kimi).<br/>Não dá para deletar no site — só remover do proxy local.`;
  if (hasWeb) {
    const ok = await confirmYesNo({
      title: "Deletar conta Kimi?",
      message: msg,
      yesLabel: "Sim, deletar",
      noLabel: "Não",
      danger: true,
    });
    if (!ok) return;
    try {
      setStatus("Deletando conta no kimi.com…");
      await LogoffKimiAccount(account.id);
      await refreshBootstrap(false);
      setStatus("Conta deletada no kimi.com");
    } catch (e) {
      alert("Falha ao deletar: " + e);
      setStatus("Erro ao deletar conta");
    }
    return;
  }
  const ok = await confirmYesNo({
    title: "Remover do proxy?",
    message: msg,
    yesLabel: "Sim, remover",
    noLabel: "Não",
    danger: true,
  });
  if (!ok) return;
  await RemoveAccount(account.id);
  await refreshBootstrap(false);
  setStatus("Conta removida do proxy");
}

function addLog(source, message, detail = null) {
  const entry = {
    time: new Date().toLocaleTimeString("pt-BR"),
    source,
    message: String(message ?? ""),
    detail: detail ? JSON.stringify(detail, null, 2) : null,
  };
  state.logs.push(entry);
  // keep last 200
  if (state.logs.length > 200) state.logs = state.logs.slice(-200);
  // update badge if modal is open
  const badge = document.getElementById("logs-badge");
  if (badge) badge.textContent = state.logs.length;
}

function escapeLogHtml(s) {
  return String(s ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

async function copyToClipboard(text) {
  try {
    await navigator.clipboard.writeText(text);
  } catch (_) {
    const ta = document.createElement("textarea");
    ta.value = text;
    document.body.appendChild(ta);
    ta.select();
    document.execCommand("copy");
    document.body.removeChild(ta);
  }
}

function openLogsModal() {
  closeOverlay();
  const overlay = document.createElement("div");
  overlay.className = "overlay overlay-glass";
  overlay.id = "logs-overlay";

  const rows = state.logs.length
    ? state.logs
        .map((log, i) => {
          const detailBlock = log.detail
            ? `<pre class="log-detail">${escapeLogHtml(log.detail)}</pre>`
            : "";
          return `
        <div class="log-row" data-index="${i}">
          <div class="log-head">
            <span class="log-time">${escapeLogHtml(log.time)}</span>
            <span class="log-source ${escapeLogHtml(log.source)}">${escapeLogHtml(log.source)}</span>
            <span class="log-msg">${escapeLogHtml(log.message)}</span>
            <button type="button" class="log-copy-btn" data-index="${i}" title="Copiar este log">Copiar</button>
          </div>
          ${detailBlock}
        </div>`;
        })
        .join("")
    : `<div class="log-empty">Nenhum log nesta sessão ainda.</div>`;

  overlay.innerHTML = `
    <div class="sheet sheet-logs">
      <div class="sheet-head">
        <div>
          <h3>Logs da sessão</h3>
          <p>${state.logs.length} evento(s) · perdido ao fechar o app</p>
        </div>
        <div style="display:flex;gap:8px;align-items:center;">
          <button type="button" class="btn btn-quiet" id="logs-copy-all" ${state.logs.length ? "" : "disabled style=\"opacity:.4\""}>Copiar tudo</button>
          <button type="button" class="btn btn-quiet" id="logs-close">Fechar</button>
        </div>
      </div>
      <div class="logs-list">${rows}</div>
    </div>`;

  document.body.appendChild(overlay);

  $("#logs-close", overlay).onclick = () => overlay.remove();
  overlay.addEventListener("click", (e) => {
    if (e.target === overlay) overlay.remove();
  });

  if (state.logs.length) {
    $("#logs-copy-all", overlay).onclick = async () => {
      const text = state.logs
        .map((l) => {
          let line = `[${l.time}] [${l.source}] ${l.message}`;
          if (l.detail) line += "\n" + l.detail;
          return line;
        })
        .join("\n\n");
      await copyToClipboard(text);
      const btn = $("#logs-copy-all", overlay);
      if (btn) {
        btn.textContent = "Copiado!";
        setTimeout(() => (btn.textContent = "Copiar tudo"), 1200);
      }
    };
  }

  overlay.querySelectorAll(".log-copy-btn").forEach((btn) => {
    btn.onclick = async () => {
      const idx = Number(btn.getAttribute("data-index"));
      const log = state.logs[idx];
      if (!log) return;
      let text = `[${log.time}] [${log.source}] ${log.message}`;
      if (log.detail) text += "\n" + log.detail;
      await copyToClipboard(text);
      btn.textContent = "Copiado!";
      setTimeout(() => (btn.textContent = "Copiar"), 1200);
    };
  });
}

function paintChrome() {
  ensureShell();
  const u = globalUsage();
  const acc = activeAccount();
  const busy = !!state.activeRequest;

  $("#u-total").textContent = fmt(u.total_tokens);
  $("#u-cost").textContent = fmtUSD(u.cost_usd);
  $("#u-prompt").textContent = fmt(u.prompt_tokens);
  $("#u-comp").textContent = fmt(u.completion_tokens);
  $("#u-reason").textContent = fmt(u.reasoning_tokens);
  if (u.latency_samples > 0) {
    $("#u-lat").textContent = fmtMs(u.latency_sum_ms / u.latency_samples);
  }
  $("#proxy-url").textContent = state.proxyBase || "—";
  updateProviderChrome();
  $("#data-dir").textContent = shortPath(state.dataDir) || "—";
  $("#data-dir").title = state.dataDir || "";

  const list = $("#accounts");
  const countEl = $("#accounts-count");
  if (countEl) {
    const n = state.accounts.length;
    countEl.textContent = n === 1 ? "1 conta" : `${n} contas`;
  }
  list.innerHTML = "";
  const pNow = (state.settings?.provider || "xai").toLowerCase();
  const kimiUI = isKimiProvider(pNow);
  if (providerAuthMode(pNow) !== "auth") {
    list.innerHTML = `<div class="account empty-hint">Provedor <b>API key</b> — sem pool de contas de sessão.<br/>Credencial direta (Ollie keyless / Gemini ADC / QwenBridge local).</div>`;
  } else if (!state.accounts.length) {
    const how = pNow.startsWith("kimi")
      ? "Clique em <b>+ Conta Kimi</b> (Desktop / JWT / sk-kimi)."
      : "Clique em <b>+ Conta Grok</b> para OAuth xAI.";
    list.innerHTML = `<div class="account empty-hint">Nenhuma conta neste provedor.<br/>${how}</div>`;
  } else {
    state.accounts.forEach((a) => {
      const u = a.usage || {};
      const card = document.createElement("div");
      card.className = "account" + (a.active ? " active" : "");
      card.innerHTML = `
        <div class="account-top" data-act="select">
          <div class="avatar">${escapeHtml(initials(a.email || a.label))}</div>
          <div style="min-width:0">
            <strong title="${escapeHtml(a.email || a.id)}">${escapeHtml(a.label || a.email || a.id)}</strong>
            <div class="meta-line">
              ${a.active ? `<span class="badge badge-live">ativa</span>` : `<span class="badge badge-ok">salva</span>`}
              ${a.exhausted ? `<span class="badge badge-danger">esgotada</span>` : ""}
              ${a.expired ? `<span class="badge badge-warn">token exp.</span>` : ""}
              ${kimiUI && a.has_web_session ? `<span class="badge badge-ok" title="Sessão web (pode deletar no site)">web</span>` : ""}
              <span>${escapeHtml((a.email || "").split("@")[0] || a.id.slice(0, 8))}</span>
            </div>
          </div>
        </div>
        <div class="account-usage">
          <span><b>${fmt(u.total_tokens || 0)}</b> tok</span>
          <span><b>${fmtUSD(u.cost_usd || 0)}</b></span>
          <span><b>${fmt(u.requests || 0)}</b> req</span>
        </div>
        <div class="account-actions">
          ${
            a.active
              ? `<button type="button" class="primary" data-act="noop" disabled style="opacity:.55">Em uso</button>`
              : `<button type="button" class="primary" data-act="select">Usar</button>`
          }
          <button type="button" data-act="rename">Renomear</button>
          ${
            kimiUI
              ? `<button type="button" class="danger" data-act="logoff" title="Deletar conta no kimi.com">Deletar</button>`
              : `<button type="button" class="danger" data-act="remove">Remover</button>`
          }
        </div>
      `;
      card.querySelectorAll("[data-act]").forEach((btn) => {
        btn.onclick = async (e) => {
          e.stopPropagation();
          const act = btn.getAttribute("data-act");
          if (act === "select") {
            await SetActiveAccount(a.id);
            await refreshBootstrap(false);
          } else if (act === "rename") {
            const next = prompt("Nome da conta", a.label || a.email || "");
            if (next == null || !String(next).trim()) return;
            try {
              await RenameAccount(a.id, String(next).trim());
              await refreshBootstrap(false);
            } catch (err) {
              alert("Rename: " + err);
            }
          } else if (act === "logoff") {
            await confirmAndLogoffKimi(a);
          } else if (act === "remove") {
            if (!confirm(`Remover conta ${a.label || a.email}?`)) return;
            await RemoveAccount(a.id);
            await refreshBootstrap(false);
          }
        };
      });
      list.appendChild(card);
    });
  }

  // refresh composer account menu
  state.menus["c-account"]?.refresh?.();
  const activeId = activeAccount()?.id || "";
  if (activeId) state.menus["c-account"]?.setValue(activeId);

  // sync pick values from settings
  state.picks.effort = state.settings.reasoning_effort || state.picks.effort || "high";
  state.picks.api = state.settings.api_mode || state.picks.api || "chat";
  state.picks.model = state.settings.default_model || state.picks.model || "grok-4.5";
  if (!state.picks.cEffort) state.picks.cEffort = state.picks.effort;
  if (!state.picks.cApi) state.picks.cApi = state.picks.api;
  if (!state.picks.cModel) state.picks.cModel = state.picks.model;

  state.menus["set-effort"]?.setValue(state.picks.effort);
  state.menus["set-api"]?.setValue(state.picks.api);
  fillModels();
  state.menus["c-effort"]?.setValue(state.picks.cEffort);
  state.menus["c-api"]?.setValue(state.picks.cApi);
  state.menus["c-model"]?.setValue(state.picks.cModel);
  if (acc?.id) state.menus["c-account"]?.setValue(acc.id);

  paintStatus();
  paintSend();
  paintMessages();
}

function paintStatus() {
  const el = $("#status");
  const text = $("#status-text");
  if (!el || !text) return;
  const acc = activeAccount();
  const req = state.activeRequest;
  if (req) {
    el.classList.add("live");
    const phase =
      req.phase === "searching"
        ? "pesquisando na web"
        : req.phase === "thinking"
          ? "thinking"
          : req.phase || "…";
    text.innerHTML = `Request → <strong>${escapeHtml(req.label || req.email || "conta")}</strong> · ${escapeHtml(phase)}`;
  } else {
    el.classList.remove("live");
    const n = state.accounts.length;
    text.innerHTML = acc
      ? `Ativa: <strong>${escapeHtml(acc.label || acc.email || acc.id)}</strong>${n > 1 ? ` · ${n} contas` : ""}`
      : "Nenhuma conta — adicione à esquerda (multi-conta ok)";
  }
}

function paintSend() {
  const btn = $("#send");
  if (!btn) return;
  btn.classList.toggle("stop", state.streaming);
  btn.textContent = state.streaming ? "■" : "↑";
  btn.title = state.streaming ? "Parar" : "Enviar";
}

function paintMessages() {
  const inner = $("#stream-inner");
  if (!inner) return;

  if (!state.messages.length) {
    inner.innerHTML = `
      <div class="hero">
        <div class="orb"></div>
        <h2>O que você quer saber?</h2>
        <p>Conversa contínua, thinking em cinza translúcido, multi-conta e proxy OpenAI local.</p>
      </div>
    `;
    return;
  }

  // Rebuild stream with markdown for assistant (and light md for user)
  const html = state.messages
    .map((m, i) => {
      if (m.role === "user") {
        // User: preserve plain text layout but allow simple md if they paste it
        const body = m.content?.includes("`") || m.content?.includes("**") || m.content?.includes("\n")
          ? renderMarkdown(m.content)
          : `<p>${escapeHtml(m.content).replaceAll("\n", "<br>")}</p>`;
        return `
          <section class="turn turn-user" data-i="${i}">
            <div class="turn-label">Você</div>
            <div class="turn-body md">${body}</div>
          </section>
        `;
      }
      const searchUI = renderSearchBlock(m);
      const think = m.thinking
        ? `<div class="think">${escapeHtml(m.thinking)}</div>`
        : "";
      const cursor = m.streaming ? `<span class="cursor" aria-hidden="true"></span>` : "";
      const meta = m.meta ? `<div class="turn-meta">${escapeHtml(m.meta)}</div>` : "";
      // Errors: plain escaped text only (never markdown/HTML from upstream pages).
      const answer = (m.isError || looksLikeHTML(m.content)
        ? `<p class="err">${escapeHtml(safeErrorText(m.content || ""))}</p>`
        : renderMarkdown(m.content || "")) + cursor;
      const hasAnswer = !!(m.content && m.content.trim());
      const searching =
        m.search?.status === "searching" ||
        (m.searches || []).some((s) => s.status === "searching") ||
        (m.tools || []).some((t) => t.status === "running");
      const showAnswer = hasAnswer || (m.streaming && !searching);
      return `
        <section class="turn turn-assistant" data-i="${i}">
          <div class="turn-label">Grok</div>
          ${searchUI}
          ${think}
          ${hasAnswer || showAnswer ? `<div class="answer md">${answer || (m.streaming ? cursor : "")}</div>` : m.streaming && searchUI ? "" : `<div class="answer md">${answer}</div>`}
          ${meta}
        </section>
      `;
    })
    .join("");

  const stream = $("#stream");
  const nearBottom = stream.scrollHeight - stream.scrollTop - stream.clientHeight < 120;
  inner.innerHTML = html;
  enhanceMarkdownRoot(inner);
  if (nearBottom || state.streaming) {
    stream.scrollTop = stream.scrollHeight;
  }
}

async function saveGlobal(patch) {
  state.settings = await UpdateSettings(patch);
  if (patch.reasoning_effort) {
    state.picks.effort = patch.reasoning_effort;
    state.picks.cEffort = patch.reasoning_effort;
    state.menus["c-effort"]?.setValue(patch.reasoning_effort);
  }
  if (patch.api_mode) {
    state.picks.api = patch.api_mode;
    state.picks.cApi = patch.api_mode;
    state.menus["c-api"]?.setValue(patch.api_mode);
  }
  if (patch.default_model) {
    state.picks.model = patch.default_model;
    state.picks.cModel = patch.default_model;
    state.menus["c-model"]?.setValue(patch.default_model);
  }
  if (patch.provider) {
    state.menus["set-provider"]?.setValue(state.settings.provider || patch.provider);
    if (state.settings.api_mode) {
      state.picks.api = state.settings.api_mode;
      state.picks.cApi = state.settings.api_mode;
      state.menus["set-api"]?.setValue(state.settings.api_mode);
      state.menus["c-api"]?.setValue(state.settings.api_mode);
    }
    updateProviderChrome();
  }
}

function providerAuthMode(p) {
  p = (p || state.settings?.provider || "xai").toLowerCase();
  if (p === "xai" || p === "grok" || p === "kimi_work" || p === "kimi" || p === "kimi-work") return "auth";
  return "api_key";
}

function updateProviderChrome() {
  const p = (state.settings?.provider || "xai").toLowerCase();
  const model = state.settings?.default_model || state.picks?.model || "—";
  const mode = providerAuthMode(p);
  const el = $("#provider-label");
  if (el) {
    if (p === "qwen" || p === "qwenbridge") {
      el.textContent = `Qwen · API key · ${shortModelLabel(model, model)}`;
    } else if (p === "ollie" || p === "olliechat") {
      el.textContent = `Ollie · API key · ${shortModelLabel(model, model)}`;
    } else if (p === "gemini" || p === "google" || p === "vertex") {
      const proj = state.settings?.gemini_project || "ADC project";
      el.textContent = `Gemini · API key · ${shortModelLabel(model, model)} · ${proj}`;
    } else if (p === "kimi_work" || p === "kimi" || p === "kimi-work") {
      el.textContent = `Kimi Work · Auth · ${shortModelLabel(model, model)}`;
    } else {
      el.textContent = `Grok · Auth · ${shortModelLabel(model, model)}`;
    }
  }
  const modeEl = $("#provider-mode");
  if (modeEl) {
    modeEl.innerHTML =
      mode === "auth"
        ? `<span class="mode-pill mode-auth">Auth · multi-conta</span>`
        : `<span class="mode-pill mode-key">API key · sem pool</span>`;
  }
  const addBtn = $("#btn-add");
  const accBtn = $("#btn-accounts");
  if (addBtn) {
    addBtn.style.display = mode === "auth" ? "" : "none";
    addBtn.textContent = p.startsWith("kimi") ? "+ Conta Kimi" : "+ Conta Grok";
  }
  if (accBtn) {
    accBtn.style.display = mode === "auth" ? "" : "none";
  }
  const hint = document.querySelector(".tool-hint");
  if (hint) {
    if (p === "qwen" || p === "qwenbridge") {
      hint.textContent = "QwenBridge";
      hint.title = "QwenBridge local · OpenAI-compatible (chat/completions)";
    } else if (p === "ollie" || p === "olliechat") {
      hint.textContent = "OllieChat";
      hint.title = "Upstream OllieChat (sem chave)";
    } else if (p === "gemini" || p === "google" || p === "vertex") {
      hint.textContent = "Gemini ADC";
      hint.title = "Vertex AI via Application Default Credentials (gcloud)";
    } else if (p === "kimi_work" || p === "kimi" || p === "kimi-work") {
      hint.textContent = "chat/completions";
      hint.title = "Kimi Work agent-gw · só /v1/chat/completions (sem Responses nativo)";
    } else {
      hint.textContent = "search: xAI";
      hint.title = "Pesquisa nativa xAI (web + X) via Responses";
    }
  }
  // Hide API mode picker for Kimi — always chat/completions.
  const isKimi = p === "kimi_work" || p === "kimi" || p === "kimi-work";
  const hideApi = isKimi;
  for (const id of ["set-api", "c-api"]) {
    const elApi = document.getElementById(id);
    if (!elApi) continue;
    const wrap = elApi.closest(".field") || elApi;
    wrap.style.display = hideApi ? "none" : "";
  }
  if (isKimi) {
    state.picks.api = "chat";
    state.picks.cApi = "chat";
    state.menus["set-api"]?.setValue("chat");
    state.menus["c-api"]?.setValue("chat");
  }
  // QwenBridge settings block: only for provider=qwen. Base URL prefilled from
  // settings; the API key comes back masked, so the input stays empty and only
  // sends a value when the user types a new one.
  const isQwen = p === "qwen" || p === "qwenbridge";
  const qwenFields = $("#qwen-fields");
  if (qwenFields) {
    qwenFields.style.display = isQwen ? "" : "none";
    if (isQwen) {
      const upInput = $("#qwen-upstream");
      if (upInput && !upInput.value) {
        upInput.value = state.settings?.qwen_upstream || "http://127.0.0.1:3000/v1";
      }
      const keyInput = $("#qwen-api-key");
      if (keyInput) {
        keyInput.placeholder = state.settings?.qwen_api_key
          ? "•••••••• (salva — digite para trocar)"
          : "API_KEY do QwenBridge";
      }
    }
  }
}


function closeOverlay() {
  document.querySelector(".overlay")?.remove();
}

function showAddAccountChooser() {
  const p = (state.settings?.provider || "xai").toLowerCase();
  if (p === "kimi_work" || p === "kimi" || p === "kimi-work") {
    showAddKimiChooser();
    return;
  }
  if (p === "ollie" || p === "gemini" || p === "google" || p === "vertex" || p === "qwen" || p === "qwenbridge") {
    closeOverlay();
    const overlay = document.createElement("div");
    overlay.className = "overlay overlay-glass";
    overlay.innerHTML = `
      <div class="sheet sheet-choose">
        <h3>Provedor API key</h3>
        <p>Ollie, Gemini e Qwen não usam pool de contas de sessão. Configure o provedor em <b>Global</b> — a credencial é direta (keyless / ADC / bridge local).</p>
        <div class="sheet-actions">
          <button class="btn btn-quiet" id="m-cancel">Fechar</button>
        </div>
      </div>`;
    document.body.appendChild(overlay);
    $("#m-cancel", overlay).onclick = () => overlay.remove();
    overlay.addEventListener("click", (e) => {
      if (e.target === overlay) overlay.remove();
    });
    return;
  }
  closeOverlay();
  const overlay = document.createElement("div");
  overlay.className = "overlay overlay-glass";
  overlay.innerHTML = `
    <div class="sheet sheet-choose">
      <h3>Adicionar conta Grok</h3>
      <p><span class="mode-pill mode-auth">Auth</span> OAuth multi-conta xAI</p>
      <div class="choose-grid">
        <button type="button" class="choose-card" id="m-auto">
          <strong>Automática</strong>
          <span>Cria conta com darkemail + Chrome isolate, depois pede device OAuth pros tokens da API.</span>
        </button>
        <button type="button" class="choose-card" id="m-manual">
          <strong>Manual</strong>
          <span>Device login clássico — você confirma o código na xAI com uma conta que já existe.</span>
        </button>
      </div>
      <label class="auto-toggle">
        <input type="checkbox" id="m-auto-quota" />
        Criar conta automática quando a cota acabar (402 / balance exhausted)
      </label>
      <div class="sheet-actions">
        <button class="btn btn-quiet" id="m-cancel">Fechar</button>
      </div>
    </div>
  `;
  document.body.appendChild(overlay);
  $("#m-cancel", overlay).onclick = () => overlay.remove();
  overlay.addEventListener("click", (e) => {
    if (e.target === overlay) overlay.remove();
  });
  GetAutoCreateOnExhausted()
    .then((v) => {
      const el = $("#m-auto-quota", overlay);
      if (el) el.checked = !!v;
    })
    .catch(() => {});
  $("#m-auto-quota", overlay).onchange = (e) => {
    SetAutoCreateOnExhausted(!!e.target.checked).catch(() => {});
  };
  $("#m-manual", overlay).onclick = () => {
    overlay.remove();
    startLogin();
  };
  $("#m-auto", overlay).onclick = () => {
    overlay.remove();
    startAutoSignupUI();
  };
}

async function showAddKimiChooser() {
  closeOverlay();
  let activeCount = 0;
  try {
    const rows = (await ListAccountsForProvider("kimi_work")) || [];
    activeCount = rows.filter((a) => !a.exhausted && !a.auth_denied).length;
  } catch (_) {
    activeCount = (state.accounts || []).filter((a) => isKimiProvider(a.provider) && !a.exhausted).length;
  }
  const atCap = activeCount >= 3;
  const overlay = document.createElement("div");
  overlay.className = "overlay overlay-glass";
  overlay.innerHTML = `
    <div class="sheet sheet-choose sheet-kimi">
      <h3>Adicionar conta Kimi Work</h3>
      <p><span class="mode-pill mode-auth">Auth</span> Pool de <b>até 3 contas</b> (agora: ${activeCount}/3). Com 1 já funciona. O proxy faz rotação e re-login automático.</p>
      <div class="choose-grid">
        <button type="button" class="choose-card" id="m-browser" ${atCap ? "disabled" : ""}>
          <strong>Login manual (Google)</strong>
          <span>Abre seu Chrome/Edge. Escolha a conta Google. Tokens entram no pool. Ideal para a 1ª conta ou quando já está logado no navegador.</span>
        </button>
        <button type="button" class="choose-card" id="m-clean" ${atCap ? "disabled" : ""}>
          <strong>Nova conta (perfil limpo)</strong>
          <span>Playwright com perfil isolado — login Google do zero. Use para adicionar outra conta Google sem misturar sessão.</span>
        </button>
      </div>
      <p class="hint" style="margin-top:10px;font-size:12px;opacity:.65">${atCap ? "Limite de 3 contas atingido — remova uma em Contas." : "Re-login automático: o proxy renova cada conta com o Google refresh salvo (sem tela). Só abre browser se o HTTP falhar."}</p>
      <div class="sheet-actions">
        <button class="btn btn-quiet" id="m-cancel">Fechar</button>
      </div>
    </div>`;
  document.body.appendChild(overlay);
  $("#m-cancel", overlay).onclick = () => overlay.remove();
  overlay.addEventListener("click", (e) => {
    if (e.target === overlay) overlay.remove();
  });
  if (atCap) return;
  $("#m-browser", overlay).onclick = async () => {
    try {
      setStatus("Kimi: abra o navegador e escolha a conta Google…");
      overlay.remove();
      const rec = await StartKimiBrowserLogin();
      await refreshBootstrap(false);
      const bits = [];
      if (rec.has_refresh) bits.push("kimi refresh");
      if (rec.has_google_refresh) bits.push("google refresh");
      if (rec.refresh_tested) bits.push("refresh validado");
      if (rec.upstream_tested) bits.push("upstream ok");
      setStatus(`Kimi ok · ${rec.label || rec.id}${bits.length ? " · " + bits.join(" + ") + " salvo" : ""}`);
    } catch (e) {
      alert("Falha login Kimi: " + e);
      setStatus("Falha login Kimi");
    }
  };
  $("#m-clean", overlay).onclick = async () => {
    try {
      setStatus("Kimi: perfil limpo — faça login Google…");
      overlay.remove();
      const rec = await StartKimiStealthLoginNewAccount(false);
      await refreshBootstrap(false);
      const bits = [];
      if (rec.has_refresh) bits.push("kimi refresh");
      if (rec.has_google_refresh) bits.push("google refresh");
      if (rec.refresh_tested) bits.push("refresh validado");
      if (rec.upstream_tested) bits.push("upstream ok");
      setStatus(`Kimi ok · ${rec.label || rec.id}${bits.length ? " · " + bits.join(" + ") + " salvo" : ""}`);
    } catch (e) {
      alert("Falha login Kimi (perfil limpo): " + e);
      setStatus("Falha login Kimi perfil limpo");
    }
  };
}

function showKimiPasteModal(kind) {
  closeOverlay();
  const isJWT = kind === "jwt";
  const overlay = document.createElement("div");
  overlay.className = "overlay overlay-glass";
  overlay.innerHTML = `
    <div class="sheet">
      <h3>${isJWT ? "Colar access JWT" : "Colar sk-kimi"}</h3>
      <p class="hint">${isJWT ? "Bearer JWT (typ=access) da conta Kimi web." : "Começa com sk-kimi-…"}</p>
      <textarea id="m-paste" rows="5" style="width:100%;margin:10px 0;border-radius:10px;padding:10px;background:rgba(0,0,0,.35);border:1px solid rgba(255,255,255,.08);color:#fff;font-family:ui-monospace,monospace;font-size:12px" placeholder="${isJWT ? "eyJhbGciOi…" : "sk-kimi-…"}"></textarea>
      ${isJWT ? "" : `<input id="m-label" placeholder="Label (opcional)" style="width:100%;margin-bottom:10px;border-radius:10px;padding:8px 10px;background:rgba(0,0,0,.35);border:1px solid rgba(255,255,255,.08);color:#fff" />`}
      <div class="sheet-actions">
        <button class="btn btn-solid" id="m-save">Salvar</button>
        <button class="btn btn-quiet" id="m-cancel">Cancelar</button>
      </div>
    </div>`;
  document.body.appendChild(overlay);
  $("#m-cancel", overlay).onclick = () => overlay.remove();
  $("#m-save", overlay).onclick = async () => {
    const raw = ($("#m-paste", overlay).value || "").trim();
    if (!raw) return;
    try {
      if (isJWT) {
        await AddKimiFromJWT(raw);
      } else {
        const label = ($("#m-label", overlay)?.value || "").trim();
        await AddKimiAPIKey(raw, label);
      }
      overlay.remove();
      await refreshBootstrap(false);
      setStatus("Conta Kimi adicionada");
    } catch (e) {
      alert("Falha: " + e);
    }
  };
}

async function showAccountCredsModal(account) {
  closeOverlay();
  const overlay = document.createElement("div");
  overlay.className = "overlay overlay-glass";
  const name = escapeHtml(account.label || account.email || account.id);
  overlay.innerHTML = `
    <div class="sheet" style="width:min(420px,92vw)">
      <h3>Credenciais Google · ${name}</h3>
      <p style="font-size:12px;opacity:.7;margin:0 0 12px;">Usadas para auto-login Playwright quando o HTTP refresh falhar.</p>
      <input type="email" id="m-google-email" placeholder="seu-email@gmail.com" style="width:100%;padding:8px 10px;border-radius:6px;border:1px solid rgba(255,255,255,0.12);background:rgba(0,0,0,0.3);color:#eee;font-size:13px;margin-bottom:8px;" />
      <div style="position:relative;width:100%;margin-bottom:12px;">
        <input type="password" id="m-google-password" placeholder="senha do Google" style="width:100%;padding:8px 10px 8px 10px;border-radius:6px;border:1px solid rgba(255,255,255,0.12);background:rgba(0,0,0,0.3);color:#eee;font-size:13px;box-sizing:border-box;" />
        <button type="button" id="m-toggle-pass" style="position:absolute;right:6px;top:50%;transform:translateY(-50%);background:none;border:none;color:#888;cursor:pointer;font-size:14px;padding:4px 6px;line-height:1;">👁</button>
      </div>
      <button type="button" class="btn btn-solid" id="m-save-creds" style="width:100%;font-size:13px;margin-bottom:8px;">Salvar credenciais</button>
      <p id="m-creds-status" style="font-size:11px;opacity:.5;margin:0 0 8px;text-align:center;"></p>
      <div class="sheet-actions">
        <button class="btn btn-quiet" id="m-cancel">Fechar</button>
      </div>
    </div>`;
  document.body.appendChild(overlay);
  $("#m-cancel", overlay).onclick = () => overlay.remove();
  overlay.addEventListener("click", (e) => {
    if (e.target === overlay) overlay.remove();
  });

  // Load per-account credentials
  try {
    const [email, password] = await GetAccountGoogleCredentials(account.id);
    const elEmail = $("#m-google-email", overlay);
    const elPass = $("#m-google-password", overlay);
    if (elEmail) elEmail.value = email || "";
    if (elPass) elPass.value = password || "";
    const statusEl = $("#m-creds-status", overlay);
    if (statusEl && email) {
      statusEl.textContent = "Credenciais salvas ✓";
      statusEl.style.opacity = "0.7";
      statusEl.style.color = "#7ee787";
    }
  } catch (_) {}

  $("#m-toggle-pass", overlay).onclick = () => {
    const passInput = $("#m-google-password", overlay);
    if (!passInput) return;
    const isHidden = passInput.type === "password";
    passInput.type = isHidden ? "text" : "password";
    $("#m-toggle-pass", overlay).textContent = isHidden ? "🙈" : "👁";
  };

  $("#m-save-creds", overlay).onclick = async () => {
    const email = $("#m-google-email", overlay)?.value || "";
    const password = $("#m-google-password", overlay)?.value || "";
    const statusEl = $("#m-creds-status", overlay);
    try {
      await SetAccountGoogleCredentials(account.id, email, password);
      if (statusEl) {
        statusEl.textContent = "Credenciais salvas ✓";
        statusEl.style.opacity = "0.7";
        statusEl.style.color = "#7ee787";
      }
    } catch (e) {
      if (statusEl) {
        statusEl.textContent = "Erro ao salvar: " + e;
        statusEl.style.color = "#ff7b72";
      }
    }
  };
}

async function openAccountsModal() {
  closeOverlay();
  const p = (state.settings?.provider || "xai").toLowerCase();
  if (providerAuthMode(p) !== "auth") {
    showAddAccountChooser();
    return;
  }
  let accounts = state.accounts || [];
  try {
    accounts = (await ListAccountsForProvider(p)) || accounts;
  } catch (_) {}
  const kimiUI = isKimiProvider(p);
  const title = kimiUI ? "Contas Kimi Work" : "Contas Grok";
  const overlay = document.createElement("div");
  overlay.className = "overlay overlay-glass";

  const rows =
    accounts.length === 0
      ? `<div class="empty-hint" style="padding:24px 4px;text-align:center">Nenhuma conta neste provedor.</div>`
      : accounts
          .map((a) => {
            const u = a.usage || {};
            const name = escapeHtml(a.label || a.email || a.id);
            const email = escapeHtml(a.email || "");
            const initialsStr = escapeHtml(initials(a.email || a.label || a.id));
            const statusBadges = [];
            if (a.active) statusBadges.push(`<span class="badge badge-live">ativa</span>`);
            else statusBadges.push(`<span class="badge badge-ok">salva</span>`);
            if (a.exhausted) statusBadges.push(`<span class="badge badge-danger">esgotada</span>`);
            if (a.auth_denied) statusBadges.push(`<span class="badge badge-danger">auth negada</span>`);
            if (kimiUI && a.has_web_session) statusBadges.push(`<span class="badge badge-ok">sessão web</span>`);
            if (a.has_google_refresh) statusBadges.push(`<span class="badge badge-ok" title="Google refresh token salvo">google refresh</span>`);
            if (a.api_key_hint) statusBadges.push(`<span class="badge badge-ok" title="sk-kimi salvo">${escapeHtml(a.api_key_hint)}</span>`);

            return `<div class="acc-card ${a.active ? "active" : ""}" data-id="${escapeHtml(a.id)}">
              <div class="acc-card-top">
                <div class="acc-avatar">${initialsStr}</div>
                <div class="acc-info">
                  <div class="acc-name" title="${email}">${name}</div>
                  <div class="acc-meta">${statusBadges.join("")}</div>
                  <div class="acc-usage">
                    <span><b>${fmt(u.total_tokens || 0)}</b> tok</span>
                    <span><b>${fmtUSD(u.cost_usd || 0)}</b></span>
                    <span><b>${fmt(u.requests || 0)}</b> req</span>
                  </div>
                </div>
              </div>
              <div class="acc-card-actions">
                ${a.active ? `<button type="button" class="btn btn-xs btn-disabled" disabled>Em uso</button>` : `<button type="button" class="btn btn-xs btn-solid" data-act="use">Usar</button>`}
                <button type="button" class="btn btn-xs btn-quiet" data-act="rename">Renomear</button>
                ${kimiUI ? `<button type="button" class="btn btn-xs btn-quiet" data-act="creds" title="Configurar credenciais Google para auto-login">Credenciais Google</button>` : ""}
                ${a.has_google_refresh ? `<button type="button" class="btn btn-xs btn-quiet" data-act="test-google-refresh" title="Google refresh token salvo">Google Refresh OK</button>` : ""}
                ${kimiUI
                  ? `<button type="button" class="btn btn-xs btn-danger" data-act="logoff">Deletar</button>`
                  : `<button type="button" class="btn btn-xs btn-danger" data-act="remove">Remover</button>`}
              </div>
            </div>`;
          })
          .join("");

  overlay.innerHTML = `
    <div class="sheet sheet-accounts" style="width:min(520px,92vw);padding:22px">
      <div class="sheet-head" style="margin-bottom:14px">
        <div>
          <h3 style="font-size:18px;margin-bottom:4px">${title}</h3>
          <p style="font-size:12px;opacity:.7;margin:0"><span class="mode-pill mode-auth">Auth</span> pool · ${accounts.length}${kimiUI ? "/3" : ""} conta(s) · rotação + re-login automático</p>
        </div>
        <button class="btn btn-quiet" id="m-close" style="height:30px">Fechar</button>
      </div>
      <div class="acc-grid">${rows}</div>
      <div class="sheet-actions" style="margin-top:16px;border-top:1px solid rgba(255,255,255,0.06);padding-top:14px">
        <button class="btn btn-solid" id="m-add">+ Adicionar conta</button>
      </div>
    </div>`;
  document.body.appendChild(overlay);
  $("#m-close", overlay).onclick = () => overlay.remove();
  $("#m-add", overlay).onclick = () => {
    overlay.remove();
    showAddAccountChooser();
  };
  overlay.addEventListener("click", (e) => {
    if (e.target === overlay) overlay.remove();
  });
  overlay.querySelectorAll(".acc-card").forEach((card) => {
    const id = card.getAttribute("data-id");
    card.querySelectorAll("[data-act]").forEach((btn) => {
      btn.onclick = async (e) => {
        e.stopPropagation();
        const act = btn.getAttribute("data-act");
        if (act === "use") {
          await SetActiveAccount(id);
          await refreshBootstrap(false);
          openAccountsModal();
        } else if (act === "rename") {
          const next = prompt("Novo nome", accounts.find((x) => x.id === id)?.label || "");
          if (next != null && next.trim()) {
            await RenameAccount(id, next.trim());
            await refreshBootstrap(false);
            openAccountsModal();
          }
        } else if (act === "creds") {
          const a = accounts.find((x) => x.id === id);
          if (a) {
            overlay.remove();
            await showAccountCredsModal(a);
            openAccountsModal();
          }
        } else if (act === "logoff") {
          const a = accounts.find((x) => x.id === id);
          if (a) {
            overlay.remove();
            await confirmAndLogoffKimi(a);
            openAccountsModal();
          }
        } else if (act === "remove") {
          if (confirm("Remover esta conta?")) {
            await RemoveAccount(id);
            await refreshBootstrap(false);
            openAccountsModal();
          }
        }
      };
    });
  });
}

function setStatus(text) {
  const el = $("#status-text");
  if (el) el.textContent = text || "Pronto";
}

async function startLogin() {
  try {
    const st = await StartDeviceLogin();
    state.device = st;
    showDeviceModal(st);
    if (st.verification_url) {
      try {
        await OpenExternal(st.verification_url);
      } catch (_) {}
    }
  } catch (e) {
    alert("Falha ao iniciar login: " + e);
  }
}

function showDeviceModal(st, extra = {}) {
  closeOverlay();
  const overlay = document.createElement("div");
  overlay.className = "overlay overlay-glass";
  const emailHint = extra.email
    ? `<p class="hint-email">Use a conta <strong>${escapeHtml(extra.email)}</strong>${extra.password ? ` · senha <code id="m-pass">${escapeHtml(extra.password)}</code>` : ""}</p>`
    : "";
  overlay.innerHTML = `
    <div class="sheet">
      <h3>${extra.title || "Adicionar conta"}</h3>
      <p>Confirme o código na página da xAI. O app completa sozinho.</p>
      ${emailHint}
      <div class="code">${escapeHtml(st.user_code)}</div>
      <div class="sheet-actions">
        <button class="btn btn-solid" id="m-open">Abrir login</button>
        <button class="btn btn-quiet" id="m-copy">Copiar código</button>
        ${extra.password ? `<button class="btn btn-quiet" id="m-copy-pass">Copiar senha</button>` : ""}
        <button class="btn btn-quiet" id="m-cancel">Cancelar</button>
      </div>
      <div class="hint">${escapeHtml(st.verification_url || "")}</div>
      <div class="signup-log" id="m-log" style="display:none"></div>
    </div>
  `;
  document.body.appendChild(overlay);
  $("#m-open", overlay).onclick = () => OpenExternal(st.verification_url);
  $("#m-copy", overlay).onclick = async () => {
    await navigator.clipboard.writeText(st.user_code);
  };
  const cp = $("#m-copy-pass", overlay);
  if (cp) {
    cp.onclick = async () => {
      await navigator.clipboard.writeText(extra.password || "");
      cp.textContent = "Senha copiada";
      setTimeout(() => (cp.textContent = "Copiar senha"), 1200);
    };
  }
  $("#m-cancel", overlay).onclick = () => {
    CancelDeviceLogin();
    CancelAutoSignup().catch(() => {});
    state.device = null;
    overlay.remove();
  };
}

async function startAutoSignupUI() {
  closeOverlay();
  const overlay = document.createElement("div");
  overlay.className = "overlay overlay-glass";
  overlay.innerHTML = `
    <div class="sheet">
      <h3>Criação automática</h3>
      <p>Chrome isolate + darkemail. Pode levar 1–3 min. Não feche o app.</p>
      <div class="signup-log" id="m-log">preparando…</div>
      <div class="sheet-actions">
        <button class="btn btn-quiet" id="m-cancel">Cancelar</button>
      </div>
    </div>
  `;
  document.body.appendChild(overlay);
  const log = (msg) => {
    const el = $("#m-log", overlay);
    if (el) el.textContent = msg;
    const st = $("#status-text");
    if (st) st.innerHTML = `Signup · <strong>${escapeHtml(msg)}</strong>`;
  };
  $("#m-cancel", overlay).onclick = () => {
    CancelAutoSignup().catch(() => {});
    overlay.remove();
  };
  try {
    await StartAutoSignup();
    log("signup iniciado…");
  } catch (e) {
    log("erro: " + e);
    alert("Auto signup: " + e);
  }
}


async function submit() {
  const promptEl = $("#prompt");
  const text = (promptEl?.value || "").trim();
  if (!text || state.streaming) return;
  if (!activeAccount()) {
    alert("Adicione e selecione uma conta primeiro.");
    return;
  }

  const model =
    state.menus["c-model"]?.getValue?.() || state.picks.cModel || state.settings.default_model;
  const effort =
    state.menus["c-effort"]?.getValue?.() || state.picks.cEffort || state.settings.reasoning_effort;
  const pNow = (state.settings?.provider || "xai").toLowerCase();
  const isKimi =
    pNow === "kimi_work" || pNow === "kimi" || pNow === "kimi-work";
  let apiMode =
    state.menus["c-api"]?.getValue?.() || state.picks.cApi || state.settings.api_mode;
  if (isKimi) apiMode = "chat";

  state.messages.push({ role: "user", content: text });
  state.messages.push({
    role: "assistant",
    content: "",
    thinking: "",
    streaming: true,
    tools: [],
    searches: [],
    search: null,
    citations: [],
  });
  promptEl.value = "";
  autoGrow(promptEl);
  state.streaming = true;
  thinkChars = 0;
  state.sessionCost = 0;
  state.sessionLat = null;
  $("#sess-in").textContent = "0";
  $("#sess-out").textContent = "0";
  $("#sess-think").textContent = "0";
  $("#sess-cost").textContent = "$0";
  const latEl = $("#sess-lat");
  if (latEl) {
    latEl.style.display = "none";
    latEl.textContent = "";
  }
  {
    const stEl = $("#sess-think");
    if (stEl) delete stEl.dataset.final;
  }
  paintSend();
  paintStatus();
  paintMessages();

  const payload = {
    model,
    messages: [{ role: "user", content: text }],
    stream: true,
    reasoning_effort: effort,
    api_mode: apiMode,
  };

  // full history for chat mode continuity in UI only; for API send context
  if (apiMode === "chat") {
    payload.messages = state.messages
      .filter((m) => m.role === "user" || (m.role === "assistant" && m.content && !m.streaming))
      .map((m) => ({ role: m.role, content: m.content }));
    // last user already included; drop incomplete assistant
    if (payload.messages.at(-1)?.role === "assistant") payload.messages.pop();
  } else if (state.lastResponseId) {
    payload.last_response_id = state.lastResponseId;
    payload.messages = [{ role: "user", content: text }];
  } else {
    // first responses turn — can send conversation so far as messages
    payload.messages = state.messages
      .filter((m) => m.role === "user" || (m.role === "assistant" && m.content && !m.streaming))
      .map((m) => ({ role: m.role, content: m.content }));
    if (payload.messages.at(-1)?.role === "assistant") payload.messages.pop();
  }

  try {
    await SendChat(payload);
  } catch (e) {
    state.streaming = false;
    const last = state.messages.at(-1);
    if (last?.role === "assistant") {
      last.content = safeErrorText(e);
      last.isError = true;
      last.streaming = false;
    }
    paintSend();
    paintMessages();
  }
}

let thinkChars = 0;
let paintScheduled = false;

function schedulePaintMessages() {
  if (paintScheduled) return;
  paintScheduled = true;
  requestAnimationFrame(() => {
    paintScheduled = false;
    paintMessages();
  });
}

function domainFromUrl(u) {
  try {
    return new URL(u).hostname.replace(/^www\./, "");
  } catch {
    return "";
  }
}

function kindLabel(kind) {
  if (kind === "x") return "X";
  return "Web";
}

function ensureSearch(last, id, kind) {
  if (!last.searches) last.searches = [];
  let s = last.searches.find((x) => x.id === id);
  if (!s) {
    s = {
      id,
      kind: kind || "web",
      query: "",
      results: [],
      status: "searching",
      provider: "xAI",
    };
    last.searches.push(s);
  }
  // keep legacy single search pointer for paint/compat
  last.search = s;
  return s;
}

function ensureTool(last, id, name) {
  if (!last.tools) last.tools = [];
  let t = last.tools.find((x) => x.id === id);
  if (!t) {
    t = { id, name: name || "web_search", status: "running", query: "" };
    last.tools.push(t);
  }
  return t;
}

function faviconUrl(domainOrUrl) {
  const d = domainOrUrl.includes(".") && !domainOrUrl.includes("://")
    ? domainOrUrl
    : domainFromUrl(domainOrUrl) || domainOrUrl;
  if (!d) return "";
  return `https://www.google.com/s2/favicons?domain=${encodeURIComponent(d)}&sz=64`;
}

function renderFavStack(results, max = 5) {
  const list = (results || []).slice(0, max);
  if (!list.length) {
    return `
      <div class="ms-favs ghost">
        <span class="ms-fav shimmer"></span>
        <span class="ms-fav shimmer"></span>
        <span class="ms-fav shimmer"></span>
      </div>
    `;
  }
  const rest = Math.max(0, (results || []).length - max);
  return `
    <div class="ms-favs">
      ${list
        .map((r, i) => {
          const domain = r.domain || domainFromUrl(r.url) || "?";
          const src = faviconUrl(domain);
          return `
            <span class="ms-fav" style="z-index:${20 - i};animation-delay:${i * 40}ms" title="${escapeHtml(domain)}">
              ${
                src
                  ? `<img src="${escapeHtml(src)}" alt="" loading="lazy" onerror="this.parentElement.classList.add('fallback');this.remove()"/>`
                  : ""
              }
              <span class="ms-fav-letter">${escapeHtml((domain[0] || "?").toUpperCase())}</span>
            </span>
          `;
        })
        .join("")}
      ${rest > 0 ? `<span class="ms-fav more">+${rest}</span>` : ""}
    </div>
  `;
}

function renderSourceCards(results) {
  const list = (results || []).slice(0, 10);
  if (!list.length) return "";
  return `
    <div class="ms-sources">
      ${list
        .map((r, idx) => {
          const domain = r.domain || domainFromUrl(r.url) || "source";
          const title = r.title || domain;
          const fav = faviconUrl(domain);
          return `
            <a class="ms-card" href="${escapeHtml(r.url || "#")}" target="_blank" rel="noopener noreferrer" style="animation-delay:${idx * 35}ms">
              <div class="ms-card-icon">
                ${
                  fav
                    ? `<img src="${escapeHtml(fav)}" alt="" loading="lazy" onerror="this.style.display='none';this.nextElementSibling.style.display='grid'"/>`
                    : ""
                }
                <span class="ms-card-letter" style="${fav ? "display:none" : ""}">${escapeHtml((domain[0] || "?").toUpperCase())}</span>
              </div>
              <div class="ms-card-body">
                <div class="ms-card-title">${escapeHtml(title)}</div>
                <div class="ms-card-domain">${escapeHtml(domain)}</div>
              </div>
              <span class="ms-card-arrow">↗</span>
            </a>
          `;
        })
        .join("")}
    </div>
  `;
}

function renderSearchBlock(m) {
  const items = m.searches?.length
    ? m.searches
    : m.search
      ? [m.search]
      : [];
  const toolsRunning = (m.tools || []).some((t) => t.status === "running");
  if (!items.length && !toolsRunning) return "";

  // Aggregate for Manus-style single research panel
  const anyLive =
    toolsRunning ||
    items.some((s) => s.status === "searching" || s.status === "running");
  const anyErr = items.some((s) => s.status === "error");
  const allResults = [];
  const queries = [];
  const kinds = new Set();
  for (const s of items) {
    if (s.query) queries.push(s.query);
    if (s.kind) kinds.add(s.kind);
    for (const r of s.results || []) {
      if (!allResults.some((x) => x.url === r.url)) allResults.push(r);
    }
  }
  const primaryQ = queries[0] || "";
  const kindTxt =
    kinds.has("web") && kinds.has("x")
      ? "Web · X"
      : kinds.has("x")
        ? "X"
        : "Web";

  let statusLine = "Researching the web";
  let statusClass = "live";
  if (anyErr && !anyLive) {
    statusLine = "Search failed";
    statusClass = "err";
  } else if (!anyLive && allResults.length) {
    statusLine = `Found ${allResults.length} source${allResults.length === 1 ? "" : "s"}`;
    statusClass = "done";
  } else if (!anyLive && items.length) {
    statusLine = "Research complete";
    statusClass = "done";
  }

  const steps = items
    .map((s) => {
      const st = s.status || "searching";
      const live = st === "searching" || st === "running";
      const icon = live ? "◌" : st === "error" ? "!" : "✓";
      return `
        <div class="ms-step ${st}">
          <span class="ms-step-ico">${icon}</span>
          <div class="ms-step-main">
            <span class="ms-step-kind">${escapeHtml(kindLabel(s.kind || "web"))}</span>
            <span class="ms-step-q">${escapeHtml(s.query || (live ? "Looking up…" : "—"))}</span>
          </div>
          ${live ? `<span class="ms-step-spin"></span>` : ""}
        </div>
      `;
    })
    .join("");

  return `
    <div class="ms-panel ${statusClass}">
      <div class="ms-head">
        <div class="ms-head-left">
          <div class="ms-orb ${anyLive ? "spin" : ""}" aria-hidden="true">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none">
              <circle cx="12" cy="12" r="9" stroke="currentColor" stroke-width="1.5" opacity="0.35"/>
              <path d="M3 12h18M12 3c2.5 2.8 3.8 5.8 3.8 9s-1.3 6.2-3.8 9c-2.5-2.8-3.8-5.8-3.8-9S9.5 5.8 12 3z" stroke="currentColor" stroke-width="1.5"/>
            </svg>
          </div>
          <div class="ms-head-text">
            <div class="ms-status">${escapeHtml(statusLine)}</div>
            <div class="ms-sub">
              <span class="ms-badge">${escapeHtml(kindTxt)}</span>
              ${primaryQ ? `<span class="ms-query">“${escapeHtml(primaryQ)}”</span>` : ""}
              ${items.length > 1 ? `<span class="ms-meta">${items.length} queries</span>` : ""}
            </div>
          </div>
        </div>
        ${renderFavStack(allResults)}
      </div>
      ${
        anyLive && !allResults.length
          ? `
        <div class="ms-loading">
          <div class="ms-bar"><i></i></div>
          <div class="ms-skeleton">
            <div class="ms-sk"></div>
            <div class="ms-sk"></div>
            <div class="ms-sk short"></div>
          </div>
        </div>
      `
          : ""
      }
      ${items.length > 1 || anyLive ? `<div class="ms-steps">${steps || ""}</div>` : ""}
      ${renderSourceCards(allResults)}
      ${
        anyErr
          ? `<div class="ms-error">${escapeHtml(items.find((s) => s.error)?.error || "Search error")}</div>`
          : ""
      }
    </div>
  `;
}

function onSearchEvent(type, payload) {
  const last = state.messages.at(-1);
  if (!last || last.role !== "assistant") return;

  const kind =
    payload?.kind === "x" || String(payload?.name || "").startsWith("x_")
      ? "x"
      : "web";
  const id = payload?.tool_call_id || payload?.id || "search";

  if (type === "search:start") {
    const s = ensureSearch(last, id, kind);
    s.status = "searching";
    s.query = payload?.query || s.query || "";
    s.kind = payload?.kind || kind;
    s.provider = "xAI";
    const t = ensureTool(last, id, kind === "x" ? "x_search" : "web_search");
    t.status = "running";
    t.query = s.query;
  } else if (type === "search:results") {
    const s = ensureSearch(last, id, payload?.kind || kind);
    s.query = payload?.query || s.query || "";
    s.results = (payload?.results || []).map((r) => ({
      ...r,
      domain: r.domain || domainFromUrl(r.url),
    }));
    s.duration_ms = payload?.duration_ms;
    s.status = "done";
    s.kind = payload?.kind || s.kind || kind;
    s.provider = "xAI";
    const t = ensureTool(last, id, s.kind === "x" ? "x_search" : "web_search");
    t.status = "done";
    t.query = s.query;
  } else if (type === "search:error") {
    const s = ensureSearch(last, id, kind);
    s.status = "error";
    s.error = payload?.error || "erro";
    const t = ensureTool(last, id, "web_search");
    t.status = "error";
  } else if (type === "search:done") {
    const s = last.searches?.find((x) => x.id === id) || last.search;
    if (s && s.status === "searching") s.status = "done";
  } else if (type === "tool:call") {
    const name = payload?.name || "web_search";
    const k = name.includes("x_") || name === "x_search" ? "x" : "web";
    ensureTool(last, id, name);
    ensureSearch(last, id, k).status = "searching";
  } else if (type === "tool:done") {
    const t = ensureTool(last, id, payload?.name || "web_search");
    t.status = "done";
    const s = last.searches?.find((x) => x.id === id);
    if (s && s.status === "searching") s.status = "done";
  }
  schedulePaintMessages();
}

function onChatEventTool(ev) {
  const last = state.messages.at(-1);
  if (!last || last.role !== "assistant") return false;

  if (ev.type === "tool_call") {
    onSearchEvent("tool:call", {
      id: ev.id,
      name: ev.text,
      kind: ev.payload?.kind,
    });
    return true;
  }
  if (ev.type === "search_query") {
    onSearchEvent("search:start", {
      query: ev.text,
      tool_call_id: ev.id,
      provider: "xAI",
      kind: ev.payload?.kind,
    });
    return true;
  }
  if (ev.type === "search_results") {
    onSearchEvent("search:results", {
      ...(ev.payload || {}),
      query: ev.text || ev.payload?.query,
      tool_call_id: ev.id,
      provider: "xAI",
    });
    return true;
  }
  if (ev.type === "tool_done") {
    onSearchEvent("tool:done", { id: ev.id, name: ev.text });
    return true;
  }
  if (ev.type === "tool_error") {
    onSearchEvent("search:error", { error: ev.error, tool_call_id: ev.id });
    return true;
  }
  if (ev.type === "citation") {
    if (!last.citations) last.citations = [];
    const url = ev.payload?.url || ev.text;
    if (url && !last.citations.some((c) => c.url === url)) {
      last.citations.push({ url, title: ev.payload?.title || "" });
    }
    return true;
  }
  return false;
}

function onChatEvent(ev) {
  const last = state.messages.at(-1);
  if (!last || last.role !== "assistant") return;

  if (onChatEventTool(ev)) return;

  if (ev.type === "thinking") {
    last.thinking = (last.thinking || "") + (ev.text || "");
    thinkChars += (ev.text || "").length;
    const approx = Math.max(0, Math.round(thinkChars / 4));
    const el = $("#sess-think");
    if (el && !el.dataset.final) el.textContent = fmt(approx) + "~";
  } else if (ev.type === "content") {
    last.content = (last.content || "") + (ev.text || "");
  } else if (ev.type === "usage" && ev.usage) {
    const u = ev.usage;
    const est = ev.estimated ? " · est." : "";
    last.meta = `${u.prompt_tokens || 0} in · ${u.completion_tokens || 0} out · ${u.reasoning_tokens || 0} think · ${fmtMs(ev.latency_ms)}${est}`;
    $("#sess-in").textContent = fmt(u.prompt_tokens);
    $("#sess-out").textContent = fmt(u.completion_tokens);
    $("#sess-think").textContent = fmt(u.reasoning_tokens);
    $("#sess-think").dataset.final = "1";
    // cost approx client-side until stats event (Grok 4.5 rates)
    const cost =
      ((u.prompt_tokens || 0) * 2 +
        (u.completion_tokens || 0) * 6 +
        (u.reasoning_tokens || 0) * 6) /
      1e6;
    state.sessionCost = cost;
    $("#sess-cost").textContent = fmtUSD(cost);
    if (ev.latency_ms) {
      const latEl = $("#sess-lat");
      if (latEl) {
        latEl.style.display = "";
        latEl.textContent = fmtMs(ev.latency_ms);
      }
    }
  } else if (ev.type === "done") {
    last.streaming = false;
    state.streaming = false;
    if (ev.id) state.lastResponseId = ev.id;
    if (ev.model) last.meta = (last.meta ? last.meta + " · " : "") + ev.model;
    if (ev.latency_ms && last.meta && !last.meta.includes("ms") && !last.meta.includes(" s")) {
      last.meta += " · " + fmtMs(ev.latency_ms);
    }
    paintSend();
    paintStatus();
  } else if (ev.type === "error") {
    const msg = safeErrorText(ev.error);
    last.content = (last.content || "") + (last.content ? "\n" : "") + msg;
    last.isError = true;
    last.streaming = false;
    state.streaming = false;
    paintSend();
    paintStatus();
  }

  schedulePaintMessages();
}

async function refreshBootstrap(full = true) {
  const b = await GetBootstrap();
  state.settings = b.settings || {};
  state.accounts = b.accounts || [];
  state.usage = b.usage || {};
  state.proxyBase = b.proxy_base || "";
  state.dataDir = b.data_dir || "";
  state.activeRequest = b.active_request || null;
  if (full || !state.models.length) {
    try {
      state.models = await ListModels();
    } catch (_) {
      state.models = [
        { id: "grok-4.5", name: "Grok 4.5" },
        { id: "grok-4.5-responses", name: "Grok 4.5 (Responses)" },
      ];
    }
  }
  paintChrome();
}

function wireEvents() {
  EventsOn("kimi:login", (p) => {
    const phase = p?.phase || "unknown";
    const msg = p?.message || String(p || "");
    addLog("kimi-login", msg, { phase, error: p?.error || null });
  });

  EventsOn("chat:event", onChatEvent);
  EventsOn("search:start", (p) => onSearchEvent("search:start", p));
  EventsOn("search:results", (p) => onSearchEvent("search:results", p));
  EventsOn("search:error", (p) => onSearchEvent("search:error", p));
  EventsOn("search:done", (p) => onSearchEvent("search:done", p));
  EventsOn("tool:call", (p) => onSearchEvent("tool:call", p));
  EventsOn("tool:args", (p) => onSearchEvent("tool:args", p));
  EventsOn("tool:done", (p) => onSearchEvent("tool:done", p));
  EventsOn("request:active", (req) => {
    state.activeRequest = req;
    paintStatus();
  });
  EventsOn("usage:update", (u) => {
    state.usage = u || {};
    const g = globalUsage();
    const set = (id, val) => {
      const n = document.getElementById(id);
      if (n) n.textContent = val;
    };
    set("u-total", fmt(g.total_tokens));
    set("u-cost", fmtUSD(g.cost_usd));
    set("u-prompt", fmt(g.prompt_tokens));
    set("u-comp", fmt(g.completion_tokens));
    set("u-reason", fmt(g.reasoning_tokens));
    if (g.latency_samples > 0) {
      set("u-lat", fmtMs(g.latency_sum_ms / g.latency_samples));
    }
  });
  EventsOn("stats:sample", (sample) => {
    if (sample?.cost_usd != null) {
      state.sessionCost = sample.cost_usd;
      const el = $("#sess-cost");
      if (el) el.textContent = fmtUSD(sample.cost_usd);
    }
  });
  EventsOn("auth:success", async (payload) => {
    addLog("auth", "Auth success", payload);
    state.device = null;
    document.querySelector(".overlay")?.remove();
    await refreshBootstrap(true);
    const n = payload?.count || state.accounts.length;
    // soft toast via status line
    const st = $("#status-text");
    if (st) {
      st.innerHTML = `Conta adicionada · <strong>${escapeHtml(payload?.email || payload?.label || "")}</strong> · ${n} no total`;
    }
  });

  EventsOn("auth:error", (msg) => {
    addLog("auth", "Auth error", { error: msg });
    alert("Auth error: " + msg);
    state.device = null;
    document.querySelector(".overlay")?.remove();
  });
  EventsOn("signup:progress", (p) => {
    const msg = p?.message || String(p || "");
    addLog("signup", msg, p);
    const el = document.querySelector("#m-log");
    if (el) el.textContent = msg;
    const st = $("#status-text");
    if (st) st.innerHTML = `Signup · <strong>${escapeHtml(msg)}</strong>`;
  });
  EventsOn("signup:error", (msg) => {
    addLog("signup", "Signup error", { error: msg });
    alert("Signup: " + msg);
    const st = $("#status-text");
    if (st) st.textContent = "Signup falhou";
  });
  EventsOn("signup:web_ok", (p) => {
    addLog("signup", "Web OK", p);
    const st = $("#status-text");
    if (st) st.innerHTML = `Conta web · <strong>${escapeHtml(p?.email || "")}</strong> criada`;
  });
  EventsOn("signup:device", (p) => {
    addLog("signup", "Device", p);
    if (p?.user_code) {
      showDeviceModal(
        { user_code: p.user_code, verification_url: p.verification_url },
        { title: "Device OAuth — conta nova", email: p.email, password: p.password }
      );
    }
  });
  EventsOn("signup:done", (p) => {
    addLog("signup", "Done", p);
    const st = $("#status-text");
    if (st) st.innerHTML = `Signup · fase <strong>${escapeHtml(p?.phase || "done")}</strong>`;
  });
  EventsOn("signup:auto_triggered", () => {
    addLog("signup", "Auto triggered (quota exhausted)");
    const st = $("#status-text");
    if (st) st.innerHTML = `Cota esgotada — <strong>criando conta nova…</strong>`;
  });
  EventsOn("kimi:relogin", async (p) => {
    addLog("kimi-relogin", p?.message || "relogin", p);
    const st = $("#status-text");
    const phase = p?.phase || "";
    if (st) {
      if (phase === "start") {
        st.innerHTML = `Cota Kimi — <strong>${p?.http ? "re-login HTTP…" : "re-login (perfil limpo)…"}</strong>`;
      } else if (phase === "ok") {
        st.innerHTML = `Kimi renovada · <strong>${escapeHtml(p?.account?.email || p?.account?.label || "ok")}</strong>`;
      } else if (phase === "error") {
        st.innerHTML = `Falha re-login Kimi · <strong>${escapeHtml(p?.error || p?.message || "")}</strong>`;
      } else if (p?.message) {
        st.innerHTML = escapeHtml(String(p.message));
      }
    }
    if (phase === "ok") await refreshBootstrap(false);
  });
  EventsOn("account:exhausted", async (p) => {
    addLog("account", "Exhausted", p);
    const st = $("#status-text");
    if (st) {
      if (p?.logoff) {
        st.innerHTML = `Kimi apagada · <strong>${escapeHtml(p?.email || p?.id || "")}</strong> — recriando…`;
      } else {
        st.innerHTML = `Conta esgotada · <strong>${escapeHtml(p?.email || p?.id || "")}</strong>`;
      }
    }
    await refreshBootstrap(false);
  });
  EventsOn("account:rotated", async (p) => {
    addLog("account", "Rotated", p);
    await refreshBootstrap(false);
    const st = $("#status-text");
    if (st) st.innerHTML = `Trocou pra conta <strong>${escapeHtml(p?.id || "")}</strong>`;
  });
}

async function main() {
  wireEvents();
  await refreshBootstrap(true);
}

main().catch((e) => {
  document.body.innerHTML = `<pre style="color:#f88;padding:24px;font-family:monospace">Falha UI: ${e}</pre>`;
});
