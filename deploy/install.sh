#!/usr/bin/env bash
# Shell Go 安装脚本（普通用户权限即可，不需要 root）
#
# 用法：
#   ./install.sh                       安装到 ~/shell-go，端口 8443
#   ./install.sh --dir /data/shell-go  指定安装目录
#   ./install.sh --port 9443           指定端口
#   ./install.sh --bind 127.0.0.1      只监听本机（配合反代）
#   ./install.sh --password 'xxx'      指定登录密码（默认随机生成）
#   ./install.sh --config /etc/shell-go.yaml  复用已有配置文件（不生成新配置）
#   ./install.sh --no-service          只解压文件，不注册服务
#   ./install.sh --allow-root          允许程序以 root 身份运行（会写入配置的 security.allow_root，不推荐）
set -euo pipefail

APP="shell-go"
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_DIR=""
PORT=8443
PASSWORD=""
WITH_SERVICE=1
BIND="0.0.0.0"
ALLOW_ROOT=0
NO_SERVICE=0       # 用户显式要求「只放文件、不起服务」
CONFIG_PATH=""     # 配置文件位置，留空 = $INSTALL_DIR/config.yaml
CONFIG_GIVEN=0     # 用户是否用 --config 显式指定
ORIG_ARGS=("$@")   # 用于在报错提示里还原用户的原始参数

c_ok()   { printf '\033[1;32m%s\033[0m\n' "$*"; }
c_info() { printf '\033[1;34m%s\033[0m\n' "$*"; }
c_warn() { printf '\033[1;33m%s\033[0m\n' "$*"; }
c_err()  { printf '\033[1;31m%s\033[0m\n' "$*" >&2; }
die()    { c_err "$*"; exit 1; }
# 路径要嵌进 sed 的 s|...|...| 里，& 和 | 必须转义，否则路径里带这两个字符就会生成坏文件
esc()    { printf '%s' "$1" | sed 's/[&|]/\\&/g'; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dir)       INSTALL_DIR="${2:-}"; shift 2 ;;
    --port)      PORT="${2:-}"; shift 2 ;;
    --bind)      BIND="${2:-}"; shift 2 ;;
    --password)  PASSWORD="${2:-}"; shift 2 ;;
    --config)    CONFIG_PATH="${2:-}"; CONFIG_GIVEN=1; shift 2 ;;
    --no-service) NO_SERVICE=1; shift ;;
    --allow-root) ALLOW_ROOT=1; shift ;;
    -h|--help)
      sed -n '2,12p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) die "未知参数：$1" ;;
  esac
done

# 默认装到 ~/shell-go：解压目录只当分发介质，装完可以删。
# 若把安装目录指向解压目录本身，升级时会覆盖到一半、也没法保留旧配置。
if [[ -z "$INSTALL_DIR" ]]; then
  [[ -n "${HOME:-}" ]] || die "无法确定 HOME，请用 --dir 指定安装目录"
  INSTALL_DIR="$HOME/$APP"
fi
# 规范化，便于与解压目录比较（目标可能还不存在，失败就用原样）
INSTALL_NORM="$(cd "$INSTALL_DIR" 2>/dev/null && pwd || printf '%s' "$INSTALL_DIR")"
if [[ "$INSTALL_NORM" == "$SRC_DIR" ]]; then
  die "--dir 不能是当前解压目录（${SRC_DIR}）。升级时会覆盖掉解压出来的文件，建议换一个独立目录。"
fi

# 配置文件位置：默认随安装目录，--config 可指向已有配置（集中管理或复用旧配置时用）。
# 服务启动时的 -config 由这里决定，装完再想换要改 unit / ctl.sh，所以在安装阶段就定下来。
abs_path() {
  local p="$1" d b
  d="$(dirname "$p")"
  b="$(basename "$p")"
  if [[ -d "$d" ]]; then d="$(cd "$d" && pwd)"; fi
  printf '%s/%s' "${d%/}" "$b"
}
if [[ -z "$CONFIG_PATH" ]]; then
  CONFIG_PATH="$INSTALL_DIR/config.yaml"
