<div align="center">

# TianXing Tunnel

**轻量级 · 零依赖 · 企业级内网穿透解决方案**

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/Platform-Windows%20%7C%20Linux%20%7C%20macOS-lightgrey)]()

</div>

---

## 特性

- **零依赖部署** — 无需复杂环境，单二进制文件即可运行
- **内网可用** — Bootstrap 资源嵌入二进制，Web 管理后台无需外网
- **安全认证** — Token 鉴权 + Web Basic Auth + API Bearer Token 三重认证
- **灵活映射** — 支持单端口映射、端口范围映射、配置文件持久化
- **多端管理** — CLI 命令行 + HTTP API + Web Dashboard 三种管理方式
- **优雅关闭** — 支持 SIGINT/SIGTERM 信号，平滑释放资源
- **自动重连** — 客户端断线自动重连，映射规则自动恢复

## 架构

```
┌─────────────┐         ┌─────────────────────────────┐         ┌─────────────┐
│  External    │  TCP    │       TianXing Server        │  TCP    │  TianXing   │
│  User        │────────▶│  ┌─────────┐  ┌──────────┐  │────────▶│  Client     │
│              │  :PORT  │  │ Control │  │  Proxy   │  │  :7001  │             │
└─────────────┘         │  │ :7000   │  │  :7001   │  │         │  ┌────────┐ │
                        │  └─────────┘  └──────────┘  │         │  │ Local  │ │
                        │  ┌────────────────────────┐  │         │  │ Service│ │
                        │  │   Web Dashboard :4200  │  │         │  └────────┘ │
                        │  └────────────────────────┘  │         │             │
                        └─────────────────────────────┘         └─────────────┘
```

## 快速开始

### 编译

```bash
# 服务端
cd server && go build -ldflags '-s -w -X "main.Version=1.0.0" -X "main.BuildTime=2026-05-26"' -o tianxing-server .

# 客户端
cd client && go build -ldflags '-s -w -X "main.Version=1.0.0" -X "main.BuildTime=2026-05-26"' -o tianxing-client .
```

### 启动

```bash
# 服务端 — 指定配置文件
./tianxing-server /path/to/server.conf

# 客户端 — 指定配置文件
./tianxing-client /path/to/client.conf

# 查看版本
./tianxing-server --version
./tianxing-client version
```

## 配置文件

配置文件采用 `key = value` 格式，`#` 开头为注释。

### 服务端配置

| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `bind_addr` | `0.0.0.0` | 监听地址 |
| `bind_port` | `7000` | 控制连接端口 |
| `web_port` | `4200` | Web 管理后台端口 |
| `web_user` | — | Web 后台用户名（不配置则无需认证） |
| `web_pass` | — | Web 后台密码 |
| `token` | — | 客户端认证令牌（可配置多个） |

### 客户端配置

| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `server_addr` | `127.0.0.1` | 服务端地址 |
| `server_port` | `7000` | 服务端控制端口 |
| `token` | — | 认证令牌（必须与服务端一致） |
| `name` | `default` | 客户端标识名称 |
| `api_port` | `7500` | 本地 API 端口 |
| `api_token` | — | API Bearer Token（为空则不验证） |

### 映射规则

在 `[mapping]` 段下配置，客户端启动时自动注册：

```ini
[mapping]
# 格式1（推荐）: key=value 形式
type=tcp remote_port=8080 local_ip=127.0.0.1 local_port=80
type=tcp remote_port=3306 local_ip=127.0.0.1 local_port=3306

# 格式2: 空格分隔
tcp 6379 127.0.0.1 6379
```

### 完整配置示例

```ini
# ===== 服务端配置 =====
bind_addr = 0.0.0.0
bind_port = 7000
web_port = 4200
web_user = admin
web_pass = your_strong_password
token = my_secret_token_1
token = my_secret_token_2

# ===== 客户端配置 =====
server_addr = 1.2.3.4
server_port = 7000
token = my_secret_token_1
name = office-pc
api_port = 7500
api_token = api_bearer_token_here

# ===== 映射规则 =====
[mapping]
type=tcp remote_port=8080 local_ip=127.0.0.1 local_port=80
type=tcp remote_port=2222 local_ip=127.0.0.1 local_port=22
tcp 3306 127.0.0.1 3306
```

## CLI 命令

