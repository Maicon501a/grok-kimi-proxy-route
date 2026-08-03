# Engenharia reversa local do Accio: catálogo de modelos e esforço de raciocínio

Data da análise: 2026-07-25

## Escopo e método

Análise somente leitura do processo local do Accio aberto no Windows e do `app.asar` extraído no workspace. Não houve encerramento de processo, alteração do binário, alteração de dados do Accio ou chamada WAN feita para testar endpoints protegidos.

Alvo confirmado:

- Executável: `C:\Users\maicon2\AppData\Local\Programs\Accio\Accio.exe`
- Processo principal: PID `11768`
- Versão observada no `app.asar`/crashpad: `0.25.0`
- Electron observado: `35.7.5`
- User data dir: `C:\Users\maicon2\AppData\Roaming\Accio`
- Gateway local: `127.0.0.1:4097`
- Bundle analisado: `.accio-asar-inspect\out\renderer\assets\app-store-CsCe_P1c.js`

## Endpoint de lista de modelos

O cliente renderer chama o gateway local através do cliente autenticado (`me.request(...)`):

### Variante legada

```http
GET http://127.0.0.1:4097/models
```

A resposta esperada é um array de provedores. O cliente filtra itens que tenham:

- `provider`
- `modelList` como array

Cada item de `modelList` é transformado quando possui `modelCode`.

### Variante atual/configurada

```http
GET http://127.0.0.1:4097/models/config
```

O cliente tenta primeiro `/models/config`. Se falhar, faz fallback para `/models` e envolve a resposta como:

```json
{
  "providers": [
    {
      "provider": "...",
      "providerDisplayName": "...",
      "modelList": [
        {
          "modelCode": "...",
          "modelDisplayName": "...",
          "modelName": "...",
          "modelDesc": "...",
          "hoverDesc": "...",
          "usageDesc": "...",
          "usageLevel": 0,
          "contextWindow": 0,
          "isDefault": false,
          "reasoningEfforts": ["..."],
          "defaultReasoningEffort": "...",
          "freeUse": true,
          "usageMultiple": 1,
          "effectiveMultiple": 1,
          "locked": false,
          "discount": {}
        }
      ]
    }
  ],
  "ext": {
    "serverTime": 0,
    "ttlSeconds": 0,
    "nextChangeAt": 0,
    "labelList": []
  }
}
```

Os campos opcionais acima são os nomes lidos pelo bundle; a presença e o tipo real dependem do modelo/provedor retornado pelo servidor.

A variante configurada também pode trazer `ext.labelList`. O renderer ignora labels cujo `labelKey` seja `auto`; quando há labels aplicáveis, usa `targetModelCode` para substituir display name e multiplicador de uso.

## Esforço de raciocínio

Há suporte explícito no contrato local.

### Metadados do catálogo

O renderer preserva por modelo:

```js
reasoningEfforts
 defaultReasoningEffort
```

O primeiro é tratado como array quando presente. O segundo é tratado como string quando presente.

Isso permite montar a UI de forma dinâmica por modelo:

```js
const options = Array.isArray(model.reasoningEfforts)
  ? model.reasoningEfforts
  : [];
const selected = model.defaultReasoningEffort ?? options[0] ?? null;
```

Não foi possível confirmar, apenas pelo bundle local, os valores concretos de `reasoningEfforts` para cada modelo da conta atual, porque as rotas protegidas retornaram `401` sem reproduzir a sessão autenticada do renderer.

### Campo enviado na mensagem do Accio

O caminho de envio WebSocket do renderer inclui explicitamente:

```js
{
  targetConversationId,
  clientId,
  model,
  reasoningEffort
}
```

O nome interno usado pelo Accio é **camelCase**: `reasoningEffort`.

O valor vem do estado de seleção do modelo/agente. A UI de agentes também mantém `model.reasoningEffort` e `modelConfig.reasoningEffort`.

### Compatibilidade com o proxy deste workspace

O frontend atual do proxy já envia o equivalente OpenAI-compatible em **snake_case**:

```json
{
  "model": "grok-4.5",
  "messages": [{"role": "user", "content": "..."}],
  "stream": true,
  "reasoning_effort": "high",
  "api_mode": "chat"
}
```

Isso aparece em `frontend/src/chat.js` e no backup monolítico. Portanto:

- Para a UI do proxy, continuar usando `reasoning_effort` é coerente com o contrato público do próprio proxy.
- Se o proxy for falar com um endpoint/ponte que imite o protocolo interno do Accio, converter para `reasoningEffort` no adaptador.
- Não enviar os dois nomes ao mesmo tempo sem necessidade; escolha o nome conforme a fronteira do protocolo.

## Autenticação e observação do gateway

A chamada local:

```http
GET http://127.0.0.1:4097/health
```

retornou `200` e informou `healthy: true`, `appVersion: "0.25.0"`, `port: 4097` e `authenticated: true` no diagnóstico de boot.

As tentativas sem sessão reproduzida para `/models`, `/models/config`, `/api/models` e variantes retornaram `401`. Isso confirma que o endpoint de catálogo não é público no gateway local; a UI usa o cliente autenticado do Accio para adicionar os headers/cookies/token necessários.

## Recomendação de implementação no proxy

1. Expor `GET /v1/models` no proxy como já previsto pelos testes locais.
2. Manter um catálogo normalizado no frontend com:

```js
{
  id: modelCode,
  name: modelDisplayName || modelCode,
  provider,
  reasoningEfforts,
  defaultReasoningEffort,
  contextWindow,
  locked,
  freeUse
}
```

3. Ao selecionar o modelo, limitar o dropdown de esforço a `reasoningEfforts` daquele modelo.
4. Se a lista estiver ausente, não inventar opções específicas; usar o fallback já suportado pelo proxy somente quando o provedor do proxy documentar esses valores.
5. No request OpenAI-compatible do proxy, enviar:

```json
"reasoning_effort": "<valor escolhido>"
```

6. No adaptador interno Accio/WebSocket, se necessário, mapear:

```js
wire.reasoningEffort = request.reasoning_effort;
```

7. Preservar o valor selecionado por modelo, não globalmente, pois o catálogo declara capacidades diferentes por modelo.

## Limitações e próximos testes seguros

- O contrato de nomes e o fluxo de envio foram confirmados estaticamente no bundle.
- Os valores concretos por modelo e a resposta autenticada de `/models/config` ainda precisam ser capturados pelo próprio renderer autenticado ou por inspeção autorizada de uma requisição já feita pela UI.
- O formato exato do pacote WebSocket que transporta `reasoningEffort` depois do renderer não foi expandido além do objeto de opções observado no bundle.
- Não há evidência local de um endpoint separado chamado `/reasoning` ou `/effort`; o catálogo anuncia as opções e o envio usa `reasoningEffort` dentro da mensagem.
