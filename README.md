# Shell Go

面向内网服务器的**网页版 Linux 终端**。单文件 Go 二进制，前端资源内嵌，不需要 Java / Node / Python，也不依赖 glibc（`CGO_ENABLED=0` 静态编译），在麒麟 V10 等国产系统的 x86_64 与 aarch64 上可直接运行。

- 登录后才能使用，账号密码来自配置文件或启动参数
- **账号分级**：`admin` / `operator` / `viewer` 三种角色，可逐项微调；只读审计账号不能开终端，
  运维账号看不到别人的会话录制
- 全链路国密：SM2 交换密钥 → SM4-CBC 加密报文 → SM3-HMAC 校验完整性，带时间戳与随机数防重放
- 服务端真 PTY，支持 `cd`、`vim`、`top`、`sudo` 交互、Tab 补全、256 色
- 多标签窗口、**标签内分屏（左右 / 上下 / 四宫格）**、搜索、明暗主题、字号缩放、断线重连与会话保活
- **文件管理**：网页内浏览、上传（分块、大文件**断点续传**）、下载（失败自动重试）、新建、重命名、删除，
  以及**在线文本编辑**（原子写入 + 冲突检测），路径限制在配置的根目录内
- **会话回放**：终端输出按时间轴录制到服务端，可在网页里按原始节奏回放（含 1×/2×/4×/8× 变速）
- 每次操作写入审计日志

> 详细设计见 [docs/01-设计说明.md](docs/01-设计说明.md)，安全边界见 [docs/03-安全说明.md](docs/03-安全说明.md)。

---

## 一、快速开始

### 1. 编译

```bash
# 本机调试版本
make build

# 交叉编译 linux/amd64 + linux/arm64，并生成发布包
./build.sh
```

产物：

```
dist/shell-go-1.5.0-linux-amd64.tar.gz
dist/shell-go-1.5.0-linux-arm64.tar.gz
```

编译只需 Go 1.22+，无需 Node。前端 xterm.js 与 sm-crypto 已作为静态资源入库（`web/js/vendor/`）。

### 2. 本地试跑

```bash
cp config.example.yaml config.yaml
# 修改 users 中的账号密码
./shell-go -config config.yaml
```

浏览器打开 `http://服务器IP:8443`，用配置的账号登录。

也可以完全不用配置文件：

```bash
./shell-go -listen 0.0.0.0:8443 -user admin -pass '你的密码'
```

### 3. 部署到服务器（普通用户权限即可，**不要用 root**）

```bash
tar xzf shell-go-1.5.0-linux-amd64.tar.gz
cd shell-go-1.5.0-linux-amd64

# 默认：装到 ~/shell-go，配置生成在 ~/shell-go/config.yaml
./install.sh --port 8443

# 指定安装目录：配置跟着走，生成在 /data/shell-go/config.yaml
./install.sh --port 8443 --dir /data/shell-go

# 复用已有配置：不生成、不覆盖，监听地址以该文件的 listen 为准（--port 此时不生效）
./install.sh --config /etc/shell-go.yaml
```

`--dir` 与 `--config` 可以一起用：程序装在 `--dir` 指定的目录，配置读 `--config` 指的那份。

脚本默认把程序装到 `~/shell-go`（**不是解压目录**），生成随机密码并只打印一次，
然后注册 systemd 用户服务；systemd 不可用时退化为 `~/shell-go/ctl.sh` 后台运行。

装完会**自检端口是否真的在监听**：起不来就打印日志末尾和排查顺序，而不是糊一句「安装完成」。
脚本任何一步失败都以非零码退出，可以用 `echo $?` 判断结果。

**为什么装到 `~/shell-go` 而不是解压目录**：

- 解压目录名带版本号（`shell-go-1.5.0-linux-amd64`），升级时路径会变，systemd 服务里写死的路径随即失效
- 解压目录常在 `/tmp` 或下载目录，可能被清理；`/tmp` 还可能挂 `noexec`，二进制根本执行不了
- 装到固定目录后，解压目录可以直接删掉，不留第二份副本

想换位置用 `--dir /data/shell-go`；把 `--dir` 指向解压目录本身会被拒绝并提示原因。

### 配置文件在哪指定？

**安装时决定位置，启动时由脚本引用**——不需要你手工传 `-config`：

| 场景 | 怎么做 |
|---|---|
| 默认（推荐） | 什么都不加，配置生成在 `~/shell-go/config.yaml` |
| 换安装目录 | `--dir /data/shell-go` → 配置随之到 `/data/shell-go/config.yaml` |
| 复用已有配置 | `--config /etc/shell-go.yaml`，脚本不生成新配置、不改你的文件 |

