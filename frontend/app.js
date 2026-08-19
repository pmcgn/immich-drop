// Frontend logic (mobile-safe picker; no settings UI)
const sessionId = (crypto && crypto.randomUUID) ? crypto.randomUUID() : (Math.random().toString(36).slice(2));
// Simple device fingerprint: stable per-browser using stored id + UA/screen/timezone
function getDeviceId(){
  try{
    let id = localStorage.getItem('immich_drop_device_id');
    if (!id) { id = (crypto && crypto.randomUUID) ? crypto.randomUUID() : (Math.random().toString(36).slice(2)); localStorage.setItem('immich_drop_device_id', id); }
    return id;
  }catch{ return 'anon'; }
}
function computeFingerprint(){
  try{
    const id = getDeviceId();
    const ua = navigator.userAgent || '';
    const lang = navigator.language || '';
    const plat = navigator.platform || '';
    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone || '';
    const scr = (screen && (screen.width+'x'+screen.height+'x'+screen.colorDepth)) || '';
    const raw = [id, ua, lang, plat, tz, scr].join('|');
    // tiny hash
    let h = 0; for (let i=0;i<raw.length;i++){ h = (h<<5) - h + raw.charCodeAt(i); h |= 0; }
    return `${id}:${Math.abs(h)}`;
  }catch{ return getDeviceId(); }
}
const FINGERPRINT = computeFingerprint();
let CFG = { chunked_uploads_enabled: false, chunk_size_mb: 95 };
// Detect invite token from URL path /invite/{token}
let INVITE_TOKEN = null;
try {
  const parts = (window.location.pathname || '').split('/').filter(Boolean);
  if (parts[0] === 'invite' && parts[1]) {
    INVITE_TOKEN = parts[1];
  }
} catch {}
let items = [];
let socket;

// Status precedence: never regress (e.g., uploading -> done shouldn't go back to uploading)
const STATUS_ORDER = { queued: 0, checking: 1, uploading: 2, duplicate: 3, done: 3, error: 4 };
const FINAL_STATES = new Set(['done','duplicate','error']);

// --- Dark mode ---
function initDarkMode() {
  const stored = localStorage.getItem('theme');
  if (stored === 'dark' || (!stored && window.matchMedia('(prefers-color-scheme: dark)').matches)) {
    document.documentElement.classList.add('dark');
  }
  updateThemeIcon();
}

function toggleDarkMode() {
  const isDark = document.documentElement.classList.toggle('dark');
  localStorage.setItem('theme', isDark ? 'dark' : 'light');
  updateThemeIcon();
}

function updateThemeIcon() {
  const isDark = document.documentElement.classList.contains('dark');
  const light = document.getElementById('iconLight');
  const dark = document.getElementById('iconDark');
  if (light && light.classList) light.classList.toggle('hidden', !isDark);
  if (dark && dark.classList) dark.classList.toggle('hidden', isDark);
}

initDarkMode();

// --- Load minimal config ---
(async function loadConfig(){
  try{
    const r = await fetch('/api/config');
    if (r.ok) {
      const j = await r.json();
      if (j && typeof j === 'object') {
        CFG.chunked_uploads_enabled = !!j.chunked_uploads_enabled;
        const n = parseInt(j.chunk_size_mb, 10);
        if (!Number.isNaN(n) && n > 0) CFG.chunk_size_mb = n;
      }
    }
  }catch{}
})();

// --- helpers ---
function human(bytes){
  if (!bytes) return '0 B';
  const k = 1024, sizes = ['B','KB','MB','GB','TB'];
  const i = Math.floor(Math.log(bytes)/Math.log(k));
  return (bytes/Math.pow(k,i)).toFixed(1)+' '+sizes[i];
}

function escapeHtml(v){
  return String(v ?? '')
    .replaceAll('&','&amp;')
    .replaceAll('<','&lt;')
    .replaceAll('>','&gt;')
    .replaceAll('"','&quot;')
    .replaceAll("'",'&#39;');
}

