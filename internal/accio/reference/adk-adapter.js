const fs = require("fs");
const path = require("path");
const crypto = require("crypto");

const ROOT = __dirname;
const ADK_PATHS = [
  path.join(ROOT, "extracted_new/node_modules/@ali/accio-adk-ts/lib/index.js"),
  path.join(ROOT, "extracted/node_modules/@ali/accio-adk-ts/lib/index.js"),
];
const SG_PATHS = [
  path.join(ROOT, "extracted_new/node_modules/@phoenix-common/security-guard/prebuild/win32-x64/security_guard.node"),
  path.join(ROOT, "extracted/node_modules/@phoenix-common/security-guard/prebuild/win32-x64/security_guard.node"),
];

let adk = null;
let sgAddon = null;
let umidPromise = null;

function loadOptional(paths) {
  for (const candidate of paths) {
    try {
      if (fs.existsSync(candidate)) return require(candidate);
    } catch (error) {
      console.warn(`[ADK] Failed to load ${candidate}: ${error.message}`);
    }
  }
  return null;
}

function getAdk() {
  if (adk === null) adk = loadOptional(ADK_PATHS) || false;
  return adk || null;
}

function getSecurityGuard() {
  if (sgAddon === null) sgAddon = loadOptional(SG_PATHS) || false;
  return sgAddon || null;
}

function getToken(override) {
  if (typeof override === "string" && override.trim()) return override.trim();
  if (typeof process.env.ACCIO_ADK_AUTH_TOKEN === "string" && process.env.ACCIO_ADK_AUTH_TOKEN.trim()) {
    return process.env.ACCIO_ADK_AUTH_TOKEN.trim();
  }
  try {
    const tokenFile = path.join(ROOT, "token.json");
    const data = JSON.parse(fs.readFileSync(tokenFile, "utf8"));
    return data.accessToken || null;
  } catch {
    return null;
  }
}

function getUtdid() {
  if (process.env.ACCIO_UTDID) return process.env.ACCIO_UTDID;
  try {
    return fs.readFileSync(path.join(process.env.USERPROFILE || "", ".accio/utdid"), "utf8").trim();
  } catch {
    return "";
  }
}

function getAppVersion() {
  if (process.env.ACCIO_APP_VERSION) return process.env.ACCIO_APP_VERSION;
  try {
    const pkg = JSON.parse(fs.readFileSync(path.join(ROOT, "extracted_new/package.json"), "utf8"));
    if (pkg.version) return pkg.version;
  } catch {}
  return "0.25.0";
}

function getGatewayBaseUrl() {
  const configured = (process.env.ACCIO_GATEWAY_BASE_URL || "https://phoenix-gw.alibaba.com").replace(/\/$/, "");
  return /\/api\/adk\/llm$/i.test(configured) ? configured : `${configured}/api/adk/llm`;
}

function getSecurityHeaders(url) {
  const sg = getSecurityGuard();
  if (!sg) return {};
  try {
    const appkey = process.env.PHOENIX_SECURITY_GUARD_APPKEY || "35336201";
    const raw = sg.getSecurityFactorsForWeb(JSON.stringify({ appkey, urlInput: url }));
    const parsed = JSON.parse(raw);
    const headers = {};
    for (const [key, value] of Object.entries(parsed)) {
      if (typeof value === "string" || typeof value === "number" || typeof value === "boolean") {
        headers[key] = encodeURIComponent(String(value));
      }
    }
    return headers;
  } catch (error) {
    console.warn(`[ADK] Security Guard headers unavailable: ${error.message}`);
    return {};
  }
}

async function initSecurityGuard() {
  const sg = getSecurityGuard();
  if (!sg || typeof sg.initUmid !== "function") return;
  if (!umidPromise) {
    umidPromise = Promise.resolve(sg.initUmid(6)).catch((error) => {
      if (!/errorCode=1|already initialized/i.test(error.message || "")) {
        console.warn(`[ADK] UMID initialization failed: ${error.message}`);
      }
    });
  }
  await umidPromise;
}