`install.sh` 会把最终路径写进 systemd unit 的 `ExecStart` 和 `ctl.sh` 的启动命令，
所以 `systemctl --user restart shell-go` / `ctl.sh restart` 用的就是你指定的那份配置。

复用已有配置时：

- **不生成、不覆盖**配置文件，也不打印密码（密码以你文件里的为准）
- `--port` / `--bind` 不生效，监听地址以配置文件里的 `listen` 为准；脚本会按它显示访问地址
- 指定路径不存在会直接报错退出，不会悄悄新建一个空配置

如果你不用这两个脚本、直接跑二进制，则启动时按 `-config <path>` → 环境变量 `SHELLGO_CONFIG`
→ 当前目录 `./config.yaml` 的顺序查找，都没有就用内置默认值（监听 `0.0.0.0:8443`，
但没有账号，等于不可用）。

**不要用 root 安装**：程序默认拒绝以 root 身份运行——它会在网页上暴露一个可执行任意命令的
shell，以 root 跑等于把整机交给任何拿到网页账号的人。脚本检测到 root 会直接停下，并给出用
普通用户安装的命令。确需在隔离测试机上以 root 运行，加 `--allow-root`（会把 `security.allow_root`
写成 `true`）。

`--allow-root` 对**新建配置和复用已有配置一视同仁**：配置里已有 `allow_root` 就改它的值，
没有这一项就插到 `security:` 段首行，整份配置里连 `security:` 都没有则在文件末尾补一段。
这样「配置是上次装的、这次只想升个版本」的场景不会出现「服务注册成功但端口不监听」——
那种情形下旧版脚本把 `--allow-root` 丢了，程序照旧拒绝启动，`Restart=on-failure`
每 3 秒重启一次，日志里会反复刷同一段 `检测到以 root 身份运行`。

开机自启需要执行一次：

```bash
loginctl enable-linger $USER
```

**安装后端口没起来怎么查**（按顺序）：

```bash
~/shell-go/ctl.sh status                            # 或 systemctl --user status shell-go
tail -n 50 ~/shell-go/data/stdout.log
~/shell-go/shell-go -config ~/shell-go/config.yaml   # 前台跑一次，直接看报错
ss -lntp | grep 8443                                # 确认端口是否被别的进程占用
```

详细部署与运维见 [docs/02-部署运维.md](docs/02-部署运维.md)。

---

## 二、配置说明

配置优先级：**默认值 < 配置文件 < 命令行参数 / 环境变量**。

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `listen` | `0.0.0.0:8443` | 监听地址，建议配合 IP 白名单 |
| `tls.enabled` | `false` | 是否启用 HTTPS |
| `tls.auto` | `true` | 无证书时自动生成自签证书（仅内存） |
| `users[].username` | — | 登录账号，可配置多个 |
| `users[].password` | — | 明文密码，启动时 bcrypt 哈希后仅存内存 |
| `users[].password_hash` | — | bcrypt 串，推荐；用 `-gen-hash '密码'` 生成 |
| `users[].role` | `operator` | `admin` / `operator` / `viewer`，见下文「账号角色与权限」 |
| `users[].perms.*` | 沿用角色 | 逐项覆盖：`shell`、`files_read`、`files_write`、`files_delete`、`records_all`、`records_delete`、`max_terms` |
| `security.ip_whitelist` | `[]` | 来源 IP / CIDR 白名单，为空表示不限制 |
| `security.idle_timeout` | `30m` | 无操作超时 |
| `security.session_ttl` | `12h` | 会话绝对有效期 |
| `security.max_sessions` | `5` | 同时在线会话数上限 |
| `security.max_terms` | `8` | 每个账号可开终端数（可被 `users[].perms.max_terms` 覆盖） |
| `security.login_max_fail` | `5` | 连续失败次数，超过后按 IP 锁定 |
| `security.login_lock` | `10m` | 锁定时长 |
| `security.allow_root` | `false` | 是否允许以 root 运行（默认拒绝） |
| `terminal.shell` | `/bin/bash` | 启动的 shell |
| `terminal.args` | `["-l"]` | shell 参数 |
| `terminal.cwd` | 空 | 工作目录，空为家目录，支持 `~` |
| `terminal.env` | `TERM=xterm-256color`；`LANG` 继承启动环境，未设置时补 `C.UTF-8` | 追加/覆盖环境变量 |
| `terminal.scrollback` | `524288` | 服务端回滚缓冲字节数 |
| `terminal.transport` | `ws` | `ws` 实时通道 / `poll` 纯 POST 轮询 |
| `terminal.poll_interval_ms` | `120` | 轮询模式建议间隔 |
| `terminal.idle_close` | `30m` | 无人连接且无输出的终端回收时间 |
| `terminal.snapshot_max` | `131072` | 重连回放最大字节数 |
| `files.enabled` | `true` | 是否启用网页文件管理 |
| `files.root` | `~` | 文件管理可访问的根目录（逻辑边界，**与 Linux 的 root 用户无关**），支持 `~` 与相对路径 |
| `files.max_upload_mb` | `200` | 单文件上传上限 |
| `files.max_edit_kb` | `1024` | 网页内在线编辑的文本大小上限 |
| `files.allow_write` | `true` | 允许上传 / 新建 / 重命名（与账号权限取交集） |
| `files.allow_delete` | `true` | 允许删除（不可恢复；与账号权限取交集） |
| `record.enabled` | `true` | 是否录制终端会话用于回放 |
| `record.dir` | `data/records` | 录制文件目录（JSONL） |
| `record.max_mb` | `50` | 单个会话录制上限，超出后停止录制 |
| `record.keep_days` | `7` | 保留天数，0 表示不按时间清理 |
| `record.max_files` | `200` | 最多保留文件数，0 表示不限制 |
| `audit.enabled` | `true` | 是否记录审计日志 |
| `audit.log_file` | `data/audit.log` | 审计日志路径 |
| `log.file` | `data/server.log` | 运行日志路径 |