fi
CONFIG_PATH="$(abs_path "$CONFIG_PATH")"
if [[ "$CONFIG_GIVEN" == "1" && ! -f "$CONFIG_PATH" ]]; then
  die "指定的配置文件不存在：${CONFIG_PATH}
  想让它自动生成就别加 --config；想指定新位置请先创建文件，或用 --dir 换安装目录。"
fi

if [[ "$(uname -s)" != "Linux" ]]; then
  c_warn "当前系统不是 Linux：跳过 systemd 注册，改为后台进程方式启动"
  WITH_SERVICE=0
fi
if [[ "$NO_SERVICE" == "1" ]]; then
  c_info "已指定 --no-service：只放文件，不注册服务也不启动"
  WITH_SERVICE=0
fi

# 程序默认拒绝以 root 运行：它把可执行任意命令的 shell 暴露在网页上，
# 以 root 跑等于把整机交给任何拿到网页账号的人。这里提前拦住，别让人装完才发现起不来。
if [[ "$(id -u)" == "0" && "$ALLOW_ROOT" == "0" ]]; then
  c_err "当前是以 root 身份安装，程序默认会拒绝启动。"
  echo "  原因：本工具会在网页上暴露一个可执行任意命令的 shell，以 root 运行等于把整机交出去。"
  echo
  echo "  推荐：用普通用户安装"
  echo "    useradd -m ops && su - ops        # 若还没有普通账号"
  echo "    tar xzf shell-go-*-linux-amd64.tar.gz && cd shell-go-*-linux-amd64"
  echo "    ./install.sh --port $PORT"
  echo
  echo "  确需以 root 运行（仅限隔离的测试机），请显式加参数重跑："
  echo "    ./install.sh --allow-root ${ORIG_ARGS[*]:-}"
  exit 1
fi

# 生成随机密码。
# 不要写 `tr -dc ... < /dev/urandom | head -c 16`：head 读够就关闭管道，tr 收到 SIGPIPE，
# 在 set -o pipefail 下整条管道返回 141，再被 set -e 捕获会直接终止脚本
# （v1.4.0 及更早的版本就死在这一行，表现为打印完「安装到 …」就没了，端口自然起不来）。
gen_password() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 12 | cut -c1-16
  else
    # od / cut 都会读完输入，不会提前关闭管道
    head -c 64 /dev/urandom | od -An -tx1 | tr -d ' \n' | cut -c1-16
  fi
}

c_info "==> 安装到 ${INSTALL_DIR}"
# 父目录不可写时给出人能看懂的提示，而不是甩一句 mkdir: Permission denied
PARENT_DIR="$(dirname "$INSTALL_DIR")"
if [[ ! -d "$PARENT_DIR" ]]; then
  mkdir -p "$PARENT_DIR" 2>/dev/null || true
fi
if [[ ! -w "$PARENT_DIR" ]]; then
  die "安装目录不可写：$PARENT_DIR 没有写权限。请换一个当前用户可写的路径，例如 ./install.sh --dir ~/$APP"
fi
mkdir -p "$INSTALL_DIR/data"
cp "$SRC_DIR/$APP" "$INSTALL_DIR/$APP"
chmod +x "$INSTALL_DIR/$APP"

if [[ -f "$CONFIG_PATH" ]]; then
  c_warn "已存在配置文件 ${CONFIG_PATH}，保留原配置不做覆盖"
  CREATED_CONFIG=0
  if [[ "$CONFIG_GIVEN" == "1" && ( "$PORT" != "8443" || "$BIND" != "0.0.0.0" ) ]]; then
    c_warn "复用已有配置时 --port / --bind 不生效，监听地址以配置文件里的 listen 为准"
  fi
else
  if [[ -z "$PASSWORD" ]]; then
    PASSWORD="$(gen_password)"
    [[ -n "$PASSWORD" ]] || die "随机密码生成失败，请用 --password 手动指定"
  fi
  HASH="$("$INSTALL_DIR/$APP" -gen-hash "$PASSWORD")"
  [[ -n "$HASH" ]] || die "生成密码哈希失败"
  mkdir -p "$(dirname "$CONFIG_PATH")" 2>/dev/null || true
  sed -e "s|^listen:.*|listen: \"${BIND}:${PORT}\"|" \
      "$SRC_DIR/config.example.yaml" > "$CONFIG_PATH"
  # 用 password_hash 替换示例里的明文密码；role: admin 在下一行，不受影响
  awk -v h="$HASH" '
    /^  - username: admin$/ { print; getline; print "    password_hash: \"" h "\""; next }
    { print }
  ' "$CONFIG_PATH" > "$CONFIG_PATH.tmp"
  mv "$CONFIG_PATH.tmp" "$CONFIG_PATH"
  chmod 600 "$CONFIG_PATH"
  CREATED_CONFIG=1
