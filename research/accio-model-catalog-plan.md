# Plano: catálogo dinâmico do Accio e reasoning effort por modelo

## Decisão recomendada

Não reproduzir a função que gera a senha do gateway local do Accio e não tentar extrair essa senha do renderer.

A melhor arquitetura para este proxy é usar o cliente Accio nativo que já existe no projeto:

1. O proxy faz o login do Accio por OAuth/PKCE no próprio aplicativo.
2. O token é armazenado no diretório privado do proxy com as permissões já usadas pelo cliente Accio.
3. O cliente Accio chama o catálogo remoto autenticado.
4. O proxy transforma o retorno em um catálogo sanitizado.
5. A UI lê esse catálogo e mostra os `reasoningEfforts` válidos para o modelo selecionado.
6. No envio OpenAI-compatible, o cliente usa `reasoning_effort`.
7. No adaptador Accio, `reasoning_effort` é convertido para `reasoningEffort`.

Assim, o proxy não depende de o `Accio.exe` estar aberto, não precisa conhecer a senha IPC e não persiste credenciais em respostas HTTP.

## O que já existe no repositório

### Cliente Accio

`internal/accio/client.go` já contém:

- login OAuth/PKCE;
- armazenamento de token por conta;
- renovação do token em caso de 401/403;
- `Client.Models(ctx)`;
- parser tolerante a respostas aninhadas;
- conversão de `reasoning_effort` para `reasoningEffort` em `buildRequest`;
- envio de requisições nativas para o gateway Accio.

Hoje `Models` retorna somente ID, nome, provider e descrição; o parser ainda descarta contexto, multimodalidade e os campos de esforço. Esses campos devem ser extraídos do JSON original na Fase 1.

### Proxy HTTP

`internal/proxyhttp/server.go` já possui:

- `GET /v1/models`;
- rota por prefixo de modelo;
- entrada `accio/*` para o cliente Accio;
- catálogo fallback para o modelo padrão;
- `SetAccio` para injetar o cliente Accio no servidor.

### UI

`frontend/src/shell.js` já possui menus separados para:

- modelo;
- esforço;
- API.

Mas o esforço ainda é fixo em `low`, `medium` e `high`, e a lista de modelos não carrega metadados por modelo.

## Implementação em fases

### Fase 1 — contrato interno de modelo

Alterar `internal/accio/client.go` para representar os metadados relevantes:

```go
type Model struct {
    ID                    string
    Name                  string
    Provider              string
    Description           string
    Context               int64
    Multimodal            bool
    ReasoningEfforts      []string
    DefaultReasoningEffort string
    FreeUse               bool
    Locked                bool
}
```

Regras do parser:

- aceitar `reasoningEfforts` como array de strings;
- aceitar `defaultReasoningEffort` como string;
- remover duplicados e valores vazios;
- se o catálogo não trouxer esforços, deixar a lista vazia;
- não inventar `low/medium/high` para modelos que não os oferecem;
- conservar `modelCode` como ID interno, com o prefixo externo `accio/` apenas na interface do proxy;
- conservar `contextWindow`, `freeUse` e `locked` quando presentes.

O parser deve continuar aceitando as formas aninhadas que já são cobertas pelos testes existentes.

### Fase 2 — catálogo cacheado no cliente Accio

Adicionar cache em memória no `accio.Client`:

- TTL curto, por exemplo 30–60 segundos;
- chave por conta ativa;
- última resposta válida mantida durante falha transitória;
- nunca armazenar token no objeto retornado pela API;
- impedir chamadas concorrentes duplicadas com mutex/singleflight simples.

Comportamento:

- primeira consulta: chama o gateway remoto autenticado;
- 401/403: tenta refresh uma vez e repete;
- erro transitório depois de haver cache: retorna o cache e registra o erro;
- sem token e sem cache: retorna erro de autenticação;
- catálogo vazio: não substitui silenciosamente por um catálogo falso.

### Fase 3 — resposta `/v1/models`

Expandir `enrichModelMeta` e a montagem de `handleModels` para incluir, nos modelos Accio:

```json
{
  "id": "accio/<modelCode>",
  "object": "model",
  "owned_by": "accio",
  "provider": "accio",
  "name": "<modelDisplayName>",
  "description": "<providerDisplayName>",
  "context_window": 100000,
  "reasoning_efforts": ["low", "medium", "high"],
  "default_reasoning_effort": "medium",
  "free_use": true,
  "locked": false
}
```

Os campos extras não quebram clientes OpenAI-compatible; clientes que ignoram campos desconhecidos continuam funcionando. O `context_window` e os demais metadados dinâmicos do catálogo devem ter precedência sobre qualquer valor estático de `enrichModelMeta`.

O fallback `accio/1Nexus-R36W8qJ5vB6h` pode permanecer apenas enquanto não houver catálogo real, marcado como fallback e não como catálogo confirmado. Quando o catálogo real carregar, remover o fallback estático antes de deduplicar, inclusive se o ID padrão não estiver presente na resposta real.

### Fase 4 — validação do esforço no envio

No `internal/proxyhttp/server.go`, antes do envio para Accio:

