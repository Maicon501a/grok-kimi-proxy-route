import {
  ImportSSO,
  ImportSSOFromFile,
  StartDeviceLogin,
  CancelDeviceLogin,
  CancelRegisterBatch,
  CreateAccounts,
  OpenExternal,
  SetActiveAccount,
  RemoveAccount,
  LogoffKimiAccount,
  RenameAccount,
  ResetAccount,
  RecoverAccounts,
} from "../wailsjs/go/main/App";
import { state } from "./state.js";
import { $, escapeHtml, fmt, fmtUSD, fmtDuration, initials, isUsableAccount, countUsableAccounts } from "./util.js";

function closeOverlays(sel = ".overlay, .stats-overlay, .accounts-overlay") {
  document.querySelectorAll(sel).forEach((el) => el.remove());
}

function cooldownSeconds(a) {
  if (!a) return 0;
  // Prefer absolute end time
  const untilRaw = a.exhausted_until || a.exhaustedUntil;
  if (untilRaw) {
    const until = Date.parse(untilRaw);
    if (!Number.isNaN(until)) {
      return Math.max(0, Math.floor((until - Date.now()) / 1000));
    }
  }
  // Or start + 24h window
  const atRaw = a.exhausted_at || a.exhaustedAt;
  if (atRaw) {
    const at = Date.parse(atRaw);
    if (!Number.isNaN(at)) {
      const end = at + 24 * 3600 * 1000;
      return Math.max(0, Math.floor((end - Date.now()) / 1000));
    }
  }
  const sec = Number(a.exhausted_remaining_sec ?? a.exhaustedRemainingSec);
  if (Number.isFinite(sec) && sec > 0) return Math.floor(sec);
  // exhausted flag without timestamps → show full window as estimate
  if (a.exhausted) return 24 * 3600;
  return 0;
}

function accountBadges(a) {
  const bits = [];
  if (a.active) {
    bits.push(`<span class="badge badge-live">ativa</span>`);
  }
  if (a.exhausted) {
    bits.push(
      `<span class="badge badge-warn" title="Cota/rate limit free tier (~24h).">cota esgotada</span>`
    );
  }
  if (a.chat_denied) {
    const tip = a.chat_denied_reason
      ? String(a.chat_denied_reason).slice(0, 200)
      : "Forbidden: Access to the chat endpoint is denied — permissões no console.x.ai";
    bits.push(
      `<span class="badge badge-danger" title="${escapeHtml(tip)}">chat negado</span>`
    );
  }
  if (a.needs_login || (a.expired && a.has_refresh === false)) {
    bits.push(
      `<span class="badge badge-warn" title="Sem refresh token — faça device login ou reimporte SSO.">login expirado</span>`
    );
  } else if (a.expired && a.has_refresh) {
    bits.push(
      `<span class="badge" title="Access token expirado; renovação automática." style="opacity:.8">renova auto</span>`
    );
  }
  if (!a.active && !a.exhausted && !a.chat_denied && !a.needs_login && !(a.expired && a.has_refresh === false)) {
    bits.push(`<span class="badge badge-ok">pronta</span>`);
  }
  if (a.has_google_refresh) {
    bits.push(`<span class="badge badge-ok" title="Google refresh token salvo — re-login sem browser possível.">google refresh</span>`);
  }
  if (a.refresh_tested) {
    bits.push(`<span class="badge badge-ok" title="Refresh token Kimi validado — re-autenticação funciona.">refresh ok</span>`);
  }
  if (a.upstream_tested) {
    bits.push(`<span class="badge badge-ok" title="Chave sk-kimi testada no upstream — conta funcional.">upstream ok</span>`);
  }
  return bits.join("");
}

