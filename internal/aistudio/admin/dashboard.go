package admin

const dashboardHTML = `<!DOCTYPE html>
<html lang="pt-BR">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>AISTUDIOPROXYPLUS Admin</title>
  <style>
    :root{
      --bg:#f3f6fb;
      --panel:#ffffff;
      --panel-soft:#f8fbff;
      --line:#d9e2ef;
      --text:#18324a;
      --muted:#64819a;
      --accent:#1d8cf2;
      --accent-2:#27b09a;
      --danger:#d85b6a;
      --warn:#c58d20;
      --shadow:0 24px 60px rgba(17,43,72,.08);
      --radius:20px;
    }
    html[data-theme="dark"]{
      --bg:#0f1722;
      --panel:#162231;
      --panel-soft:#1b2a3a;
      --line:#294054;
      --text:#eaf2fb;
      --muted:#9cb2c7;
      --accent:#6bc1ff;
      --accent-2:#52d4bf;
      --danger:#ff8a9a;
      --warn:#f3bb54;
      --shadow:0 24px 60px rgba(0,0,0,.28);
    }
    *{box-sizing:border-box}
    body{
      margin:0;
      font-family:Inter, "Segoe UI", sans-serif;
      background:
        radial-gradient(circle at top left, rgba(29,140,242,.12), transparent 24%),
        radial-gradient(circle at top right, rgba(39,176,154,.10), transparent 20%),
        var(--bg);
      color:var(--text);
    }
    .shell{display:grid;grid-template-columns:280px 1fr;min-height:100vh}
    .sidebar{
      border-right:1px solid var(--line);
      padding:28px 20px;
      background:rgba(255,255,255,.5);
      backdrop-filter: blur(14px);
      position:sticky;top:0;height:100vh;
    }
    html[data-theme="dark"] .sidebar{background:rgba(10,18,28,.55)}
    .brand{font-size:24px;font-weight:800;letter-spacing:-.04em}
    .brand small{display:block;font-size:12px;color:var(--muted);margin-top:6px;font-weight:600}
    .nav{margin-top:28px;display:grid;gap:10px}
    .nav button,.action,.secondary,.input,textarea,select{
      font:inherit
    }
    .nav button{
      text-align:left;border:1px solid transparent;background:transparent;color:var(--muted);
      padding:13px 14px;border-radius:14px;cursor:pointer;font-weight:600
    }
    .nav button.active,.nav button:hover{
      background:var(--panel);color:var(--text);border-color:var(--line);box-shadow:var(--shadow)
    }
    .theme-toggle{
      margin-top:auto;display:flex;gap:10px;padding-top:24px
    }
    .theme-toggle button,.action,.secondary{
      border:none;border-radius:14px;padding:12px 16px;cursor:pointer;font-weight:700
    }
    .theme-toggle button,.secondary{background:var(--panel);color:var(--text);border:1px solid var(--line)}
    .content{padding:28px 34px 40px}
    .topbar{display:flex;justify-content:space-between;align-items:center;gap:20px;margin-bottom:24px}
    .topbar h1{margin:0;font-size:32px;letter-spacing:-.05em}
    .topbar p{margin:6px 0 0;color:var(--muted)}
    .pill{
      display:inline-flex;align-items:center;gap:8px;padding:9px 12px;border-radius:999px;
      border:1px solid var(--line);background:var(--panel);font-weight:700;color:var(--text)
    }
    .grid-kpi{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:16px}
    .card{
      background:var(--panel);border:1px solid var(--line);border-radius:var(--radius);box-shadow:var(--shadow)
    }
    .kpi{padding:20px 22px}
    .kpi .eyebrow{color:var(--muted);font-size:13px;font-weight:700;text-transform:uppercase;letter-spacing:.08em}
    .kpi strong{display:block;font-size:34px;letter-spacing:-.05em;margin-top:10px}
    .layout{display:grid;grid-template-columns:1.35fr .95fr;gap:18px;margin-top:18px}
    .panel{padding:22px}
    .panel h2{margin:0 0 8px;font-size:20px;letter-spacing:-.04em}
    .panel .sub{color:var(--muted);margin-bottom:18px}
    .bars{display:grid;gap:14px}
    .bar-row{display:grid;grid-template-columns:140px 1fr 64px;gap:12px;align-items:center}
    .bar-track{height:12px;border-radius:999px;background:var(--panel-soft);overflow:hidden;border:1px solid var(--line)}
    .bar-fill{height:100%;background:linear-gradient(90deg,var(--accent),var(--accent-2))}
    .table{width:100%;border-collapse:collapse}
    .table th,.table td{padding:14px 12px;border-bottom:1px solid var(--line);text-align:left;font-size:14px}
    .table th{font-size:12px;text-transform:uppercase;letter-spacing:.08em;color:var(--muted)}
    .chip{display:inline-flex;padding:6px 10px;border-radius:999px;font-size:12px;font-weight:800}
    .chip.ok{background:rgba(39,176,154,.12);color:var(--accent-2)}
    .chip.warn{background:rgba(197,141,32,.12);color:var(--warn)}
    .chip.bad{background:rgba(216,91,106,.12);color:var(--danger)}
    .row-actions{display:flex;gap:8px;flex-wrap:wrap}
    .action{background:linear-gradient(135deg,var(--accent),#4ea8f7);color:white}
    .secondary.small,.action.small{padding:9px 12px;border-radius:10px;font-size:13px}
    .section{display:none}
    .section.active{display:block}
    .toolbar{display:flex;justify-content:space-between;align-items:center;gap:14px;margin-bottom:18px}
	    .logs{
	      background:#0e1721;color:#d5f4ff;border-radius:18px;padding:18px;height:460px;overflow:auto;
	      font-family:"SFMono-Regular",Consolas,monospace;font-size:12px;line-height:1.5;
	      white-space:pre-wrap;overflow-wrap:anywhere;word-break:break-word
	    }
    .readme{white-space:pre-wrap;line-height:1.6;color:var(--text)}
    .input,textarea,select{
      width:100%;padding:12px 14px;border-radius:14px;border:1px solid var(--line);background:var(--panel-soft);color:var(--text)
    }
    textarea{min-height:180px;resize:vertical}
    .form-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:14px}
    .stack{display:grid;gap:14px}
    .muted{color:var(--muted)}
    .banner{
      margin-bottom:18px;padding:14px 16px;border-radius:16px;background:rgba(29,140,242,.09);
      border:1px solid rgba(29,140,242,.18);color:var(--text);display:none
    }
    .banner.show{display:block}
    .split{display:grid;grid-template-columns:1.15fr .85fr;gap:18px}
    .file-row{display:flex;gap:10px;align-items:center}
    .tiny{font-size:12px;color:var(--muted)}
    @media (max-width: 1180px){
      .grid-kpi,.layout,.split,.form-grid{grid-template-columns:1fr}
      .shell{grid-template-columns:1fr}
      .sidebar{position:static;height:auto}
      .content{padding:22px}
    }
  </style>
</head>
<body>
  <div class="shell">
    <aside class="sidebar">
      <div class="brand">AISTUDIOPROXYPLUS<small>Proxy AI Studio local com contas por cookies Netscape</small></div>
      <div class="nav">
        <button class="active" data-target="dashboard">Dashboard</button>
        <button data-target="accounts">Contas</button>
        <button data-target="config">Config</button>
        <button data-target="logs">Logs</button>
        <button data-target="models">Modelos</button>
        <button data-target="readme">README</button>
      </div>
      <div class="theme-toggle">
        <button id="themeBtn">Alternar tema</button>
      </div>
    </aside>
    <main class="content">
      <div class="topbar">
        <div>
          <h1>Painel Operacional</h1>
          <p>Contas, cookies, saúde do proxy e observabilidade em um só lugar.</p>
        </div>
        <div class="pill" id="statusPill">Carregando…</div>
      </div>

      <div class="banner" id="banner"></div>

      <section class="section active" id="dashboard">
        <div class="grid-kpi" id="kpiGrid"></div>
        <div class="layout">
          <div class="card panel">
            <h2>Requisições por conta</h2>
            <div class="sub">Distribuição de uso e presença de sessão válida.</div>
            <div class="bars" id="usageBars"></div>
          </div>
          <div class="card panel">
            <h2>Estado rápido</h2>
            <div class="sub">Leitura operacional por conta.</div>
            <div class="stack" id="statusCards"></div>
          </div>
        </div>
      </section>

      <section class="section" id="accounts">
        <div class="split">
          <div class="card panel">
            <div class="toolbar">
              <div>
                <h2>Gerenciar contas</h2>
                <div class="sub">Importe cookies válidos, valide sessão, troque padrão ou remova.</div>
              </div>
              <button class="secondary" id="refreshAccountsBtn">Atualizar</button>
            </div>
            <table class="table">
              <thead>
                <tr>
                  <th>Conta</th>
                  <th>Status</th>
                  <th>Uso</th>
                  <th>Ações</th>
                </tr>
              </thead>
              <tbody id="accountsTable"></tbody>
            </table>
          </div>
          <div class="card panel">
            <h2>Adicionar ou substituir cookies</h2>
            <div class="sub">Cole o conteúdo Netscape ou carregue um arquivo .txt exportado por uma extensão como Locally.</div>
            <div class="stack">
              <div class="form-grid">
                <label><div class="tiny">ID da conta</div><input class="input" id="accountId" placeholder="account1"></label>
                <label><div class="tiny">Label</div><input class="input" id="accountLabel" placeholder="Conta principal"></label>
              </div>
              <label><div class="tiny">Email</div><input class="input" id="accountEmail" placeholder="voce@gmail.com"></label>
              <div class="file-row">
                <input type="file" id="cookiesFile" accept=".txt">
                <button class="secondary small" id="loadFileBtn">Carregar arquivo</button>
              </div>
              <label><div class="tiny">Cookies Netscape</div><textarea id="cookiesText" placeholder="# Netscape HTTP Cookie File&#10;.google.com TRUE / TRUE 1893456000 SAPISID valor"></textarea></label>
              <button class="action" id="importBtn">Salvar conta com cookies</button>
              <div class="tiny">Quando os cookies expirarem, basta substituir por novos válidos neste mesmo formulário.</div>
            </div>
          </div>
        </div>
      </section>

      <section class="section" id="config">
        <div class="card panel">
          <div class="toolbar">
            <div>
              <h2>Configurações</h2>
              <div class="sub">Persistidas para o próximo restart do proxy.</div>
            </div>
            <div class="row-actions">
              <button class="secondary" id="saveConfigBtn">Salvar</button>
              <button class="action" id="restartBtn">Reiniciar proxy</button>
            </div>
          </div>
          <div class="form-grid">
            <label><div class="tiny">Browser mode</div>
              <select id="browserMode">
                <option value="headless_spoof">headless_spoof</option>
                <option value="headless_raw">headless_raw</option>
                <option value="visible_legacy">visible_legacy</option>
              </select>
            </label>
            <label><div class="tiny">Conversation mode</div>
              <select id="conversationMode">
                <option value="stateless">stateless</option>
                <option value="stateful">stateful</option>
              </select>
            </label>
            <label><div class="tiny">Tool calling mode</div>
              <select id="toolCallingMode">
                <option value="bridge_first">bridge_first</option>
                <option value="native_first">native_first</option>
              </select>
            </label>
            <label><div class="tiny">Tool stream mode</div>
              <select id="toolStreamMode">
                <option value="buffered">buffered</option>
                <option value="hybrid">hybrid</option>
                <option value="live">live</option>
              </select>
            </label>
            <label><div class="tiny">Debug message flow</div>
              <select id="debugMessageFlow">
                <option value="false">false</option>
                <option value="true">true</option>
              </select>
            </label>
            <label><div class="tiny">Migrações por request</div><input class="input" id="migrationHops" type="number" min="1" max="9"></label>
            <label><div class="tiny">Perfil padrão</div><input class="input" id="defaultProfile" list="profileOptions" placeholder="1"></label>
            <label><div class="tiny">Eager boot</div>
              <select id="eagerBoot">
                <option value="default">default</option>
                <option value="all">all</option>
                <option value="none">none</option>
              </select>
            </label>
            <label style="grid-column:1/-1"><div class="tiny">CDP WS endpoint (avançado)</div><input class="input" id="cdpWsEndpoint" placeholder="ws://127.0.0.1:9222/devtools/browser/..."></label>
          </div>
          <datalist id="profileOptions"></datalist>
        </div>
      </section>

      <section class="section" id="logs">
        <div class="card panel">
          <h2>Logs em tempo real</h2>
          <div class="sub">Eventos do proxy, requests e ações administrativas.</div>
          <div class="logs" id="logsBox"></div>
        </div>
      </section>

      <section class="section" id="models">
        <div class="card panel">
          <h2>Modelos disponíveis</h2>
          <div class="sub">Lista conhecida pelo proxy para a sessão atual.</div>
          <table class="table">
            <thead><tr><th>Modelo</th><th>Tipo</th></tr></thead>
            <tbody id="modelsTable"></tbody>
          </table>
        </div>
      </section>

      <section class="section" id="readme">
        <div class="card panel">
          <h2>README</h2>
          <div class="sub">Referência rápida de uso e operação.</div>
          <div class="readme" id="readmeBox">Carregando…</div>
        </div>
      </section>
    </main>
  </div>
  <script>
    const state = { stats:null, accounts:null, models:null, config:null, eventSource:null };
    const q = (s)=>document.querySelector(s);
    const qa = (s)=>Array.from(document.querySelectorAll(s));
    const fmtNum = (v)=>new Intl.NumberFormat('pt-BR').format(v||0);
    const fmtPct = (v)=>(v||0).toFixed(1) + '%';
    const fmtDate = (v)=>v ? new Date(v).toLocaleString('pt-BR') : '—';

    function showBanner(text, tone='info'){
      const banner=q('#banner');
      banner.textContent=text;
      banner.className='banner show';
      if(tone==='error') banner.style.borderColor='rgba(216,91,106,.25)';
      setTimeout(()=>banner.className='banner', 5000);
    }

    function setTheme(next){
      document.documentElement.dataset.theme=next;
      localStorage.setItem('aistudio-theme', next);
    }

    function initNav(){
      qa('.nav button').forEach(btn=>{
        btn.addEventListener('click', ()=>{
          qa('.nav button').forEach(b=>b.classList.remove('active'));
          qa('.section').forEach(s=>s.classList.remove('active'));
          btn.classList.add('active');
          q('#' + btn.dataset.target).classList.add('active');
        });
      });
      q('#themeBtn').addEventListener('click', ()=>{
        setTheme(document.documentElement.dataset.theme==='dark'?'light':'dark');
      });
      setTheme(localStorage.getItem('aistudio-theme') || 'light');
    }

    async function api(path, options={}){
      const res=await fetch(path,{headers:{'Content-Type':'application/json'},...options});
      const data=await res.json().catch(()=>({}));
      if(!res.ok){
        throw new Error(data?.error?.message || data?.message || 'Erro na API');
      }
      return data;
    }

    function renderStats(){
      const stats=state.stats?.stats || {};
      q('#statusPill').textContent=(stats.logged_accounts||0) + ' contas logadas • ' + (stats.active_accounts||0) + ' ativas';
      const items=[
        ['Contas logadas', stats.logged_accounts||0],
        ['Contas ativas', stats.active_accounts||0],
        ['Requests', fmtNum(stats.requests||0)],
        ['Sucesso médio', fmtPct(stats.success_rate||0)],
        ['Latência média', fmtNum(stats.avg_latency_ms||0) + ' ms'],
        ['Tokens usados', stats.tokens_supported ? fmtNum(stats.tokens_used||0) : 'Indisponível'],
        ['Prompt caching', state.stats?.prompt_cache?.status || 'indisponível'],
        ['Cooldowns', (state.stats?.cards || []).filter(c=>c.cooldown_until).length]
      ];
      q('#kpiGrid').innerHTML = items.map(function(item){
        return '<article class="card kpi"><div class="eyebrow">' + item[0] + '</div><strong>' + item[1] + '</strong></article>';
      }).join('');

      const metrics=state.stats?.metrics?.by_profile || {};
      const max=Math.max(1,...Object.values(metrics).map(m=>m.requests||0));
      q('#usageBars').innerHTML = Object.entries(metrics).map(function(entry){
        var id = entry[0];
        var m = entry[1];
        var width = ((m.requests||0) / max) * 100;
        return '<div class="bar-row"><strong>' + id + '</strong><div class="bar-track"><div class="bar-fill" style="width:' + width + '%"></div></div><span>' + fmtNum(m.requests||0) + '</span></div>';
      }).join('') || '<div class="muted">Sem tráfego registrado ainda.</div>';

      q('#statusCards').innerHTML = (state.stats?.cards || []).map(function(card){
        var hasOperationalError = !!card.last_error;
        var tone = !card.logged_in ? 'bad' : (card.active ? (hasOperationalError ? 'warn' : 'ok') : 'warn');
        var status = !card.logged_in ? 'Inválida' : (card.active ? (hasOperationalError ? 'Atenção' : 'Ativa') : 'Cooldown');
        var errorBlock = card.last_error ? '<div class="tiny" style="margin-top:6px;color:var(--danger)">Erro: ' + card.last_error + '</div>' : '';
        return '<div class="card" style="padding:16px 18px"><div style="display:flex;justify-content:space-between;gap:10px;align-items:center"><div><strong>' + card.label + '</strong><div class="tiny">' + card.id + '</div></div><span class="chip ' + tone + '">' + status + '</span></div><div class="tiny" style="margin-top:10px">Requests: ' + fmtNum(card.requests) + ' • Cooldown: ' + (card.cooldown_until ? fmtDate(card.cooldown_until) : 'livre') + '</div>' + errorBlock + '</div>';
      }).join('');
    }

    function renderAccounts(){
      const rows=state.accounts?.profiles || [];
      q('#profileOptions').innerHTML = rows.map(function(row){
        return '<option value="' + row.id + '">' + (row.email || row.label || row.id) + '</option>';
      }).join('');
      // Banner de sessão VNC ativa (se houver).
      renderVNCBanner();
      const vncAttachedId = (state.vncStatus && state.vncStatus.attached) ? state.vncStatus.profile_id : null;
      q('#accountsTable').innerHTML = rows.map(function(row){
        var hasOperationalError = !!row.last_error;
        var tone = !row.is_valid ? 'bad' : (row.available ? (hasOperationalError ? 'warn' : 'ok') : 'warn');
        var status = !row.is_valid ? 'Inválida' : (row.available ? (hasOperationalError ? 'Atenção' : 'Pronta') : 'Cooldown');
        var label = String(row.label || '').replace(/'/g, '&#39;');
        var email = String(row.email || '').replace(/'/g, '&#39;');
        var defaultBtn = row.default ? '' : '<button class="secondary small" onclick="setDefaultAccount(\'' + row.id + '\')">Tornar padrão</button>';
        // Botões VNC: se esta conta está no VNC, mostrar "Finalizar login"; senão, "Login via noVNC".
        var vncBtn;
        if (vncAttachedId === row.id) {
          vncBtn = '<button class="action small" style="background:linear-gradient(135deg,var(--accent-2),#3ec4ad)" onclick="vncDetach(\'' + row.id + '\')">🚪 Finalizar login</button>';
        } else if (vncAttachedId) {
          // Outra conta está usando o VNC — botão desabilitado.
          vncBtn = '<button class="secondary small" disabled title="Outra conta está em sessão VNC">🔌 Login via noVNC</button>';
        } else {
          vncBtn = '<button class="secondary small" onclick="vncAttach(\'' + row.id + '\')">🔌 Login via noVNC</button>';
        }
        return '<tr><td><strong>' + row.label + '</strong><div class="tiny">' + (row.email || row.id) + (row.default ? ' • padrão' : '') + '</div></td><td><span class="chip ' + tone + '">' + status + '</span><div class="tiny">' + (row.validation_error || row.last_error || row.login_mode || 'sem alertas') + '</div></td><td><div>' + fmtNum(row.requests || 0) + ' req</div><div class="tiny">' + fmtNum(row.total_tokens || 0) + ' tokens • ' + fmtNum(row.avg_latency_ms || 0) + ' ms</div></td><td><div class="row-actions">' + defaultBtn + vncBtn + '<button class="secondary small" onclick="validateAccount(\'' + row.id + '\')">Validar</button><button class="secondary small" onclick="prefillReplace(\'' + row.id + '\',\'' + label + '\',\'' + email + '\')">Substituir cookies</button><button class="secondary small" onclick="deleteAccount(\'' + row.id + '\')">Remover</button></div></td></tr>';
      }).join('') || '<tr><td colspan="4" class="muted">Nenhuma conta cadastrada.</td></tr>';
    }

    function renderVNCBanner(){
      const banner = q('#vncBanner');
      if (!banner) return;
      if (state.vncStatus && state.vncStatus.attached) {
        const pid = state.vncStatus.profile_id || '?';
        const label = state.vncStatus.label || state.vncStatus.email || pid;
        const host = window.location.hostname || 'localhost';
        const vncPort = window.location.port ? (parseInt(window.location.port) + 3000 - 3001 + 6080 - 3001) : 6080;
        // Heurística: porta noVNC = porta proxy + 3079 (3001->6080). Se não bater, usa 6080.
        const novncUrl = 'http://' + host + ':6080/vnc.html';
        banner.style.display = 'block';
        banner.innerHTML = '🖥️ <strong>Sessão VNC ativa:</strong> ' + label +
          ' — <a href="' + novncUrl + '" target="_blank" style="color:var(--accent);font-weight:700">abrir noVNC</a>' +
          ' <span class="tiny">(faça login no Google AI Studio nesta janela e clique em "Finalizar login")</span>';
      } else {
        banner.style.display = 'none';
        banner.innerHTML = '';
      }
    }

    function renderModels(){
      const rows=state.models?.data || [];
      q('#modelsTable').innerHTML = rows.map(function(row){
        return '<tr><td><strong>' + (row.name || row.id) + '</strong><div class="tiny">' + row.id + '</div></td><td>' + (row.type || 'model') + '</td></tr>';
      }).join('');
    }

    function applyConfig(){
      if(!state.config) return;
      q('#browserMode').value=state.config.browser_mode || 'headless_spoof';
      q('#conversationMode').value=state.config.conversation_mode || 'stateless';
      q('#toolCallingMode').value=state.config.tool_calling_mode || 'native_first';
      q('#toolStreamMode').value=state.config.tool_stream_mode || 'buffered';
      q('#debugMessageFlow').value=state.config.debug_message_flow || 'false';
      q('#migrationHops').value=state.config.migration_hops || '2';
      q('#defaultProfile').value=state.config.default_profile || '';
      q('#eagerBoot').value=state.config.eager_boot || 'none';
      q('#cdpWsEndpoint').value=state.config.cdp_ws_endpoint || '';
    }

    function prefillReplace(id,label,email){
      q('#accountId').value=id;
      q('#accountLabel').value=label;
      q('#accountEmail').value=email;
      q('#cookiesText').focus();
    }

    async function refreshAll(){
      const [stats,accounts,models,config,readme,vncStatus] = await Promise.all([
        api('/admin/api/stats'),
        api('/admin/api/accounts'),
        api('/admin/api/models'),
        api('/admin/api/config'),
        api('/admin/api/readme'),
        api('/admin/api/vnc/status').catch(()=>({attached:false,profile_id:null}))
      ]);
      state.stats=stats; state.accounts=accounts; state.models=models; state.config=config; state.vncStatus=vncStatus;
      q('#readmeBox').textContent=readme.content || 'README indisponível';
      renderStats(); renderAccounts(); renderModels(); applyConfig();
    }

    async function importAccount(){
      try{
        await api('/admin/api/accounts/import', {
          method:'POST',
          body:JSON.stringify({
            profile_id:q('#accountId').value.trim(),
            label:q('#accountLabel').value.trim(),
            email:q('#accountEmail').value.trim(),
            cookies_text:q('#cookiesText').value
          })
        });
        showBanner('Conta importada e validada com sucesso.');
        q('#cookiesText').value='';
        await refreshAll();
      }catch(err){ showBanner(err.message, 'error'); }
    }

    async function deleteAccount(id){
      if(!confirm('Remover ' + id + '?')) return;
      try{
        await api('/admin/api/accounts/delete', {method:'POST', body:JSON.stringify({profile_id:id})});
        showBanner('Conta removida.');
        await refreshAll();
      }catch(err){ showBanner(err.message, 'error'); }
    }

    async function setDefaultAccount(id){
      try{
        await api('/admin/api/accounts/default', {method:'POST', body:JSON.stringify({profile_id:id})});
        showBanner('Conta padrão atualizada.');
        await refreshAll();
      }catch(err){ showBanner(err.message, 'error'); }
    }

    async function validateAccount(id){
      try{
        const res = await api('/admin/api/accounts/validate', {method:'POST', body:JSON.stringify({profile_id:id})});
        showBanner(res.message || 'Conta validada.');
        await refreshAll();
      }catch(err){ showBanner(err.message, 'error'); }
    }

    async function vncAttach(id){
      if(!confirm('Abrir sessão VNC para login desta conta?\n\nVocê será direcionado ao noVNC para fazer login no Google AI Studio. Após logar, clique em "Finalizar login" nesta tela.')) return;
      try{
        const res = await api('/admin/api/vnc/attach', {method:'POST', body:JSON.stringify({profile_id:id})});
        showBanner(res.message || 'Sessão VNC aberta. Abra http://<host>:6080/vnc.html para fazer login.');
        await refreshAll();
        // Abre o noVNC em nova aba para facilitar o login.
        const host = window.location.hostname || 'localhost';
        window.open('http://' + host + ':6080/vnc.html', '_blank');
      }catch(err){ showBanner(err.message, 'error'); }
    }

    async function vncDetach(id){
      if(!confirm('Finalizar login e voltar ao modo headless?\n\nO proxy irá validar a sessão e reiniciar o Chrome em modo headless_spoof.')) return;
      try{
        const res = await api('/admin/api/vnc/detach', {method:'POST', body:JSON.stringify({profile_id:id})});
        showBanner(res.message || 'Login finalizado. Proxy voltando ao modo headless.');
        await refreshAll();
      }catch(err){ showBanner(err.message, 'error'); }
    }

    async function saveConfig(){
      try{
        await api('/admin/api/config', {
          method:'POST',
          body:JSON.stringify({
            browser_mode:q('#browserMode').value,
            conversation_mode:q('#conversationMode').value,
            tool_calling_mode:q('#toolCallingMode').value,
            tool_stream_mode:q('#toolStreamMode').value,
            debug_message_flow:q('#debugMessageFlow').value,
            migration_hops:q('#migrationHops').value,
            default_profile:q('#defaultProfile').value.trim(),
            eager_boot:q('#eagerBoot').value,
            cdp_ws_endpoint:q('#cdpWsEndpoint').value.trim()
          })
        });
        showBanner('Configurações salvas. Reinicie o proxy para aplicar.');
      }catch(err){ showBanner(err.message, 'error'); }
    }

    async function restartProxy(){
      if(!confirm('Reiniciar o proxy agora?')) return;
      try{
        await api('/admin/api/restart', {method:'POST'});
        showBanner('Reinício solicitado. Aguarde alguns segundos.');
      }catch(err){ showBanner(err.message, 'error'); }
    }

    function initLogs(){
      const box=q('#logsBox');
    window.vncAttach=vncAttach;
    window.vncDetach=vncDetach;
      state.eventSource = new EventSource('/admin/api/logs');
      state.eventSource.onmessage=(evt)=>{
        const entry=JSON.parse(evt.data);
        box.textContent += '[' + entry.timestamp + '] ' + entry.line + '\n';
        box.scrollTop = box.scrollHeight;
      };
    }

    function initFileImport(){
      q('#loadFileBtn').addEventListener('click', ()=>{
        const file=q('#cookiesFile').files[0];
        if(!file){ showBanner('Escolha um arquivo .txt primeiro.', 'error'); return; }
        const reader=new FileReader();
        reader.onload=()=>{ q('#cookiesText').value=reader.result || ''; showBanner('Arquivo carregado no campo de cookies.'); };
        reader.readAsText(file);
      });
    }

    window.deleteAccount=deleteAccount;
    window.setDefaultAccount=setDefaultAccount;
    window.validateAccount=validateAccount;
    window.prefillReplace=prefillReplace;

    initNav();
    initLogs();
    initFileImport();
    q('#importBtn').addEventListener('click', importAccount);
    q('#saveConfigBtn').addEventListener('click', saveConfig);
    q('#restartBtn').addEventListener('click', restartProxy);
    q('#refreshAccountsBtn').addEventListener('click', refreshAll);
    refreshAll().catch(err=>showBanner(err.message, 'error'));
  </script>
</body>
</html>`
