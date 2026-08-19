# WAF Accio — Engenharia Reversa (sessão 2026-08-03)

> Estado: ABI confirmada, dispatch runtime mapeado, inicialização da tabela de
> transformação recuperada e round-trip do buffer final confirmado em 102/102
> bytes. Próximo alvo: recuperar a montagem do staging binário por request.

---

## 1. Visão geral da pilha WAF

O Accio (desktop app 0.26.0, beta) protege a API por trás de um gateway Alibaba
"phoenix" com camadas em série:

```
Cliente (Go proxy / Node SDK)
  │
  ├─ TLS spoofed: utls HelloChrome_120 + HTTP/2  (JA3/JA4 = Chrome 120)
  ├─ URL:  /api/adk/llm/generateContent?sg_k=<md5>
  ├─ Headers pctb-x-* (11) gerados pelo SecurityGuard SDK nativo
  ├─ Headers de contexto: utdid, version, appKey, x-deploy-target,
  │   x-accio-route-region, x-package-region, User-Agent: node
  └─ Body: protobuf-JSON com token no campo "token" (SEM Authorization header)
        │
        ▼
phoenix-gw.alibaba.com  (WAF estadoful: correlação cookie2/cna/UA/fingerprint)
```

### Endpoints relevantes

| Constante (client.go) | URL |
|---|---|
| GatewayBase | `https://phoenix-gw.alibaba.com/api` |
| GatewayLLM | `.../api/adk/llm` (POST + `/generateContent?sg_k=...`) |
| RefreshURL | `.../api/auth/safe/refresh_token` |
| TokenExchangeURL | `.../api/oauth/token` |
| modelCatalogURL | `.../api/llm/config/v2` |
| modelRoutingURL | `.../api/tool/rlab/call` |
| LoginBase | `https://www.accio.com` |

Domínios (`.accio/domain-config.json`, region=GLOBAL):
`filebroker.accio.com`, `www.accio.com`, `work-download.accio.com` (file/client/web/infra).

---

## 2. Inventário dos 11 headers (dump empírico AO VIVO, addon nativo)

Chamada: `getSecurityFactorsForWeb('{"appkey":"35336201","urlInput":"<url>"}')`
→ retorna JSON → cada valor passa por `encodeURIComponent`.

| # | Header | Valor (exemplo real, 2026-08-03) | Comportamento |
|---|---|---|---|
| 1 | `pctb-x-umt` | `P1gAm_XDTwXUmu0igP82tS00accS5Le0gERTOZX_q46VE2Pwc39J3Vd1KLwX-Gn1Z2lbccDUgQgn6KhjpYy6H4uj` (104 chars) | **Estável** por sessão = UMID token (mesmo valor em todas as URLs; muda se reiniciar o init) |
| 2 | `pctb-x-mini-wua` | `Aj1ZuseMtJ8FHU+5kgM/9UQrdOWXZ2pGliZrPZJxFmJz1IiDjcEUnPCVXE4UDvXV...` (~500 chars, base64) | **Muda por URL** — bound ao api/sign (mini-WUA: token de fingerprint de dispositivo) |
| 3 | `pctb-x-sgext` | `win_reserve` | Fixo |
| 4 | `pctb-x-sign` | `wzpCVE002xAAL6Tp55Xr0S5zbvx0/6TvrWeZadRUyJUX3o7nHXolcrv2zy5k2KKEEZrxv1HmBoHfXZC7J67g6/Vb91+k/6TvpP+k76` | **Muda por URL** — marcador textual `wzpCVE002xAA` constante; payload Base64 de 67 bytes; cauda com bloco de 4 bytes repetido |
| 5 | `pctb-x-pv` | `6.3` | Versão do protocolo (fixa) |
| 6 | `pctb-x-api` | `phoenix-gw.alibaba.com/api/adk/llm/generateContent` **ou** `/api/tool/featureFlag/evaluate` (path-only) | Varia: host+path nos endpoints de gateway "conhecidos", path-only em outros |
| 7 | `pctb-x-apiver` | `1.0` | Fixo |
| 8 | `pctb-x-t` | `1785780213` | Unix timestamp (segundos) |
| 9 | `pctb-x-appkey` | `35336201` (Windows) / `35337600` (macOS) | appkey da aplicação |
| 10 | `x-bx-env` | `web` | Ambiente |
| 11 | `pctb-x-bx-version` | `pc_1.0.70.3061_0` | versão do "bx" (biz?) — vem do SDK, não do app |

Observações do dump:
- `pctb-x-sign` e `pctb-x-mini-wua` são os campos dinâmicos críticos (bound à URL).
- `pctb-x-umt` é persistente no processo (token UMID; `getSecurityToken(6)` retorna o mesmo).
- `pctb-x-api` dentro do `www.accio.com` → path-only (`/api/user/login`).

---

## 3. Fórmulas e semânticas conhecidas

### 3.1 `sg_k` (query param)

```
sg_k = md5_hex("req-" + Date.now())
```
Usado em: `/api/adk/llm/generateContent`, `/api/rc/pc/token`, `/api/account/switch/renew`,
`mobileBindSendCode`. (extraído do bundle main do app: `createHash("md5").update("req-"+Date.now()).digest("hex")`)

### 3.2 UMID init

- `initUmid(env)` com `env=6` = `UMID_ENV_AREA_ONLINE` (regional, resolvido por DNS pelo SDK).
- Env map: 0=online, 1=pre, 2=daily, 3=US, 4=SG, 5=KR, 6=area, 7=unset.
- `errorCode=1` = "já inicializado" (cross-process) → tratar como sucesso.
- Token via `getSecurityToken(env)`; wrapper `getUmidToken(env)` cacheia o promise por env.

### 3.3 Appkeys

- Windows: `35336201` (const `DefaultSecurityAppKey` no client.go; `ACCIO_SG_APPKEY` no daemon).
- macOS: `35337600`.
- Override: env `PHOENIX_SECURITY_GUARD_APPKEY`.

### 3.4 Input do addon por plataforma

