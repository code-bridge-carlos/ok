#!/usr/bin/env bash
# entrypoint — prepara el checkout del repo en PROJECT_PATH y arranca el worker.
#
# Si PROJECT_PATH no existe o está vacío, lo clona desde REPO_URL (que puede
# incluir credenciales). Workaround para Render (storage efímero: el checkout
# se re-crea en cada arranque; los cambios implementados deben committearse y
# pushearse para persistir).
set -e

: "${PROJECT_PATH:=/workspace}"
: "${REPO_URL:=}"

mkdir -p "$PROJECT_PATH"

if [ -n "$REPO_URL" ]; then
  if [ -z "$(ls -A "$PROJECT_PATH")" ]; then
    echo "[entrypoint] clonando repo en $PROJECT_PATH ..."
    git clone "$REPO_URL" "$PROJECT_PATH"
  else
    echo "[entrypoint] repo ya presente en $PROJECT_PATH (configurando remote por si cambió)"
    git -C "$PROJECT_PATH" remote set-url origin "$REPO_URL" || true
    git -C "$PROJECT_PATH" pull --ff-only origin HEAD || echo "[entrypoint] pull omitido (sin HEAD o sin red)"
  fi
else
  echo "[entrypoint] REPO_URL vacío: usando $PROJECT_PATH tal cual (debe ya contener el checkout)"
fi

# Identidad de git local (el contenedor no tiene identidad global y el commit
# de worker-code falla sin esto). Solo afecta a este checkout.
git -C "$PROJECT_PATH" config user.name "${GIT_NAME:-worker-code (C6)}"
git -C "$PROJECT_PATH" config user.email "${GIT_EMAIL:-worker-code@render.local}"

echo "[entrypoint] arrancando worker-code con PROJECT_PATH=$PROJECT_PATH"
exec worker-code