function addItem(file){
  const id = (crypto && crypto.randomUUID) ? crypto.randomUUID() : (Math.random().toString(36).slice(2));
  const it = { id, file, name: file.name, size: file.size, status: 'queued', progress: 0 };
  items.unshift(it);
  render();
}

function render(){
  const itemsEl = document.getElementById('items');
  itemsEl.innerHTML = items.map(it => `
    <div class="rounded-2xl border bg-white dark:bg-gray-800 dark:border-gray-700 p-4 shadow-sm transition-colors">
      <div class="flex items-center justify-between">
        <div class="min-w-0">
          <div class="truncate font-medium">${escapeHtml(it.name)} <span class="text-xs text-gray-500 dark:text-gray-400">(${human(it.size)})</span></div>
          <div class="mt-1 text-xs text-gray-600 dark:text-gray-400">
            ${it.message ? `<span>${escapeHtml(it.message)}</span>` : ''}
          </div>
        </div>
        <div class="flex items-center gap-2 text-sm">
          <span>${it.status}</span>
          ${it.status==='error' ? `<button class="btnRetry rounded-xl border px-3 py-1 text-xs dark:border-gray-600 dark:hover:bg-gray-700 hover:bg-gray-100 transition-colors" data-id="${it.id}" aria-label="Retry upload">Retry</button>` : ''}
        </div>
      </div>
      <div class="mt-3 h-2 w-full overflow-hidden rounded-full bg-gray-100 dark:bg-gray-700">
        <div class="h-full ${it.status==='done'?'bg-green-500':it.status==='duplicate'?'bg-amber-500':it.status==='error'?'bg-red-500':'bg-blue-500'}" style="width:${Math.max(it.progress, (it.status==='done'||it.status==='duplicate'||it.status==='error')?100:it.progress)}%"></div>
      </div>
      <div class="mt-2 text-sm text-gray-600 dark:text-gray-400">
        ${it.status==='uploading' ? `Uploading… ${it.progress}%` : it.status.charAt(0).toUpperCase()+it.status.slice(1)}
      </div>
    </div>
  `).join('');

  // Attach retry handlers for errored items
  try {
    itemsEl.querySelectorAll('.btnRetry').forEach(btn => {
      btn.addEventListener('click', (e) => {
        e.preventDefault();
        const id = btn.getAttribute('data-id');
        const it = items.find(x => x.id === id);
        if (!it) return;
        it.status = 'queued';
        it.progress = 0;
        try { delete it.message; } catch {}
        render();
        runQueue();
      });
    });
  } catch {}

  const c = {queued:0,uploading:0,done:0,dup:0,err:0};
  for(const it of items){
    if(['queued','checking'].includes(it.status)) c.queued++;
    if(it.status==='uploading') c.uploading++;
    if(it.status==='done') c.done++;
    if(it.status==='duplicate') c.dup++;
    if(it.status==='error') c.err++;
  }
  document.getElementById('countQueued').textContent=c.queued;
  document.getElementById('countUploading').textContent=c.uploading;
  document.getElementById('countDone').textContent=c.done;
  document.getElementById('countDup').textContent=c.dup;
  document.getElementById('countErr').textContent=c.err;
}

// --- WebSocket progress ---
function openSocket(){
  socket = new WebSocket((location.protocol==='https:'?'wss':'ws')+'://'+location.host+'/ws');
  socket.onopen = () => { socket.send(JSON.stringify({session_id: sessionId})); };
  socket.onmessage = (evt) => {
    const msg = JSON.parse(evt.data);
    const { item_id, status, progress, message } = msg;
    const it = items.find(x => x.id===item_id);
    if(!it) return;
    // If we've already finalized this item, ignore late/regressive updates
    if (FINAL_STATES.has(it.status)) return;

    const cur = STATUS_ORDER[it.status] ?? 0;
    const inc = STATUS_ORDER[status] ?? 0;
    if (inc < cur) {
      // ignore regressive status updates
    } else {
      it.status = status;
    }
    if (typeof progress==='number') {
      // never decrease progress
      it.progress = Math.max(it.progress || 0, progress);
    }
    if (message) it.message = message;
    if (FINAL_STATES.has(it.status)) {
      it.progress = 100;
    }
    render();
  };
  socket.onclose = () => setTimeout(openSocket, 2000);
}
openSocket();