- **Windows**: `{"appkey": <k>, "urlInput": <url completa>}`
- **macOS/não-Windows**: `{"appkey": <k>, "api": <hostname+path>, "data": md5(path + ?query)}`

---

## 4. Camada WAF estadoful (lições empíricas do client.go)

O gateway NÃO confia só nos headers pctb — correlaciona contexto HTTP:

- **Cookie `cookie2=<accessToken>`** é OBRIGATÓRIO no chat POST. Remover → WAF quebra correlação → captcha.
- **`x-cna` deve ser vazio** (`""`) quando o cookie não tem `cna=`. Forjar UUID → lockout para contas novas.
- **Sem `Authorization: Bearer`** no chat: o token vive só no body JSON. Header de auth diferente → WAF classifica outro contexto.
- **`User-Agent: node`** (SDK roda em Node/Electron; fetch manda "node"). UA "Accio/x" ou Electron chutado → punido como bot.
- Headers de interceptor do httpClient (Cookie, x-source, x-os, x-utdid, x-cna, Electron UA) **NÃO** devem ir no /generateContent → o WAF pune com página `sufei-punish` captcha.
- **Baxia challenge**: quando o fingerprint do device ainda não foi visto pela camada estadoful, o gateway responde **HTML challenge**. Solução atual: logar uma vez no desktop app para registrar o fingerprint. (texto do erro em client.go:1832)
- **Rota local**: se o desktop Accio estiver rodando, o proxy usa o gateway local (`localhost:4097`, basic auth `phoenix:<token>`) — bypass total do WAF.

---

## 5. Assets nativos (inventário p/ RE profunda)

### 5.1 security-guard (extraído no workspace)

```
internal/accio/security-guard/
├─ sg_daemon.js                    ← daemon JSON-lines (Go ↔ Node)
├─ prebuild/win32-x64/security_guard.node   (168 KB, addon N-API)
└─ resources/x64/
   ├─ SecurityGuardSDK64.dll       (7.3 MB) ← SDK principal (sign, wua, umid)
   ├─ ThreatSieveSDK64.dll         (3.6 MB) ← threat detection / risk
   ├─ AliSafeProxy.dll             (2.5 MB)
   ├─ AliSafePath64.dll            (2.4 MB)
   ├─ NetCore.dll                  (2.5 MB)
   ├─ Report.dll                   (524 KB)
   ├─ vcruntime140.dll / _1.dll
   └─ ps/                          (~120 arquivos; 92 KB, 201 KB e 97 B)
```

### 5.2 app instalado (referência viva)

`%LOCALAPPDATA%\Programs\Accio\resources\`
- `app.asar.unpacked\node_modules\@phoenix-common\security-guard\` (versão 0.5.4) — dist/*.js lido (index, init-umid, get-umid-token, get-security-factors, native-addon)
- `ps/` — 13 arquivos 92.854 B (subconjunto dos ~120 do bundle do workspace)
- `ut/win/x64/` — UTDID.dll (3.5 MB), UTForPC.dll (5.5 MB), wtnet.dll → telemetria + geração do UTDID
- `pre-install/` — node.exe bundled (fallback p/ daemon)

### 5.3 Arquivos `ps/` — formato

- Tamanhos vistos: 92.854 B (maioria), 201.332 B, 212.706 B, 97 B, 82 B.
- Conteúdo: **bytes pseudo-aleatórios** (sem header mágico visível; entropia alta).
- Hipótese: scripts/plugins criptografados (provavelmente AES com chave embutida no SDK) carregados pelo SecurityGuardSDK64.dll — o `ps` = "plugin script" / "payload script".
- Os de 97 B: cabeçalho/índice curto (provável manifesto). Os de 92 KB: payloads de risco/threat-sieve.

---

## 6. Pontos de integração no proxy (código atual)

| Arquivo | Papel |
|---|---|
| `internal/accio/security_bridge.go` | Daemon Node sidecar persistente (JSON-lines), embutido via `//go:embed` |
| `internal/accio/security_bridge.mjs` | Bridge alternativo (importa dist/index.js do package instalado) |
| `internal/accio/tls_transport.go` | utls Chrome_120 + h2 (http2.Transport + DialTLSContext) |
| `internal/accio/client.go` | `chatWithToken` → `securityHeaders(ctx, endpoint)` → merge nos headers; `applyChatHeaders` |
| `internal/accio/security_process_windows.go` | `HideWindow` no sidecar |

Daemon protocolo:
```
→ {"id":"sg-<ts>-<n>","url":"https://..."}
← {"id":"...","headers":{"pctb-x-sign":"...",...}}
← {"ready":true}   (no boot, após initUmid)
```

---

## 7. Próximas frentes de RE

1. **Tabela de transformação** — inicialização, seed, LCG e aplicação XOR recuperados; falta ligar a tabela à montagem do staging binário por request.
2. **`pctb-x-mini-wua`** — separar a cadeia WUA da cadeia do sign e mapear o binding ao device/URL.
3. **Arquivos `ps/`** — identificar o decoder e relacioná-lo às tabelas carregadas pelo SDK.
4. **`ThreatSieveSDK64.dll`** — mapear as fontes de fingerprint que alimentam o estado nativo.
5. **Reimplementação pura em Go** — o gerador da tabela já pode ser reproduzido; a cadeia completa de headers ainda depende do estado nativo.

### 7.1. Binding Windows — caminho confirmado no `.node`

Disassembly do `security_guard.node` (N-API, versão Windows):

1. Resolve `AliSafePath64.dll` / `AliSafeProxy.dll` com `LoadLibraryW`.
2. Resolve o export `QueryInterface`.
3. Solicita a interface **`0x129`**, armazenada no global usado pela operação
   `getSecurityFactorsForWeb`; o texto do binding identifica essa interface como
   `ISecurityFactorProxy`.
4. Testa o primeiro slot da vtable (`ISecurityFactorProxy::Start`).
5. Chama o slot `+0x40` com `(this, inputParamJson, &statusCode)` — o JSON
   Windows `{appkey, urlInput}` segue bruto até o SDK. `statusCode=0` indica sucesso.
