#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Install Mule as a systemd service.

Usage:
  install-systemd.sh agent --config FILE [options]
  install-systemd.sh server --config FILE [options]

Inline configuration (for simple setups):
  install-systemd.sh agent --server HOST:PORT --agent-id ID [options]
  install-systemd.sh server --listen-udp ADDR --agent-id ID [options]

Options:
  --binary PATH          Mule binary to install (default: mule in PATH)
  --config PATH          Existing YAML config to validate and install (recommended)
  --metrics-listen ADDR  Enable local metrics and /status HTTP listener
  --force                Replace existing config/unit (and imported secret)
  --no-start             Install but do not enable/start the service
  --dry-run              Validate and print generated files without changes
  -h, --help             Show this help

Inline configuration options:
  --server HOST:PORT     Server address for an agent
  --listen-udp ADDR     UDP/QUIC listen address for a server
  --agent-id ID         Agent identity; one active connection per ID
  --secret-source PATH   Existing shared secret to install
  --forward SERVICE=ADDR Add a forward service; may be repeated
  --reverse SERVICE=ADDR Add a reverse service; may be repeated
  --rotate-secret        Generate and install a new secret explicitly

Agent example:
  sudo ./deployment/install-systemd.sh agent \
    --config ./agent.yaml --binary ./bin/mule

Server example:
  sudo ./deployment/install-systemd.sh server \
    --config ./server.yaml --binary ./bin/mule

The config file owns agent IDs, secret paths, routes, timeouts, and limits.
Referenced secrets should use absolute paths and must already exist on the
target host with safe permissions.
EOF
}

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
info() { printf '==> %s\n' "$*"; }

