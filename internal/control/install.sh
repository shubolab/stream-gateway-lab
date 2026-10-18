#!/bin/sh
# 用户权限安装；Linux 使用 systemd，macOS 使用 LaunchAgent，否则以前台模式运行。
set -eu
umask 077
gateway=
join_token=
name=$(hostname)
install_dir=${HOME:?}/.local/share/stream-gateway-executor
mode=auto
expected_platform=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --gateway|--token|--name|--dir|--platform)
      [ "$#" -ge 2 ] || { echo "Missing value for $1" >&2; exit 1; }
      case "$1" in
        --gateway) gateway=$2 ;; --token) join_token=$2 ;;
        --name) name=$2 ;; --dir) install_dir=$2 ;;
        --platform) expected_platform=$2 ;;
      esac
      shift 2 ;;
    --foreground) mode=foreground; shift ;;
    --no-start) mode=no-start; shift ;;
    *) echo "Unknown option: $1" >&2; exit 1 ;;
  esac
done
detected=$(uname -s)/$(uname -m)
case "$detected" in
  Linux/x86_64) platform=linux-amd64; checksum_tool=sha256sum ;;
  Darwin/arm64) platform=darwin-arm64; checksum_tool=shasum ;;
  *) echo 'Supported platforms: Linux/amd64 and macOS/arm64 (Apple Silicon).' >&2; exit 1 ;;
esac
if [ -n "$expected_platform" ] && [ "$expected_platform" != "$platform" ]; then
  printf 'Platform mismatch: selected %s, but this terminal reports %s (%s).\n' "$expected_platform" "$detected" "$platform" >&2
  echo 'Run the command in a terminal on the selected machine. No download or registration was performed.' >&2
  exit 1
fi
echo "Detected machine: $detected ($(hostname))"
case "$gateway" in http://*|https://*) ;; *) echo 'Supply --gateway http(s)://host:port' >&2; exit 1 ;; esac
gateway=${gateway%/}
case "$install_dir" in /*) ;; *) echo '--dir must be an absolute path' >&2; exit 1 ;; esac
for tool in curl gzip "$checksum_tool"; do
  command -v "$tool" >/dev/null 2>&1 || { echo "Install $tool first." >&2; exit 1; }
done
mkdir -p "$install_dir"
config=$install_dir/executor.json
binary=$install_dir/stream-gateway-executor
if [ ! -f "$config" ] && [ -z "$join_token" ]; then
  echo 'Supply a fresh one-use --token from the Gateway console.' >&2; exit 1
fi
work=$(mktemp -d "$install_dir/.download.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM
artifact=stream-gateway-executor-$platform
echo "Downloading and verifying $platform Executor…"
curl --fail --silent --show-error --location --connect-timeout 15 --max-time 600 "$gateway/downloads/$artifact.gz" -o "$work/$artifact.gz"
gzip -d "$work/$artifact.gz"
curl --fail --silent --show-error --location --connect-timeout 10 --max-time 30 "$gateway/downloads/$artifact.sha256" -o "$work/checksum"
if [ "$checksum_tool" = shasum ]; then
  (cd "$work" && shasum -a 256 -c checksum)
else
  (cd "$work" && sha256sum -c checksum)
fi
chmod 700 "$work/$artifact"
# 下载完成后原子替换文件，运行中的旧进程不受半截下载影响。
mv "$work/$artifact" "$binary"
LAB_JOIN_TOKEN=$join_token "$binary" executor register --gateway "$gateway" --name "$name" --out "$config"
unset join_token
rm -rf "$work"
trap - EXIT HUP INT TERM
echo "Executor installed. Registration: $config"
if [ "$mode" = no-start ]; then
  printf 'Start with: "%s" executor -registration "%s"\n' "$binary" "$config"
  exit 0
fi
if [ "$mode" = auto ] && [ "$platform" = darwin-arm64 ]; then
  domain=gui/$(id -u)
  label=com.shubolab.stream-gateway-executor
  if launchctl print "$domain" >/dev/null 2>&1; then
    # plist 中使用 XML 转义，安装路径可以包含空格和 XML 特殊字符。
    xml_escape() { printf '%s' "$1" | sed -e 's/\&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g' -e "s/'/\&apos;/g"; }
    agent_dir=$HOME/Library/LaunchAgents
    mkdir -p "$agent_dir" "$install_dir/logs"
    plist=$agent_dir/$label.plist
    cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key><array>
    <string>$(xml_escape "$binary")</string><string>executor</string>
    <string>-registration</string><string>$(xml_escape "$config")</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>ExitTimeOut</key><integer>10</integer>
  <key>Umask</key><integer>63</integer>
  <key>StandardOutPath</key><string>$(xml_escape "$install_dir/logs/stdout.log")</string>
  <key>StandardErrorPath</key><string>$(xml_escape "$install_dir/logs/stderr.log")</string>
</dict></plist>
EOF
    plutil -lint "$plist"
    launchctl bootout "$domain/$label" >/dev/null 2>&1 || true
    launchctl bootstrap "$domain" "$plist"
    echo "Executor started as LaunchAgent: $label"
    echo "Logs: $install_dir/logs/stderr.log"
    echo "Stop: launchctl bootout $domain/$label"
    exit 0
  fi
fi
if [ "$mode" = auto ] && command -v systemctl >/dev/null 2>&1; then
  scope=
  unit_dir=
  if [ "$(id -u)" = 0 ] && [ -d /run/systemd/system ]; then
    unit_dir=/etc/systemd/system
  elif systemctl --user show-environment >/dev/null 2>&1; then
    scope=--user
    unit_dir=$HOME/.config/systemd/user
  fi
  if [ -n "$unit_dir" ]; then
    # systemd 对引号、反斜线和百分号有特殊解释；特殊路径可用 --foreground。
    case "$install_dir" in *\"*|*\\*|*%*) echo 'Use --foreground for this installation path.' >&2; exit 1 ;; esac
    mkdir -p "$unit_dir"
    cat > "$unit_dir/stream-gateway-executor.service" <<EOF
[Unit]
Description=Stream Gateway Executor
After=network-online.target
Wants=network-online.target

[Service]
ExecStart="$binary" executor -registration "$config"
Restart=on-failure
RestartSec=5
TimeoutStopSec=10
UMask=0077

[Install]
WantedBy=default.target
EOF
    systemctl $scope daemon-reload
    systemctl $scope enable stream-gateway-executor.service
    systemctl $scope restart stream-gateway-executor.service
    systemctl $scope --no-pager status stream-gateway-executor.service
    if [ -n "$scope" ]; then
      echo 'For operation after logout, enable lingering for this user: loginctl enable-linger'
    fi
    exit 0
  fi
fi
echo 'Running in foreground; keep this session open. Ctrl+C stops the Executor.'
exec "$binary" executor -registration "$config"