命令行参数：

```
-config      配置文件路径（默认 ./config.yaml）
-listen      监听地址
-user        用户名（覆盖配置）
-pass        密码（覆盖配置）
-pass-hash   bcrypt 密码哈希
-allow-root  允许 root 运行
-gen-hash    生成 bcrypt 哈希后退出
-init-config 由 config.example.yaml 生成 config.yaml
-version     版本信息
```

环境变量：`SHELLGO_CONFIG`、`SHELLGO_USER`、`SHELLGO_PASSWORD`。

> 密码写在命令行会出现在 `ps` 输出里，生产环境建议用 `password_hash` 或环境变量。

### 账号角色与权限

三个角色覆盖了大多数分工，需要更细时用 `perms` 逐项改：

| 能力 | `admin` | `operator`（默认） | `viewer` |
|---|---|---|---|
| 新建终端、执行命令 | ✓ | ✓ | ✗ |
| 浏览 / 下载文件 | ✓ | ✓ | ✓ |
| 上传 / 编辑 / 重命名文件 | ✓ | ✓ | ✗ |
| 删除文件 | ✓ | ✓ | ✗ |
| 查看**所有人**的会话录制 | ✓ | ✗（仅自己） | ✓ |
| 删除会话录制 | ✓ | ✓（仅自己） | ✗ |

```yaml
users:
  - username: admin
    password_hash: "$2a$10$..."
    role: admin
  - username: auditor            # 审计岗：能看全部回放，不能动手
    password_hash: "$2a$10$..."
    role: viewer
  - username: ops                # 运维岗：额外允许查看所有人的录制
    password_hash: "$2a$10$..."
    role: operator
    perms:
      records_all: true
      max_terms: 4               # 这个人只给 4 个并发终端
```

几条约定：

- **`perms` 里没写的项沿用角色默认值**，写 `false` 才是显式关闭。所以「给 admin 但禁止开终端」
  只需 `perms: { shell: false }`，其他权限不变。
- 文件类的 `files.allow_write` / `files.allow_delete` 是**全局闸门**，与账号权限取交集：
  全局关了谁都写不了，全局开着但账号是 `viewer` 也写不了。
- 用 `-user/-pass` 命令行启动时创建的引导账号按 `admin` 处理，否则服务起来连终端都开不了。
- 角色在**启动时**读取，改配置要重启服务；已登录的会话在下次登录时拿到新角色。

---

## 三、使用

| 操作 | 快捷键 |
|---|---|
| 新建终端 | `Ctrl+Shift+T` |
| 左右分屏 | `Ctrl+Shift+D`，或点工具栏分屏图标 |
| 上下分屏 | `Ctrl+Shift+S` |
| 四宫格 | `Ctrl+Shift+G`，或点工具栏宫格图标 |
| 在窗格间切换 | `Ctrl+Shift+[` / `Ctrl+Shift+]`，或直接点窗格 |
| 按方向切换窗格 | `Ctrl+Shift+←/→/↑/↓` |
| 关闭当前窗格（只剩一个时关标签） | `Ctrl+Shift+W` |
| 切换标签 | `Ctrl+Tab` / `Ctrl+Shift+Tab` |
| 搜索终端内容 | `Ctrl+F` |
| 复制选中内容 | `Ctrl+Shift+C` |
| 粘贴 | `Ctrl+Shift+V`（或直接 `Ctrl+V`） |
| 调整字号 | `Ctrl+=` / `Ctrl+-` |
| 重命名标签 | 双击标签 |
| 打开文件管理 | `Ctrl+Shift+E`，或点工具栏文件夹图标 |
| 打开会话回放 | 点工具栏时钟图标 |

