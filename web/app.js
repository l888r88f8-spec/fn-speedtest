const $ = (id) => document.getElementById(id);
const els = {
  sourceHint: $('sourceHint'), start: $('startButton'), actionLabel: $('actionLabel'), actionIcon: $('actionIcon'), profile: $('profile'), cnServerSelect: $('cnServerSelect'), netServerSelect: $('netServerSelect'), cnServerCount: $('cnServerCount'), netServerCount: $('netServerCount'), refreshServers: $('refreshServersButton'), progressWrap: $('progressWrap'),
  progressBar: $('progressBar'), progressText: $('progressText'), phase: $('phaseText'), error: $('actionError'),
  down: $('downloadValue'), up: $('uploadValue'), latency: $('latencyValue'), jitter: $('jitterValue'),
  downProgress: $('downloadProgressBar'), upProgress: $('uploadProgressBar'), downProgressText: $('downloadProgressText'), upProgressText: $('uploadProgressText'),
  server: $('serverValue'), sponsor: $('sponsorValue'), isp: $('ispValue'), region: $('regionValue'), ip: $('ipValue'), body: $('historyBody'), empty: $('emptyHistory'),
  clear: $('clearButton'), version: $('version')
};
let polling = null;
let historyItems = [];
let nearbyServers = [];
let currentNetwork = null;
let testRunning = false;
let serversReady = false;
let serversLoading = false;
let testStatus = 'idle';
let actionPending = '';
let stateEpoch = 0;
let refreshing = false;
let selectedServerID = '';

async function api(path, options = {}) {
  const response = await fetch(`api/${path}`, {headers: {'Content-Type': 'application/json'}, ...options});
  const raw = await response.text();
  let data = {};
  if (raw) {
    try { data = JSON.parse(raw); }
    catch {
      if (!response.ok) {
        const summary = raw.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim().slice(0, 120);
        throw new Error(summary ? `HTTP ${response.status}：${summary}` : `HTTP ${response.status}`);
      }
      throw new Error(`服务返回格式异常（HTTP ${response.status}）`);
    }
  }
  if (!response.ok) { const error = new Error(data.error || `HTTP ${response.status}`); error.sources = data.sources; throw error; }
  return data;
}