[[ $# -gt 0 ]] || { usage; exit 2; }
if [[ $1 == -h || $1 == --help ]]; then usage; exit 0; fi
role=$1
shift
[[ $role == agent || $role == server ]] || die "role must be agent or server"

binary=""
config_source=""
secret_source=""
server_addr=""
listen_udp=""
agent_id=""
metrics_listen=""
force=false
rotate_secret=false
start=true
dry_run=false
secret_changed=false
declare -a forwards=() reverses=()

while [[ $# -gt 0 ]]; do
  case $1 in
    --binary) [[ $# -ge 2 ]] || die "--binary requires a value"; binary=$2; shift 2 ;;
    --config) [[ $# -ge 2 ]] || die "--config requires a value"; config_source=$2; shift 2 ;;
    --secret-source) [[ $# -ge 2 ]] || die "--secret-source requires a value"; secret_source=$2; shift 2 ;;
    --server) [[ $# -ge 2 ]] || die "--server requires a value"; server_addr=$2; shift 2 ;;
    --listen-udp) [[ $# -ge 2 ]] || die "--listen-udp requires a value"; listen_udp=$2; shift 2 ;;
    --agent-id) [[ $# -ge 2 ]] || die "--agent-id requires a value"; agent_id=$2; shift 2 ;;
    --forward) [[ $# -ge 2 ]] || die "--forward requires a value"; forwards+=("$2"); shift 2 ;;
    --reverse) [[ $# -ge 2 ]] || die "--reverse requires a value"; reverses+=("$2"); shift 2 ;;
    --metrics-listen) [[ $# -ge 2 ]] || die "--metrics-listen requires a value"; metrics_listen=$2; shift 2 ;;
    --force) force=true; shift ;;
    --rotate-secret) rotate_secret=true; shift ;;
    --no-start) start=false; shift ;;
    --dry-run) dry_run=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

if [[ -n $config_source ]]; then
  [[ -f $config_source ]] || die "config is not a regular file: $config_source"
  [[ -z $secret_source && $rotate_secret == false && -z $server_addr && -z $listen_udp && -z $agent_id && ${#forwards[@]} -eq 0 && ${#reverses[@]} -eq 0 ]] || \
    die "--config cannot be combined with inline configuration or secret options"
else
  [[ -z $secret_source || $rotate_secret == false ]] || die "do not combine --secret-source and --rotate-secret"
  [[ $agent_id =~ ^[A-Za-z0-9-]{1,63}$ ]] || die "--agent-id must use 1-63 letters, digits, or hyphens"
  if [[ $role == agent ]]; then
    [[ -n $server_addr ]] || die "agent requires --config FILE or --server HOST:PORT"
    [[ ${#forwards[@]} -gt 0 || ${#reverses[@]} -gt 0 ]] || die "inline agent configuration requires at least one --forward or --reverse"
  else
    [[ -n $listen_udp ]] || die "server requires --config FILE or --listen-udp ADDR"
  fi
fi

validate_mapping() {
  local mapping=$1 service address
  [[ $mapping == *=* ]] || die "mapping must be SERVICE=ADDR: $mapping"
  service=${mapping%%=*}
  address=${mapping#*=}
  [[ $service =~ ^[A-Za-z0-9_.-]{1,64}$ ]] || die "invalid service name: $service"
  [[ -n $address && $address != *$'\n'* ]] || die "invalid mapping address"
	validate_port "$address"
}

validate_port() {
  local address=$1 port=${1##*:}
  [[ $address != *[[:space:]]* && $address != *\\* && $address != *\"* && $address != *\'* ]] || die "invalid characters in address: $address"
  [[ $address == *:* && $port =~ ^[0-9]+$ && $port -ge 1 && $port -le 65535 ]] || die "invalid host:port address: $address"
}

reject_privileged_listener() {
  local address=$1 port=${1##*:}
  (( 10#$port >= 1024 )) || die "listener port $port requires privileges removed by the systemd unit; use port 1024 or higher"
}
if [[ -z $config_source ]]; then
  for mapping in "${forwards[@]}" "${reverses[@]}"; do validate_mapping "$mapping"; done
  if [[ $role == agent ]]; then
    validate_port "$server_addr"
    for mapping in "${forwards[@]}"; do reject_privileged_listener "${mapping#*=}"; done
  else
    validate_port "$listen_udp"
    reject_privileged_listener "$listen_udp"
    for mapping in "${reverses[@]}"; do reject_privileged_listener "${mapping#*=}"; done
  fi
fi
[[ -z $metrics_listen ]] || { validate_port "$metrics_listen"; reject_privileged_listener "$metrics_listen"; }

if [[ -z $binary ]]; then
  binary=$(command -v mule || true)
fi
[[ -n $binary && -x $binary ]] || die "Mule binary not found; use --binary PATH"
binary=$(readlink -f "$binary")
if [[ -n $config_source ]]; then
  config_source=$(readlink -f "$config_source")
  info "validating ${role} config ${config_source}"
  "$binary" check-config --role "$role" --config "$config_source" --skip-secrets
fi

if [[ $dry_run == false ]]; then
  [[ $EUID -eq 0 ]] || die "run as root (or use --dry-run)"
  [[ $(uname -s) == Linux ]] || die "system installation is supported on Linux only"
  command -v systemctl >/dev/null || die "systemctl not found"
  command -v runuser >/dev/null || die "runuser not found"
  [[ -d /run/systemd/system ]] || die "systemd is not running"
fi

distro=unknown
if [[ -r /etc/os-release ]]; then
  # shellcheck disable=SC1091
  . /etc/os-release
  distro=${PRETTY_NAME:-${ID:-unknown}}
fi
info "detected ${distro}"

config_path="/etc/mule/${role}.yaml"
secret_path="/etc/mule/${agent_id}.key"
unit_path="/etc/systemd/system/mule-${role}.service"

yaml_quote() {
  local value=${1//\\/\\\\}
  value=${value//\"/\\\"}
  printf '"%s"' "$value"
}

render_mappings() {
  local heading=$1
  shift
  [[ $# -gt 0 ]] || return 0
  printf '%s:\n' "$heading"
  local mapping service address
  for mapping in "$@"; do
    service=${mapping%%=*}
    address=${mapping#*=}
    printf '  %s: ' "$service"
    yaml_quote "$address"
    printf '\n'
  done
}

render_config() {
  if [[ -n $config_source ]]; then
    cat "$config_source"
    return
  fi
  if [[ $role == agent ]]; then
    printf 'server: '; yaml_quote "$server_addr"; printf '\n'
    printf 'agent_id: '; yaml_quote "$agent_id"; printf '\n'
    printf 'secret_file: '; yaml_quote "$secret_path"; printf '\n'
    printf 'idle_timeout: "1h"\nkeepalive: "20s"\n\n'
    render_mappings forward "${forwards[@]}"
    [[ ${#forwards[@]} -eq 0 || ${#reverses[@]} -eq 0 ]] || printf '\n'
    render_mappings reverse "${reverses[@]}"
  else
    printf 'listen_udp: '; yaml_quote "$listen_udp"; printf '\n'
    printf 'idle_timeout: "1h"\nkeepalive: "20s"\n\nagents:\n  %s:\n    secret_file: ' "$agent_id"
    yaml_quote "$secret_path"; printf '\n'
    if [[ ${#forwards[@]} -gt 0 ]]; then
      printf '    forward:\n'
      local mapping service address
      for mapping in "${forwards[@]}"; do
        service=${mapping%%=*}; address=${mapping#*=}
        printf '      %s: ' "$service"; yaml_quote "$address"; printf '\n'
      done
    fi
    if [[ ${#reverses[@]} -gt 0 ]]; then
      printf '    reverse:\n'
      local mapping service address
      for mapping in "${reverses[@]}"; do
        service=${mapping%%=*}; address=${mapping#*=}
        printf '      %s: ' "$service"; yaml_quote "$address"; printf '\n'
      done
    fi
  fi
}

render_unit() {
  local metrics_arg="" escaped_metrics
  if [[ -n $metrics_listen ]]; then
    escaped_metrics=${metrics_listen//%/%%}
    metrics_arg=" --metrics-listen ${escaped_metrics}"
  fi
  cat <<EOF
[Unit]
Description=Mule encrypted TCP tunnel ${role}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=mule
Group=mule
ExecStart=/usr/local/bin/mule ${role} --config ${config_path} --log-format json${metrics_arg}
Restart=on-failure
RestartSec=3
RuntimeDirectory=mule

NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
CapabilityBoundingSet=
AmbientCapabilities=
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictSUIDSGID=yes

[Install]
WantedBy=multi-user.target
EOF
}

if [[ $dry_run == true ]]; then
  printf '\n--- %s ---\n' "$config_path"
  render_config
  printf '\n--- %s ---\n' "$unit_path"
  render_unit
  if [[ -n $config_source ]]; then
    printf '\nNote: referenced secrets are checked as the mule user during installation.\n'
  fi
  exit 0
fi

if [[ $force == false ]]; then
  [[ ! -e $config_path ]] || die "$config_path exists; use --force to replace it"
  [[ ! -e $unit_path ]] || die "$unit_path exists; use --force to replace it"
fi
if [[ -z $config_source && -n $secret_source ]]; then
  [[ -f $secret_source ]] || die "secret source is not a regular file: $secret_source"
  [[ ! -e $secret_path || $force == true ]] || die "$secret_path exists; use --force to replace it with --secret-source"
fi

info "creating system user and directories"
if ! getent group mule >/dev/null; then groupadd --system mule; fi
if ! getent passwd mule >/dev/null; then
  nologin_shell=$(command -v nologin || true)
  [[ -n $nologin_shell ]] || die "nologin shell not found"
  useradd --system --gid mule --home-dir /nonexistent --shell "$nologin_shell" mule
fi
install -d -o root -g mule -m 0750 /etc/mule
install -d -o root -g root -m 0755 /usr/local/bin
if [[ $binary != /usr/local/bin/mule ]]; then
  install -o root -g root -m 0755 "$binary" /usr/local/bin/mule
else
  chown root:root /usr/local/bin/mule
  chmod 0755 /usr/local/bin/mule
fi

tmp_config=$(mktemp)
tmp_unit=$(mktemp --suffix=.service)
tmp_secret=""
secret_stage_dir=""
trap 'rm -f "$tmp_config" "$tmp_unit" "${tmp_secret:-}"; [[ -z ${secret_stage_dir:-} ]] || rmdir "$secret_stage_dir" 2>/dev/null || true' EXIT
render_config >"$tmp_config"
render_unit >"$tmp_unit"
if command -v systemd-analyze >/dev/null; then
  systemd-analyze verify "$tmp_unit"
else
  printf 'warning: systemd-analyze not found; unit was not verified\n' >&2
fi

if [[ -n $config_source ]]; then
  info "validating referenced secrets as the mule user"
  chown root:mule "$tmp_config"
  chmod 0640 "$tmp_config"
  runuser --user mule -- /usr/local/bin/mule check-config --role "$role" --config "$tmp_config"
elif [[ -n $secret_source ]]; then
  info "validating existing secret"
  secret_stage_dir=$(mktemp -d /etc/mule/.secret-stage.XXXXXX)
  tmp_secret="${secret_stage_dir}/secret.key"
  install -o mule -g mule -m 0600 "$secret_source" "$tmp_secret"
  /usr/local/bin/mule check --secret-file "$tmp_secret"
  secret_changed=true
elif [[ $rotate_secret == true || ! -e $secret_path ]]; then
  info "generating and validating new secret"
  secret_stage_dir=$(mktemp -d /etc/mule/.secret-stage.XXXXXX)
  tmp_secret="${secret_stage_dir}/secret.key"
  /usr/local/bin/mule keygen --out "$tmp_secret"
  chown mule:mule "$tmp_secret"
  chmod 0600 "$tmp_secret"
  /usr/local/bin/mule check --secret-file "$tmp_secret"
  secret_changed=true
else
  info "preserving existing secret ${secret_path}"
  /usr/local/bin/mule check --secret-file "$secret_path"
fi

was_active=false
if systemctl is-active --quiet "mule-${role}.service"; then was_active=true; fi
backup_suffix="$(date -u +%Y%m%d-%H%M%S).$$"
if [[ $force == true ]]; then
  [[ ! -e $config_path ]] || cp -a "$config_path" "${config_path}.bak.${backup_suffix}"
  [[ ! -e $unit_path ]] || cp -a "$unit_path" "${unit_path}.bak.${backup_suffix}"
fi
if [[ $secret_changed == true && -e $secret_path ]]; then
  cp -a "$secret_path" "${secret_path}.bak.${backup_suffix}"
fi
install -o root -g mule -m 0640 "$tmp_config" "$config_path"
install -o root -g root -m 0644 "$tmp_unit" "$unit_path"
if [[ $secret_changed == true ]]; then
  mv -f "$tmp_secret" "$secret_path"
  tmp_secret=""
  rmdir "$secret_stage_dir"
  secret_stage_dir=""
fi
if [[ -z $config_source ]]; then
  chown mule:mule "$secret_path"
  chmod 0600 "$secret_path"
fi

systemctl daemon-reload
if [[ $start == true ]]; then
  systemctl enable "mule-${role}.service"
  if [[ $was_active == true ]]; then
    systemctl restart "mule-${role}.service"
  else
    systemctl start "mule-${role}.service"
  fi
  systemctl --no-pager --full status "mule-${role}.service" || true
else
  info "installed without starting (--no-start)"
  printf 'Next steps:\n'
  printf '  systemctl enable --now mule-%s.service\n' "$role"
  printf '  journalctl -u mule-%s.service -f\n' "$role"
  if [[ -n $metrics_listen ]]; then
    printf '  curl http://%s/status\n' "$metrics_listen"
  fi
fi

info "installed ${unit_path}"
info "configuration: ${config_path}"
if [[ -z $config_source ]]; then
  info "shared secret: ${secret_path}"
else
  info "secret paths: managed by ${config_path}"
fi
if [[ -z $config_source && $secret_changed == true ]]; then
  printf 'IMPORTANT: securely copy %s to the matching peer before starting that peer.\n' "$secret_path"
fi
