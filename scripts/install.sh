#!/usr/bin/env bash
set -euo pipefail

APP_NAME="primecel-gestor"
APP_DIR="/opt/primecel-gestor"
DATA_DIR="/etc/primecel-gestor"
BACKUP_DIR="$DATA_DIR/backups"
BIN_PATH="$APP_DIR/primecel-gestor"
CONFIG_FILE="$DATA_DIR/config.env"
DB_FILE="$DATA_DIR/gestor.db"
SERVERS_FILE="$DATA_DIR/servers.conf"
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOTMENU_BIN="/usr/local/bin/botmenu"
TOKEN_FILE="${TOKEN_FILE:-/etc/primecel-gestor/tokens.env}"
CHECKUSER_PACKAGE_DIR="${SRC_DIR}/checkuser_installer"
CHECKUSER_INSTALLER_STORE="/opt/primecel/checkuser-installer"
LEGACY_TOKEN_FILE="/etc/.sysd-cache-7f3a91b2.conf"
DEFAULT_AGENT_PORT="8787"
DEFAULT_CHECKUSER_PORT="2052"
DEFAULT_WEBHOOK_PORT="8099"
GO_REQUIRED_VERSION="${GO_REQUIRED_VERSION:-1.23.0}"
GO_MIN_MAJOR="1"
GO_MIN_MINOR="23"
NODE_MAJOR_REQUIRED="${NODE_MAJOR_REQUIRED:-18}"
NODE_LTS_LINE="${NODE_LTS_LINE:-20}"
INSTALL_LOG="/tmp/primecel-gestor-install.log"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
WHITE='\033[1;37m'
GRAY='\033[0;90m'
NC='\033[0m'

export DEBIAN_FRONTEND=noninteractive
export NEEDRESTART_MODE=a
export APT_LISTCHANGES_FRONTEND=none

need_root() {
  if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
    echo -e "${RED}Execute como root:${NC} sudo bash install.sh"
    exit 1
  fi
}

pause() { echo; read -r -p "Pressione ENTER para continuar..." _ || true; }
line() { echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"; }
box_header() {
  clear 2>/dev/null || true
  echo -e "${CYAN}╔════════════════════════════════════════════════╗${NC}"
  echo -e "${WHITE}           ⚡ PRIMECEL - BOT GESTOR          ${NC}"
  echo -e "${CYAN}╚════════════════════════════════════════════════╝${NC}"
}
ok() { echo -e "${GREEN}✅ $*${NC}"; }
warn() { echo -e "${YELLOW}⚠️ $*${NC}"; }
fail() { echo -e "${RED}❌ $*${NC}"; exit 1; }
info() { echo -e "${CYAN}$*${NC}"; }

ask() {
  local p="$1" d="${2:-}" v
  if [[ -n "$d" ]]; then
    read -r -p "$p [$d]: " v || true
    echo "${v:-$d}"
  else
    read -r -p "$p: " v || true
    echo "$v"
  fi
}


repeat_char() {
  local char="$1" count="$2" out="" i
  for ((i=0; i<count; i++)); do out+="$char"; done
  printf '%s' "$out"
}

install_progress_line() {
  local step="$1" total="$2" msg="$3"
  local width=20 percent filled empty bar spaces
  if [[ "$total" -le 0 ]]; then total=1; fi
  percent=$(( step * 100 / total ))
  if [[ "$percent" -gt 100 ]]; then percent=100; fi
  filled=$(( percent * width / 100 ))
  empty=$(( width - filled ))
  bar="$(repeat_char '█' "$filled")"
  spaces="$(repeat_char '░' "$empty")"
  printf "\n\033[K${CYAN}%s%s${NC} ${GREEN}%3d%%${NC} - ${WHITE}%s${NC}" "$bar" "$spaces" "$percent" "$msg"
}

install_progress_done() {
  printf "\r\033[K${CYAN}████████████████████${NC} ${GREEN}100%%${NC} - ${WHITE}Concluído${NC}\n"
}

run_quiet() {
  mkdir -p "$(dirname "$INSTALL_LOG")" 2>/dev/null || true
  "$@" >>"$INSTALL_LOG" 2>&1
}

cmd_exists() { command -v "$1" >/dev/null 2>&1; }

node_bin_path() {
  command -v node 2>/dev/null || echo /usr/bin/node
}

os_id() {
  . /etc/os-release 2>/dev/null || true
  echo "${ID:-unknown}"
}

os_version_id() {
  . /etc/os-release 2>/dev/null || true
  echo "${VERSION_ID:-unknown}"
}

is_supported_ubuntu_version() {
  local id ver
  id="$(os_id)"
  ver="$(os_version_id)"
  [[ "$id" != "ubuntu" ]] && return 1
  case "$ver" in
    20.04|22.04|24.04|26|26.*) return 0 ;;
    *) return 1 ;;
  esac
}

check_linux_compatibility() {
  local id ver arch
  id="$(os_id)"
  ver="$(os_version_id)"
  arch="$(detect_arch)"
  case "$arch" in
    amd64|arm64) : ;;
    *) fail "Arquitetura não suportada automaticamente: $(uname -m). Suportado: amd64/x86_64 e arm64/aarch64." ;;
  esac
  if [[ "$id" == "ubuntu" ]]; then
    case "$ver" in
      20.04|22.04|24.04|26|26.*) return 0 ;;
      *) warn "Ubuntu $ver não está na lista principal testada. O instalador continuará por compatibilidade, mas a base alvo é 20.04/22.04/24.04/26.x." ;;
    esac
  elif [[ "$id" == "debian" ]]; then
    warn "Debian detectado. O instalador continua, mas o alvo principal deste pacote é Ubuntu 20.04/22.04/24.04/26.x."
  else
    warn "Sistema $id $ver detectado. O alvo principal é Ubuntu 20.04/22.04/24.04/26.x."
  fi
}

prepare_apt_for_ubuntu() {
  [[ "$(os_id)" == "ubuntu" ]] || return 0
  # Alguns Ubuntu minimal não têm universe habilitado; sshpass e dependências auxiliares podem estar lá.
  if cmd_exists add-apt-repository; then
    run_quiet add-apt-repository -y universe || true
    apt_updated_once=0
  else
    apt_update_once || true
    run_quiet apt-get install -y --no-install-recommends software-properties-common || true
    if cmd_exists add-apt-repository; then
      run_quiet add-apt-repository -y universe || true
      apt_updated_once=0
    fi
  fi
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    armv7l|armv6l) echo "armv6l" ;;
    *) echo "unknown" ;;
  esac
}


pkg_installed_deb() {
  dpkg -s "$1" >/dev/null 2>&1
}

apt_updated_once=0
apt_update_once() {
  if [[ "$apt_updated_once" -eq 0 ]]; then
    run_quiet apt-get update -y
    apt_updated_once=1
  fi
}

install_apt_packages_if_missing() {
  local missing=() pkg
  for pkg in "$@"; do
    if ! pkg_installed_deb "$pkg"; then
      missing+=("$pkg")
    fi
  done
  if [[ "${#missing[@]}" -eq 0 ]]; then
    return 0
  fi
  apt_update_once
  if ! run_quiet apt-get install -y --no-install-recommends "${missing[@]}"; then
    # Tenta reparar índice/repositório em Ubuntu minimal e repetir uma vez.
    prepare_apt_for_ubuntu || true
    apt_update_once
    run_quiet apt-get install -y --no-install-recommends "${missing[@]}"
  fi
}

node_major_version() {
  if ! cmd_exists node; then echo 0; return 0; fi
  node -v 2>/dev/null | sed -E 's/^v([0-9]+).*/\1/' | grep -E '^[0-9]+$' || echo 0
}

node_official_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "x64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) echo "" ;;
  esac
}

install_official_node() {
  local arch base latest_dir list tar_name url tmp extract_dir
  arch="$(node_official_arch)"
  [[ -n "$arch" ]] || fail "Arquitetura não suportada para Node oficial: $(uname -m)"
  base="https://nodejs.org/dist/latest-v${NODE_LTS_LINE}.x"
  list="/tmp/primecel-node-shasums.txt"
  rm -f "$list"
  if cmd_exists curl; then
    run_quiet curl -fL "$base/SHASUMS256.txt" -o "$list"
  elif cmd_exists wget; then
    run_quiet wget -O "$list" "$base/SHASUMS256.txt"
  else
    fail "curl/wget não encontrado para baixar Node.js."
  fi
  tar_name="$(awk -v a="linux-${arch}.tar.xz" '$2 ~ a"$" {print $2; exit}' "$list")"
  [[ -n "$tar_name" ]] || fail "Não foi possível localizar Node.js linux-${arch} na linha v${NODE_LTS_LINE}.x."
  url="$base/$tar_name"
  tmp="/tmp/$tar_name"
  rm -f "$tmp"
  if cmd_exists curl; then
    run_quiet curl -fL "$url" -o "$tmp"
  else
    run_quiet wget -O "$tmp" "$url"
  fi
  [[ -s "$tmp" ]] || fail "Falha ao baixar Node.js em $url"
  rm -rf /usr/local/node-primecel
  mkdir -p /usr/local/node-primecel
  run_quiet tar -C /usr/local/node-primecel --strip-components=1 -xJf "$tmp"
  ln -sf /usr/local/node-primecel/bin/node /usr/local/bin/node
  ln -sf /usr/local/node-primecel/bin/npm /usr/local/bin/npm
  ln -sf /usr/local/node-primecel/bin/npx /usr/local/bin/npx
}

ensure_nodejs_dependency() {
  local major
  major="$(node_major_version)"
  if [[ "$major" -ge "$NODE_MAJOR_REQUIRED" ]] && cmd_exists npm; then
    return 0
  fi

  if cmd_exists apt-get; then
    install_apt_packages_if_missing ca-certificates curl wget gnupg xz-utils
    # Primeiro tenta o repositório padrão. Ubuntu 22/24/26 normalmente já entrega Node adequado.
    apt_update_once
    run_quiet apt-get install -y --no-install-recommends nodejs npm || true
    major="$(node_major_version)"
    if [[ "$major" -lt "$NODE_MAJOR_REQUIRED" ]] || ! cmd_exists npm; then
      # Depois tenta NodeSource. Se não suportar a release/codename, cai para binário oficial.
      if curl -fsSL "https://deb.nodesource.com/setup_${NODE_LTS_LINE}.x" -o /tmp/primecel_nodesource_setup.sh; then
        run_quiet bash /tmp/primecel_nodesource_setup.sh || true
        run_quiet apt-get install -y nodejs || true
      fi
    fi
    major="$(node_major_version)"
    if [[ "$major" -lt "$NODE_MAJOR_REQUIRED" ]] || ! cmd_exists npm; then
      install_official_node
    fi
  elif cmd_exists dnf; then
    run_quiet dnf install -y nodejs npm || install_official_node
  elif cmd_exists yum; then
    run_quiet yum install -y nodejs npm || install_official_node
  elif cmd_exists apk; then
    run_quiet apk add --no-cache nodejs npm || install_official_node
  else
    install_official_node
  fi

  major="$(node_major_version)"
  if [[ "$major" -lt "$NODE_MAJOR_REQUIRED" ]] || ! cmd_exists npm; then
    fail "Node.js ${NODE_MAJOR_REQUIRED}+ não foi instalado corretamente. Versão atual: $(node -v 2>/dev/null || echo ausente)"
  fi
}

ensure_base_dependencies() {
  if cmd_exists apt-get; then
    prepare_apt_for_ubuntu || true
    install_apt_packages_if_missing \
      ca-certificates curl wget tar gzip unzip zip xz-utils openssl jq \
      rsync sqlite3 libsqlite3-dev build-essential gcc g++ make pkg-config \
      openssh-client sshpass procps iproute2 net-tools \
      lsof coreutils findutils sed grep gawk bash systemd
  elif cmd_exists dnf; then
    run_quiet dnf install -y ca-certificates curl wget tar gzip unzip zip openssl jq rsync sqlite sqlite-devel gcc make pkgconfig openssh-clients sshpass procps-ng iproute net-tools lsof coreutils findutils sed grep gawk bash
  elif cmd_exists yum; then
    run_quiet yum install -y ca-certificates curl wget tar gzip unzip zip openssl jq rsync sqlite sqlite-devel gcc make pkgconfig openssh-clients sshpass procps-ng iproute net-tools lsof coreutils findutils sed grep gawk bash
  elif cmd_exists apk; then
    run_quiet apk add --no-cache ca-certificates curl wget tar gzip unzip zip openssl jq rsync sqlite sqlite-dev gcc make pkgconfig openssh-client sshpass procps iproute2 net-tools lsof coreutils findutils sed grep gawk bash
  else
    fail "Gerenciador de pacotes não suportado. Use Debian/Ubuntu, Alma/Rocky/CentOS ou Alpine."
  fi
  update-ca-certificates >/dev/null 2>&1 || true
}


