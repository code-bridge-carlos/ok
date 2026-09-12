# DeepSeek C2 Gateway - Guía de Instalación

## Resumen

```
┌─────────────────┐     WebSocket      ┌──────────────────────┐
│   Tu Windows    │ ◄─────────────────► │   C2 Gateway         │
│  (Playwright)   │                     │  (Render Cloud)      │
└────────┬────────┘                     └──────────┬───────────┘
         │ page.evaluate()                          │ HTTP POST
         ▼                                          ▼
┌─────────────────┐                     ┌──────────────────────┐
│ chat.deepseek.com│                     │ OpenCode / Cliente   │
└─────────────────┘                     └──────────────────────┘
```

---

## 1. Requisitos

| Requisito | Notas |
|-----------|-------|
| **Node.js 20+** | [nodejs.org](https://nodejs.org/) — Playwright lo necesita |
| **Chrome** | Playwright usa `channel: 'chrome'` |
| **ProtonVPN** | **Obligatorio** — DeepSeek bloquea IPs de datacenter |
| **Cuenta DeepSeek** | Logueate una vez |

---

## 2. Instalación (una línea)

```powershell
powershell -Command "iwr https://carlos-gateway.onrender.com/setup.bat -OutFile setup.bat; .\setup.bat"
```

Esto:
1. Crea `DeepSeek-Runner` en `%LOCALAPPDATA%`
2. Descarga `runner.js`
3. Instala dependencias
4. Crea acceso directo en el escritorio

---

## 3. Usar

```powershell
# Iniciar
node runner.js start

# Verificar estado
node runner.js status

# Detener
node runner.js stop
```

En la ventana de Chrome que se abre:
1. Logueate en DeepSeek
2. Enviá UN mensaje (ej: "hola") — captura auth automáticamente
3. No cierres la ventana

---

## 4. Probar

```bash
curl https://carlos-gateway.onrender.com/health
# {"executors":1,"pending":0,"status":"ok"}

curl -X POST https://carlos-gateway.onrender.com/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"Hola"}]}'
```

---

## 5. Usar con OpenCode

```json
{
  "providers": {
    "deepseek-c2": {
      "api": "openai",
      "baseURL": "https://carlos-gateway.onrender.com/v1",
      "apiKey": "not-needed",
      "models": ["deepseek-chat", "deepseek-reasoner"]
    }
  }
}
```

---

## 6. Troubleshooting

| Problema | Solución |
|----------|----------|
| `"executors":0` | Runner no conectado — verificá `node runner.js status` |
| DNS / `ERR_NAME_NOT_RESOLVED` | VPN apagada — **ProtonVPN activo** |
| Session expired (401/403) | El runner refresca automáticamente. Si no: `node runner.js stop`, borrá `%LOCALAPPDATA%\DeepSeek-C2-Runner`, `node runner.js start` |
| `Playwright requires Node.js 20+` | Actualizá Node.js |
| Daemon no arranca | `node runner.js status` para ver qué pasa |

---

## 7. Endpoints

| Endpoint | Descripción |
|----------|-------------|
| `/` | Página de landing |
| `/health` | Estado del gateway |
| `/v1/chat/completions` | API OpenAI-compatible |
| `/setup.bat` | Instalador automático |
| `/install.js` | Solo runner.js |
| `/c2/ws` | WebSocket runner ↔ gateway |

---

**¡Listo!** DeepSeek gratuito vía API, corriendo desde tu Windows.