export async function importSSO(refreshBootstrap) {
  closeOverlays(".overlay");
  const overlay = document.createElement("div");
  overlay.className = "overlay";
  overlay.innerHTML = `
    <div class="sheet sheet-form" role="dialog" aria-label="Importar SSO">
      <h3>Importar SSO</h3>
      <p>Cole o token SSO (ou access token). Contas sem refresh não renovam sozinhas.</p>
      <label class="field-label" for="sso-token">Token</label>
      <textarea id="sso-token" class="sheet-input sheet-textarea" rows="5" placeholder="eyJhbGciOi..." spellcheck="false"></textarea>
      <div class="sheet-actions">
        <button type="button" class="btn btn-solid" id="sso-ok">Importar</button>
        <button type="button" class="btn btn-quiet" id="sso-cancel">Cancelar</button>
      </div>
      <div id="sso-err" class="sheet-err" hidden></div>
    </div>
  `;
  document.body.appendChild(overlay);
  const ta = $("#sso-token", overlay);
  ta?.focus();
  const close = () => overlay.remove();
  $("#sso-cancel", overlay).onclick = close;
  overlay.addEventListener("click", (e) => {
    if (e.target === overlay) close();
  });
  $("#sso-ok", overlay).onclick = async () => {
    const token = (ta?.value || "").trim();
    const errEl = $("#sso-err", overlay);
    if (!token) {
      if (errEl) {
        errEl.hidden = false;
        errEl.textContent = "Cole um token.";
      }
      return;
    }
    try {
      await ImportSSO(token);
      close();
      await refreshBootstrap?.(true);
    } catch (e) {
      if (errEl) {
        errEl.hidden = false;
        errEl.textContent = "Falha: " + e;
      }
    }
  };
}

export async function importSSOFile(refreshBootstrap) {
  closeOverlays(".overlay");
  const overlay = document.createElement("div");
  overlay.className = "overlay";
  overlay.innerHTML = `
    <div class="sheet sheet-form" role="dialog" aria-label="Importar arquivo SSO">
      <h3>Importar de arquivo</h3>
      <p>Caminho absoluto de um <code>.txt</code> (um token por linha, ou <code>email:senha:SSO</code>).</p>
      <label class="field-label" for="sso-path">Caminho</label>
      <input id="sso-path" class="sheet-input" type="text" placeholder="/home/você/tokens.txt" spellcheck="false" />
      <div class="sheet-actions">
        <button type="button" class="btn btn-solid" id="sso-file-ok">Importar</button>
        <button type="button" class="btn btn-quiet" id="sso-file-cancel">Cancelar</button>
      </div>
      <div id="sso-file-err" class="sheet-err" hidden></div>
    </div>
  `;
  document.body.appendChild(overlay);
  $("#sso-path", overlay)?.focus();
  const close = () => overlay.remove();
  $("#sso-file-cancel", overlay).onclick = close;
  overlay.addEventListener("click", (e) => {
    if (e.target === overlay) close();
  });
  $("#sso-file-ok", overlay).onclick = async () => {
    const path = ($("#sso-path", overlay)?.value || "").trim();
    const errEl = $("#sso-file-err", overlay);
    if (!path) {
      if (errEl) {
        errEl.hidden = false;
        errEl.textContent = "Informe o caminho do arquivo.";
      }
      return;
    }
    try {
      const result = await ImportSSOFromFile(path);
      close();
      await refreshBootstrap?.(true);
      // soft status via temporary toast in sheet isn't needed; bootstrap updates UI
      console.info("SSO file import", result);
    } catch (e) {
      if (errEl) {
        errEl.hidden = false;
        errEl.textContent = "Falha: " + e;
      }
    }
  };
}

