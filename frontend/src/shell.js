import {
  GetBootstrap,
  SetActiveAccount,
  UpdateSettings,
  ListModels,
} from "../wailsjs/go/main/App";
import { state } from "./state.js";
import { $, escapeHtml, fmt, fmtUSD, fmtMs, shortPath, initials, countUsableAccounts, isUsableAccount } from "./util.js";
import { renderMarkdown, enhanceMarkdownRoot } from "./markdown.js";
import { mountMenu, closeAllMenus } from "./menus.js";
import { renderSearchBlock } from "./search-ui.js";
import { openStatsModal } from "./stats.js";
import {
  startLogin,
  openAccountsManager,
} from "./register-ui.js";

export function globalUsage() {
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

export function activeAccount() {
  return state.accounts.find((a) => a.active) || state.accounts[0] || null;
}

export function ensureShell() {
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
            <div class="rail-label">Contas ativas</div>
            <span class="accounts-count" id="accounts-count" title="Utilizáveis agora (sem cota/chat negado/login morto)">0</span>
          </div>
          <div class="accounts accounts-active-only" id="accounts"></div>
          <div class="rail-actions">
            <button type="button" class="btn btn-quiet" id="btn-accounts">Gerenciar contas</button>
            <button type="button" class="btn btn-quiet" id="btn-add">Adicionar conta</button>
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
        </div>

        <div class="rail-foot">
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

  $("#btn-add").onclick = () => startLogin();
  $("#btn-accounts").onclick = () =>
    openAccountsManager({ refreshBootstrap, paintChrome });
  $("#btn-stats").onclick = openStatsModal;
  $("#btn-stats-top").onclick = openStatsModal;

  const fallbackEffortOpts = [
    { value: "low", label: "Low" },
    { value: "medium", label: "Medium" },
    { value: "high", label: "High" },
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
  const apiOpts = [
    { value: "chat", label: "Chat" },
    { value: "responses", label: "Responses" },
  ];
  const modelOpts = () =>
    (state.models.length
      ? state.models
      : [
          { id: "grok-4.5", name: "Grok 4.5" },
          { id: "grok-4.5-responses", name: "Grok 4.5 (Responses)" },
        ]
    ).map((m) => ({ value: m.id, label: m.name || m.id }));

  mountMenu($("#set-effort"), {
    id: "set-effort",
    options: () => effortOptionsForModel(selectedModelId(false)),
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
      syncEffortMenus();
      saveGlobal({ default_model: v });
    },
  });

  // Composer account switcher: only usable accounts (hide cooldown / denied / dead SSO)
  const accountOpts = () => {
    const usable = state.accounts.filter(isUsableAccount);
    if (!usable.length) {
      return [{ value: "", label: "sem conta utilizável" }];
    }
    return usable.map((a) => ({
      value: a.id,
      label: a.active
        ? `● ${a.email || a.label || a.id}`
        : a.email || a.label || a.id,
    }));
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
      syncEffortMenus();
    },
  });
  mountMenu($("#c-effort"), {
    id: "c-effort",
    options: () => effortOptionsForModel(selectedModelId(true)),
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
  if (prompt) {
    prompt.addEventListener("input", () => autoGrow(prompt));
  }

  state.shellBuilt = true;
}

export function autoGrow(ta) {
  ta.style.height = "auto";
  ta.style.height = Math.min(160, ta.scrollHeight) + "px";
}

export function fillModels() {
  // custom menus re-render options via refresh
  state.menus["set-model"]?.refresh?.();
  state.menus["c-model"]?.refresh?.();
  const prefer = state.settings.default_model || state.picks.model || "grok-4.5";
  if (state.menus["set-model"]) state.menus["set-model"].setValue(prefer);
  if (state.menus["c-model"] && !state.picks.cModelTouched) {
    state.menus["c-model"].setValue(prefer);
    state.picks.cModel = prefer;
  }
  const selected = state.models.find((m) => m.id === prefer);
  const defaultEffort = selected?.default_reasoning_effort;
  if (defaultEffort) {
    state.picks.effort = defaultEffort;
    state.picks.cEffort = defaultEffort;
  }
  state.menus["set-effort"]?.refresh?.();
  state.menus["c-effort"]?.refresh?.();
  state.menus["set-effort"]?.setValue(state.picks.effort);
  state.menus["c-effort"]?.setValue(state.picks.cEffort);
}

function accountBadges(a) {
  const bits = [];
  if (a.active) {
    bits.push(`<span class="badge badge-live">ativa</span>`);
  }
  // Quota (24h) — exclusive of "pronta"
  if (a.exhausted) {
    bits.push(
      `<span class="badge badge-warn" title="Cota/rate limit free tier (~24h). Resetar se já liberou, ou use outra conta.">cota esgotada</span>`
    );
  }
  // Upstream Forbidden: chat endpoint denied / permissions
  if (a.chat_denied) {
    const tip = a.chat_denied_reason
      ? String(a.chat_denied_reason).slice(0, 200)
      : "Forbidden: Access to the chat endpoint is denied — permissões no console.x.ai";
    bits.push(
      `<span class="badge badge-danger" title="${escapeHtml(tip)}">chat negado</span>`
    );
  }
  // Login only when access dead AND no refresh (SSO / ImportSSO)
  if (a.needs_login || (a.expired && a.has_refresh === false)) {
    bits.push(
      `<span class="badge badge-warn" title="Sem refresh token — faça device login ou reimporte SSO.">login expirado</span>`
    );
  } else if (a.expired && a.has_refresh) {
    bits.push(
      `<span class="badge" title="Access token expirado; renovação automática via refresh." style="opacity:.8">renova auto</span>`
    );
  }
  // Healthy non-active
  if (!a.active && !a.exhausted && !a.chat_denied && !a.needs_login && !(a.expired && a.has_refresh === false)) {
    bits.push(`<span class="badge badge-ok">pronta</span>`);
  }
  return bits.join("");
}

export function paintChrome() {
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
  $("#data-dir").textContent = shortPath(state.dataDir) || "—";
  $("#data-dir").title = state.dataDir || "";

  const list = $("#accounts");
  const countEl = $("#accounts-count");
  if (countEl) {
    const usable = countUsableAccounts(state.accounts);
    const total = state.accounts.length;
    countEl.textContent = String(usable);
    countEl.title = `${usable} utilizáveis agora · ${total} no pool total`;
    countEl.dataset.usable = String(usable);
    countEl.dataset.total = String(total);
  }
  list.innerHTML = "";
  if (!state.accounts.length) {
    list.innerHTML = `<div class="account empty-hint">Nenhuma conta.<br/>Use <b>Adicionar conta</b> ou <b>Gerenciar contas</b>.</div>`;
  } else {
    const a = acc || state.accounts.find((x) => x.active) || state.accounts[0];
    const u = a.usage || {};
    const card = document.createElement("div");
    card.className = "account active account-focus";
    card.innerHTML = `
      <div class="account-top">
        <div class="avatar">${escapeHtml(initials(a.email || a.label))}</div>
        <div style="min-width:0">
          <strong title="${escapeHtml(a.email || a.id)}">${escapeHtml(a.label || a.email || a.id)}</strong>
          <div class="meta-line">
            ${accountBadges({ ...a, active: true })}
            <span>${escapeHtml((a.email || "").split("@")[0] || a.id.slice(0, 8))}</span>
          </div>
        </div>
      </div>
      <div class="account-usage">
        <span><b>${fmt(u.total_tokens || 0)}</b> tok</span>
        <span><b>${fmtUSD(u.cost_usd || 0)}</b></span>
        <span><b>${fmt(u.requests || 0)}</b> req</span>
      </div>
    `;
    list.appendChild(card);
  }

  // refresh composer account menu (usable only)
  state.menus["c-account"]?.refresh?.();
  const usableList = state.accounts.filter(isUsableAccount);
  const pickId =
    (acc && isUsableAccount(acc) && acc.id) ||
    usableList.find((a) => a.active)?.id ||
    usableList[0]?.id ||
    "";
  if (pickId) state.menus["c-account"]?.setValue(pickId);

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
  if (pickId) state.menus["c-account"]?.setValue(pickId);

  paintStatus();
  paintSend();
  paintMessages();
}

export function paintStatus() {
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
    const usable = countUsableAccounts(state.accounts);
    text.innerHTML = acc
      ? `Ativa: <strong>${escapeHtml(acc.label || acc.email || acc.id)}</strong> · <span title="utilizáveis">${usable} ativas</span>`
      : "Nenhuma conta — adicione à esquerda (multi-conta ok)";
  }
}