6. O retorno é um `char*` NUL-terminated contendo o JSON dos fatores; o wrapper mede
   o comprimento, converte para string JS e libera-o via o slot `+0x18`.

O probe isolado (`sg_abi_probe.cpp`) confirma o ID e os slots, mas não substitui o
bootstrap completo do módulo N-API: carregamento direto das DLLs retorna comprimento
zero, enquanto a sequência real do addon inicializa UMID/estado antes da chamada.

Consequência: não existe transformação, MD5 de URL ou cifra implementada no wrapper
JS/Node que explique `pctb-x-sign`. A lógica de geração está dentro do SDK carregado
por `AliSafeProxy.dll`/`SecurityGuardSDK64.dll`.

Offsets runtime confirmados no Windows:

- `AliSafeProxy.dll+0x95790` é o adapter externo `(this, JSON, &status)`; ele lê o
  objeto interno em `[this+8]` e salta para `inner_vtable+0x40`.
- O slot interno observado aponta para `SecurityGuardSDK64.dll+0xe1130`.
- A referência antiga a `AliSafeProxy.dll+0x8f790` está obsoleta e não deve ser usada.

### 7.2. Estrutura observada do `pctb-x-sign`

Capturas combinadas (`sg_dump.jsonl` + `sg_fine.jsonl`, 47 chamadas):

- marcador textual sempre com **12 caracteres**: `wzpCVE002xAA`;
- string completa sempre com **102 caracteres**;
- payload `sign.slice(12)` sempre com 90 caracteres Base64, decodificando para **67 bytes**;
- o Base64 completo do sign decodifica para **76 bytes**; os 9 bytes iniciais são
  estáveis no corpus: `c3 3a 42 54 4d 34 db 10 00`;
- bytes finais do payload sempre repetem o mesmo bloco de 4 bytes duas vezes
  (`X Y X Z X Y X Z`);
- em indexação zero, as 47 amostras obedecem:
  `payload[1] = payload[13] = payload[59] = payload[61]` e
  `payload[14] = payload[62]`;
- `payload[12] ^ payload[60]` e `payload[12] ^ payload[14]` sempre têm os
  quatro bits baixos zerados; os valores não são necessariamente iguais;
- uma mutação de um caractere da URL altera praticamente todo o body;
- chamadas espaçadas por 1,1 s também alteram todo o body;
- processos Node separados produzem bodies diferentes mesmo com o mesmo URL, enquanto
  o `pctb-x-umt` permanece igual.

O padrão de cauda **não prova ECB ou XOR periódico**: o body não tem repetição de
4 bytes fora da cauda e a avalanche é incompatível com essas hipóteses simples. O
modelo atual é um payload proprietário com estado/nonce interno, request binding e um
campo final estruturado/repetido. O contrato runtime já foi observado; a lacuna agora
é a montagem do staging binário por request, documentada nas seções 7.3.2 e 7.3.3.

### 7.3. Dispatch runtime e transformação XOR observada

O tracer `sg_trace.cpp` confirmou a cadeia abaixo em código runtime desempaquetado:

```
+0x112a10
  └─ alvo indireto +0x111c20
      └─ +0x1bb4e0 / +0x1bb5f0 / +0x1bbe70
          ├─ +0x2648a0 → alvo local +0x264924 → tabela[0x18] → +0x261040
          └─ +0x264960 → push/ret +0x2649d5 → tabela[0x20] → +0x2651e0
```

- Em `+0x2648a0`, os pontos de dispatch são `+0x71` (`call r15`) e `+0xa1`
  (`call [rax+rsi*8]`).
- Em `+0x264960`, são `+0x66` (`push rax; ret`) e `+0x86`
  (`call [r9+rax*8]`).
- `+0x2615d7 → +0x2615ea` é continuação local do state machine, não uma primitiva
  externa.
- `+0x261040` e `+0x2651e0` têm o mesmo loop de máscara de dois bytes. O código
  usa uma tabela por processo em `tableData+0xa0` e faz, para `off=0,2,4...`:

  ```text
  dst[off]   ^= table[(seed + off)     & 0xff]
  dst[off+1] ^= table[(seed + (off|1)) & 0xff]
  ```

- `seed` observado é o byte baixo do ponteiro-base do buffer (`[rsp+0x38]`);
  `dst` avança dois bytes e o contador restante diminui dois.
- A tabela tem 256 bytes, valores observados entre `0x00` e `0x7f`, não é uma
  permutação S-box clássica e muda entre processos Node. É idêntica entre
  `+0x261040` e `+0x2651e0` dentro do mesmo processo.
- Em três chamadas consecutivas no mesmo processo, `tableSlot`, `tableData` e a
  tabela de 256 bytes permaneceram no mesmo endereço/conteúdo; o `pctb-x-sign`
  mudou nas três chamadas. Portanto, a variação do sign vem do buffer/estado de
  cada request, não de uma regeneração da tabela a cada chamada.
- O run bidirecional com `r8=0x66` fechou o papel do loop no sign ASCII:
  `+0x261040` recebeu um staging binário de 102 bytes e produziu, in-place, o
  `pctb-x-sign` ASCII de 102 bytes; logo depois, `+0x2651e0` recebeu exatamente
  esse ASCII e devolveu o staging binário byte a byte (`102/102`). Como a operação
  é XOR, os dois caminhos são o mesmo transformador auto-inverso, não duas cifras.
- A sequência observada ficou:

  ```text
  staging binário (102)
    └─ +0x2648a0 → tabela[0x18] → +0x261040
        └─ sign ASCII (102)
            └─ +0x264960 → tabela[0x20] → +0x2651e0
                └─ staging binário (102)
  ```

  O `sign-extract` lê uma cópia/material ASCII separada depois dessa validação.
  Portanto, a parte ainda desconhecida não é a tabela nem o encode ASCII: é a
  montagem dos 102 bytes binários antes de `+0x261040`.
