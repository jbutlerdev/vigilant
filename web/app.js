// Vigilant web UI — vanilla JS, no build step.
const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => document.querySelectorAll(sel);

let cameras = [];
let liveState = {};
let maskTool = null; // 'rect' | 'poly' | null

// ---------------------------------------------------------------- tabs
$$('nav .tab').forEach((btn) => {
  btn.addEventListener('click', () => {
    $$('nav .tab').forEach((b) => b.classList.toggle('active', b === btn));
    $$('.panel').forEach((p) => p.classList.add('hidden'));
    $('#tab-' + btn.dataset.tab).classList.remove('hidden');
    if (btn.dataset.tab === 'events') loadEvents();
    if (btn.dataset.tab === 'recordings') loadRecordings();
    if (btn.dataset.tab === 'masks') initMaskEditor();
    if (btn.dataset.tab === 'settings') loadSettings();
  });
});

setInterval(() => { $('#clock').textContent = new Date().toLocaleString(); }, 1000);
$('#clock').textContent = new Date().toLocaleString();

// ---------------------------------------------------------------- boot
async function boot() {
  cameras = await fetch('/api/cameras').then((r) => r.json());
  renderCameras();
  fillCameraSelects();
  connectSSE();
}

function fillCameraSelects() {
  for (const id of ['#ev-camera', '#rec-camera', '#mask-camera']) {
    const sel = $(id);
    sel.innerHTML = '<option value="">All</option>';
    if (id === '#rec-camera' || id === '#mask-camera') sel.innerHTML = '';
    for (const c of cameras) {
      const o = document.createElement('option');
      o.value = o.textContent = c.name;
      sel.appendChild(o);
    }
  }
}

// ---------------------------------------------------------------- live view
function renderCameras() {
  const root = $('#cameras');
  root.innerHTML = '';
  for (const cam of cameras) {
    const card = document.createElement('div');
    card.className = 'camera-card';
    card.innerHTML = `
      <div class="cam-head">
        <h3>${cam.name}</h3>
        <span class="badge" id="badge-${cam.name}">…</span>
      </div>
      <iframe loading="lazy" src="/go2rtc/stream.html?src=${encodeURIComponent(cam.stream)}&mode=webrtc,mse"></iframe>`;
    root.appendChild(card);
  }
}

// ---------------------------------------------------------------- SSE state
function connectSSE() {
  const es = new EventSource('/api/sse');
  es.onmessage = (e) => {
    const st = JSON.parse(e.data);
    liveState = {};
    for (const c of st.cameras || []) {
      liveState[c.name] = c;
      const badge = $('#badge-' + c.name);
      if (!badge) continue;
      const det = c.detector;
      if (!c.motion_enabled) { badge.textContent = 'no motion'; badge.className = 'badge'; continue; }
      if (c.active_event || (det && det.state === 'motion')) {
        badge.textContent = 'MOTION';
        badge.className = 'badge motion';
      } else {
        badge.textContent = det && det.running ? 'clear' : 'starting…';
        badge.className = 'badge ' + (det && det.running ? 'idle' : '');
      }
    }
  };
  es.onerror = () => { es.close(); setTimeout(connectSSE, 3000); };
}

// ---------------------------------------------------------------- events
$('#ev-refresh').addEventListener('click', loadEvents);

async function loadEvents() {
  const params = new URLSearchParams();
  const cam = $('#ev-camera').value; if (cam) params.set('camera', cam);
  if ($('#ev-from').value) params.set('from', $('#ev-from').value);
  if ($('#ev-to').value) params.set('to', $('#ev-to').value);
  const evs = await fetch('/api/events?' + params).then((r) => r.json());
  const root = $('#events');
  root.innerHTML = '';
  $('#ev-count').textContent = `${(evs || []).length} events`;
  for (const ev of evs || []) {
    const card = document.createElement('div');
    card.className = 'event-card';
    const thumb = ev.thumbnail
      ? `<img loading="lazy" src="/api/events/${ev.id}/thumbnail">`
      : `<div class="no-thumb">no thumbnail</div>`;
    card.innerHTML = `${thumb}
      <div class="ev-info">
        <strong>${ev.camera}</strong>
        <span>${fmt(ev.start)} → ${fmt(ev.end)}</span>
        <span class="muted">${ev.frames || 0} motion frames · score ${ev.max_score || 0}</span>
      </div>`;
    card.addEventListener('click', () => openEvent(ev));
    root.appendChild(card);
  }
}

