#!/usr/bin/env bash
# ═══════════════════════════════════════════════════════════════════════════════
# start.sh — EMBFinder unified service launcher
#
# Usage:
#   ./start.sh              Start all three services (dev mode)
#   ./start.sh --prod       Start in production mode (headless, no Wails)
#   ./start.sh --check      Health-check all services only (no start)
#   ./start.sh --setup      Install all dependencies only (no start)
#   ./start.sh --stop       Kill all running EMBFinder services
#
# Services started:
#   ① emb-engine    → http://localhost:8767  (EMB preview renderer)
#   ② embedder      → http://localhost:8766  (AI CLIP embedder)
#   ③ go-server     → http://localhost:8765  (main backend + UI)
# ═══════════════════════════════════════════════════════════════════════════════
set -euo pipefail

# ── Colours ─────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GRN='\033[0;32m'; YLW='\033[1;33m'
BLU='\033[0;34m'; CYN='\033[0;36m'; BOLD='\033[1m'; RST='\033[0m'

info()    { echo -e "${BLU}[info]${RST}  $*"; }
ok()      { echo -e "${GRN}[ok]${RST}    $*"; }
warn()    { echo -e "${YLW}[warn]${RST}  $*"; }
err()     { echo -e "${RED}[error]${RST} $*" >&2; }
header()  { echo -e "\n${BOLD}${CYN}▶ $*${RST}"; }

# ── Script location (works when called from any directory) ───────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$SCRIPT_DIR"

EMB_ENGINE_DIR="$ROOT/emb-engine"
EMBEDDER_DIR="$ROOT/embedder"
GO_SERVER_DIR="$ROOT/go-server"

# ── Load .env if present ─────────────────────────────────────────────────────
if [[ -f "$ROOT/.env" ]]; then
  set -a; source "$ROOT/.env"; set +a
fi

PORT="${PORT:-8765}"
EMBEDDER_PORT="${EMBEDDER_PORT:-8766}"
EMB_ENGINE_PORT="${EMB_ENGINE_PORT:-8767}"
HOST="${HOST:-127.0.0.1}"
MODE="${MODE:-development}"

# ── Parse arguments ──────────────────────────────────────────────────────────
ARG="${1:-}"
case "$ARG" in
  --prod)    MODE="production" ;;
  --check)   _CHECK_ONLY=1 ;;
  --setup)   _SETUP_ONLY=1 ;;
  --stop)    _STOP_ONLY=1 ;;
  --help|-h)
    sed -n '3,12p' "$0" | sed 's/^# \?//'
    exit 0 ;;
  "")        : ;;   # default: start all
  *)         err "Unknown argument: $ARG"; exit 1 ;;
esac

# ═══════════════════════════════════════════════════════════════════════════════
# STOP
# ═══════════════════════════════════════════════════════════════════════════════
stop_all() {
  header "Stopping EMBFinder services"
  # Kill by port
  for port in "$PORT" "$EMBEDDER_PORT" "$EMB_ENGINE_PORT"; do
    local pid
    pid=$(lsof -ti tcp:"$port" 2>/dev/null || true)
    if [[ -n "$pid" ]]; then
      kill "$pid" 2>/dev/null && ok "Stopped PID $pid (port $port)" || warn "Could not stop $pid"
    fi
  done
  # Also kill any go-server or uvicorn we own
  pkill -f "embfind\|embfinder" 2>/dev/null || true
  pkill -f "uvicorn main:app"   2>/dev/null || true
  pkill -f "python.*server\.py" 2>/dev/null || true
  ok "All services stopped"
}

if [[ "${_STOP_ONLY:-0}" == "1" ]]; then stop_all; exit 0; fi

# ═══════════════════════════════════════════════════════════════════════════════
# PREREQUISITES CHECK
# ═══════════════════════════════════════════════════════════════════════════════
check_prereqs() {
  header "Checking prerequisites"
  local ok_all=1

  # Python 3.9+
  if command -v python3 &>/dev/null; then
    local pyver
    pyver=$(python3 -c "import sys; print(f'{sys.version_info.major}.{sys.version_info.minor}')")
    local pymaj pymin
    pymaj=$(echo "$pyver" | cut -d. -f1)
    pymin=$(echo "$pyver" | cut -d. -f2)
    if [[ $pymaj -ge 3 && $pymin -ge 9 ]]; then
      ok "Python $pyver"
    else
      err "Python 3.9+ required (found $pyver)"
      ok_all=0
    fi
  else
    err "python3 not found — install Python 3.10+"
    ok_all=0
  fi

  # Go 1.22+
  if command -v go &>/dev/null; then
    local gover
    gover=$(go version | grep -oP 'go\K[0-9]+\.[0-9]+')
    ok "Go $gover"
  else
    err "go not found — install from https://go.dev/dl/"
    ok_all=0
  fi

  # pip / venv
  python3 -m pip  --version &>/dev/null && ok "pip available" || warn "pip not found — package installs may fail"
  python3 -m venv --help    &>/dev/null && ok "venv module available" || { err "python3-venv not found — run: sudo apt install python3-venv"; ok_all=0; }

  [[ $ok_all -eq 1 ]] && ok "All prerequisites met" || { err "Fix the above issues then re-run"; return 1; }
}