// --- Upload queue ---
async function runQueue(){
  let inflight = 0;
  async function runNext(){
    if(inflight >= 3) return; // client-side throttle; server handles uploads regardless
    const next = items.find(i => i.status==='queued');
    if(!next) return;
    next.status='checking';
    render();
    inflight++;
    try{
      if (CFG.chunked_uploads_enabled && next.file.size > (CFG.chunk_size_mb * 1024 * 1024)) {
        await uploadChunked(next);
      } else {
        await uploadWhole(next);
      }
    }catch(err){
      next.status='error';
      next.message = String(err);
      render();
    }finally{
      inflight--;
      setTimeout(runNext, 50);
    }
  }
  for(let i=0;i<3;i++) runNext();
}

async function uploadWhole(next){
  const form = new FormData();
  form.append('file', next.file);
  form.append('item_id', next.id);
  form.append('session_id', sessionId);
  form.append('last_modified', next.file.lastModified || '');
  if (INVITE_TOKEN) form.append('invite_token', INVITE_TOKEN);
  form.append('fingerprint', FINGERPRINT);
  const res = await fetch('/api/upload', { method:'POST', body: form });
  const body = await res.json().catch(()=>({}));
  if(!res.ok && next.status!=='error'){
    next.status='error';
    next.message = body.error || 'Upload failed';
    render();
  } else if (res.ok) {
    const statusText = (body && body.status) ? String(body.status) : '';
    const isDuplicate = /duplicate/i.test(statusText);
    next.status = isDuplicate ? 'duplicate' : 'done';
    next.message = statusText || (isDuplicate ? 'Duplicate' : 'Uploaded');
    next.progress = 100;
    render();
    try { showBanner(isDuplicate ? `Duplicate: ${next.name}` : `Uploaded: ${next.name}`, isDuplicate ? 'warn' : 'ok'); } catch {}
  }
}

// --- Streaming SHA-1 (RFC 3174) ---
// WebCrypto's crypto.subtle.digest cannot hash incrementally (it needs the
// whole file in memory), and chunked uploads exist precisely so large files
// never do — so the whole-file hash is computed with this small hasher over
// file slices. The server recomputes the SHA-1 of the assembled file and
// rejects the upload with `checksum_mismatch` if it differs.
class StreamingSHA1 {
  constructor(){
    this.h0=0x67452301; this.h1=0xEFCDAB89; this.h2=0x98BADCFE; this.h3=0x10325476; this.h4=0xC3D2E1F0;
    this.tail=new Uint8Array(64); this.tailLen=0; this.bytes=0; this.w=new Int32Array(80);
  }
  update(u8){
    this.bytes += u8.length;
    let off = 0;
    if (this.tailLen > 0){
      const n = Math.min(64 - this.tailLen, u8.length);
      this.tail.set(u8.subarray(0, n), this.tailLen);
      this.tailLen += n; off = n;
      if (this.tailLen === 64){ this._block(this.tail, 0); this.tailLen = 0; }
    }
    while (off + 64 <= u8.length){ this._block(u8, off); off += 64; }
    if (off < u8.length){ this.tail.set(u8.subarray(off)); this.tailLen = u8.length - off; }
  }
  _block(b, o){
    const w = this.w;
    for (let i=0;i<16;i++,o+=4){ w[i]=(b[o]<<24)|(b[o+1]<<16)|(b[o+2]<<8)|b[o+3]; }
    for (let i=16;i<80;i++){ const x=w[i-3]^w[i-8]^w[i-14]^w[i-16]; w[i]=(x<<1)|(x>>>31); }
    let a=this.h0,bb=this.h1,c=this.h2,d=this.h3,e=this.h4;
    for (let i=0;i<80;i++){
      let f,k;
      if(i<20){ f=(bb&c)|((~bb)&d); k=0x5A827999; }
      else if(i<40){ f=bb^c^d; k=0x6ED9EBA1; }
      else if(i<60){ f=(bb&c)|(bb&d)|(c&d); k=0x8F1BBCDC; }
      else { f=bb^c^d; k=0xCA62C1D6; }
      const t=(((a<<5)|(a>>>27))+f+e+k+w[i])|0;
      e=d; d=c; c=(bb<<30)|(bb>>>2); bb=a; a=t;
    }
    this.h0=(this.h0+a)|0; this.h1=(this.h1+bb)|0; this.h2=(this.h2+c)|0; this.h3=(this.h3+d)|0; this.h4=(this.h4+e)|0;
  }
  hex(){
    // 64-bit big-endian bit length; JS bit-ops are 32-bit, so split via math
    const hi = Math.floor(this.bytes / 0x20000000);      // bits >> 32
    const lo = (this.bytes % 0x20000000) * 8;            // bits & 0xffffffff
    const pad = new Uint8Array((this.tailLen < 56 ? 64 : 128) - this.tailLen);
    pad[0] = 0x80;
    const dv = new DataView(pad.buffer);
    dv.setUint32(pad.length - 8, hi);
    dv.setUint32(pad.length - 4, lo >>> 0);
    this.update(pad);
    return [this.h0,this.h1,this.h2,this.h3,this.h4].map(x => (x>>>0).toString(16).padStart(8,'0')).join('');
  }
}