export async function startLogin() {
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

export function showDeviceModal(st) {
  closeOverlays(".overlay");
  const overlay = document.createElement("div");
  overlay.className = "overlay";
  overlay.innerHTML = `
    <div class="sheet sheet-form" role="dialog" aria-label="Adicionar conta">
      <h3>Adicionar conta</h3>
      <p>Confirme o código na página da xAI. O app completa sozinho.</p>
      <div class="code">${escapeHtml(st.user_code)}</div>
      <div class="sheet-actions">
        <button type="button" class="btn btn-solid" id="m-open">Abrir login</button>
        <button type="button" class="btn btn-quiet" id="m-copy">Copiar código</button>
        <button type="button" class="btn btn-quiet" id="m-cancel">Cancelar</button>
      </div>
      <div class="hint">${escapeHtml(st.verification_url || "")}</div>
    </div>
  `;
  document.body.appendChild(overlay);
  $("#m-open", overlay).onclick = () => OpenExternal(st.verification_url);
  $("#m-copy", overlay).onclick = async () => {
    await navigator.clipboard.writeText(st.user_code);
  };
  $("#m-cancel", overlay).onclick = () => {
    CancelDeviceLogin();
    state.device = null;
    overlay.remove();
  };
  overlay.addEventListener("click", (e) => {
    if (e.target === overlay) {
      CancelDeviceLogin();
      state.device = null;
      overlay.remove();
    }
  });
}

export function showBatchCreateModal(paintChrome) {
  closeOverlays(".overlay");
  const activeN = state.accounts.filter((a) => a && !a.exhausted && !a.expired).length;
  const overlay = document.createElement("div");
  overlay.className = "overlay";
  overlay.innerHTML = `
    <div class="sheet sheet-form" role="dialog" aria-label="Gerar contas">
      <h3>Gerar contas</h3>
      <p>Quantas contas <b>novas</b> criar neste lote? (teto por lote, sem limite no pool)</p>
      <p class="sheet-sub">No pool: ${state.accounts.length} · ~ativas: ${activeN}</p>
      <label class="field-label" for="bc-count">Quantidade</label>
      <input type="number" id="bc-count" class="sheet-input sheet-input-sm" value="2" min="1" max="5" />
      <div class="sheet-actions" style="margin-top:14px">
        <button type="button" class="btn btn-solid" id="bc-start">Gerar</button>
        <button type="button" class="btn btn-quiet" id="bc-cancel">Cancelar</button>
      </div>
      <div id="bc-progress" class="sheet-progress"></div>
    </div>
  `;
  document.body.appendChild(overlay);

  $("#bc-start", overlay).onclick = async () => {
    let n = parseInt($("#bc-count", overlay)?.value || "1", 10);
    if (!Number.isFinite(n) || n < 1) n = 1;
    if (n > 5) n = 5;
    const btn = $("#bc-start", overlay);
    const cancelBtn = $("#bc-cancel", overlay);
    const prog = $("#bc-progress", overlay);
    btn.disabled = true;
    btn.textContent = "Gerando...";
    if (cancelBtn) cancelBtn.textContent = "Parar";
    state.batchCreating = true;
    prog.innerHTML = `<div class="muted">Aguarde, criando ${n} conta(s)… (Parar cancela o lote)</div>`;
    try {
      const results = await CreateAccounts(n);
      const list = Array.isArray(results) ? results : [];
      const okN = list.filter((r) => r.status === "success").length;
      const cancelled = list.some((r) => String(r.reason || "").includes("cancel"));
      prog.innerHTML =
        list
          .map((r, i) => {
            const status = r.status === "success" ? "✅" : "❌";
            const email = r.creds?.email || r.creds?.account_id || "";
            const creds = email
              ? ` <span class="muted">${escapeHtml(email)}</span>`
              : "";
            const reason = r.reason ? ` — ${escapeHtml(String(r.reason))}` : "";
            return `<div>${status} Tentativa ${r.attempt || i + 1}: ${escapeHtml(r.status || "?")}${reason}${creds}</div>`;
          })
          .join("") || `<div class="sheet-err">Nenhum resultado (pediu ${n})</div>`;
      if (cancelled) {
        prog.innerHTML += `<div class="muted" style="margin-top:8px">Lote cancelado · Criadas: ${okN}/${list.length}</div>`;
      } else {
        prog.innerHTML += `<div class="muted" style="margin-top:8px">Pedido: ${n} · Criadas: ${okN}/${list.length}</div>`;
      }
      await paintChrome?.();
      if (list.some((r) => r.status === "success") && !cancelled) {
        setTimeout(async () => {
          if (document.body.contains(overlay)) overlay.remove();
          state.batchCreating = false;
          await paintChrome?.();
        }, 4000);
      } else {
        state.batchCreating = false;
        btn.disabled = false;
        btn.textContent = "Gerar";
        if (cancelBtn) cancelBtn.textContent = "Cancelar";
      }
    } catch (e) {
      state.batchCreating = false;
      prog.innerHTML = `<div class="sheet-err">Erro: ${escapeHtml(String(e))}</div>`;
      btn.disabled = false;
      btn.textContent = "Gerar";
      if (cancelBtn) cancelBtn.textContent = "Cancelar";
    }
  };
  $("#bc-cancel", overlay).onclick = () => {
    if (state.batchCreating) {
      CancelRegisterBatch();
      const prog = $("#bc-progress", overlay);
      if (prog) {
        prog.innerHTML += `<div class="muted" style="margin-top:6px">Cancelando… (encerrando bot/poll)</div>`;
      }
      return;
    }
    state.batchCreating = false;
    overlay.remove();
  };
  overlay.addEventListener("click", (e) => {
    if (e.target === overlay && !state.batchCreating) overlay.remove();
  });
}

/** Full accounts manager — same visual language as Stats modal */
export function openAccountsManager({ refreshBootstrap, paintChrome }) {
  document.querySelector(".accounts-overlay")?.remove();

  // all | usable | exhausted | denied | login
  let accFilter = "all";

  const matchesFilter = (a) => {
    if (accFilter === "all") return true;
    if (accFilter === "usable") return isUsableAccount(a);
    if (accFilter === "exhausted") return !!a.exhausted;
    if (accFilter === "denied") return !!a.chat_denied;
    if (accFilter === "login") return !!(a.needs_login || (a.expired && a.has_refresh === false));
    return true;
  };

  const renderList = () => {
    const body = $("#acc-mgr-list");
    if (!body) return;
    if (!state.accounts.length) {
      body.innerHTML = `<div class="acc-mgr-empty">Nenhuma conta. Use <b>Adicionar</b> ou <b>Gerar</b>.</div>`;
      return;
    }
    const filtered = state.accounts.filter(matchesFilter);
    // update filter chip counts
    const setChip = (id, n) => {
      const el = document.getElementById(id);
      if (el) el.textContent = String(n);
    };
    const all = state.accounts;
    setChip("flt-n-all", all.length);
    setChip("flt-n-usable", countUsableAccounts(all));
    setChip("flt-n-exh", all.filter((a) => a.exhausted).length);
    setChip("flt-n-denied", all.filter((a) => a.chat_denied).length);
    setChip("flt-n-login", all.filter((a) => a.needs_login || (a.expired && a.has_refresh === false)).length);
    document.querySelectorAll("[data-acc-filter]").forEach((btn) => {
      btn.classList.toggle("on", btn.getAttribute("data-acc-filter") === accFilter);
    });
    if (!filtered.length) {
      body.innerHTML = `<div class="acc-mgr-empty">Nenhuma conta neste filtro.</div>`;
      return;
    }
    body.innerHTML = filtered
      .map((a) => {
        const u = a.usage || {};
        const remSec = a.exhausted ? cooldownSeconds(a) : 0;
        const untilLabel = a.exhausted_until || a.exhaustedUntil || "";
        const coolHtml = a.exhausted
          ? remSec > 0
            ? `<div class="acc-mgr-cooldown" title="Rate limit free tier (~24h)${untilLabel ? " — libera " + escapeHtml(untilLabel) : ""}">
                 <span class="badge badge-warn">cooldown</span>
                 <strong class="acc-cool-val" data-cool-for="${escapeHtml(a.id)}">${fmtDuration(remSec)}</strong>
                 <span class="muted">restantes</span>
               </div>`
            : `<div class="acc-mgr-cooldown">
                 <span class="badge badge-ok">cooldown</span>
                 <strong class="acc-cool-val" data-cool-for="${escapeHtml(a.id)}">0s</strong>
                 <span class="muted">— pode Resetar</span>
               </div>`
          : "";
        return `
        <div class="acc-mgr-row ${a.active ? "is-active" : ""}" data-id="${escapeHtml(a.id)}">
          <div class="acc-mgr-main">
            <div class="avatar">${escapeHtml(initials(a.email || a.label))}</div>
            <div class="acc-mgr-text">
              <strong title="${escapeHtml(a.email || a.id)}">${escapeHtml(a.label || a.email || a.id)}</strong>
              <div class="meta-line">${accountBadges(a)} <span class="muted">${escapeHtml(a.email || a.id.slice(0, 12))}</span></div>
              ${
                a.chat_denied
                  ? `<div class="acc-mgr-denied" title="${escapeHtml(a.chat_denied_reason || "")}">${escapeHtml(
                      (a.chat_denied_reason || "Access to the chat endpoint is denied").slice(0, 140)
                    )}</div>`
                  : ""
              }
              ${coolHtml}
              <div class="acc-mgr-usage">
                <span><b>${fmt(u.total_tokens || 0)}</b> tok</span>
                <span><b>${fmtUSD(u.cost_usd || 0)}</b></span>
                <span><b>${fmt(u.requests || 0)}</b> req</span>
              </div>
            </div>
          </div>
          <div class="acc-mgr-actions">
            ${
              a.active
                ? `<button type="button" class="icon-btn" disabled>Em uso</button>`
                : `<button type="button" class="icon-btn" data-act="use">Usar</button>`
            }
            ${a.exhausted || a.chat_denied ? `<button type="button" class="icon-btn" data-act="reset" title="Limpa cota esgotada e chat negado">Resetar</button>` : ""}
            ${a.provider === "kimi_work" ? `<button type="button" class="icon-btn" data-act="edit-creds" title="Email/senha Google para re-login automático">Credenciais Google</button>` : ""}
            <button type="button" class="icon-btn" data-act="rename">Renomear</button>
            <button type="button" class="icon-btn danger" data-act="remove">Remover</button>
          </div>
        </div>`;
      })
      .join("");

    body.querySelectorAll(".acc-mgr-row").forEach((row) => {
      const id = row.getAttribute("data-id");
      row.querySelectorAll("[data-act]").forEach((btn) => {
        btn.onclick = async () => {
          const act = btn.getAttribute("data-act");
          try {
            if (act === "use") {
              await SetActiveAccount(id);
              await refreshBootstrap?.(false);
              renderList();
              await paintChrome?.();
            } else if (act === "reset") {
              await ResetAccount(id);
              await RecoverAccounts();
              await refreshBootstrap?.(false);
              renderList();
              await paintChrome?.();
            } else if (act === "rename") {
              const a = state.accounts.find((x) => x.id === id);
              const next = prompt("Nome da conta", a?.label || a?.email || "");
              if (next == null || !String(next).trim()) return;
              await RenameAccount(id, String(next).trim());
              await refreshBootstrap?.(false);
              renderList();
              await paintChrome?.();
            } else if (act === "remove") {
              const a = state.accounts.find((x) => x.id === id);
              if (!confirm(`Remover ${a?.label || a?.email || id}?`)) return;
              if (a?.provider === "kimi_work" && (a.has_web_session || a.has_refresh)) {
                await LogoffKimiAccount(id);
              } else {
                await RemoveAccount(id);
              }
              await refreshBootstrap?.(false);
              renderList();
              await paintChrome?.();
            } else if (act === "edit-creds") {
              const a = state.accounts.find((x) => x.id === id);
              if (!a) return;
              const current = await GetAccountGoogleCredentials(id);
              const email = prompt(`Email Google para re-login automático da conta "${a.label || a.email || id}" (opcional):`, current?.[0] || "");
              if (email === null) return;
              const password = prompt(`Senha Google (opcional - usada apenas se o refresh token falhar):`, current?.[1] || "");
              if (password === null) return;
              await SetAccountGoogleCredentials(id, email || "", password || "");
              alert("Credenciais salvas para esta conta.");
            }
          } catch (e) {
            alert(String(e));
          }
        };
      });
    });
  };

  const overlay = document.createElement("div");
  overlay.className = "accounts-overlay";
  overlay.innerHTML = `
    <div class="stats-panel accounts-panel" role="dialog" aria-label="Gerenciar contas">
      <div class="stats-head">
        <div>
          <h2>Contas</h2>
          <p>Gerencie o pool · ative · importe SSO · gere novas</p>
        </div>
        <button type="button" class="icon-btn" id="acc-mgr-close">Fechar</button>
      </div>

      <div class="acc-mgr-toolbar">
        <button type="button" class="btn btn-solid" id="acc-mgr-add">Adicionar conta</button>
        <button type="button" class="btn btn-quiet" id="acc-mgr-gen">Gerar contas</button>
        <button type="button" class="btn btn-quiet" id="acc-mgr-sso">Importar SSO</button>
        <button type="button" class="btn btn-quiet" id="acc-mgr-file">Importar arquivo</button>
      </div>

      <div class="acc-mgr-filters" role="tablist" aria-label="Filtrar contas">
        <button type="button" class="acc-filter on" data-acc-filter="all">Todas <b id="flt-n-all">0</b></button>
        <button type="button" class="acc-filter" data-acc-filter="usable">Ativas <b id="flt-n-usable">0</b></button>
        <button type="button" class="acc-filter" data-acc-filter="exhausted">Cota <b id="flt-n-exh">0</b></button>
        <button type="button" class="acc-filter" data-acc-filter="denied">Chat negado <b id="flt-n-denied">0</b></button>
        <button type="button" class="acc-filter" data-acc-filter="login">Login <b id="flt-n-login">0</b></button>
      </div>

      <div class="stats-grid acc-mgr-kpis">
        <div class="kpi"><label>Total</label><strong id="acc-kpi-total">0</strong><span>no pool</span></div>
        <div class="kpi"><label>Ativas</label><strong id="acc-kpi-ready">0</strong><span>utilizáveis agora</span></div>
        <div class="kpi"><label>Cota</label><strong id="acc-kpi-exh">0</strong><span>esgotadas ~24h</span></div>
        <div class="kpi"><label>Login</label><strong id="acc-kpi-login">0</strong><span>precisam reauth</span></div>
        <div class="kpi"><label>Chat</label><strong id="acc-kpi-denied">0</strong><span>negado (403)</span></div>
      </div>

      <div id="acc-mgr-list" class="acc-mgr-list"></div>
    </div>
  `;
  document.body.appendChild(overlay);

  const updateKpis = () => {
    const all = state.accounts || [];
    const ready = countUsableAccounts(all);
    const exh = all.filter((a) => a.exhausted).length;
    const login = all.filter((a) => a.needs_login || (a.expired && a.has_refresh === false)).length;
    const denied = all.filter((a) => a.chat_denied).length;
    const set = (id, v) => {
      const el = $(id, overlay);
      if (el) el.textContent = String(v);
    };
    set("#acc-kpi-total", all.length);
    set("#acc-kpi-ready", ready);
    set("#acc-kpi-exh", exh);
    set("#acc-kpi-login", login);
    set("#acc-kpi-denied", denied);
  };

  overlay.querySelectorAll("[data-acc-filter]").forEach((btn) => {
    btn.onclick = () => {
      accFilter = btn.getAttribute("data-acc-filter") || "all";
      renderList();
    };
  });

  let coolTimerRef = null;
  const close = () => {
    if (coolTimerRef) clearInterval(coolTimerRef);
    overlay.remove();
  };
  $("#acc-mgr-close", overlay).onclick = close;
  overlay.addEventListener("click", (e) => {
    if (e.target === overlay) close();
  });
  document.addEventListener(
    "keydown",
    function esc(e) {
      if (e.key === "Escape") {
        close();
        document.removeEventListener("keydown", esc);
      }
    },
    { once: true }
  );

  $("#acc-mgr-add", overlay).onclick = () => {
    close();
    startLogin();
  };
  $("#acc-mgr-gen", overlay).onclick = () => {
    close();
    showBatchCreateModal(paintChrome);
  };
  $("#acc-mgr-sso", overlay).onclick = async () => {
    close();
    await importSSO(refreshBootstrap);
  };
  $("#acc-mgr-file", overlay).onclick = async () => {
    close();
    await importSSOFile(refreshBootstrap);
  };

  updateKpis();
  renderList();

  // Cooldown tick every 30s (window is hours; avoid 1s churn)
  coolTimerRef = setInterval(() => {
    if (!document.body.contains(overlay)) {
      clearInterval(coolTimerRef);
      return;
    }
    let needRefresh = false;
    overlay.querySelectorAll("[data-cool-for]").forEach((el) => {
      const id = el.getAttribute("data-cool-for");
      const a = state.accounts.find((x) => x.id === id);
      if (!a) return;
      const rem = cooldownSeconds(a);
      el.textContent = fmtDuration(rem);
      if (rem <= 0) needRefresh = true;
    });
    if (needRefresh) {
      refreshBootstrap?.(false).then(() => {
        updateKpis();
        renderList();
      });
    }
  }, 30000);
}