function fmt(t) { return t ? new Date(t).toLocaleString() : ''; }

async function openEvent(ev) {
  const dlg = $('#event-dialog');
  $('#dlg-title').textContent = `${ev.camera} — ${fmt(ev.start)}`;
  const body = $('#dlg-body');
  body.innerHTML = '';
  const clips = ev.clips || [];
  if (clips.length) {
    const v = document.createElement('video');
    v.controls = true; v.autoplay = true;
    v.src = `/api/events/${ev.id}/clip/${encodeURIComponent(clips[0])}`;
    body.appendChild(v);
    const links = document.createElement('div');
    links.className = 'clip-links';
    clips.forEach((c, i) => {
      const a = document.createElement('a');
      a.href = `/api/events/${ev.id}/clip/${encodeURIComponent(c)}`;
      a.textContent = 'clip ' + (i + 1);
      a.addEventListener('click', (e) => { e.preventDefault(); v.src = a.href; v.play(); });
      links.appendChild(a);
    });
    body.appendChild(links);
  } else {
    body.innerHTML = '<p class="muted">No clips (still copying, or no segments available).</p>';
  }
  const del = document.createElement('button');
  del.textContent = 'Delete event'; del.className = 'danger';
  del.style.marginTop = '12px';
  del.addEventListener('click', async () => {
    if (!confirm('Delete this event?')) return;
    await fetch('/api/events/' + ev.id, { method: 'DELETE' });
    dlg.close(); loadEvents();
  });
  body.appendChild(del);
  dlg.showModal();
}
$('#dlg-close').addEventListener('click', () => $('#event-dialog').close());

// ---------------------------------------------------------------- recordings
$('#rec-refresh').addEventListener('click', loadRecordings);

async function loadRecordings() {
  const cam = $('#rec-camera').value;
  if (!cam) return;
  const params = new URLSearchParams({ camera: cam });
  if ($('#rec-date').value) {
    params.set('from', $('#rec-date').value);
    params.set('to', $('#rec-date').value);
  }
  const recs = await fetch('/api/recordings?' + params).then((r) => r.json());
  const root = $('#recordings');
  if (!recs || !recs.length) { root.innerHTML = '<p class="muted">No recordings for that day.</p>'; return; }
  let html = '<table><tr><th>Camera</th><th>Start</th><th>Duration</th><th>Size</th><th></th></tr>';
  for (const r of recs) {
    const url = `/api/recordings/file?camera=${encodeURIComponent(r.camera)}&file=${encodeURIComponent(r.file)}`;
    html += `<tr><td>${r.camera}</td><td>${fmt(r.start)}</td><td>${dur(r.start, r.end)}</td>
      <td>${(r.size / 1048576).toFixed(1)} MB</td>
      <td><a href="${url}" download>download</a> · <a href="#" data-play="${url}">play</a></td></tr>`;
  }
  html += '</table><video id="rec-player" controls style="width:100%;max-width:900px;margin-top:14px;background:#000;border-radius:8px"></video>';
  root.innerHTML = html;
  root.querySelectorAll('a[data-play]').forEach((a) => {
    a.addEventListener('click', (e) => {
      e.preventDefault();
      const v = $('#rec-player');
      v.src = a.dataset.play; v.play(); v.scrollIntoView();
    });
  });
}

function dur(a, b) {
  const s = Math.round((new Date(b) - new Date(a)) / 1000);
  return `${Math.floor(s / 60)}m ${s % 60}s`;
}

