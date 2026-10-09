# xhx2api — 小浣熊 OpenAI 兼容反代

把商汤「小浣熊」（`xiaohuanxiong.com`）网页端 / 桌面端所依赖的 LLM 网关，逆向成本地一个
**OpenAI 兼容**的 HTTP 反代：客户端用 OpenAI SDK / `curl` 指向本服务，请求会被转发到小浣熊上游，
响应（含流式、思维链、function calling、usage）原样回传。

- **上游**：商汤小浣熊官方接口 `https://xiaohuanxiong.com`（使用你自己的账号登录后取得的 token）。
- **用途**：**仅个人学习与自用**。本项目与商汤科技（SenseTime）**没有任何关系**，非官方、未获授权。
  请遵守上游服务条款，不要用于商业用途、批量注册、转售或任何滥用行为；由此产生的一切后果由使用者自负。

> 本仓库只包含反代程序与部署文件，**不包含**任何账号、token 或密钥。

---

## 目录

- [快速开始](#快速开始)
- [配置项说明](#配置项说明)
- [账号文件与 token 获取](#账号文件与-token-获取)
- [HTTP 端点](#http-端点)
- [管理面板](#管理面板)
- [每日签到](#每日签到)
- [限制与排障](#限制与排障)

---

## 快速开始

### 1. 准备配置

```bash
cp config.example.json config.json
# 编辑 config.json（监听地址、api_key 等；不需要密钥时保持默认即可）
```

`config.json` 支持 JSONC（`//`、`/* */` 注释与尾逗号）。**写错键名会启动失败**（未知键会报错）。

### 2. 准备账号

```bash
mkdir -p data
cp data/accounts.example.json data/accounts.json 2>/dev/null || cat > data/accounts.json <<'EOF'
{
  "accounts": [
    {
      "id": "user-你的ID",
      "name": "小浣熊-1",
      "user_id": "你的ID",
      "access_token": "eyJhbGci…",
      "refresh_token": "eyJhbGci…",
      "expires_at": 0,
      "enabled": true,
      "priority": 100
    }
  ]
}
EOF
```

token 的获取方式见 [账号文件与 token 获取](#账号文件与-token-获取)。

### 3. 启动

```bash
# 容器以非 root 用户（uid 10001）运行，data/ 需要可写：首次部署与每次新建 data/ 后都要做一次
sudo chown -R 10001:10001 data

docker compose up -d --build
docker compose logs -f          # 看到「生效配置」与「版本=… 监听=… 账号=…/… 目录=…」即启动成功
```

> 漏掉 `chown` 的表现：服务能起来、`/healthz` 正常，但 `[pool] 警告：…账号文件…` 反复出现，
> 且刷新出来的 token 不会持久化（重启后回到旧 token）。

### 4. 验证（可直接复制）

```bash
# 健康检查
curl -s http://127.0.0.1:3066/healthz

# 模型列表（OpenAI 形状）
curl -s http://127.0.0.1:3066/v1/models | head -c 400

# 一条流式 chat（模型名直接用上游 id，如 sn-glm-5-3-flash）
curl -sN -H 'Content-Type: application/json' \
  -d '{"model":"sn-glm-5-3-flash","messages":[{"role":"user","content":"你好"}],"stream":true}' \
  http://127.0.0.1:3066/v1/chat/completions

# 浏览器打开管理面板
#   http://127.0.0.1:3066/admin
```

> 若在 `config.json` 里设置了 `api_key`，上面 `/v1` 的请求要加 `-H 'Authorization: Bearer <api_key>'`；
> 若设置了 `admin.api_key`，面板地址要带 `?key=<admin_api_key>`。

---

## 配置项说明

下表列出 `config.example.json` 中的**每一个键**（默认值即模板中的值；按段分组）。

### 顶层

| 键 | 默认值 | 含义 |
|---|---|---|
| `listen` | `"0.0.0.0:3066"` | 监听地址与端口；非法（无法解析或端口越界）会报错退出。 |
| `api_key` | `""` | `/v1/*` 的 Bearer 鉴权密钥；空 = 不鉴权。 |
| `shutdown_timeout_seconds` | `10` | 收到 `SIGINT`/`SIGTERM` 后优雅退出的超时（秒）。 |

### `upstream`

| 键 | 默认值 | 含义 |
|---|---|---|
| `llm_base` | `"https://xiaohuanxiong.com/api/web/llm/v2"` | LLM 网关基址（chat、模型目录）。 |
| `auth_base` | `"https://xiaohuanxiong.com/api/web/auth/v1"` | 鉴权基址（token 刷新、订阅信息）。 |
| `points_base` | `"https://xiaohuanxiong.com/api/web/points/v1"` | 积分基址（余额、账单）。 |
| `origin` | `"https://xiaohuanxiong.com"` | 主站 origin（签到与账单接口挂在这里，独立配置点）。 |
| `connect_timeout_seconds` | `30` | TCP 建连超时（秒）。 |
| `response_header_timeout_seconds` | `120` | 等待响应头超时（秒）。 |
| `request_timeout_seconds` | `0` | 单次 chat 的总超时（秒）；`0` = 不设（长回答流式）。 |
| `refresh_timeout_seconds` | `30` | 刷新 token 超时（秒）。 |
| `catalog_timeout_seconds` | `10` | 拉取模型目录超时（秒）。 |
| `balance_timeout_seconds` | `20` | 查询积分/余额超时（秒）。 |
| `bills_timeout_seconds` | `25` | 账单核对超时（秒，签到用）。 |
| `insecure_skip_verify` | `false` | 是否跳过上游 TLS 证书校验（仅排障用，勿在生产打开）。 |
| `user_agent` | `"xhx2api/0.1.0"` | 发往上游的 `User-Agent`。 |
| `extra_headers` | `{}` | 附加到所有上游请求的头（键值均为字符串）。 |

### `accounts`

| 键 | 默认值 | 含义 |
|---|---|---|
| `file` | `"./data/accounts.json"` | 账号文件路径；不存在则空池启动（`/v1` 返回 503）。 |
| `strategy` | `"round_robin"` | 选号策略：`round_robin`（在可用账号间轮转，默认）或 `priority`（按 priority 升序，数值越小越优先）。 |
| `refresh_margin_seconds` | `300` | 过期前多久主动刷新 token（秒）。 |
| `cooldown_seconds` | `600` | 上游 429/5xx 后该账号的冷却时长（秒）。 |
| `max_attempts` | `3` | 一次客户端请求最多尝试几个不同账号。 |
| `refresh_on_401` | `true` | 收到 401 时强制刷新同账号并重试一次。 |

### `models`

| 键 | 默认值 | 含义 |
|---|---|---|
| `default` | `"sn-glm-5-3-flash"` | 请求未给 `model` 时使用的模型 id。 |
| `validate` | `"catalog"` | 模型校验：`none`（原样透传）或 `catalog`（不在目录里则 400；目录为空时跳过校验、原样透传）。 |
| `catalog_cache_seconds` | `600` | 目录内存缓存有效期（秒）。 |
| `catalog_cache_file` | `"./data/model_catalog.json"` | 目录持久化缓存文件；远程拉不到时用它。 |

> 模型名一律用**上游 id**（`sn-glm-5-3-flash`、`sn-glm-5-3`、`sn-kimi-k3` 等），没有别名映射。
> 目录来源优先级：内存（TTL 内）→ 持久化缓存文件 → 空（来源显示 `empty`，此时 `/v1/models` 返回空列表）。

### `log`

| 键 | 默认值 | 含义 |
|---|---|---|
| `level` | `"info"` | 日志级别：`debug` / `info` / `warn` / `error`。 |
| `access_log` | `true` | 是否向 stdout 打印 `[access] …` 访问日志。 |
| `buffer_size` | `500` | 面板「最近请求」保留的条数。 |
| `file` | `""` | 额外日志文件路径；空 = 只写 stdout。 |

> 启动摘要（生效配置、版本/监听/账号/目录）**不受 `level` 影响**，始终打印。

### `admin`

| 键 | 默认值 | 含义 |
|---|---|---|
| `enabled` | `true` | 是否启用 `/admin` 与 `/admin/api/*`；`false` 时一律 404。 |
| `api_key` | `""` | 面板与 `/admin/api` 的鉴权密钥；空 = 不鉴权（仅内网）。支持 `Authorization: Bearer` 与 `?key=`。 |
| `title` | `"小浣熊反代"` | 面板顶部标题。 |

### `checkin`

| 键 | 默认值 | 含义 |
|---|---|---|
| `enabled` | `true` | 是否启用每日签到后台任务。 |
| `time` | `"00:01"` | 每日触发时刻（本地时间 `HH:MM`，容器 `TZ=Asia/Shanghai`）。 |
| `on_start` | `true` | 启动时若「今天还没成功」立即补签一次。 |
| `verify_bills` | `true` | `true` = 以「今日账单确有入账」为成功；`false` = 以 `setting_info` 返回 `code==0` 为成功。 |
| `bills_limit` | `20` | 账单核对查询条数。 |
| `retry_interval_minutes` | `30` | 当天失败后的重试间隔（分钟）。 |
| `max_attempts_per_day` | `5` | 当天最多尝试次数（含首次）。 |
| `history_file` | `"./data/checkin.json"` | 签到历史文件。 |
| `history_days` | `30` | 历史保留天数（按日期去重）。 |

### `login`

| 键 | 默认值 | 含义 |
|---|---|---|
| `enabled` | `true` | 是否启用网页登录；`false` 时两个登录接口返回 403，面板隐藏「网页登录」入口（手动添加仍可用）。 |
| `app_name` | `"办公小浣熊客户端"` | 授权页上报的客户端名（中文，会正确 URL 编码）。 |
| `state_ttl_seconds` | `600` | 登录 state 有效期（秒）；过期后必须重新发起。`0` 表示立即过期（仅测试用）。 |

> 授权页路径 `/code/authorize`、`login_source=desktop` 与回调 `office-raccoon://auth/callback`
> 是**协议常量**，不做配置项。

---

## 账号文件与 token 获取

### 文件格式（`data/accounts.json`）

```json
{
  "accounts": [
    {
      "id": "user-7591340",
      "name": "小浣熊-1",
      "user_id": "7591340",
      "access_token": "eyJhbGci…",
      "refresh_token": "eyJhbGci…",
      "expires_at": 1791529021000,
      "enabled": true,
      "priority": 100,
      "last_refresh_at": 0,
      "cooldown_until": 0,
      "last_error": ""
    }
  ]
}
```

| 字段 | 说明 |
|---|---|
| `id` | 账号唯一标识，反代内部使用（建议 `user-<user_id>`）。 |
| `name` | 显示名，面板展示用。 |
| `user_id` | 小浣熊用户 id。 |
| `access_token` | 访问令牌（JWT），聊天请求的 `Authorization: Bearer`。 |
| `refresh_token` | 刷新令牌；用于换取新的 `access_token`。 |
| `expires_at` | `access_token` 过期时间（**毫秒** Unix 时间戳）；`0` = 未知（不主动刷新，靠 401 触发）。 |
| `enabled` | 是否启用；缺省 `true`。 |
| `priority` | 选号优先级，数值越小越优先。 |
| `last_refresh_at` | 上次刷新时间（毫秒），由程序维护。 |
| `cooldown_until` | 冷却截止时间（毫秒），由程序维护。 |
| `last_error` | 最近一次错误，由程序维护。 |

> 文件里**其它未知字段会被原样保留**，程序回写时不会丢。程序回写用「临时文件 + rename」原子替换，权限 `0600`。

### 怎么拿到 token

1. 浏览器登录 `https://xiaohuanxiong.com`，打开开发者工具（F12）：
   - 在 **Network** 里找任意一个发往 `xiaohuanxiong.com` 的请求，复制请求头里的 `Authorization: Bearer eyJ…`，`eyJ…` 即 `access_token`；
   - `refresh_token` 通常在登录/刷新接口的响应里，或存放于 **Application → Local Storage / Cookies**（形如 `refresh_token`）。
2. 或者使用官方桌面端：登录后 token 保存在 `~/.box-agent/config/auth.json`（`access_token` / `refresh_token`）。

**务必注意：**

- `access_token` 会**过期**（一般数小时到一天）。本服务会在临期前自动用 `refresh_token` 刷新。
- `refresh_token` **可能被轮换**（刷新后上游可能下发新的），本服务会自动回写。
- **同一个账号不要同时被两个反代/客户端刷新**：两边都拿旧 `refresh_token` 去刷新，会互相顶掉，
  导致其中一方频繁 401。若要从别的工具迁移，先停掉那边再用本服务。

---

## HTTP 端点

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/healthz` | 无 | 健康检查：`{"status":"ok","version":…,"uptime_seconds":…,"accounts":{…},"models":…}`。 |
| GET | `/favicon.ico` | 无 | 返回 204（在鉴权前短路）。 |
| GET | `/v1/models` | `api_key` | OpenAI 模型列表（仅上游目录条目；目录为空时 `data` 为空数组）。 |
| POST | `/v1/chat/completions` | `api_key` | 聊天补全，支持流式/非流式、思维链、function calling。 |
| GET | `/admin`、`/admin/` | `admin.api_key` | 内嵌管理面板 HTML。 |
| GET | `/admin/api/*` | `admin.api_key` | 只读 JSON 接口（见下）。 |
| POST/PATCH/DELETE | `/admin/api/accounts…` | `admin.api_key` | 账号管理写接口（见「面板管理账号」）。 |
| POST | `/admin/api/login/start` | `admin.api_key` | 发起网页登录，返回授权页 URL 与 state（见「网页登录」）。 |
| POST | `/admin/api/login/exchange` | `admin.api_key` | 用回调 URL（或 code+state）换凭证并落账号。 |

`/admin/api/*` 的只读 GET：

| 路径 | 返回 |
|---|---|
| `/admin/api/status` | 版本、启动时间、运行时长、监听地址、生效配置（密钥已掩码）、账号/模型统计。 |
| `/admin/api/accounts` | 账号池快照（不含完整 token，只有尾 4 位）。 |
| `/admin/api/accounts/balance` | 每账号积分与钱包明细（逐账号查上游，结果缓存 30 秒）。 |
| `/admin/api/models` | 模型目录条目。 |
| `/admin/api/requests?limit=N` | 最近 N 条请求日志（默认 100）。 |
| `/admin/api/checkin` | 每账号的每日签到状态与历史。 |

---

## 管理面板

浏览器打开 `http://<监听地址>/admin`（设置了 `admin.api_key` 时用 `/admin?key=<key>`）。

- 展示状态，并提供**账号管理**（网页登录 / 手动新增 / 启用禁用 / 删除 / 测试）。
- **每 5 秒自动刷新**一次；某次刷新失败时顶部显示「接口不可用」，但**不会清空**已展示的数据。
- 自上而下包含六块：顶部状态条、账号池、积分卡、每日签到、模型目录、最近请求。
- 页面为**单文件、零外部依赖**（无 CDN / 外部字体 / 图标），离线可用；暗色主题，窄屏可用（表格可横向滚动）。
- 页面**不会**显示完整 `access_token` / `refresh_token` / `api_key`（接口本身也不返回，只显示尾 4 位）。

### 面板管理账号（写操作）

> **前提：必须先在 `config.json` 里设置 `admin.api_key`。** 未设置时所有写操作返回 **403**，
> 并提示「请先在 config.json 里设置 admin.api_key，再使用账号管理功能」。只读 GET 不受影响。

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/admin/api/accounts` | 新增账号，body：`name` / `access_token`（必填）/ `refresh_token` / `priority`（默认 100）/ `enabled`（默认 true）。成功 201。 |
| PATCH | `/admin/api/accounts/{id}` | 只改给出的字段：`name` / `priority` / `enabled` / `access_token` / `refresh_token`。成功 200。 |
| DELETE | `/admin/api/accounts/{id}` | 删除账号。成功 200 `{"deleted":"<id>"}`。 |
| POST | `/admin/api/accounts/{id}/test` | 用该账号当前 token 只读打一次上游目录，返回 `{"ok":true,"status":200,"models":<n>}`；上游失败也返回 HTTP 200，用 `{"ok":false,"status":<上游码>,"error":"…"}` 表达。 |

- 鉴权沿用 `/admin/api/*`：`Authorization: Bearer <key>` 或 `?key=<key>`。
- 新增时 `id` = `user-<userId>`（从 JWT 解析，不验签）；取不到 userId 时用 `acct-<8 位随机十六进制>`。
- `expires_at` / `user_id` 从 JWT 解析；解析不出则 `expires_at = 0`（不主动刷新，靠 401 触发）、`user_id = ""`。
- 每次写操作成功后**原子回写** `accounts.json`（临时文件 + rename、权限 `0600`、保留未知字段）。
- 面板「账号池」表格的「操作」列即可启用/禁用、删除（二次确认）、测试；表格上方「新增账号」按钮展开表单。

---

## 网页登录（一次性授权码换凭证）

面板「账号池」上方的**「网页登录」**按钮走小浣熊官方登录页，登录成功后自动拿到
`access_token` / `refresh_token`，**不需要手动粘贴 token**。

流程（`login.enabled=true` 时）：

1. 面板点「网页登录」→ `POST /admin/api/login/start`，服务用 `crypto/rand` 生成一次性
   `state`（32 位十六进制，带过期时间）并返回授权页 URL：
   `https://xiaohuanxiong.com/code/authorize?login_source=desktop&appname=…&state=…`。
2. 打开该链接，在官方页面用微信/验证码/密码登录；成功后官方页面跳转自定义协议
   `office-raccoon://auth/callback?code=…&state=…`（浏览器打不开这个协议是正常的）。
3. 把地址栏里的**整条 URL** 复制回面板输入框，点「完成登录」→
   `POST /admin/api/login/exchange`，服务校验 state 后用 code 调
   `POST {auth_base}/login_with_authorization_code` 换凭证并落账号。

接口：

| 方法 | 路径 | body | 成功返回 |
|---|---|---|---|
| POST | `/admin/api/login/start` | 无 | `{"url":…,"state":…,"expires_at":…,"ttl_seconds":…}` |
| POST | `/admin/api/login/exchange` | `{"callback_url":"office-raccoon://auth/callback?code=…&state=…"}` 或 `{"code":…,"state":…}` | `{"id":"user-…","updated":false,"account":{…}}`（同 id 再登录一次为 `updated:true`） |

- 鉴权沿用 `/admin/api/*`：`Authorization: Bearer <key>` 或 `?key=<key>`；`admin.api_key` 为空时返回 403。
- **state 安全语义**：state 由本服务生成、内存 pending 表登记（带过期时间）；回调里的 state 必须与其中
  一条逐字相等且未过期，且**一次性消费**。校验失败/过期/缺失一律 400，且**不调用上游**。
  回调 URL 解析后逐项比对 `scheme=office-raccoon`、`host=auth`（大小写不敏感）、`path=/callback`，
  不使用前缀匹配。`code` ≤ 8192、`state` ≤ 512 字符。
- `login.enabled=false` 时两个接口返回 403「网页登录已禁用（config.json 的 login.enabled=false）」，面板隐藏入口。
- 换回的 `office_identity` / `office_org_name` / `office_org_role` 非空时一并写入账号记录（作为未知字段保留）。
- 日志只记 `[login]` 的 id/结果，**不打印 code 或 token**。

---

## 挂进 CPA（CLIProxyAPI）

本服务本身就是 OpenAI 兼容端点，在 CPA 里按 `openai-compatibility` 渠道挂即可，只需要三样东西：
**base-url**、**api-key**（= 本服务 `config.json` 的 `api_key`）、**模型名**（就是上游 id，见 `/v1/models`；
本服务**不做别名**）。

```yaml
# 追加到 /zfspool/data/ai/cpamp/cliproxyapi/config.yaml 顶层的 openai-compatibility: 列表里
  - name: xhx2api
    prefix: xhx
    base-url: http://10.1.0.8:3066/v1
    api-key-entries:
      - api-key: <config.json 里的 api_key>
    models:
      - name: sn-glm-5-3-flash
        alias: ""
      - name: sn-glm-5-3
        alias: ""
      - name: sn-kimi-k3
        alias: ""
      - name: sn-deepseek-v4-1-flash
        alias: ""
      - name: sn-sensenova-6-8-flash
        alias: ""
      - name: sn-sensenova-6-8-flash-lite
        alias: ""
      # 可选（visible=false 的内部模型）：raccoon-8c4485 / raccoon-19b265 / raccoon-405a1c
```

- 读 api_key：`grep -o '"api_key": "[^"]*"' /zfspool/data/ai/xiaohuanxiong/config.json`
- 生效：`ssh ai 'cd /zfspool/data/ai/cpamp && docker compose restart cli-proxy-api'`
- 挂上后模型对外名带前缀（`xhx/sn-glm-5-3-flash`）。验证：
  `curl -s -H "Authorization: Bearer <CPA key>" http://10.1.0.8:8317/v1/models | grep xhx`
- 现成片段（含真实 api_key，放在不进 git 的 `data/` 下）：`data/cpa-provider-snippet.yaml`

---

## 每日签到

小浣熊每天会给账号发放一次「每日积分」。本服务内置签到任务，替账号领取这笔积分：

- 触发时机：进程启动时若「今天还没成功」立即补签（`checkin.on_start`）→ 每天 `checkin.time`（默认 `00:01`，本地时区）→ 当天失败后按 `retry_interval_minutes` 重试。
- 每个账号**每天最多成功一次**（靠 `data/checkin.json` 幂等判定，不会重复打上游）。
- `verify_bills=true` 时，以「当日账单确有正数入账」为成功判据；否则以触发接口 `code==0` 为成功。
- 结果会写入日志（`[Checkin] 账号 <id> 今日积分 +300` / 失败 warn），并可在面板「每日签到」表或 `GET /admin/api/checkin` 查看。
- 设 `checkin.enabled=false` 可关闭后台任务（历史仍可读）。

---

## 限制与排障

### 已知限制

- 仅代理**文本**聊天与 function calling；上游网页端的**图片/文件上传**等能力不在此反代范围内。
- **不支持**注册机、新手任务等；网页登录已支持（见「网页登录」），也可手动粘贴 token。
- 上游模型行为、额度、风控策略随时可能变化，本项目不保证长期可用。
- 单账号被多处同时刷新会互相顶掉（见上文 token 说明）。

### 常见问题

**401 反复出现**
- 说明 `access_token` 失效且自动刷新没成功。先看面板「账号池」的**最近错误**与 `last_error`：
  - 若刷新失败：`refresh_token` 可能已过期或被别处轮换。重新登录抓一份新的 token 写入 `data/accounts.json`。
  - 确认**没有第二个程序**在用同一账号刷新。
- 日志里会看到 `[chat] 账号 … 刷新失败：…`。

**429（限流）**
- 上游对该账号限流，服务会把账号冷却 `accounts.cooldown_seconds` 秒并自动换下一个账号。
- 面板「冷却中」列可看到状态。若频繁 429，降低请求频率或增加账号（`accounts.max_attempts` 决定一次请求最多换几个号）。

**目录拉不到**
- 不影响服务转发：会依次退回**持久化缓存文件** → **空**（来源显示 `empty`）。目录为空时 `/v1/models` 返回空列表；`validate=catalog` 会跳过校验、原样透传上游。
- 排查：检查 `upstream.llm_base` 连通性与网络/代理；启动日志若有 `[catalog] 远程目录拉取失败…` 即为此问题。
- 面板顶部「模型数（来源）」会显示当前来源是 `remote` / `cache` / `empty`。

**启动失败：未知键 `aliases` / `fallback`**
- `models.aliases` 与 `models.fallback` 已在第四轮移除。配置里若仍留着这两个键，会因未知键报错并以退出码 2 启动失败。
- 处理：从 `config.json` 删除这两个键；模型名直接使用上游 id（如 `sn-glm-5-3-flash`）。

**账号文件写回失败**
- 日志出现 `[pool] 警告：…账号文件…` 即写回失败。常见原因：`data/` 目录不可写、挂载为只读、磁盘满。
- 写回失败**不影响本次请求**，但 token 刷新结果不会持久化，重启后可能仍是旧 token。
- 检查容器内 `data/` 挂载与权限（程序以非 root 用户运行，需可写）。

**签到失败**
- 看面板「每日签到」表的**最近消息 / 最近错误**，或 `GET /admin/api/checkin` 的 `last_error`。
- 日志里的 `[Checkin] 账号 … 签到失败：…` 会给出原因（token 刷新失败 / 触发发放失败 / 今日未见积分入账 / 账单核对失败）。
- 常见原因：token 失效、`checkin.time` 时区不对、当天已达 `max_attempts_per_day` 上限。

---

## 免责声明

本项目为个人学习与自用工具，与商汤科技无任何关联，也未获其授权。请自行确保使用方式符合上游服务条款
与当地法律法规；请勿用于商业牟利、批量注册或任何滥用行为。使用风险由使用者自行承担。
