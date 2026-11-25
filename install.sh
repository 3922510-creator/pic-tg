#!/usr/bin/env bash
# Pic TG one-click interactive installer for Linux (systemd + optional Caddy HTTPS)
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/3922510-creator/pic-tg/main/install.sh | sudo bash
#   sudo bash install.sh --upgrade
#   sudo bash install.sh --uninstall

set -Eeuo pipefail

REPO="${PIC_TG_REPO:-3922510-creator/pic-tg}"
BRANCH="${PIC_TG_BRANCH:-main}"
SERVICE_NAME="pic-tg"
SERVICE_USER="pictg"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
CADDY_MAIN_FILE="${PIC_TG_CADDYFILE:-/etc/caddy/Caddyfile}"
CADDY_CONF_DIR="${PIC_TG_CADDY_CONF_DIR:-/etc/caddy/conf.d}"
CADDY_SITE_FILE="${CADDY_CONF_DIR}/pic-tg.caddy"

ACTION="install"
VERSION="latest"
FROM_SOURCE="no"
NON_INTERACTIVE="no"
DOMAIN=""
PORT="8798"
INSTALL_DIR="/opt/pic-tg"
USE_CLOUDFLARE=""
USE_CADDY=""
INSTALL_OPTIMIZERS=""
AUTH_PASSWORD=""
ALLOW_FIREWALL=""

TMP_DIR=""
CREATED_CONFIG="no"
GENERATED_PASSWORD=""
GENERATED_API_KEY=""

log()  { printf '[Pic TG] %s\n' "$*"; }
warn() { printf '[Pic TG][WARN] %s\n' "$*" >&2; }
err()  { printf '[Pic TG][ERROR] %s\n' "$*" >&2; }
die()  { err "$*"; exit 1; }

cleanup() {
  if [[ -n "${TMP_DIR:-}" && -d "${TMP_DIR}" ]]; then
    rm -rf "${TMP_DIR}"
  fi
}
trap cleanup EXIT

usage() {
  cat <<'EOF'
Pic TG Installer

Usage:
  sudo bash install.sh [options]

Actions:
  --install                 Interactive install (default)
  --upgrade                 Upgrade binary in-place without touching config/data
  --uninstall               Remove systemd service and Caddy site config

Options:
  --domain DOMAIN           Public domain name, e.g. pic.example.com
  --port PORT               Local listen port (default: 8798)
  --dir PATH                Install directory (default: /opt/pic-tg)
  --password SECRET         Initial Web login password (auto-generated if empty)
  --cloudflare yes|no       Behind Cloudflare proxy (sets CF-Connecting-IP)
  --caddy yes|no            Install and configure Caddy automatic HTTPS
  --optimizers yes|no       Install optional jpegtran and cwebp packages
  --version TAG             Release tag to install (default: latest)
  --from-source             Build from source instead of downloading release binary
  --yes                     Non-interactive mode (use supplied flags / defaults)
  -h, --help                Show this help
EOF
}

normalize_bool() {
  case "${1,,}" in
    y|yes|true|1) printf 'yes' ;;
    n|no|false|0) printf 'no' ;;
    *) return 1 ;;
  esac
}

sanitize_domain() {
  local raw="${1:-}"
  raw="${raw#http://}"
  raw="${raw#https://}"
  raw="${raw%%/*}"
  raw="${raw%%:*}"
  raw="${raw#.}"
  raw="${raw%.}"
  printf '%s' "${raw,,}"
}