其它行为：

- 标签可以中键关闭，右键终端弹出「复制 / 粘贴 / 全选 / 清屏 / 搜索 / 关闭标签」
- 刷新页面或断网重连后，会自动回放最近约 128KB 输出并继续会话
- 关掉标签才会结束对应的 shell 进程；只关闭浏览器标签页时，服务端会话会保活一段时间（默认 30 分钟空闲回收）
- 一个标签最多四个窗格：2 个时按你选的方向排列，3 个时上二下一（最后一个横跨整行），4 个即四宫格
- 每个窗格是独立会话，各自有 PTY、WebSocket 和录制，命令在哪个窗格执行取决于焦点；非焦点窗格照样实时刷新
- 关掉窗格后布局会自动回退：4→3 仍是宫格，3→2 变回左右分屏
- 非 HTTPS 环境下浏览器不允许脚本读取剪贴板，此时 `Ctrl+Shift+V` 会弹出输入框供手动粘贴
- 文件管理支持双击目录进入；双击文件直接打开编辑器（只读账号则改为下载）
- **打开文件管理会直接定位到当前窗格终端的目录**：切换窗格/标签后遵循焦点所在的那个；
  该目录不在 `files.root` 之内时，弹窗会提示实际路径并退回上次浏览的位置
- **上传、新建、改名、删除完成后列表自动刷新**，不用再手动点「刷新」
- 顶部过滤框按名称筛选当前目录，不会递归搜索子目录
- 上传按 256KB 分块走加密 POST，进度显示在弹窗底部；重复上传同名文件时，若已传部分与本地文件开头一致，
  会从断点继续而不是从头再来；内容不一致则提示覆盖
- 在线编辑用 `Ctrl+S` 保存、`Esc` 关闭（`Tab` 插入两格缩进）；保存是整体覆盖 + 原子替换，
  如果期间有别人（或另一个终端）改过这个文件，会提示冲突并要求重新打开，不会静默覆盖
- 编辑器只处理文本：超过 `files.max_edit_kb` 或含二进制字节的文件会被拒绝，请下载后用本地编辑器处理
- 会话回放只录输出（按键由 shell 回显），按录制时的真实节奏播放；播放器是只读的，不会连接真实终端

---

## 四、安全模型

```
浏览器                              服务端
  │  POST /api/handshake  ─────────►  生成临时 SM2 密钥对，返回 sid + 公钥
  │  POST /api/login      ─────────►  SM2 解出会话密钥 → 校验 bcrypt 密码 → 返回加密 token
  │  POST /api/xxx (密文)  ─────────►  每条请求独立 IV + HMAC-SM3 校验 + 时间戳/nonce 防重放
  │  GET  /ws?ticket=...  ─────────►  一次性票据升级（唯一非 POST 的接口）
```

要点：

1. **应用层国密不等于链路加密**。没有 TLS 时，中间人仍可替换握手公钥做全量解密。内网可信环境可接受，跨网段建议开启 `tls.enabled`。
2. 工具本质是「把能执行任意命令的 shell 暴露在网页上」。谁能登录，谁就拥有**运行该二进制的那个系统用户**的全部权限。默认拒绝 root 运行，需要显式 `-allow-root`。
3. 建议始终配置 `ip_whitelist`，并按需开启 HTTPS、调小 `idle_timeout`。
4. WebSocket 升级协议本身规定必须用 GET，因此 `/ws` 是唯一例外，且必须携带由 POST 接口签发的、30 秒有效的一次性票据。

---

## 五、目录结构

```
shell-go/
├─ main.go                    启动、配置装载、TLS、优雅退出
├─ internal/
│  ├─ config/                 YAML 配置与校验
│  ├─ crypto/                 SM2/SM4/SM3 信封（含跨语言单测）
│  ├─ auth/                   账号、令牌、失败锁定
│  ├─ terminal/               PTY 会话、环形缓冲、会话管理、会话录制
│  ├─ files/                  受限目录的文件读写（防路径与符号链接逃逸）
│  ├─ server/                 HTTP/WS 路由、信封解析、限流
│  └─ audit/                  JSONL 审计日志
├─ web/                       内嵌前端（xterm.js、sm-crypto、原生 ES Module）
├─ docs/                      设计、部署、安全、协议文档
├─ deploy/                    systemd 单元与安装脚本
├─ build.sh / Makefile        打包
└─ test/                      跨语言与端到端协议测试脚本
```