go_version_major_minor() {
  if ! cmd_exists go; then echo "0 0"; return 0; fi
  local v major minor
  v="$(go version 2>/dev/null | awk '{print $3}' | sed -E 's/^go([0-9]+)\.([0-9]+).*/\1 \2/')"
  if [[ "$v" =~ ^[0-9]+[[:space:]][0-9]+$ ]]; then
    echo "$v"
  else
    echo "0 0"
  fi
}

go_satisfies_required() {
  local major minor
  read -r major minor < <(go_version_major_minor)
  if [[ "$major" -gt "$GO_MIN_MAJOR" ]]; then return 0; fi
  if [[ "$major" -eq "$GO_MIN_MAJOR" && "$minor" -ge "$GO_MIN_MINOR" ]]; then return 0; fi
  return 1
}

install_official_go() {
  local arch url tmp
  case "$(uname -m)" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    armv6l|armv7l) arch="armv6l" ;;
    *) fail "Arquitetura não suportada para instalar Go automaticamente: $(uname -m)" ;;
  esac
  url="https://go.dev/dl/go${GO_REQUIRED_VERSION}.linux-${arch}.tar.gz"
  tmp="/tmp/go${GO_REQUIRED_VERSION}.linux-${arch}.tar.gz"
  rm -f "$tmp"
  if cmd_exists curl; then
    run_quiet curl -fL "$url" -o "$tmp"
  elif cmd_exists wget; then
    run_quiet wget -O "$tmp" "$url"
  else
    fail "curl/wget não encontrado para baixar Go."
  fi
  [[ -s "$tmp" ]] || fail "Falha ao baixar Go em $url"
  rm -rf /usr/local/go
  run_quiet tar -C /usr/local -xzf "$tmp"
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
  ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
}

ensure_go_dependency() {
  if go_satisfies_required; then
    return 0
  fi

  # Tenta primeiro o pacote da distro. Se a versão for antiga, usa o Go oficial.
  if cmd_exists apt-get; then
    install_apt_packages_if_missing golang-go || true
  elif cmd_exists dnf; then
    run_quiet dnf install -y golang || true
  elif cmd_exists yum; then
    run_quiet yum install -y golang || true
  elif cmd_exists apk; then
    run_quiet apk add --no-cache go || true
  fi

  if go_satisfies_required; then
    return 0
  fi

  install_official_go
  if ! go_satisfies_required; then
    fail "Go ${GO_MIN_MAJOR}.${GO_MIN_MINOR}+ não foi instalado corretamente. Versão atual: $(go version 2>/dev/null || echo ausente)"
  fi
}

ensure_whatsapp_dependencies() {
  ensure_nodejs_dependency
  if [[ -d "$SRC_DIR/whatsapp" && -f "$SRC_DIR/whatsapp/package.json" ]]; then
    if [[ ! -d "$SRC_DIR/whatsapp/node_modules" ]]; then
      (cd "$SRC_DIR/whatsapp" && NPM_CONFIG_FUND=false NPM_CONFIG_AUDIT=false run_quiet npm install --omit=dev --silent --no-audit --no-fund)
    else
      :
    fi
  fi
}

ensure_all_dependencies() {
  need_root
  : >"$INSTALL_LOG" 2>/dev/null || true
  {
    echo "== Compatibilidade PrimeCel Gestor =="
    echo "Data: $(date -Is)"
    echo "OS: $(. /etc/os-release 2>/dev/null; echo ${PRETTY_NAME:-unknown})"
    echo "Kernel: $(uname -a)"
    echo "Arch: $(uname -m) -> $(detect_arch)"
  } >>"$INSTALL_LOG" 2>&1 || true
  check_linux_compatibility
  ensure_base_dependencies
  ensure_go_dependency
  ensure_whatsapp_dependencies
}

ask_required() {
  local prompt="$1" value=""
  while true; do
    read -r -p "$prompt" value || true
    value="${value:-}"
    if [[ -n "${value// /}" ]]; then
      printf '%s' "$value"
      return 0
    fi
    echo "Campo obrigatório."
  done
}


install_action_label() {
  local update_mode="${1:-0}"
  if [[ "$update_mode" == "1" ]]; then
    echo "ATUALIZANDO"
  else
    echo "INSTALANDO"
  fi
}

box_header_install_action() {
  local action="$1"
  clear 2>/dev/null || true
  echo -e "${CYAN}╔════════════════════════════════════════════════╗${NC}"
  printf "${WHITE}%s${NC}\n" "           ⚡ PRIMECEL - ${action}          "
  echo -e "${CYAN}╚════════════════════════════════════════════════╝${NC}"
}

install_step_screen() {
  local update_mode="$1" step="$2" total="$3" msg="$4"
  local action
  action="$(install_action_label "$update_mode")"
  box_header_install_action "$action"
  install_progress_line "$step" "$total" "$msg"
  echo
}

install_error_screen() {
  local update_mode="$1" where="$2" err="$3"
  local action
  action="$(install_action_label "$update_mode")"
  box_header_install_action "$action"
  line
  echo -e "${RED}❌ Erro em: ${where}${NC}"
  echo -e "${WHITE}${err}${NC}"
  line
  echo -e "  ${CYAN}00.${NC} Voltar ao Menu"
  line
  while true; do
    read -r -p "Opção: " op || return 0
    case "$op" in
      0|00|voltar|Voltar|VOLTAR) return 0 ;;
      *) warn "Opção inválida." ;;
    esac
  done
}

validate_admin_ids_value() {
  local ids="$1"
  [[ "$ids" =~ ^[0-9]+([,][0-9]+)*$ ]]
}

validate_telegram_bot_token() {
  local token="$1" response ok desc
  [[ -n "$token" ]] || return 1
  response="$(curl -fsS --max-time 12 "https://api.telegram.org/bot${token}/getMe" 2>/tmp/primecel-telegram-token.err || true)"
  ok="$(printf '%s' "$response" | jq -r '.ok // false' 2>/dev/null || echo false)"
  if [[ "$ok" == "true" ]]; then
    return 0
  fi
  desc="$(printf '%s' "$response" | jq -r '.description // empty' 2>/dev/null || true)"
  [[ -n "$desc" ]] && echo "$desc" || cat /tmp/primecel-telegram-token.err 2>/dev/null || true
  return 1
}

cf_api_request_install() {
  local token="$1" method="$2" path="$3" data="${4:-}"
  if [[ -n "$data" ]]; then
    curl -fsS --max-time 25 -X "$method" "https://api.cloudflare.com/client/v4${path}" \
      -H "Authorization: Bearer ${token}" \
      -H "Content-Type: application/json" \
      --data "$data"
  else
    curl -fsS --max-time 25 -X "$method" "https://api.cloudflare.com/client/v4${path}" \
      -H "Authorization: Bearer ${token}" \
      -H "Content-Type: application/json"
  fi
}

validate_cloudflare_token_full() {
  local token="$1" verify zones zone_id zone_name tmp_name body create_json rec_id del_json
  [[ -n "$token" ]] || { echo "Token vazio."; return 1; }
  verify="$(cf_api_request_install "$token" GET "/user/tokens/verify" 2>/tmp/primecel-cf-verify.err || true)"
  if ! printf '%s' "$verify" | jq -e '.success == true and (.result.status == "active" or .result.status == "ok")' >/dev/null 2>&1; then
    echo "Token inválido ou inativo. Permissões necessárias: Zone:Read e DNS:Edit. Para CheckUser/Origin Rules: Rulesets:Edit."
    printf '%s\n' "$verify" >>"$INSTALL_LOG" 2>/dev/null || true
    return 1
  fi
  zones="$(cf_api_request_install "$token" GET "/zones?per_page=1" 2>/tmp/primecel-cf-zones.err || true)"
  if ! printf '%s' "$zones" | jq -e '.success == true and (.result | length) >= 1' >/dev/null 2>&1; then
    echo "Token válido, mas sem permissão Zone:Read ou sem domínio disponível. Permissões necessárias: Zone:Read e DNS:Edit."
    printf '%s\n' "$zones" >>"$INSTALL_LOG" 2>/dev/null || true
    return 1
  fi
  zone_id="$(printf '%s' "$zones" | jq -r '.result[0].id')"
  zone_name="$(printf '%s' "$zones" | jq -r '.result[0].name')"
  tmp_name="_primecel_token_test_${RANDOM}.$zone_name"
  body="$(jq -n --arg name "$tmp_name" '{type:"TXT", name:$name, content:"primecel-token-test", ttl:60, proxied:false}')"
  create_json="$(cf_api_request_install "$token" POST "/zones/${zone_id}/dns_records" "$body" 2>/tmp/primecel-cf-dns.err || true)"
  if ! printf '%s' "$create_json" | jq -e '.success == true' >/dev/null 2>&1; then
    echo "Token válido, mas sem permissão DNS:Edit. Permissões necessárias: Zone:Read e DNS:Edit."
    printf '%s\n' "$create_json" >>"$INSTALL_LOG" 2>/dev/null || true
    return 1
  fi
  rec_id="$(printf '%s' "$create_json" | jq -r '.result.id // empty')"
  if [[ -n "$rec_id" ]]; then
    del_json="$(cf_api_request_install "$token" DELETE "/zones/${zone_id}/dns_records/${rec_id}" 2>/dev/null || true)"
    printf '%s\n' "$del_json" >>"$INSTALL_LOG" 2>/dev/null || true
  fi
  return 0
}

ask_bot_token_validated() {
  local token err
  while true; do
    read -r -p "> Informe o Token do Bot: " token || true
    token="$(printf '%s' "$token" | tr -d '\r\n' | sed 's/^ *//;s/ *$//')"
    if [[ -z "$token" ]]; then
      warn "Token obrigatório."
      continue
    fi
    err="$(validate_telegram_bot_token "$token" 2>&1)" && { printf '%s' "$token"; return 0; }
    warn "Token do Bot inválido. ${err}"
  done
}

ask_admin_ids_validated() {
  local ids
  while true; do
    read -r -p "> Informe o ID Telegram do Admin: " ids || true
    ids="$(printf '%s' "$ids" | tr -d '\r\n ' | sed 's/，/,/g')"
    if validate_admin_ids_value "$ids"; then
      printf '%s' "$ids"
      return 0
    fi
    warn "ID inválido. Use somente números. Para mais de um admin, separe por vírgula."
  done
}

ask_cloudflare_token_validated() {
  local token err
  while true; do
    echo "> Token Cloudflare"
    echo "  Permissões necessárias: Zone:Read e DNS:Edit."
    echo "  Para CheckUser/Origin Rules: Rulesets:Edit."
    read -r -p "> Informe o Token Cloudflare: " token || true
    token="$(printf '%s' "$token" | tr -d '\r\n' | sed 's/^ *//;s/ *$//')"
    if [[ -z "$token" ]]; then
      warn "Token Cloudflare obrigatório."
      continue
    fi
    err="$(validate_cloudflare_token_full "$token" 2>&1)" && { printf '%s' "$token"; return 0; }
    warn "$err"
  done
}

ask_yes_no_strict() {
  local prompt="$1" ans norm
  while true; do
    read -r -p "$prompt [s/n]: " ans || true
    norm="$(printf '%s' "$ans" | tr '[:upper:]' '[:lower:]' | sed 's/^ *//;s/ *$//')"
    case "$norm" in
      s|sim) echo -e "\033[1;32mSIM${NC}" >&2; printf 's'; return 0 ;;
      n|nao|não) echo -e "\033[1;31mNÃO${NC}" >&2; printf 'n'; return 0 ;;
      *) warn "Responda apenas s/sim ou n/não." ;;
    esac
  done
}

