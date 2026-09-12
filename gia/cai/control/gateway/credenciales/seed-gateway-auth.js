#!/usr/bin/env node
// Seed-runner: reactiva el auth del Gateway C2 con credenciales frescas de
// DeepSeek. Ahora usa el endpoint HTTP POST /api/auth (resultado síncrono);
// si el Gateway es antiguo, cae al WebSocket del runner como fallback.
// Uso: node seed-gateway-auth.js [segundos]
const fs = require("fs");
const path = require("path");

const PROFILES = path.join(__dirname, "auth-profiles.json");
const GW = process.env.GATEWAY_URL || "https://carlos-gateway.onrender.com";
const KEEP_ALIVE_S = parseInt(process.argv[2] || "5", 10);

function readCredentials() {
  // Soporta archivo completo {profiles:{deepseek:{credentials}}} o
  // un JSON plano {bearer, cookies}.
  const raw = JSON.parse(fs.readFileSync(PROFILES, "utf8"));
  if (raw.bearer) return raw;
  if (raw.profiles && raw.profiles.deepseek && raw.profiles.deepseek.credentials) {
    return raw.profiles.deepseek.credentials;
  }
  return null;
}

async function seedViaHTTP(bearer, cookies) {
  const res = await fetch(GW + "/api/auth", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ bearer, cookies: cookies || "" }),
  });
  const text = await res.text();
  if (!res.ok) throw new Error("HTTP " + res.status + ": " + text.slice(0, 200));
  return text;
}

async function seedViaWS(bearer, cookies) {
  const wsUrl = GW.replace(/^http/, "ws") + "/c2/ws";
  const ws = new WebSocket(wsUrl);
  const t = setTimeout(() => { try { ws.close(); } catch {} }, KEEP_ALIVE_S * 1000 + 1000);
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("timeout WS")), 15000);
    ws.onopen = () => {
      ws.send(JSON.stringify({ type: "register", role: "runner" }));
      setTimeout(() => ws.send(JSON.stringify({ type: "auth", bearer, cookies: cookies || "" })), 300);
    };
    ws.onerror = () => reject(new Error("WS error"));
    ws.onclose = () => { clearTimeout(timer); clearTimeout(t); resolve("WS cerrado (auth enviado)"); };
    ws.onmessage = (ev) => console.log("WS:", String(ev.data).slice(0, 120));
  });
}

async function main() {
  const creds = readCredentials();
  if (!creds || !creds.bearer || creds.bearer.length < 10) {
    console.error("No hay credenciales deepseek válidas en " + PROFILES);
    console.error("Renoválas desde chat.deepseek.com (runner/extension) y actualizá el archivo local.");
    process.exit(1);
  }
  const { bearer, cookies } = creds;
  console.log(`Seed en ${GW} (bearer_len=${bearer.length}, cookies_len=${(cookies || "").length})`);
  try {
    const r = await seedViaHTTP(bearer, cookies);
    console.log("OK vía HTTP /api/auth:", r);
    console.log("Verificá: curl " + GW + "/health  →  auth:true");
  } catch (e) {
    console.log("HTTP falló (" + e.message + "), probando WS fallback...");
    try {
      const r = await seedViaWS(bearer, cookies);
      console.log("OK vía WS:", r);
    } catch (e2) {
      console.error("Seed falló por ambos caminos:", e2.message);
      process.exit(2);
    }
  }
}

main().catch((e) => { console.error(e); process.exit(1); });