function parseImageUrl(url) {
  if (typeof url !== "string" || !url.trim()) return null;
  const match = url.match(/^data:([^;]+);base64,(.+)$/i);
  if (match) return { mimeType: match[1], data: match[2] };
  // The ADK wire format carries inline bytes, not a remote URL. Do not send a
  // URL as if it were base64: that produces an opaque upstream 400 response.
  throw new Error("image_url must use a data:<mime>;base64,... URL");
}

function extractText(content) {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content.filter((part) => part && part.type === "text").map((part) => part.text || "").join("");
}

function normalizeToolChoice(value) {
  if (value === undefined || value === null) return undefined;
  if (value === "required") return "any";
  if (value === "auto" || value === "none" || value === "any") return value;
  if (typeof value === "object" && value.type === "function" && value.function?.name) {
    return { type: "function", function: { name: value.function.name } };
  }
  return value;
}

function convertMessages(messages) {
  let systemInstruction = "";
  const contents = [];
  for (const message of messages) {
    if (message.role === "system") {
      const text = extractText(message.content);
      if (text) systemInstruction += (systemInstruction ? "\n\n" : "") + text;
      continue;
    }

    if (message.role === "user") {
      const parts = [];
      if (typeof message.content === "string") {
        parts.push({ text: message.content, thought: false });
      } else if (Array.isArray(message.content)) {
        for (const part of message.content) {
          if (part.type === "text") parts.push({ text: part.text || "", thought: false });
          if (part.type === "image_url" && part.image_url?.url) {
            const image = parseImageUrl(part.image_url.url);
            if (image) parts.push({ inlineData: image, thought: false });
          }
        }
      }
      contents.push({ role: "user", parts });
      continue;
    }

    if (message.role === "assistant") {
      const parts = [];
      const reasoning = message.reasoning_content || message.reasoningContent;
      if (reasoning) parts.push({ text: reasoning, thought: true });
      const text = extractText(message.content);
      if (text) parts.push({ text, thought: false });
      for (const toolCall of message.tool_calls || []) {
        const args = safeParseJson(toolCall.function?.arguments);
        parts.push({
          functionCall: {
            id: toolCall.id || "",
            name: toolCall.function?.name || "",
            argsJson: JSON.stringify(args),
          },
        });
      }
      if (parts.length) contents.push({ role: "model", parts });
      continue;
    }

    if (message.role === "tool") {
      contents.push({
        role: "user",
        parts: [{
          functionResponse: {
            id: message.tool_call_id || "",
            name: message.name || "",
            responseJson: JSON.stringify({ result: message.content || "" }),
          },
        }],
      });
    }
  }
  return { contents, systemInstruction };
}

function convertTools(tools, sdk) {
  return (tools || []).map((tool) => {
    const definition = {
      name: tool.function?.name || "",
      description: tool.function?.description || "",
      parameters: tool.function?.parameters || { type: "object", properties: {} },
    };
    if (typeof sdk.toToolDeclaration === "function") return sdk.toToolDeclaration(definition);
    return { name: definition.name, description: definition.description, parametersJson: JSON.stringify(definition.parameters) };
  });
}

function safeParseJson(value) {
  if (typeof value !== "string") return value || {};
  try { return JSON.parse(value); } catch { return { raw: value }; }
}

function getContent(frame) {
  return frame?.content || frame?.candidate?.content || frame?.payload?.content || null;
}

function getParts(frame) {
  const content = getContent(frame);
  return Array.isArray(content?.parts) ? content.parts : [];
}

function getFunctionCall(part) {
  return part?.functionCall || part?.function_call || null;
}

function getUsage(frame) {
  const usage = frame?.usageMetadata || frame?.usage_metadata || frame?.usage;
  if (!usage) return null;
  return {
    prompt_tokens: usage.promptTokenCount ?? usage.prompt_token_count ?? usage.inputTokens ?? usage.input_tokens ?? 0,
    completion_tokens: usage.candidatesTokenCount ?? usage.candidates_token_count ?? usage.outputTokens ?? usage.output_tokens ?? 0,
    total_tokens: usage.totalTokenCount ?? usage.total_token_count ?? usage.totalTokens ?? usage.total_tokens ?? 0,
  };
}