- `+0xe2b46` ainda confirma o resultado final: `EAX=1`, comprimento `0x66` e
  material ASCII de 102 bytes antes de `+0x30ae0` inserir `pctb-x-sign`.

Os dumps de tabela são produzidos por `SG_SBOX_DUMP`/`SG_TABLE_DUMP`; os dumps de
runtime dos operadores usam `SG_OP_DUMP`/`SG_OP_TARGET_DUMP`. A geração da tabela por
processo está documentada abaixo.

### 7.3.2. Estrutura de entrada do gerador

Em `+0x112a10`, `RDX` aponta para uma estrutura nativa de pelo menos 512 bytes;
ela não é o payload final. A captura focada `sg_trace_table28_two.log` mostrou,
para duas URLs idênticas no mesmo processo:

- `+0x00`: ponteiro + comprimento de um bloco grande de estado/fingerprint;
- `+0x28`: ponteiro + comprimento de um bloco binário de aproximadamente
  `0x614`–`0x660` bytes;
- `+0x68` e `+0xc8`: palavras de estado opacas; `+0xc8` mudou entre as duas
  chamadas;
- `+0x88`: ponteiro para um buffer fixo de **0x61 (97) bytes**, igual nas duas
  chamadas observadas;
- `+0xd0` em diante: armazenamento direto ou indireto do material UMID
  (`pctb-x-umt`), dependendo da forma materializada pelo runtime;
- offsets altos: callbacks e metadados do objeto nativo.

O gerador retornou `RAX=1` nas duas chamadas. Mesmo com os buffers persistentes
iguais, os dois payloads finais diferiram em 67/67 bytes e 272 bits. Portanto,
`+0x112a10` consome estado adicional por request; ele não simplesmente cifra o
buffer fixo de 97 bytes.

O estado adicional apareceu numa chamada de `+0x2651e0` com `r8=0x14`: um bloco
de 20 bytes (`0x10` + 16 bytes variáveis) é processado por `+0x2654e0` em dez
pares XOR, in-place. No run focado, o destino começou no próprio ponteiro do
bloco e o byte baixo do endereço-base foi `0x60`; a saída foi então encaminhada
ao operador `+0x264960` em `R9`. As duas chamadas iguais produziram blocos
centrais diferentes, confirmando que esse é um dos materiais dinâmicos do
request, antes da montagem do sign ASCII final.

Na mesma rota, o prefixo de comprimento `0x10` foi alterado para `0x0c` antes
do loop XOR; depois da transformação, os 20 bytes completos — inclusive os
quatro bytes iniciais — aparecem em `R9` no operador. O bloco é, portanto, um
registro dinâmico intermediário, não os 67 bytes finais do payload.

O tracer agora registra 512 bytes da entrada, o material UMID, os candidatos de
ponteiro, os quatro buffers principais e os dois lados do round-trip final em
`sg_trace.cpp`. `SG_TRACE_TRANSFORM_FINAL=1` observa o último byte do buffer,
evitando capturar o primeiro byte da última instrução de par XOR.

### 7.3.1. Inicialização da tabela e seed

O preenchimento de `tableData+0xa0` foi confirmado em `sg_trace_table23.log` e
`sg_trace_table27.log`:

```
+0x28e88d  dispatch index 0x06  → +0x5c6310  (fonte da seed)
+0x28e89e  dispatch index 0x22  → +0x5cf410  (setter do estado PRNG)
+0x28e13a  dispatch index 0x2c  → +0x5cf428  (próximo valor PRNG)
+0x28e156  tableData+0xa0       ← byte gerado, 256 vezes
```

`+0x5c6310` chama um helper que fornece Windows `FILETIME`. O código subtrai
`116444736000000000` (`0xfe624e212ac18000` como `int64`) e converte os ticks de
100 ns para segundos. Portanto, a seed observada é o Unix timestamp atual:

```text
seed = uint32((filetime - 116444736000000000) / 10000000)
```

No run `table27`, o retorno foi `0x6a7185d0` = `1785824720` (`2026-08-04
06:25:20 UTC`), e o mesmo valor chegou a `+0x5cf41d` em `EBX`. O setter grava a
seed em `stateObject+0x28`; o estado anterior observado na primeira inicialização
foi `1`.

O gerador em `+0x5cf43e` é:

```text
state = (state * 0x343fd + 0x269ec3) & 0xffffffff
byte  = ((state >> 16) & 0x7fff) & 0x7f
```

São 256 iterações. A implementação Go correspondente está em
`internal/accio/security_table.go`. Com seed `0x6a7184c5`, ela produz o mesmo
dump capturado em `sg_sbox_probe25.bin.261040` e `.2651e0`:

```text
prefix: 650137280a747b44391b233379767f13
sha256: c2fa2498d7751e197ee96e557fc424c1a2d529915d72000b845ea157e3f798f4
```

Chamadas posteriores ao setter no mesmo processo repetem a seed, enquanto a
tabela permanece no mesmo endereço e conteúdo. A tabela agora está reproduzida;
o trabalho restante é mapear como o estado/request buffer alimenta o staging
binário de `pctb-x-sign` e `pctb-x-mini-wua`.

### 7.3.3. Round-trip do staging final

Uma captura focada confirmou, no mesmo endereço de buffer e com a mesma tabela:

1. `+0x261040`, `r8=0x66`: entrada binária → saída ASCII de 102 bytes.
2. `+0x2651e0`, `r8=0x66`: entrada ASCII idêntica → saída binária idêntica à
   entrada do passo 1.

O teste foi byte a byte, não apenas por igualdade textual do header. Isso reduz a
busca restante para o produtor do staging: estado interno, URL, UMID, timestamp e
os campos estruturados que aparecem na estrutura de entrada de `+0x112a10`.

### 7.4. Bootstrap UMID observado no SDK nativo

O caminho de inicialização foi confirmado com `sg_trace.cpp` em código runtime:

