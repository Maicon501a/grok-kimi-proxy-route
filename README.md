# Grok Proxy kimi Router

<p align="center">
  <strong>Proxy desktop multi-rota</strong><br/>
  <b>Grok (xAI)</b> + <b>Kimi Work</b> + <b>Qwen</b> · multi-conta · streaming · API local <code>/v1</code> · SQLite
</p>

<p align="center">
  <a href="#o-que-é-isso">O que é</a> ·
  <a href="#provedores-multi-rota">Provedores</a> ·
  <a href="#início-rápido">Início rápido</a> ·
  <a href="#proxy-compatível-com-openai">Proxy OpenAI</a> ·
  <a href="#kimi-work">Kimi Work</a> ·
  <a href="#pesquisa-nativa-xai-v1search">Pesquisa xAI</a> ·
  <a href="#multi-conta--failover">Multi-conta</a> ·
  <a href="#auto-registro--sso">Auto-registro / SSO</a> ·
  <a href="#documentação">Docs</a> ·
  <a href="#compilar-do-código">Build</a> ·
  <a href="#releases">Releases</a> ·
  <a href="#aviso-legal">Aviso</a> ·
  <a href="#licença">Licença</a>
</p>

---

## O que é isso?

**Grok Proxy Plus** é um **app desktop** (Wails + Go) que virou um **hub multi-provedor**:

1. **Grok (xAI)** — login OAuth por device-code, pool multi-conta, API **`/v1/responses`**
2. **Kimi Work** — login Google no navegador do sistema (mesmo fluxo do Kimi Desktop), gera `sk-kimi`, multi-conta, API **`/v1/chat/completions`**
3. **Qwen (via QwenBridge)** — base URL + API key do bridge local; catálogo de models via probe; sem rotação no proxy (o bridge já rotaciona)
4. Um servidor local compatível com OpenAI (`http://127.0.0.1:8787/v1`) para Cursor, OpenCode, etc.
5. Chat na própria UI (streaming, thinking, tokens/custo)
6. Opcional: importar SSO do Grok e bot de auto-registro

> **Não é afiliado à xAI, Moonshot/Kimi nem Alibaba/Qwen.** Projeto comunitário não oficial. Use por sua conta e risco. Veja [DISCLAIMER.md](./DISCLAIMER.md) e [LICENSE](./LICENSE).

---

## Provedores (multi-rota)

| Provedor | Modo de auth | Como adicionar contas | API HTTP do proxy | Modelos (exemplos) |
|----------|--------------|------------------------|-------------------|--------------------|
| **Grok (xAI)** | **Auth** (pool de sessão) | OAuth device / SSO / auto-registro | **Padrão `POST /v1/chat/completions`** (também aceita `/v1/responses`) | `grok-4.5` |
| **Kimi Work** | **Auth** (pool de sessão) | **Login com Google** (navegador do sistema) → mint `sk-kimi` | **`POST /v1/chat/completions`** | `kimi-for-coding`, `k3-agent`, `k3-agent-{low,medium,high,xhigh}`, `k2d6-agent` |
| **Qwen** | **API key** (bridge) | UI → provider Qwen → base URL + API key do QwenBridge | **`POST /v1/chat/completions`** (+ conversão responses/messages) | Catálogo dinâmico via probe (`/v1/models` do bridge) |

### OpenCode Zen Free (nativo, sem terminal)

O Grok Proxy chama `https://opencode.ai/zen/v1` diretamente, injeta `Authorization: Bearer public` e remove o prefixo `opencode/` antes de enviar o model ao Zen. Basta selecionar **OpenCode Zen Free** na UI ou usar um model `opencode/*` no proxy local; não é necessário instalar ou manter `opencode serve` rodando.

Modelos free expostos: `opencode/deepseek-v4-flash-free`, `opencode/mimo-v2.5-free`, `opencode/nemotron-3-ultra-free`, `opencode/north-mini-code-free`, `opencode/ling-3.0-flash-free`, `opencode/laguna-s-2.1-free` e `opencode/big-pickle`.