// ---------------------------------------------------------------- mask editor
let mask = { rectangles: [], polygons: [] };
let draft = null; // current shape being drawn
let img = new Image();

$('#mask-camera').addEventListener('change', initMaskEditor);
$('#mask-rect').addEventListener('click', () => setTool('rect'));
$('#mask-poly').addEventListener('click', () => setTool('poly'));
$('#mask-undo').addEventListener('click', () => {
  if (draft) { draft = null; }
  else if (mask.polygons.length) mask.polygons.pop();
  else if (mask.rectangles.length) mask.rectangles.pop();
  drawMask();
});
$('#mask-clear').addEventListener('click', () => { mask = { rectangles: [], polygons: [] }; draft = null; drawMask(); });
$('#mask-save').addEventListener('click', saveMask);

function setTool(t) {
  maskTool = maskTool === t ? null : t;
  draft = null;
  $('#mask-rect').classList.toggle('active', maskTool === 'rect');
  $('#mask-poly').classList.toggle('active', maskTool === 'poly');
  drawMask();
}

async function initMaskEditor() {
  const name = $('#mask-camera').value || (cameras[0] && cameras[0].name);
  if (!name) return;
  $('#mask-camera').value = name;
  const info = cameras.find((c) => c.name === name);
  mask = info && info.mask ? JSON.parse(JSON.stringify(info.mask)) : { rectangles: [], polygons: [] };
  if (!mask.rectangles) mask.rectangles = [];
  if (!mask.polygons) mask.polygons = [];
  draft = null;
  const cam = $('#mask-camera').value;
  img = new Image();
  img.onload = () => {
    const canvas = $('#mask-canvas');
    canvas.width = img.naturalWidth || 960;
    canvas.height = img.naturalHeight || 540;
    drawMask();
  };
  img.src = '/api/frame.jpg?camera=' + encodeURIComponent(name) + '&t=' + Date.now();
}

function drawMask() {
  const canvas = $('#mask-canvas');
  const ctx = canvas.getContext('2d');
  ctx.clearRect(0, 0, canvas.width, canvas.height);
  if (img.complete && img.naturalWidth) ctx.drawImage(img, 0, 0, canvas.width, canvas.height);
  ctx.fillStyle = 'rgba(248,81,73,0.45)';
  ctx.strokeStyle = '#f85149';
  ctx.lineWidth = 2;
  const W = canvas.width, H = canvas.height;
  for (const r of mask.rectangles) {
    ctx.fillRect(r.x * W, r.y * H, r.w * W, r.h * H);
    ctx.strokeRect(r.x * W, r.y * H, r.w * W, r.h * H);
  }
  for (const p of mask.polygons) poly(ctx, p.points, W, H);
  if (draft) {
    if (draft.type === 'rect') {
      const r = rectFrom(draft);
      ctx.fillRect(r.x * W, r.y * H, r.w * W, r.h * H);
      ctx.strokeRect(r.x * W, r.y * H, r.w * W, r.h * H);
    } else {
      poly(ctx, draft.points, W, H);
    }
  }
}

function poly(ctx, pts, W, H) {
  if (!pts.length) return;
  ctx.beginPath();
  pts.forEach(([x, y], i) => i ? ctx.lineTo(x * W, y * H) : ctx.moveTo(x * W, y * H));
  ctx.closePath();
  ctx.fill(); ctx.stroke();
  for (const [x, y] of pts) {
    ctx.beginPath(); ctx.arc(x * W, y * H, 4, 0, 7); ctx.fillStyle = '#fff'; ctx.fill();
    ctx.fillStyle = 'rgba(248,81,73,0.45)';
  }
}

function rectFrom(d) {
  const x = Math.min(d.x0, d.x1), y = Math.min(d.y0, d.y1);
  return { x, y, w: Math.abs(d.x1 - d.x0), h: Math.abs(d.y1 - d.y0) };
}

