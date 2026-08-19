import { GetStats } from "../wailsjs/go/main/App";
import { $, escapeHtml, fmt, fmtUSD, fmtMs } from "./util.js";

export function sparklineSVG(values, color = "rgba(125,211,252,0.9)") {
  const nums = (values || []).map((v) => Number(v) || 0);
  if (!nums.length) {
    return `<div class="chart-empty">Sem amostras ainda — envie um chat</div>`;
  }
  const w = 320;
  const h = 88;
  const pad = 6;
  const max = Math.max(...nums, 1);
  const min = Math.min(...nums, 0);
  const span = Math.max(max - min, 1);
  const pts = nums.map((v, i) => {
    const x = pad + (i * (w - pad * 2)) / Math.max(nums.length - 1, 1);
    const y = h - pad - ((v - min) / span) * (h - pad * 2);
    return [x, y];
  });
  const line = pts.map((p, i) => (i === 0 ? `M ${p[0]} ${p[1]}` : `L ${p[0]} ${p[1]}`)).join(" ");
  const area =
    line +
    ` L ${pts[pts.length - 1][0]} ${h - pad} L ${pts[0][0]} ${h - pad} Z`;
  const last = nums[nums.length - 1];
  return `
    <svg viewBox="0 0 ${w} ${h}" preserveAspectRatio="none">
      <defs>
        <linearGradient id="g-${color.replace(/[^a-z0-9]/gi, "")}" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stop-color="${color}" stop-opacity="0.35"/>
          <stop offset="100%" stop-color="${color}" stop-opacity="0"/>
        </linearGradient>
      </defs>
      <path d="${area}" fill="url(#g-${color.replace(/[^a-z0-9]/gi, "")})"/>
      <path d="${line}" fill="none" stroke="${color}" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>
      <text x="${w - pad}" y="${pad + 10}" text-anchor="end" fill="rgba(255,255,255,0.35)" font-size="10">${fmt(last)}</text>
    </svg>
  `;
}