客户端支持命令行操作（需要客户端已在运行）：

```bash
# 添加单端口映射
tianxing-client add <远程端口> <本地IP> <本地端口>
tianxing-client add 8080 127.0.0.1 80

# 添加端口范围映射
tianxing-client range <起始端口>-<结束端口> <本地IP>
tianxing-client range 9000-9005 127.0.0.1

# 删除映射
tianxing-client remove <远程端口>
tianxing-client remove 8080

# 列出所有映射
tianxing-client list

# 查看版本
tianxing-client version

# 帮助
tianxing-client help
```

## 客户端 API

客户端运行后提供本地 HTTP API，默认监听 `127.0.0.1:7500`。

配置 `api_token` 后，所有请求需携带 `Authorization: Bearer <token>` 头。

### 接口列表

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/status` | 连接状态 |
| `GET` | `/api/mappings` | 列出映射 |
| `POST` | `/api/mappings` | 添加映射 |
| `POST` | `/api/mappings/range` | 添加端口范围映射 |
| `DELETE` | `/api/mappings/:port` | 删除映射 |
| `GET` | `/api/stats` | 流量统计 |

### 请求示例

```bash
# 查看状态
curl -H "Authorization: Bearer <token>" http://127.0.0.1:7500/api/status

# 添加映射
curl -X POST http://127.0.0.1:7500/api/mappings \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"type":"tcp","remote_port":8080,"local_ip":"127.0.0.1","local_port":80}'

# 端口范围映射
curl -X POST http://127.0.0.1:7500/api/mappings/range \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"type":"tcp","port_start":9000,"port_end":9005,"local_ip":"127.0.0.1"}'

# 删除映射
curl -X DELETE -H "Authorization: Bearer <token>" http://127.0.0.1:7500/api/mappings/8080
```

### 响应示例

**GET /api/status**
```json
{
  "connected": true,
  "server": "1.2.3.4:7000",
  "name": "office-pc",
  "mapping_count": 3
}
```

**POST /api/mappings**
```json
{
  "type": "tcp",
  "remote_port": 8080,
  "local_ip": "127.0.0.1",
  "local_port": 80
}
```

## Web 管理后台

服务端启动后访问 `http://<服务端IP>:4200`，提供：

| 页面 | 功能 |
|------|------|
| 仪表盘 | 隧道数、客户端数、连接数、流量统计、系统日志 |
| 隧道管理 | 查看/删除隧道映射 |
| 客户端列表 | 查看在线客户端及在线时长 |

配置 `web_user` / `web_pass` 后访问需 Basic Auth 认证。

## 端口说明

| 端口 | 组件 | 说明 |
|------|------|------|
| `7000` | 服务端 | 控制连接端口（`bind_port`） |
| `7001` | 服务端 | 代理端口（自动，`bind_port + 1`） |
| `4200` | 服务端 | Web 管理后台（`web_port`） |
| `7500` | 客户端 | 本地 API 端口（`api_port`） |

## 工作原理

```
1. 客户端通过控制端口(7000)连接服务端，使用 Token 认证
2. 客户端发送 ADD 命令注册映射（远程端口 -> 本地地址）
3. 服务端在远程端口上监听外部连接
4. 外部用户连接远程端口时，服务端通知客户端
5. 客户端建立工作连接到服务端代理端口(7001)
6. 服务端桥接外部连接和工作连接
7. 客户端桥接工作连接和本地服务
8. 数据双向透明转发
```

## 安全建议

- 生产环境务必配置 `token`，避免未授权访问
- 配置 `web_user` / `web_pass` 保护 Web 管理后台
- 配置 `api_token` 保护客户端 API
- 使用强密码，避免使用默认值
- 建议通过防火墙限制控制端口(7000)和管理端口(4200)的访问来源

## 项目结构

```
Tianxing/
├── server/
│   ├── main.go              # 服务端（单文件，含 Web 后台）
│   ├── static/               # 嵌入的 Bootstrap 资源
│   │   ├── bootstrap.min.css
│   │   └── bootstrap.bundle.min.js
│   └── go.mod
├── client/
│   ├── main.go              # 客户端（单文件，含 CLI + API）
│   └── go.mod
├── tianxing.conf.example    # 配置文件示例
├── server.conf              # 服务端配置
├── client.conf              # 客户端配置
└── README.md
```

## License

MIT License