fi

# --allow-root 必须对「复用已有配置」同样生效。
# v1.4.2 及更早只在「新建配置」的分支里写 allow_root，复用旧配置时装完仍然被程序拒绝启动：
# 表现是「已注册 systemd 用户服务」+「端口没有监听」，日志里反复刷 4 条 [错误] 检测到以 root 身份运行
# （Restart=on-failure 每 3 秒重启一次，看起来像「刷了 4 次」）。
set_allow_root() {
  local cfg="$1" tmp="$1.tmp.$$"
  if grep -qE '^[[:space:]]+allow_root:' "$cfg" 2>/dev/null; then
    # 已有这一项（值可能是 false）：就地改值，保留缩进
    sed -E 's|^([[:space:]]+)allow_root:.*|\1allow_root: true       # 由 install.sh --allow-root 写入|' \
      "$cfg" > "$tmp"
  elif grep -qE '^security:[[:space:]]*$' "$cfg" 2>/dev/null; then
    # 旧配置里没有这一项：插到 security: 段落第一行
    awk '/^security:[[:space:]]*$/ && !d {
           print; print "  allow_root: true       # 由 install.sh --allow-root 写入"; d=1; next
         }
         { print }' "$cfg" > "$tmp"
  else
    # 极简配置连 security: 段都没有
    { cat "$cfg"; printf '\nsecurity:\n  allow_root: true       # 由 install.sh --allow-root 写入\n'; } > "$tmp"
  fi
  mv "$tmp" "$cfg"
}

if [[ "$ALLOW_ROOT" == "1" ]]; then
  [[ -w "$CONFIG_PATH" ]] || die "配置文件不可写，无法写入 allow_root：${CONFIG_PATH}"
  set_allow_root "$CONFIG_PATH"
  c_warn "已允许以 root 身份运行（${CONFIG_PATH} 的 security.allow_root: true）"
  echo "        root 跑等于把整机交给任何拿到网页账号的人，请务必配置 security.ip_whitelist 与外网隔离"
elif [[ "$(id -u)" == "0" ]]; then
  # 上面已经拦过 root 且未给 --allow-root 的情况，这里只兜底提示，正常走不到
  c_warn "当前以 root 运行但未带 --allow-root，配置里的 security.allow_root 会决定能否启动"
fi