function canvasPos(e) {
  const canvas = $('#mask-canvas');
  const rect = canvas.getBoundingClientRect();
  return [
    Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width)),
    Math.min(1, Math.max(0, (e.clientY - rect.top) / rect.height)),
  ];
}

$('#mask-canvas').addEventListener('mousedown', (e) => {
  if (!maskTool) return;
  const [x, y] = canvasPos(e);
  if (maskTool === 'rect') draft = { type: 'rect', x0: x, y0: y, x1: x, y1: y };
  else {
    if (!draft) draft = { type: 'poly', points: [] };
    draft.points.push([x, y]);
  }
  drawMask();
});

$('#mask-canvas').addEventListener('mousemove', (e) => {
  if (draft && draft.type === 'rect') {
    const [x, y] = canvasPos(e);
    draft.x1 = x; draft.y1 = y;
    drawMask();
  }
});

window.addEventListener('mouseup', () => {
  if (draft && draft.type === 'rect') {
    const r = rectFrom(draft);
    if (r.w > 0.005 && r.h > 0.005) mask.rectangles.push(r);
    draft = null; drawMask();
  }
});

$('#mask-canvas').addEventListener('dblclick', () => {
  if (draft && draft.type === 'poly' && draft.points.length >= 3) {
    mask.polygons.push({ points: draft.points });
    draft = null; drawMask();
  }
});

window.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && $('#tab-masks') && !$('#tab-masks').classList.contains('hidden')) {
    if (draft && draft.type === 'poly' && draft.points.length >= 3) {
      mask.polygons.push({ points: draft.points });
      draft = null; drawMask();
    }
  }
});

async function saveMask() {
  const name = $('#mask-camera').value;
  if (!name) return;
  const res = await fetch('/api/cameras/' + encodeURIComponent(name) + '/mask', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(mask),
  });
  const ok = res.ok;
  $('#mask-status').textContent = ok ? 'Saved at ' + new Date().toLocaleTimeString() : 'Save failed: ' + await res.text();
  if (ok) {
    const info = cameras.find((c) => c.name === name);
    if (info) info.mask = mask;
  }
}

// ---------------------------------------------------------------- settings
const SEC_FIELDS = [['window', 'Trigger window (s)'], ['hold', 'Hold timer (s)'], ['buffer', 'Clip buffer (s)']];
const NUM_FIELDS = [
  ['width', 'Motion width'], ['height', 'Motion height'], ['fps', 'Motion fps'],
  ['threshold', 'Pixel threshold'], ['min_frames', 'Min frames'],
];

// sensitivityPercent derives the % display value, converting legacy
// min_pixels / min_ratio settings when min_percent is absent.
function sensitivityPercent(mc) {
  if (mc.min_percent > 0) return Math.round(mc.min_percent * 100) / 100;
  if (mc.min_ratio > 0) return Math.round(mc.min_ratio * 10000) / 100;
  if (mc.min_pixels > 0 && mc.width > 0 && mc.height > 0)
    return Math.round((mc.min_pixels / (mc.width * mc.height)) * 10000) / 100;
  return 0.3;
}

