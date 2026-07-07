#!/usr/bin/env bash
set -euo pipefail
DATA_DIR="/etc/primecel-gestor"
APP_DIR="/opt/primecel-gestor"

need_root() {
  if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
    echo "Execute como root." >&2
    exit 1
  fi
}

stop_disable_remove() {
  local svc="$1"
  systemctl stop "${svc}.service" 2>/dev/null || true
  systemctl disable "${svc}.service" 2>/dev/null || true
  rm -f "/etc/systemd/system/${svc}.service"
}

need_root
cat <<MSG
━━━━━━━━━━━━━━━━━━━━━━━━━━━━
 REMOVER PRIMECEL GESTOR
━━━━━━━━━━━━━━━━━━━━━━━━━━━━
1. Remover somente serviços/binário e preservar dados
2. Remover tudo, incluindo /etc/primecel-gestor
0. Cancelar
MSG
read -r -p "Opção: " opt
case "$opt" in
  1)
    for svc in primecel-gestor primecel-gestor-agent primecel-checkuser primecel-whatsapp primecel-webhook; do
      stop_disable_remove "$svc"
    done
    rm -rf "$APP_DIR"
    systemctl daemon-reload || true
    echo "Serviços/binário removidos. Dados preservados em $DATA_DIR."
    ;;
  2)
    read -r -p "Digite REMOVER para confirmar remoção total: " confirm
    if [[ "$confirm" != "REMOVER" ]]; then
      echo "Cancelado."
      exit 0
    fi
    for svc in primecel-gestor primecel-gestor-agent primecel-checkuser primecel-whatsapp primecel-webhook; do
      stop_disable_remove "$svc"
    done
    rm -rf "$APP_DIR" "$DATA_DIR"
    systemctl daemon-reload || true
    echo "Remoção total concluída."
    ;;
  0) echo "Cancelado." ;;
  *) echo "Opção inválida." >&2; exit 1 ;;
esac