progress_bar() {
  local msg="$1"
  printf "${CYAN}████████████████████${NC} ${GREEN}100%%${NC} - ${WHITE}%s${NC}\n" "$msg"
}

ensure_dirs() {
  mkdir -p "$APP_DIR" "$DATA_DIR" "$BACKUP_DIR" "$DATA_DIR/apps" "$DATA_DIR/whatsapp-auth"
}

ensure_config() {
  ensure_dirs
  if [[ ! -f "$CONFIG_FILE" ]]; then
    if [[ -f "$SRC_DIR/config.env.example" ]]; then
      cp "$SRC_DIR/config.env.example" "$CONFIG_FILE"
    else
      cat > "$CONFIG_FILE" <<CFG
BOT_TOKEN=
ADMIN_IDS=
ADMIN_DISPLAY_NAME=Admin
WHATSAPP_ADMIN_NUMBERS=
SERVER_HOST=
SSH_PORTS=22,80,443
SSH_SHELL=/bin/false
BOT_DATA_DIR=$DATA_DIR
GESTOR_DB_PATH=$DATA_DIR/gestor.db
USUARIOS_DB_PATH=/root/usuarios.db
CHECKUSER_DB_PATH=/root/db.sqlite3
PRINCIPAL_MANAGER_ONLY=0
REMOTE_AGENT_PORT=$DEFAULT_AGENT_PORT
REMOTE_AGENT_TOKEN=
REMOTE_VPS_CONFIG=$SERVERS_FILE
CHECKUSER_HOST=0.0.0.0
CHECKUSER_PORT=$DEFAULT_CHECKUSER_PORT
CHECKUSER_PUBLIC_URL=
CHECKUSER_CENTRAL_MODE=1
VPN_DNS_DOMAIN=
VPN_DNS_ENABLED=0
VPN_DNS_INTERVAL=60
CLOUDFLARE_API_TOKEN=
VPS_SUSPENSION_INTERVAL=5
ONLINE_XRAY_WINDOW_SECONDS=30
WHATSAPP_AUTH_DIR=$DATA_DIR/whatsapp-auth
CFG
    fi
    chmod 600 "$CONFIG_FILE" 2>/dev/null || true
  fi
  touch "$DATA_DIR/users.jsonl" "$DATA_DIR/resellers.json" "$SERVERS_FILE"
  chmod 600 "$CONFIG_FILE" 2>/dev/null || true
}

set_env_value() {
  local key="$1" value="$2" file="${3:-$CONFIG_FILE}" escaped
  mkdir -p "$(dirname "$file")"
  touch "$file"
  escaped=$(printf '%s' "$value" | sed 's/[&/\\]/\\&/g')
  if grep -qE "^${key}=" "$file"; then
    sed -i "s/^${key}=.*/${key}=${escaped}/" "$file"
  else
    printf '%s=%s\n' "$key" "$value" >> "$file"
  fi
}

get_env_value() {
  local key="$1" file="${2:-$CONFIG_FILE}"
  [[ -f "$file" ]] || return 0
  grep -E "^${key}=" "$file" | tail -n1 | cut -d= -f2-
}


is_empty_or_placeholder() {
  local key="$1" value="${2:-}"
  value="${value//[$'\t\r\n ']/}"
  case "$key:$value" in
    BOT_TOKEN:|BOT_TOKEN:COLE_SEU_TOKEN_AQUI|BOT_TOKEN:TOKEN|BOT_TOKEN:SEU_TOKEN|BOT_TOKEN:123456:ABC*) return 0 ;;
    ADMIN_IDS:|ADMIN_IDS:123456789|ADMIN_IDS:SEU_ID|ADMIN_IDS:ID_ADMIN) return 0 ;;
  esac
  [[ -z "$value" ]]
}

same_path() {
  local a="$1" b="$2"
  [[ -e "$a" && -e "$b" ]] || return 1
  [[ "$(cd "$a" 2>/dev/null && pwd -P)" == "$(cd "$b" 2>/dev/null && pwd -P)" ]]
}


sqlite_get_setting() {
  local key="$1" db_file="${DB_FILE:-/etc/primecel-gestor/gestor.db}" esc_key
  [[ -n "$key" && -f "$db_file" ]] || return 0
  command -v sqlite3 >/dev/null 2>&1 || return 0
  esc_key="${key//'/''}"
  sqlite3 "$db_file" "SELECT value FROM settings WHERE key='$esc_key' LIMIT 1;" 2>/dev/null || true
}

sqlite_set_setting() {
  local key="$1" value="$2" db_file="${DB_FILE:-/etc/primecel-gestor/gestor.db}" now esc_key esc_value esc_now
  [[ -n "$key" ]] || return 0
  command -v sqlite3 >/dev/null 2>&1 || return 0
  mkdir -p "$(dirname "$db_file")" 2>/dev/null || true
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  esc_key="${key//'/''}"
  esc_value="${value//'/''}"
  esc_now="${now//'/''}"
  sqlite3 "$db_file" "CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL); INSERT INTO settings(key,value,updated_at) VALUES('$esc_key','$esc_value','$esc_now') ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at;" 2>/dev/null || true
}

save_cloudflare_token_internal() {
  local token="$1"
  token="$(printf '%s' "$token" | tr -d '\r\n' | sed 's/^ *//;s/ *$//')"
  [[ -z "$token" ]] && return 0

  # Fonte central única: SQLite settings.cloudflare_token.
  # Arquivos antigos são usados apenas como origem de migração/compatibilidade.
  sqlite_set_setting "cloudflare_token" "$token"
}

load_saved_cloudflare_token() {
  local cfg_token="" db_token="" legacy_token="" check_token="" file_token=""

  # Fonte central atual: SQLite.
  db_token="$(sqlite_get_setting cloudflare_token | tail -1 | tr -d '\r\n' || true)"
  [[ -n "$db_token" ]] && CLOUDFLARE_API_TOKEN="$db_token"

  # Migração automática: se SQLite ainda estiver vazio, busca arquivos antigos e salva no SQLite.
  if [[ -z "${CLOUDFLARE_API_TOKEN:-}" && -f "$TOKEN_FILE" ]]; then
    file_token="$(grep -E '^CLOUDFLARE_API_TOKEN=' "$TOKEN_FILE" 2>/dev/null | tail -1 | cut -d= -f2- | tr -d '"' | tr -d '\r\n' || true)"
    [[ -n "$file_token" ]] && CLOUDFLARE_API_TOKEN="$file_token"
  fi
  if [[ -z "${CLOUDFLARE_API_TOKEN:-}" ]]; then
    cfg_token="$(get_env_value CLOUDFLARE_API_TOKEN "$CONFIG_FILE" 2>/dev/null || true)"
    [[ -n "$cfg_token" ]] && CLOUDFLARE_API_TOKEN="$cfg_token"
  fi
  if [[ -z "${CLOUDFLARE_API_TOKEN:-}" && -f "/etc/checkuser/checkuser.env" ]]; then
    check_token="$(grep -E '^CHECKUSER_CLOUDFLARE_API_TOKEN=' /etc/checkuser/checkuser.env 2>/dev/null | tail -1 | cut -d= -f2- | tr -d '"' | tr -d '\r\n' || true)"
    [[ -n "$check_token" ]] && CLOUDFLARE_API_TOKEN="$check_token"
  fi
  if [[ -z "${CLOUDFLARE_API_TOKEN:-}" && -f "$LEGACY_TOKEN_FILE" ]]; then
    legacy_token="$(grep -E '^CLOUDFLARE_API_TOKEN=' "$LEGACY_TOKEN_FILE" 2>/dev/null | tail -1 | cut -d= -f2- | tr -d '"' | tr -d '\r\n' || true)"
    [[ -n "$legacy_token" ]] && CLOUDFLARE_API_TOKEN="$legacy_token"
  fi

  CLOUDFLARE_API_TOKEN="$(printf '%s' "${CLOUDFLARE_API_TOKEN:-}" | tr -d '\r\n' | sed 's/^ *//;s/ *$//')"
  export CLOUDFLARE_API_TOKEN="${CLOUDFLARE_API_TOKEN:-}"
  if [[ -n "$CLOUDFLARE_API_TOKEN" ]]; then
    save_cloudflare_token_internal "$CLOUDFLARE_API_TOKEN" >/dev/null 2>&1 || true
  fi
}

pre_update_snapshot() {
  local ts out
  ensure_dirs
  ts=$(date +%Y%m%d-%H%M%S)
  out="$BACKUP_DIR/pre-update-${APP_NAME}-${ts}.tar.gz"
  tar -czf "$out" --ignore-failed-read -C / \
    "etc/primecel-gestor/config.env" \
    "etc/primecel-gestor/gestor.db" \
    "etc/primecel-gestor/gestor.db-wal" \
    "etc/primecel-gestor/gestor.db-shm" \
    "etc/primecel-gestor/users.jsonl" \
    "etc/primecel-gestor/resellers.json" \
    "etc/primecel-gestor/servers.conf" \
    "root/usuarios.db" "root/db.sqlite3" 2>/dev/null || true
}

install_binary() {
  ensure_dirs
  local arch tmp_bin
  arch="$(detect_arch)"

  # Ubuntu 20 usa glibc 2.31. Binários pré-compilados em sistemas novos podem exigir
  # GLIBC_2.32/2.34 e falhar no systemd. Por isso a instalação agora SEMPRE compila
  # localmente quando as fontes estão presentes. O binário fica vinculado à glibc local
  # da VPS (amd64 ou arm64), evitando erro de arquitetura/GLIBC.
  if [[ ! -f "$SRC_DIR/go.mod" || ! -d "$SRC_DIR/cmd/primecel-gestor" ]]; then
    fail "Fontes Go ausentes em $SRC_DIR. Extraia novamente o pacote completo .tar.gz."
  fi

  ensure_go_dependency
  tmp_bin="$APP_DIR/.primecel-gestor.new"
  rm -f "$tmp_bin"
  {
    echo "== Compilando primecel-gestor localmente =="
    echo "Data: $(date -Is)"
    echo "Arquitetura detectada: $(uname -m) -> $arch"
    echo "Go: $(go version 2>/dev/null || true)"
    echo "Glibc: $(ldd --version 2>/dev/null | head -n1 || true)"
  } >>"$INSTALL_LOG" 2>&1 || true

  (cd "$SRC_DIR" && CGO_ENABLED=1 GOFLAGS="${GOFLAGS:-}" go build -trimpath -ldflags "-s -w" -o "$tmp_bin" ./cmd/primecel-gestor) >>"$INSTALL_LOG" 2>&1 || fail "Falha ao compilar o binário local para $arch. Veja $INSTALL_LOG"
  chmod +x "$tmp_bin"
  "$tmp_bin" version >>"$INSTALL_LOG" 2>&1 || fail "Binário compilado não executou corretamente. Veja $INSTALL_LOG"
  mv -f "$tmp_bin" "$BIN_PATH"
  chmod +x "$BIN_PATH"
  "$BIN_PATH" version >>"$INSTALL_LOG" 2>&1 || fail "Binário instalado não executou corretamente. Veja $INSTALL_LOG"
}

copy_dir_fallback() {
  local src="$1" dst="$2"
  [[ -d "$src" ]] || return 0
  mkdir -p "$dst"
  if same_path "$src" "$dst"; then
    return 0
  fi
  if command -v rsync >/dev/null 2>&1; then
    rsync -a --delete "$src/" "$dst/"
  else
    find "$dst" -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || true
    cp -a "$src/." "$dst/"
  fi
}