# ═══════════════════════════════════════════════════════════════════════════════
# VENV SETUP
# ═══════════════════════════════════════════════════════════════════════════════
setup_venv() {
  local dir="$1"
  local name="$2"
  local venv="$dir/.venv"

  # Detect if existing venv is broken (binary missing)
  if [[ -d "$venv" ]]; then
    if [[ ! -x "$venv/bin/python3" ]]; then
      warn "$name: .venv exists but python3 binary is missing — recreating"
      rm -rf "$venv"
    fi
  fi

  if [[ ! -d "$venv" ]]; then
    info "$name: creating virtual environment…"
    python3 -m venv "$venv"
    ok "$name: venv created at $venv"
  else
    ok "$name: venv OK ($venv/bin/python3)"
  fi
}

install_deps() {
  local dir="$1"
  local name="$2"
  local venv="$dir/.venv"
  local req="$dir/requirements.txt"

  if [[ ! -f "$req" ]]; then
    warn "$name: requirements.txt not found — skipping"
    return
  fi

  info "$name: installing dependencies…"
  "$venv/bin/pip" install --quiet --upgrade pip
  "$venv/bin/pip" install --quiet -r "$req"
  ok "$name: dependencies installed"
}

setup_service() {
  local dir="$1"
  local name="$2"
  setup_venv "$dir" "$name"
  install_deps "$dir" "$name"
}

setup_all() {
  header "Setting up virtual environments"
  setup_service "$EMB_ENGINE_DIR" "emb-engine"
  setup_service "$EMBEDDER_DIR"   "embedder"

  header "Checking Go modules"
  (cd "$GO_SERVER_DIR" && go mod download 2>/dev/null && ok "go-server: modules OK")
}

if [[ "${_SETUP_ONLY:-0}" == "1" ]]; then
  check_prereqs
  setup_all
  ok "Setup complete — run ./start.sh to launch all services"
  exit 0
fi

# ═══════════════════════════════════════════════════════════════════════════════
# HEALTH CHECK
# ═══════════════════════════════════════════════════════════════════════════════
wait_for() {
  local url="$1" name="$2" timeout="${3:-60}"
  local elapsed=0
  printf "${BLU}[wait]${RST}  $name ($url) "
  while ! curl -sf "$url" &>/dev/null; do
    if [[ $elapsed -ge $timeout ]]; then
      echo -e " ${RED}TIMEOUT${RST}"
      return 1
    fi
    printf "."
    sleep 2
    elapsed=$((elapsed + 2))
  done
  echo -e " ${GRN}ready${RST} (${elapsed}s)"
  return 0
}

check_health() {
  header "Health checks"
  local failed=0

  check_one() {
    local url="$1" name="$2"
    if curl -sf "$url" &>/dev/null; then
      ok "$name → $url"
    else
      err "$name → OFFLINE ($url)"
      failed=$((failed + 1))
    fi
  }

  check_one "http://$HOST:$EMB_ENGINE_PORT/health" "emb-engine (8767)"
  check_one "http://$HOST:$EMBEDDER_PORT/health"   "embedder   (8766)"
  check_one "http://$HOST:$PORT/"                  "go-server  (8765)"

  if [[ $failed -eq 0 ]]; then
    echo ""
    ok "All services healthy"
    echo -e "  ${BOLD}UI:${RST} http://$HOST:$PORT"
  else
    err "$failed service(s) not responding"
    return 1
  fi
}

if [[ "${_CHECK_ONLY:-0}" == "1" ]]; then check_health; exit $?; fi

# ═══════════════════════════════════════════════════════════════════════════════
# MAIN STARTUP
# ═══════════════════════════════════════════════════════════════════════════════
echo -e "${BOLD}${CYN}"
cat << 'EOF'
╔═══════════════════════════════════════════════════════╗
║          EMBFinder — Embroidery Visual Search         ║
╚═══════════════════════════════════════════════════════╝
EOF
echo -e "${RST}"
info "Mode: ${BOLD}$MODE${RST}"
info "Services: emb-engine:$EMB_ENGINE_PORT  embedder:$EMBEDDER_PORT  go-server:$PORT"
echo ""

# ── Step 1: prerequisites ────────────────────────────────────────────────────
check_prereqs

# ── Step 2: auto-setup if venvs are missing or broken ───────────────────────
setup_all

# ── Step 3: launch services (background) ─────────────────────────────────────
LOG_DIR="$ROOT/.logs"
mkdir -p "$LOG_DIR"

