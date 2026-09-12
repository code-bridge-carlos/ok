"""
Integración de Carlos Code con el Web Panel.

Permite que el panel envíe prompts a Carlos Code (orquestador) en vez de
que el leader PC planifique. Se activa con CARLOS_CODE_URL en env vars.
"""

import os
import httpx
import logging

logger = logging.getLogger("carlos-code-integration")

# URL de Carlos Code (si está configurado)
CARLOS_CODE_URL = os.getenv("CARLOS_CODE_URL", "")
CARLOS_CODE_TIMEOUT = int(os.getenv("CARLOS_CODE_TIMEOUT", "30"))


async def carlos_code_available() -> bool:
    """Verifica si Carlos Code está disponible."""
    if not CARLOS_CODE_URL:
        return False
    try:
        async with httpx.AsyncClient(timeout=5.0) as client:
            resp = await client.get(f"{CARLOS_CODE_URL}/health")
            return resp.status_code == 200
    except Exception:
        return False


async def carlos_code_plan(
    prompt: str,
    workers: list[str],
    existing_files: list[str] | None = None,
    project_path: str | None = None,
    section_id: str | None = None,
) -> dict | None:
    """
    Solicita un plan a Carlos Code.

    Returns:
        dict con mode, note, assignments si éxito
        None si Carlos Code no está disponible o falla
    """
    if not CARLOS_CODE_URL:
        logger.info("CARLOS_CODE_URL no configurado — saltando Carlos Code")
        return None

    try:
        async with httpx.AsyncClient(timeout=CARLOS_CODE_TIMEOUT) as client:
            resp = await client.post(
                f"{CARLOS_CODE_URL}/plan",
                json={
                    "prompt": prompt,
                    "workers": workers,
                    "existing_files": existing_files or [],
                    "project_path": project_path,
                    "section_id": section_id or "",
                },
            )

            if resp.status_code == 200:
                plan = resp.json()
                logger.info(f"Carlos Code devolvió plan: {plan.get('note', 'sin nota')}")
                return plan
            else:
                logger.warning(f"Carlos Code error {resp.status_code}: {resp.text[:200]}")
                return None

    except httpx.TimeoutException:
        logger.warning("Carlos Code timeout")
        return None
    except httpx.ConnectError:
        logger.warning("Carlos Code no disponible (connect error)")
        return None
    except Exception as e:
        logger.error(f"Error inesperado con Carlos Code: {e}")
        return None


async def carlos_code_closeout(
    task_id: str,
    prompt: str,
    outputs: list[str] | None = None,
    deploy_service_id: str | None = None,
) -> dict | None:
    """
    Notifica a Carlos Code que una tarea distribuida terminó para que
    consolide: snapshot Engram+EMGRAN, commit del closeout y deploy opcional.

    Best-effort: devuelve el dict de C3 o None si no está disponible.
    """
    if not CARLOS_CODE_URL:
        return None
    try:
        async with httpx.AsyncClient(timeout=CARLOS_CODE_TIMEOUT) as client:
            resp = await client.post(
                f"{CARLOS_CODE_URL}/closeout",
                json={
                    "task_id": task_id,
                    "prompt": prompt,
                    "outputs": outputs or [],
                    "deploy_service_id": deploy_service_id or "",
                },
            )
            if resp.status_code == 200:
                return resp.json()
            logger.warning(f"Carlos Code closeout {resp.status_code}: {resp.text[:200]}")
            return None
    except Exception as e:
        logger.warning(f"Carlos Code closeout falló: {e}")
        return None


def parse_plan_assignments(plan: dict, worker_ids: list[str]) -> dict:
    """
    Convierte el plan de Carlos Code a formato de assignments del panel.

    Returns:
        dict: {worker_id: [file1, file2, ...], ...}
    """
    assignments = plan.get("assignments", {})

    # Validar que todos los workers en el plan estén en la lista de workers
    valid_assignments = {}
    for wid, files in assignments.items():
        if wid in worker_ids:
            valid_assignments[wid] = files
        else:
            # Si el worker no existe, redistribuir a la primera worker disponible
            if worker_ids:
                fallback = worker_ids[0]
                if fallback not in valid_assignments:
                    valid_assignments[fallback] = []
                valid_assignments[fallback].extend(files)

    # Asegurar que todos los workers tengan al menos algo (si hay archivos)
    if not valid_assignments and worker_ids:
        # Plan vacío: asignar todo a la primera worker
        valid_assignments[worker_ids[0]] = ["tarea_principal"]

    return valid_assignments