---

## 六、测试

```bash
# 单元测试（信封往返、篡改检测）
go test ./...

# 跨语言互通校验（需要 node 与 sm-crypto 的 dist 目录）
SMCRYPTO_PATH=/path/to/sm-crypto/dist/sm2.js \
SMCRYPTO_DIR=/path/to/sm-crypto/dist \
go test ./internal/crypto/ -v

# 端到端协议测试（服务需已启动）
BASE=http://127.0.0.1:18443 USER_NAME=admin USER_PASS=xxx \
SMCRYPTO_DIR=/path/to/sm-crypto/dist node test/e2e.mjs

# 追加文件管理与录制回放的协议用例
BASE=http://127.0.0.1:18443 USER_NAME=admin USER_PASS=xxx \
SMCRYPTO_DIR=/path/to/sm-crypto/dist E2E_FILES=1 E2E_RECORD=1 node test/e2e.mjs

# 追加多账号权限用例（需配置一个 operator 与一个 viewer 账号）
BASE=http://127.0.0.1:18443 USER_NAME=admin USER_PASS=xxx \
E2E_OPS=ops E2E_VIEWER=auditor E2E_PASS=xxx \
SMCRYPTO_DIR=/path/to/sm-crypto/dist E2E_PERMS=1 node test/e2e.mjs
```

跨语言测试验证的是「浏览器加密 → Go 解密」「Go 加密 → 浏览器解密」两个方向，
避免国密实现细节（如 SM2 密文的 `04` 前缀、HMAC 分组长度）不一致导致线上才暴露问题。

---

## 七、常见问题

**Q：中文显示成问号或方块？**
程序会继承启动环境的 `LANG`；只有在完全没有设置时才补 `C.UTF-8`。如果部署机是 POSIX locale，
在配置里显式指定，例如 `terminal.env.LANG: zh_CN.UTF-8`（前提是该 locale 已生成，`locale -a` 可查）。

**Q：浏览器提示「连接失败」或握手报错？**
检查服务器与浏览器时间是否相差超过 5 分钟，报文时间戳窗口为 ±300 秒。

**Q：为什么刷新页面后要重新登录？**
不需要。会话密钥存在 `sessionStorage`（按标签页隔离），刷新会复用；只有关闭浏览器标签页才需要重新登录。

**Q：终端里 `top` / `vim` 显示错乱？**
先确认 `terminal.env.TERM` 为 `xterm-256color`，并检查窗口尺寸是否被压缩过小；服务端会拒绝小于 20 列的尺寸。

**Q：麒麟系统上中文显示为方块？**
系统缺少等宽中文字体。安装 `fonts-wqy-microhei` 或 `wqy-zenhei` 即可，前端字体栈已内置回退。

**Q：运维账号看不到别人的会话回放？**
这是默认行为：`operator` 只能看自己创建的录制，看全部需要 `admin`，或给该账号单独开
`perms.records_all: true`。只读审计账号（`viewer`）反过来——能看全部但不能开终端。

**Q：文件管理里看不到某些目录 / 提示路径超出范围？**
所有路径都被限制在 `files.root` 之下，`..` 与指向目录外的符号链接会被拒绝。需要访问其他位置时，把 `files.root` 改成它们的共同上级，或用符号链接（同样必须指向 root 内）。

**Q：回放是空的 / 只有提示符？**
录制只保存输出，若会话里没有执行命令，文件里就只有登录提示符。另外 `record.enabled: false` 或刚打开的录制尚未关闭时，列表里会显示「录制中」。

**Q：在线编辑保存时提示「文件在你编辑期间已被修改」？**
说明打开编辑器之后，这个文件被其他会话或进程改过。服务端不会覆盖别人的改动，重新打开编辑即可看到最新内容。
如果确认自己的版本才要，可以关闭后重新打开、再把内容粘回去保存。

**Q：上传大文件到一半断了怎么办？**
重新选择同一个文件上传即可。前端会先读服务端已有内容的前 32KB 与本地文件比对，一致就从断点继续，
不一致（说明不是同一个文件）才会提示覆盖重传。

**Q：WAF/安全设备禁止 WebSocket 怎么办？**
把 `terminal.transport` 改成 `poll`，全站即可只用 POST（延迟约 100~200ms）。

---

## 八、变更记录

见 [CHANGELOG.md](CHANGELOG.md)。
