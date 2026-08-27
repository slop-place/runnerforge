#!/usr/bin/env bash
# Stands up the local dependencies the end-to-end tests need: a Forgejo instance
# and a Docker network shared by the controller's runners.
#
# Everything here is real. The only thing the tests substitute is running
# runners as containers rather than cloud VMs.
#
# Usage:  eval "$(testdata/e2e-up.sh)"
set -euo pipefail

NET=${RF_TEST_DOCKER_NETWORK:-rf-net}
NAME=${RF_FORGEJO_NAME:-rf-forgejo}
PORT=${RF_FORGEJO_PORT:-3000}
IMAGE=${RF_FORGEJO_IMAGE:-codeberg.org/forgejo/forgejo:15}
USER_NAME=rfadmin
USER_PASS=rfadmin-pass-123

log() { echo "# $*" >&2; }

docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null

if ! docker inspect "$NAME" >/dev/null 2>&1; then
  log "starting Forgejo ($IMAGE)"
  docker run -d --name "$NAME" --network "$NET" \
    -e FORGEJO__security__INSTALL_LOCK=true \
    -e FORGEJO__actions__ENABLED=true \
    -e "FORGEJO__server__ROOT_URL=http://${NAME}:3000/" \
    -e FORGEJO__database__DB_TYPE=sqlite3 \
    -e FORGEJO__log__LEVEL=warn \
    -p "${PORT}:3000" "$IMAGE" >/dev/null
elif [ "$(docker inspect -f '{{.State.Running}}' "$NAME")" != "true" ]; then
  docker start "$NAME" >/dev/null
fi

log "waiting for Forgejo to answer"
for _ in $(seq 1 60); do
  curl -sf -m 5 "http://localhost:${PORT}/api/v1/version" >/dev/null 2>&1 && break
  sleep 2
done

docker exec -u git "$NAME" forgejo admin user create --admin \
  --username "$USER_NAME" --password "$USER_PASS" --email admin@example.com \
  --must-change-password=false >/dev/null 2>&1 || true

TOKEN=$(curl -sS -X POST -u "${USER_NAME}:${USER_PASS}" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"runnerforge-e2e-$(date +%s)\",\"scopes\":[\"all\"]}" \
  "http://localhost:${PORT}/api/v1/users/${USER_NAME}/tokens" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["sha1"])')

curl -sS -X POST -H "Authorization: token $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"ci-test","auto_init":true,"default_branch":"main"}' \
  "http://localhost:${PORT}/api/v1/user/repos" >/dev/null 2>&1 || true

log "ready"
cat <<ENV
export RF_TEST_FORGEJO_API=http://localhost:${PORT}
export RF_TEST_FORGEJO_INTERNAL=http://${NAME}:3000
export RF_TEST_FORGEJO_TOKEN=${TOKEN}
export RF_TEST_FORGEJO_OWNER=${USER_NAME}
export RF_TEST_FORGEJO_REPO=ci-test
export RF_TEST_DOCKER_NETWORK=${NET}
ENV
