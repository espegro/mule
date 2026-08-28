#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Install Mule as a systemd service.

Usage:
  # Default setup: run once on each agent, then copy its generated key
  install-systemd.sh agent --server HOST:PORT --agent-id ID [options]

  # Default setup: run on the server after collecting the agent keys
  install-systemd.sh server --listen-udp ADDR --agent ID=KEYFILE \
    [--agent ID=KEYFILE ...] [options]

  # Install an existing advanced configuration
  install-systemd.sh agent|server --config FILE [options]

Options:
  --binary PATH          Mule binary to install (default: mule in PATH)
  --config PATH          Existing YAML config to validate and install
  --metrics-listen ADDR  Enable local metrics and /status HTTP listener
  --force                Replace existing config/unit (and imported secret)
  --no-start             Install but do not enable/start the service
  --dry-run              Validate and print generated files without changes
  -h, --help             Show this help

Inline configuration options:
  --server HOST:PORT     Server address for an agent
  --listen-udp ADDR      UDP/QUIC listen address for a server
  --agent-id ID          Identity for an agent; generates its key if absent
  --agent ID=KEYFILE     Server agent and collected key; may be repeated
  --secret-source PATH   Existing key for legacy single-agent setup
  --forward SERVICE=ADDR Add a forward service; may be repeated
  --reverse SERVICE=ADDR Add a reverse service; may be repeated
  --rotate-secret        Generate and install a new secret explicitly

Default agent example:
  sudo ./deployment/install-systemd.sh agent \
    --binary ./bin/mule --server mule.example.org:4400 \
    --agent-id client-a --forward ollama=127.0.0.1:10000

Default server example:
  sudo ./deployment/install-systemd.sh server \
    --binary ./bin/mule --listen-udp :4400 \
    --agent client-a=./client-a.key --agent client-b=./client-b.key \
    --forward ollama=127.0.0.1:11434

The agent command creates the mule user, directories, key, config, and unit.
Copy /etc/mule/ID.key securely from every agent to the server. The server
command imports those keys and creates its config and unit. Use --config for
advanced setups that already have YAML and safely installed secret files.
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
declare -a forwards=() reverses=()
declare -a agent_specs=() imported_agent_ids=() imported_secret_sources=()

