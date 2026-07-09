# ddns-go

基于 Go 的多平台 DDNS 客户端，支持阿里云 DNS 和 Cloudflare DNS，自动将公网 IP 同步到域名解析记录。

## 功能特性

- **多源 IP 获取**：内置 5 个可信 IP API 自动切换，任一可用即可获取公网 IP
- **三层检测机制**：
  - 连通性探测（默认 5 秒）— 检测到断网恢复后秒级触发 DNS 更新
  - IP 变更检测（默认 30 秒）— 常规轮询对比 IP 变化
  - 强制兜底更新（默认 5 分钟）— 防止极端情况漏检
- **格式兼容**：同时支持 JSON 响应（`ip`/`origin` 字段）和纯文本响应
- **多 RR 支持**：一条域名下多个解析记录（逗号分隔）
- **优雅退出**：监听 SIGINT/SIGTERM，退出前写入日志

## 快速开始

### 编译

```bash
go build -o DDNS ddns.go
```

### ARM64 交叉编译

```bash
GOOS=linux GOARCH=arm64 go build -o DDNS-linux-arm64 ddns.go
```

### 配置

```bash
cp config.example.json config.json
```

编辑 `config.json`，填入阿里云 AccessKey 和域名信息。

### 运行

```bash
./DDNS -config config.json
```

## 配置说明

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `accessKey` | string | - | 阿里云 AccessKey ID |
| `accessSecret` | string | - | 阿里云 AccessKey Secret |
| `domainName` | string | - | 主域名 |
| `logPath` | string | DDns.log | 日志文件路径 |
| `apiURLs` | []string | 5 个内置源 | IP 查询 API 列表，顺序尝试 |
| `recordType` | string | A | DNS 记录类型 |
| `rr` | string | * | 解析记录，多个用逗号分隔（如 `@,hz`） |
| `checkInterval` | int | 5 | 连通性探测间隔（秒） |
| `ipCheckInterval` | int | 30 | IP 变更检测间隔（秒） |
| `forceUpdateInterval` | int | 5 | 强制更新间隔（分钟） |
| `probeTargets` | []string | 1.1.1.1:443, dns.alidns.com:443 | TCP 探测目标 |
| `timeout` | int | 10 | HTTP 请求超时（秒） |

## 部署

### systemd 服务

创建 `/etc/systemd/system/ddns.service`：

```ini
[Unit]
Description=DDNS
After=network.target

[Service]
WorkingDirectory=/opt/DDNS
ExecStart=/opt/DDNS/DDNS-linux-arm64
User=root
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

启用并启动：

```bash
systemctl daemon-reload
systemctl enable ddns
systemctl start ddns
```

### 快速部署脚本

```bash
chmod +x deploy.sh
./deploy.sh
```

## 工作原理

```
┌──────────────────────────────────────────────┐
│  第 1 层：连通性探测（每 5 秒）                 │
│  TCP 连接 1.1.1.1:443 / dns.alidns.com:443    │
│  不通→通 = 恢复事件 → 立即查 IP                │
├──────────────────────────────────────────────┤
│  第 2 层：IP 变更检测（每 30 秒）              │
│  调用 IP API 获取公网 IP                       │
│  IP 变化 → 更新 DNS 记录                       │
├──────────────────────────────────────────────┤
│  第 3 层：强制兜底（每 5 分钟）                 │
│  无论状态如何，强制执行完整流程                  │
│  防止极端情况漏检                               │
└──────────────────────────────────────────────┘
```

## 许可证

MIT