### Failover WARP do Zen

Quando o Zen responde `500 Internal server error` no envelope do gateway, ou `429/502/503/504`, o proxy ativa o WARP automaticamente via `warp-cli` e repete a mesma request uma única vez pelo SOCKS5 local (`127.0.0.1:40000`). Falhas de rede também acionam o mesmo caminho. Erros de autenticação, modelo ou validação não são repetidos cegamente; streams que já começaram não são replayados.

O estado aparece em `GET /health` no campo `warp`. O padrão é ativo; pode ser desativado com `WARP_AUTO_FAILOVER=false`. Ajustes opcionais: `WARP_SOCKS_PORT`, `WARP_SOCKS_HOST`, `WARP_BIN_PATH`, `WARP_COOLDOWN` e `WARP_STARTUP_WAIT`.

### Regras de roteamento (v1.3+)

- O **modelo escolhido na UI do app** vale **somente no chat interno**. **Não** reescreve o `model` das requests HTTP (OpenCode/Cursor/SDK/Kilo).
- Clientes HTTP mandam o `model` que quiserem; o proxy **roteia o provedor pelo model** na mesma base URL (`grok-*` → xAI, `kimi-for-coding` / `k3-agent` → Kimi Work, models Qwen → bridge).
- `GET /v1/models` lista **todos os provedores** (independente do provider selecionado na UI).
- **`/v1/search`** só aceita models Grok (400 claro se o cliente mandar outro).
- **Grok** padrão: `/v1/chat/completions` (OpenCode/Kilo). `/v1/responses` continua opcional. `/v1/messages` (Anthropic) fala `/responses` com xAI + rotação + SSE.
- **Kimi** usa `/v1/chat/completions` (se mandar `/responses`, o proxy reescreve).
- **Qwen**: configure base URL (ex. `http://127.0.0.1:3000/v1`) + API key do bridge; sem rotação no lado do proxy.
- Contas: pool separado por provedor (login Grok e login Kimi no app; o proxy puxa o pool certo pelo model).

---

## Funcionalidades

| Recurso | Descrição |
|---------|-----------|
| **Multi-rota** | Grok + Kimi Work + Qwen no mesmo app / mesma porta local |
| **Sem Grok CLI** | OAuth device próprio + refresh de token |
| **Kimi Work (coding)** | Login Google → `CreateAPIKey(WORK)` → `sk-kimi` → `agent-gw.kimi.com/coding/v1` |
| **Kimi multi-conta** | Fila FIFO de relogin com dedupe, poller por conta, cura de morte parcial, cap atômico |
| **Qwen (bridge)** | Base URL + API key mascarada na UI; models via probe 60s; chat/completions + conversões |
| **Multi-conta (Auth)** | Pool separado por provedor; sidebar + modal **Ver contas** |
| **Persistência SQLite** | Contas, settings, usage e history em `grokdesktop.db` (JSON antigo migra 1x) |
| **Failover de cota** | Marca conta esgotada, pula e tenta de novo na mesma request (Grok); pin de provider no retry |
| **Erro de capacidade Kimi** | “Too many people chatting…” → falha rápida, **sem** rotacionar conta |
| **Streaming + thinking** | Raciocínio e resposta em tempo real; quota detectada dentro de SSE |
| **Pesquisa nativa xAI** | `web_search` / `x_search` (painel de pesquisa no chat) |
| **Logging estruturado** | `internal/logging` · `GROK_LOG_LEVEL` · máscara de segredos · `req_id` por request |
| **Estatísticas** | Tokens, latência e custo estimado (Grok 4.5 + Kimi K3/K2.6) |
| **Proxy local** | OpenAI chat/completions + responses (conforme provedor) · models · Anthropic messages · SSO |
| **Importar SSO** | Colar token, arquivo, pasta `sso-watch`, `POST /v1/sso` |
| **Auto-registro** | OAuth device + bot DrissionPage + e-mail temporário (Grok) |
| **Build multiplataforma** | Windows + Linux via GitHub Actions |