while [[ $# -gt 0 ]]; do
  case $1 in
    --binary) [[ $# -ge 2 ]] || die "--binary requires a value"; binary=$2; shift 2 ;;
    --config) [[ $# -ge 2 ]] || die "--config requires a value"; config_source=$2; shift 2 ;;
    --secret-source) [[ $# -ge 2 ]] || die "--secret-source requires a value"; secret_source=$2; shift 2 ;;
    --server) [[ $# -ge 2 ]] || die "--server requires a value"; server_addr=$2; shift 2 ;;
    --listen-udp) [[ $# -ge 2 ]] || die "--listen-udp requires a value"; listen_udp=$2; shift 2 ;;
    --agent-id) [[ $# -ge 2 ]] || die "--agent-id requires a value"; agent_id=$2; shift 2 ;;
    --agent) [[ $# -ge 2 ]] || die "--agent requires a value"; agent_specs+=("$2"); shift 2 ;;
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
  [[ -z $secret_source && $rotate_secret == false && -z $server_addr && -z $listen_udp && -z $agent_id && ${#agent_specs[@]} -eq 0 && ${#forwards[@]} -eq 0 && ${#reverses[@]} -eq 0 ]] || \
    die "--config cannot be combined with inline configuration or secret options"
else
  [[ -z $secret_source || $rotate_secret == false ]] || die "do not combine --secret-source and --rotate-secret"
  if [[ $role == agent ]]; then
    [[ ${#agent_specs[@]} -eq 0 ]] || die "--agent is only valid for the server role"
    [[ $agent_id =~ ^[A-Za-z0-9-]{1,63}$ ]] || die "--agent-id must use 1-63 letters, digits, or hyphens"
    [[ -n $server_addr ]] || die "agent requires --config FILE or --server HOST:PORT"
    [[ ${#forwards[@]} -gt 0 || ${#reverses[@]} -gt 0 ]] || die "inline agent configuration requires at least one --forward or --reverse"
  else
    [[ -n $listen_udp ]] || die "server requires --config FILE or --listen-udp ADDR"
    if [[ ${#agent_specs[@]} -gt 0 ]]; then
      [[ -z $agent_id && -z $secret_source && $rotate_secret == false ]] || \
        die "--agent cannot be combined with --agent-id, --secret-source, or --rotate-secret"
    else
      [[ $agent_id =~ ^[A-Za-z0-9-]{1,63}$ ]] || \
        die "server requires at least one --agent ID=KEYFILE (or legacy --agent-id ID)"
    fi
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

if [[ ${#agent_specs[@]} -gt 0 ]]; then
  declare -A seen_agent_ids=()
  for spec in "${agent_specs[@]}"; do
    [[ $spec == *=* ]] || die "--agent must use ID=KEYFILE: $spec"
    imported_id=${spec%%=*}
    imported_source=${spec#*=}
    [[ $imported_id =~ ^[A-Za-z0-9-]{1,63}$ ]] || die "invalid agent ID in --agent: $imported_id"
    [[ -z ${seen_agent_ids[$imported_id]+x} ]] || die "duplicate --agent ID: $imported_id"
    [[ -f $imported_source ]] || die "agent key is not a regular file: $imported_source"
    imported_source=$(readlink -f "$imported_source")
    for previous_i in "${!imported_secret_sources[@]}"; do
      if cmp -s "$imported_source" "${imported_secret_sources[$previous_i]}"; then
        die "agents ${imported_agent_ids[$previous_i]} and ${imported_id} use the same key; every agent needs a unique key"
      fi
    done
    seen_agent_ids[$imported_id]=1
    imported_agent_ids+=("$imported_id")
    imported_secret_sources+=("$imported_source")
  done
fi

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
    printf 'idle_timeout: "1h"\nkeepalive: "20s"\n\nagents:\n'
    local -a rendered_agent_ids=()
    if [[ ${#imported_agent_ids[@]} -gt 0 ]]; then
      rendered_agent_ids=("${imported_agent_ids[@]}")
    else
      rendered_agent_ids=("$agent_id")
    fi
    local rendered_id mapping service address
    for rendered_id in "${rendered_agent_ids[@]}"; do
      printf '  %s:\n    secret_file: ' "$rendered_id"
      yaml_quote "/etc/mule/${rendered_id}.key"; printf '\n'
      if [[ ${#forwards[@]} -gt 0 ]]; then
        printf '    forward:\n'
        for mapping in "${forwards[@]}"; do
          service=${mapping%%=*}; address=${mapping#*=}
          printf '      %s: ' "$service"; yaml_quote "$address"; printf '\n'
        done
      fi
      if [[ ${#reverses[@]} -gt 0 ]]; then
        printf '    reverse:\n'
        for mapping in "${reverses[@]}"; do
          service=${mapping%%=*}; address=${mapping#*=}
          printf '      %s: ' "$service"; yaml_quote "$address"; printf '\n'
        done
      fi
    done
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
  elif [[ ${#imported_agent_ids[@]} -gt 0 ]]; then
    printf '\nCollected agent keys to import:\n'
    for i in "${!imported_agent_ids[@]}"; do
      printf '  %s -> /etc/mule/%s.key\n' "${imported_secret_sources[$i]}" "${imported_agent_ids[$i]}"
    done
  elif [[ $role == agent ]]; then
    printf '\nThe agent key will be generated or preserved at %s.\n' "$secret_path"
  fi
  exit 0
fi

if [[ $force == false ]]; then
  [[ ! -e $config_path ]] || die "$config_path exists; use --force to replace it"
  [[ ! -e $unit_path ]] || die "$unit_path exists; use --force to replace it"
fi
if [[ ${#imported_agent_ids[@]} -gt 0 ]]; then
  for imported_id in "${imported_agent_ids[@]}"; do
    imported_destination="/etc/mule/${imported_id}.key"
    [[ ! -e $imported_destination || $force == true ]] || \
      die "$imported_destination exists; use --force to replace imported agent keys"
  done
elif [[ -z $config_source && -n $secret_source ]]; then
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
secret_stage_dir=""
declare -a tmp_secrets=() secret_destinations=()
cleanup() {
  rm -f "$tmp_config" "$tmp_unit"
  local staged
  for staged in "${tmp_secrets[@]}"; do
    [[ -z $staged ]] || rm -f "$staged"
  done
  [[ -z $secret_stage_dir ]] || rmdir "$secret_stage_dir" 2>/dev/null || true
}
trap cleanup EXIT
render_config >"$tmp_config"
render_unit >"$tmp_unit"
/usr/local/bin/mule check-config --role "$role" --config "$tmp_config" --skip-secrets
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
elif [[ ${#imported_agent_ids[@]} -gt 0 ]]; then
  info "validating collected agent keys"
  secret_stage_dir=$(mktemp -d /etc/mule/.secret-stage.XXXXXX)
  for i in "${!imported_agent_ids[@]}"; do
    imported_id=${imported_agent_ids[$i]}
    tmp_secret="${secret_stage_dir}/${imported_id}.key"
    tmp_secrets+=("$tmp_secret")
    secret_destinations+=("/etc/mule/${imported_id}.key")
    install -o mule -g mule -m 0600 "${imported_secret_sources[$i]}" "$tmp_secret"
    /usr/local/bin/mule check --secret-file "$tmp_secret"
    for ((previous_i = 0; previous_i < i; previous_i++)); do
      if cmp -s "$tmp_secret" "${tmp_secrets[$previous_i]}"; then
        die "agents ${imported_agent_ids[$previous_i]} and ${imported_id} use the same key; every agent needs a unique key"
      fi
    done
  done
elif [[ -n $secret_source ]]; then
  info "validating existing secret"
  secret_stage_dir=$(mktemp -d /etc/mule/.secret-stage.XXXXXX)
  tmp_secret="${secret_stage_dir}/secret.key"
  tmp_secrets+=("$tmp_secret")
  secret_destinations+=("$secret_path")
  install -o mule -g mule -m 0600 "$secret_source" "$tmp_secret"
  /usr/local/bin/mule check --secret-file "$tmp_secret"
elif [[ $rotate_secret == true || ! -e $secret_path ]]; then
  info "generating and validating new secret"
  secret_stage_dir=$(mktemp -d /etc/mule/.secret-stage.XXXXXX)
  tmp_secret="${secret_stage_dir}/secret.key"
  tmp_secrets+=("$tmp_secret")
  secret_destinations+=("$secret_path")
  /usr/local/bin/mule keygen --out "$tmp_secret"
  chown mule:mule "$tmp_secret"
  chmod 0600 "$tmp_secret"
  /usr/local/bin/mule check --secret-file "$tmp_secret"
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
for destination in "${secret_destinations[@]}"; do
  [[ ! -e $destination ]] || cp -a "$destination" "${destination}.bak.${backup_suffix}"
done
install -o root -g mule -m 0640 "$tmp_config" "$config_path"
install -o root -g root -m 0644 "$tmp_unit" "$unit_path"
for i in "${!tmp_secrets[@]}"; do
  mv -f "${tmp_secrets[$i]}" "${secret_destinations[$i]}"
  tmp_secrets[$i]=""
done
if [[ -n $secret_stage_dir ]]; then
  rmdir "$secret_stage_dir"
  secret_stage_dir=""
fi
if [[ -z $config_source && ${#imported_agent_ids[@]} -eq 0 ]]; then
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

if [[ -z $config_source && $role == agent ]]; then
  printf '\nAgent key to copy securely to the server:\n  %s\n' "$secret_path"
  printf 'On the server, install it with:\n'
  printf '  install-systemd.sh server ... --agent %s=%s\n' "$agent_id" "${agent_id}.key"
elif [[ ${#imported_agent_ids[@]} -gt 0 ]]; then
  printf '\nImported %s agent key(s). No manual config editing is required.\n' "${#imported_agent_ids[@]}"
fi

info "installed ${unit_path}"
info "configuration: ${config_path}"
if [[ -n $config_source ]]; then
  info "secret paths: managed by ${config_path}"
elif [[ ${#imported_agent_ids[@]} -gt 0 ]]; then
  info "agent keys: /etc/mule/ID.key"
else
  info "shared secret: ${secret_path}"
fi
