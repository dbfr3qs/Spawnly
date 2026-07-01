#!/usr/bin/env bash
# Clone the sibling Spawnly repos next to this one so the multi-root workspace
# (spawnly.code-workspace) resolves all three folders. Idempotent: safe to run
# on every container start. Uses HTTPS via gh (SSH to github.com fails DNS in
# this devcontainer), and skips gracefully if gh is not authenticated yet.
set -euo pipefail

WORKSPACES="${WORKSPACES:-/workspaces}"

# The /workspaces root is root-owned by default, so vscode cannot create sibling
# clones or the workspace file there. Make just the directory writable (not
# recursive — the repos mounted inside keep their own ownership).
if [ ! -w "${WORKSPACES}" ]; then
  sudo chown "$(id -un):$(id -gn)" "${WORKSPACES}" || true
fi

# gh clones over whatever git_protocol is configured; force HTTPS because SSH
# to github.com does not resolve in this container.
gh config set -h github.com git_protocol https >/dev/null 2>&1 || true

clone_if_missing() {
  local repo="$1" dir="$2"
  if [ -d "$dir/.git" ]; then
    echo "✓ ${dir} already present"
    return 0
  fi
  echo "Cloning ${repo} → ${dir} ..."
  if gh repo clone "$repo" "$dir" >/dev/null 2>&1; then
    echo "✓ cloned ${repo}"
  else
    echo "⚠ could not clone ${repo} — re-auth with 'gh auth refresh -h github.com' then re-run .devcontainer/clone-siblings.sh"
  fi
}

# spawnly-docs is public; spawnly-infra is private (needs valid gh auth).
clone_if_missing dbfr3qs/spawnly-docs  "${WORKSPACES}/spawnly-docs"
clone_if_missing dbfr3qs/spawnly-infra "${WORKSPACES}/spawnly-infra"

# (Re)generate the multi-root workspace file at the /workspaces root so it
# survives a container rebuild without living inside any single repo's git tree.
WS_FILE="${WORKSPACES}/spawnly.code-workspace"
if [ ! -f "${WS_FILE}" ]; then
  cat > "${WS_FILE}" <<'JSON'
{
  "folders": [
    { "name": "Spawnly (code)", "path": "agent-platform" },
    { "name": "spawnly-docs", "path": "spawnly-docs" },
    { "name": "spawnly-infra", "path": "spawnly-infra" }
  ],
  "settings": {}
}
JSON
  echo "✓ wrote ${WS_FILE}"
fi
