# 白名单（要做再议）

> 状态：**非路线、非永不做**。2026-08-25 裁定：默认开放是产品形态（开放是 feature，
> 与中继入口 cloud_function_urls.txt 的公开逻辑一致），滥用治理走**速率维度**
> （限速/auto-ban/CF 层）不走身份准入。白名单**要做再议**——真出现需要身份准入的场景
> 再启动，不预设触发线。

## 已留好的缝（将来做白名单=低成本插入，handler 零重构）

| 缝 | 位置 | 白名单复用方式 |
|---|---|---|
| 统一键空间 `who:`/`ip:` | `resolveIdentity`（main.go） | 名单命中 → who:\<label\>，未命中 → 现 anonymous 行为（或拒，届时定） |
| `TICKET_AUTH_TOKEN` | config.yml / env | 名单的最小载体形态（单值=一行名单）；扩成多值/文件是加载器的事 |
| 策略栈挂点 | resolveIdentity 之后、auto-ban 之前 | 准入判断插入位现成（身份已解析、记账已就绪） |
| token 桶 per-server | `TICKET_RATE_LIMIT_TOKEN` | who: 键记账已在——名单用户天然分桶 |
| config 键可扩 | cfgGet（热加载） | 加 `auth_allowlist_file:` 类键不动机制 |
| 观察底座 | /console Top IP + who 日志 | 名单标签直接进现有归因 |

## 将来做的形态（草案，启动时再议定）

- 云端 txt 热更（复用 cloud_function_urls.txt 基建）：行 = `<sha256(token)> <label>`
  ——哈希存储，token 真值不进公开仓
- 删行 = 吊销（旧票活 ≤TTL）；一户一 token，label 进日志/面板归因
- 成本估计：文件加载器 + resolveIdentity 查表，~50 行，handler/策略栈/面板零动

## 为什么现在不做

单用户自托管 + 默认开放的产品定位下，身份准入没有消费者；速率治理已覆盖滥用面。
（防"又手搓"复发：本文件是唯一记录点，README 只留一句指针。）
