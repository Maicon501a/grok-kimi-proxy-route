# Google Sign-in v3 — batchexecute Flow (RE 2026-08-19)

> Complemento ao ENDPOINTS.md (2026-07-17). A pesquisa anterior parou no HTML da
> página de signin; desta vez capturamos o fluxo COMPLETO via CDP (Playwright +
> Network.getResponseBody) com conta real, incluindo todos os POSTs batchexecute.

## Sequência completa capturada (browser, conta real, OAuth loopback Kimi)

```
1. GET  /o/oauth2/v2/auth?client_id=…&redirect_uri=http://127.0.0.1:61120/callback&…
   → 302 → /v3/signin/identifier?opparams=%3F&dsh=S…&rart=ANgoxc…&continue=<consentURL>&flowName=GeneralOAuthFlow&service=lso…
2. GET  /v3/signin/identifier?…
   → 200 HTML. Extrair do WIZ_global_data:
     - "SNlM0e" → token `at` (XSRF, ex: ALX_P8ujIAxFMN3N6wLgoLobkW0v:1787119494255)
     - "FdrFJe" → f.sid (ex: 5909246935972877515)
     - "cfb2h"  → bl  (ex: boq_identityfrontendauthuiserver_20260816.00_p0)
3. POST /v3/signin/_/AccountsSignInUi/browserinfo?f.sid=…&bl=…&dsh=…&_reqid=…&rt=j
   body: f.req=[9,1,1,[null,800,1280],[null,800,1280],[1,1,null,1],[0,2,2]]&at=<at>
4. POST /v3/signin/_/AccountsSignInUi/data/batchexecute?rpcids=MI613e&source-path=/v3/signin/identifier&f.sid=…&bl=…&hl=pt-BR&dsh=<dsh>&_reqid=…&rt=c
   headers: x-same-domain: 1
            x-goog-ext-278367001-jspb: ["GeneralOAuthFlow"]
            x-goog-ext-391502476-jspb: ["<dsh>","lso"]
   body: f.req=[[["MI613e","<inner json>",null,"generic"]]]&at=<at>
   → resposta wrb.fr contém ["/v3/signin/challenge/pwd", [["cid","2"],["TL","<TL>"],["flowName","GeneralOAuthFlow"],["checkedDomains","youtube"],["checkConnection","youtube:NNN"],["pstMsg","1"]]]
5. POST batchexecute?rpcids=WZfWSd,etGTrd,Aho3hb,i3kFoc,zKAP2e,RzSO2e (page data; zKAP2e carrega módulo "bfkj" = botguard)
6. POST batchexecute?rpcids=B4hajb&source-path=/v3/signin/challenge/pwd&…&TL=<TL>&dsh=<dsh>
   inner: [1,2,null,[1,null,null,null,["<PASSWORD>",null,1]],
           [<rart>,"lso",null,null,<continueURL>,null,null,<client_id>],
           null,
           [[["TL",<TL>],["access_type","offline"],… pares query da página pwd, ORDENADOS alfabeticamente …],"accounts.google.com","/v3/signin/challenge/pwd"],
           null,
           [["identity-signin-password","!<botguard token>"]]]
   → resposta contém URL /CheckCookie?continue=…
7. GET /CheckCookie?continue=… → 302 (seta SID/HSID/SSID/__Secure-*) → consent
   GET /signin/oauth/consent?authuser=0&part=AJi8h… → 302 → /signin/oauth/id?… → 200 (página de consentimento)
   (paralelo: accounts.youtube.com/accounts/SetSID e accounts.google.com.br/accounts/SetSID — provavelmente opcionais)
8. Página de consentimento (path /signin/oauth/id): extrair WIZ_global_data novo:
     - "SNlM0e" → at2   - "FdrFJe" → f.sid2   - "cfb2h" → bl2
     - "Qzxixc" → dsh2 (vai no header x-goog-ext-391502476-jspb do xyhAld)
     - token de consentimento no HTML: `!ChR…∙AKphh…` (proto serializado, server-embedded)
9. POST /_/OAuthUi/data/batchexecute?rpcids=xyhAld&source-path=/signin/oauth/id&f.sid=<sid2>&bl=<bl2>&hl=pt-BR&authuser=0&soc-app=1&soc-platform=1&soc-device=1&_reqid=…&rt=c
   body inner: [[[null,<client_id>,[scopes],"<consentToken>",null×7,"http://127.0.0.1:61120",<dsh original>,0,…,14,null,1,[scopes],…,[[[204,1,…,scope1,1],[202,…,scope2,1],[9315,…,openid,1]],1],null,[]],[scopes]]
   scope ids: 204=userinfo.profile, 202=userinfo.email, 9315=openid
   → resposta indica próxima URL
10. GET /signin/oauth/consent?as=<dsh>&authuser=0&client_id=…&requestPath=…
    → 302 → http://127.0.0.1:61120/callback?iss=…&code=4/0AT…&scope=…&authuser=0&prompt=none
11. POST oauth2.googleapis.com/token (code exchange c/ PKCE) — já implementado
```

## Campos ENFORCED pelo servidor (teste de mutação com sessão real, 2026-08-19)

| Campo | Mutação | Resultado |
|---|---|---|
| trailing hash do inner MI613e (88 chars base64 ≈ 64 bytes → SHA-512?) | null | **REJEITADO** ([3]) |
| token botguard `["identity-signin-identifier","!…"]` | null | **REJEITADO** ([3]) |
| token botguard `["identity-signin-password","!…"]` | (não testado isolado — mesmo padrão, assumir enforced) |

Resposta de rejeição: `["wrb.fr","MI613e",null,null,null,[3],"generic"]`

## Tokens botguard (identity-signin-*)