function mapFinishReason(reason) {
  return ({
    STOP: "stop",
    MAX_TOKENS: "length",
    SAFETY: "content_filter",
    BLOCKLIST: "content_filter",
    PROHIBITED_CONTENT: "content_filter",
    RECITATION: "content_filter",
    TOOL_CALL: "tool_calls",
    stop: "stop",
    length: "length",
    tool_calls: "tool_calls",
  })[reason] || "stop";
}

function buildRequest(body, sdk, token) {
  const { contents, systemInstruction } = convertMessages(body.messages);
  const requestId = `req-${Date.now()}-${crypto.randomBytes(4).toString("hex")}`;
  const request = {
    model: body.model,
    contents,
    systemInstruction,
    tools: convertTools(body.tools, sdk),
    properties: { normalized_response: "true" },
    token,
    requestId,
    userRequestId: requestId,
    messageId: `msg-${Date.now()}-${crypto.randomBytes(4).toString("hex")}`,
    conversationId: `conv-${crypto.randomBytes(8).toString("hex")}`,
    sessionKey: `sess-${crypto.randomBytes(8).toString("hex")}`,
    maxOutputTokens: body.max_tokens ?? 16384,
    incremental: Boolean(body.stream),
  };
  if (body.temperature !== undefined) request.temperature = body.temperature;
  if (body.top_p !== undefined) request.topP = body.top_p;
  if (body.stop) request.stopSequences = Array.isArray(body.stop) ? body.stop : [body.stop];
  if (body.tool_choice !== undefined) request.toolChoice = normalizeToolChoice(body.tool_choice);
  if (body.include_thoughts !== undefined) request.includeThoughts = Boolean(body.include_thoughts);
  if (body.thinking_budget !== undefined) request.thinkingBudget = body.thinking_budget;
  if (body.thinking_level !== undefined) request.thinkingLevel = body.thinking_level;
  if (body.reasoning_effort !== undefined) request.reasoningEffort = body.reasoning_effort;
  if (body.response_format !== undefined) request.responseFormat = body.response_format;
  if (body.generation_config !== undefined) request.generationConfig = body.generation_config;
  return request;
}

async function createClient(sdk, token) {
  sdk.setAuthToken(token);
  sdk.initParams({ utdid: getUtdid(), version: getAppVersion(), conversationName: "", appKey: process.env.PHOENIX_SECURITY_GUARD_APPKEY || "35336201" });
  await initSecurityGuard();
  return new sdk.AccioLlm({
    model: process.env.ACCIO_DEFAULT_MODEL || "1Nexus-R36W8qJ5vB6h",
    empid: process.env.ACCIO_ADK_EMPID || "",
    tenant: process.env.ACCIO_ADK_TENANT || "",
    gatewayBaseUrl: getGatewayBaseUrl(),
    transportInterceptor: {
      beforeRequest: async (request) => {
        request.headers["x-deploy-target"] = process.env.ACCIO_DEPLOY_TARGET || "desktop";
        request.headers["x-accio-route-region"] = process.env.ACCIO_ROUTE_REGION || "SG";
        request.headers["x-package-region"] = process.env.ACCIO_PACKAGE_REGION || "SG";
        Object.assign(request.headers, getSecurityHeaders(request.url));
        return request;
      },
    },
  });
}

function assertNoGatewayError(frames) {
  for (const frame of frames) {
    const payload = frame?.gatewayPayload;
    if (payload?.error_code || payload?.errorCode) {
      const code = payload.error_code || payload.errorCode;
      const message = payload.error_message || payload.errorMessage || "Gateway rejected the request";
      const error = new Error(`Gateway ${code}: ${message}`);
      if (/401|UNAUTHORIZED|TOKEN[_ -]?(EXPIRED|INVALID|EMPTY)|AUTH[_ -]?EXPIRED/i.test(`${code} ${message}`)) error.status = 401;
      error.code = code;
      throw error;
    }
    const envelope = frame?.gatewayEnvelope;
    if (envelope?.code && envelope.code >= 400) {
      const error = new Error(`Gateway ${envelope.code}: ${envelope.messageRaw || "Gateway rejected the request"}`);
      error.status = envelope.code;
      error.code = envelope.code;
      throw error;
    }
  }
}

