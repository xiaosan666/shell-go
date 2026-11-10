# API 协议

所有接口均为 `POST`，请求与响应体为 JSON，`Content-Type: application/json`。
除 `/api/handshake` 外，业务载荷均为国密信封加密。

## 1. 通用约定

### 1.1 信封

```json
{
  "sid": "32位hex会话ID",
  "data": "base64( IV(16) | 密文 | MAC(32) )"
}
```

解密后的明文载荷：

```json
{
  "ts": 1700000000,
  "nonce": "16字节hex，随机，会话内不重复",
  "token": "登录令牌，登录接口为空",
  "p": { }
}
```

响应体：

```json
{ "sid": "...", "data": "base64(...)" }
```

解密后：

```json
{ "code": 0, "msg": "", "data": { } }
```

### 1.2 错误码

| code | 含义 | 前端处理 |
|---|---|---|
| 0 | 成功 | — |
| 1 | 参数错误 | 提示 `msg` |
| 2 | 未登录/会话过期 | 跳转登录页 |
| 3 | 服务端内部错误 | 提示 `msg` |
| 4 | 业务错误（如终端数超限） | 提示 `msg` |

传输层错误（无法解密、时间戳过期、sid 不存在）返回 HTTP 4xx 与明文 `{"error":"..."}`。

### 1.3 通用错误

| HTTP | error | 原因 |
|---|---|---|
| 400 | 请求体解析失败 | JSON 格式非法 |
| 400 | 报文解密失败 | MAC 校验失败或密钥不匹配 |
| 400 | 请求已过期，请检查服务器与浏览器时间 | 时间戳超出 ±300s |
| 400 | 重复请求被拒绝 | nonce 重复 |
| 401 | 会话已失效，请刷新页面 | sid 不存在或已回收 |
| 403 | forbidden | 来源 IP 不在白名单 |
| 429 | 握手请求过于频繁 | 超过 60 次/分钟/IP |

## 2. 接口列表

| 路径 | 说明 | 需要登录 |
|---|---|---|
| `POST /api/handshake` | 获取 sid、SM2 公钥、服务器时间 | 否 |
| `POST /api/login` | 登录，建立会话密钥 | 否 |
| `POST /api/session` | 校验登录态 | 是 |
| `POST /api/logout` | 退出登录 | 是 |
| `POST /api/sysinfo` | 主机、进程用户、在线数等 | 是 |
| `POST /api/term/create` | 新建终端 | 是 |
| `POST /api/term/list` | 列出终端 | 是 |
| `POST /api/term/close` | 关闭终端 | 是 |
| `POST /api/term/rename` | 重命名 | 是 |
| `POST /api/term/resize` | 调整尺寸 | 是 |
| `POST /api/term/input` | 写入输入（轮询模式） | 是 |
| `POST /api/term/poll` | 拉取输出（轮询模式） | 是 |
| `POST /api/term/snapshot` | 拉取快照 | 是 |
| `POST /api/term/ticket` | 签发 WebSocket 一次性票据 | 是 |
| `POST /api/term/cwd` | 终端 shell 的当前目录（用于文件管理定位） | 是 |
| `POST /api/file/list` | 浏览目录 | 是 |
| `POST /api/file/read` | 分块读取文件 | 是 |
| `POST /api/file/write` | 分块写入文件 | 是 |
| `POST /api/file/mkdir` | 新建目录 | 是 |
| `POST /api/file/rename` | 重命名 | 是 |
| `POST /api/file/remove` | 删除文件/目录 | 是 |
| `POST /api/file/stat` | 单文件元信息（大小、纳秒版本号） | 是 |
| `POST /api/file/save` | 在线编辑整文件保存（带并发校验） | 是 |
| `POST /api/record/list` | 列出会话录制（按账号权限过滤） | 是 |
| `POST /api/record/get` | 分页读取录制事件 | 是 |
| `POST /api/record/delete` | 删除录制 | 是 |
| `GET /ws?ticket=` | WebSocket 升级 | 票据 |