```
security_guard.node+0x42c0       callback initUmid
security_guard.node+0x486d       IUmid::InitUMID via [vtable+0x8]
AliSafeProxy.dll+0x9ed0          alvo vtable
AliSafeProxy.dll+0x9ed0+0x125    call rax
SecurityGuardSDK64.dll+0xfd1e0   export initUMID
security_guard.node+0x5f30       callback de conclusão
```

Na entrada de `SecurityGuardSDK64.dll+0xfd1e0`, os argumentos observados foram
`RCX=6`, `RDX=security_guard.node+0x5f30`, `R8=7` e `R9=0xf`. A função usa
principalmente `ECX` e o callback em `RDX`; `R8/R9` fazem parte do contrato do
adapter, não da assinatura efetiva do export.

O dump runtime de 16 KiB (`sg_sdk_initUMID5.bin`) mostra que `initUMID`:

- preserva `RDX` como callback e `ECX` como ambiente;
- acessa o selector global em `SecurityGuardSDK64+0x73f7c0` e tabelas de dispatch
  em `+0x73f7d0`/`+0x73f870`;
- procura/registra o callback numa coleção interna, passando por
  `SecurityGuardSDK64+0x60ae0`;
- prepara o estado UMID por `SecurityGuardSDK64+0x108780`;
- chama o verificador de cookie em `+0x333310` antes de retornar o status.

Na chamada real, `+0x60ae0` recebeu `RCX` apontando para a coleção e retornou para
`+0xfd4bf`. O helper `+0x108780` recebeu:

- `RCX` apontando para uma estrutura local `{ env=6, 1, 0, 1 }`;
- `RDX` apontando para um status local inicializado em zero;
- `RAX=1` no ponto de entrada observado;
- retorno para `+0xfd54b`.

Depois disso, o callback `security_guard.node+0x5f30` executou e retornou em
`SecurityGuardSDK64+0xfd9ee`; o `getSecurityFactorsForWeb` seguinte retornou
`status=0` e os 11 headers completos, incluindo `pctb-x-sign`.

Detalhe operacional: `SecurityGuardSDK64+0xfd1e0` é desempaquetado depois do load.
No load inicial o primeiro byte pode ser `0x00`; antes da execução ele se torna
`0x56` (`push rsi`). O tracer agora atualiza o byte original durante o
rearmamento físico. Sem isso, restaurar o breakpoint corrompia a entrada e gerava
`0xC0000005`.

---

## 8.5 Site WAF vs API WAF — VEREDICTO (investigação 2026-08-03)

**Pergunta:** o WAF que protege o site (signup + score de aprovação dos 500 créditos grátis)
é o mesmo sistema do WAF da API?

**Resposta: SIM — mesma família AliSafe/AWSC, com dois "sabores" (browser mtop vs native SDK).**

### Evidências

1. **Landing page www.accio.com** (fetch do HTML cru, `acciowork-home/0.0.184`):
   - Servido por `s.alicdn.com` (CDN Alibaba), meta `aplus-accio` + beacon-aplus
     (telemetria Alibaba), `data-spm="a2700"` (tracking spm).
   - Carrega **`https://s.alicdn.com/@g/mtb/lib-mtop/2.7.2/mtop.js`** — o cliente
     do protocolo mtop da Alibaba. mtop tem o campo **`ua`** (o WUA do SecurityGuard
     browser-side) + `etSign`/`bx_et` (assinatura de URL). É o mesmo token que o app
     manda como `pctb-x-mini-wua`, só que gerado pelo JS (um.js/wua) em vez do addon nativo.
   - Cookie de login `xman_i` (LoginStateUtils.isLoggedIn), cookie locale `sc_g_cfg_f`
     — cookies de sessão do ecossistema Alibaba/ICBU.
   - O bundle do landing é 100% marketing (FAQ, cocreate pitch); o signup real vive
     no SPA `/work/app` (pre-warmado via service worker `/work/app/sw.js`).

2. **Challenge HTML que o gateway devolve** (capturado no `sdk.log`, 316673+):
   ```html
   <script src="//g.alicdn.com/mtb/??lib-promise/3.1.3/polyfillB.js,lib-mtop/2.6.3/mtop.js,lib-windvane/3.0.6/windvane.js"></script>
   <script src="//g.alicdn.com/AWSC/CAPTCHA/0.0.1/awsc.js"></script>
   <script async src="//g.alicdn.com/bsop-static/sufei-punish/0.1.124/build/htmltocanvas.min.js"></script>
   <punish-component />
   ```
   - **`sufei-punish`** = página de punição anti-bot padrão da Alibaba (mesma família
     usada em Taobao/AliExpress). **`awsc.js`** = CAPTCHA do **AWSC (Alibaba Cloud Web
     Security)** — o produto de anti-bot/captcha que compartilha o mesmo engine de risco
     do SecurityGuard nativo.
   - Ou seja: quando o WAF da API pune, ele entrega a MESMA página de challenge que o
     site usaria para um signup suspeito.

3. **Credits/entitlement** (client.go:35-36):
   - `CreditsURL = GatewayBase + "/entitlement/currentSubscription"`
   - `CreditsQuotaURL = GatewayBase + "/entitlement/quota"` (`subscripType=INDIVIDUAL`)
   - Resposta normaliza pools `daily + monthly + referral`; a aprovação/score dos 500
     créditos é decidida SERVER-SIDE no serviço de entitlement — o WAF apenas protege o
     acesso ao endpoint. A criação de conta/score no site passa pelo mesmo portão:
     mtop → risco → captcha se o fingerprint for estranho.

### Conclusão prática para o proxy

- O signup "site" e o chat "API" estão atrás do MESMO controlador de risco Alibaba.
- A diferença é só o transport do cliente: browser usa mtop.js (WUA via JS), o app usa
  SecurityGuardSDK nativo (mini-WUA via addon). Os headers `pctb-x-*` são o equivalente
  HTTP do campo `ua` do mtop.
