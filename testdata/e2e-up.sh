#!/usr/bin/env bash
# Stands up the local dependencies the end-to-end tests need: a Forgejo
# instance, a GitLab instance, and a Docker network shared by the runners.
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

# ---- GitLab ----------------------------------------------------------------
# Optional: skipped unless RF_WITH_GITLAB=1, because GitLab CE needs several
# minutes and a few gigabytes of RAM to start.
GL_ENV=""
if [ "${RF_WITH_GITLAB:-0}" = "1" ]; then
  GL_NAME=${RF_GITLAB_NAME:-rf-gitlab}
  GL_PORT=${RF_GITLAB_PORT:-8929}
  if ! docker inspect "$GL_NAME" >/dev/null 2>&1; then
    # GitLab rejects passwords it considers common, so this is generated.
    GL_PW=$(python3 -c 'import secrets,string; a=string.ascii_letters+string.digits; print("Rf"+"".join(secrets.choice(a) for _ in range(22)))')
    log "starting GitLab (this takes several minutes)"
    docker run -d --name "$GL_NAME" --network "$NET" --shm-size 256m \
      -e GITLAB_ROOT_PASSWORD="$GL_PW" \
      -e GITLAB_OMNIBUS_CONFIG="external_url 'http://${GL_NAME}'; gitlab_rails['initial_root_password']='${GL_PW}'; prometheus_monitoring['enable']=false; alertmanager['enable']=false; puma['worker_processes']=2; sidekiq['max_concurrency']=5; gitlab_kas['enable']=false; registry['enable']=false;" \
      -p "${GL_PORT}:80" gitlab/gitlab-ce:latest >/dev/null
  elif [ "$(docker inspect -f '{{.State.Running}}' "$GL_NAME")" != "true" ]; then
    docker start "$GL_NAME" >/dev/null
  fi

  log "waiting for GitLab"
  for _ in $(seq 1 150); do
    code=$(curl -sS -o /dev/null -w '%{http_code}' -m 5 "http://localhost:${GL_PORT}/users/sign_in" 2>/dev/null || echo 000)
    [ "$code" = "200" ] && break
    sleep 4
  done

  GL_TOKEN="rfglpat-$(python3 -c 'import secrets;print(secrets.token_hex(10))')"
  docker exec "$GL_NAME" gitlab-rails runner "
    u = User.find_by_username('root')
    t = u.personal_access_tokens.create!(scopes: ['api','create_runner','manage_runner'], name: 'runnerforge-e2e', expires_at: 365.days.from_now)
    t.set_token('${GL_TOKEN}'); t.save!
  " >/dev/null 2>&1

  GL_PID=$(curl -sS -X POST -H "PRIVATE-TOKEN: $GL_TOKEN" -H 'Content-Type: application/json' \
    -d '{"name":"ci-test","path":"ci-test","initialize_with_readme":true,"visibility":"private"}' \
    "http://localhost:${GL_PORT}/api/v4/projects" 2>/dev/null \
    | python3 -c 'import sys,json; print(json.load(sys.stdin).get("id",""))' 2>/dev/null)
  if [ -z "$GL_PID" ]; then
    GL_PID=$(curl -sS -H "PRIVATE-TOKEN: $GL_TOKEN" \
      "http://localhost:${GL_PORT}/api/v4/projects?search=ci-test" 2>/dev/null \
      | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d[0]["id"] if d else "")')
  fi

  GL_ENV="export RF_TEST_GITLAB_API=http://localhost:${GL_PORT}
export RF_TEST_GITLAB_INTERNAL=http://${GL_NAME}
export RF_TEST_GITLAB_TOKEN=${GL_TOKEN}
export RF_TEST_GITLAB_PROJECT=${GL_PID}"
fi

log "ready"
cat <<ENV
export RF_TEST_FORGEJO_API=http://localhost:${PORT}
export RF_TEST_FORGEJO_INTERNAL=http://${NAME}:3000
export RF_TEST_FORGEJO_TOKEN=${TOKEN}
export RF_TEST_FORGEJO_OWNER=${USER_NAME}
export RF_TEST_FORGEJO_REPO=ci-test
export RF_TEST_DOCKER_NETWORK=${NET}
${GL_ENV}
ENV
