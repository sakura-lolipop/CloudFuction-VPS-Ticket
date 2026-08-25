# CloudFuction-VPS-Ticket

Hotify CF 2.0 **铸票厂**（VPS 形态）：验 token → 用华为 Service Account 签短 exp PS256 JWT（"票"）→ 吐给调用方。调用方拿票**自己直连**华为 push-api 推送——通知内容不再过任何自有基础设施。

> 隐私模型：本服务**不见任何通知内容**（请求体可为空）。凭证（SA 私钥）锁在本服务，短 exp 票外借 ≤TTL。
> 设计依据：Hotify `docs/pushkit-transport.md` §9.7（票据化图纸）。
> Netlify 形态联邦件：[CloudFuction-Ticket](https://github.com/sakura-lolipop/CloudFuction-Ticket)。

## 契约

```
GET/POST /
Authorization: Bearer <TICKET_AUTH_TOKEN>

→ 200 {"ticket": "<PS256 JWT>", "project_id": "<本节点 SA 的 project_id>", "expires_at": <unix秒>}
→ 401 {"error":"unauthorized"}          # token 错
→ 429 {"error":"rate_limited"}          # 超过每分钟签发上限（默认 10）
→ 500 {"error":"private_json"|"sign"}   # SA 配置/签名错
```

调用方消费方式（照抄即可）：

```
POST https://push-api.cloud.huawei.com/v3/{project_id}/messages:send
Authorization: Bearer <ticket>          # 票直接当 Bearer（push-jwt-token 模式，不换 access_token）
push-type: 0                            # 0=通知 / 6=后台消息
body: {target, payload, pushOptions}    # 华为 v3 原生格式
```

- 票在 `expires_at` 前有效，**提前 60s 刷新**（margin 防边界）。
- 每个节点签出的票带**本节点 SA 的 project_id**——票与节点配套，切节点须重新拿票（project_id 一起换）。

## 部署（自有 VPS，Windows 实况）

与 go-harmony 中继同机共存的部署方式（go-harmony 服务原样不动，cf-ticket 独立进程+独立端口）：

1. **编译**（Windows VPS 直接原生件）：本仓目录 `go build -o cf-ticket.exe .`
2. **建独立目录**（如 `C:\hotify\cf-ticket\`）：放 `cf-ticket.exe` + `nssm-ticket.bat` + 你的 `private.json`（华为 AGC → 项目设置 → 服务账号；同目录自动扫描；与 go-harmony 各持一份副本，SA 轮换互不牵连）
3. **NSSM 注册**：管理员运行 `nssm-ticket.bat`（装 `HotifyTicketCF` 服务，匿名模式 + TTL 600，env 在脚本里改）
4. **Tunnel 复用**：不动现有 `HotifyTunnel` 服务，只改它的 `config.yml` 加一条 ingress 后重启服务：
   ```yaml
   ingress:
     - hostname: push.hotify.love
       service: http://127.0.0.1:12345     # 旧中继，原样
     - hostname: ticket.hotify.love        # 新增：铸票
       service: http://127.0.0.1:8091
     - service: http_status:404
   ```
   DNS 一次性：`cloudflared.exe tunnel route dns hotify ticket.hotify.love`（或 CF 控制台加 CNAME）
5. **验证**：本机 `curl http://localhost:8091/` → 三字段 JSON；外网 `curl https://ticket.hotify.love/` 同
6. （可选，DoS 真防线）CF 控制台给 `ticket.hotify.love` 开一条免费限速规则

Linux VPS 等价：`go build` + systemd + 任意反代指到 8091，逻辑相同。

## 鉴权与滥用策略（演进中，2026-08-25 设计定稿）

**统一模型**：调用方键三级择一 `who:标签` > `inst:部署uuid`（`X-Hotify-Instance` 头）> `ip:可信IP`（CF-Connecting-IP/RemoteAddr，绝不读客户端自带 XFF）。滥用策略族全查这个键空间：

| 策略 | 语义 | 状态 |
|---|---|---|
| 限速 | 每 key 每分钟 N 张（`TICKET_RATE_LIMIT`，**默认 0=无限制**） | ✅ 已实装（env 可调） |
| 黑名单 ban | key 进表即拒 | 缝已留：ban txt 云端热更（参考 `cloud_function_urls.txt` 基建），滥用出现时上 |
| 白名单准入 | 一户一 token → who 标签，删行=吊销（旧票活 ≤TTL） | 缝已留：同 txt 基建，公开推广前上 |

- **现在：匿名开放**（`TICKET_AUTH_TOKEN` 不设）+ 限速能力待命——测试期配额可控。
- 三级择一的结构收益：ban ip 时带 instance 头的诚实 Server 不受影响（instance 头=诚实方准白名单通道），裸请求才吃 IP ban。
- ⚠️ token 真值不能进公开 txt（届时存哈希或私有仓 raw）。

## 安全边界（诚实版）

- **匿名期风险**：任何知道 URL 的人都能拿票给该 SA 应用的 token 发推——量小可控（配额消耗/内容落项目档案）；触发线到（公开推广）前切白名单。
- `TICKET_AUTH_TOKEN` 是**纸墙**：防扫描器白嫖签发配额，不防看得到你调用方配置的人。真要锁死须网络层（Tunnel Access / 内网）。
- SA 私钥泄露 = 该 Push Kit 应用全权暴露。泄露应对：AGC 重新生成服务账号私钥 → 更新本服务 → 旧私钥（及它签过的票）全部作废。
- 停止签发即吊销未来；已签出票最多活 TTL。

## 开发

```
go test ./...    # 单测（签票格式/鉴权/限速/TTL claims）
go vet ./...
```
