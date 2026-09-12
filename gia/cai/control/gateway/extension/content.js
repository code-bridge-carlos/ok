// DeepSeek Gateway Extension - Content Script (MAIN world)
// Runs inside chat.deepseek.com — has access to page cookies and context
// Polls gateway for pending requests, executes them, returns results

(function() {
  'use strict';

  const GATEWAY = 'https://carlos-gateway.onrender.com';
  const POLL_MS = 2000;
  let capturedBearer = '';

  console.log('[DS Gateway v2.1] Content script loaded (MAIN world)');

  // ─── STEP 1: Intercept fetch to capture Authorization header ──────────────

  const _origFetch = window.fetch;
  window.fetch = function() {
    const url = typeof arguments[0] === 'string' ? arguments[0] : arguments[0]?.url || '';
    if (url.includes('deepseek.com') || url.includes('api.deepseek.com')) {
      const opts = arguments[1] || {};
      const hdrs = opts.headers;
      let auth = '';
      if (hdrs instanceof Headers) {
        auth = hdrs.get('Authorization') || '';
      } else if (hdrs && typeof hdrs === 'object') {
        auth = hdrs['Authorization'] || hdrs['authorization'] || '';
      }
      if (auth.startsWith('Bearer ') && auth.length > 10) {
        capturedBearer = auth.slice(7);
        console.log('[DS Gateway] Bearer captured via fetch! Length:', capturedBearer.length, 'URL:', url);
      }
    }
    return _origFetch.apply(this, arguments);
  };

  // Also intercept XMLHttpRequest for good measure
  const _origXHROpen = XMLHttpRequest.prototype.open;
  const _origXHRSend = XMLHttpRequest.prototype.send;
  const _origXHRSetHeader = XMLHttpRequest.prototype.setRequestHeader;

  XMLHttpRequest.prototype.open = function(method, url) {
    this._dsUrl = url;
    this._dsHeaders = {};
    return _origXHROpen.apply(this, arguments);
  };

  XMLHttpRequest.prototype.setRequestHeader = function(name, value) {
    if (this._dsHeaders) this._dsHeaders[name] = value;
    if (name === 'Authorization' && value.startsWith('Bearer ') && value.length > 10) {
      capturedBearer = value.slice(7);
      console.log('[DS Gateway] Bearer captured via XHR! Length:', capturedBearer.length);
    }
    return _origXHRSetHeader.apply(this, arguments);
  };

  XMLHttpRequest.prototype.send = function() {
    return _origXHRSend.apply(this, arguments);
  };

  // ─── STEP 2: Poll gateway for pending requests ────────────────────────────

  async function pollGateway() {
    try {
      const cookies = document.cookie || '';
      const resp = await fetch(GATEWAY + '/extension/pending', {
        headers: { 'X-Extension-Cookies': cookies }
      });
      const data = await resp.json();
      if (data.pending) {
        console.log('[DS Gateway] Request received:', data.id);
        await executeRequest(data);
      }
    } catch (e) {
      // Gateway might be sleeping, ignore
    }
  }

  async function executeRequest(data) {
    try {
      const messages = data.messages || [];
      let prompt = '';
      for (const msg of messages) {
        if (msg.role === 'user') prompt = msg.content;
      }
      if (!prompt && messages.length > 0) prompt = messages[messages.length - 1].content;

      const sessionId = crypto.randomUUID();
      const targetPath = '/api/v0/chat/completion';

      // ── Create PoW challenge ──
      let powHeader = '';
      try {
        const chRes = await _origFetch('/api/v0/chat/create_pow_challenge', {
          method: 'POST',
          credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ target_path: targetPath })
        });
        if (chRes.ok) {
          const chData = await chRes.json();
          const ch = chData.data?.biz_data?.challenge || chData.data?.challenge || chData.challenge;
          if (ch) {
            const answer = await solvePow(ch);
            powHeader = btoa(JSON.stringify({ ...ch, answer, target_path: targetPath }));
            console.log('[DS Gateway] PoW solved:', ch.algorithm, 'answer:', answer);
          }
        } else {
          console.warn('[DS Gateway] PoW challenge failed:', chRes.status);
        }
      } catch (e) {
        console.warn('[DS Gateway] PoW error:', e.message);
      }

      // ── Build headers ──
      const headers = {
        'Content-Type': 'application/json',
        'Accept': '*/*',
        'x-client-platform': 'web',
        'x-client-version': '1.7.0',
        'x-app-version': '20241129.1',
        'x-client-locale': 'zh_CN',
        'x-client-timezone-offset': '28800'
      };
      if (capturedBearer) {
        headers['Authorization'] = 'Bearer ' + capturedBearer;
        console.log('[DS Gateway] Using captured bearer, length:', capturedBearer.length);
      } else {
        console.warn('[DS Gateway] NO bearer token captured yet!');
      }
      if (powHeader) headers['x-ds-pow-response'] = powHeader;

      // ── Send chat completion ──
      const body = {
        chat_session_id: sessionId,
        parent_message_id: null,
        prompt: prompt,
        ref_file_ids: [],
        thinking_enabled: true,
        search_enabled: false,
        preempt: false
      };

      console.log('[DS Gateway] Sending chat request...');
      const res = await _origFetch(targetPath, {
        method: 'POST',
        credentials: 'include',
        headers: headers,
        body: JSON.stringify(body)
      });

      if (!res.ok) {
        const errText = await res.text();
        console.error('[DS Gateway] API error:', res.status, errText.substring(0, 200));
        await postResult(data.id, '', 'API ' + res.status + ': ' + errText.substring(0, 300));
        return;
      }

      // ── Parse SSE response ──
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let content = '';
      let buffer = '';

      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        const lines = buffer.split('\n');
        buffer = lines.pop(); // keep incomplete line in buffer
        for (const line of lines) {
          if (line.startsWith('data: ')) {
            const d = line.slice(6).trim();
            if (d === '[DONE]') break;
            try {
              const p = JSON.parse(d);
              content += p.choices?.[0]?.delta?.content || '';
            } catch (e) {}
          }
        }
      }

      console.log('[DS Gateway] Response received, length:', content.length);
      await postResult(data.id, content, '');

    } catch (e) {
      console.error('[DS Gateway] Execute error:', e.message);
      await postResult(data.id, '', e.message);
    }
  }

  // ─── PoW Solver ────────────────────────────────────────────────────────────

  async function solvePow(ch) {
    if (ch.algorithm === 'sha256') {
      return solvePowSHA256(ch);
    }
    if (ch.algorithm === 'DeepSeekHashV1') {
      return solvePowDeepSeekHashV1(ch);
    }
    throw new Error('Unknown algorithm: ' + ch.algorithm);
  }

  async function solvePowSHA256(ch) {
    const { challenge: target, salt, difficulty } = ch;
    const td = difficulty > 1000 ? Math.floor(Math.log2(difficulty)) : difficulty;
    for (let nonce = 0; nonce < 10000000; nonce++) {
      const inp = salt + target + nonce;
      const buf = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(inp));
      const hex = Array.from(new Uint8Array(buf)).map(b => b.toString(16).padStart(2, '0')).join('');
      let zb = 0;
      for (const c of hex) {
        if (c === '0') { zb += 4; }
        else { zb += 4 - Math.clz32(parseInt(c, 16)) + 28; break; }
      }
      if (zb >= td) return nonce;
    }
    throw new Error('SHA256 PoW timeout');
  }

  // DeepSeekHashV1 uses WebAssembly — try to use the page's WASM module
  async function solvePowDeepSeekHashV1(ch) {
    const { challenge: target, salt, difficulty, expire_at } = ch;
    const prefix = salt + '_' + (expire_at || '') + '_';

    // Try to find the WASM module in the page's global scope
    // DeepSeek loads it via a script tag, it might be available
    try {
      // First try: use the page's existing solver if available
      // DeepSeek stores it in a module, try common patterns
      const wasmUrl = 'https://fe-static.deepseek.com/chat/static/74038.ebf6d8f55d.wasm';

      // Fetch the WASM binary
      const wasmRes = await _origFetch(wasmUrl);
      if (!wasmRes.ok) throw new Error('WASM fetch failed: ' + wasmRes.status);

      const wasmBytes = await wasmRes.arrayBuffer();
      const wasmModule = await WebAssembly.compile(wasmBytes);
      const instance = await WebAssembly.instantiate(wasmModule, {});

      const exports = instance.exports;
      const memory = exports.memory;

      // Allocate memory for strings
      const alloc = exports.__wbindgen_export_0;
      const addToStack = exports.__wbindgen_add_to_stack_pointer;
      const wasmSolve = exports.wasm_solve;

      // Encode strings to WASM memory
      const encodeStr = (str) => {
        const bytes = new TextEncoder().encode(str);
        const ptr = alloc(bytes.length, 1);
        new Uint8Array(memory.buffer).set(bytes, ptr);
        return [ptr, bytes.length];
      };

      const [ptrC, lenC] = encodeStr(target);
      const [ptrP, lenP] = encodeStr(prefix);
      const retptr = addToStack(-16);

      wasmSolve(retptr, ptrC, lenC, ptrP, lenP, difficulty);

      const view = new DataView(memory.buffer);
      const status = view.getInt32(retptr, true);
      const answer = view.getFloat64(retptr + 8, true);
      addToStack(16);

      if (status === 0) throw new Error('WASM solve failed (status=0)');

      console.log('[DS Gateway] DeepSeekHashV1 solved via WASM:', answer);
      return answer;
    } catch (e) {
      console.error('[DS Gateway] DeepSeekHashV1 WASM failed:', e.message);
      throw e;
    }
  }

  // ─── Post Result ───────────────────────────────────────────────────────────

  async function postResult(id, content, error) {
    try {
      await fetch(GATEWAY + '/extension/result', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id, content, error })
      });
    } catch (e) {
      console.error('[DS Gateway] Post result failed:', e.message);
    }
  }

  // ─── Heartbeat ─────────────────────────────────────────────────────────────

  async function heartbeat() {
    try {
      const cookies = document.cookie || '';
      await fetch(GATEWAY + '/extension/heartbeat', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-Extension-Cookies': cookies,
          'X-Extension-Bearer': capturedBearer
        },
        body: JSON.stringify({ hasBearer: !!capturedBearer, cookieLen: cookies.length })
      });
    } catch (e) {}
  }

  // ─── Start ─────────────────────────────────────────────────────────────────

  // Send heartbeat immediately and every 15s
  heartbeat();
  setInterval(heartbeat, 15000);

  // Start polling
  pollGateway();
  setInterval(pollGateway, POLL_MS);

  console.log('[DS Gateway v2.1] Started — polling every', POLL_MS, 'ms');

})();