- Formato: `!` + 8 chars rand + `NAAaNhfGzrYVC` (constante entre sessões) + ~13 chars
  (session-bound) + `AEABEA` + tail rand. Total ~170 chars.
- Gerados client-side pelo módulo `bfkj` (botguard VM) carregado via zKAP2e.
- Programa botguard servido de `https://www.google.com/js/bg/` (ver loader `P.botguard.KrH_` na página).

## O que falta para HTTP-direct completo

1. ~~Trailing hash~~ **RESOLVIDO**: não é hash — é uma chave pública ECDH P-256
   efêmera (SEC1 uncompressed `0x04||X||Y`, 65 bytes → 88 chars base64), gerada
   por `crypto.subtle.generateKey({name:"ECDH",namedCurve:"P-256"})` a cada
   chamada MI613e. Em Go: `ecdh.P256().GenerateKey()` + `PublicKey().Bytes()` +
   base64 std. A chave PRIVADA é guardada (sessionStorage no browser) e usada
   depois para decriptar dados do challenge de senha (ECDH→AES-GCM) — manter a
   chave privada por sessão no probe.
2. Runner do botguard VM headless (Node→goja) para gerar os tokens `!…`
   (subagente investigando; padrão BgUtils: `vm.a(program, cb, true, el, telemetry, [[],[]], undefined, false, loggers)` → `[syncSnapshot]`; cb recebe `asyncSnapshotFunction` etc.)
3. Com ambos: probe Go (cmd/probe-google-http) completa o fluxo

## Observações do probe Go (2026-08-19)

- Com chave ECDH válida + token botguard vazio: servidor responde payload real
  `["/v3/signin/rejected",[["rrk","46"],["rhlk","le"],["idnf","<email>"],["epd","…"]]]`
  (página "não conseguimos verificar você") — ou seja, a chave ECDH passa na
  validação estrutural; o bloqueio restante é só o botguard token.
- Com campo ausente/null/malformado: `wrb.fr …,null,null,null,[3]` (rejeição dura).
- Token botguard decodificado: 1234 bytes (snapshot proto completo — não forjável).
- Constante no token: `NAAaNhfGzrYVC` (igual entre sessões) — header de versão.

## Artefatos

- `scripts/capture-google-login.mjs` — captura de rede CDP do fluxo completo
- `scripts/mutate-login.mjs` — teste de mutação (qual campo é enforced)
- `trace1.json` / `trace2.json` / `trace3.json` — traces (trace3 = completo c/ CDP bodies)
- `google-login-html/01-03*.html` — HTML das páginas
- `tmp_probe/templates/*.tpl.json` — templates f.req com placeholders @@…@@
- `cmd/probe-google-http/main.go` — probe Go (falha no MI613e até botguard/hash resolvidos)

## Estado 2026-08-19 (tarde) — rrk=46 e account flag

- Probe Go agora usa bogdanfinn/tls-client (perfil Chrome_133: JA3/JA4 + H2
  SETTINGS idênticos ao Chrome), sec-ch-ua completo, x-browser-validation
  (Base64(SHA1(APIKey+UA))), browserinfo beacon, requests ambientes
  (CheckConnection/bscframe/generate_204) e token botguard REAL mintado via
  `tmp_probe/mint.mjs` (happy-dom + bgutils-js, patch integCheckBypass).
- Mesmo assim MI613e retorna rrk=46.
- trace4.json (08:28) = login browser completo OK, com filtro largo
  (play.google.com/log, bscframe, CheckConnection). Sequência real:
  oauth→identifier→{CheckConnection,bscframe,204}→MI613e→playlog(bg/frs,/ec,/el)
  →WZfWSd→{204,playlog,browserinfo}→B4hajb→CheckCookie→SetSID×2→consent
  →playlog(oauth)→xyhAld→browserinfo(OAuthUi)→code.
  IMPORTANTE: pings play.google.com/log só acontecem DEPOIS do MI613e — não são
  pré-requisito. Template MI613e do probe é byte-idêntico ao do browser
  (comparado campo a campo, exceto valores de sessão).
- **Account flag**: após ~15 tentativas automatizadas com token ruim, a conta
  <conta-teste-1> foi flaggeada — a partir de ~08:37 até login BROWSER real
  (controle, sem bloqueios) é rejeitado logo após submeter o email
  (/v3/signin/rejected). IP NÃO está flaggeado (página identifier carrega normal
  em sessão fresca; página OAuth identifier também). Flag é por conta
  (ou conta+IP). Esperar cooldown para retomar testes.

## Experimento decisivo preparado (pós-cooldown)

Teste híbrido para isolar o defeito (token vs envelope Go):

```
node scripts/check-cooldown.mjs          # exit 0 = flag limpou
URL=$(node tmp_probe/authurl.js | tail -1)
node scripts/mutate-login.mjs --authurl "$URL" --email E --password P \
  --mutate "idtoken|replace" --headless true
```

O modo `idtoken|replace` minta um token via mint.mjs contra a página live do
browser (GLIF_PAGE_URL/GLIF_UA da página real) e injeta no lugar do token do
browser no MI613e:
- Se login completa → token mintado é BOM → defeito está no envelope Go
  (headers/cookies/H2) → diff byte-a-byte do request do probe vs trace4.
- Se falha → qualidade do token (happy-dom detectável) → melhorar minter
  (fingerprints bablosoft GLIF_BABLO=1, eventos comportamentais, URL do snapshot).

Artefatos novos: `scripts/block-test.mjs` (ablação de requests ambientes),
`scripts/check-cooldown.mjs` (verifica flag da conta),
mutate-login.mjs modo `idtoken|replace` (teste híbrido).