export async function openStatsModal() {
  document.querySelector(".stats-overlay")?.remove();
  let stats;
  try {
    stats = await GetStats();
  } catch (e) {
    alert("API: " + e);
    return;
  }
  const g = stats.global || {};
  const proxy = stats.proxy || {};
  const rate = stats.active_rate || {};
  const base = proxy.base_url || "http://127.0.0.1:8787/v1";
  const modelsURL = proxy.models_url || base + "/models";
  const key = proxy.api_key || "local";
  const modelList = (proxy.models_example || ["grok-4.6", "kimi-for-coding", "k3-agent", "k3-agent-low", "k3-agent-medium", "k3-agent-high", "k3-agent-xhigh"]).join(", ");

  const snippets = {
    opencode: proxy.opencode || "",
    kilo: proxy.kilo || "",
    env: proxy.openai_env || "",
    curl: proxy.curl || "",
  };
  let tab = "opencode";

  const overlay = document.createElement("div");
  overlay.className = "stats-overlay";
  overlay.innerHTML = `
    <div class="stats-panel api-panel" role="dialog" aria-label="Ver mais da API">
      <div class="stats-head">
        <div>
          <h2>Ver mais da API</h2>
          <p>Proxy local OpenAI-compatible · OpenCode · Kilo Code · models pela base URL</p>
        </div>
        <button class="icon-btn" id="stats-close">Fechar</button>
      </div>

      <div class="api-hero">
        <div class="api-row">
          <label>Base URL</label>
          <code id="api-base">${escapeHtml(base)}</code>
          <button type="button" class="copy-inline" data-copy="${escapeHtml(base)}">Copiar</button>
        </div>
        <div class="api-row">
          <label>Models URL</label>
          <code id="api-models">${escapeHtml(modelsURL)}</code>
          <button type="button" class="copy-inline" data-copy="${escapeHtml(modelsURL)}">Copiar</button>
        </div>
        <div class="api-row">
          <label>API key</label>
          <code>${escapeHtml(key)}</code>
          <button type="button" class="copy-inline" data-copy="${escapeHtml(key)}">Copiar</button>
        </div>
      </div>

      <div class="api-explain">
        <h3>Como o OpenCode / Kilo puxam os models</h3>
        <ol>
          <li>Você cola a <b>Base URL</b> no client (provider OpenAI Compatible).</li>
          <li>O client chama <code>GET ${escapeHtml(modelsURL)}</code>.</li>
          <li>A lista vem com <b>Grok + Kimi</b> juntos (mesma porta).</li>
          <li>Na hora do chat, o client manda o <b>model</b> escolhido — o proxy roteia sozinho:
            <ul>
              <li><code>grok-4.6</code> → Grok · <code>POST /v1/responses</code></li>
              <li><code>kimi-for-coding</code> / <code>k3-agent</code> → Kimi · <code>POST /v1/chat/completions</code></li>
            </ul>
          </li>
        </ol>
        <p class="api-models-line">Models: <code>${escapeHtml(modelList)}</code></p>
        <p class="sub">Não precisa trocar “provedor ativo” no app. O model do client manda. App precisa estar aberto.</p>
      </div>

      <div class="stats-grid api-kpis">
        <div class="kpi"><label>Tokens total</label><strong>${fmt(g.total_tokens)}</strong><span>${fmt(g.requests)} requests</span></div>
        <div class="kpi"><label>Custo est.</label><strong>${fmtUSD(g.cost_usd)}</strong><span>in $${rate.input_per_m ?? 2}/M · out $${rate.output_per_m ?? 6}/M</span></div>
        <div class="kpi"><label>Latência méd.</label><strong>${fmtMs(stats.avg_latency_ms)}</strong><span>TTFT ${fmtMs(stats.avg_ttft_ms)}</span></div>
        <div class="kpi"><label>Roteamento</label><strong>por model</strong><span>base + /v1/models</span></div>
      </div>

      <div class="snippet-card">
        <h3>Configurar OpenCode / Kilo / ENV / cURL</h3>
        <p class="sub">Cole o snippet no client. OpenCode usa JSON; Kilo usa provider OpenAI Compatible com a Base URL.</p>
        <div class="snippet-tabs">
          <button type="button" data-tab="opencode" class="on">OpenCode JSON</button>
          <button type="button" data-tab="kilo">Kilo Code</button>
          <button type="button" data-tab="env">ENV</button>
          <button type="button" data-tab="curl">cURL</button>
        </div>
        <div class="snippet-body">
          <pre id="snippet-pre">${escapeHtml(snippets.opencode)}</pre>
          <button type="button" class="copy" id="snippet-copy">Copiar</button>
        </div>
      </div>

      <div class="charts">
        <div class="chart-card">
          <h3>Latência (ms)</h3>
          <div id="chart-lat">${sparklineSVG(stats.latency_series, "rgba(125,211,252,0.95)")}</div>
        </div>
        <div class="chart-card">
          <h3>TTFT (ms)</h3>
          <div id="chart-ttft">${sparklineSVG(stats.ttft_series, "rgba(167,139,250,0.95)")}</div>
        </div>
      </div>

      <p class="pricing-note">
        Proxy local: <code>${escapeHtml(base)}</code> ·
        Models: <code>${escapeHtml(modelsURL)}</code>
      </p>
    </div>
  `;
  document.body.appendChild(overlay);

  const close = () => overlay.remove();
  $("#stats-close", overlay).onclick = close;
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

  const pre = $("#snippet-pre", overlay);
  const tabs = overlay.querySelectorAll(".snippet-tabs button");
  tabs.forEach((btn) => {
    btn.onclick = () => {
      tabs.forEach((b) => b.classList.remove("on"));
      btn.classList.add("on");
      tab = btn.dataset.tab;
      pre.textContent = snippets[tab] || "";
    };
  });
  $("#snippet-copy", overlay).onclick = async () => {
    await navigator.clipboard.writeText(snippets[tab] || "");
    const b = $("#snippet-copy", overlay);
    b.textContent = "Copiado";
    setTimeout(() => (b.textContent = "Copiar"), 1200);
  };
  overlay.querySelectorAll(".copy-inline").forEach((btn) => {
    btn.onclick = async () => {
      await navigator.clipboard.writeText(btn.dataset.copy || "");
      const t = btn.textContent;
      btn.textContent = "OK";
      setTimeout(() => (btn.textContent = t), 900);
    };
  });
}