// sha1File hashes a File/Blob in 8 MB slices without holding it in memory.
async function sha1File(file, onProgress){
  const hasher = new StreamingSHA1();
  const step = 8 * 1024 * 1024;
  for (let off = 0; off < file.size; off += step){
    const buf = await file.slice(off, Math.min(file.size, off + step)).arrayBuffer();
    hasher.update(new Uint8Array(buf));
    if (onProgress) onProgress(Math.min(off + step, file.size), file.size);
  }
  return hasher.hex();
}

async function uploadChunked(next){
  const chunkBytes = Math.max(1, CFG.chunk_size_mb|0) * 1024 * 1024;
  const total = Math.ceil(next.file.size / chunkBytes) || 1;
  // Hash the whole file up front so the server can verify the assembled
  // result before forwarding to Immich. Best-effort: on any failure the
  // upload proceeds without verification (empty sha1 = skip).
  let sha1 = '';
  try {
    next.message = 'Computing checksum…';
    render();
    sha1 = await sha1File(next.file, (done, totalBytes) => {
      next.message = `Computing checksum… ${Math.floor((done/totalBytes)*100)}%`;
      render();
    });
    next.message = '';
    render();
  } catch { sha1 = ''; }
  // init
  try {
    await fetch('/api/upload/chunk/init', { method:'POST', headers:{'Content-Type':'application/json','Accept':'application/json'}, body: JSON.stringify({
      item_id: next.id,
      session_id: sessionId,
      name: next.file.name,
      size: next.file.size,
      last_modified: next.file.lastModified || '',
      invite_token: INVITE_TOKEN || '',
      content_type: next.file.type || 'application/octet-stream',
      fingerprint: FINGERPRINT,
      sha1: sha1
    }) });
  } catch {}
  // upload parts
  let uploaded = 0;
  for (let i=0;i<total;i++){
    const start = i * chunkBytes;
    const end = Math.min(next.file.size, start + chunkBytes);
    const blob = next.file.slice(start, end);
    const fd = new FormData();
    fd.append('item_id', next.id);
    fd.append('session_id', sessionId);
    fd.append('chunk_index', String(i));
    fd.append('total_chunks', String(total));
    if (INVITE_TOKEN) fd.append('invite_token', INVITE_TOKEN);
    fd.append('fingerprint', FINGERPRINT);
    fd.append('chunk', blob, `${next.file.name}.part${i}`);
    const r = await fetch('/api/upload/chunk', { method:'POST', body: fd });
    if (!r.ok) {
      const j = await r.json().catch(()=>({}));
      throw new Error(j.error || `Chunk ${i} failed`);
    }
    uploaded++;
    // Approximate progress until final server-side upload takes over
    next.status = 'uploading';
    next.progress = Math.min(90, Math.floor((uploaded/total) * 60) + 20); // stay under 100 until WS finish
    render();
  }
  // complete (sha1 repeated here in case the init request was lost)
  const rc = await fetch('/api/upload/chunk/complete', { method:'POST', headers:{'Content-Type':'application/json','Accept':'application/json'}, body: JSON.stringify({
    item_id: next.id,
    session_id: sessionId,
    name: next.file.name,
    last_modified: next.file.lastModified || '',
    invite_token: INVITE_TOKEN || '',
    content_type: next.file.type || 'application/octet-stream',
    fingerprint: FINGERPRINT,
    total_chunks: total,
    sha1: sha1
  }) });
  const body = await rc.json().catch(()=>({}));
  if (!rc.ok && next.status!=='error'){
    next.status='error';
    next.message = body.error || 'Upload failed';
    render();
  } else if (rc.ok) {
    const statusText = (body && body.status) ? String(body.status) : '';
    const isDuplicate = /duplicate/i.test(statusText);
    next.status = isDuplicate ? 'duplicate' : 'done';
    next.message = statusText || (isDuplicate ? 'Duplicate' : 'Uploaded');
    next.progress = 100;
    render();
  }
}

