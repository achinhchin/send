const API_URL = '';
let ws = null;
let currentSession = localStorage.getItem('send_session');
let activeDevices = {};
let myDeviceId = null;
let selectedTarget = null;
let pendingFiles = [];

const authView = document.getElementById('auth-view');
const dashboardView = document.getElementById('dashboard-view');
let authMode = 'login';
const authForm = document.getElementById('auth-form');
const authSubmit = document.getElementById('auth-submit');
const tabLogin = document.getElementById('tab-login');
const tabSignup = document.getElementById('tab-signup');
const authError = document.getElementById('auth-error');
const logoutBtn = document.getElementById('logout-btn');
const reloadBtn = document.getElementById('reload-btn');
const currentUsernameSpan = document.getElementById('current-username');
const deviceList = document.getElementById('device-list');
const dropZone = document.getElementById('drop-zone');
const fileInput = document.getElementById('file-input');
const transferStatus = document.getElementById('transfer-status');

function init() {
    if (currentSession) {
        showDashboard();
        connectWebSocket();
    } else {
        showAuth();
    }
}

function showAuth() { authView.classList.add('active'); dashboardView.classList.remove('active'); }
function showDashboard() { authView.classList.remove('active'); dashboardView.classList.add('active'); }

function timeAgo(ts) {
    const diff = Math.floor(Date.now() / 1000) - ts;
    if (diff < 60) return 'just now';
    if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
    if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
    return `${Math.floor(diff / 86400)}d ago`;
}

// Auth
tabLogin.addEventListener('click', () => {
    authMode = 'login'; tabLogin.classList.add('active'); tabSignup.classList.remove('active');
    authSubmit.textContent = 'Login'; authError.textContent = '';
});
tabSignup.addEventListener('click', () => {
    authMode = 'signup'; tabSignup.classList.add('active'); tabLogin.classList.remove('active');
    authSubmit.textContent = 'Sign Up'; authError.textContent = '';
});

authForm.addEventListener('submit', async (e) => {
    e.preventDefault();
    const username = document.getElementById('auth-username').value;
    const password = document.getElementById('auth-password').value;
    try {
        const res = await fetch(`${API_URL}/api/${authMode}`, {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ username, password })
        });
        const data = await res.json();
        if (res.ok) {
            if (authMode === 'login') {
                currentSession = data.session;
                localStorage.setItem('send_session', currentSession);
                currentUsernameSpan.textContent = username;
                showDashboard();
                connectWebSocket();
            } else {
                alert('Signup successful! Please login.');
                tabLogin.click();
            }
            authError.textContent = '';
        } else {
            authError.textContent = data.message || `${authMode} failed`;
        }
    } catch (err) { authError.textContent = 'Connection error'; }
});

logoutBtn.addEventListener('click', async () => {
    if (currentSession) {
        try { await fetch(`${API_URL}/api/logout`, { method: 'POST', headers: { 'Authorization': `Bearer ${currentSession}` } }); } catch (e) {}
    }
    localStorage.removeItem('send_session'); currentSession = null;
    if (ws) ws.close(); showAuth();
});

// WebSocket
function connectWebSocket() {
    if (ws) ws.close();
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    ws = new WebSocket(`${protocol}//${window.location.host}/ws`);
    ws.onopen = () => ws.send(JSON.stringify({ type: 'auth', session: currentSession, device_type: 'web' }));
    ws.onmessage = (e) => handleWsMsg(JSON.parse(e.data));
    ws.onclose = () => console.log('WS disconnected');
}

function handleWsMsg(data) {
    switch (data.type) {
        case 'auth_error':
            localStorage.removeItem('send_session'); currentSession = null; showAuth(); break;
        case 'state':
            activeDevices = data.devices;
            if (data.you) myDeviceId = data.you;
            renderDevices(); break;
        case 'file_offer':
            handleFileOffer(data); break;
        case 'file_chunk':
            handleFileChunk(data); break;
        case 'file_done':
            handleFileDone(data); break;
    }
}

document.addEventListener('keydown', (e) => {
    if (e.key === 'r' && document.activeElement.tagName !== 'INPUT') reloadState();
});
reloadBtn.addEventListener('click', reloadState);
function reloadState() { if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'reload' })); }

