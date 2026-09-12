// DeepSeek Gateway Extension - Background Service Worker v2.1
// Minimal — content.js handles everything

const GATEWAY_URL = 'https://carlos-gateway.onrender.com';

// Heartbeat
async function heartbeat() {
  try {
    const tabs = await chrome.tabs.query({ url: '*://chat.deepseek.com/*' });
    let cookies = '';
    try {
      const cl = await chrome.cookies.getAll({ domain: 'deepseek.com' });
      cookies = cl.map(c => c.name + '=' + c.value).join('; ');
    } catch (e) {}
    await fetch(GATEWAY_URL + '/extension/heartbeat', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Extension-Cookies': cookies },
      body: JSON.stringify({ tabs: tabs.length })
    });
  } catch (e) {}
}

heartbeat();
setInterval(heartbeat, 15000);

console.log('[DS BG] v2.1 minimal — content.js handles execution');