1. ler `reasoning_effort` do corpo OpenAI;
2. aceitar `reasoningEffort` apenas como compatibilidade opcional, sem preferência ambígua;
3. localizar o modelo no catálogo cacheado;
4. se o modelo declarar esforços, rejeitar valores fora da lista com HTTP 400;
5. se o modelo não declarar esforços, não forçar nenhum valor;
6. encaminhar para `internal/accio/client.go` como `reasoningEffort`.

Mapeamento único:

```text
OpenAI request: reasoning_effort
Accio wire:     reasoningEffort
```

O proxy não deve converter automaticamente `high` para outra escala nem transformar esforço em um modelo diferente. Os aliases antigos (`k3-agent-high`, etc.) podem continuar existindo somente para Kimi, sem contaminar a lógica do Accio.

### Fase 5 — UI dependente do modelo

Alterar `frontend/src/state.js` e `frontend/src/shell.js`:

- guardar `state.models` com `reasoning_efforts` e `default_reasoning_effort`;
- ao trocar o modelo, recalcular as opções do menu de esforço;
- selecionar o default informado pelo catálogo;
- se o esforço atual não existir no novo modelo, trocar para o default;
- esconder/desabilitar o menu quando o modelo não oferecer esforço configurável;
- manter `reasoning_effort` nas chamadas do chat;
- não manter uma lista global fixa como fonte de verdade.

O menu global e o menu do composer devem usar a mesma função `effortOptionsForModel(modelID)` para evitar divergência.

### Fase 6 — testes

Adicionar ou atualizar testes em:

- `internal/accio/client_contract_test.go`:
  - parser de `reasoningEfforts`;
  - default de esforço;
  - deduplicação;
  - `locked` e `freeUse`;
  - ausência de esforço sem valores inventados;
  - mapeamento snake_case para camelCase.

- `internal/proxyhttp/models_route_test.go`:
  - campos extras em `/v1/models`;
  - deduplicação do fallback;
  - catálogo anterior em caso de falha transitória, se o cache for implementado.

- novo teste do handler de Accio:
  - esforço válido chega como `reasoningEffort`;
  - esforço inválido retorna 400;
  - ausência de esforço não injeta campo.

- frontend:
  - teste manual ou automatizado de troca de modelo com conjuntos de esforços diferentes.

## Fluxo operacional final

```text
Usuário faz login no proxy
        ↓
Token Accio fica no armazenamento privado do proxy
        ↓
GET /v1/models
        ↓
accio.Client.Models()
        ↓
catálogo remoto autenticado + refresh se necessário
        ↓
proxy sanitiza e cacheia modelos/metadados
        ↓
UI monta modelo e esforços daquele modelo
        ↓
POST /v1/chat/completions
  {"model":"accio/...", "reasoning_effort":"high"}
        ↓
validação por modelo
        ↓
reasoning_effort → reasoningEffort
        ↓
request nativa ao gateway Accio
```

## O que acontece se o Accio Desktop estiver aberto

Nada especial é necessário. O proxy usa sua própria sessão autorizada e não compete com o processo local. O `Accio.exe` pode estar aberto ou fechado.

## O que acontece se o usuário estiver logado apenas no Accio Desktop

O proxy não deve tentar reutilizar silenciosamente a sessão interna do desktop. Sem uma API oficial de compartilhamento, isso exigiria capturar senha IPC, cookies ou instrumentar o renderer.

Nesse caso há duas opções legítimas:

1. fazer o login do Accio uma vez dentro do proxy, usando o fluxo OAuth já implementado;
2. no futuro, criar uma integração explícita e consentida entre os dois aplicativos, com contrato documentado e entrega somente do catálogo sanitizado.

A opção 1 é a recomendada agora porque já está parcialmente implementada e não depende de engenharia reversa do segredo local.

## Compatibilidade e fallback

- Clientes OpenAI que não conhecem `reasoning_efforts` continuam usando somente `id` e `owned_by`.
- Clientes que enviam esforço recebem validação clara por modelo.
- Quando o catálogo remoto está temporariamente indisponível, o último catálogo válido pode continuar servindo por um período curto.
- Sem qualquer catálogo autenticado, o fallback mostra o modelo padrão apenas para permitir configuração, mas uma chamada real deve retornar erro de autenticação se não houver conta válida.
- Tokens e senhas nunca aparecem em `/v1/models`, logs, mensagens de erro ou payloads enviados à UI.

## Ordem prática de execução

1. Expandir `accio.Model` e `catalogModel`.
2. Criar testes do parser com o formato `provider/modelList` informado.
3. Alterar `/v1/models` para expor os metadados.
4. Implementar validação e encaminhamento do esforço.
5. Alterar os menus da UI para serem dependentes do modelo.
6. Implementar cache e fallback transitório.
7. Rodar `go test ./...` e validar manualmente com login Accio real.

## Critério de pronto

A solução está pronta quando:

- o proxy lista os modelos reais da conta Accio sem hardcode;
- cada modelo mostra apenas os esforços que seu catálogo declara;
- `reasoning_effort` chega ao gateway como `reasoningEffort`;
- um esforço inválido é rejeitado antes do upstream;
- 401 dispara refresh sem expor token;
- Accio aberto/fechado não altera o funcionamento do proxy;
- testes cobrem catálogo, transformação e falhas de autenticação.