// --- DOM refs ---
const dz = document.getElementById('dropzone');
const fi = document.getElementById('fileInput');
const btnMobilePick = document.getElementById('btnMobilePick');
const btnClearFinished = document.getElementById('btnClearFinished');
const btnClearAll = document.getElementById('btnClearAll');
const btnPing = document.getElementById('btnPing');
const pingStatus = document.getElementById('pingStatus');
const banner = document.getElementById('topBanner');
const btnTheme = document.getElementById('btnTheme');
const dropHint = document.getElementById('dropHint');

// --- Simple banner helper ---
function showBanner(text, kind='ok'){
  if(!banner) return;
  banner.textContent = text;
  // reset classes and apply based on kind
  banner.className = 'rounded-2xl p-3 text-center transition-colors ' + (
    kind==='ok' ? 'border border-green-200 bg-green-50 text-green-700 dark:bg-green-900 dark:border-green-700 dark:text-green-300'
    : kind==='warn' ? 'border border-amber-200 bg-amber-50 text-amber-700 dark:bg-amber-900 dark:border-amber-700 dark:text-amber-300'
    : 'border border-red-200 bg-red-50 text-red-700 dark:bg-red-900 dark:border-red-700 dark:text-red-300'
  );
  banner.classList.remove('hidden');
  setTimeout(() => banner.classList.add('hidden'), 3000);
}

// --- Connection test with ephemeral banner ---
if (btnPing) btnPing.onclick = async () => {
  pingStatus.textContent = 'checking…';
  try{
    const r = await fetch('/api/ping', { method:'POST' });
    const j = await r.json();
    pingStatus.textContent = j.ok ? 'Connected' : 'No connection';
    pingStatus.className = 'ml-2 text-sm ' + (j.ok ? 'text-green-600' : 'text-red-600');
    if(j.ok){
      let bannerText = `Connected to Immich at ${j.base_url}`;
      if(j.album_name) {
        bannerText += ` | Uploading to album: "${j.album_name}"`;
      }
      showBanner(bannerText, 'ok');
    }
  }catch{
    pingStatus.textContent = 'No connection';
    pingStatus.className='ml-2 text-sm text-red-600';
  }
};