- Implicação: um token que passou no signup do site pode ainda ser bloqueado no gateway
  se o fingerprint do client (addon nativo) não bater com o que o site registrou —
  o que explica o "logar uma vez no desktop" (seção 4) como requisito de fingerprint.

---

## 9. Decodificação do `pctb-x-sign` (2026-08-04) — tabela, transform XOR e estrutura do payload

### 9.1 Tabela de transformação (sbox) — RECUPERADA

- A tabela runtime é lida em `tableData + 0xa0` (256 bytes), apontada por `[objeto+0xd0]`.
- Gerada por **LCG com seed = Unix timestamp (segundos)** da inicialização do SDK:

```
state = seed
para i em 0..255:
    state = state * 0x343fd + 0x269ec3   (mod 2^32)
    table[i] = (state >> 16) & 0x7f
```

- Confirmado 256/256: corrida staging6 → sbox `35 18 5a 03 29 0b 5b 37...` == `generateSecurityTable(0x6a725a75)` (seed = 1785879157, ~351s antes da análise).
- Já reproduzido em `internal/accio/security_table.go`.

### 9.2 Transform final (0x261040 / 0x2651e0) — XOR auto-inverso

- Assinatura: `f(objeto=rcx, buffer=rdx, len=r8, ...)`; opera **in-place** em chunks de 2 bytes (loops internos `0x261372` e `0x2654e0`).
- Loop (2 bytes por iteração, disassembly confirmado):

```asm
; para pos = 0, 2, 4, ...:
;   al = sbox[(estado + pos) & 0xff]        ; movzx al, [rbp+rax+0xa0]
;   buffer[pos]   ^= al
;   al = sbox[(estado + (pos|1)) & 0xff]
;   buffer[pos+1] ^= al
```

- **`estado = buffer_ptr & 0xff`** — o `[rsp+0x38]` recebe o ponteiro do buffer de saída (log `transform-loop ... rsp+38=0x29bda6128d4`). Confirmado 102/102 em 4 corridas.
- No ambiente atual o buffer = `scratch + 0x6144` onde `scratch = [objeto+0xd0 → field → field+0x98]`; o scratch termina sempre em `0x790` (pool alinhado + offset fixo) → **estado = 0xd4 determinístico** (4/4 corridas: `...8d4`).
- Resultado: `asc[i] = raw[i] ^ table[((buffer&0xff) + i) & 0xff]` — XOR auto-inverso (round-trip 102/102).

### 9.3 Descoberta-chave: o staging é o sign PRÉ-CODIFICADO

- O "raw block" de 102 bytes capturado no buffer NÃO é dado puro: `raw[0] = 'w'(0x77) ^ table[0xd4]` — o SDK **monta o buffer já com o XOR aplicado** e o transform apenas "revela" o sign com o segundo XOR.
- Ou seja: para reimplementar em Go **não precisamos do XOR** — o sign final é:

```
sign = "wzpCVE002xAA" + base64(67 bytes)     # total 102 chars
```

- Fluxo do sign no runtime: staging escreve raw(=sign^key) → operador `0x2648a0` (r8=0x66) chama `0x261040` → buffer vira o sign → operador `0x264960` chama `0x2651e0` (re-encode) → `sign-extract` lê os 102 chars ASCII.

### 9.4 Estrutura do payload de 67 bytes (validada em 50/50 signs reais)

O payload é: **header (15 bytes) + corpo por sessão (44 bytes) + tail (8 bytes)**.
Mascaramento por paridade com dois tokens de 1 byte (X e Z) + um contador.

```
pos  0 : A       = 0x20 | (Z & 0x0f)            (fixo high-nibble 0x2)
pos  1 : X        (espelhado em 13, 59, 61, 63, 65)
pos 2..11: campos (10 bytes, random por request)
pos 12 : Y       = Z ^ (counter << 4)            ← contador GLOBAL mod 16 (1..15,0,1...)
pos 13 : X
pos 14 : Z        (espelhado em 62, 66)
pos 15..58: corpo por sessão (44 bytes) mascarado:
              i par  → body[i-15] ^ Z
              i ímpar→ body[i-15] ^ X
pos 59 : X
pos 60 : Z ^ (j<<4)   (espelhado em 64; j = 1 na maioria das capturas)
pos 61 : X
pos 62 : Z
pos 63 : X
pos 64 : Z ^ (j<<4)
pos 65 : X
pos 66 : Z
```

- `(Y^Z) & 0x0f == 0` e `(b60^Z) & 0x0f == 0` (nibble baixo compartilhado com Z).
- O corpo (44 bytes) = [13 bytes fixos SDK] + [21 bytes por SESSÃO/UMID] + [10 bytes fixos SDK].
  Mudou entre as sessões do corpus e do probe novo; é estável dentro de uma sessão
  (mesmo nos 3 signs consecutivos). Não é derivado do utdid nem XOR simples do umt.
- O `X`, `Z` e os 10 campos são **random por request** (entropia consistente);
  o payload NÃO contém ts/url/hash de forma direta (nenhum byte constante por URL).
- O k (Y12) é o contador de signs; o j (b60) é um campo secundário (na maioria = 1).

### 9.5 Gerador Go implementado, validado e INTEGRADO

- `internal/accio/sign_generator.go`: `NewSignGenerator(body44)` +
  `Generate()` → sign de 102 chars; `ExtractSignBody(sign)` recupera os 44 bytes;
  `ValidateStructure` checa todas as invariantes.
- `internal/accio/local_sign.go`: `applyLocalSign(headers)` substitui o
  `pctb-x-sign` do addon por um gerado em Go (corpo extraído do primeiro sign
  real da sessão; fallback para o sign do addon em qualquer erro). Ativado por
  padrão; desative com `ACCIO_SG_SIGN_LOCAL=0`.
- Integrado em `securityHeaders` (security_bridge.go): o daemon continua
  fornecendo umt/mini-wua/sgext/etc; só o sign é gerado localmente.