export function paintSend() {
  const btn = $("#send");
  if (!btn) return;
  btn.classList.toggle("stop", state.streaming);
  btn.textContent = state.streaming ? "■" : "↑";
  btn.title = state.streaming ? "Parar" : "Enviar";
}

export function paintMessages() {
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
      const answer = renderMarkdown(m.content || "") + cursor;
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

export async function saveGlobal(patch) {
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
}

export async function refreshBootstrap(full = true) {
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
      const p = (state.settings?.provider || "xai").toLowerCase();
      state.models = ["opencode_zen", "opencode-zen", "opencode", "zen", "zen-free"].includes(p)
        ? [
            { id: "opencode/deepseek-v4-flash-free", name: "DeepSeek V4 Flash Free" },
            { id: "opencode/big-pickle", name: "Big Pickle Free" },
            { id: "opencode/mimo-v2.5-free", name: "MiMo V2.5 Free" },
            { id: "opencode/nemotron-3-ultra-free", name: "Nemotron 3 Ultra Free" },
            { id: "opencode/north-mini-code-free", name: "North Mini Code Free" },
            { id: "opencode/ling-3.0-flash-free", name: "Ling 3.0 Flash Free" },
            { id: "opencode/laguna-s-2.1-free", name: "Laguna S 2.1 Free" },
          ]
        : [
            { id: "grok-4.5", name: "Grok 4.5" },
            { id: "grok-4.5-responses", name: "Grok 4.5 (Responses)" },
          ];
    }
  }
  paintChrome();
}