validate_domain() {
  local d="$1"
  [[ -n "$d" && ${#d} -le 253 ]] || return 1
  [[ "$d" =~ ^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$ ]]
}

validate_port() {
  local p="$1"
  [[ "$p" =~ ^[0-9]+$ ]] || return 1
  (( p >= 1 && p <= 65535 ))
}

validate_install_dir() {
  local dir="$1"
  [[ "$dir" == /* ]] || return 1
  dir="${dir%/}"
  case "$dir" in
    ""|"/"|"/bin"|"/boot"|"/dev"|"/etc"|"/home"|"/lib"|"/lib64"|"/opt"|"/proc"|"/root"|"/run"|"/sbin"|"/srv"|"/sys"|"/tmp"|"/usr"|"/var")
      return 1
      ;;
  esac
  [[ ! "$dir" =~ [[:space:]] ]]
}

json_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  s="${s//$'\n'/\\n}"
  s="${s//$'\r'/\\r}"
  s="${s//$'\t'/\\t}"
  printf '%s' "$s"
}

generate_secret() {
  local bytes="${1:-16}"
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex "$bytes"
    return 0
  fi
  od -An -tx1 -N "$bytes" /dev/urandom | tr -d ' \n'
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --install) ACTION="install" ;;
      --upgrade) ACTION="upgrade" ;;
      --uninstall) ACTION="uninstall" ;;
      --domain) shift; [[ $# -gt 0 ]] || die "--domain requires a value"; DOMAIN="$1" ;;
      --port) shift; [[ $# -gt 0 ]] || die "--port requires a value"; PORT="$1" ;;
      --dir) shift; [[ $# -gt 0 ]] || die "--dir requires a value"; INSTALL_DIR="$1" ;;
      --password) shift; [[ $# -gt 0 ]] || die "--password requires a value"; AUTH_PASSWORD="$1" ;;
      --cloudflare) shift; [[ $# -gt 0 ]] || die "--cloudflare requires yes|no"; USE_CLOUDFLARE="$(normalize_bool "$1")" || die "invalid --cloudflare value" ;;
      --caddy) shift; [[ $# -gt 0 ]] || die "--caddy requires yes|no"; USE_CADDY="$(normalize_bool "$1")" || die "invalid --caddy value" ;;
      --optimizers) shift; [[ $# -gt 0 ]] || die "--optimizers requires yes|no"; INSTALL_OPTIMIZERS="$(normalize_bool "$1")" || die "invalid --optimizers value" ;;
      --version) shift; [[ $# -gt 0 ]] || die "--version requires a value"; VERSION="$1" ;;
      --from-source) FROM_SOURCE="yes" ;;
      --yes|-y) NON_INTERACTIVE="yes" ;;
      -h|--help) usage; exit 0 ;;
      *) die "unknown option: $1 (use --help for usage)" ;;
    esac
    shift
  done
}

tty_available() {
  [[ -r /dev/tty && -w /dev/tty ]]
}

prompt_line() {
  local prompt="$1"
  local default_val="${2:-}"
  local reply=""
  if [[ "$NON_INTERACTIVE" == "yes" ]]; then
    printf '%s' "$default_val"
    return 0
  fi
  if ! tty_available; then
    printf '%s' "$default_val"
    return 0
  fi
  if [[ -n "$default_val" ]]; then
    printf '%s [%s]: ' "$prompt" "$default_val" >/dev/tty
  else
    printf '%s: ' "$prompt" >/dev/tty
  fi
  IFS= read -r reply </dev/tty || true
  if [[ -z "$reply" ]]; then
    reply="$default_val"
  fi
  printf '%s' "$reply"
}

prompt_secret() {
  local prompt="$1"
  local reply=""
  if [[ "$NON_INTERACTIVE" == "yes" ]] || ! tty_available; then
    printf ''
    return 0
  fi
  printf '%s: ' "$prompt" >/dev/tty
  IFS= read -rs reply </dev/tty || true
  printf '\n' >/dev/tty
  printf '%s' "$reply"
}

prompt_yes_no() {
  local prompt="$1"
  local default_val="${2:-no}"
  local suffix="[y/N]"
  [[ "$default_val" == "yes" ]] && suffix="[Y/n]"
  if [[ "$NON_INTERACTIVE" == "yes" ]] || ! tty_available; then
    printf '%s' "$default_val"
    return 0
  fi
  while true; do
    local ans=""
    printf '%s %s: ' "$prompt" "$suffix" >/dev/tty
    IFS= read -r ans </dev/tty || true
    if [[ -z "$ans" ]]; then
      printf '%s' "$default_val"
      return 0
    fi
    if norm="$(normalize_bool "$ans")"; then
      printf '%s' "$norm"
      return 0
    fi
    printf '请输入 y 或 n。\n' >/dev/tty
  done
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'amd64' ;;
    aarch64|arm64) printf 'arm64' ;;
    *) die "unsupported CPU architecture: $(uname -m)" ;;
  esac
}

detect_pkg_mgr() {
  if command -v apt-get >/dev/null 2>&1; then
    printf 'apt'
  elif command -v dnf >/dev/null 2>&1; then
    printf 'dnf'
  elif command -v yum >/dev/null 2>&1; then
    printf 'yum'
  else
    printf 'unknown'
  fi
}

require_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    die "please run as root (e.g. curl ... | sudo bash)"
  fi
}

require_linux_systemd() {
  [[ "$(uname -s)" == "Linux" ]] || die "this installer only supports Linux"
  command -v systemctl >/dev/null 2>&1 || die "systemd is required (systemctl not found)"
  [[ -d /run/systemd/system ]] || die "systemd does not appear to be active on this host"
}

check_port_available() {
  local port="$1"
  if command -v ss >/dev/null 2>&1; then
    local listeners
    listeners="$(ss -tlnp "sport = :${port}" 2>/dev/null | tail -n +2 || true)"
    if [[ -n "$listeners" ]] && ! grep -q '"pic-tg"' <<<"$listeners"; then
      warn "端口 ${port} 当前已被其他进程占用:"
      printf '%s\n' "$listeners" >&2
      return 1
    fi
  fi
  return 0
}

check_dns_hint() {
  local domain="$1"
  local resolved=""
  if command -v getent >/dev/null 2>&1; then
    resolved="$(getent ahosts "$domain" 2>/dev/null | awk '{print $1}' | sort -u | tr '\n' ' ' | sed 's/ $//' || true)"
  fi
  if [[ -z "$resolved" ]]; then
    warn "当前尚未检测到域名 ${domain} 的 DNS 解析记录。"
    warn "请确认已将 ${domain} 的 A/AAAA 记录指向本机公网 IP，否则 Caddy 自动申请 HTTPS 证书将失败。"
    return 0
  fi
  local pub_ip=""
  pub_ip="$(curl -fsS --max-time 4 https://api.ipify.org 2>/dev/null || true)"
  if [[ -n "$pub_ip" && " $resolved " != *" $pub_ip "* ]]; then
    log "域名 ${domain} 解析到: ${resolved}（本机 IPv4: ${pub_ip}）"
    log "如果已开启 Cloudflare 代理（橙云）属正常情况；否则请确认 DNS 指向本机。"
  else
    log "DNS 预检通过: ${domain} -> ${resolved}"
  fi
}

install_base_deps() {
  local pm
  pm="$(detect_pkg_mgr)"
  local need=()
  command -v curl >/dev/null 2>&1 || need+=(curl)
  command -v tar >/dev/null 2>&1 || need+=(tar)
  command -v openssl >/dev/null 2>&1 || need+=(openssl)
  [[ ${#need[@]} -eq 0 ]] && return 0

  log "安装基础依赖: ${need[*]}"
  case "$pm" in
    apt)
      DEBIAN_FRONTEND=noninteractive apt-get update -qq
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ca-certificates "${need[@]}"
      ;;
    dnf)
      dnf install -y -q ca-certificates "${need[@]}"
      ;;
    yum)
      yum install -y -q ca-certificates "${need[@]}"
      ;;
    *)
      die "missing required commands (${need[*]}) and no supported package manager found"
      ;;
  esac
}

install_image_optimizers() {
  local pm
  pm="$(detect_pkg_mgr)"
  log "安装可选无损压缩工具 (jpegtran / cwebp)..."
  case "$pm" in
    apt)
      DEBIAN_FRONTEND=noninteractive apt-get update -qq
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq libjpeg-turbo-progs webp || warn "无损工具安装失败，Pic TG 将自动回退为保留原图"
      ;;
    dnf)
      dnf install -y -q libjpeg-turbo-utils libwebp-tools || warn "无损工具安装失败，Pic TG 将自动回退为保留原图"
      ;;
    yum)
      yum install -y -q libjpeg-turbo-utils libwebp-tools || warn "无损工具安装失败，Pic TG 将自动回退为保留原图"
      ;;
    *)
      warn "未识别包管理器，跳过安装 jpegtran / cwebp（不影响正常运行）"
      ;;
  esac
}

verify_sha256() {
  local file="$1"
  local sums_file="$2"
  local asset_name="$3"
  local expected actual
  expected="$(awk -v name="$asset_name" '$2 == name || $2 == ("*" name) {print $1; exit}' "$sums_file")"
  [[ -n "$expected" ]] || die "SHA256SUMS missing entry for ${asset_name}"
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$file" | awk '{print $1}')"
  elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$file" | awk '{print $1}')"
  else
    die "sha256sum or shasum is required to verify download integrity"
  fi
  [[ "$actual" == "$expected" ]] || die "SHA256 checksum mismatch for ${asset_name} (got ${actual}, want ${expected})"
}

download_release_binary() {
  local arch="$1"
  local dest="$2"
  local asset="pic-tg-linux-${arch}"
  local base_url
  if [[ "$VERSION" == "latest" ]]; then
    base_url="https://github.com/${REPO}/releases/latest/download"
  else
    base_url="https://github.com/${REPO}/releases/download/${VERSION}"
  fi

  log "尝试下载预编译二进制: ${base_url}/${asset}"
  if ! curl -fsSL --retry 2 --connect-timeout 15 -o "${TMP_DIR}/${asset}" "${base_url}/${asset}"; then
    return 1
  fi
  if ! curl -fsSL --retry 2 --connect-timeout 15 -o "${TMP_DIR}/SHA256SUMS" "${base_url}/SHA256SUMS"; then
    warn "未获取到 SHA256SUMS，放弃使用未校验二进制"
    return 1
  fi
  verify_sha256 "${TMP_DIR}/${asset}" "${TMP_DIR}/SHA256SUMS" "${asset}"
  install -m 0755 "${TMP_DIR}/${asset}" "$dest"
  log "SHA256 校验通过，已获取预编译二进制"
  return 0
}

go_version_ok() {
  command -v go >/dev/null 2>&1 || return 1
  local v major minor
  v="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
  major="${v%%.*}"
  minor="${v#*.}"
  minor="${minor%%.*}"
  [[ "$major" =~ ^[0-9]+$ && "$minor" =~ ^[0-9]+$ ]] || return 1
  (( major > 1 || (major == 1 && minor >= 26) ))
}

build_from_source() {
  local arch="$1"
  local dest="$2"
  local go_bin="go"

  if ! go_version_ok; then
    local go_ver="1.26.0"
    local go_tar="go${go_ver}.linux-${arch}.tar.gz"
    log "系统未检测到 Go 1.26+，正在临时目录下载 Go ${go_ver} 用于编译（编译完成后自动清除）..."
    curl -fsSL --retry 2 --connect-timeout 15 -o "${TMP_DIR}/${go_tar}" "https://go.dev/dl/${go_tar}" \
      || die "无法下载 Go 工具链，请先手动安装 Go 1.26+ 或指定已发布的 Release 版本"
    tar -C "${TMP_DIR}" -xzf "${TMP_DIR}/${go_tar}"
    go_bin="${TMP_DIR}/go/bin/go"
  fi

  local ref="${BRANCH}"
  [[ "$VERSION" != "latest" ]] && ref="${VERSION}"
  local src_tar="${TMP_DIR}/src.tar.gz"
  log "正在从 GitHub 下载源码 (${REPO}@${ref}) 并编译..."
  curl -fsSL --retry 2 --connect-timeout 15 -o "$src_tar" "https://github.com/${REPO}/archive/${ref}.tar.gz" \
    || die "下载源码失败: https://github.com/${REPO}/archive/${ref}.tar.gz"
  mkdir -p "${TMP_DIR}/src"
  tar -C "${TMP_DIR}/src" --strip-components=1 -xzf "$src_tar"

  (
    cd "${TMP_DIR}/src"
    CGO_ENABLED=0 "$go_bin" build -trimpath -ldflags="-s -w" -o "$dest" .
  )
  chmod 0755 "$dest"
  log "源码编译完成"
}

acquire_binary() {
  local arch="$1"
  local dest="$2"
  if [[ "$FROM_SOURCE" == "yes" ]]; then
    build_from_source "$arch" "$dest"
    return 0
  fi
  if download_release_binary "$arch" "$dest"; then
    return 0
  fi
  warn "预编译 Release 下载不可用，自动回退为源码编译..."
  build_from_source "$arch" "$dest"
}

ensure_service_user() {
  if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
    local nologin="/usr/sbin/nologin"
    [[ -x "$nologin" ]] || nologin="/sbin/nologin"
    [[ -x "$nologin" ]] || nologin="/bin/false"
    useradd --system --no-create-home --shell "$nologin" "$SERVICE_USER"
    log "已创建系统运行用户: ${SERVICE_USER}"
  fi
}

write_config_json() {
  local cfg_path="$1"
  local port="$2"
  local public_origin="$3"
  local state_dir="$4"
  local auth_token="$5"
  local api_key="$6"
  local trust_header="$7"

  local tmp="${cfg_path}.tmp.$$"
  (
    umask 077
    cat >"$tmp" <<EOF
{
  "port": ${port},
  "host": "127.0.0.1",
  "public_origin": "$(json_escape "$public_origin")",
  "allowed_origins": [],
  "trust_proxy_header": "$(json_escape "$trust_header")",
  "state_dir": "$(json_escape "$state_dir")",
  "auth_token": "$(json_escape "$auth_token")",
  "api_key": "$(json_escape "$api_key")",
  "default_connection_id": "",
  "telegram_connections": []
}
EOF
  )
  chmod 0600 "$tmp"
  mv -f "$tmp" "$cfg_path"
}

write_systemd_unit() {
  local dir="$1"
  cat >"$SERVICE_FILE" <<EOF
[Unit]
Description=Pic TG Image Hosting Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
WorkingDirectory=${dir}
ExecStart=${dir}/pic-tg -config ${dir}/config.json
Restart=on-failure
RestartSec=3
LimitNOFILE=65536
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=${dir}

[Install]
WantedBy=multi-user.target
EOF
  chmod 0644 "$SERVICE_FILE"
}

render_caddy_site() {
  local domain="$1"
  local port="$2"
  cat <<EOF
${domain} {
	encode zstd gzip

	header {
		X-Content-Type-Options "nosniff"
		X-Frame-Options "DENY"
		Referrer-Policy "strict-origin-when-cross-origin"
		Content-Security-Policy "default-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'"
	}

	reverse_proxy 127.0.0.1:${port} {
		flush_interval -1
	}
}
EOF
}

ensure_caddy_installed() {
  if command -v caddy >/dev/null 2>&1; then
    log "检测到已安装 Caddy: $(caddy version 2>/dev/null | head -n 1 || echo unknown)"
    return 0
  fi
  local pm
  pm="$(detect_pkg_mgr)"
  log "正在安装 Caddy（自动申请并续期 HTTPS 证书）..."
  case "$pm" in
    apt)
      DEBIAN_FRONTEND=noninteractive apt-get update -qq
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https curl gnupg
      curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
      curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' -o /etc/apt/sources.list.d/caddy-stable.list
      DEBIAN_FRONTEND=noninteractive apt-get update -qq
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq caddy
      ;;
    dnf)
      dnf install -y -q 'dnf-command(copr)' || true
      dnf copr enable -y @caddy/caddy
      dnf install -y -q caddy
      ;;
    yum)
      yum install -y -q yum-plugin-copr || true
      yum copr enable -y @caddy/caddy
      yum install -y -q caddy
      ;;
    *)
      die "无法自动安装 Caddy，请先手动安装 Caddy 或使用 --caddy no"
      ;;
  esac
}

ensure_caddy_import_line() {
  local main_file="$1"
  local conf_dir="$2"
  local import_line="import ${conf_dir}/*.caddy"
  local backup_file=""

  mkdir -p "$(dirname "$main_file")" "$conf_dir"
  if [[ ! -f "$main_file" ]]; then
    printf '%s\n' "$import_line" >"$main_file"
    printf ''
    return 0
  fi
  if grep -Fq "$import_line" "$main_file"; then
    printf ''
    return 0
  fi
  backup_file="${main_file}.bak.$(date +%Y%m%d%H%M%S)"
  cp -p "$main_file" "$backup_file"
  printf '\n# Added by Pic TG installer\n%s\n' "$import_line" >>"$main_file"
  printf '%s' "$backup_file"
}

configure_caddy() {
  local domain="$1"
  local port="$2"

  ensure_caddy_installed

  local main_backup=""
  local site_backup=""
  if [[ -f "$CADDY_SITE_FILE" ]]; then
    site_backup="${CADDY_SITE_FILE}.bak.$(date +%Y%m%d%H%M%S)"
    cp -p "$CADDY_SITE_FILE" "$site_backup"
  fi
  main_backup="$(ensure_caddy_import_line "$CADDY_MAIN_FILE" "$CADDY_CONF_DIR")"
  render_caddy_site "$domain" "$port" >"$CADDY_SITE_FILE"

  if ! caddy validate --config "$CADDY_MAIN_FILE" --adapter caddyfile >/dev/null 2>&1; then
    err "Caddy 配置校验失败，正在自动回滚..."
    if [[ -n "$site_backup" ]]; then
      mv -f "$site_backup" "$CADDY_SITE_FILE"
    else
      rm -f "$CADDY_SITE_FILE"
    fi
    if [[ -n "$main_backup" ]]; then
      mv -f "$main_backup" "$CADDY_MAIN_FILE"
    fi
    caddy validate --config "$CADDY_MAIN_FILE" --adapter caddyfile || true
    die "请检查现有 ${CADDY_MAIN_FILE} 是否已存在冲突的 ${domain} 站点配置"
  fi

  [[ -n "$site_backup" ]] && rm -f "$site_backup"
  systemctl enable caddy >/dev/null 2>&1 || true
  if systemctl is-active --quiet caddy; then
    systemctl reload caddy || systemctl restart caddy
  else
    systemctl start caddy
  fi
  log "Caddy 已重载，站点配置位于 ${CADDY_SITE_FILE}"
}

maybe_open_firewall() {
  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -qi "Status: active"; then
    local ans="${ALLOW_FIREWALL:-}"
    [[ -z "$ans" ]] && ans="$(prompt_yes_no "检测到 UFW 防火墙已启用，是否放行 80/tcp 和 443/tcp 以支持 HTTPS？" "yes")"
    if [[ "$ans" == "yes" ]]; then
      ufw allow 80/tcp >/dev/null
      ufw allow 443/tcp >/dev/null
      log "已通过 UFW 放行 80/tcp 与 443/tcp"
    fi
  elif command -v firewall-cmd >/dev/null 2>&1 && systemctl is-active --quiet firewalld; then
    local ans="${ALLOW_FIREWALL:-}"
    [[ -z "$ans" ]] && ans="$(prompt_yes_no "检测到 firewalld 已启用，是否放行 http 和 https 服务？" "yes")"
    if [[ "$ans" == "yes" ]]; then
      firewall-cmd --permanent --add-service=http >/dev/null
      firewall-cmd --permanent --add-service=https >/dev/null
      firewall-cmd --reload >/dev/null
      log "已通过 firewalld 放行 http 与 https"
    fi
  fi
}

wait_for_service() {
  local port="$1"
  local i
  for i in $(seq 1 20); do
    if curl -fsS --max-time 2 "http://127.0.0.1:${port}/api/status?auth_only=1" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  return 1
}

detect_existing_dir() {
  if [[ -f "$SERVICE_FILE" ]]; then
    local wd
    wd="$(sed -n 's/^WorkingDirectory=//p' "$SERVICE_FILE" | head -n 1)"
    if [[ -n "$wd" ]]; then
      printf '%s' "$wd"
      return 0
    fi
  fi
  printf '%s' "$INSTALL_DIR"
}

do_install() {
  require_root
  require_linux_systemd
  local arch
  arch="$(detect_arch)"
  TMP_DIR="$(mktemp -d /tmp/pic-tg-install.XXXXXX)"

  log "Pic TG 交互式安装向导（目标架构: linux/${arch}）"
  echo "------------------------------------------------------------"

  while true; do
    if [[ -z "$DOMAIN" ]]; then
      DOMAIN="$(prompt_line "请输入图床域名（例如 pic.example.com）" "")"
    fi
    DOMAIN="$(sanitize_domain "$DOMAIN")"
    if validate_domain "$DOMAIN"; then
      break
    fi
    if [[ "$NON_INTERACTIVE" == "yes" ]]; then
      die "无效域名: '${DOMAIN}'（请通过 --domain 指定，如 pic.example.com）"
    fi
    warn "域名格式无效，请重新输入（仅需填写域名，无需 https://）"
    DOMAIN=""
  done

  check_dns_hint "$DOMAIN"

  if [[ -z "$USE_CLOUDFLARE" ]]; then
    USE_CLOUDFLARE="$(prompt_yes_no "该域名是否经过 Cloudflare 代理（橙云）？" "no")"
  fi

  if [[ "$NON_INTERACTIVE" != "yes" ]]; then
    while true; do
      local p_in
      p_in="$(prompt_line "本地监听端口（仅监听 127.0.0.1）" "$PORT")"
      if validate_port "$p_in"; then
        PORT="$p_in"
        break
      fi
      warn "端口必须是 1-65535 之间的整数"
    done
  else
    validate_port "$PORT" || die "无效端口: ${PORT}"
  fi
  check_port_available "$PORT" || {
    if [[ "$NON_INTERACTIVE" == "yes" ]]; then
      die "端口 ${PORT} 已被占用"
    fi
    local cont
    cont="$(prompt_yes_no "端口 ${PORT} 已被占用，是否仍要继续？" "no")"
    [[ "$cont" == "yes" ]] || die "已取消安装，请更换端口后重试"
  }

  if [[ "$NON_INTERACTIVE" != "yes" ]]; then
    while true; do
      local d_in
      d_in="$(prompt_line "安装目录" "$INSTALL_DIR")"
      if validate_install_dir "$d_in"; then
        INSTALL_DIR="${d_in%/}"
        break
      fi
      warn "安装目录无效，请使用独立子目录（例如 /opt/pic-tg）"
    done
  else
    validate_install_dir "$INSTALL_DIR" || die "非法安装目录: ${INSTALL_DIR}"
    INSTALL_DIR="${INSTALL_DIR%/}"
  fi

  if [[ ! -f "${INSTALL_DIR}/config.json" && -z "$AUTH_PASSWORD" ]]; then
    AUTH_PASSWORD="$(prompt_secret "请设置 Web 登录密码（直接回车则自动生成强随机密码）")"
  fi

  if [[ -z "$INSTALL_OPTIMIZERS" ]]; then
    INSTALL_OPTIMIZERS="$(prompt_yes_no "是否安装可选无损图片优化工具（jpegtran / cwebp）？" "yes")"
  fi

  if [[ -z "$USE_CADDY" ]]; then
    USE_CADDY="$(prompt_yes_no "是否自动安装并配置 Caddy（自动申请 HTTPS 证书与反代）？" "yes")"
  fi

  local trust_header="X-Forwarded-For"
  if [[ "$USE_CLOUDFLARE" == "yes" ]]; then
    trust_header="CF-Connecting-IP"
  elif [[ "$USE_CADDY" != "yes" ]]; then
    trust_header=""
  fi

  echo "------------------------------------------------------------"
  log "安装确认:"
  log "  访问地址:       https://${DOMAIN}"
  log "  本地监听:       127.0.0.1:${PORT}"
  log "  安装目录:       ${INSTALL_DIR}"
  log "  代理 IP 头:     ${trust_header:-（关闭）}"
  log "  自动配置 Caddy: ${USE_CADDY}"
  log "  安装无损工具:   ${INSTALL_OPTIMIZERS}"
  echo "------------------------------------------------------------"

  if [[ "$NON_INTERACTIVE" != "yes" ]]; then
    local confirm
    confirm="$(prompt_yes_no "确认开始安装？" "yes")"
    [[ "$confirm" == "yes" ]] || die "已取消安装"
  fi

  install_base_deps
  [[ "$INSTALL_OPTIMIZERS" == "yes" ]] && install_image_optimizers

  ensure_service_user
  mkdir -p "${INSTALL_DIR}/data"

  acquire_binary "$arch" "${TMP_DIR}/pic-tg"
  install -m 0755 "${TMP_DIR}/pic-tg" "${INSTALL_DIR}/pic-tg.new"
  mv -f "${INSTALL_DIR}/pic-tg.new" "${INSTALL_DIR}/pic-tg"

  if [[ -f "${INSTALL_DIR}/config.json" ]]; then
    log "检测到已存在 ${INSTALL_DIR}/config.json，保留原有配置与凭据不覆盖"
  else
    GENERATED_PASSWORD="$AUTH_PASSWORD"
    if [[ -z "$GENERATED_PASSWORD" ]]; then
      GENERATED_PASSWORD="$(generate_secret 12)"
    fi
    GENERATED_API_KEY="pic_$(generate_secret 18)"
    while [[ "$GENERATED_API_KEY" == "$GENERATED_PASSWORD" ]]; do
      GENERATED_API_KEY="pic_$(generate_secret 18)"
    done
    write_config_json \
      "${INSTALL_DIR}/config.json" \
      "$PORT" \
      "https://${DOMAIN}" \
      "${INSTALL_DIR}/data" \
      "$GENERATED_PASSWORD" \
      "$GENERATED_API_KEY" \
      "$trust_header"
    CREATED_CONFIG="yes"
    log "已生成 ${INSTALL_DIR}/config.json（权限 0600）"
  fi

  chown -R "${SERVICE_USER}:${SERVICE_USER}" "${INSTALL_DIR}"
  chmod 0700 "${INSTALL_DIR}/data"
  chmod 0600 "${INSTALL_DIR}/config.json"

  write_systemd_unit "${INSTALL_DIR}"
  systemctl daemon-reload
  systemctl enable --now "${SERVICE_NAME}.service"

  if ! wait_for_service "$PORT"; then
    journalctl -u "${SERVICE_NAME}.service" -n 30 --no-pager >&2 || true
    die "Pic TG 服务启动后健康检查未通过，请查看上方日志"
  fi
  log "Pic TG 服务已启动并通过本地健康检查"

  if [[ "$USE_CADDY" == "yes" ]]; then
    configure_caddy "$DOMAIN" "$PORT"
    maybe_open_firewall
  fi

  echo ""
  echo "============================================================"
  echo "Pic TG 安装完成"
  echo "============================================================"
  echo "访问地址:   https://${DOMAIN}"
  echo "安装目录:   ${INSTALL_DIR}"
  echo "配置文件:   ${INSTALL_DIR}/config.json"
  if [[ "$CREATED_CONFIG" == "yes" ]]; then
    echo "登录密码:   ${GENERATED_PASSWORD}"
    echo "API Key:    ${GENERATED_API_KEY}"
    echo "（请妥善保存上述凭据；登录后可在「设置 -> Telegram 存储」绑定 Bot 与频道）"
  else
    echo "（已沿用原有 ${INSTALL_DIR}/config.json 中的登录密码与 API Key）"
  fi
  if [[ "$USE_CADDY" != "yes" ]]; then
    echo ""
    echo "您选择了手动配置反向代理，请将 https://${DOMAIN} 反向代理到 http://127.0.0.1:${PORT}"
    echo "若使用了反向代理，请在 config.json 设置对应的 trust_proxy_header（CF-Connecting-IP / X-Real-IP / X-Forwarded-For）。"
  fi
  echo ""
  echo "常用运维命令:"
  echo "  查看状态: systemctl status ${SERVICE_NAME}"
  echo "  查看日志: journalctl -u ${SERVICE_NAME} -f"
  echo "  无损升级: curl -fsSL https://raw.githubusercontent.com/${REPO}/${BRANCH}/install.sh | sudo bash -s -- --upgrade"
  echo "  卸载服务: curl -fsSL https://raw.githubusercontent.com/${REPO}/${BRANCH}/install.sh | sudo bash -s -- --uninstall"
  echo "============================================================"
}

do_upgrade() {
  require_root
  require_linux_systemd
  local arch
  arch="$(detect_arch)"
  TMP_DIR="$(mktemp -d /tmp/pic-tg-upgrade.XXXXXX)"

  INSTALL_DIR="$(detect_existing_dir)"
  validate_install_dir "$INSTALL_DIR" || die "非法安装目录: ${INSTALL_DIR}"
  [[ -f "${INSTALL_DIR}/pic-tg" ]] || die "未在 ${INSTALL_DIR} 找到已安装的 pic-tg"

  local port="8798"
  if [[ -f "${INSTALL_DIR}/config.json" ]]; then
    local parsed_port
    parsed_port="$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "${INSTALL_DIR}/config.json" | head -n 1)"
    [[ -n "$parsed_port" ]] && port="$parsed_port"
  fi

  install_base_deps
  acquire_binary "$arch" "${TMP_DIR}/pic-tg"

  local backup="${INSTALL_DIR}/pic-tg.bak.$(date +%Y%m%d%H%M%S)"
  cp -p "${INSTALL_DIR}/pic-tg" "$backup"
  install -m 0755 "${TMP_DIR}/pic-tg" "${INSTALL_DIR}/pic-tg.new"
  if id -u "$SERVICE_USER" >/dev/null 2>&1; then
    chown "${SERVICE_USER}:${SERVICE_USER}" "${INSTALL_DIR}/pic-tg.new"
  fi
  mv -f "${INSTALL_DIR}/pic-tg.new" "${INSTALL_DIR}/pic-tg"

  systemctl restart "${SERVICE_NAME}.service"
  if ! wait_for_service "$port"; then
    err "新版本启动失败，正在自动回滚至 ${backup}..."
    mv -f "$backup" "${INSTALL_DIR}/pic-tg"
    systemctl restart "${SERVICE_NAME}.service" || true
    die "升级失败并已尝试回滚，请查看 journalctl -u ${SERVICE_NAME}"
  fi
  log "升级完成并已通过健康检查（旧版本备份: ${backup}）"
}

do_uninstall() {
  require_root
  require_linux_systemd
  INSTALL_DIR="$(detect_existing_dir)"
  validate_install_dir "$INSTALL_DIR" || die "非法安装目录: ${INSTALL_DIR}"

  local confirm
  confirm="$(prompt_yes_no "确认停用并移除 Pic TG systemd 服务及 Caddy 站点配置？" "no")"
  [[ "$confirm" == "yes" ]] || die "已取消卸载"

  if systemctl list-unit-files | grep -q "^${SERVICE_NAME}\.service"; then
    systemctl disable --now "${SERVICE_NAME}.service" || true
  fi
  rm -f "$SERVICE_FILE"
  systemctl daemon-reload

  if [[ -f "$CADDY_SITE_FILE" ]]; then
    rm -f "$CADDY_SITE_FILE"
    if command -v caddy >/dev/null 2>&1 && systemctl is-active --quiet caddy; then
      systemctl reload caddy || true
    fi
    log "已移除 Caddy 站点配置: ${CADDY_SITE_FILE}"
  fi

  local purge
  purge="$(prompt_yes_no "是否同时删除数据与配置目录 ${INSTALL_DIR}？（包含 SQLite 索引与 config.json）" "no")"
  if [[ "$purge" == "yes" ]]; then
    rm -rf "$INSTALL_DIR"
    log "已删除 ${INSTALL_DIR}"
  else
    log "已保留数据与配置目录: ${INSTALL_DIR}"
  fi
  log "Pic TG 卸载完成"
}

main() {
  parse_args "$@"
  case "$ACTION" in
    install) do_install ;;
    upgrade) do_upgrade ;;
    uninstall) do_uninstall ;;
    *) die "unsupported action: ${ACTION}" ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