install_project_files() {
  ensure_dirs
  mkdir -p "$APP_DIR/whatsapp" "$APP_DIR/scripts"

  # Mantém as fontes Go em /opt para que o botmenu também consiga reinstalar/compilar
  # localmente em Ubuntu 20/ARM sem depender de binário pré-compilado.
  if [[ -d "$SRC_DIR/cmd" && ! "$(cd "$SRC_DIR" 2>/dev/null && pwd -P)" == "$(cd "$APP_DIR" 2>/dev/null && pwd -P)" ]]; then
    mkdir -p "$APP_DIR/cmd" "$APP_DIR/gestor_bot"
    copy_dir_fallback "$SRC_DIR/cmd" "$APP_DIR/cmd"
    copy_dir_fallback "$SRC_DIR/gestor_bot" "$APP_DIR/gestor_bot"
    cp -f "$SRC_DIR/go.mod" "$APP_DIR/go.mod" 2>/dev/null || true
    cp -f "$SRC_DIR/go.sum" "$APP_DIR/go.sum" 2>/dev/null || true
    cp -f "$SRC_DIR/config.env.example" "$APP_DIR/config.env.example" 2>/dev/null || true
  fi

  if [[ -d "$SRC_DIR/whatsapp" ]]; then
    copy_dir_fallback "$SRC_DIR/whatsapp" "$APP_DIR/whatsapp"
  fi

  if [[ -d "$SRC_DIR/checkuser_installer" && -f "$SRC_DIR/checkuser_installer/install.sh" ]]; then
    rm -rf "$APP_DIR/checkuser_installer" "$CHECKUSER_INSTALLER_STORE"
    mkdir -p "$APP_DIR/checkuser_installer" "$CHECKUSER_INSTALLER_STORE"
    copy_dir_fallback "$SRC_DIR/checkuser_installer" "$APP_DIR/checkuser_installer"
    copy_dir_fallback "$SRC_DIR/checkuser_installer" "$CHECKUSER_INSTALLER_STORE"
    chmod +x "$APP_DIR/checkuser_installer/install.sh" "$CHECKUSER_INSTALLER_STORE/install.sh" 2>/dev/null || true
  fi

  cp -f "$SRC_DIR/scripts/install.sh" "$APP_DIR/scripts/install.sh" 2>/dev/null || true
  cp -f "$SRC_DIR/scripts/uninstall.sh" "$APP_DIR/scripts/uninstall.sh" 2>/dev/null || true
  chmod +x "$APP_DIR/scripts/"*.sh 2>/dev/null || true

  if command -v npm >/dev/null 2>&1 && [[ -f "$APP_DIR/whatsapp/package.json" ]]; then
    (cd "$APP_DIR/whatsapp" && NPM_CONFIG_FUND=false NPM_CONFIG_AUDIT=false run_quiet npm install --omit=dev --silent --no-audit --no-fund) || fail "Falha ao instalar dependências do WhatsApp. Log: $INSTALL_LOG"
  fi

  cat > "$BOTMENU_BIN" <<EOF
#!/usr/bin/env bash
exec "$APP_DIR/scripts/install.sh" "\$@"
EOF
  chmod +x "$BOTMENU_BIN" 2>/dev/null || true
}

write_service_bot() {
  cat > /etc/systemd/system/primecel-gestor.service <<UNIT
[Unit]
Description=PrimeCel Gestor Telegram Bot
After=network.target

[Service]
Type=simple
WorkingDirectory=$APP_DIR
EnvironmentFile=$CONFIG_FILE
ExecStart=$BIN_PATH bot
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
UNIT
}

write_service_agent() {
  cat > /etc/systemd/system/primecel-gestor-agent.service <<UNIT
[Unit]
Description=PrimeCel Gestor Remote Agent
After=network.target

[Service]
Type=simple
WorkingDirectory=$APP_DIR
EnvironmentFile=$CONFIG_FILE
Environment=REMOTE_AGENT_MODE=1
ExecStart=$BIN_PATH agent --start --host 0.0.0.0 --port \${REMOTE_AGENT_PORT:-$DEFAULT_AGENT_PORT}
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
UNIT
}

write_service_checkuser() {
  cat > /etc/systemd/system/primecel-checkuser.service <<UNIT
[Unit]
Description=PrimeCel CheckUser
After=network.target

[Service]
Type=simple
WorkingDirectory=$APP_DIR
EnvironmentFile=$CONFIG_FILE
ExecStart=$BIN_PATH checkuser --start
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
UNIT
}

write_service_webhook() {
  cat > /etc/systemd/system/primecel-webhook.service <<UNIT
[Unit]
Description=PrimeCel Payments WebHook
After=network.target primecel-gestor.service

[Service]
Type=simple
WorkingDirectory=$APP_DIR
EnvironmentFile=$CONFIG_FILE
ExecStart=$BIN_PATH payments webhook --start --port $DEFAULT_WEBHOOK_PORT
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
UNIT
}

write_service_whatsapp() {
  local node_bin
  node_bin="$(node_bin_path)"
  cat > /etc/systemd/system/primecel-whatsapp.service <<UNIT
[Unit]
Description=PrimeCel WhatsApp Bridge
After=network.target primecel-gestor.service

[Service]
Type=simple
WorkingDirectory=$APP_DIR/whatsapp
EnvironmentFile=$CONFIG_FILE
Environment=PRIMECEL_BIN=$BIN_PATH
Environment=PRIMECEL_GESTOR_BIN=$BIN_PATH
Environment=PRIMECEL_ENV_FILE=$CONFIG_FILE
Environment=CONFIG_ENV=$CONFIG_FILE
Environment=WHATSAPP_AUTH_DIR=$DATA_DIR/whatsapp-auth
ExecStart=$node_bin $APP_DIR/whatsapp/primecel-whatsapp.js
Restart=always
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
UNIT
}

reload_systemd() { systemctl daemon-reload >>"$INSTALL_LOG" 2>&1 || true; }

systemd_has_unit() {
  local svc="$1"
  [[ -f "/etc/systemd/system/${svc}.service" ]] && return 0
  systemctl list-unit-files "${svc}.service" 2>/dev/null | grep -q "^${svc}.service" && return 0
  systemctl cat "${svc}.service" >/dev/null 2>&1 && return 0
  return 1
}

enable_restart() {
  local unit="$1"
  systemctl enable "$unit" >>"$INSTALL_LOG" 2>&1 || return 1
  systemctl restart "$unit" >>"$INSTALL_LOG" 2>&1 || return 1
}

start_service_checked() {
  local svc="$1" unit="${1}.service" active_state sub_state main_pid i
  if [[ ! -f "/etc/systemd/system/${unit}" ]]; then
    fail "Serviço ${unit} não foi criado. Log: $INSTALL_LOG"
  fi
  reload_systemd
  systemctl reset-failed "$unit" >>"$INSTALL_LOG" 2>&1 || true
  if ! systemctl enable "$unit" >>"$INSTALL_LOG" 2>&1; then
    fail "Não foi possível habilitar ${unit}. Log: $INSTALL_LOG"
  fi
  if ! systemctl restart "$unit" >>"$INSTALL_LOG" 2>&1; then
    journalctl -u "$unit" -n 120 --no-pager >>"$INSTALL_LOG" 2>&1 || true
    warn "${unit} foi criado, mas não iniciou. Veja: journalctl -u ${svc} -n 120 --no-pager"
    return 1
  fi

  for i in $(seq 1 25); do
    active_state="$(systemctl is-active "$unit" 2>/dev/null || true)"
    sub_state="$(systemctl show "$unit" -p SubState --value 2>/dev/null || true)"
    main_pid="$(systemctl show "$unit" -p MainPID --value 2>/dev/null || echo 0)"

    if [[ "$active_state" == "active" ]]; then
      return 0
    fi

    # Para serviços Go simples, alguns ambientes retornam "activating" por alguns segundos.
    # Se houver PID principal rodando e não estiver em auto-restart/falha, aceitamos como iniciado.
    if [[ "$active_state" == "activating" && "${main_pid:-0}" != "0" && "$sub_state" != "auto-restart" && "$sub_state" != "failed" ]]; then
      return 0
    fi

    if [[ "$active_state" == "failed" || "$sub_state" == "failed" || "$sub_state" == "auto-restart" ]]; then
      break
    fi
    sleep 1
  done

  active_state="$(systemctl is-active "$unit" 2>/dev/null || true)"
  sub_state="$(systemctl show "$unit" -p SubState --value 2>/dev/null || true)"
  main_pid="$(systemctl show "$unit" -p MainPID --value 2>/dev/null || echo 0)"
  journalctl -u "$unit" -n 120 --no-pager >>"$INSTALL_LOG" 2>&1 || true
  warn "${unit} não ficou ativo. Estado: ${active_state:-desconhecido}/${sub_state:-desconhecido}, PID: ${main_pid:-0}. Veja: journalctl -u ${svc} -n 120 --no-pager"
  return 1
}

stop_disable_remove() { local svc="$1"; systemctl stop "$svc.service" 2>/dev/null || true; systemctl disable "$svc.service" 2>/dev/null || true; rm -f "/etc/systemd/system/$svc.service"; }

service_state() {
  local svc="$1" state sub
  if systemd_has_unit "$svc"; then
    state="$(systemctl is-active "$svc.service" 2>/dev/null || true)"
    sub="$(systemctl show "$svc.service" -p SubState --value 2>/dev/null || true)"
    case "$state" in
      active) echo "✅ ativo" ;;
      activating) echo "⏳ iniciando" ;;
      failed) echo "❌ falhou" ;;
      inactive) echo "⚠️ parado" ;;
      *) echo "⚠️ ${state:-parado}${sub:+/$sub}" ;;
    esac
  else
    if [[ "$svc" == "primecel-gestor" && -x "$BIN_PATH" ]]; then
      echo "⚠️ sem serviço"
    else
      echo "— não instalado"
    fi
  fi
}

service_icon() {
  local svc="$1"
  if systemctl is-active --quiet "$svc.service" 2>/dev/null || systemctl is-active --quiet "$svc" 2>/dev/null; then
    echo "✅"
  else
    echo "❌"
  fi
}

checkuser_icon() {
  if systemctl is-active --quiet checkuser 2>/dev/null || systemctl is-active --quiet checkuser.service 2>/dev/null || systemctl is-active --quiet primecel-checkuser.service 2>/dev/null; then
    echo "✅"
  else
    echo "❌"
  fi
}

show_status_short() {
  echo -e "${WHITE}     Bot: $(service_icon primecel-gestor) | CheckUser: $(checkuser_icon) | WhatsApp: $(service_icon primecel-whatsapp)${NC}"
}

show_status_manage() {
  echo -e "${WHITE}             Bot: $(service_icon primecel-gestor) | WhatsApp: $(service_icon primecel-whatsapp)${NC}"
}

install_bot_label() {
  if systemd_has_unit primecel-gestor || [[ -x "$BIN_PATH" ]]; then
    echo "Atualizar Bot"
  else
    echo "Instalar Bot"
  fi
}

whatsapp_manage_label() {
  if systemctl is-active --quiet primecel-whatsapp.service 2>/dev/null; then
    echo "Desativar Bot WhatsApp"
  elif systemctl is-failed --quiet primecel-whatsapp.service 2>/dev/null; then
    echo "Reinstalar Bot WhatsApp"
  elif systemd_has_unit primecel-whatsapp; then
    echo "Ativar Bot WhatsApp"
  else
    echo "Instalar Bot WhatsApp"
  fi
}

general_bot_label() {
  if systemctl is-active --quiet primecel-gestor.service 2>/dev/null || systemctl is-active --quiet primecel-whatsapp.service 2>/dev/null; then
    echo "Parar Bot Geral"
  else
    echo "Iniciar Bot Geral"
  fi
}

run_bin() {
  ensure_config
  [[ -x "$BIN_PATH" ]] || install_binary
  CONFIG_ENV="$CONFIG_FILE" "$BIN_PATH" "$@"
}

run_bin_quiet() {
  ensure_config
  [[ -x "$BIN_PATH" ]] || install_binary
  CONFIG_ENV="$CONFIG_FILE" "$BIN_PATH" "$@" >>"$INSTALL_LOG" 2>&1
}


get_public_ip() {
  local ip=""
  ip="$(curl -4 -fsS --max-time 4 https://api.ipify.org 2>/dev/null || true)"
  if [[ -z "$ip" ]]; then
    ip="$(curl -4 -fsS --max-time 4 https://ifconfig.me/ip 2>/dev/null || true)"
  fi
  printf '%s' "$ip" | tr -d '\r\n' | sed 's/^ *//;s/ *$//'
}

pix_cf_api_request() {
  local token="$1" method="$2" path="$3" data="${4:-}"
  if [[ -n "$data" ]]; then
    curl -fsS --max-time 25 -X "$method" "https://api.cloudflare.com/client/v4${path}" \
      -H "Authorization: Bearer ${token}" \
      -H "Content-Type: application/json" \
      --data "$data"
  else
    curl -fsS --max-time 25 -X "$method" "https://api.cloudflare.com/client/v4${path}" \
      -H "Authorization: Bearer ${token}" \
      -H "Content-Type: application/json"
  fi
}