## 3. 接口详情

### 3.1 POST /api/handshake

请求：`{}`（明文）

响应（明文）：

```json
{
  "sid": "a1b2...",
  "pub": "04...130位hex",
  "ts": 1700000000,
  "alg": "SM2/SM4-CBC/SM3-HMAC",
  "version": "1.0.0 (abc1234)"
}
```

`pub` 为未压缩 SM2 公钥（`04‖X‖Y`）。sid 未用于登录时 5 分钟后回收。

### 3.2 POST /api/login

请求：

```json
{
  "sid": "...",
  "k": "04‖C1C3C2 的 hex，明文为 16 字节会话密钥",
  "data": "base64(信封，会话密钥加密)"
}
```

信封内明文：

```json
{ "ts": 1700000000, "nonce": "...", "token": "", "p": { "u": "admin", "p": "密码" } }
```

响应（会话密钥加密）：

```json
{
  "code": 0,
  "data": {
    "token": "64位hex",
    "expire": 1700036000,
    "idle": 1800,
    "user": "admin",
    "transport": "ws",
    "pollMs": 120,
    "title": "Shell Go",
    "maxTerms": 8,
    "caps": {
      "role": "admin",
      "shell": true,
      "filesRead": true,
      "filesWrite": true,
      "filesDelete": true,
      "recordsAll": true,
      "recordsDelete": true,
      "maxTerms": 8
    }
  }
}
```

`caps` 是该账号的最终能力集合，前端据此隐藏无权限的入口。**它只是界面提示**：
服务端对每个接口独立校验，改前端也提不了权。`/api/session` 返回同样的 `caps`，
用于刷新页面后按最新角色渲染。

### 3.3 POST /api/term/create

请求：`{"cols":148,"rows":38}`

响应：`{"id":"...","title":"","snapshot":"base64","cursor":123}`

`snapshot` 为最近 `snapshot_max` 字节输出，`cursor` 为对应绝对偏移。

### 3.4 POST /api/term/poll

请求：`{"term":"...","cursor":123,"wait":600}`

响应：

```json
{ "cursor": 4096, "data": "base64", "reset": false, "closed": false }
```

`reset=true` 表示游标已滚出缓冲，应清屏后以本次 `data` 作为全量。

### 3.5 POST /api/term/ticket

请求：`{"term":"..."}` → 响应：`{"ticket":"48位hex"}`

票据一次性、30 秒有效，绑定当前用户与终端。

### 3.5.1 POST /api/term/cwd

请求：`{"term":"..."}` → 响应：

```json
{"abs":"/home/ops/conf","inside":true,"rel":"conf"}
```

`abs` 是 shell 进程的工作目录（Linux 上读 `/proc/<pid>/cwd`，shell 每次 `cd` 内核即更新，
不往 PTY 写命令、不解析提示符）。`inside` 表示该目录是否在 `files.root` 之内，
是则给出相对路径 `rel`；否则 `rel` 为空，前端只把 `abs` 显示给用户并退回上次浏览的位置。

嵌套环境（在 shell 里再 `ssh` / `docker exec` 到别的机器）拿到的是本机这一层 shell 的目录，
远端目录无从得知。此接口需要文件浏览权限。

### 3.6 GET /ws?ticket=...

二进制帧，内容为信封。明文为 JSON 控制帧：

| `t` | 方向 | 字段 | 说明 |
|---|---|---|---|
| `s` | S→C | `d`,`c` | 快照与游标 |
| `o` | S→C | `d` | 输出 |
| `e` | S→C | — | shell 退出 |
| `p` | S→C | — | 心跳 |
| `q` | S→C | `c` | 心跳回显（用于测延迟） |
| `i` | C→S | `d` | 输入字节 |
| `r` | C→S | `x`,`y` | 列、行 |
| `p` | C→S | `c` | 心跳请求 |

服务端每 25 秒发送一次 WebSocket Ping 与 `p` 帧；90 秒未收到 Pong 视为断线。
订阅者积压超过 256 条消息时以 `1013` 关闭，客户端重连后重新取快照。

