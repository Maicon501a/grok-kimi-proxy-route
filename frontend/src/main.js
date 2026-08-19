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
  StartCodexLogin,
  CancelDeviceLogin,
  StartAccioLogin,
  AccioStatus,
  AccioCredits,
  OpenExternal,
  UpdateSettings,
  GetSystemPrompt,
  SetSystemPrompt,
  SendChat,
  CancelChat,
  GetStats,
  StartAutoSignup,
  CancelAutoSignup,
  IsSignupRunning,
  SetAutoCreateOnExhausted,
  GetAutoCreateOnExhausted,
  SetAutoCreateMinAccounts,
  GetAutoCreateMinAccounts,
  StartSignupBatch,
  GetGoogleCredentials,
  SetGoogleCredentials,
  GetAccountGoogleCredentials,
  SetAccountGoogleCredentials,
  TestKimiGoogleCredentials,
  StartKimiBrowserLogin,
  StartKimiStealthLoginNewAccount,
  AddKimiFromJWT,
  AddKimiAPIKey,
  LogoffKimiAccount,
  StartGeminiLogin,
  CompleteGeminiLogin,
  CancelGeminiLogin,
  ValidateGeminiAccount,
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
  logsModal: null,
  lastChatEventLogAt: 0,
  accioCredits: null,
  accioPolling: false,
  // custom dropdowns
  picks: {
    effort: "high",
    api: "chat",
    model: "grok-4.6",
    cEffort: "high",
    cApi: "chat",
    cModel: "grok-4.6",
  },
  menus: {},
};

// Completed turns are immutable. Cache their sanitized markdown so a long
// streaming answer does not re-parse the whole conversation every frame.
const messageMarkupCache = new WeakMap();