pix_cf_require_success() {
  local json="$1" action="$2"
  if ! printf '%s' "$json" | jq -e '.success == true' >/dev/null 2>&1; then
    warn "Cloudflare falhou ao ${action}."
    printf '%s\n' "$json" >>"$INSTALL_LOG" 2>&1 || true
    return 1
  fi
  return 0
}

pix_cf_token_or_ask() {
  local token="" err
  load_saved_cloudflare_token
  token="${CLOUDFLARE_API_TOKEN:-}"
  if [[ -n "$token" ]]; then
    printf '%s' "$token"
    return 0
  fi
  echo "Token Cloudflare necessário para configurar a API Pix."
  echo "Permissões necessárias: Zone:Read, DNS:Edit e Rulesets:Edit."
  token="$(ask_cloudflare_token_validated)" || return 1
  token="$(printf '%s' "$token" | tr -d '\r\n' | sed 's/^ *//;s/ *$//')"
  if [[ -z "$token" ]]; then
    warn "Token Cloudflare vazio. API Pix não configurada."
    return 1
  fi
  save_cloudflare_token_internal "$token" >/dev/null 2>&1 || true
  set_env_value CLOUDFLARE_API_TOKEN "$token" >/dev/null 2>&1 || true
  printf '%s' "$token"
}

pix_cf_update_dns() {
  local token="$1" zone_id="$2" fqdn="$3" ip="$4" response count body id
  response="$(pix_cf_api_request "$token" GET "/zones/${zone_id}/dns_records?type=A&name=${fqdn}")" || return 1
  pix_cf_require_success "$response" "consultar DNS da API Pix" || return 1
  count="$(printf '%s' "$response" | jq '.result | length')"
  body="$(jq -n --arg name "$fqdn" --arg content "$ip" '{type:"A", name:$name, content:$content, ttl:1, proxied:true}')"
  if [[ "$count" -gt 0 ]]; then
    while read -r id; do
      [[ -z "$id" || "$id" == "null" ]] && continue
      response="$(pix_cf_api_request "$token" PUT "/zones/${zone_id}/dns_records/${id}" "$body")" || return 1
      pix_cf_require_success "$response" "atualizar DNS da API Pix" || return 1
    done < <(printf '%s' "$response" | jq -r '.result[]?.id')
  else
    response="$(pix_cf_api_request "$token" POST "/zones/${zone_id}/dns_records" "$body")" || return 1
    pix_cf_require_success "$response" "criar DNS da API Pix" || return 1
  fi
  return 0
}