### 3.7 文件管理

请求参数统一使用相对 `files.root` 的路径（`""` 表示根目录），`..` 会被归一化，
解析符号链接后再校验是否仍在根目录内。

**list** 请求 `{"path":""}`，响应：

```json
{
  "path": "sub",
  "parent": "",
  "root": "/home/ops",
  "entries": [
    {"name":"run.sh","path":"sub/run.sh","dir":false,"size":123,"mode":"-rw-r--r--","mtime":1700000000}
  ],
  "writable": true,
  "deletable": true,
  "maxUpload": 209715200
}
```

**read** 请求 `{"path":"sub/run.sh","offset":0,"size":524288}`，响应：

```json
{ "data": "base64", "offset": 524288, "eof": false, "size": 734001 }
```

**write** 请求 `{"path":"a.txt","offset":0,"total":9,"data":"base64"}`，响应：

```json
{ "offset": 9, "done": true }
```

`offset` 必须等于服务端当前文件长度（首块为 0 会截断重建），否则返回「上传偏移不连续」。
`total` 超过 `files.max_upload_mb` 直接拒绝。

**mkdir** `{"path":"logs"}`；**rename** `{"path":"a.txt","name":"b.txt"}` → `{"path":"b.txt"}`；
**remove** `{"path":"a.txt","recursive":false}`。

**stat** 请求 `{"path":"a.txt"}`，响应：

```json
{
  "entry": {"name":"a.txt","path":"a.txt","dir":false,"size":9,"mode":"-rw-r--r--","mtime":1700000000},
  "version": "1700000000123456789",
  "maxEdit": 1048576
}
```

`version` 是纳秒级修改时间的**字符串**。用字符串是因为 19 位整数超过了
JavaScript 的 `Number.MAX_SAFE_INTEGER`，走 JSON 数字会在浏览器端丢精度，
导致保存时永远判定为冲突。

**save** 请求 `{"path":"a.txt","version":"1700000000123456789","data":"base64"}`，响应：

```json
{ "size": 12, "version": "1700000000987654321" }
```

- `version` 必须等于服务端当前 mtime，否则返回「文件在你编辑期间已被修改」——这是乐观并发控制，
  防止两个人同时编辑时后保存的把前一个覆盖掉。
- `version` 传空串或 `"0"` 表示新建：若文件已存在则同样返回冲突，不会静默覆盖。
- 内容上限 `files.max_edit_kb`；含 NUL 的二进制内容直接拒绝。
- 写入走「同目录临时文件 + rename」原子替换，中途失败不会留下写了一半的文件，权限位沿用原文件。

### 3.8 会话录制

**list** 请求 `{"limit":200}`，响应：

```json
{
  "enabled": true,
  "records": [
    {"id":"20261109-184332_bccbd4283f9b7307","term":"bccbd4283f9b7307","user":"admin",
     "start":1700000000,"cols":148,"rows":38,"size":1678,"end":2100}
  ]
}
```

`end` 为毫秒时长，0 表示录制仍在进行。

**get** 请求 `{"id":"...","from":0,"count":500}`，响应：

```json
{
  "meta": { "...": "同上" },
  "events": [ {"t": 120, "d": "base64(输出字节)"} ],
  "next": 500,
  "eof": false
}
```

`t` 为相对录制开始的毫秒数，前端按此时间轴回放；`eof=false` 时以 `next` 继续拉取。

**delete** 请求 `{"id":"..."}`。ID 必须匹配 `^\d{8}-\d{6}_[0-9a-f]{8,32}$`。

## 4. 请求示例（curl）

握手是无状态明文接口，可直接验证连通性：

```bash
curl -s -X POST http://127.0.0.1:8443/api/handshake \
  -H 'Content-Type: application/json' -d '{}'
```

其余接口需要先完成 SM2/SM4 握手，推荐直接用 `test/e2e.mjs` 作为协议参考实现。
