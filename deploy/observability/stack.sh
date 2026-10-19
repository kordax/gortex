#!/bin/sh
set -eu

stack_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
cd "$stack_dir"

init_password() {
  if [ ! -e .state ]; then
    (umask 077; mkdir .state)
  fi
  if [ -L .state ] || [ ! -d .state ]; then
    printf '%s\n' 'ERROR: .state must be a private directory.' >&2
    exit 1
  fi
  chmod 700 .state
  if [ ! -e .state/grafana-password ]; then
    # Directory privacy protects the host file; read permission lets Grafana's
    # unprivileged container UID read the bind-mounted Compose secret.
    python3 -c 'import os,secrets; p=".state/grafana-password"; fd=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o444); os.write(fd,(secrets.token_urlsafe(32)+"\n").encode()); os.fchmod(fd,0o444); os.close(fd)'
    printf '%s\n' 'Created private Grafana password file: deploy/observability/.state/grafana-password'
  fi
  if [ -L .state/grafana-password ] || [ ! -f .state/grafana-password ]; then
    printf '%s\n' 'ERROR: Grafana password must be a regular file.' >&2
    exit 1
  fi
}

if [ "$(uname -s)" != Linux ]; then
  printf '%s\n' 'This host-network stack supports native Linux. See README.md for other runtimes.' >&2
  exit 1
fi
command -v docker >/dev/null 2>&1 || { printf '%s\n' 'Docker with Compose is required.' >&2; exit 1; }

case "${1:-help}" in
  init)
    init_password
    ;;
  up)
    init_password
    shift
    case "${1:-}" in
      --tracing) docker compose --profile tracing up -d ;;
      '') docker compose up -d ;;
      *) printf '%s\n' 'Usage: sh stack.sh up [--tracing]' >&2; exit 2 ;;
    esac
    printf '%s\n' 'Grafana: http://127.0.0.1:3000 (admin; password in .state/grafana-password)' 'Prometheus: http://127.0.0.1:9090' 'This command does not configure, start, or restart the Gortex daemon.'
    ;;
  down)
    docker compose --profile tracing down
    printf '%s\n' 'Containers stopped. Metrics, dashboards, traces and password retained.'
    ;;
  status)
    docker compose --profile tracing ps
    ;;
  logs)
    shift
    docker compose --profile tracing logs --tail=100 "$@"
    ;;
  check)
    docker compose --profile tracing config -q
    docker run --rm --network none --cap-drop ALL --security-opt no-new-privileges --read-only --entrypoint /bin/promtool -v "$stack_dir/prometheus.yaml:/etc/prometheus/prometheus.yml:ro" prom/prometheus:v3.15.0 check config /etc/prometheus/prometheus.yml
    python3 validate.py
    python3 -m unittest discover -s . -p 'test_*.py'
    ;;
  smoke)
    python3 smoke.py
    ;;
  help|-h|--help)
    printf '%s\n' 'Gortex local observability (Linux)' '  sh deploy/observability/stack.sh init' '  sh deploy/observability/stack.sh up [--tracing]' '  sh deploy/observability/stack.sh status' '  sh deploy/observability/stack.sh logs [service]' '  sh deploy/observability/stack.sh down' '  sh deploy/observability/stack.sh check' '  sh deploy/observability/stack.sh smoke'
    ;;
  *) printf '%s\n' 'Unknown command; use help.' >&2; exit 2 ;;
esac