// Device Rendering
function renderDevices() {
    deviceList.innerHTML = '';
    const entries = Object.entries(activeDevices)
        .sort((a, b) => b[1].joined_at - a[1].joined_at);

    if (entries.length === 0) {
        deviceList.innerHTML = '<li class="empty-state">No devices connected.</li>';
        return;
    }

    entries.forEach(([name, info]) => {
        const li = document.createElement('li');
        const isYou = name === myDeviceId;
        const isSelected = name === selectedTarget;

        li.innerHTML = `<span>${name}${isYou ? ' <b>(You)</b>' : ''} [${info.device_type}]</span><span class="time">${timeAgo(info.joined_at)}</span>`;
        if (isSelected) li.classList.add('selected');
        if (!isYou) {
            li.style.cursor = 'pointer';
            li.addEventListener('click', () => {
                selectedTarget = name;
                renderDevices();
                updateDropZone();
            });
        }
        deviceList.appendChild(li);
    });
}

function updateDropZone() {
    const p = dropZone.querySelector('p');
    if (selectedTarget && pendingFiles.length > 0) {
        p.textContent = `Send ${pendingFiles.length} file(s) to ${selectedTarget}`;
    } else if (selectedTarget) {
        p.textContent = `Drop files to send to ${selectedTarget}`;
    } else {
        p.textContent = 'Select a device first, then drop files';
    }
}

// Drag & Drop
dropZone.addEventListener('click', () => fileInput.click());
['dragenter', 'dragover', 'dragleave', 'drop'].forEach(e => dropZone.addEventListener(e, ev => { ev.preventDefault(); ev.stopPropagation(); }, false));
['dragenter', 'dragover'].forEach(e => dropZone.addEventListener(e, () => dropZone.classList.add('dragover'), false));
['dragleave', 'drop'].forEach(e => dropZone.addEventListener(e, () => dropZone.classList.remove('dragover'), false));
dropZone.addEventListener('drop', (e) => handleFiles(e.dataTransfer.files));
fileInput.addEventListener('change', function() { handleFiles(this.files); });

function handleFiles(files) {
    if (!selectedTarget) {
        transferStatus.textContent = '⚠ Select a target device first by clicking on it.';
        return;
    }
    pendingFiles = Array.from(files);
    sendFiles();
}

// File Transfer (WebSocket relay, chunked base64)
const CHUNK_SIZE = 64 * 1024; // 64KB chunks

async function sendFiles() {
    for (const file of pendingFiles) {
        transferStatus.textContent = `Sending ${file.name} (${formatSize(file.size)})...`;

        ws.send(JSON.stringify({ type: 'file_offer', to: selectedTarget, filename: file.name, size: file.size }));

        const buf = await file.arrayBuffer();
        const bytes = new Uint8Array(buf);
        const totalChunks = Math.ceil(bytes.length / CHUNK_SIZE);

        for (let i = 0; i < totalChunks; i++) {
            const chunk = bytes.slice(i * CHUNK_SIZE, (i + 1) * CHUNK_SIZE);
            const b64 = btoa(String.fromCharCode(...chunk));
            ws.send(JSON.stringify({ type: 'file_chunk', to: selectedTarget, index: i, data: b64 }));
            transferStatus.textContent = `Sending ${file.name}: ${Math.round(((i + 1) / totalChunks) * 100)}%`;
            await new Promise(r => setTimeout(r, 10)); // yield to event loop
        }

        ws.send(JSON.stringify({ type: 'file_done', to: selectedTarget, filename: file.name, totalChunks }));
        transferStatus.textContent = `✓ Sent ${file.name}`;
    }
    pendingFiles = [];
    updateDropZone();
}

// Receiving files
let incomingFiles = {};

function handleFileOffer(data) {
    incomingFiles[data.from] = { name: data.filename, size: data.size, chunks: [] };
    transferStatus.textContent = `Receiving ${data.filename} (${formatSize(data.size)}) from ${data.from}...`;
}

function handleFileChunk(data) {
    const f = incomingFiles[data.from];
    if (!f) return;
    f.chunks[data.index] = data.data;
    const pct = Math.round((f.chunks.filter(Boolean).length / Math.ceil(f.size / CHUNK_SIZE)) * 100);
    transferStatus.textContent = `Receiving ${f.name}: ${pct}%`;
}

function handleFileDone(data) {
    const f = incomingFiles[data.from];
    if (!f) return;

    const binary = f.chunks.map(b64 => {
        const raw = atob(b64);
        const arr = new Uint8Array(raw.length);
        for (let i = 0; i < raw.length; i++) arr[i] = raw.charCodeAt(i);
        return arr;
    });

    const blob = new Blob(binary);
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url; a.download = f.name; a.click();
    URL.revokeObjectURL(url);

    transferStatus.textContent = `✓ Received ${f.name}`;
    delete incomingFiles[data.from];
}

function formatSize(bytes) {
    if (bytes < 1024) return bytes + ' B';
    if (bytes < 1048576) return (bytes / 1024).toFixed(1) + ' KB';
    return (bytes / 1048576).toFixed(1) + ' MB';
}

init();
