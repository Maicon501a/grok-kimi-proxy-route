import { SendChat, CancelChat } from "../wailsjs/go/main/App";
import { state } from "./state.js";
import { $, fmt, fmtUSD, fmtMs } from "./util.js";
import { onChatEventTool } from "./search-ui.js";
import { activeAccount, paintSend, paintStatus, paintMessages } from "./shell.js";

let thinkChars = 0;
let paintScheduled = false;

export function schedulePaintMessages() {
  if (paintScheduled) return;
  paintScheduled = true;
  requestAnimationFrame(() => {
    paintScheduled = false;
    paintMessages();
  });
}

export async function submit() {
  const promptEl = $("#prompt");
  const text = (promptEl?.value || "").trim();
  if (!text || state.streaming) return;
  // Provedores API key não usam pool de contas — só provedores de sessão exigem conta.
  const pNow = (state.settings?.provider || "xai").toLowerCase();
  const sessionAuth =
    pNow === "xai" || pNow === "grok" || pNow === "kimi_work" || pNow === "kimi" ||
    pNow === "kimi-work" || pNow === "accio" || pNow === "accio-work" || pNow === "phoenix";
  if (sessionAuth && !activeAccount()) {
    alert("Adicione e selecione uma conta primeiro.");
    return;
  }

  const model =
    state.menus["c-model"]?.getValue?.() || state.picks.cModel || state.settings.default_model;
  const effort =
    state.menus["c-effort"]?.getValue?.() || state.picks.cEffort || state.settings.reasoning_effort;
  const apiMode =
    state.menus["c-api"]?.getValue?.() || state.picks.cApi || state.settings.api_mode;

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
  const { autoGrow } = await import("./shell.js");
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

  if (apiMode === "chat") {
    payload.messages = state.messages
      .filter((m) => m.role === "user" || (m.role === "assistant" && m.content && !m.streaming))
      .map((m) => ({ role: m.role, content: m.content }));
    if (payload.messages.at(-1)?.role === "assistant") payload.messages.pop();
  } else if (state.lastResponseId) {
    payload.last_response_id = state.lastResponseId;
    payload.messages = [{ role: "user", content: text }];
  } else {
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
      last.content = "Erro: " + e;
      last.streaming = false;
    }
    paintSend();
    paintMessages();
  }
}

export function onChatEvent(ev) {
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
    last.content = (last.content || "") + (last.content ? "\n" : "") + ev.error;
    last.streaming = false;
    state.streaming = false;
    paintSend();
    paintStatus();
  }

  schedulePaintMessages();
}