const n = (value) => Number(value).toLocaleString('zh-CN', {maximumFractionDigits: 2});
const profileName = {quick: '快速', standard: '标准', deep: '深度'};
const escapeHTML = (value) => String(value ?? '').replace(/[&<>"']/g, char => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[char]));

function showError(message = '') {
  els.error.textContent = message;
  els.error.classList.toggle('hidden', !message);
}

function renderControls() {
  const stopping = actionPending === 'cancelling' || testStatus === 'cancelling';
  const starting = actionPending === 'starting';
  const running = testStatus === 'running' && !stopping;
  testRunning = running || stopping || starting;
  els.actionLabel.textContent = stopping ? '正在停止…' : starting ? '正在开始…' : running ? '取消测速' : '开始测速';
  els.actionIcon.textContent = running ? '■' : testRunning ? '…' : '→';
  els.start.classList.toggle('is-cancel', running || stopping);
  els.start.disabled = stopping || starting || (!running && !serversReady);
  els.profile.disabled = testRunning;
  els.cnServerSelect.disabled = testRunning || !serversReady;
  els.netServerSelect.disabled = testRunning || !serversReady;
  els.refreshServers.disabled = testRunning || serversLoading;
  els.progressWrap.classList.toggle('hidden', !running);
  document.querySelectorAll('.metric strong').forEach(el => el.classList.toggle('pulse', running));
}

function setMetricProgress(bar, label, value, waitingText) {
  const percent = Math.max(0, Math.min(100, Number(value) || 0));
  bar.style.width = `${percent}%`;
  label.textContent = percent > 0 ? `${Math.round(percent)}%` : waitingText;
}

function showNetwork(network, isp, ip) {
  const identity = network || {};
  els.isp.textContent = identity.carrier || isp || identity.isp || '未识别运营商';
  els.ip.textContent = ip || identity.publicIp || '未获取';
  els.region.textContent = identity.province ? `${identity.province} · IP 归属地` : identity.countryCode && identity.countryCode !== 'CN' ? '境外出口' : '省份未识别';
  els.region.title = '按 NAS 公网出口 IP 识别，可能与实际所在地不同';
}

function serverLabel(server) {
  const placeParts = [];
  if (server.province) placeParts.push(server.province);
  if (server.name && server.name !== server.province) placeParts.push(server.name);
  if (!placeParts.length && server.country) placeParts.push(server.country);
  const place = placeParts.join(' ');
  const carrier = server.carrier || server.sponsor || '未知网络';
  const distance = Number(server.distanceKm) > 0 ? `｜${n(server.distanceKm)} km` : '';
  const measured = Number(server.latencyMs) > 0;
  let latency = measured ? `${n(server.latencyMs)} ms` : '延时待实测';
  if (server.kind === 'speedtestcn') {
    latency = `${server.bandwidthReady ? '测速协议可达' : '待验证'}｜${latency}`;
  } else if (measured && Number(server.jitterMs) > 0) {
    latency += `｜抖动 ${n(server.jitterMs)} ms`;
  }
  return `${place || '未知地区'}｜${carrier}｜${latency}${distance}`;
}

function serverEngine(server) {
  return server.engine || (server.kind === 'speedtestcn' ? 'Speedtest.cn' : 'Speedtest.net');
}

function serverReachable(server) {
  return server.kind === 'speedtestcn' ? Boolean(server.bandwidthReady) : true;
}

function displayLatency(server) {
  const value = Number(server.latencyMs);
  return Number.isFinite(value) && value > 0 ? value : Number.POSITIVE_INFINITY;
}

function sortServersForDisplay(servers) {
  return [...servers].sort((a, b) => {
    if (serverReachable(a) !== serverReachable(b)) return serverReachable(a) ? -1 : 1;
    const latencyDiff = displayLatency(a) - displayLatency(b);
    if (Number.isFinite(latencyDiff) && latencyDiff !== 0) return latencyDiff;
    if (displayLatency(a) !== displayLatency(b)) return displayLatency(a) < displayLatency(b) ? -1 : 1;
    return 0;
  });
}

function serverColumn(server) {
  return server.kind === 'speedtest' || server.engine === 'Speedtest.net' ? 'net' : 'cn';
}

function chooseServer(id, column) {
  if (!id || !nearbyServers.some(server => server.id === id)) return;
  selectedServerID = id;
  if (column === 'cn') {
    els.cnServerSelect.value = id;
    els.netServerSelect.value = '';
  } else {
    els.netServerSelect.value = id;
    els.cnServerSelect.value = '';
  }
  updateSelectedServerDetails();
}

function updateSelectedServerDetails() {
  const selected = nearbyServers.find(server => server.id === selectedServerID);
  const recommended = nearbyServers.find(server => server.recommended);
  const server = selected || recommended;
  if (!server) return;
  const location = [server.province, server.name].filter((value, index, items) => value && items.indexOf(value) === index).join(' · ');
  const id = String(server.id).startsWith('http:') ? '' : ` · #${server.id}`;
  els.server.textContent = `${serverEngine(server)} · ${location || server.country || '未知地区'}${id}`;
  els.sponsor.textContent = server.carrier || server.sponsor || '未提供';
  els.sponsor.classList.toggle('carrier-match', Boolean(server.carrierMatched));
}

function showSourceStatus(sources) {
  const cn = Array.isArray(sources) ? sources.find(source => source.id === 'speedtestcn') : null;
  const message = cn && cn.status !== 'available' && cn.status !== 'disabled' ? `Speedtest.cn：${cn.message || '暂无可用中国节点'}` : '';
  els.sourceHint.textContent = message;
  els.sourceHint.classList.toggle('hidden', !message);
}

async function loadServers() {
  if (testRunning || serversLoading) return;
  serversLoading = true;
  showError();
  showSourceStatus([]);
  const previous = selectedServerID;
  serversReady = false;
  els.start.disabled = true;
  els.refreshServers.disabled = true;
  els.cnServerSelect.disabled = true;
  els.netServerSelect.disabled = true;
  els.cnServerSelect.replaceChildren(new Option('正在加载 CN 节点…', ''));
  els.netServerSelect.replaceChildren(new Option('正在加载 Speedtest.net 节点…', ''));
  els.cnServerCount.textContent = '…';
  els.netServerCount.textContent = '…';
  try {
    const data = await api('servers');
    showSourceStatus(data.sources);
    nearbyServers = Array.isArray(data.servers) ? data.servers : [];
    const cnServers = sortServersForDisplay(nearbyServers.filter(server => serverColumn(server) === 'cn'));
    const netServers = sortServersForDisplay(nearbyServers.filter(server => serverColumn(server) === 'net'));
    const recommended = nearbyServers.find(server => server.id === data.recommendedId) || nearbyServers.find(server => server.recommended) || cnServers[0] || netServers[0];
    if (!recommended) throw new Error('没有找到可用的测速节点');

    const makeOptions = (servers, placeholder) => [
      new Option(placeholder, ''),
      ...servers.map(server => new Option(
        `${server.id === recommended.id ? '★ ' : ''}${serverLabel(server)}`,
        server.id
      ))
    ];
    els.cnServerSelect.replaceChildren(...makeOptions(cnServers, cnServers.length ? '选择 CN / 国内节点' : '暂无 CN 节点'));
    els.netServerSelect.replaceChildren(...makeOptions(netServers, netServers.length ? '选择 Speedtest.net 节点' : '暂无 Speedtest.net 节点'));
    els.cnServerCount.textContent = `${cnServers.length} 个`;
    els.netServerCount.textContent = `${netServers.length} 个`;

    const preferred = previous && nearbyServers.some(server => server.id === previous) ? previous : recommended.id;
    const preferredServer = nearbyServers.find(server => server.id === preferred);
    selectedServerID = preferred;
    if (preferredServer && serverColumn(preferredServer) === 'net') {
      els.netServerSelect.value = preferred;
      els.cnServerSelect.value = '';
    } else {
      els.cnServerSelect.value = preferred;
      els.netServerSelect.value = '';
    }
    serversReady = Boolean(selectedServerID);
    currentNetwork = data.network || null;
    showNetwork(currentNetwork, data.isp, data.publicIp);
    updateSelectedServerDetails();
  } catch (error) {
    if (error.sources) showSourceStatus(error.sources);
    nearbyServers = [];
    selectedServerID = '';
    els.cnServerSelect.replaceChildren(new Option('请刷新 CN 节点', ''));
    els.netServerSelect.replaceChildren(new Option('请刷新 Speedtest.net 节点', ''));
    els.cnServerCount.textContent = '—';
    els.netServerCount.textContent = '—';
    showError(`节点加载失败：${error.message}`);
  } finally {
    serversLoading = false;
    renderControls();
  }
}

function showResult(result) {
  if (!result) return;
  els.down.textContent = n(result.downloadMbps);
  els.up.textContent = n(result.uploadMbps);
  const hasLatency = Number(result.latencyMs) > 0;
  els.latency.textContent = hasLatency ? n(result.latencyMs) : '—';
  els.jitter.textContent = hasLatency ? `抖动 ${n(result.jitterMs)} ms` : '抖动 —';
  const resultId = result.serverId && !String(result.serverId).startsWith('http:') ? ` · #${result.serverId}` : '';
  els.server.textContent = result.serverLocation ? `${result.engine ? `${result.engine} · ` : ''}${result.serverLocation}${resultId}` : '自动匹配节点';
  els.sponsor.textContent = result.serverSponsor || '未提供';
  els.sponsor.classList.toggle('carrier-match', Boolean(result.carrierMatched));
  els.sponsor.title = result.carrierMatched ? '已匹配同运营商节点' : '';
  const network = result.network || (currentNetwork?.publicIp === result.publicIp ? currentNetwork : null);
  showNetwork(network, result.isp, result.publicIp);
  setMetricProgress(els.downProgress, els.downProgressText, 100, '等待下载测试');
  setMetricProgress(els.upProgress, els.upProgressText, 100, '等待上传测试');
}

function phaseName(phase) {
  return {preparing:'连接测速节点', detecting:'识别公网网络', connecting:'连接已选节点', latency:'测量延迟', download:'测量下载', upload:'测量上传', finishing:'生成报告'}[phase] || '测速中';
}

async function refreshState() {
  if (refreshing || actionPending) return;
  refreshing = true;
  const epoch = stateEpoch;
  try {
    const state = await api('state');
    // Discard polls started before a click, so stale samples cannot unfreeze the UI.
    if (epoch !== stateEpoch || actionPending) return;
    testStatus = state.status;
    renderControls();
    if (testRunning && !polling) polling = setInterval(refreshState, 400);
    if (state.status === 'running') {
      els.progressBar.style.width = `${state.progress || 0}%`;
      els.progressText.textContent = `${state.progress || 0}%`;
      els.phase.textContent = phaseName(state.phase);
      if (Number(state.liveDownloadMbps) > 0) els.down.textContent = n(state.liveDownloadMbps);
      if (Number(state.liveUploadMbps) > 0) els.up.textContent = n(state.liveUploadMbps);
      if (Number(state.liveLatencyMs) > 0) els.latency.textContent = n(state.liveLatencyMs);
      setMetricProgress(els.downProgress, els.downProgressText, state.downloadProgress, state.phase === 'download' ? '正在开始下载测试' : '等待下载测试');
      setMetricProgress(els.upProgress, els.upProgressText, state.uploadProgress, state.phase === 'upload' ? '正在开始上传测试' : '等待上传测试');
    } else if (!testRunning) {
      if (polling) { clearInterval(polling); polling = null; }
      if (state.status === 'complete') {
        showResult(state.result);
        await loadHistory();
      } else if (state.status === 'error') {
        showError(state.message || '测速失败，请重试');
      }
    }
  } catch (error) {
    if (epoch === stateEpoch) showError(`服务连接异常：${error.message}`);
  } finally { refreshing = false; }
}

async function startTest() {
  if (testRunning || actionPending || !serversReady) return;
  const input = {profile:els.profile.value, serverId:selectedServerID};
  actionPending = 'starting';
  stateEpoch++;
  renderControls();
  showError();
  try {
    els.down.textContent = els.up.textContent = els.latency.textContent = '—';
    els.jitter.textContent = '抖动 — ms';
    setMetricProgress(els.downProgress, els.downProgressText, 0, '等待下载测试');
    setMetricProgress(els.upProgress, els.upProgressText, 0, '等待上传测试');
    await api('test', {method:'POST', body:JSON.stringify(input)});
    testStatus = 'running';
  } catch (error) { showError(error.message); }
  finally {
    actionPending = '';
    renderControls();
    if (!polling) polling = setInterval(refreshState, 400);
    await refreshState();
  }
}

async function cancelTest() {
  if (testStatus !== 'running' || actionPending) return;
  actionPending = 'cancelling';
  stateEpoch++;
  renderControls();
  showError();
  try {
    await api('test/cancel', {method:'POST', body:'{}'});
    testStatus = 'cancelling';
  } catch (error) { showError(error.message); }
  finally {
    actionPending = '';
    renderControls();
    if (!polling) polling = setInterval(refreshState, 400);
    await refreshState();
  }
}

function formatTime(iso) {
  const d = new Date(iso);
  return d.toLocaleString('zh-CN', {month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'}).replaceAll('/','-');
}

async function loadHistory() {
  const history = await api('history');
  historyItems = Array.isArray(history) ? history : [];
  els.body.innerHTML = historyItems.map(item => {
    const node = item.serverSponsor ? `${item.serverSponsor} · ${item.serverLocation || ''}` : (item.serverLocation || '—');
    const latency = Number(item.latencyMs) > 0 ? `${n(item.latencyMs)} ms` : '—';
    return `<tr><td>${formatTime(item.timestamp)}</td><td class="node-col" title="${escapeHTML(node)}">${escapeHTML(node)}</td><td><b>${n(item.downloadMbps)}</b> Mbps</td><td><b>${n(item.uploadMbps)}</b> Mbps</td><td>${latency}</td><td>${escapeHTML(profileName[item.profile] || item.profile)}</td></tr>`;
  }).join('');
  els.empty.classList.toggle('hidden', historyItems.length > 0);
}

els.start.addEventListener('click', () => testStatus === 'running' ? cancelTest() : startTest());
els.refreshServers.addEventListener('click', loadServers);
els.cnServerSelect.addEventListener('change', () => chooseServer(els.cnServerSelect.value, 'cn'));
els.netServerSelect.addEventListener('change', () => chooseServer(els.netServerSelect.value, 'net'));
els.clear.addEventListener('click', async () => { if (!historyItems.length || !confirm('确定清空全部测速记录吗？')) return; await api('history',{method:'DELETE'}); await loadHistory(); });

function preventBoundaryOverscroll() {
  const scrollRoot = () => document.scrollingElement || document.documentElement;
  const boundary = () => {
    const root = scrollRoot();
    return {top: root.scrollTop <= 0, bottom: root.scrollTop + root.clientHeight >= root.scrollHeight - 1};
  };
  window.addEventListener('wheel', event => {
    const edge = boundary();
    if ((event.deltaY < 0 && edge.top) || (event.deltaY > 0 && edge.bottom)) event.preventDefault();
  }, {passive:false});
  let lastTouchY = 0;
  window.addEventListener('touchstart', event => { if (event.touches[0]) lastTouchY = event.touches[0].clientY; }, {passive:true});
  window.addEventListener('touchmove', event => {
    if (!event.touches[0]) return;
    const currentY = event.touches[0].clientY;
    const delta = currentY - lastTouchY;
    const edge = boundary();
    if ((delta > 0 && edge.top) || (delta < 0 && edge.bottom)) event.preventDefault();
    lastTouchY = currentY;
  }, {passive:false});
}

preventBoundaryOverscroll();

Promise.all([api('info'), loadHistory(), refreshState(), loadServers()]).then(([info]) => { els.version.textContent = info.version; }).catch(error => { showError(error.message); });