---

## Início rápido

### Opção A — baixar release

1. Abra [Releases](../../releases)
2. Baixe:
   - **Windows:** `GrokProxyPlus-windows-amd64.exe` (bot embutido) **ou** o `.zip` portátil (exe + `grok-signup-bot/`)
   - **Linux:** `GrokProxyPlus-linux-amd64` **ou** `.tar.gz`
3. Abra o app → **+ Adicionar conta** (Grok) ou **+ Conta Kimi** (Google)
4. Aponte o cliente para o proxy local (abaixo)

**WebView2 (Windows):** a UI precisa do [Microsoft Edge WebView2 Runtime](https://developer.microsoft.com/microsoft-edge/webview2/) (já vem na maioria do Win10/11).

**Auto-registro na release:**

| | Windows | Linux |
|--|---------|--------|
| Pacote | `.exe` solto (bot embutido) ou zip com `grok-signup-bot\` | binário solto ou tar.gz |
| Python | Python 3 do python.org, **Add to PATH** | `python3` + venv |
| Dependências | **auto** `venv` + pip no AppData no primeiro registro | idem |
| Navegador | Chrome ou Edge | Chrome/Chromium |

Sem Python, **login device + import SSO** do Grok continuam funcionando. SmartScreen pode avisar em builds não assinados — “Mais info → Executar mesmo assim”.

### Opção B — rodar do código

**Requisitos:** Go 1.24+ (preferível 1.25), Node 20+, [Wails v2](https://wails.io/), (Linux: pacotes GTK/WebKit)

```bash
git clone https://github.com/Maicon501a/grok-proxy-plus.git
cd grok-proxy-plus

# instalar Wails uma vez
go install github.com/wailsapp/wails/v2/cmd/wails@v2.12.0

# desenvolvimento
wails dev

# build de produção
wails build
```

Saída: `build/bin/` (ex.: `GrokDesktop.exe` no Windows).

**Auto-registro opcional (árvore de dev):**

```bash
# Linux / macOS
python3 -m venv .venv
.venv/bin/pip install -r grok-signup-bot/requirements.txt

# Windows (PowerShell)
python -m venv .venv
.\.venv\Scripts\pip install -r grok-signup-bot\requirements.txt
```

---

## Proxy compatível com OpenAI

Com o app aberto e uma conta ativa no **provedor selecionado**, o servidor local escuta em:

```text
http://127.0.0.1:8787/v1
```

(Se `8787` estiver ocupada, tenta **`8788`**.)

| Configuração | Valor |
|--------------|--------|
| **Base URL** | `http://127.0.0.1:8787/v1` |
| **API key** | qualquer string (ou a key opcional definida no app) |
| **Roteamento** | **pelo `model` do cliente** (mesma base URL lista Grok + Kimi) |

`GET /v1/models` devolve **Grok e Kimi juntos**. Não precisa trocar “provedor ativo” no app para o proxy HTTP.

### Grok (model `grok-*`)

| Item | Valor |
|------|--------|
| Endpoint padrão | **`POST /v1/chat/completions`** (OpenCode/Kilo) |
| Endpoint opcional | **`POST /v1/responses`** (formato nativo xAI) |
| Modelo | `grok-4.5` |

```bash
# Padrão (chat/completions) — o proxy traduz internamente para a xAI
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer local" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "grok-4.5",
    "stream": true,
    "messages": [{"role":"user","content":"Olá"}]
  }'

# Opcional: Responses nativo
curl http://127.0.0.1:8787/v1/responses \
  -H "Authorization: Bearer local" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "grok-4.5",
    "stream": true,
    "input": "Olá"
  }'
```

### Kimi Work (model `kimi-for-coding` / `k3-agent` / …)

| Item | Valor |
|------|--------|
| Endpoint | **`POST /v1/chat/completions`** (`/responses` retorna 400) |
| Modelos | `kimi-for-coding` (id de fio), aliases `k3-agent`, `k3-agent-{low,medium,high,xhigh}`, `k2d6-agent` |
| Tools | OpenAI nativo: `tools` / `tool_calls` |

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer kimi-work" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "kimi-for-coding",
    "stream": false,
    "messages": [{"role":"user","content":"Olá"}]
  }'