/** Short label for long model ids (Ollie full paths, aliases, etc.). */
function shortModelLabel(name, id) {
  let s = normalizeAccioModelLabel(name, id);
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

// Older Accio catalog responses sometimes expose the opaque modelCode as the
// display name (for example 1Orbit-Q2XN3dX6cP2m). Keep the full ID as the
// option value, but make the visible fallback readable when the server has
// not yet supplied modelDisplayName/labelList.
function normalizeAccioModelLabel(name, id) {
  const rawId = String(id || "").trim().replace(/^accio\//i, "");
  const rawName = String(name || id || "").trim();
  const candidate = rawName.replace(/^accio\//i, "");
  if (
    !rawId ||
    candidate.toLowerCase() !== rawId.toLowerCase() ||
    !/^1[A-Za-z][A-Za-z0-9]*-[A-Za-z0-9]{8,}$/.test(candidate)
  ) {
    return rawName;
  }
  const dash = candidate.indexOf("-");
  const family = candidate.slice(0, dash).replace(/^1/, "");
  const variant = candidate.slice(dash + 1);
  return variant ? `${family} - ${variant}` : family;
}

// Keep this at module scope: refreshBootstrap() runs outside ensureShell().
function fallbackModels(prov) {
  const p = (prov || state.settings?.provider || "xai").toLowerCase();
  if (p === "qwen" || p === "qwenbridge") {
    return [{ id: "qwen3.8", name: "qwen3.8" }];
  }
  if (p === "deepseek") {
    return [
      { id: "deepseek-v4-flash", name: "deepseek-v4-flash" },
      { id: "deepseek-v4-pro", name: "deepseek-v4-pro" },
    ];
  }
  if (["openai_codex", "codex", "openai-codex", "chatgpt"].includes(p)) {
    return [
      { id: "codex/gpt-5.6-sol", name: "GPT-5.6-Sol" },
      { id: "codex/gpt-5.6-terra", name: "GPT-5.6-Terra" },
      { id: "codex/gpt-5.6-luna", name: "GPT-5.6-Luna" },
      { id: "codex/gpt-5.5", name: "GPT-5.5" },
      { id: "codex/gpt-5.2", name: "GPT-5.2" },
    ];
  }
  if (p === "opencode_go" || p === "opencode-go") {
    return [
      { id: "opencode-go/deepseek-v4-pro", name: "DeepSeek V4 Pro" },
      { id: "opencode-go/deepseek-v4-flash", name: "DeepSeek V4 Flash" },
      { id: "opencode-go/gpt-5.6-luna", name: "GPT-5.6 Luna" },
      { id: "opencode-go/grok-4.5", name: "Grok 4.5" },
      { id: "opencode-go/hy3", name: "Hy3" },
      { id: "opencode-go/kimi-k2.5", name: "Kimi K2.5" },
      { id: "opencode-go/kimi-k2.6", name: "Kimi K2.6" },
      { id: "opencode-go/kimi-k2.7-code", name: "Kimi K2.7 Code" },
      { id: "opencode-go/kimi-k3", name: "Kimi K3" },
      { id: "opencode-go/glm-5", name: "GLM 5" },
      { id: "opencode-go/glm-5.1", name: "GLM 5.1" },
      { id: "opencode-go/glm-5.2", name: "GLM 5.2" },
      { id: "opencode-go/mimo-v2.5", name: "MiMo V2.5" },
      { id: "opencode-go/mimo-v2.5-pro", name: "MiMo V2.5 Pro" },
      { id: "opencode-go/mimo-v2-omni", name: "MiMo V2 Omni" },
      { id: "opencode-go/mimo-v2-pro", name: "MiMo V2 Pro" },
      { id: "opencode-go/minimax-m2.5", name: "MiniMax M2.5" },
      { id: "opencode-go/minimax-m2.7", name: "MiniMax M2.7" },
      { id: "opencode-go/minimax-m3", name: "MiniMax M3" },
      { id: "opencode-go/qwen3.5-plus", name: "Qwen3.5 Plus" },
      { id: "opencode-go/qwen3.6-plus", name: "Qwen3.6 Plus" },
      { id: "opencode-go/qwen3.7-plus", name: "Qwen3.7 Plus" },
      { id: "opencode-go/qwen3.7-max", name: "Qwen3.7 Max" },
      { id: "opencode-go/qwen3.8-max", name: "Qwen3.8 Max" },
      { id: "opencode-go/big-pickle", name: "Big Pickle" },
      { id: "opencode-go/deepseek-v4-flash-free", name: "DeepSeek V4 Flash Free" },
      { id: "opencode-go/laguna-s-2.1-free", name: "Laguna S 2.1" },
      { id: "opencode-go/ling-3.0-tiny-free", name: "Ling 3.0 Tiny" },
      { id: "opencode-go/longcat-2.0-free", name: "LongCat 2.0" },
      { id: "opencode-go/mimo-v2.5-free", name: "MiMo V2.5 Free" },
      { id: "opencode-go/nemotron-3-ultra-free", name: "Nemotron 3 Ultra" },
      { id: "opencode-go/nemotron-3.5-lightning-free", name: "Nemotron 3.5 Lightning" },
    ];
  }
  if (p === "accio" || p === "accio-work" || p === "phoenix") {
    return [{ id: "accio/1Nexus-R36W8qJ5vB6h", name: "Accio Nexus" }];
  }
  if (p === "ollie") {
    return [
      { id: "claude-sonnet-5", name: "claude-sonnet-5" },
      { id: "claude-fable-5", name: "claude-fable-5" },
      { id: "claude-opus-4-8", name: "claude-opus-4-8" },
      { id: "deepseek-v4-flash-free", name: "deepseek-v4-flash-free" },
    ];
  }
  if (["opencode_zen", "opencode-zen", "opencode", "zen", "zen-free"].includes(p)) {
    return [
      { id: "opencode/deepseek-v4-flash-free", name: "DeepSeek V4 Flash Free" },
      { id: "opencode/big-pickle", name: "Big Pickle" },
      { id: "opencode/mimo-v2.5-free", name: "MiMo V2.5 Free" },
      { id: "opencode/nemotron-3-ultra-free", name: "Nemotron 3 Ultra Free" },
      { id: "opencode/north-mini-code-free", name: "North Mini Code Free" },
      { id: "opencode/ling-3.0-flash-free", name: "Ling 3.0 Flash Free" },
      { id: "opencode/laguna-s-2.1-free", name: "Laguna S 2.1 Free" },
    ];
  }
  if (p === "gemini" || p === "google" || p === "vertex") {
    return [
      { id: "gemini-3.7-flash", name: "gemini-3.7-flash" },
      { id: "gemini-3.6-flash", name: "gemini-3.6-flash" },
      { id: "gemini-3.5-flash", name: "gemini-3.5-flash" },
      { id: "gemini-3.1-pro-preview", name: "gemini-3.1-pro-preview" },
      { id: "gemini-2.5-pro", name: "gemini-2.5-pro" },
      { id: "gemini-2.5-flash", name: "gemini-2.5-flash" },
      { id: "gemini-2.5-flash-image", name: "gemini-2.5-flash-image" },
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
    { id: "grok-4.6", name: "Grok 4.6" },
    { id: "grok-4.6-responses", name: "Grok 4.6 (Responses)" },
  ];
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
    root._menu = menu;
    opts.forEach((o) => {
      const item = document.createElement("button");
      item.type = "button";
      item.className = "dd-item" + (o.value === root._value ? " active" : "");
      if (o.status) item.className += ` dd-item-status-${o.status}`;
      item.role = "option";
      const itemTitle = o.value && o.value !== o.label ? o.value : o.label;
      item.title = itemTitle;
      const status = o.statusLabel
        ? `<span class="dd-item-status-label">${escapeHtml(o.statusLabel)}</span>`
        : "";
      item.innerHTML = `<span class="dd-item-main"><span class="dd-item-label">${escapeHtml(o.label)}</span>${status}</span><span class="check">✓</span>`;
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
        root.classList.add("open");
        menu.classList.add("dd-menu-fixed");
        positionMenu(root, menu);
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
      root._menu = menu;
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
          root.classList.add("open");
          menu.classList.add("dd-menu-fixed");
          positionMenu(root, menu);
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

function positionMenu(root, menu) {
  if (!root || !menu || !root.classList.contains("open")) return;
  const rect = root.getBoundingClientRect();
  const isChip = root.classList.contains("dd-chip");
  if (!isChip) menu.style.width = `${Math.max(120, rect.width)}px`;
  const edge = 8;
  const menuWidth = menu.getBoundingClientRect().width || rect.width;
  const left = Math.max(edge, Math.min(rect.left, window.innerWidth - menuWidth - edge));
  const spaceBelow = Math.max(0, window.innerHeight - rect.bottom - edge);
  const spaceAbove = Math.max(0, rect.top - edge);
  const naturalHeight = menu.scrollHeight || menu.offsetHeight || 240;
  // Prefer the side with more room and always constrain the menu to the
  // viewport, so model lists remain usable on short windows and near the
  // bottom of the sidebar/composer.
  const openUp = spaceAbove > spaceBelow && spaceAbove >= 80;
  const available = Math.max(80, Math.min(240, (openUp ? spaceAbove : spaceBelow) - 6));
  menu.style.maxHeight = `${available}px`;
  const menuHeight = Math.min(menu.getBoundingClientRect().height || naturalHeight, available);
  const preferredTop = openUp ? rect.top - menuHeight - 6 : rect.bottom + 6;
  const top = Math.max(edge, Math.min(preferredTop, window.innerHeight - menuHeight - edge));
  menu.classList.toggle("drop-up", openUp);
  menu.style.left = `${left}px`;
  menu.style.bottom = "auto";
  menu.style.top = `${Math.max(8, top)}px`;
}

function repositionOpenMenus() {
  document.querySelectorAll(".dd.open").forEach((root) => positionMenu(root, root._menu));
}

function closeAllMenus() {
  document.querySelectorAll(".dd.open").forEach((el) => el.classList.remove("open"));
  document.querySelectorAll(".dd-menu-fixed").forEach((el) => el.classList.remove("dd-menu-fixed"));
}

window.addEventListener("resize", repositionOpenMenus);
window.addEventListener("scroll", repositionOpenMenus, true);

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
  // Accio accounts are synchronized into the same store pool as the other
  // session providers. Never synthesize an "accio" row: it hides the real
  // account id and prevents the account picker from selecting the token that
  // the proxy will actually use.
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
            <button class="btn btn-quiet" id="btn-pool">Criar em lote</button>
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
          <section class="system-prompt-card" aria-labelledby="system-prompt-heading">
            <div class="system-prompt-card-head">
              <div>
                <span class="field-label" id="system-prompt-heading">System prompt</span>
                <span class="system-prompt-scope" id="system-prompt-scope">—</span>
              </div>
              <span class="system-prompt-count" id="system-prompt-count">0</span>
            </div>
            <textarea id="system-prompt-input" rows="7" spellcheck="false" placeholder="Instruções adicionadas pelo proxy a este modelo…"></textarea>
            <div class="system-prompt-actions">
              <button class="btn btn-quiet system-prompt-clear" id="system-prompt-clear" type="button">Apagar</button>
              <button class="btn btn-solid system-prompt-save" id="system-prompt-save" type="button">Salvar</button>
            </div>
            <p class="system-prompt-note">Aplicado no chat do app e em toda chamada que usar este provedor + modelo.</p>
          </section>
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
            <div id="accio-credit-avatar" class="accio-credit-avatar" style="display:none"></div>
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
  $("#btn-pool").onclick = showPoolBatchModal;
  $("#btn-accounts").onclick = openAccountsModal;
  $("#btn-stats").onclick = openStatsModal;
  $("#btn-stats-top").onclick = openStatsModal;
  $("#btn-logs").onclick = openLogsModal;

  const fallbackEffortOpts = [
    { value: "low", label: "Low" },
    { value: "medium", label: "Medium" },
    { value: "high", label: "High" },
    { value: "xhigh", label: "xHigh" },
  ];
  const selectedModelId = (composer = false) =>
    composer
      ? state.picks.cModel || state.settings.default_model
      : state.picks.model || state.settings.default_model;
  const effortOptionsForModel = (modelId) => {
    const model = state.models.find((m) => m.id === modelId);
    const efforts = Array.isArray(model?.reasoning_efforts)
      ? model.reasoning_efforts.filter(Boolean)
      : [];
    return efforts.length
      ? efforts.map((value) => ({ value, label: value }))
      : fallbackEffortOpts;
  };
  const syncEffortMenus = () => {
    const globalOptions = effortOptionsForModel(selectedModelId(false));
    const composerOptions = effortOptionsForModel(selectedModelId(true));
    state.menus["set-effort"]?.refresh?.();
    state.menus["c-effort"]?.refresh?.();
    const globalCurrent = globalOptions.some((o) => o.value === state.picks.effort)
      ? state.picks.effort
      : globalOptions[0]?.value;
    const composerCurrent = composerOptions.some((o) => o.value === state.picks.cEffort)
      ? state.picks.cEffort
      : composerOptions[0]?.value;
    if (globalCurrent) {
      state.picks.effort = globalCurrent;
      state.menus["set-effort"]?.setValue(globalCurrent);
    }
    if (composerCurrent) {
      state.picks.cEffort = composerCurrent;
      state.menus["c-effort"]?.setValue(composerCurrent);
    }
  };
  const providerOpts = [
    { value: "xai", label: "Grok · Auth" },
    { value: "openai_codex", label: "OpenAI Codex · ChatGPT Auth" },
    { value: "kimi_work", label: "Kimi Work · Auth" },
    { value: "ollie", label: "OllieChat · API key", status: "disabled", statusLabel: "Indispon\u00edvel" },
    { value: "gemini", label: "Gemini AI Studio · Auth" },
    { value: "qwen", label: "Qwen · API key", status: "disabled", statusLabel: "Indispon\u00edvel" },
    { value: "deepseek", label: "DeepSeek · API key" },
    { value: "opencode_go", label: "OpenCode Go · API key" },
    { value: "accio", label: "Accio · Auth", status: "maintenance", statusLabel: "Em manuten\u00e7\u00e3o" },
  ];
  providerOpts.splice(providerOpts.length - 1, 0, {
    value: "opencode_zen",
    label: "OpenCode Zen Free - keyless",
  });
  const apiOpts = [
    { value: "responses", label: "Responses ★" },
    { value: "chat", label: "Chat" },
  ];
  const modelOpts = () =>
    (state.models.length ? state.models : fallbackModels()).map((m) => ({
      value: m.id,
      label: shortModelLabel(m.name || m.id, m.id),
    }));

  const systemPromptInput = () => $("#system-prompt-input");
  const selectedSystemPromptScope = () => ({
    provider: state.settings?.provider || "xai",
    model: selectedModelId(false) || state.settings?.default_model || "default",
  });
  let systemPromptLoad = 0;
  async function loadSystemPrompt() {
    const input = systemPromptInput();
    if (!input) return;
    const scope = selectedSystemPromptScope();
    const loadId = ++systemPromptLoad;
    const scopeLabel = $("#system-prompt-scope");
    if (scopeLabel) scopeLabel.textContent = `${scope.provider} · ${shortModelLabel(scope.model, scope.model)}`;
    input.disabled = true;
    try {
      const prompt = await GetSystemPrompt(scope.provider, scope.model);
      if (loadId !== systemPromptLoad) return;
      input.value = prompt || "";
      updateSystemPromptCounter();
    } catch (error) {
      console.warn("system prompt", error);
    } finally {
      if (loadId === systemPromptLoad) input.disabled = false;
    }
  }
  function updateSystemPromptCounter() {
    const input = systemPromptInput();
    const count = $("#system-prompt-count");
    if (input && count) count.textContent = `${input.value.length.toLocaleString("pt-BR")} caracteres`;
  }
  async function saveSystemPrompt(clear = false) {
    const input = systemPromptInput();
    if (!input) return;
    const scope = selectedSystemPromptScope();
    const value = clear ? "" : input.value;
    const save = $("#system-prompt-save");
    const clearButton = $("#system-prompt-clear");
    if (save) save.disabled = true;
    if (clearButton) clearButton.disabled = true;
    try {
      await SetSystemPrompt(scope.provider, scope.model, value);
      input.value = value.trim();
      updateSystemPromptCounter();
    } catch (error) {
      console.error("salvar system prompt", error);
      alert(`Não foi possível salvar o system prompt: ${error?.message || error}`);
    } finally {
      if (save) save.disabled = false;
      if (clearButton) clearButton.disabled = false;
    }
  }

  async function switchProvider(v) {
    const statusInfo = providerStatusInfo(v);
    if (statusInfo) {
      showProviderStatusModal(v);
      // Maintenance is advisory for Accio: keep the selection and continue
      // through the normal login/model-loading flow. Disabled providers stay
      // blocked at the selector.
      if (statusInfo.status === "disabled") {
        state.menus["set-provider"]?.setValue(state.settings?.provider || "xai");
        return;
      }
    }
    if (v === "deepseek" || v === "opencode_go") {
      // Chave já salva → conecta direto (sem modal). Modal só quando não há chave.
      const hasKey = v === "deepseek"
        ? !!state.settings?.deepseek_api_key
        : !!state.settings?.opencode_go_api_key;
      if (!hasKey) {
        const res = await showAPIKeyModal(v);
        if (!res) return;
        const patch = { provider: v };
        if (res.key != null) patch[v === "deepseek" ? "deepseek_api_key" : "opencode_go_api_key"] = res.key;
        await saveGlobal(patch);
      } else {
        await saveGlobal({ provider: v });
      }
    } else {
      // One shot: backend resets model+upstream for the provider.
      await saveGlobal({ provider: v });
      if (v === "accio") {
        try {
          const status = await AccioStatus();
          if (!status?.authenticated) {
            await StartAccioLogin();
            pollAccioLogin();
          } else {
            await refreshAccioCredits();
          }
        } catch (e) {
          console.warn("Accio login", e);
        }
      } else {
        state.accioCredits = null;
        paintAccioCreditAvatar();
      }
    }
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
    const api = v === "xai" || v === "openai_codex" ? "responses" : "chat";
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
    await loadSystemPrompt();
  }

  const effortOpts = () => {
    const opts = effortOptionsForModel(selectedModelId(false));
    return opts.length ? opts : fallbackEffortOpts;
  };

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
      syncEffortMenus();
      loadSystemPrompt();
    },
  });

  systemPromptInput()?.addEventListener("input", updateSystemPromptCounter);
  $("#system-prompt-save")?.addEventListener("click", () => saveSystemPrompt());
  $("#system-prompt-clear")?.addEventListener("click", () => saveSystemPrompt(true));
  loadSystemPrompt();

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
    options: () => effortOpts().map((o) => ({ ...o, label: o.value })),
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
  const prefer = state.settings.default_model || state.picks.model || "grok-4.6";
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
      const result = await LogoffKimiAccount(account.id);
      await refreshBootstrap(false);
      setStatus(result?.remote ? "Conta deletada no kimi.com e removida do proxy" : "Conta removida do proxy");
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
  // live-update an open Logs modal
  if (state.logsModal && document.body.contains(state.logsModal)) {
    const list = $("#logs-list", state.logsModal);
    const counter = $("#logs-count", state.logsModal);
    if (list) {
      const div = document.createElement("div");
      div.innerHTML = renderLogRow(state.logs.length - 1, entry);
      list.appendChild(div.firstElementChild);
      list.scrollTop = list.scrollHeight;
    }
    if (counter) counter.textContent = state.logs.length;
  }
}

function renderLogRow(i, log) {
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
  state.logsModal = overlay;

  const rows = state.logs.length
    ? state.logs.map((log, i) => renderLogRow(i, log)).join("")
    : `<div class="log-empty">Nenhum log nesta sessão ainda.</div>`;

  overlay.innerHTML = `
    <div class="sheet sheet-logs">
      <div class="sheet-head">
        <div>
          <h3>Logs da sessão</h3>
          <p><span id="logs-count">${state.logs.length}</span> evento(s) · tempo real · perdido ao fechar o app</p>
        </div>
        <div style="display:flex;gap:8px;align-items:center;">
          <button type="button" class="btn btn-quiet" id="logs-copy-all" ${state.logs.length ? "" : "disabled style=\"opacity:.4\""}>Copiar tudo</button>
          <button type="button" class="btn btn-quiet" id="logs-close">Fechar</button>
        </div>
      </div>
      <div class="logs-list" id="logs-list">${rows}</div>
    </div>`;

  document.body.appendChild(overlay);

  const list = $("#logs-list", overlay);
  if (list) list.scrollTop = list.scrollHeight;

  $("#logs-close", overlay).onclick = () => {
    state.logsModal = null;
    overlay.remove();
  };
  overlay.addEventListener("click", (e) => {
    if (e.target === overlay) {
      state.logsModal = null;
      overlay.remove();
    }
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
    const how = pNow === "accio" || pNow === "accio-work" || pNow === "phoenix"
      ? "Clique em <b>Login Accio</b> para autenticar; depois o proxy poderá alternar entre as contas automaticamente."
      : pNow.startsWith("kimi")
      ? "Clique em <b>+ Conta Kimi</b> (Desktop / JWT / sk-kimi)."
      : "Clique em <b>+ Conta Grok</b> para OAuth xAI.";
    list.innerHTML = `<div class="account empty-hint">Nenhuma conta neste provedor.<br/>${how}</div>`;
  } else {
    // Keep healthy accounts visible at the top of a large pool. Previously the
    // store order put old exhausted rows immediately below the active account,
    // making newly-created usable accounts look like they were never imported.
    const orderedAccounts = [...state.accounts].sort((a, b) => {
      const rank = (item) => {
        if (item.active) return 0;
        if (!item.exhausted && !item.chat_denied && !item.needs_login) return 1;
        if (!item.exhausted) return 2;
        return 3;
      };
      const diff = rank(a) - rank(b);
      if (diff) return diff;
      return String(b.created_at || b.updated_at || "").localeCompare(String(a.created_at || a.updated_at || ""));
    });
    orderedAccounts.forEach((a) => {
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
              ${a.expired && a.has_refresh === false ? `<span class="badge badge-warn" title="Sem refresh token — faça login de novo.">login expirado</span>` : ""}
              ${a.expired && a.has_refresh !== false ? `<span class="badge" title="Access token expirado; renovação automática via refresh.">renova auto</span>` : ""}
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
  state.picks.model = state.settings.default_model || state.picks.model || "grok-4.6";
  if (!state.picks.cEffort) state.picks.cEffort = state.picks.effort;
  if (!state.picks.cApi) state.picks.cApi = state.picks.api;
  if (!state.picks.cModel) state.picks.cModel = state.picks.model;

  state.menus["set-effort"]?.setValue(state.picks.effort);
  state.menus["set-api"]?.setValue(state.picks.api);
  fillModels();
  state.menus["c-effort"]?.setValue(state.picks.cEffort);
  state.menus["c-api"]?.setValue(state.picks.cApi);
  state.menus["c-model"]?.setValue(state.picks.cModel);
  if (activeId) state.menus["c-account"]?.setValue(activeId);

  paintStatus();
  paintSend();
  paintMessages();
  paintAccioCreditAvatar();
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

function cachedMessageMarkup(message, kind) {
  const content = String(message.content || "");
  const key = `${kind}:${message.isError ? "error" : "text"}:${content}`;
  const hit = messageMarkupCache.get(message);
  if (hit?.key === key) return hit.html;

  let html;
  if (kind === "user") {
    html = content.includes("`") || content.includes("**") || content.includes("\n")
      ? renderMarkdown(content)
      : `<p>${escapeHtml(content).replaceAll("\n", "<br>")}</p>`;
  } else if (message.isError || looksLikeHTML(content)) {
    html = `<p class="err">${escapeHtml(safeErrorText(content))}</p>`;
  } else {
    html = renderMarkdown(content);
  }
  messageMarkupCache.set(message, { key, html });
  return html;
}

function renderStreamingText(content) {
  // Markdown parsing + DOMPurify on every token is the main UI bottleneck for
  // long answers. Escape plain text during the stream, then render markdown
  // once when the done event arrives.
  return `<p>${escapeHtml(String(content || "")).replaceAll("\n", "<br>")}</p>`;
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
        const body = cachedMessageMarkup(m, "user");
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
      // Keep the live path cheap; sanitize/render markdown after completion.
      const answerBody = m.streaming
        ? renderStreamingText(m.content)
        : cachedMessageMarkup(m, "assistant");
      const answer = answerBody + cursor;
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

function providerStatusInfo(provider) {
  const p = (provider || "").toLowerCase();
  if (p === "ollie" || p === "olliechat") {
    return {
      name: "OllieChat",
      status: "disabled",
      badge: "PROVEDOR DESATIVADO",
      label: "Indispon\u00edvel",
      symbol: "O",
      message: "Sinto muito, mas infelizmente esse provedor se encontra desabilitado por enquanto. Tente outro.",
      detail: "OllieChat est\u00e1 temporariamente fora da rota do Grok Desktop.",
    };
  }
  if (p === "qwen" || p === "qwenbridge") {
    return {
      name: "Qwen",
      status: "disabled",
      badge: "PROVEDOR DESATIVADO",
      label: "Indispon\u00edvel",
      symbol: "Q",
      message: "Sinto muito, mas infelizmente esse provedor se encontra desabilitado por enquanto. Tente outro.",
      detail: "Qwen est\u00e1 temporariamente fora da rota do Grok Desktop.",
    };
  }
  if (p === "accio" || p === "accio-work" || p === "phoenix") {
    return {
      name: "Accio",
      status: "maintenance",
      badge: "EM MANUTEN\u00c7\u00c3O",
      label: "Em manuten\u00e7\u00e3o",
      symbol: "A",
      message: "Sinto muito, mas infelizmente esse provedor se encontra em manuten\u00e7\u00e3o por enquanto. O uso continua liberado, mas podem ocorrer problemas e erros.",
      detail: "A rota Accio est\u00e1 recebendo ajustes. Voc\u00ea ainda pode usar o provedor; se uma request falhar, tente novamente ou confira o erro retornado.",
    };
  }
  return null;
}

function showProviderStatusModal(provider) {
  const info = providerStatusInfo(provider);
  if (!info) return;
  closeOverlay();
  const overlay = document.createElement("div");
  overlay.className = "overlay overlay-glass provider-status-overlay";
  overlay.innerHTML = `
    <div class="sheet sheet-provider-status provider-status-${info.status}" role="dialog" aria-modal="true" aria-labelledby="provider-status-title">
      <button type="button" class="provider-status-close" id="provider-status-close" aria-label="Fechar">&times;</button>
      <div class="provider-status-orb"><span>${escapeHtml(info.symbol)}</span></div>
      <div class="provider-status-kicker">${escapeHtml(info.badge)}</div>
      <h3 id="provider-status-title">${escapeHtml(info.name)}</h3>
      <p class="provider-status-message">${escapeHtml(info.message)}</p>
      <div class="provider-status-note">
        <span class="provider-status-note-icon">i</span>
        <span>${escapeHtml(info.detail)}</span>
      </div>
      <div class="sheet-actions provider-status-actions">
        <button type="button" class="btn btn-solid" id="provider-status-ok">Entendi</button>
      </div>
    </div>
  `;
  document.body.appendChild(overlay);
  const close = () => overlay.remove();
  $("#provider-status-close", overlay).onclick = close;
  $("#provider-status-ok", overlay).onclick = close;
  overlay.addEventListener("click", (e) => {
    if (e.target === overlay) close();
  });
}

function providerAuthMode(p) {
  p = (p || state.settings?.provider || "xai").toLowerCase();
  if (p === "xai" || p === "grok" || p === "openai_codex" || p === "codex" || p === "openai-codex" || p === "chatgpt" || p === "kimi_work" || p === "kimi" || p === "kimi-work" || p === "accio" || p === "accio-work" || p === "phoenix" || isGeminiProvider(p)) return "auth";
  return "api_key";
}

function isGeminiProvider(provider = state.settings?.provider) {
  const p = (provider || "").toLowerCase();
  return p === "gemini" || p === "google" || p === "vertex";
}

function updateProviderChrome() {
  const p = (state.settings?.provider || "xai").toLowerCase();
  const model = state.settings?.default_model || state.picks?.model || "—";
  const mode = providerAuthMode(p);
  const statusInfo = providerStatusInfo(p);
  const el = $("#provider-label");
  if (el) {
    if (statusInfo) {
      el.textContent = `${statusInfo.name} · ${statusInfo.label}`;
    } else if (p === "qwen" || p === "qwenbridge") {
      el.textContent = `Qwen · API key · ${shortModelLabel(model, model)}`;
    } else if (p === "deepseek") {
      el.textContent = `DeepSeek · API key ${state.settings?.deepseek_api_key ? "🔒" : "⚠"} · ${shortModelLabel(model, model)}`;
    } else if (["openai_codex", "codex", "openai-codex", "chatgpt"].includes(p)) {
      el.textContent = `OpenAI Codex · ChatGPT Auth · ${shortModelLabel(model, model)}`;
    } else if (p === "opencode_go" || p === "opencode-go") {
      el.textContent = `OpenCode Go · API key ${state.settings?.opencode_go_api_key ? "🔒" : "⚠"} · ${shortModelLabel(model, model)}`;
    } else if (p === "accio" || p === "accio-work" || p === "phoenix") {
      el.textContent = `Accio · Auth · ${shortModelLabel(model, model)}`;
    } else if (["opencode_zen", "opencode-zen", "opencode", "zen", "zen-free"].includes(p)) {
      el.textContent = `OpenCode Zen Free - keyless - ${shortModelLabel(model, model)}`;
    } else if (p === "ollie" || p === "olliechat") {
      el.textContent = `Ollie · API key · ${shortModelLabel(model, model)}`;
    } else if (isGeminiProvider(p)) {
      el.textContent = `Gemini AI Studio · Auth · ${shortModelLabel(model, model)}`;
    } else if (p === "kimi_work" || p === "kimi" || p === "kimi-work") {
      el.textContent = `Kimi Work · Auth · ${shortModelLabel(model, model)}`;
    } else {
      el.textContent = `Grok · Auth · ${shortModelLabel(model, model)}`;
    }
  }
  const modeEl = $("#provider-mode");
  if (modeEl) {
    modeEl.innerHTML = statusInfo
      ? `<span class="mode-pill mode-status-${statusInfo.status}">${escapeHtml(statusInfo.badge)}</span>`
      : mode === "auth"
        ? `<span class="mode-pill mode-auth">Auth · multi-conta</span>`
        : `<span class="mode-pill mode-key">${["opencode_zen", "opencode-zen", "opencode", "zen", "zen-free"].includes(p) ? "Keyless" : "API key"} - sem pool</span>`;
  }
  const addBtn = $("#btn-add");
  const accBtn = $("#btn-accounts");
  const disabled = statusInfo?.status === "disabled";
  if (addBtn) {
    // Accio maintenance is advisory: account login/management remains usable.
    // Only providers explicitly marked disabled hide these controls.
    addBtn.style.display = disabled ? "none" : mode === "auth" ? "" : "none";
    addBtn.textContent = p.startsWith("kimi") ? "+ Conta Kimi" : isGeminiProvider(p) ? "+ Conta Gemini" : (p === "accio" || p === "accio-work" || p === "phoenix" ? "Login Accio" : (["openai_codex", "codex", "openai-codex", "chatgpt"].includes(p) ? "+ Conta ChatGPT" : "+ Conta Grok"));
  }
  if (accBtn) {
    accBtn.style.display = disabled ? "none" : mode === "auth" ? "" : "none";
  }
  // Pool batch creation only exists for the Grok/xAI provider (grok-register).
  const poolBtn = $("#btn-pool");
  if (poolBtn) {
    const isGrok = p === "xai" || p === "grok" || p === "";
    poolBtn.style.display = !disabled && isGrok ? "" : "none";
  }
  const hint = document.querySelector(".tool-hint");
  if (hint) {
    if (statusInfo) {
      hint.textContent = statusInfo.label;
      hint.title = statusInfo.message;
    } else if (p === "qwen" || p === "qwenbridge") {
      hint.textContent = "QwenBridge";
      hint.title = "QwenBridge local · OpenAI-compatible (chat/completions)";
    } else if (p === "deepseek") {
      hint.textContent = "DeepSeek";
      hint.title = "DeepSeek API oficial · chat/completions · chave criptografada";
    } else if (["openai_codex", "codex", "openai-codex", "chatgpt"].includes(p)) {
      hint.textContent = "Codex Responses";
      hint.title = "Backend oficial do Codex usando a assinatura ChatGPT da conta";
    } else if (p === "opencode_go" || p === "opencode-go") {
      hint.textContent = "OpenCode Go";
      hint.title = "OpenCode direto · chave de opencode.ai/auth · chat/completions";
    } else if (p === "accio" || p === "accio-work" || p === "phoenix") {
      hint.textContent = "Accio";
      hint.title = "Accio/Phoenix · login no Accio · chat/completions";
    } else if (["opencode_zen", "opencode-zen", "opencode", "zen", "zen-free"].includes(p)) {
      hint.textContent = "OpenCode Zen Free";
      hint.title = "OpenCode Zen direto - sem opencode serve/terminal";
    } else if (p === "ollie" || p === "olliechat") {
      hint.textContent = "OllieChat";
      hint.title = "Upstream OllieChat (sem chave)";
    } else if (isGeminiProvider(p)) {
      hint.textContent = "Gemini AI Studio";
      hint.title = "Sessões Google AI Studio · multi-conta · rotação local";
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
  if (isGeminiProvider(p)) {
    showGeminiLoginModal();
    return;
  }
  if (["openai_codex", "codex", "openai-codex", "chatgpt"].includes(p)) {
    startCodexLogin();
    return;
  }
  if (p === "accio" || p === "accio-work" || p === "phoenix") {
    StartAccioLogin().catch((e) => alert("Falha ao iniciar login Accio: " + e));
    return;
  }
  if (p === "kimi_work" || p === "kimi" || p === "kimi-work") {
    showAddKimiChooser();
    return;
  }
  if (p === "deepseek" || p === "opencode_go" || p === "opencode-go") {
    const isOpenCodeGo = p === "opencode_go" || p === "opencode-go";
    showAPIKeyModal(isOpenCodeGo ? "opencode_go" : "deepseek").then(async (res) => {
      if (!res) return;
      const patch = { provider: isOpenCodeGo ? "opencode_go" : "deepseek" };
      if (res.key != null) patch[isOpenCodeGo ? "opencode_go_api_key" : "deepseek_api_key"] = res.key;
      await saveGlobal(patch);
      await refreshBootstrap(false);
      const name = isOpenCodeGo ? "OpenCode Go" : "DeepSeek";
      setStatus(res.key ? `${name} conectado — chave criptografada e salva` : `${name} — chave mantida`);
    });
    return;
  }
  if (p === "ollie" || p === "qwen" || p === "qwenbridge" || ["opencode_zen", "opencode-zen", "opencode", "zen", "zen-free"].includes(p)) {
    closeOverlay();
    const overlay = document.createElement("div");
    overlay.className = "overlay overlay-glass";
    overlay.innerHTML = `
      <div class="sheet sheet-choose">
        <h3>Provedor API key</h3>
        <p>Este provedor não usa pool de contas de sessão. Configure a credencial em <b>Global</b>.</p>
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
          <span>Cria conta com mailtm + Turnstile no Chrome real, autoriza o device OAuth e valida no grok-4.6.</span>
        </button>
        <button type="button" class="choose-card" id="m-manual">
          <strong>Manual</strong>
          <span>Device login clássico — você confirma o código na xAI com uma conta que já existe.</span>
        </button>
      </div>
      <label class="auto-toggle">
        <input type="checkbox" id="m-auto-quota" />
        Manter pool de contas — quando a cota acabar, criar até ter pelo menos
        <input type="number" id="m-auto-min" min="1" max="10" step="1" value="3" class="auto-min-input" />
        contas válidas
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
  GetAutoCreateMinAccounts()
    .then((v) => {
      const el = $("#m-auto-min", overlay);
      if (el && v > 0) el.value = v;
    })
    .catch(() => {});
  $("#m-auto-min", overlay).onchange = (e) => {
    const n = parseInt(e.target.value, 10);
    if (Number.isFinite(n)) SetAutoCreateMinAccounts(n).catch(() => {});
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

// Modal do botão "Criar em lote" (só visível com o provedor Grok selecionado):
// escolhe quantas contas novas a automação deve criar para o pool.
function showPoolBatchModal() {
  closeOverlay();
  const overlay = document.createElement("div");
  overlay.className = "overlay overlay-glass";
  const usableNow = (state.accounts || []).filter(
    (a) => (a.provider || "xai") === "xai" && !a.exhausted && !a.chat_denied && !a.needs_login
  ).length;
  overlay.innerHTML = `
    <div class="sheet sheet-choose" style="width:min(460px,92vw)">
      <h3>Criar contas Grok em lote</h3>
      <p><span class="mode-pill mode-auth">Auth</span> Automação grok-register — cada conta leva ~2-3 min (Chrome + Turnstile + OAuth), criadas em sequência.</p>
      <label class="field">
        <span>Quantas contas criar agora (1-10)</span>
        <input id="pool-count" type="number" class="input" min="1" max="10" step="1" value="3" />
      </label>
      <div class="empty-hint" id="pool-status" style="margin-top:8px">
        Pool atual: ${usableNow} conta(s) utilizável(is).
      </div>
      <div class="sheet-actions">
        <button class="btn btn-quiet" id="pool-cancel">Cancelar</button>
        <button class="btn btn-solid" id="pool-start">Criar contas</button>
      </div>
    </div>`;
  document.body.appendChild(overlay);
  const close = () => overlay.remove();
  $("#pool-cancel", overlay).onclick = close;
  overlay.addEventListener("click", (e) => {
    if (e.target === overlay) close();
  });
  const startBtn = $("#pool-start", overlay);
  const countInput = $("#pool-count", overlay);
  const statusEl = $("#pool-status", overlay);
  startBtn.onclick = async () => {
    let n = parseInt(countInput.value, 10);
    if (!Number.isFinite(n) || n < 1) n = 1;
    if (n > 10) n = 10;
    startBtn.disabled = true;
    statusEl.textContent = `Iniciando criação de ${n} conta(s)…`;
    try {
      await StartSignupBatch(n);
      const st = $("#status-text");
      if (st) st.innerHTML = `Lote Grok · criando <strong>${n}</strong> conta(s)…`;
      close();
    } catch (err) {
      statusEl.textContent = "Erro: " + (err?.message || err);
      startBtn.disabled = false;
    }
  };
}

async function showGeminiLoginModal(accountID = "") {
  closeOverlay();
  const overlay = document.createElement("div");
  overlay.className = "overlay overlay-glass";
  const relogin = !!accountID;
  overlay.innerHTML = `
    <div class="sheet sheet-choose" style="width:min(500px,92vw)">
      <h3>${relogin ? "Entrar novamente no Gemini" : "Adicionar conta Gemini"}</h3>
      <p>O Grok Desktop abrirá um Chrome dedicado. Entre no Google e aguarde o AI Studio carregar; depois volte aqui para concluir.</p>
      ${relogin ? "" : `
        <label class="field"><span>Nome da conta (opcional)</span><input id="gemini-login-label" class="input" placeholder="Conta pessoal" /></label>
        <label class="field"><span>E-mail (opcional)</span><input id="gemini-login-email" class="input" placeholder="voce@gmail.com" /></label>`}
      <div id="gemini-login-status" class="empty-hint" style="margin-top:12px">Pronto para abrir o navegador.</div>
      <div class="sheet-actions">
        <button class="btn btn-quiet" id="gemini-login-cancel">Cancelar</button>
        <button class="btn btn-solid" id="gemini-login-start">Abrir Chrome</button>
        <button class="btn btn-solid" id="gemini-login-complete" style="display:none">Concluir login</button>
      </div>
    </div>`;
  document.body.appendChild(overlay);
  let pendingID = accountID;
  const status = $("#gemini-login-status", overlay);
  const start = $("#gemini-login-start", overlay);
  const complete = $("#gemini-login-complete", overlay);
  const cancel = $("#gemini-login-cancel", overlay);
  start.onclick = async () => {
    start.disabled = true;
    status.textContent = "Abrindo Chrome dedicado…";
    try {
      const result = await StartGeminiLogin(
        accountID,
        $("#gemini-login-label", overlay)?.value?.trim() || "",
        $("#gemini-login-email", overlay)?.value?.trim() || "",
      );
      pendingID = `gemini:${result.profile_id}`;
      status.textContent = "Chrome aberto. Faça login no Google/AI Studio e clique em Concluir login.";
      start.style.display = "none";
      complete.style.display = "";
    } catch (error) {
      status.textContent = `Não foi possível abrir o Chrome: ${error?.message || error}`;
      start.disabled = false;
    }
  };
  complete.onclick = async () => {
    complete.disabled = true;
    status.textContent = "Validando a sessão do AI Studio…";
    try {
      const result = await CompleteGeminiLogin(pendingID);
      if (result?.status === "warning") {
        status.textContent = `Login ainda não detectado: ${result.message || "termine o login no Chrome"}`;
        complete.disabled = false;
        return;
      }
      status.textContent = `Conta conectada${result?.email ? `: ${result.email}` : ""}.`;
      await refreshBootstrap(false);
      overlay.remove();
      openAccountsModal();
    } catch (error) {
      status.textContent = `Falha ao validar: ${error?.message || error}`;
      complete.disabled = false;
    }
  };
  cancel.onclick = async () => {
    if (pendingID) {
      try { await CancelGeminiLogin(pendingID); } catch (_) {}
    }
    overlay.remove();
  };
  overlay.addEventListener("click", (event) => {
    if (event.target === overlay) cancel.click();
  });
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

/**
 * Modal de API key do DeepSeek — aparece ao selecionar o provedor DeepSeek.
 * A chave é enviada ao backend, que a criptografa (DPAPI/Windows) antes de
 * persistir. Retorna { key } com a chave nova, { key: null } para manter a
 * salva, ou null se o usuário cancelou.
 */
function showAPIKeyModal(provider) {
  return new Promise((resolve) => {
    const isOpenCodeGo = provider === "opencode_go";
    const providerName = isOpenCodeGo ? "OpenCode Go" : "DeepSeek";
    const hasKey = isOpenCodeGo ? !!state.settings?.opencode_go_api_key : !!state.settings?.deepseek_api_key;
    const overlay = document.createElement("div");
    overlay.className = "overlay overlay-glass";
    overlay.innerHTML = `
      <div class="sheet sheet-deepseek">
        <div class="ds-hero">
          <div class="ds-logo"><span>DS</span></div>
          <h3>Conectar ao ${providerName}</h3>
          <p>${isOpenCodeGo ? "API key do OpenCode Go · chat/completions" : "API oficial DeepSeek · chat/completions"}</p>
        </div>
        <div class="ds-body">
          <div class="field" style="margin-bottom:14px">
            <span class="field-label">API Key</span>
            <div class="ds-key-wrap">
              <span class="ds-lock">🔒</span>
              <input id="ds-key" type="password" spellcheck="false" autocomplete="off"
                placeholder="${hasKey ? "•••••••••• (chave salva — digite para trocar)" : "sk-…"}"
                style="flex:1;background:transparent;border:none;outline:none;color:#fff;font-family:ui-monospace,monospace;font-size:13px;padding:0 6px" />
              <button type="button" id="ds-toggle" class="ds-eye" title="Mostrar/ocultar">👁</button>
            </div>
            ${hasKey ? `<p class="ds-saved">✓ Chave salva no cofre do Windows (DPAPI) — deixe vazio para manter.</p>` : ""}
          </div>
          <p class="ds-sec">
            🔒 Sua chave é criptografada com <b>DPAPI do Windows</b> (CryptProtectData)
            antes de ir para o disco — nunca fica em texto puro e só o seu usuário consegue ler.
          </p>
          <p id="ds-error" class="ds-error" style="display:none"></p>
          <div class="ds-actions">
            <a href="#" id="ds-link">${isOpenCodeGo ? "Criar chave em opencode.ai/auth" : "Criar chave no platform.deepseek.com"}</a>
            <div class="sheet-actions" style="margin-top:4px">
              <button class="btn btn-solid btn-ds" id="ds-save" type="button">Conectar</button>
              <button class="btn btn-quiet" id="ds-cancel" type="button">Cancelar</button>
            </div>
          </div>
        </div>
      </div>`;
    document.body.appendChild(overlay);

    const keyInput = $("#ds-key", overlay);
    const errEl = $("#ds-error", overlay);
    const fail = (msg) => {
      errEl.textContent = msg;
      errEl.style.display = "";
      keyInput?.focus();
    };

    $("#ds-toggle", overlay).onclick = () => {
      if (!keyInput) return;
      const hidden = keyInput.type === "password";
      keyInput.type = hidden ? "text" : "password";
      $("#ds-toggle", overlay).textContent = hidden ? "🙈" : "👁";
    };
    $("#ds-link", overlay).onclick = (e) => {
      e.preventDefault();
      try {
        OpenExternal(isOpenCodeGo ? "https://opencode.ai/auth" : "https://platform.deepseek.com/api_keys");
      } catch (_) {}
    };
    const cancel = () => {
      overlay.remove();
      resolve(null);
    };
    $("#ds-cancel", overlay).onclick = cancel;
    overlay.addEventListener("click", (e) => {
      if (e.target === overlay) cancel();
    });
    keyInput?.addEventListener("keydown", (e) => {
      if (e.key === "Enter") $("#ds-save", overlay)?.click();
    });
    $("#ds-save", overlay).onclick = async () => {
      const raw = (keyInput?.value || "").trim();
      if (!raw && !hasKey) {
        fail(`Cole a API key do ${providerName} para conectar.`);
        return;
      }
      overlay.remove();
      resolve({ key: raw || null });
    };
    keyInput?.focus();
  });
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
      <button type="button" class="btn btn-quiet" id="m-test-creds" style="width:100%;font-size:13px;margin-bottom:8px;">Testar auto-login</button>
      <p id="m-creds-status" style="font-size:11px;opacity:.5;margin:0 0 8px;text-align:center;"></p>
      <div class="sheet-actions">
        <button class="btn btn-quiet" id="m-cancel">Fechar</button>
      </div>
    </div>`;
  document.body.appendChild(overlay);
  const close = () => {
    overlay.remove();
    openAccountsModal();
  };
  $("#m-cancel", overlay).onclick = close;
  overlay.addEventListener("click", (e) => {
    if (e.target === overlay) close();
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
  $("#m-test-creds", overlay).onclick = async () => {
    const statusEl = $("#m-creds-status", overlay);
    const btn = $("#m-test-creds", overlay);
    if (btn) btn.disabled = true;
    if (statusEl) {
      statusEl.textContent = "Testando relogin automático...";
      statusEl.style.color = "#9ecbff";
      statusEl.style.opacity = "0.9";
    }
    try {
      const result = await TestKimiGoogleCredentials(account.id);
      if (statusEl) {
        statusEl.textContent = `Auto-login OK (${result?.mode || "sessão renovada"})`;
        statusEl.style.color = "#7ee787";
      }
      await refreshBootstrap(false);
    } catch (e) {
      if (statusEl) {
        statusEl.textContent = "Auto-login falhou: " + e;
        statusEl.style.color = "#ff7b72";
      }
    } finally {
      if (btn) btn.disabled = false;
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
    const geminiUI = isGeminiProvider(p);
    const accioUI = p === "accio" || p === "accio-work" || p === "phoenix";
    const codexUI = ["openai_codex", "codex", "openai-codex", "chatgpt"].includes(p);
    const title = geminiUI ? "Contas Gemini AI Studio" : kimiUI ? "Contas Kimi Work" : accioUI ? "Contas Accio" : codexUI ? "Contas ChatGPT Codex" : "Contas Grok";

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
            if (geminiUI && a.is_valid) statusBadges.push(`<span class="badge badge-ok">sessão válida</span>`);
            if (geminiUI && !a.is_valid) statusBadges.push(`<span class="badge badge-danger">login necessário</span>`);
            if (geminiUI && a.default) statusBadges.push(`<span class="badge badge-live">padrão</span>`);
            if (kimiUI && a.has_web_session) statusBadges.push(`<span class="badge badge-ok">sessão web</span>`);
            if (accioUI) statusBadges.push(`<span class="badge badge-ok">pool Accio</span>`);
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
                ${a.active ? `<button type="button" class="btn btn-xs btn-disabled" disabled>${geminiUI ? "Padrão" : "Em uso"}</button>` : `<button type="button" class="btn btn-xs btn-solid" data-act="use">${geminiUI ? "Tornar padrão" : "Usar"}</button>`}
                <button type="button" class="btn btn-xs btn-quiet" data-act="rename">Renomear</button>
                ${geminiUI ? `<button type="button" class="btn btn-xs btn-quiet" data-act="validate">Validar</button><button type="button" class="btn btn-xs btn-quiet" data-act="relogin">Login</button>` : ""}
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
          <p style="font-size:12px;opacity:.7;margin:0"><span class="mode-pill mode-auth">${geminiUI ? "AI Studio" : "Auth"}</span> pool · ${accounts.length}${kimiUI ? "/3" : ""} conta(s) · ${geminiUI ? "Chrome dedicado + rotação local" : (codexUI ? "refresh OAuth automático" : "rotação + re-login automático")}</p>
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
          }
        } else if (act === "logoff") {
          const a = accounts.find((x) => x.id === id);
          if (a) {
            overlay.remove();
            await confirmAndLogoffKimi(a);
            openAccountsModal();
          }
        } else if (act === "validate") {
          btn.disabled = true;
          try {
            const result = await ValidateGeminiAccount(id);
            alert(result?.status === "ok" ? "Conta Gemini validada." : `Validação: ${result?.message || "sessão inválida"}`);
            await refreshBootstrap(false);
            openAccountsModal();
          } catch (error) {
            alert(`Falha ao validar conta Gemini: ${error?.message || error}`);
            btn.disabled = false;
          }
        } else if (act === "relogin") {
          overlay.remove();
          showGeminiLoginModal(id);
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

async function startCodexLogin() {
  try {
    const st = await StartCodexLogin();
    state.device = st;
    showDeviceModal(st, {
      title: "Entrar com ChatGPT",
      providerName: "OpenAI",
		instruction: "Entre na sua conta ChatGPT no navegador. Ao concluir, esta tela fecha automaticamente.",
		browserOnly: true,
    });
    if (st.verification_url) {
      try {
        await OpenExternal(st.verification_url);
      } catch (_) {}
    }
  } catch (e) {
    alert("Falha ao iniciar login Codex: " + e);
  }
}

function showDeviceModal(st, extra = {}) {
  closeOverlay();
  const overlay = document.createElement("div");
  overlay.className = "overlay overlay-glass";
  const emailHint = extra.email
    ? `<p class="hint-email">Use a conta <strong>${escapeHtml(extra.email)}</strong>${extra.password ? ` · senha <code id="m-pass">${escapeHtml(extra.password)}</code>` : ""}</p>`
    : "";
	const codeBlock = extra.browserOnly ? "" : `<div class="code">${escapeHtml(st.user_code)}</div>`;
	const copyButton = extra.browserOnly ? "" : `<button class="btn btn-quiet" id="m-copy">Copiar código</button>`;
  overlay.innerHTML = `
    <div class="sheet">
      <h3>${extra.title || "Adicionar conta"}</h3>
      <p>${escapeHtml(extra.instruction || `Confirme o código na página da ${extra.providerName || "xAI"}. O app completa sozinho.`)}</p>
      ${emailHint}
		${codeBlock}
      <div class="sheet-actions">
        <button class="btn btn-solid" id="m-open">Abrir login</button>
			${copyButton}
        ${extra.password ? `<button class="btn btn-quiet" id="m-copy-pass">Copiar senha</button>` : ""}
        <button class="btn btn-quiet" id="m-cancel">Cancelar</button>
      </div>
      <div class="hint">${escapeHtml(st.verification_url || "")}</div>
      <div class="signup-log" id="m-log" style="display:none"></div>
    </div>
  `;
  document.body.appendChild(overlay);
  $("#m-open", overlay).onclick = () => OpenExternal(st.verification_url);
	const copyCode = $("#m-copy", overlay);
	if (copyCode) {
		copyCode.onclick = async () => {
			await navigator.clipboard.writeText(st.user_code);
		};
	}
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
      <p>grok-register + OAuth Device Flow + teste real no Grok 4.6. Pode levar alguns minutos.</p>
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
  // Provedores API key (DeepSeek, Qwen, Ollie, Gemini) não usam pool de contas —
  // só provedores de sessão (xAI/Kimi/Accio) exigem conta selecionada.
  const pNow = (state.settings?.provider || "xai").toLowerCase();
  const statusInfo = providerStatusInfo(pNow);
  if (statusInfo?.status === "disabled") {
    showProviderStatusModal(pNow);
    return;
  }
  if (statusInfo?.status === "maintenance") showProviderStatusModal(pNow);
  if (providerAuthMode(pNow) === "auth" && !activeAccount()) {
    alert("Adicione e selecione uma conta primeiro.");
    return;
  }

  const model =
    state.menus["c-model"]?.getValue?.() || state.picks.cModel || state.settings.default_model;
  const effort =
    state.menus["c-effort"]?.getValue?.() || state.picks.cEffort || state.settings.reasoning_effort;
  const isKimi =
    pNow === "kimi_work" || pNow === "kimi" || pNow === "kimi-work";
	const isCodex = pNow === "openai_codex" || pNow === "codex" || pNow === "openai-codex";
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
		reasoningItems: [],
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
			.map((m) => ({
				role: m.role,
				content: m.content,
				...(m.reasoningItems?.length ? { reasoning_items: m.reasoningItems } : {}),
			}));
    // last user already included; drop incomplete assistant
    if (payload.messages.at(-1)?.role === "assistant") payload.messages.pop();
	} else if (state.lastResponseId && !isCodex) {
    payload.last_response_id = state.lastResponseId;
    payload.messages = [{ role: "user", content: text }];
  } else {
    // first responses turn — can send conversation so far as messages
    payload.messages = state.messages
      .filter((m) => m.role === "user" || (m.role === "assistant" && m.content && !m.streaming))
			.map((m) => ({
				role: m.role,
				content: m.content,
				...(m.reasoningItems?.length ? { reasoning_items: m.reasoningItems } : {}),
			}));
    if (payload.messages.at(-1)?.role === "assistant") payload.messages.pop();
  }

  try {
    addLog("chat-send", "Enviando", {
      model,
      effort,
      api_mode: apiMode,
      messages: payload.messages.length,
      streaming: true,
    });
    await SendChat(payload);
  } catch (e) {
    state.streaming = false;
    const last = state.messages.at(-1);
    addLog("chat-send", "SendChat rejeitou", { error: String(e) });
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

  // Surface chat stream activity in the Logs modal (throttled) so the user
  // sees the request progressing even when the bubble render is stuck.
  logChatEventThrottled(ev);

	if (ev.type === "reasoning_item" && ev.payload) {
		last.reasoningItems = last.reasoningItems || [];
		if (!last.reasoningItems.some((item) => item.id && item.id === ev.payload.id)) {
			last.reasoningItems.push(ev.payload);
		}
	} else if (ev.type === "thinking") {
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
    if (!last.content && !last.thinking && !last.isError) {
      addLog("chat-event", "DONE sem conteúdo — resposta vazia do upstream", {
        model: ev.model,
        tools: (last.tools || []).length,
      });
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

// logChatEventThrottled mirrors chat stream events into the Logs modal at a
// low rate (control events always; content/thinking at most once per second)
// so a stuck stream is visible instead of silent.
function logChatEventThrottled(ev) {
  const now = Date.now();
  if (ev.type === "content" || ev.type === "thinking") {
    if (now - (state.lastChatEventLogAt || 0) < 1000) return;
  }
  state.lastChatEventLogAt = now;
  const preview =
    ev.type === "error"
      ? String(ev.error || "").slice(0, 200)
      : ev.type === "usage"
        ? `${ev.usage?.prompt_tokens || 0} in / ${ev.usage?.completion_tokens || 0} out`
        : ev.type === "content" || ev.type === "thinking"
          ? String(ev.text || "").slice(0, 60)
          : "";
  addLog("chat-event", `chat:${ev.type}${preview ? " · " + preview : ""}`);
}

async function pollAccioLogin() {
  if (state.accioPolling) return;
  state.accioPolling = true;
  const deadline = Date.now() + 120000;
  const tick = async () => {
    if (!state.accioPolling) return;
    if (Date.now() > deadline) {
      state.accioPolling = false;
      const st = $("#status-text");
      if (st) st.textContent = "Login Accio: tempo esgotado — tente novamente";
      return;
    }
    try {
      const status = await AccioStatus();
      if (status?.authenticated) {
        state.accioPolling = false;
        await refreshBootstrap(true);
        await refreshAccioCredits();
        return;
      }
    } catch (_) {}
    setTimeout(tick, 2000);
  };
  setTimeout(tick, 1500);
}

async function refreshAccioCredits() {
  const p = (state.settings?.provider || "xai").toLowerCase();
  if (p !== "accio" && p !== "accio-work" && p !== "phoenix") {
    state.accioCredits = null;
    return;
  }
  try {
    const credits = await AccioCredits();
    state.accioCredits = credits;
  } catch (e) {
    state.accioCredits = null;
    console.warn("Accio credits", e);
  }
  paintAccioCreditAvatar();
}

function paintAccioCreditAvatar() {
  const host = $("#accio-credit-avatar");
  if (!host) return;
  const p = (state.settings?.provider || "xai").toLowerCase();
  const isAccio = p === "accio" || p === "accio-work" || p === "phoenix";
  if (!isAccio) {
    host.style.display = "none";
    return;
  }
  host.style.display = "";
  const c = state.accioCredits || {};
  const total = Number(c.total || 0);
  const remaining = Number(c.remaining || 0);
  const used = Number(c.used || 0);
  const pct = total > 0 ? Math.min(100, Math.round((used / total) * 100)) : 0;
  const remainingPct = total > 0 ? 100 - pct : 0;
  const status = state.accioCredits == null ? "loading" : (total > 0 ? "ok" : "none");
  const statusLabel = c.type || "Accio";
  const initials = "A";
  host.innerHTML = `
    <div class="accio-ring accio-ring--${status}" title="${escapeHtml(statusLabel)} · ${remaining}/${total} créditos restantes">
      <svg viewBox="0 0 36 36" class="accio-ring-svg">
        <circle class="accio-ring-bg" cx="18" cy="18" r="16"></circle>
        <circle class="accio-ring-fg" cx="18" cy="18" r="16"
          stroke-dasharray="${2 * Math.PI * 16}"
          stroke-dashoffset="${2 * Math.PI * 16 * (1 - remainingPct / 100)}"></circle>
      </svg>
      <span class="accio-ring-label">${escapeHtml(initials)}</span>
    </div>
    <div class="accio-credit-meta">
      <span class="accio-credit-plan">${escapeHtml(statusLabel)}</span>
      <span class="accio-credit-val">${total > 0 ? `${remaining}/${total}` : "—"}</span>
    </div>
  `;
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
      // Keep the fallback aligned with the selected provider. Showing Grok
      // here made a failed Accio catalog look like a successful model load.
      state.models = fallbackModels(state.settings?.provider);
    }
  }
  paintChrome();
}

function wireEvents() {
  // Structured proxy/app logs streamed in real time from the Go side.
  EventsOn("log", (p) => {
    const level = String(p?.level || "INFO").toLowerCase();
    const msg = p?.msg || "log";
    const fields = p?.fields || null;
    addLog(level, msg, fields);
  });

  EventsOn("kimi:login", (p) => {
    const phase = p?.phase || "unknown";
    const msg = p?.message || String(p || "");
    addLog("kimi-login", msg, { phase, error: p?.error || null });
  });

  EventsOn("accio:login", async (p) => {
    addLog("accio-login", "Login capturado", p);
    state.accioPolling = false;
    await refreshBootstrap(true);
    const st = $("#status-text");
    if (st) {
      st.innerHTML = `Accio conectado · <strong>${escapeHtml(p?.email || p?.label || "ok")}</strong>`;
    }
    await refreshAccioCredits();
  });
  EventsOn("accio:error", (p) => {
    const raw = p?.raw || p?.error || "erro Accio sem payload";
    addLog("accio-gateway", `Erro bruto · conta ${p?.account_id || "?"} · tentativa ${p?.attempt || "?"}`, raw);
    const st = $("#status-text");
    if (st) st.innerHTML = `Accio · <strong>erro do gateway</strong> · ${escapeHtml(String(raw).slice(0, 160))}`;
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
		await refreshBootstrap(true);
		if (payload?.login_id && state.device?.login_id && payload.login_id !== state.device.login_id) return;
    state.device = null;
    document.querySelector(".overlay")?.remove();
    const n = payload?.count || state.accounts.length;
    // soft toast via status line
    const st = $("#status-text");
    if (st) {
      st.innerHTML = `Conta adicionada · <strong>${escapeHtml(payload?.email || payload?.label || "")}</strong> · ${n} no total`;
    }
  });

	EventsOn("auth:error", (msg) => {
		const loginID = typeof msg === "object" ? msg?.login_id : "";
		const errorText = typeof msg === "object" ? msg?.error || "Falha de autenticação" : msg;
		if (loginID && state.device?.login_id && loginID !== state.device.login_id) return;
		addLog("auth", "Auth error", { error: errorText });
		alert("Auth error: " + errorText);
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
    if (st && p?.model_valid === false) {
      const tier = Number.isFinite(Number(p?.oauth_tier)) ? `tier ${Number(p.oauth_tier)}` : "sem entitlement";
      st.innerHTML = `Conta criada · <strong>API Grok indisponível (${escapeHtml(tier)})</strong>`;
    } else if (st) {
      st.innerHTML = `Signup · fase <strong>${escapeHtml(p?.phase || "done")}</strong>`;
    }
  });
  EventsOn("signup:auto_triggered", (p) => {
    addLog("signup", "Auto triggered (pool below floor)", p);
    const st = $("#status-text");
    const usable = p?.usable ?? 0;
    const target = p?.target ?? 3;
    if (st) st.innerHTML = `Pool Grok · <strong>${usable}/${target}</strong> contas válidas — criando conta…`;
  });
  EventsOn("signup:pool_ready", (p) => {
    addLog("signup", "Pool ready", p);
    const st = $("#status-text");
    if (st) st.innerHTML = `Pool Grok OK · <strong>${escapeHtml(String(p?.usable ?? ""))}</strong> contas válidas`;
  });
  EventsOn("signup:pool_retry", (p) => {
    addLog("signup", "Pool retry", p);
  });
  EventsOn("signup:batch_progress", (p) => {
    addLog("signup", "Batch progress", p);
    const st = $("#status-text");
    if (st) st.innerHTML = `Lote Grok · <strong>${escapeHtml(String(p?.created ?? 0))}</strong> criadas · ${escapeHtml(String(p?.usable ?? 0))} utilizáveis`;
  });
  EventsOn("signup:pool_incomplete", (p) => {
    addLog("signup", "Pool incomplete", p);
    const st = $("#status-text");
    if (st) st.innerHTML = `Pool Grok incompleto · <strong>${escapeHtml(String(p?.usable ?? 0))}/${escapeHtml(String(p?.target ?? 3))}</strong> · ${escapeHtml(p?.reason || "")}`;
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
  EventsOn("accounts:changed", async (p) => {
    addLog("account", "Changed", p);
    await refreshBootstrap(false);
  });
}

async function main() {
  wireEvents();
  await refreshBootstrap(true);
  const p = (state.settings?.provider || "xai").toLowerCase();
  if (p === "accio" || p === "accio-work" || p === "phoenix") {
    await refreshAccioCredits();
  }
}

main().catch((e) => {
  document.body.innerHTML = `<pre style="color:#f88;padding:24px;font-family:monospace">Falha UI: ${e}</pre>`;
});