async function loadSettings() {
  const cams = await fetch('/api/cameras').then((r) => r.json());
  const st = await fetch('/api/state').then((r) => r.json());
  const root = $('#settings');
  root.innerHTML = '';

  // retention card
  const ret = document.createElement('div');
  ret.className = 'settings-card';
  ret.innerHTML = `
    <h3>Retention</h3>
    <div class="setting-row"><span>Continuous days</span>
      <input type="number" min="1" max="3650" id="ret-cont" value="${st.retention.continuous_days}"></div>
    <div class="setting-row"><span>Motion event days</span>
      <input type="number" min="1" max="3650" id="ret-mot" value="${st.retention.motion_days}"></div>
    <div class="save-row"><span class="status-msg muted" id="ret-status"></span>
      <button class="primary" id="ret-save">Save</button></div>`;
  root.appendChild(ret);
  $('#ret-save').addEventListener('click', async () => {
    const res = await fetch('/api/config/retention', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        continuous_days: +$('#ret-cont').value,
        motion_days: +$('#ret-mot').value,
      }),
    });
    $('#ret-status').textContent = res.ok ? 'Saved' : 'Failed: ' + res.status;
  });

  for (const cam of cams) {
    const mc = cam.motion_config || {};
    const card = document.createElement('div');
    card.className = 'settings-card';
    const secs = SEC_FIELDS.map(([f, label]) =>
      `<div class="setting-row"><span>${label}</span><input type="number" min="0.1" step="0.5"
        id="set-${cam.name}-${f}" value="${parseFloat(mc[f]) || 0}"></div>`).join('');
    const nums = NUM_FIELDS.map(([f, label]) =>
      `<div class="setting-row"><span>${label}</span><input type="number" min="1"
        id="set-${cam.name}-${f}" value="${mc[f] || ''}"></div>`).join('');
    card.innerHTML = `
      <h3>${cam.name} <span class="badge ${cam.motion ? 'idle' : ''}">${cam.motion ? 'motion on' : 'motion off'}</span></h3>
      <div class="setting-row"><span>Camera URL</span>
        <input type="text" id="set-${cam.name}-url" value="${cam.url || ''}"></div>
      <div class="setting-row"><span>Motion substream URL</span>
        <input type="text" id="set-${cam.name}-murl" value="${mc.url || ''}"></div>
      <div class="setting-row"><span>Recording</span>
        <input type="checkbox" id="set-${cam.name}-record" ${cam.record ? 'checked' : ''}></div>
      <div class="setting-row"><span>Motion detection</span>
        <input type="checkbox" id="set-${cam.name}-motion" ${cam.motion ? 'checked' : ''}></div>
      <div class="setting-row"><span>Sensitivity (% of frame)</span>
        <input type="number" min="0.01" max="100" step="0.05" id="set-${cam.name}-min_percent"
          value="${sensitivityPercent(mc)}"></div>
      <details><summary class="muted" style="cursor:pointer;margin:6px 0">Motion tuning</summary>
        ${nums}${secs}
      </details>
      <div class="save-row"><span class="status-msg muted" id="set-${cam.name}-status"></span>
        <button class="primary" data-save="${cam.name}">Save</button></div>`;
    root.appendChild(card);
  }

  root.querySelectorAll('button[data-save]').forEach((btn) => {
    btn.addEventListener('click', () => saveCameraSettings(btn));
  });
}

async function saveCameraSettings(btn) {
  const name = btn.dataset.save;
  const v = (f) => document.getElementById(`set-${name}-${f}`);
  const num = (f) => { const el = v(f); const n = parseFloat(el.value); return isNaN(n) ? 0 : n; };
  const body = {
    record: v('record').checked,
    url: v('url').value.trim(),
    motion: {
      enabled: v('motion').checked,
      url: v('murl').value.trim(),
      width: num('width'),
      height: num('height'),
      fps: num('fps'),
      threshold: num('threshold'),
      min_percent: num('min_percent'),
      min_frames: num('min_frames'),
      window: num('window') + 's',
      hold: num('hold') + 's',
      buffer: num('buffer') + 's',
    },
  };
  v('status').textContent = 'Saving…';
  btn.disabled = true;
  const res = await fetch('/api/cameras/' + encodeURIComponent(name) + '/settings', {
    method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
  });
  btn.disabled = false;
  if (res.ok) {
    v('status').textContent = 'Saved · pipelines restarted';
    const info = await res.json();
    const c = cameras.find((x) => x.name === name);
    if (c) { c.record = info.record; c.motion = info.motion; c.motion_config = info.motion_config; }
  } else {
    v('status').textContent = 'Failed: ' + await res.text();
  }
  setTimeout(() => { const s = v('status'); if (s) s.textContent = ''; }, 5000);
}

boot();