header "Starting emb-engine (port $EMB_ENGINE_PORT)"
# Kill any stale instance first
lsof -ti tcp:"$EMB_ENGINE_PORT" 2>/dev/null | xargs kill 2>/dev/null || true
sleep 0.5
(
  cd "$EMB_ENGINE_DIR"
  export EMB_ENGINE_PORT MODE
  exec .venv/bin/python3 server.py \
    >> "$LOG_DIR/emb-engine.log" 2>&1
) &
EMB_PID=$!
info "emb-engine PID=$EMB_PID  log: .logs/emb-engine.log"

header "Starting embedder (port $EMBEDDER_PORT)"
lsof -ti tcp:"$EMBEDDER_PORT" 2>/dev/null | xargs kill 2>/dev/null || true
sleep 0.5
(
  cd "$EMBEDDER_DIR"
  export CLIP_MODEL="${CLIP_MODEL:-ViT-L-14}"
  exec .venv/bin/python3 -m uvicorn main:app \
    --host "$HOST" \
    --port "$EMBEDDER_PORT" \
    --workers 1 \
    --log-level warning \
    >> "$LOG_DIR/embedder.log" 2>&1
) &
EMB_AI_PID=$!
info "embedder PID=$EMB_AI_PID  log: .logs/embedder.log"

header "Starting go-server (port $PORT)"
lsof -ti tcp:"$PORT" 2>/dev/null | xargs kill 2>/dev/null || true
sleep 0.5

# Build fresh binary (dev: with race detector; prod: optimised)
if [[ "$MODE" == "production" ]]; then
  info "Building go-server (production, -tags headless)…"
  (cd "$GO_SERVER_DIR" && \
    CGO_ENABLED=0 go build -tags headless \
      -ldflags="-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo dev)" \
      -o "$ROOT/.logs/embfinder-server" . 2>> "$LOG_DIR/go-build.log") && \
    ok "go-server built" || { err "go-server build failed — check .logs/go-build.log"; exit 1; }

  MODE="$MODE" HEADLESS=1 \
    "$ROOT/.logs/embfinder-server" \
    >> "$LOG_DIR/go-server.log" 2>&1 &
  GO_PID=$!
else
  info "Starting go-server (development, Wails window)…"
  (
    cd "$GO_SERVER_DIR"
    export MODE HEADLESS="${HEADLESS:-0}"
    exec go run --tags dev . \
      >> "$LOG_DIR/go-server.log" 2>&1
  ) &
  GO_PID=$!
fi
info "go-server PID=$GO_PID  log: .logs/go-server.log"

# ── Step 4: wait for all services to be ready ────────────────────────────────
header "Waiting for services to become ready"
echo ""

FAIL=0
wait_for "http://$HOST:$EMB_ENGINE_PORT/health" "emb-engine" 90 || FAIL=1
wait_for "http://$HOST:$EMBEDDER_PORT/health"   "embedder"   300 || FAIL=1  # model download can take time
wait_for "http://$HOST:$PORT/"                  "go-server"  60  || FAIL=1

echo ""
if [[ $FAIL -eq 1 ]]; then
  err "One or more services failed to start."
  echo ""
  echo -e "  Check logs in ${BOLD}.logs/${RST}:"
  echo "    tail -50 .logs/emb-engine.log"
  echo "    tail -50 .logs/embedder.log"
  echo "    tail -50 .logs/go-server.log"
  exit 1
fi

# ── Step 5: summary ──────────────────────────────────────────────────────────
echo -e "${GRN}${BOLD}"
cat << 'EOF'
  ╔═════════════════════════════════════════════════╗
  ║             All services are running!           ║
  ╚═════════════════════════════════════════════════╝
EOF
echo -e "${RST}"
echo -e "  ${BOLD}Web UI:${RST}      http://$HOST:$PORT"
echo -e "  ${BOLD}Embedder:${RST}    http://$HOST:$EMBEDDER_PORT/health"
echo -e "  ${BOLD}EMB Engine:${RST}  http://$HOST:$EMB_ENGINE_PORT/health"
echo ""
echo -e "  ${YLW}Logs:${RST}  .logs/emb-engine.log  .logs/embedder.log  .logs/go-server.log"
echo -e "  ${YLW}Stop:${RST}  ./start.sh --stop   OR   Ctrl+C"
echo ""

# ── Keep script alive — forward Ctrl+C to all children ───────────────────────
cleanup() {
  echo ""
  header "Shutting down…"
  kill "$EMB_PID" "$EMB_AI_PID" "$GO_PID" 2>/dev/null || true
  ok "All services stopped"
  exit 0
}
trap cleanup SIGINT SIGTERM

# Tail all logs to stdout so the user sees live output
echo -e "${BOLD}── Live logs (Ctrl+C to stop all) ────────────────────────${RST}"
tail -f "$LOG_DIR/emb-engine.log" "$LOG_DIR/embedder.log" "$LOG_DIR/go-server.log" &
TAIL_PID=$!

# Wait for any child to die unexpectedly
wait "$EMB_PID" "$EMB_AI_PID" "$GO_PID" 2>/dev/null || true
kill "$TAIL_PID" 2>/dev/null || true
