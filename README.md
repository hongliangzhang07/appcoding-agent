# desktop-agent-go

桌面端插件（Go + PTY + WebSocket + 二维码配对鉴权）。

目标：安装后启动插件会显示二维码，手机 App 扫码即可与当前电脑配对，随后手机可远程控制本机 `claude` / `codex` CLI。

## 功能

- WebSocket 控制通道：`/ws`
- PTY 终端会话：保证 CLI 交互能力（中断/提示符/流式输出）
- 二维码配对鉴权（推荐）
- 设备 token 持久化（配对后免扫码重连）
- 会话控制：启动、输入、Ctrl+C、resize、停止

## 一条命安装（你的仓库）

```bash
curl -fsSL https://raw.githubusercontent.com/hongliangzhang07/appcoding-agent/main/install/install.sh | bash
```

安装后常用命令：

```bash
appcoding-agentctl status
appcoding-agentctl pairing
appcoding-agentctl tunnel-status
appcoding-agentctl logs
```

卸载：

```bash
curl -fsSL https://raw.githubusercontent.com/hongliangzhang07/appcoding-agent/main/install/uninstall.sh | bash
```

## 运行

```bash
cd /Users/zhanghongliang/Desktop/apple_app_project/desktop-agent-go
go mod tidy
go run .
```

## 发布二进制（给 install.sh 下载）

打包 4 平台产物并生成校验：

```bash
cd /Users/zhanghongliang/Desktop/apple_app_project/desktop-agent-go
./scripts/release-pack.sh
```

输出目录默认 `dist/`，产物名：

- `appcoding-agent_darwin_amd64.tar.gz`
- `appcoding-agent_darwin_arm64.tar.gz`
- `appcoding-agent_linux_amd64.tar.gz`
- `appcoding-agent_linux_arm64.tar.gz`
- `checksums.txt`

然后把这些文件上传到 GitHub Release（tag 可用 `v0.1.0`）。

也可以一条命发布（需要先安装并登录 `gh` CLI）：

```bash
cd /Users/zhanghongliang/Desktop/apple_app_project/desktop-agent-go
./scripts/publish-gh-release.sh v0.1.0
```

## 启动后你会看到什么

1. 终端打印二维码（ASCII）
2. 同时生成二维码图片文件（默认）：
   - `~/.desktop-agent-go/pairing-qr.png`
3. App 扫码后发起 `pair_request` 即可完成绑定

## 环境变量

- `AGENT_ADDR`：监听地址，默认 `:8088`
- `AGENT_DEFAULT_CWD`：默认工作目录
- `AGENT_STATE_PATH`：状态文件路径，默认 `~/.desktop-agent-go/state.json`
- `AGENT_QR_PATH`：二维码图片路径，默认 `~/.desktop-agent-go/pairing-qr.png`
- `AGENT_PAIR_TTL_SEC`：配对码有效期秒数，默认 `600`
- `AGENT_PUBLIC_HOST`：二维码里给 App 的主机地址（不填则自动探测局域网 IP）
- `AGENT_PUBLIC_PORT`：二维码里给 App 的端口（默认从 `AGENT_ADDR` 推导）
- `AGENT_WS_SCHEME`：`ws` / `wss`，默认 `ws`
- `AGENT_QR_LOG`：`1` 打印二维码到终端，`0` 不打印
- `AGENT_TOKEN`：可选，保留的旧版 token 鉴权（不推荐作为主流程）
- `AGENT_TUNNEL_AUTOSTART`：`1` 启动时自动拉起 Cloudflare Quick Tunnel（默认 `0`）
- `AGENT_TUNNEL_BIN`：`cloudflared` 可执行文件名或路径（默认 `cloudflared`）
- `AGENT_TUNNEL_TARGET_URL`：Quick Tunnel 转发目标（默认 `http://127.0.0.1:<AGENT_ADDR端口>`）

## HTTP 接口

- 健康检查：`GET /health`
- 当前配对信息：`GET /pairing`
- 当前二维码 PNG：`GET /pairing/qr`
- Tunnel 状态：`GET /tunnel/status`
- 启动 Tunnel（仅本机回环地址允许）：`POST /tunnel/start`
- 停止 Tunnel（仅本机回环地址允许）：`POST /tunnel/stop`

## 无域名外网访问（Cloudflare Quick Tunnel）

1. 安装 `cloudflared`（电脑端）  
2. 启动后端：`go run .`  
3. 本机执行：`curl -X POST http://127.0.0.1:8088/tunnel/start`  
4. 查看状态：`curl http://127.0.0.1:8088/tunnel/status`  

当 tunnel ready 后：
- `/pairing` 的 `connect_ws_url`/`pairing_ws_url` 会自动切到 `wss://*.trycloudflare.com/ws`
- 二维码内容也会自动带上 `wss` 地址

## WebSocket 协议

### 1) 首次配对（扫码后）

App 扫码拿到内容（JSON），从中读取：
- `pair_code`
- `pairing_ws`

然后连接 `pairing_ws`，发送：

```json
{
  "type": "pair_request",
  "pair_code": "ABCDE-FGHIJ",
  "device_id": "ios-device-uuid",
  "device_name": "Hongliang iPhone"
}
```

成功后服务端返回：

```json
{
  "type": "pair_success",
  "device_id": "ios-device-uuid",
  "device_token": "<long_token>",
  "agent_id": "<agent_id>",
  "message": "device paired successfully"
}
```

App 必须保存 `device_id + device_token`。

### 2) 后续鉴权（免扫码）

```json
{
  "type": "auth",
  "device_id": "ios-device-uuid",
  "token": "<device_token>"
}
```

### 3) 启动会话

```json
{
  "type": "start_session",
  "tool": "claude",
  "args": [],
  "cwd": "/Users/you/work/project",
  "rows": 40,
  "cols": 120
}
```

### 4) 输入

```json
{"type":"input","session_id":"s-xxx","data":"请帮我修复这个bug\n"}
```

### 5) 中断

```json
{"type":"interrupt","session_id":"s-xxx"}
```

### 6) 停止

```json
{"type":"stop_session","session_id":"s-xxx"}
```

### 7) 输出事件

```json
{"type":"output","session_id":"s-xxx","stream":"stdout","data":"..."}
{"type":"session_exit","session_id":"s-xxx","exit_code":0}
{"type":"error","message":"..."}
```

## 安全建议

- 生产环境请使用 `wss`（TLS）。
- 外网访问建议放到反向代理后，并限制来源 IP。
- 保持命令白名单，仅允许 `claude` / `codex`。