pix_cf_update_origin_rule() {
  local token="$1" zone_id="$2" fqdn="$3" port="$4" rulesets ruleset_id response full ref body
  rulesets="$(pix_cf_api_request "$token" GET "/zones/${zone_id}/rulesets")" || return 1
  pix_cf_require_success "$rulesets" "listar Origin Rules" || return 1
  ruleset_id="$(printf '%s' "$rulesets" | jq -r '.result[] | select(.phase=="http_request_origin" and .kind=="zone") | .id' | head -n1)"
  if [[ -z "$ruleset_id" || "$ruleset_id" == "null" ]]; then
    body='{"name":"PrimeCel Pix API Origin Rules","kind":"zone","phase":"http_request_origin","rules":[]}'
    response="$(pix_cf_api_request "$token" POST "/zones/${zone_id}/rulesets" "$body")" || return 1
    pix_cf_require_success "$response" "criar ruleset da API Pix" || return 1
    ruleset_id="$(printf '%s' "$response" | jq -r '.result.id')"
  fi
  full="$(pix_cf_api_request "$token" GET "/zones/${zone_id}/rulesets/${ruleset_id}")" || return 1
  pix_cf_require_success "$full" "ler ruleset da API Pix" || return 1
  ref="primecel_pix_api_$(printf '%s' "$fqdn" | sed 's/[^A-Za-z0-9_]/_/g')"
  body="$(printf '%s' "$full" | jq --arg fqdn "$fqdn" --arg ref "$ref" --argjson port "$port" '
    .result.rules as $rules |
    {
      rules: (
        ($rules // [] | map(select((.ref // "") != $ref and (.description // "") != ("PrimeCel Pix API - " + $fqdn) and (.expression // "") != ("http.host eq \"" + $fqdn + "\""))))
        + [{
          ref: $ref,
          expression: ("http.host eq \"" + $fqdn + "\""),
          description: ("PrimeCel Pix API - " + $fqdn),
          action: "route",
          action_parameters: { origin: { port: $port } }
        }]
      )
    }
  ' )"
  response="$(pix_cf_api_request "$token" PUT "/zones/${zone_id}/rulesets/${ruleset_id}" "$body")" || return 1
  pix_cf_require_success "$response" "criar/atualizar Origin Rule da API Pix" || return 1
}

install_pix_api_from_option1() {
  need_root
  ensure_config
  if ! command -v jq >/dev/null 2>&1; then
    if command -v apt-get >/dev/null 2>&1; then
      apt-get update -y >/dev/null 2>&1 || true
      DEBIAN_FRONTEND=noninteractive apt-get install -y jq >/dev/null 2>&1 || true
    fi
  fi
  if ! command -v jq >/dev/null 2>&1; then
    warn "jq não encontrado. Não foi possível configurar a API Pix automaticamente."
    return 1
  fi

  local token zones count idx choice zone_id zone_name fqdn ip api_url status_url
  token="$(pix_cf_token_or_ask)" || return 1
  zones="$(pix_cf_api_request "$token" GET "/zones?per_page=100")" || { warn "Não foi possível conectar à Cloudflare."; return 1; }
  pix_cf_require_success "$zones" "listar domínios" || return 1
  count="$(printf '%s' "$zones" | jq '.result | length')"
  if [[ "$count" -lt 1 ]]; then
    warn "Nenhum domínio encontrado nessa conta/token."
    return 1
  fi

  if [[ "$count" -eq 1 ]]; then
    idx=0
    zone_name="$(printf '%s' "$zones" | jq -r '.result[0].name')"
    echo "Domínio: ${zone_name}"
  else
    echo "Domínios encontrados:"
    printf '%s' "$zones" | jq -r '.result | to_entries[] | "\(.key + 1). \(.value.name)"'
    echo
    read -r -p "Escolha o domínio para Pix: " choice || true
    if ! [[ "$choice" =~ ^[0-9]+$ ]] || [[ "$choice" -lt 1 || "$choice" -gt "$count" ]]; then
      warn "Opção inválida."
      return 1
    fi
    idx=$((choice - 1))
    zone_name="$(printf '%s' "$zones" | jq -r ".result[$idx].name")"
  fi

  zone_id="$(printf '%s' "$zones" | jq -r ".result[$idx].id")"
  fqdn="pix.${zone_name}"
  ip="$(get_public_ip)"
  if [[ -z "$ip" ]]; then
    read -r -p "IP público da VPS principal: " ip || true
    ip="$(printf '%s' "$ip" | tr -d '\r\n' | sed 's/^ *//;s/ *$//')"
  fi
  if [[ -z "$ip" ]]; then
    warn "IP público vazio. API Pix não configurada."
    return 1
  fi

  pix_cf_update_dns "$token" "$zone_id" "$fqdn" "$ip" || return 1
  pix_cf_update_origin_rule "$token" "$zone_id" "$fqdn" "$DEFAULT_WEBHOOK_PORT" || return 1

  api_url="https://${fqdn}"
  status_url="${api_url}/pix/status"
  sqlite_set_setting "payments_webhook_url" "${api_url}/pix" >/dev/null 2>&1 || true
  sqlite_set_setting "payments_api_url" "$api_url" >/dev/null 2>&1 || true
  set_env_value PAYMENTS_WEBHOOK_URL "${api_url}/pix" >/dev/null 2>&1 || true
  set_env_value PAYMENTS_API_URL "$api_url" >/dev/null 2>&1 || true

  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -qi active; then
    ufw allow "${DEFAULT_WEBHOOK_PORT}/tcp" >/dev/null 2>&1 || true
  fi

  write_service_webhook
  reload_systemd
  start_service_checked primecel-webhook || true

  echo "API Pix: ${api_url}"
  echo "WebHook Pix: ${api_url}/pix"
  return 0
}

print_configured_domains() {
  local check_url
  check_url="$(get_env_value CHECKUSER_PUBLIC_URL || true)"
  echo "Servidores: vpn.primecel.shop"
  if [[ -n "$check_url" ]]; then
    echo "CheckUser: $check_url"
  fi
}

ask_import_backup_before_install() {
  echo
  read -r -p "Importar backup? (/root/backup-painel.tar.gz) [s/N]: " BACKUP_RESP || true
  BACKUP_RESP="${BACKUP_RESP:-N}"
  if [[ ! "$BACKUP_RESP" =~ ^[sS]$ ]]; then
    return 0
  fi

  local backup_file="/root/backup-painel.tar.gz"
  if [[ ! -f "$backup_file" ]]; then
    warn "Backup não encontrado em $backup_file. Continuando sem importar."
    return 0
  fi

  if ! tar -tzf "$backup_file" >/dev/null 2>&1; then
    fail "Arquivo .tar.gz inválido ou corrompido: $backup_file"
  fi

  echo
  echo "Backup encontrado: $backup_file"
  read -r -p "Confirmar importação do backup? [s/N]: " CONFIRM || true
  CONFIRM="${CONFIRM:-N}"
  if [[ ! "$CONFIRM" =~ ^[sS]$ ]]; then
    warn "Importação cancelada. Continuando sem importar backup."
    return 0
  fi

  progress_bar "Importando backup"
  run_bin backup import --file "$backup_file" --clean --confirm IMPORTAR || {
    fail "Não foi possível importar o backup."
  }
  ok "Backup importado"
}

ask_install_bot_after_dragonssh() {
  echo
  read -r -p "Instalar/Atualizar Bot? [S/n]: " INSTALL_RESP || true
  INSTALL_RESP="${INSTALL_RESP:-S}"
  if [[ "$INSTALL_RESP" =~ ^[nN]$ ]]; then
    warn "Instalação cancelada."
    pause
    return 1
  fi
  return 0
}

install_or_update() {
  need_root

  local UPDATE_MODE=0
  if [[ -f "$CONFIG_FILE" && -x "$BIN_PATH" ]]; then
    UPDATE_MODE=1
  fi

  local TOTAL_STEPS=9
  local STEP_NOW=1
  install_step_screen "$UPDATE_MODE" "$STEP_NOW" "$TOTAL_STEPS" "Verificando/instalando dependências"
  ensure_all_dependencies || { install_error_screen "$UPDATE_MODE" "Verificando dependências" "Falha ao preparar dependências. Log: $INSTALL_LOG"; return 0; }

  STEP_NOW=2
  install_step_screen "$UPDATE_MODE" "$STEP_NOW" "$TOTAL_STEPS" "Preparando instalação"
  ensure_dirs
  ensure_config

  local BOT_TOKEN_INPUT ADMIN_IDS_INPUT CF_TOKEN_INPUT NEED_BOT_CONFIG NEED_CF_CONFIG
  BOT_TOKEN_INPUT="$(get_env_value BOT_TOKEN || true)"
  ADMIN_IDS_INPUT="$(get_env_value ADMIN_IDS || true)"
  load_saved_cloudflare_token >/dev/null 2>&1 || true
  CF_TOKEN_INPUT="${CLOUDFLARE_API_TOKEN:-}"
  NEED_BOT_CONFIG=0
  NEED_CF_CONFIG=0
  if [[ "$UPDATE_MODE" -eq 0 ]] || is_empty_or_placeholder BOT_TOKEN "$BOT_TOKEN_INPUT" || is_empty_or_placeholder ADMIN_IDS "$ADMIN_IDS_INPUT"; then
    NEED_BOT_CONFIG=1
  fi
  if [[ "$UPDATE_MODE" -eq 0 && -z "$CF_TOKEN_INPUT" ]]; then
    NEED_CF_CONFIG=1
  fi

  if [[ "$NEED_BOT_CONFIG" -eq 1 ]]; then
    if [[ "$UPDATE_MODE" -eq 0 ]] || is_empty_or_placeholder BOT_TOKEN "$BOT_TOKEN_INPUT"; then
      install_step_screen "$UPDATE_MODE" "$STEP_NOW" "$TOTAL_STEPS" "Aguardando Token do Bot"
      BOT_TOKEN_INPUT="$(ask_bot_token_validated)"
      set_env_value BOT_TOKEN "$BOT_TOKEN_INPUT"
      chmod 600 "$CONFIG_FILE" 2>/dev/null || true
    fi
    if [[ "$UPDATE_MODE" -eq 0 ]] || is_empty_or_placeholder ADMIN_IDS "$ADMIN_IDS_INPUT"; then
      install_step_screen "$UPDATE_MODE" "$STEP_NOW" "$TOTAL_STEPS" "Aguardando ID Telegram do Admin"
      ADMIN_IDS_INPUT="$(ask_admin_ids_validated)"
      set_env_value ADMIN_IDS "$ADMIN_IDS_INPUT"
      chmod 600 "$CONFIG_FILE" 2>/dev/null || true
    fi
  fi

  if [[ "$NEED_CF_CONFIG" -eq 1 ]]; then
    install_step_screen "$UPDATE_MODE" "$STEP_NOW" "$TOTAL_STEPS" "Validando Token Cloudflare"
    CF_TOKEN_INPUT="$(ask_cloudflare_token_validated)"
    save_cloudflare_token_internal "$CF_TOKEN_INPUT" >/dev/null 2>&1 || true
    set_env_value CLOUDFLARE_API_TOKEN "$CF_TOKEN_INPUT"
    chmod 600 "$CONFIG_FILE" 2>/dev/null || true
  fi

  STEP_NOW=3
  install_step_screen "$UPDATE_MODE" "$STEP_NOW" "$TOTAL_STEPS" "Validando ambiente"
  command -v systemctl >/dev/null 2>&1 || warn "systemctl não encontrado; serviços podem não iniciar neste ambiente."
  command -v tar >/dev/null 2>&1 || { install_error_screen "$UPDATE_MODE" "Validando ambiente" "tar não encontrado."; return 0; }
  command -v node >/dev/null 2>&1 || { install_error_screen "$UPDATE_MODE" "Validando ambiente" "Node.js não encontrado após instalação de dependências."; return 0; }
  command -v npm >/dev/null 2>&1 || { install_error_screen "$UPDATE_MODE" "Validando ambiente" "npm não encontrado após instalação de dependências."; return 0; }
  sleep 0.1

  STEP_NOW=4
  install_step_screen "$UPDATE_MODE" "$STEP_NOW" "$TOTAL_STEPS" "Configurando ambiente"
  pre_update_snapshot
  set_env_value BOT_DATA_DIR "$DATA_DIR"
  set_env_value GESTOR_DB_PATH "$DATA_DIR/gestor.db"
  set_env_value USUARIOS_DB_PATH "/root/usuarios.db"
  set_env_value CHECKUSER_DB_PATH "/root/db.sqlite3"
  set_env_value REMOTE_VPS_CONFIG "$SERVERS_FILE"
  set_env_value VPS_SUSPENSION_INTERVAL "5"
  set_env_value ONLINE_XRAY_WINDOW_SECONDS "30"
  set_env_value WHATSAPP_AUTH_DIR "$DATA_DIR/whatsapp-auth"
  chmod 600 "$CONFIG_FILE" 2>/dev/null || true

  STEP_NOW=5
  install_step_screen "$UPDATE_MODE" "$STEP_NOW" "$TOTAL_STEPS" "Parando serviços antigos"
  systemctl stop primecel-gestor primecel-webhook primecel-checkuser primecel-whatsapp 2>/dev/null || true
  systemctl stop tg-access-bot tg-access-bot-vps-suspension 2>/dev/null || true

  STEP_NOW=6
  if [[ "$UPDATE_MODE" -eq 0 ]]; then
    install_step_screen "$UPDATE_MODE" "$STEP_NOW" "$TOTAL_STEPS" "Instalando Bot"
  else
    install_step_screen "$UPDATE_MODE" "$STEP_NOW" "$TOTAL_STEPS" "Atualizando Bot"
  fi
  install_binary || { install_error_screen "$UPDATE_MODE" "Instalando/Atualizando Bot" "Falha ao compilar/instalar o binário. Log: $INSTALL_LOG"; return 0; }
  install_project_files || { install_error_screen "$UPDATE_MODE" "Instalando/Atualizando Bot" "Falha ao atualizar arquivos do projeto. Log: $INSTALL_LOG"; return 0; }

  STEP_NOW=7
  install_step_screen "$UPDATE_MODE" "$STEP_NOW" "$TOTAL_STEPS" "Configurando serviços"
  write_service_bot
  write_service_webhook
  reload_systemd

  STEP_NOW=8
  install_step_screen "$UPDATE_MODE" "$STEP_NOW" "$TOTAL_STEPS" "Iniciando serviços"
  start_service_checked primecel-gestor || { install_error_screen "$UPDATE_MODE" "Iniciando serviços" "O serviço primecel-gestor não ficou ativo. Veja: journalctl -u primecel-gestor -n 120 --no-pager"; return 0; }
  # API pública de pagamentos/renovação. Necessária para o layout mobile validar login e gerar Pix.
  start_service_checked primecel-webhook || true

  STEP_NOW=9
  install_step_screen "$UPDATE_MODE" "$STEP_NOW" "$TOTAL_STEPS" "Limpando cache"
  clean_primecel_cache >/dev/null 2>&1 || true

  local gestor_only cf_resp sync_principal_resp check_resp check_url_print principal_only_resp
  gestor_only="$(get_env_value PRINCIPAL_MANAGER_ONLY || echo 0)"

  if [[ "$UPDATE_MODE" -eq 0 ]]; then
    install_step_screen "$UPDATE_MODE" "$STEP_NOW" "$TOTAL_STEPS" "Modo do servidor principal"
    principal_only_resp="$(ask_yes_no_strict "> Deixar o servidor principal somente como gestor?")"
    if [[ "$principal_only_resp" == "s" ]]; then
      set_env_value PRINCIPAL_MANAGER_ONLY "1"
      gestor_only="1"
    else
      set_env_value PRINCIPAL_MANAGER_ONLY "0"
      gestor_only="0"
      install_step_screen "$UPDATE_MODE" "$STEP_NOW" "$TOTAL_STEPS" "Servidor principal"
      echo -e "> Sincronizar servidor principal agora? \033[1;32mSIM${NC}"
      set_env_value PRINCIPAL_LOCAL_SYNC "1" >/dev/null 2>&1 || true
    fi
  fi

  if [[ "$UPDATE_MODE" -eq 0 ]]; then
    install_step_screen "$UPDATE_MODE" "$TOTAL_STEPS" "$TOTAL_STEPS" "Instalando Bot"
  else
    install_step_screen "$UPDATE_MODE" "$TOTAL_STEPS" "$TOTAL_STEPS" "Atualizando Bot"
  fi
  check_resp="$(ask_yes_no_strict "> Atualizar o CheckUser?")"
  if [[ "$check_resp" == "s" ]]; then
    install_checkuser_from_option1 || { install_error_screen "$UPDATE_MODE" "CheckUser" "Falha ao instalar/atualizar CheckUser."; return 0; }
    check_url_print="$(get_env_value CHECKUSER_PUBLIC_URL || true)"
    if [[ -n "$check_url_print" ]]; then
      echo "CheckUser: $check_url_print"
    else
      echo "CheckUser: https://check.primecel.shop"
    fi
  fi

  install_step_screen "$UPDATE_MODE" "$TOTAL_STEPS" "$TOTAL_STEPS" "Concluído"
  install_progress_done
  echo
  line
  if [[ "$UPDATE_MODE" -eq 0 ]]; then
    echo -e "${GREEN}Instalação concluída com sucesso.${NC}"
  else
    echo -e "${GREEN}Atualização concluída com sucesso.${NC}"
  fi
  line
  echo
  echo -e "${GREEN}Voltando ao menu automaticamente...${NC}"
  sleep 1
}

remove_bot() {
  need_root
  box_header
  line
  echo -e "${WHITE}Remover bot${NC}"
  line
  echo "Remove apenas os serviços/código do bot Go. Dados em $DATA_DIR são preservados."
  echo
  read -r -p "Remover bot principal? [s/N]: " RESP || true
  RESP="${RESP:-N}"
  if [[ ! "$RESP" =~ ^[sS]$ ]]; then warn "Remoção cancelada."; pause; return 0; fi
  progress_bar "Removendo serviço principal"
  stop_disable_remove primecel-gestor
  stop_disable_remove primecel-webhook
  reload_systemd
  ok "Bot removido. Dados preservados."
  read -r -p "Apagar também /opt/primecel-gestor? [s/N]: " DEL || true
  DEL="${DEL:-N}"
  if [[ "$DEL" =~ ^[sS]$ ]]; then rm -rf "$APP_DIR"; ok "Arquivos de /opt removidos."; fi
  pause
}

alter_bot_config() {
  need_root
  ensure_config
  while true; do
    box_header
    line
    echo -e "${WHITE}Alterar bot${NC} ${GRAY}(token/admin)${NC}"
    line
    echo -e "  ${CYAN}1.${NC} Alterar token"
    echo -e "  ${CYAN}2.${NC} Alterar admin"
    echo -e "  ${CYAN}3.${NC} Alterar nome/WhatsApp admin"
    echo -e "  ${CYAN}4.${NC} Reiniciar bot"
    echo -e "  ${CYAN}0.${NC} Voltar"
    line
    echo
    read -r -p "Opção: " op || return 0
    case "$op" in
      1)
        local token; token=$(ask "Token do bot Telegram" "$(get_env_value BOT_TOKEN || true)")
        set_env_value BOT_TOKEN "$token"; chmod 600 "$CONFIG_FILE"; ok "Token salvo."; systemctl restart primecel-gestor.service 2>/dev/null || true; pause ;;
      2)
        local admins; admins=$(ask "Admin IDs, separados por vírgula" "$(get_env_value ADMIN_IDS || true)")
        set_env_value ADMIN_IDS "$admins"; chmod 600 "$CONFIG_FILE"; ok "Admin salvo."; systemctl restart primecel-gestor.service 2>/dev/null || true; pause ;;
      3)
        local name whats; name=$(ask "Nome exibido do admin" "$(get_env_value ADMIN_DISPLAY_NAME || echo Admin)"); whats=$(ask "WhatsApp admin com DDI" "$(get_env_value WHATSAPP_ADMIN_NUMBERS || true)")
        set_env_value ADMIN_DISPLAY_NAME "$name"; set_env_value WHATSAPP_ADMIN_NUMBERS "$whats"; run_bin settings set-profile --name "$name" --whatsapp "$whats" >/dev/null 2>&1 || true; ok "Perfil salvo."; pause ;;
      4) systemctl restart primecel-gestor.service 2>/dev/null || true; ok "Bot reiniciado."; pause ;;
      0|voltar|Voltar|VOLTAR) return 0 ;;
      *) warn "Opção inválida."; sleep 1 ;;
    esac
  done
}

restore_backup() {
  need_root
  ensure_config
  box_header
  line
  echo -e "${WHITE}Importar backup${NC} ${GRAY}(/root/backup-painel.tar.gz)${NC}"
  line
  local file sync
  file=$(ask "Caminho do backup" "/root/backup-painel.tar.gz")
  if [[ ! -f "$file" ]]; then warn "Backup não encontrado: $file"; pause; return 0; fi
  echo
  echo "Essa importação limpa contas/revendas/servidores atuais antes de importar."
  read -r -p "Digite IMPORTAR para confirmar: " confirm || true
  if [[ "$confirm" != "IMPORTAR" ]]; then warn "Importação cancelada."; pause; return 0; fi
  read -r -p "Sincronizar remoções nas VPS secundárias antes da limpeza? [s/N]: " sync || true
  progress_bar "Importando backup"
  if [[ "$sync" =~ ^[sS]$ ]]; then
    run_bin backup import --file "$file" --clean --confirm IMPORTAR --sync-remotes
  else
    run_bin backup import --file "$file" --clean --confirm IMPORTAR
  fi
  ok "Backup importado."
  pause
}

create_backup() {
  need_root
  ensure_config
  progress_bar "Gerando backup"
  run_bin backup create --output "$BACKUP_DIR/backup-painel.tar.gz"
  ok "Backup: $BACKUP_DIR/backup-painel.tar.gz"
}