// If on invite page, fetch invite info and show context banner
(async function initInviteBanner(){
  if (!INVITE_TOKEN) return;
  try {
    const r = await fetch(`/api/invite/${INVITE_TOKEN}`);
    if (!r.ok) return;
    const j = await r.json();
    const parts = [];
    if (j.albumName) parts.push(`Uploading to album: "${j.albumName}"`);
    if (j.expiresAt) parts.push(`Expires: ${new Date(j.expiresAt).toLocaleString()}`);
    if (typeof j.remaining === 'number') parts.push(`Uses left: ${j.remaining}`);
    if (parts.length) showBanner(parts.join(' | '), 'ok');
  } catch {}
})();

// --- Drag & drop (no click-to-open on touch) ---
['dragenter','dragover'].forEach(ev => dz.addEventListener(ev, e=>{ e.preventDefault(); dz.classList.add('border-blue-500','bg-blue-50','dark:bg-blue-900','dark:bg-opacity-20'); }));
['dragleave','drop'].forEach(ev => dz.addEventListener(ev, e=>{ e.preventDefault(); dz.classList.remove('border-blue-500','bg-blue-50','dark:bg-blue-900','dark:bg-opacity-20'); }));
dz.addEventListener('drop', (e)=>{
  e.preventDefault();
  const files = Array.from(e.dataTransfer.files || []);
  const accepted = files.filter(f => /^(image|video)\//.test(f.type) || /\.(jpe?g|png|heic|heif|webp|gif|tiff|bmp|mp4|mov|m4v|avi|mkv)$/i.test(f.name));
  accepted.forEach(addItem);
  render();
  runQueue();
});

// --- Mobile-safe file input change handler ---
const isTouch = ('ontouchstart' in window) || (navigator.maxTouchPoints > 0);

// On iOS Safari, the `capture` attribute forces camera-only and hides Photo Library.
// Keep camera default on Android, but remove capture elsewhere to allow picking from Photos/Files.
try {
  const ua = (navigator.userAgent || navigator.vendor || window.opera || '');
  const isAndroid = /Android/i.test(ua);
  if (fi) {
    if (isAndroid) {
      fi.setAttribute('capture', 'environment');
    } else {
      fi.removeAttribute('capture');
    }
  }
} catch {}
let suppressClicksUntil = 0;
if (isTouch && dropHint) {
  try { dropHint.classList.add('hidden'); } catch {}
}

fi.addEventListener('click', (e) => {
  // prevent bubbling to parents (extra safety)
  e.stopPropagation();
});

fi.onchange = () => {
  // Suppress any stray clicks for a short window after the picker closes
  suppressClicksUntil = Date.now() + 800;

  const files = Array.from(fi.files || []);
  const accepted = files.filter(f =>
    /^(image|video)\//.test(f.type) ||
    /\.(jpe?g|png|heic|heif|webp|gif|tiff|bmp|mp4|mov|m4v|avi|mkv)$/i.test(f.name)
  );
  accepted.forEach(addItem);
  render();
  runQueue();

  // Reset a bit later so selecting the same items again still triggers 'change'
  setTimeout(() => { try { fi.value = ''; } catch {} }, 500);
};

// If you want the whole dropzone clickable on desktop only, enable this:
if (!isTouch) {
  dz.addEventListener('click', () => {
    // avoid accidental double-open if something weird happens
    if (Date.now() < suppressClicksUntil) return;
    try { fi.value = ''; } catch {}
    fi.click();
  });
}

// Mobile sticky CTA: trigger system file picker
if (btnMobilePick) {
  btnMobilePick.onclick = () => {
    try { fi.value = ''; } catch {}
    fi.click();
  };
}

// --- Clear buttons ---
btnClearFinished.onclick = ()=>{
  items = items.filter(i => !['done','duplicate'].includes(i.status));
  render();
  // also tell server to refresh album cache so a renamed album triggers a new one
  fetch('/api/album/reset', { method: 'POST' }).catch(()=>{});
};
btnClearAll.onclick = ()=>{
  items = [];
  render();
  // also reset album cache server-side
  fetch('/api/album/reset', { method: 'POST' }).catch(()=>{});
};

// --- Dark mode toggle ---
if (btnTheme) btnTheme.onclick = toggleDarkMode;