async function complete(body, tokenOverride) {
  const sdk = getAdk();
  const token = getToken(tokenOverride);
  if (!sdk) throw new Error("ADK SDK is unavailable in extracted_new or extracted");
  if (!token) throw new Error("Authentication token is required");
  const client = await createClient(sdk, token);
  try {
    const request = buildRequest(body, sdk, token);
    const frames = [];
    for await (const frame of client.generateContentStream(request)) frames.push(frame);
    assertNoGatewayError(frames);
    return normalize(frames, body.model);
  } finally {
    await client.close?.();
  }
}

async function stream(body, write, tokenOverride) {
  const sdk = getAdk();
  const token = getToken(tokenOverride);
  if (!sdk) throw new Error("ADK SDK is unavailable in extracted_new or extracted");
  if (!token) throw new Error("Authentication token is required");
  const client = await createClient(sdk, token);
  try {
    const request = buildRequest(body, sdk, token);
    const frames = [];
    for await (const frame of client.generateContentStream(request)) {
      frames.push(frame);
      const normalized = normalize([frame], body.model);
      const message = normalized.choices[0].message;
      if (message.content) write({ role: "assistant", content: message.content });
      if (message.reasoning_content) write({ role: "assistant", reasoning_content: message.reasoning_content });
      if (message.tool_calls) {
        write({
          role: "assistant",
          tool_calls: message.tool_calls.map((toolCall, index) => ({ ...toolCall, index })),
        });
      }
    }
    return normalize(frames, body.model);
  } finally {
    await client.close?.();
  }
}

function normalize(frames, model) {
  let content = "";
  let reasoning = "";
  const toolCalls = [];
  let usage = null;
  let finishReason = "stop";
  for (const frame of frames) {
    for (const part of getParts(frame)) {
      if (part.text !== undefined && part.text !== null) {
        if (part.thought) reasoning += part.text;
        else content += part.text;
      }
      const call = getFunctionCall(part);
      if (call?.name) {
        const args = call.argsJson ?? call.args_json ?? call.args ?? {};
        const id = call.id || `call_${call.name}_${toolCalls.length}`;
        if (!toolCalls.some((item) => item.id === id)) {
          toolCalls.push({ id, type: "function", function: { name: call.name, arguments: typeof args === "string" ? args : JSON.stringify(args) } });
        }
      }
    }
    const rawError = frame?.error || frame?.gatewayError;
    if (rawError) {
      const errorMessage = typeof rawError === "string" ? rawError : rawError.message || rawError.errorMessage;
      if (errorMessage) throw new Error(`Gateway error: ${errorMessage}`);
    }
  }
  for (const frame of frames) {
    usage = getUsage(frame) || usage;
    const reason = frame.finishReason ?? frame.finish_reason;
    if (reason) finishReason = mapFinishReason(reason);
  }
  if (frames.some((frame) => frame.turnComplete || frame.turn_complete)) finishReason = toolCalls.length ? "tool_calls" : finishReason;
  const message = { role: "assistant", content: content || null };
  if (reasoning) message.reasoning_content = reasoning;
  if (toolCalls.length) message.tool_calls = toolCalls;
  return {
    id: `chatcmpl-${crypto.randomBytes(12).toString("hex")}`,
    object: "chat.completion",
    created: Math.floor(Date.now() / 1000),
    model,
    choices: [{ index: 0, message, finish_reason: finishReason }],
    usage: usage || { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
  };
}

function isAvailable() {
  return Boolean(getAdk());
}

// Kept non-enumerated from the HTTP surface; useful for deterministic local
// contract tests without exposing transport internals to callers.
module.exports = {
  complete,
  stream,
  isAvailable,
  __test: { convertMessages, convertTools, normalize, buildRequest, parseImageUrl, normalizeToolChoice },
};