run_whatsapp_installer_sync() {
  local node_bin timeout_sec qr_file status_file pair_log pair_pid shown_qr elapsed status
  node_bin="$(node_bin_path)"
  timeout_sec="${WHATSAPP_PAIR_TIMEOUT:-180}"
  qr_file="/tmp/primecel-whatsapp-qr.txt"
  status_file="/tmp/primecel-whatsapp-pair.status"
  pair_log="/tmp/primecel-whatsapp-pair.log"
  shown_qr=0
  elapsed=0
  line
  echo -e "${WHITE}Sincronizar WhatsApp${NC}"
  line
  echo -e "${CYAN}Instalação concluída. Agora o QR será exibido limpo e inteiro no próprio instalador.${NC}"
  echo -e "${CYAN}Escaneie em:${NC} WhatsApp > Aparelhos conectados > Conectar aparelho"
  echo -e "${GRAY}Arquivo do QR: ${qr_file}${NC}"
  echo

  systemctl stop primecel-whatsapp >/dev/null 2>&1 || true
  rm -f "$qr_file" "$status_file" "$pair_log"

  # Força uma nova sessão limpa quando o usuário escolhe sincronizar pelo instalador.
  # Sessões antigas/corrompidas podem ficar tentando reconectar sem emitir QR.
  if [[ -d "$DATA_DIR/whatsapp-auth" ]]; then
    rm -rf "$DATA_DIR/whatsapp-auth.bak" 2>/dev/null || true
    mv "$DATA_DIR/whatsapp-auth" "$DATA_DIR/whatsapp-auth.bak" 2>/dev/null || rm -rf "$DATA_DIR/whatsapp-auth" 2>/dev/null || true
  fi
  mkdir -p "$DATA_DIR/whatsapp-auth"
  chmod 700 "$DATA_DIR/whatsapp-auth" 2>/dev/null || true
  echo -e "${CYAN}Gerando novo QR de sincronização...${NC}"

  set +e
  WHATSAPP_QR_COMPACT="${WHATSAPP_QR_COMPACT:-1}" \
  WHATSAPP_QR_STDOUT=0 \
  WHATSAPP_PAIR_VERBOSE=0 \
  WHATSAPP_QR_FILE="$qr_file" \
  WHATSAPP_STATUS_FILE="$status_file" \
  WHATSAPP_PAIR_TIMEOUT_MS="$((timeout_sec * 1000))" \
  PRIMECEL_BIN="$BIN_PATH" \
  PRIMECEL_GESTOR_BIN="$BIN_PATH" \
  PRIMECEL_ENV_FILE="$CONFIG_FILE" \
  CONFIG_ENV="$CONFIG_FILE" \
  WHATSAPP_AUTH_DIR="$DATA_DIR/whatsapp-auth" \
  "$node_bin" "$APP_DIR/whatsapp/primecel-whatsapp.js" --pair-once >>"$pair_log" 2>&1 &
  pair_pid=$!
  set -e

  while (( elapsed < timeout_sec )); do
    status=""
    [[ -f "$status_file" ]] && status="$(tr -d '\r\n' < "$status_file")"
    if [[ "$shown_qr" -eq 0 && -s "$qr_file" ]]; then
      printf "\r\033[K" || true
      line
      echo -e "${WHITE}QR Code WhatsApp${NC}"
      line
      cat "$qr_file"
      line
      shown_qr=1
      echo -e "${CYAN}Aguardando conexão...${NC}"
    elif [[ "$shown_qr" -eq 0 && $((elapsed % 5)) -eq 0 ]]; then
      printf "\r\033[K${CYAN}Aguardando geração do QR...${NC}"
    fi
    if [[ "$status" == "connected" ]]; then
      wait "$pair_pid" >/dev/null 2>&1 || true
      ok "WhatsApp conectado."
      return 0
    fi
    if ! kill -0 "$pair_pid" >/dev/null 2>&1; then
      wait "$pair_pid" >/dev/null 2>&1 || true
      [[ -f "$status_file" ]] && status="$(tr -d '\r\n' < "$status_file")"
      if [[ "$status" == "connected" ]]; then
        ok "WhatsApp conectado."
        return 0
      fi
      break
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done

  if kill -0 "$pair_pid" >/dev/null 2>&1; then
    kill "$pair_pid" >/dev/null 2>&1 || true
    wait "$pair_pid" >/dev/null 2>&1 || true
  fi
  if [[ "$shown_qr" -eq 0 && -s "$qr_file" ]]; then
    line
    echo -e "${WHITE}QR Code WhatsApp${NC}"
    line
    cat "$qr_file"
    line
  fi
  warn "Sincronização do WhatsApp não foi concluída no tempo limite."
  if [[ ! -s "$qr_file" ]]; then
    warn "QR não foi gerado. A sessão antiga foi limpa; execute novamente a opção Instalar/Atualizar WhatsApp."
  else
    warn "Execute novamente a opção Instalar/Atualizar WhatsApp ou veja o QR salvo em: $qr_file"
  fi
  [[ -s "$pair_log" ]] && cat "$pair_log" >>"$INSTALL_LOG" 2>/dev/null || true
}

install_or_update_whatsapp() {
  need_root
  box_header
  line
  echo -e "${WHITE}Instalar/Atualizar WhatsApp${NC}"
  line
  ensure_config
  ensure_all_dependencies
  set_env_value WHATSAPP_AUTH_DIR "$DATA_DIR/whatsapp-auth"
  chmod 600 "$CONFIG_FILE" 2>/dev/null || true
  mkdir -p "$DATA_DIR/whatsapp-auth" "$APP_DIR/whatsapp"
  install_binary
  install_project_files
  command -v node >/dev/null 2>&1 || fail "Node.js não encontrado após instalação de dependências."
  command -v npm >/dev/null 2>&1 || fail "npm não encontrado após instalação de dependências."
  if [[ -f "$APP_DIR/whatsapp/package.json" ]]; then
    progress_bar "Instalando dependências WhatsApp"
    (cd "$APP_DIR/whatsapp" && rm -rf node_modules package-lock.json && NPM_CONFIG_FUND=false NPM_CONFIG_AUDIT=false run_quiet npm install --omit=dev --silent --no-audit --no-fund) || fail "Falha ao instalar dependências do WhatsApp. Log: $INSTALL_LOG"
    (cd "$APP_DIR/whatsapp" && run_quiet node -e "require('@whiskeysockets/baileys'); require('pino'); require('qrcode-terminal');") || fail "Dependências do WhatsApp não carregaram. Log: $INSTALL_LOG"
  else
    fail "Arquivo $APP_DIR/whatsapp/package.json não encontrado."
  fi
  write_service_whatsapp
  reload_systemd
  run_whatsapp_installer_sync
  if ! start_service_checked primecel-whatsapp; then
    fail "WhatsApp instalado, mas o serviço não iniciou. Veja: journalctl -u primecel-whatsapp -n 120 --no-pager"
  fi
  ok "WhatsApp instalado/atualizado."
  echo "Status: systemctl status primecel-whatsapp --no-pager"
  pause
}

remove_whatsapp() {
  need_root
  box_header
  line
  echo -e "${WHITE}Remover WhatsApp${NC}"
  line
  read -r -p "Remover serviço WhatsApp? [s/N]: " RESP || true
  RESP="${RESP:-N}"
  if [[ ! "$RESP" =~ ^[sS]$ ]]; then warn "Remoção cancelada."; pause; return 0; fi
  stop_disable_remove primecel-whatsapp
  reload_systemd
  ok "WhatsApp removido. Sessão preservada em $DATA_DIR/whatsapp-auth."
  pause
}

show_secondary_servers() {
  if [[ -x "$BIN_PATH" ]]; then
    CONFIG_ENV="$CONFIG_FILE" "$BIN_PATH" servers list 2>/dev/null | sed 's/[{}\[\]",]//g' | head -40 || true
  else
    echo "Nenhum servidor listado."
  fi
  echo
}

add_secondary_server() {
  need_root
  ensure_config
  install_binary
  local name host port token sshp sshu sshpass
  name=$(ask "Nome do servidor" "Sv")
  host=$(ask "IP/Host da VPS secundária")
  [[ -z "$host" ]] && { warn "IP obrigatório."; pause; return 0; }
  port=$(ask "Porta do agente" "$(get_env_value REMOTE_AGENT_PORT || echo $DEFAULT_AGENT_PORT)")
  token=$(ask "Token do agente" "$(get_env_value REMOTE_AGENT_TOKEN || true)")
  sshp=$(ask "Porta SSH" "22")
  sshu=$(ask "Usuário SSH" "root")
  sshpass=$(ask "Senha SSH/chave cadastrada" "")
  run_bin servers add --name "$name" --host "$host" --agent-port "$port" --token "$token" --ssh-port "$sshp" --ssh-user "$sshu" --ssh-password "$sshpass"
  ok "Servidor adicionado."
  pause
}

edit_secondary_server() {
  need_root
  ensure_config
  install_binary
  local id name host port token sshp sshu sshpass
  show_secondary_servers
  id=$(ask "ID do servidor para editar")
  [[ -z "$id" ]] && return 0
  name=$(ask "Novo nome" "")
  host=$(ask "Novo IP/Host" "")
  port=$(ask "Nova porta do agente" "")
  token=$(ask "Novo token do agente" "")
  sshp=$(ask "Nova porta SSH" "")
  sshu=$(ask "Novo usuário SSH" "")
  sshpass=$(ask "Nova senha SSH/chave" "")
  local args=(servers update --id "$id")
  [[ -n "$name" ]] && args+=(--name "$name")
  [[ -n "$host" ]] && args+=(--host "$host")
  [[ -n "$port" ]] && args+=(--agent-port "$port")
  [[ -n "$token" ]] && args+=(--token "$token")
  [[ -n "$sshp" ]] && args+=(--ssh-port "$sshp")
  [[ -n "$sshu" ]] && args+=(--ssh-user "$sshu")
  [[ -n "$sshpass" ]] && args+=(--ssh-password "$sshpass")
  run_bin "${args[@]}"
  ok "Servidor atualizado."
  pause
}

remove_secondary_server() {
  need_root
  ensure_config
  install_binary
  local id resp
  show_secondary_servers
  id=$(ask "ID do servidor para remover")
  [[ -z "$id" ]] && return 0
  read -r -p "Remover servidor ID $id? [s/N]: " resp || true
  [[ "$resp" =~ ^[sS]$ ]] || { warn "Cancelado."; pause; return 0; }
  run_bin servers remove --id "$id"
  ok "Servidor removido."
  pause
}

sync_secondary_servers() {
  need_root
  ensure_config
  install_binary
  progress_bar "Sincronizando secundários"
  run_bin sync state
  pause
}

update_cloudflare_token_manual() {
  need_root
  ensure_config
  load_saved_cloudflare_token
  box_header
  line
  echo -e "${WHITE}Cloudflare${NC}"
  line
  echo "Informe o novo token da Cloudflare."
  echo "Permissões recomendadas: Zone/Read e DNS/Edit. Para CheckUser com proxy/origin rule, Rulesets/Edit também."
  echo
  local token
  token=$(ask "Novo Token Cloudflare" "${CLOUDFLARE_API_TOKEN:-}")
  token="$(printf '%s' "$token" | tr -d '\r\n' | sed 's/^ *//;s/ *$//')"
  if [[ -z "$token" ]]; then
    warn "Token vazio. Nada foi alterado."
    pause
    return 0
  fi
  save_cloudflare_token_internal "$token"
  set_env_value CLOUDFLARE_API_TOKEN "$token"
  set_env_value VPN_DNS_DOMAIN ""
  set_env_value VPN_DNS_ENABLED "0"
  ok "Novo token Cloudflare salvo."
  pause
}