- Testes:
  - `sign_generator_test.go`: 7 signs reais, corpo por sessão, round-trip,
    distribuição do contador (40 signs cobrem 0..15).
  - `sign_daemon_integration_test.go`: sessão LIVE do addon → 10 signs Go
    estruturalmente válidos.
  - `sign_server_live_test.go`: **request real ao gateway** — controle (sign do
    addon) e teste (sign Go) ambos `200` em `/api/tool/featureFlag/evaluate`.
    O WAF **aceita** o sign gerado em Go (X/Z/campos random + contador + corpo).

### 9.6 Próximos passos

1. `pctb-x-mini-wua` (~364 chars, 273 bytes decodificados): também bound à
   sessão e muda por request — mesmo tratamento de RE (estrutura + gerador).
2. Decifrar o corpo de 21 bytes por sessão (derivação do UMID/fingerprint) para
   dispensar a captura inicial via addon.
3. Entender o campo secundário j (b60 = Z^(j<<4)); o servidor aceitou j=1,
   então a prioridade é baixa.

---

## 11. Sistema de risco de contas novas — DIAGNÓSTICO (2026-08-04)

### 11.1 O WAF NÃO é o bloqueio real do signup

Testes empíricos com Chrome controlado (chromedp) + email temporário:

| Cenário | Resultado |
|---|---|
| Chrome com PERFIL LIMPO + IP real (128.201.x) | Signup OK (sem captcha) |
| Chrome com PERFIL LIMPO + IP WARP (datacenter CF) | Signup OK (sem captcha) |
| Chrome com PERFIL SUJO (cookies/histórico do user) + qualquer IP | **Captcha x5sec** ("unusual traffic") |
| Chrome headless + perfil limpo | Signup OK |

- O sistema de risco marca o **PERFIL DO NAVEGADOR** (cookies/fingerprint), NÃO o IP.
- O captcha (NoCaptcha Aliyun, `cf.aliyun.com/nocaptcha`, v1.3.21) aparece no fluxo
  mas **NÃO bloqueia o envio do código** (o `slide` endpoint retorna 200 sem
  interação) — o signup completa mesmo com o captcha presente.
- O accmgr já usa perfil novo por tentativa → o signup do email **funciona**
  (logs de 02/08: "page_after_email" = tela de código, sem bloqueio).

### 11.2 O fluxo de signup atual (site)

```
www.accio.com/login (buyer-agent 2.2.346)
  → saml/route/check (login.accio.com)
  → newXmanRegister.htm?email=...           (registro legado)
  → integrated/captcha/token → punish (x5sec, as vezes)
  → nocaptcha/initialize + slide (NC)       (nao bloqueia)
  → integrated/sendCode                     (codigo por email)
  → integrated/captcha (POST) → login OK
  → www.accio.com/work/app (Accio Work web, sessao completa)
```

A sessão web resultante (cookies em `.accio.com`):
- `phoenix_cookie = accessToken=<32hex>&expiresAt=<ts>&refreshToken=<128hex>`
- `cookie2 = <accessToken>` (mesmo valor), `xman_t`, `xman_i=aid=<userId>`,
  `xman_f`, `xman_us_f`, `xman_us_t`
- `userId` (ex: 4500101109233) no formato OAuth.

### 11.3 O gargalo: token web ≠ token OAuth desktop

| Teste | Resultado |
|---|---|
| GET `/api/entitlement/currentSubscription` com accessToken web | `{"success":false,"code":"401","message":"NOT_LOGIN"}` |
| POST `/api/auth/safe/refresh_token` com refreshToken web | `{"success":false,"code":"502","message":"auth not pass"}` |
| POST refresh com refreshToken OAuth real (controle) | **OK** (`userId`, accessToken novo, cookie2) |

- Os formatos dos tokens são idênticos (32/128 hex) e compartilham um segmento
  comum (`...c0e889dc3139d350425c2840f134b4a...`) — mesma família, canais diferentes.
- O gateway phoenix só aceita tokens do canal **desktop (OAuth)**.

### 11.4 O fluxo OAuth desktop (PKCE) está QUEBRADO no site

O re-login PKCE (`/login?return_url=...&code_challenge=...&client_id=accio-work`)
na sessão web recém-criada:

- Tela "Você se inscreveu em uma conta existente" → botão Continuar
- `hasLogin.do` → **`{"success":false,"code":101,"message":"illegal param error"}`**
  → `signin_failed` → `toMainSignIn` (volta ao email)
- `oauth_sign.htm?returnUrl=...&client_id=accio-work` →
  **`{"code":1101,"message":"auth_states_null"}`** — falta o "auth state" do
  passo authorize (o buyer-agent novo não o cria para o fluxo legado).
- O fluxo novo do site (integrated/sendCode) não popula a sessão legada
  (login.accio.com/newlogin) que o hasLogin.do espera.

### 11.5 Correções aplicadas

- `manager.go`: `ACCIO_USE_WARP=0` agora é lido (o DefaultConfig não lia a env —
  o WARP ligava sempre). O WARP é opcional (o risco é do perfil, não do IP) e os
  IPs WARP (datacenter CF) podem até atrapalhar.
- Probes de diagnóstico: `tmp_probe/nc_inspect.go`, `nc_pkce.go`,
  `web_session_token.go` (signup completo + extração de cookies + teste do gateway).

### 11.6 Próximos passos para destravar o token OAuth

1. Descobrir o endpoint **authorize** do fluxo novo (o que cria o "auth state"
   que o oauth_sign.htm exige) — o loginURL PKCE deveria criá-lo, mas o
   buyer-agent 2.2.346 pode usar outro caminho (procurar `authorize`/`auth_state`
   no bundle).
2. Capturar o **deep link do app** (`accio-work://...`, "正在唤起 Accio Work") —
   o code OAuth válido está na URL do protocolo (o app o usa para o exchange).
3. Procurar endpoint de **conversão web→OAuth** (ex: `mtop.alibaba.intl.accio.token.compensate`
   visto no log do site).

---

## 12. Referências de arquivos-chave

