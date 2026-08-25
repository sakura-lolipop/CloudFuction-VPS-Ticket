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
→ 401 {"error":"unauthorized"}          # 带了 Bearer 且 TICKET_AUTH_TOKEN 设了但不匹配
→ 403 {"error":"banned"}                # auto-ban 临时封（Retry-After 头给剩余秒数）
→ 429 {"error":"rate_limited"}          # 超 IP 桶或 token 桶限速
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
3. **NSSM 注册**：管理员运行 `nssm-ticket.bat`（装 `HotifyTicketCF` 服务，匿名模式 + TTL 600，全部配置走 config.yml（热加载））
4. **Tunnel 复用**：不动现有 `HotifyTunnel` 服务，只改它的 `config.yml` 加一条 ingress 后重启服务：
   ```yaml
   ingress:
     - hostname: push.hotify.love
       service: http://127.0.0.1:12345     # 旧中继，原样
     - hostname: ticket.hotify.love        # 新增：铸票
       service: http://127.0.0.1:12346
     - service: http_status:404
   ```
   DNS 一次性：`cloudflared.exe tunnel route dns hotify ticket.hotify.love`（或 CF 控制台加 CNAME）
5. **验证**：本机 `curl http://localhost:12346/` → 三字段 JSON；外网 `curl https://ticket.hotify.love/` 同
6. （可选，DoS 真防线）CF 控制台给 `ticket.hotify.love` 开一条免费限速规则

Linux VPS 等价：`go build` + systemd + 任意反代指到 12346，逻辑相同。

## 鉴权与滥用策略（2026-08-25 设计定稿）

**策略栈**（env 全默认关 = 纯匿名直通）：

```
请求 → 身份二分（Bearer 命中 TICKET_AUTH_TOKEN → who:default / 匿名 → ip:addr）
     → auto-ban 内存临时封（403，固定刑期=strikes 窗口 → 解封即白纸；默认 0=关）
     → 双桶限速（429，IP 桶宽兜底 + token 桶紧 per-server，分别设置；默认 0=关）
     → 签票
```

- **现在：匿名开放**（`TICKET_AUTH_TOKEN` 不设，任何 Bearer 都忽略）——测试期配额可控。
- 双桶恒记（匿名期 IP 桶独活）：防单 IP 无限刷/多开绕过；token 桶 per-server（token 期生效）。
- auto-ban：窗口内撞 429 达 N 次自动封该 key（固定刑期，期间请求零记账，到期白纸）。
- **永久封不设表**（云函数内查表挡不住 DoS——请求到达已耗 invoke）：永久封 IP = Cloudflare 层（不耗配额）；DoS = CF 限速规则 + txt 摘节点。
- ~~白名单终态~~ **已裁废（2026-08-25）**：产品定位=默认开放给所有人（推广期也是），滥用治理走速率维度（限速/auto-ban/CF 层），不走身份准入。TICKET_AUTH_TOKEN 仅作调用方配置错检测。
- 观察底座：`issue ok` 日志按 `who=`/`ip:` 可数（滥用第一时间可见）。

## 安全边界（诚实版）

- **开放是 feature**：任何知道 URL 的人都能拿票（与中继入口的公开逻辑一致）；滥用信号=观察面板 Top IP/auto-ban 计数，响应=收紧限速，非关门。
- `TICKET_AUTH_TOKEN`（准确描述，对齐当前实现）：设了只校验**带 Bearer 的请求**（抓调用方配置错），**不带头仍匿名放行**——不防白嫖（开放模式下也无此需求）。真要锁死某部署须网络层（Tunnel Access / 内网）。
- SA 私钥泄露 = 该 Push Kit 应用全权暴露。泄露应对：AGC 重新生成服务账号私钥 → 更新本服务 → 旧私钥（及它签过的票）全部作废。
- 停止签发即吊销未来；已签出票最多活 TTL。

## 开发

```
go test ./...    # 单测（签票格式/鉴权/限速/TTL claims）
go vet ./...
```