```

### Exemplo — OpenCode / Kilo (mesma baseURL, sem trocar provedor no app)

Escolha o **model** no cliente: `grok-4.5` → Grok · `kimi-for-coding` → Kimi.

```json
{
  "provider": {
    "grok-proxy-plus": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Grok Proxy Plus",
      "options": {
        "baseURL": "http://127.0.0.1:8787/v1",
        "apiKey": "local"
      },
      "models": {
        "grok-4.5": { "name": "Grok 4.5 (Responses)" },
        "kimi-for-coding": { "name": "Kimi For Coding" },
        "k3-agent": { "name": "K3 Max (Work)" },
        "k3-agent": { "name": "K3 Max (Work)" },
        "k3-agent-low": { "name": "K3 Max Low Think" },
        "k3-agent-medium": { "name": "K3 Max Medium Think" },
        "k3-agent-high": { "name": "K3 Max High Think" },
        "k3-agent-xhigh": { "name": "K3 Max Extra High Think" }
      }
    }
  }
}
```

### Superfície da API

| Endpoint | Grok | Kimi Work | Notas |
|----------|------|-----------|--------|
| `/v1/models` | ✓ | ✓ | Catálogo unificado (Grok + Kimi na mesma base URL) |
| `/v1/chat/completions` | ✓ (padrão) | ✓ | OpenCode/Kilo usam este |
| `/v1/responses` | ✓ (opcional) | reescreve → chat | Grok nativo xAI |
| `/v1/messages` | ✓* | — | Formato Anthropic (caminho Grok) |
| `/v1/search` | ✓ | — | **Pesquisa nativa xAI** (`web_search` + `x_search`) |
| `POST /v1/sso` | ✓ | — | Importar SSO Grok |

\*Melhor esforço; para Grok prefira Responses quando o cliente permitir.

Em rate-limit do Grok (429/402 free-usage-exhausted) o proxy pode marcar a conta como esgotada, trocar de conta e **repetir a mesma request**.  
No Kimi, “Too many people are chatting…” vira **503 `kimi_server_busy`** e **não** rotaciona conta.

---

## Pesquisa nativa xAI (`/v1/search`)

Com o **provedor ativo = Grok (xAI)**, o proxy expõe uma rota de pesquisa que usa as **tools nativas da xAI** (não é scraper de terceiros):

```text
POST http://127.0.0.1:8787/v1/search
POST http://127.0.0.1:8787/v1/web_search
POST http://127.0.0.1:8787/v1/x_search
```

Por baixo dos panos roda um turno curto de **Responses** só com:

| Tool | O que pesquisa |
|------|----------------|
| `web_search` | Web (nativo xAI) |
| `x_search` | X / Twitter (nativo xAI) |

Exemplo:

```bash
curl http://127.0.0.1:8787/v1/search \
  -H "Authorization: Bearer grok-desktop" \
  -H "Content-Type: application/json" \
  -d '{
    "query": "últimas notícias do Grok 4.5",
    "mode": "web"
  }'
```

- `mode`: `web` / `web_search` · `x` / `x_search` · omitir ou `both` = web + X  
- Exige **conta Grok ativa** (Kimi e outros provedores retornam erro nessa rota)  
- No chat do app, eventos de pesquisa nativa também aparecem no painel de pesquisa quando o modelo chama essas tools em `/v1/responses`

---

## Kimi Work

Kimi Work é o produto de **coding/agent** da Moonshot (modo Work do Desktop), **não** o chat web consumer com JWT solto.

### Fluxo de auth (mesma ideia do app oficial)

```text
Navegador do sistema → Google OAuth (loopback 127.0.0.1:61120+)
  → POST https://www.kimi.com/api/auth/login/google  { code: <google id_token> }
  → access_token + refresh_token
  → Connect CreateAPIKey(scope=WORK) → sk-kimi-…
  → Upstream: https://agent-gw.kimi.com/coding/v1