# 实际监听地址：新生成的配置就是 BIND:PORT；复用已有配置时以文件里的 listen 为准，
# 后面的端口自检与访问地址都按这个来，否则会去探一个根本没在听的端口。
EFF_BIND="$BIND"
EFF_PORT="$PORT"
if [[ "${CREATED_CONFIG:-0}" == "0" ]]; then
  CFG_LISTEN="$(sed -n 's/^listen:[[:space:]]*"\{0,1\}\([^"]*\)"\{0,1\}[[:space:]]*$/\1/p' "$CONFIG_PATH" 2>/dev/null | head -1)"
  if [[ -n "$CFG_LISTEN" && "$CFG_LISTEN" == *:* ]]; then
    EFF_BIND="${CFG_LISTEN%:*}"
    EFF_PORT="${CFG_LISTEN##*:}"
  fi
fi
c_info "==> 配置：${CONFIG_PATH}（监听 ${EFF_BIND}:${EFF_PORT}）"

cp "$SRC_DIR/README.md" "$INSTALL_DIR/README.md" 2>/dev/null || true

# 等端口真的在监听：进程起不来时能看到日志，而不是被一句「已启动」糊过去。
# bash 没编译 /dev/tcp 时退回 ss / netstat；用 awk 判断而不是 `| grep -q`，
# 后者找到即退出会让上游收 SIGPIPE，在 pipefail 下把结果变成假失败。
port_listening() {
  local port="$1" out=""
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    exec 3>&- 2>/dev/null || true
    return 0
  fi
  if command -v ss >/dev/null 2>&1; then
    out="$(ss -lnt 2>/dev/null || true)"
  fi
  if [[ -z "$out" ]] && command -v netstat >/dev/null 2>&1; then
    out="$(netstat -lnt 2>/dev/null || true)"
  fi
  if [[ -n "$out" ]]; then
    if printf '%s\n' "$out" | awk -v p=":$port" '$4 ~ p"$" {f=1} END{exit !f}'; then
      return 0
    fi
    return 1
  fi
  # 最后退回 lsof（精简系统上没有 ss/netstat 时）
  if command -v lsof >/dev/null 2>&1; then
    lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1 && return 0
  fi
  return 1
}

wait_port() {
  local port="$1" tries="${2:-40}" i=0
  while [[ "$i" -lt "$tries" ]]; do
    if port_listening "$port"; then
      return 0
    fi
    sleep 0.25
    i=$((i + 1))
  done
  return 1
}

verify_start() {
  local log="$1" hint="$2"
  # 探的必须是实际监听端口 EFF_PORT：复用已有配置时 --port 不生效，
  # 探 $PORT 会在服务明明起来了的情况下误报「端口没有监听」（v1.4.2 的遗留问题）。
  if wait_port "$EFF_PORT" 40; then
    c_ok "端口 $EFF_PORT 已在监听"
    return 0
  fi
  c_err "安装完成但服务没有起来：$EFF_PORT 端口没有监听。"
  if [[ -r "$log" ]]; then
    echo "  日志尾部（${log}）："
    tail -n 20 "$log" 2>/dev/null | sed 's/^/    /'
  else
    echo "  日志文件不存在：$log"
  fi
  echo
  echo "  可以按顺序排查："
  echo "    1) 端口被占用：  ss -lntp | grep :$EFF_PORT"
  echo "    2) 前台跑一次看报错："
  echo "       $INSTALL_DIR/$APP -config $CONFIG_PATH"
  echo "    3) $hint"
  return 1
}

STARTED_OK=1

if [[ "$WITH_SERVICE" == "1" ]]; then
  UNIT_DIR="$HOME/.config/systemd/user"
  mkdir -p "$UNIT_DIR"
  # unit 模板里的 %h/shell-go 是默认安装目录，--dir 换成别处时必须替换掉，
  # 否则 systemd 会去 ~/shell-go 找二进制（不存在），表现为「注册成功但端口不监听」。
  # 装在 $HOME 下时仍用 %h 前缀，保持对 home 迁移的可移植性。
  case "$INSTALL_DIR" in
    "$HOME"/*) UNIT_BASE="%h/${INSTALL_DIR#"$HOME"/}" ;;
    *)         UNIT_BASE="$INSTALL_DIR" ;;
  esac
  if [[ "$CONFIG_PATH" == "$INSTALL_DIR/config.yaml" ]]; then
    sed -e "s|%h/shell-go|$(esc "$UNIT_BASE")|g" \
      "$SRC_DIR/shell-go.service" > "$UNIT_DIR/shell-go.service"
  else
    # 配置在别处：先换整段配置路径，再换剩下的目录前缀
    sed -e "s|%h/shell-go/config.yaml|$(esc "$CONFIG_PATH")|g" \
        -e "s|%h/shell-go|$(esc "$UNIT_BASE")|g" \
      "$SRC_DIR/shell-go.service" > "$UNIT_DIR/shell-go.service"
  fi
  systemctl --user daemon-reload 2>/dev/null || true
  if systemctl --user enable --now shell-go 2>/dev/null; then
    c_ok "已注册 systemd 用户服务：shell-go"
    verify_start "$INSTALL_DIR/data/stdout.log" \
      "查服务状态：systemctl --user status shell-go ／ journalctl --user -u shell-go -n 50" \
      || STARTED_OK=0
  else
    c_warn "systemd 用户服务不可用，改用后台进程方式"
    WITH_SERVICE=0
  fi
fi

if [[ "$WITH_SERVICE" == "0" ]]; then
  cat > "$INSTALL_DIR/ctl.sh" <<'EOS'
#!/usr/bin/env bash
# Shell Go 启停脚本（无 systemd 时使用）
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PID_FILE="$DIR/data/shell-go.pid"
LOG="$DIR/data/stdout.log"

alive() { [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; }

case "${1:-}" in
  start)
    if alive; then
      echo "已在运行（PID $(cat "$PID_FILE")）"; exit 0
    fi
    mkdir -p "$DIR/data"
    # 配置路径由安装脚本按 --config / --dir 决定后写死在这里（占位符在下面替换）
    nohup "$DIR/shell-go" -config "__SHELLGO_CONFIG__" >> "$LOG" 2>&1 &
    pid=$!
    # 等一秒确认没立刻退出：直接打印「已启动」会掩盖配置错误导致的启动失败
    sleep 1
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "启动失败，日志尾部（${LOG}）："
      tail -n 20 "$LOG" 2>/dev/null || true
      rm -f "$PID_FILE"
      exit 1
    fi
    echo "$pid" > "$PID_FILE"
    echo "已启动（PID ${pid}），日志：${LOG}"
    ;;
  stop)
    if alive; then
      kill "$(cat "$PID_FILE")" 2>/dev/null || true
      rm -f "$PID_FILE"
      echo "已停止"
    else
      rm -f "$PID_FILE"
      echo "未在运行"
    fi
    ;;
  restart) "$0" stop; sleep 1; "$0" start ;;
  status)
    if alive; then
      echo "运行中（PID $(cat "$PID_FILE")）"
    else
      echo "未运行"
    fi
    ;;
  logs) tail -n "${2:-50}" "$LOG" ;;
  *) echo "用法：$0 {start|stop|restart|status|logs}"; exit 1 ;;
esac
EOS
  chmod +x "$INSTALL_DIR/ctl.sh"
  # ctl.sh 用单引号 heredoc 生成（里面全是运行时才求值的 $），
  # 配置路径只能事后替换，否则会被当成运行时变量展开成空串。
  sed -e "s|__SHELLGO_CONFIG__|$(esc "$CONFIG_PATH")|" \
    "$INSTALL_DIR/ctl.sh" > "$INSTALL_DIR/ctl.sh.tmp"
  mv "$INSTALL_DIR/ctl.sh.tmp" "$INSTALL_DIR/ctl.sh"
  chmod +x "$INSTALL_DIR/ctl.sh"
  if [[ "$NO_SERVICE" == "1" ]]; then
    STARTED_OK=1
    c_info "未启动服务，需要时执行：$INSTALL_DIR/ctl.sh start"
  else
    "$INSTALL_DIR/ctl.sh" start || true
    verify_start "$INSTALL_DIR/data/stdout.log" \
      "确认 ${INSTALL_DIR}/${APP} 有执行权限，且没被安全软件拦截" \
      || STARTED_OK=0
  fi
fi

echo
if [[ "$STARTED_OK" == "1" ]]; then
  c_ok "安装完成"
else
  c_err "安装文件已就位，但服务未启动成功，请按上面的提示排查"
fi

if [[ "${CREATED_CONFIG:-0}" == "1" ]]; then
  echo "  账号：admin"
  echo "  密码：$PASSWORD"
  echo "         （已写入 ${CONFIG_PATH} 的 bcrypt 哈希，请立即记录，之后无法找回）"
fi
HOST_IP="$(hostname -I 2>/dev/null | awk '{print $1}' || true)"
if [[ "$EFF_BIND" == "127.0.0.1" || "$EFF_BIND" == "localhost" ]]; then
  echo "  访问：http://127.0.0.1:${EFF_PORT} （仅本机，需经反向代理对外）"
else
  echo "  访问：http://${HOST_IP:-127.0.0.1}:${EFF_PORT}"
fi
echo "  配置：$CONFIG_PATH"
echo "  日志：$INSTALL_DIR/data/stdout.log"
if [[ "$WITH_SERVICE" == "1" ]]; then
  echo "  管理：systemctl --user {status|restart|stop} shell-go"
else
  echo "  管理：$INSTALL_DIR/ctl.sh {start|stop|restart|status|logs}"
fi
echo
c_warn "安全提示"
echo "  1. 请在 ${CONFIG_PATH} 的 security.ip_whitelist 中限制来源网段"
echo "  2. 若需开机自启（systemd 用户服务），执行：loginctl enable-linger \$USER"
echo "  3. 需要 HTTPS 时把 tls.enabled 设为 true（默认自动生成自签证书）"