servers_menu() {
  while true; do
    box_header
    line
    echo -e "${WHITE}Servidores secundários${NC}"
    line
    echo
    show_secondary_servers
    echo -e "  ${CYAN}1.${NC} Adicionar servidor"
    echo -e "  ${CYAN}2.${NC} Editar servidor"
    echo -e "  ${CYAN}3.${NC} Remover servidor"
    echo -e "  ${CYAN}4.${NC} Sincronizar todos os secundários"
    echo -e "  ${CYAN}5.${NC} Atualizar token Cloudflare"
    echo -e "  ${CYAN}6.${NC} Promover VPS para Principal"
    echo -e "  ${CYAN}0.${NC} Voltar"
    line
    echo
    read -r -p "Opção: " OPCAO_SERVER || return 0
    case "$OPCAO_SERVER" in
      1) add_secondary_server ;;
      2) edit_secondary_server ;;
      3) remove_secondary_server ;;
      4) sync_secondary_servers ;;
      5) update_cloudflare_token_manual ;;
      6) promote_to_principal_menu ;;
      0|voltar|Voltar|VOLTAR) return 0 ;;
      *) warn "Opção inválida."; sleep 1 ;;
    esac
  done
}

promote_to_principal_menu() {
  need_root
  box_header
  line
  echo -e "${WHITE}Promover VPS para Principal${NC}"
  line
  echo "Essa opção deixa esta VPS como principal/gestora e desativa o modo Gestor Only."
  echo
  read -r -p "Promover esta VPS para Principal? [s/N]: " RESP || true
  RESP="${RESP:-N}"
  if [[ ! "$RESP" =~ ^[sS]$ ]]; then warn "Operação cancelada."; pause; return 0; fi
  ensure_config
  set_env_value PRINCIPAL_MANAGER_ONLY 0
  install_binary
  write_service_bot
  reload_systemd
  start_service_checked primecel-gestor || true
  ok "VPS configurada como Principal."
  pause
}

checkuser_installer_path() {
  if [[ -f "$CHECKUSER_PACKAGE_DIR/install.sh" ]]; then
    printf '%s' "$CHECKUSER_PACKAGE_DIR/install.sh"
    return 0
  fi
  if [[ -f "$CHECKUSER_INSTALLER_STORE/install.sh" ]]; then
    printf '%s' "$CHECKUSER_INSTALLER_STORE/install.sh"
    return 0
  fi
  if [[ -f "$APP_DIR/checkuser_installer/install.sh" ]]; then
    printf '%s' "$APP_DIR/checkuser_installer/install.sh"
    return 0
  fi
  return 1
}

checkuser_status_text() {
  if systemctl is-active --quiet checkuser 2>/dev/null || systemctl is-active --quiet checkuser.service 2>/dev/null; then
    echo "✅ ativo"
  elif [[ -x /usr/local/bin/checkuser || -f /etc/systemd/system/checkuser.service ]]; then
    echo "⚠️ parado"
  elif systemctl is-active --quiet primecel-checkuser.service 2>/dev/null; then
    echo "✅ ativo"
  elif [[ -f /etc/systemd/system/primecel-checkuser.service ]]; then
    echo "⚠️ legado/parado"
  else
    echo "— não instalado"
  fi
}

install_checkuser_from_option1() {
  CHECKUSER_AUTO_CLOUDFLARE=1 CHECKUSER_NO_PAUSE=1 run_checkuser_installer_action install
}

run_checkuser_installer_action() {
  local action="$1"
  local installer=""
  need_root
  ensure_config
  install_project_files >/dev/null 2>&1 || true
  if ! installer="$(checkuser_installer_path)"; then
    fail "Instalador completo do CheckUser não encontrado no pacote."
  fi
  chmod +x "$installer" 2>/dev/null || true
  load_saved_cloudflare_token
  TOKEN_FILE="$TOKEN_FILE" CONFIG_FILE="$CONFIG_FILE" DB_FILE="${DB_FILE:-/etc/primecel-gestor/gestor.db}" CHECKUSER_GITHUB_TOKEN="${CHECKUSER_GITHUB_TOKEN:-}" CLOUDFLARE_API_TOKEN="${CLOUDFLARE_API_TOKEN:-}" CHECKUSER_AUTO_CLOUDFLARE="${CHECKUSER_AUTO_CLOUDFLARE:-0}" CHECKUSER_NO_PAUSE="${CHECKUSER_NO_PAUSE:-0}" bash "$installer" "$action"
}

install_or_update_checkuser_menu() {
  while true; do
    box_header
    line
    echo -e "${WHITE}CheckUser${NC}"
    line
    echo "Status: $(checkuser_status_text)"
    echo
    echo -e "  ${CYAN}1.${NC} Instalar/Atualizar"
    echo -e "  ${CYAN}2.${NC} Remover"
    echo -e "  ${CYAN}3.${NC} Status"
    echo -e "  ${CYAN}0.${NC} Voltar"
    line
    echo
    read -r -p "Opção: " op || return 0
    case "$op" in
      1) run_checkuser_installer_action install ; pause ;;
      2) run_checkuser_installer_action remove ;;
      3) run_checkuser_installer_action status ;;
      0|voltar|Voltar|VOLTAR) return 0 ;;
      *) warn "Opção inválida."; sleep 1 ;;
    esac
  done
}

clean_primecel_cache() {
  find "$APP_DIR" -type d -name '__pycache__' -prune -exec rm -rf {} + 2>/dev/null || true
  find "$APP_DIR" -type f \( -name '*.pyc' -o -name '*.pyo' -o -name '*.bak' \) -delete 2>/dev/null || true
  rm -rf /tmp/primecel-gestor-* /tmp/primecel_* /tmp/go-build* 2>/dev/null || true
}

clean_cache_menu() {
  need_root
  box_header
  line
  echo -e "${WHITE}Limpar cache${NC}"
  line
  echo "Esta opção remove apenas caches e arquivos temporários."
  echo "Não remove contas, backups, configurações, bancos ou servidores."
  echo
  read -r -p "Limpar cache? [S/n]: " RESP || true
  RESP="${RESP:-S}"
  if [[ ! "$RESP" =~ ^[sS]$ ]]; then warn "Limpeza cancelada."; pause; return 0; fi
  progress_bar "Limpando cache"
  clean_primecel_cache
  ok "Cache limpo com segurança."
  pause
}

toggle_whatsapp_bot() {
  need_root
  ensure_config
  if systemctl is-active --quiet primecel-whatsapp.service 2>/dev/null; then
    systemctl stop primecel-whatsapp.service 2>/dev/null || true
    ok "WhatsApp desativado."
  elif systemctl is-failed --quiet primecel-whatsapp.service 2>/dev/null; then
    warn "WhatsApp em erro. Reinstalando..."
    install_or_update_whatsapp
    return 0
  elif systemd_has_unit primecel-whatsapp; then
    start_service_checked primecel-whatsapp || true
    ok "WhatsApp ativado."
  else
    install_or_update_whatsapp
    return 0
  fi
  pause
}

restart_general_bots() {
  need_root
  ensure_config
  if systemd_has_unit primecel-gestor; then
    systemctl restart primecel-gestor.service 2>/dev/null || true
  fi
  if systemd_has_unit primecel-webhook; then
    systemctl restart primecel-webhook.service 2>/dev/null || true
  fi
  if systemd_has_unit primecel-whatsapp; then
    systemctl restart primecel-whatsapp.service 2>/dev/null || true
  fi
  ok "Bot Telegram/geral, API de renovação e WhatsApp reiniciados."
  pause
}

toggle_general_bots() {
  need_root
  ensure_config
  if systemctl is-active --quiet primecel-gestor.service 2>/dev/null || systemctl is-active --quiet primecel-whatsapp.service 2>/dev/null; then
    systemctl stop primecel-whatsapp.service primecel-gestor.service 2>/dev/null || true
    ok "Bot geral parado."
  else
    if systemd_has_unit primecel-gestor; then
      start_service_checked primecel-gestor || true
    fi
    if systemd_has_unit primecel-webhook; then
      start_service_checked primecel-webhook || true
    fi
    if systemd_has_unit primecel-whatsapp; then
      start_service_checked primecel-whatsapp || true
    fi
    ok "Bot geral iniciado."
  fi
  pause
}

payments_menu() {
  need_root
  ensure_config
  while true; do
    box_header
    show_status_short
    line
    echo -e "${WHITE}Pagamentos${NC}"
    line
    echo -e "  ${CYAN}01.${NC} Configurar API Pix"
    line
    echo -e "  ${CYAN}00.${NC} Voltar ao Menu"
    line
    echo
    read -r -p "Opção: " OPCAO_PAY || return 0
    case "$OPCAO_PAY" in
      1|01)
        local resp
        resp="$(ask_yes_no_strict "> Configurar API Pix?")"
        if [[ "$resp" == "s" ]]; then
          progress_bar "Configurando API Pix"
          install_pix_api_from_option1 || { warn "API Pix não configurada. Veja o log: $INSTALL_LOG"; pause; }
          ok "API Pix configurada."
          pause
        else
          warn "Configuração cancelada."
          pause
        fi
        ;;
      0|00|voltar|Voltar|VOLTAR) return 0 ;;
      *) warn "Opção inválida."; sleep 1 ;;
    esac
  done
}

manage_bot_menu() {
  need_root
  ensure_config
  while true; do
    box_header
    show_status_manage
    line
    echo -e "  ${CYAN}01.${NC} Ativar / Desativar Bot Whatsapp"
    echo -e "  ${CYAN}02.${NC} Reiniciar Bot Geral"
    echo -e "  ${CYAN}03.${NC} Iniciar / Parar Bot Geral"
    line
    echo -e "  ${CYAN}00.${NC} Voltar ao Menu"
    line
    echo
    read -r -p "Opção: " OPCAO_GER || return 0
    case "$OPCAO_GER" in
      1|01) toggle_whatsapp_bot ;;
      2|02) restart_general_bots ;;
      3|03) toggle_general_bots ;;
      0|00|voltar|Voltar|VOLTAR) return 0 ;;
      *) warn "Opção inválida."; sleep 1 ;;
    esac
  done
}

main_menu() {
  need_root
  ensure_config
  while true; do
    box_header
    show_status_short
    line
    echo -e "  ${CYAN}01.${NC} $(install_bot_label)"
    echo -e "  ${CYAN}02.${NC} Gerenciar Bot"
    line
    echo -e "  ${CYAN}03.${NC} Servidores"
    line
    echo -e "  ${CYAN}04.${NC} Importar Backup ${GRAY}(/root/backup-painel.tar.gz)${NC}"
    line
    echo -e "  ${CYAN}05.${NC} CheckUser"
    echo -e "  ${CYAN}06.${NC} Alterar Bot"
    echo -e "  ${CYAN}07.${NC} Atualizar CloudFlare"
    echo -e "  ${CYAN}08.${NC} Pagamentos"
    line
    echo -e "  ${CYAN}0.${NC} Sair"
    line
    echo
    read -r -p "Opção: " OPCAO || exit 0
    case "$OPCAO" in
      1|01) install_or_update ;;
      2|02) manage_bot_menu ;;
      3|03) servers_menu ;;
      4|04) restore_backup ;;
      5|05) install_or_update_checkuser_menu ;;
      6|06) alter_bot_config ;;
      7|07) update_cloudflare_token_manual ;;
      8|08) payments_menu ;;
      0) exit 0 ;;
      *) warn "Opção inválida."; sleep 1 ;;
    esac
  done
}

case "${1:-menu}" in
  menu) main_menu ;;
  install|update) install_or_update ;;
  configure|alterar-bot) alter_bot_config ;;
  remove-bot) remove_bot ;;
  whatsapp) install_or_update_whatsapp ;;
  remove-whatsapp) remove_whatsapp ;;
  servers) servers_menu ;;
  agent)
    need_root; ensure_config; install_binary
    port=$(ask "Porta do agente" "$(get_env_value REMOTE_AGENT_PORT || echo $DEFAULT_AGENT_PORT)")
    token=$(get_env_value REMOTE_AGENT_TOKEN || true); [[ -z "$token" ]] && token=$(openssl rand -hex 24 2>/dev/null || date +%s%N)
    set_env_value REMOTE_AGENT_PORT "$port"; set_env_value REMOTE_AGENT_TOKEN "$token"
    write_service_agent; reload_systemd; enable_restart primecel-gestor-agent.service; ok "Agente instalado na porta $port."; echo "Token: $token" ;;
  checkuser) install_or_update_checkuser_menu ;;
  import-backup) restore_backup ;;
  backup) create_backup ;;
  status) show_status_short ;;
  restart) systemctl restart primecel-gestor.service 2>/dev/null || true; ok "Bot reiniciado." ;;
  clean-cache) clean_cache_menu ;;
  *) main_menu ;;
esac
