/# Setup Trabajadoras — Carlos Code

Guía para configurar cada PC trabajadora con el bridge y skills necesarios.

## Requisitos previos

- Node.js 18+ instalado
- Git instalado
- Acceso a la red (para conectar al Web Panel en Render)

## Paso 1: Clonar el repo

```bash
git clone https://github.com/1000carlospena-prog/cai.git
cd cai
```

## Paso 2: Instalar dependencias del bridge

```bash
cd control/worker
npm install
```

## Paso 3: Configurar el bridge

```bash
# Crear directorio de configuración
mkdir -p ~/.bridge

# Configurar el bridge
cat > ~/.bridge/config.json << 'EOF'
{
  "workerId": "pc-NOMBRE",
  "panelUrl": "https://bridgecarlos.onrender.com",
  "controlToken": "rnd_rmDmQ4gb7yGMGLnTVhYrDTWG0NgR",
  "gitRepo": "/ruta/al/cai",
  "gitPush": true
}
EOF
```

Reemplazar:
- `pc-NOMBRE` con un nombre único para esta PC (ej: `pc-linux1`, `pc-win1`)
- `/ruta/al/cai` con la ruta real del repositorio clonado

## Paso 4: Verificar conexión

```bash
# Probar conexión al panel
curl -s https://bridgecarlos.onrender.com/api/state | python3 -m json.tool

# Si pide login, usar:
# usuario: cmpf
# password: Carlos1*
```

## Paso 5: Iniciar el bridge

```bash
# En Linux (con setsid para que sobreviva):
setsid node bridge.js &

# En Windows (con pm2 o similar):
# npm install -g pm2
# pm2 start bridge.js --name bridge-pc-NOMBRE
```

## Paso 6: Verificar en el panel

1. Ir a https://bridgecarlos.onrender.com
2. Login: `cmpf` / `Carlos1*`
3. La PC debería aparecer como "conectada" en el panel

## Skills por defecto

Las trabajadoras vienen con estos skills pre-instalados:

- `sdd-apply` — Implementar tareas
- `work-unit-commits` — Commits por unidad
- `go-testing` — Tests (si el proyecto usa Go)

### Instalar skills adicionales

Para proyectos específicos, instalar skills adicionales:

```bash
# Copiar skills desde el repo principal
cp -r /ruta/al/cai/skills/skill-name ~/.config/opencode/skills/
```

## Troubleshooting

### Bridge no se conecta
- Verificar que el `controlToken` sea correcto
- Verificar que el `panelUrl` apunte al panel correcto
- Verificar conexión a internet

### Bridge se desconecta
- El proxy de Render corta conexiones después de ~20s
- El bridge usa HTTP polling (no WebSocket) para evitar esto
- Verificar que `setsid` esté funcionando en Linux

### Errores de git
- Verificar que `gitRepo` apunte al directorio correcto
- Verificar que el repo tenga permisos de escritura
- Verificar que `gitPush` esté en `true` si se quiere push automático

## Configuración avanzada

### Múltiples proyectos

Si quieres que una PC trabaje en múltiples proyectos, puedes configurar múltiples bridges:

```bash
# Bridge para proyecto 1
setsid node bridge.js --config ~/.bridge/config-proyecto1.json &

# Bridge para proyecto 2
setsid node bridge.js --config ~/.bridge/config-proyecto2.json &
```

### Modo interactivo (con tmux)

Para que el bridge use una sesión de OpenCode ya abierta:

```bash
# Configurar modo interactivo
cat > ~/.bridge/config.json << 'EOF'
{
  "workerId": "pc-NOMBRE",
  "panelUrl": "https://bridgecarlos.onrender.com",
  "controlToken": "rnd_rmDmQ4gb7yGMGLnTVhYrDTWG0NgR",
  "gitRepo": "/ruta/al/cai",
  "gitPush": true,
  "interactive": true,
  "session": "opencode"
}
EOF

# Iniciar tmux con OpenCode
tmux new -s opencode
opencode
# Ctrl+B, D para desacoplar

# Iniciar bridge
setsid node bridge.js &
```

## Seguridad

- **NUNCA** commitear el `controlToken` al repo
- Usar env vars o archivos locales para tokens sensibles
- El bridge solo se comunica con el panel en Render
- No exponer puertos locales a la red