```

No app: **Provedor → Kimi Work · Auth** → **+ Conta Kimi** → **Login com Google**.

- Multi-conta = vários usuários Kimi (cada login Google → uma entrada `sk-kimi` no pool).
- **Não** precisa do Kimi Desktop instalado.
- O id de fio no agent-gw costuma ser **`kimi-for-coding`** (SKU coding da família K3). Labels `k3-agent` e variantes (`-low`, `-medium`, `-high`, `-xhigh`) são aliases que definem o reasoning effort automaticamente.

### Preço estimado (lista da platform)

Usado nas estatísticas do app (USD / 1M tokens):

| Família | Cache hit | Input (miss) | Output |
|---------|----------:|-------------:|-------:|
| Kimi K3 / `kimi-for-coding` | $0.30 | $3.00 | $15.00 |
| Kimi K2.6 / `k2d6-agent` | $0.16 | $0.95 | $4.00 |

Fonte: [pricing Kimi K3](https://platform.kimi.ai/docs/pricing/chat-k3). Conta consumer/assinatura pode diferir da tabela da API platform.

---

## Multi-conta & failover

### Grok (xAI)

- **+ Conta Grok** → login device / SSO / auto-registro
- Contas esgotadas: badge + skip + auto-registro opcional
- Plano de failover: [plan/executed/account-exhaustion-plan.md](./plan/executed/account-exhaustion-plan.md)

### Kimi Work

- **+ Conta Kimi** → login Google no navegador (caminho principal)
- Pool por usuário (`sk-kimi`); erro de capacidade é do servidor (sem rotacionar)

### UX compartilhada

- Troque o **provedor** em Global
- **Ver contas** na UI lista o pool do provedor selecionado no app; o proxy HTTP usa o pool certo pelo `model`
- A conta ativa alimenta o chat da UI **e** o proxy local daquele provedor

### Onde ficam os dados (não vai pro git)

| SO | Caminho |
|----|---------|
| Windows | `%LOCALAPPDATA%\GrokDesktop\` |
| macOS | `~/Library/Application Support/GrokDesktop/` |
| Linux | `~/.local/share/GrokDesktop/` |

```text
GrokDesktop/
├── grokdesktop.db        # SQLite: contas, settings, usage, history
├── settings.json         # dual-write legado
├── usage.json / history.json
├── accounts/<id>.json    # backup dual-write legado
├── signup-bot/<ver>/
├── python-venv/
├── skills/
├── mcp_servers.json
├── sso-watch/*.txt
└── logs/
```

**Importante:** desinstalar só o `.exe` **não apaga** o AppData. Apagar a pasta `GrokDesktop` ou formatar o PC **apaga** as contas. Faça backup de `grokdesktop.db` (ou da pasta inteira).

---

## Auto-registro & SSO

### SSO (Grok)

| Método | Como |
|--------|------|
| Colar na UI | Importar SSO |
| Arquivo | linhas `email:senha:SSO` ou token cru |
| Pasta watch | `AppData/sso-watch/*.txt` a cada 30s |
| HTTP | `POST /v1/sso` (protegido pela API key do proxy, se houver) |

### Auto-registro (opcional, experimental)

Fluxo: **Device OAuth** → bot Python (`grok-signup-bot/`, **DrissionPage**) → **PollDevice** → conta salva.

| UI / backend | Comportamento |
|--------------|---------------|
| **+ Gerar contas** | Lote 1–5 (teto 5 ativas) |
| `autoRegisterLoop` | A cada 5 min se ativas &lt; mínimo (**opt-in**: `auto_register_enabled`, default **false**) |
| E-mail | DuckMail → Mail.tm |
| Plano | [plan/executed/auto-register-plan-v1.md](./plan/executed/auto-register-plan-v1.md) |

**Riscos:** ToS, ban, automação. Para uso pessoal, prefira login device manual.  
Binários de release **embutem** o bot e extraem no AppData; no primeiro auto-registro criam **`python-venv`** e instalam deps. Ainda precisa de **Python 3 no host** e **Chrome/Edge**.

---

## Documentação

| Doc | Conteúdo |
|------|----------|
| [plan/executed/](./plan/executed/) | Planos **concluídos** (exhaustion, auto-registro, FINDINGS) |
| [plan/executed/hardening-plan-v1.md](./plan/executed/hardening-plan-v1.md) | Hardening A–D (feito) |
| [plan/executed/account-exhaustion-plan.md](./plan/executed/account-exhaustion-plan.md) | Failover + usage (feito) |
| [plan/executed/auto-register-plan-v1.md](./plan/executed/auto-register-plan-v1.md) | Auto-registro (feito) |
| [docs/grok-register-analysis.md](./docs/grok-register-analysis.md) | Guia SSO + análise grok-register |
| [grok-signup-bot/README.md](./grok-signup-bot/README.md) | Setup do bot Python |

---

## Compilar do código

```bash
wails build
wails build -platform windows/amd64
wails build -platform linux/amd64
```

### Dependências Linux (Debian/Ubuntu)

```bash
sudo apt-get install -y \
  libgtk-3-dev libwebkit2gtk-4.1-dev \
  libayatana-appindicator3-dev librsvg2-dev \
  gcc pkg-config
```

### Self-test (sem GUI)

```bash
go run ./cmd/selftest
```

---

## Releases

| Gatilho | Workflow |
|---------|----------|
| Push / PR em `main` | [CI](./.github/workflows/ci.yml) |
| Push de tag `v*.*.*` | [Release](./.github/workflows/release.yml) |

```bash
git tag v1.3.0
git push origin v1.3.0
```

Artefatos: `GrokProxyPlus-windows-amd64.exe`, `…exe.zip`, `GrokProxyPlus-linux-amd64`, `…tar.gz`

---

## Estrutura do projeto

```text
.
├── main.go / app.go
├── internal/
│   ├── oauth/          # Grok OAuth
│   ├── kimi/           # login Google + mint sk-kimi
│   ├── store/          # SQLite + settings + accounts
│   ├── upstream/       # clientes HTTP Grok/Kimi
│   ├── proxyhttp/      # servidor local /v1
│   ├── pricing/        # custos estimados
│   ├── register/       # auto-registro embutido
│   ├── skills/
│   └── mcpconfig/
├── grok-signup-bot/
├── frontend/
├── docs/
├── plan/executed/
├── .github/workflows/
├── LICENSE
├── DISCLAIMER.md
└── README.md
```

---

## Idioma da UI

Os textos da UI desktop são **português (pt-BR)** de propósito. Este README também está em português.

## Segurança

- **Tokens não vão pro repositório git** — só no AppData da máquina  
- Contas/tokens ficam em **`grokdesktop.db`** (e dual-write JSON legado)  
- Client OAuth do Grok no código é o **client público** do CLI xAI (PKCE)  
- Client OAuth Google do Kimi é o **client de app do Desktop** (compartilhado; não é senha da sua conta)  
- Não commite `accounts/`, `*.db`, `*.env` nem binários “sujos”  
- Trate o proxy como **somente localhost**  
- API key vazia no proxy deixa `/v1/sso` aberto no localhost  
- Auto-registro pode gravar senhas em texto em `auto_creds.json`  

---

## Aviso legal

**Use por sua conta e risco.** Os autores **não se responsabilizam** por ban, cobrança, perda de dados, violação de ToS ou qualquer dano.  
Isto **não** é produto oficial da xAI nem da Moonshot. Texto completo: [DISCLAIMER.md](./DISCLAIMER.md).

---

## Licença

**MIT (Non-Commercial)** — livre para uso pessoal / não comercial.  
**Sem uso comercial** sem permissão por escrito.  
Termos: [LICENSE](./LICENSE).