- Bundle main do app: `.accio-asar-inspect\out\main\index.js` (14 MB minificado) — contém sg_k, getSecurityFactorsForWebHeaders, Lc/Mc (urls), Sc (paths).
- SDK wrapper JS: `%LOCALAPPDATA%\Programs\Accio\resources\app.asar.unpacked\node_modules\@phoenix-common\security-guard\dist\*.js`
- Dados do app: `%USERPROFILE%\.accio\` (utdid, domain-config.json, settings.jsonc, logs\sdk.log 60MB)
- Log de runtime (ouro): `.accio\logs\sdk.log` — mostra `beforeRequest` com utdid e URL, e `security.factors.succeeded` com headerKeyCount=11.
- `sg_dump.cjs` / `sg_fine.cjs`: harnesses de captura controlada (URL, tempo, host, appkey).
- `sg_analyze.cjs`: análise offline dos JSONL (estrutura, avalanche, invariantes).
- `sg_abi_probe.cpp`: probe isolado do contrato `QueryInterface(0x129)`/vtable; compilado
  fora do workspace para não depender do Node.
- `sg_wait.cjs` + `sg_remote_probe.cpp`: processo Node mínimo + leitor de memória local
  para confirmar o objeto/vtable depois do bootstrap real do addon.
- `sg_trace.cpp`: tracer `DEBUG_ONLY_THIS_PROCESS`; confirmou o JSON cru no RDX, o
  `statusCode` no R8 e o JSON de 11 headers retornado no RAX.

---

## 13. Descobertas de 13/08/2026 (criação de contas confiável)

### 13.1 O exchange é gated por fingerprint TLS do CLIENTE

- Request de exchange byte-perfeito (S256 conferido, redirectUri igual, headers
  desktop exatos do app 0.29.1) ainda recebia `invalid_code` (90001) em ~50-70%
  das tentativas a partir do cliente Go (crypto/tls, HTTP/2).
- Prova decisiva (TestNodeExchange): o MESMO código capturado pelo nosso fluxo,
  trocado por um processo **Node** filho (https.request, HTTP/1.1, TLS do
  BoringSSL do Node) → **sucesso na primeira tentativa**.
- Solução integrada: `exchangeCodeForToken` prefere um sidecar Node
  (`accio-oauth-exchange.cjs` em %TEMP%) quando `node` existe no PATH; o caminho
  Go puro fica como fallback (`ACCIO_EXCHANGE_VIA=go` força).

### 13.2 Refresh de conta nova sempre falha — e no app oficial também

- `POST /api/auth/refresh_token` de conta recém-criada responde
  `{"success":false,"code":"502","message":"auth not pass"}` — inclusive no app
  oficial (43 tentativas capturadas no sniff, todas 502 logo após o signup).
- O app inclui o **accessToken atual no body** do refresh ({utdid, version,
  accessToken, refreshToken}) — contrato corrigido no client.
- Conclusão: refresh NÃO serve como validação de conta nova. A validação correta
  é `GET /api/entitlement/currentSubscription` (funciona imediatamente com o
  access token quando a conta está limpa).

### 13.3 Aquecimento de IP/perfil: NOT_LOGIN (401) → user blocked (423)

- Sessão das 15:19 (IP frio): conta criada via nosso Chrome automatizado +
  exchange do app → entitlement 200 com **520 créditos (500 referral + 20
  daily)** imediatos.
- Depois de ~15 signups em poucas horas no mesmo IP: tokens passam a sair
  `NOT_LOGIN` (401) no entitlement (todas as variações de header/cookie
  testadas — o problema é a CONTA, não o request) e, mais tarde, `user blocked`
  (423).
- IPs do WARP (Cloudflare) são PRÉ-marcados: código emitido sob IP WARP foi
  envenenado mesmo para o exchange via Node. WARP permanece OFF por padrão.
- Mitigação em produção: cooldown de criação de 10min (DefaultConfig) + retry
  com backoff crescente. Cadência baixa = contas limpas.

### 13.4 Pipeline final de signup

1. Perfil de Chrome PERSISTENTE (%TEMP%/accio-signup-profile) — envelhece o
   cna/_m_h5_tk como a partição do app oficial; prompt "continuar com conta
   existente" é contornado clicando "Entre agora com outra conta".
2. Inbox temporário novo por tentativa + snapshot de mensagens vistas
   (`Inbox.Reset`) — impede ler código velho da tentativa anterior.
3. Input com eventos REAIS de teclado/mouse (CDP Input.dispatch*) — injeção JS
   pura não gera entropia de interação.
4. Callback local em porta aleatória (a 4097 canônica é rejeitada pelo servidor
   para sessões não-app).
5. Exchange via Node sidecar; fail-fast em invalid_code (canal LoginErrors);
   retry até 4 tentativas com backoff 25-55s.
6. Aceite só com entitlement > 0 créditos (polling até 2min).

## 14. Signup via proxy residencial rotativa (13/08/2026, noite)

O gargalo final do pipeline de criação é a VELOCIDADE de cadastro por IP: após
~5-15 criações em poucas horas, o risco sobe e as contas nascem limitadas
(NOT_LOGIN no entitlement) ou bloqueadas (423). Esperar o IP esfriar (~75min+)
não sustenta cadência.

Solução implementada: `ACCIO_SIGNUP_PROXY=http://user:pass@gateway:porta`
(internal/accmgr/proxy.go). Apenas o BROWSER de signup sai pela proxy — é ele
quem toca todos os endpoints pontuados por risco (formulário, envio do código,
emissão do oauth/code). A troca de código (sidecar Node) continua saindo do IP
de casa, mesmo split do app oficial. Gateways residenciais rotativos (IPRoyal,
Proxy-Cheap, BrightData) entregam um IP novo por conexão/sessão, então cada
tentativa nasce com reputação fria. Autenticação user:pass é respondida via
Fetch auth-challenge (Chrome ignora credenciais no --proxy-server).

Sem a variável, comportamento inalterado (IP de casa + cooldown de 10min +
pool de espera re-checando contas pendentes a cada minuto).
