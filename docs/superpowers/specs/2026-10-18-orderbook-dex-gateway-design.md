# 子项目 4 设计：排序器 + 网关 + 状态机

> **目标形态**：去中心化订单簿 DEX（混合式：链下订单簿 + 链下撮合 + 链上批量结算，参考 dYdX v3 / Vertex）
> **本文档范围**：子项目 4（排序器 + 网关 + 状态机），即撮合内核的对外入口。
> **前置**：子项目 1（结算合约 + 链下账本）、子项目 2（服务装配 + 资金安全修复）、子项目 3（链下订单簿与撮合引擎）均已合入 `master`。
> **日期**：2026-10-18
> **状态**：已裁定关键设计，待实现

---

## 1. 背景与目标

### 1.1 承接关系

子项目 3 交付了纯确定性撮合内核 `backend/matching` 与宿主服务 `services/OrderBookService`：
冻结 → 落指令日志 → 撮合 → 账本零和结算 → 释放冻结 → EventBus 发布事件。但它**没有对外入口**——
下单、撤单、查询只能通过 Go 代码调用，外部客户端无法接入。

本子项目补上这一环：一个 HTTP/WebSocket 网关，验证客户端的**签名订单**、限流、串行化后送入
`OrderBookService`，并把订单/成交/深度变化经 WebSocket 推回客户端。这是"交易第一次可被外部用户使用"。

### 1.2 子项目路线图（依赖驱动，编号已按实际执行顺序重排）

| # | 子项目 | 内容 | 依赖 | 状态 |
|---|--------|------|------|------|
| 1 | 结算合约 + 链下账本 | 链上托管金库、状态根锚定、强制退出；链下账本 | — | ✅ 已合入 |
| 2 | 服务装配 + 资金安全修复 | `BuildSettlementStack` + `main` 入口；WAL 尾损、锚定取索引、提现钉住 | 1 | ✅ 已合入 |
| 3 | 链下订单簿与撮合引擎 | 内存订单簿、限价/市价、部分成交、资金冻结 | 1,2 | ✅ 已合入 |
| **4** | **排序器 + 网关 + 状态机**（本文档） | 收单/鉴权/限流、WebSocket 推送；下单→撮合→更新账本的对外入口 | 3 | 进行中 |
| 5 | 链上批量结算 + 索引器 | 批量提交成交结算上链、状态重建、历史查询 | 3,4 | — |
| 6 | 永续合约 + 风险引擎 | 仓位、保证金、杠杆、标记价格、资金费率、强平 | 1..5 | — |
| 7 | 前端 + 行情 | 交易界面、深度图、K 线 | 4,5 | — |

### 1.3 本子项目目标

1. 外部客户端可通过 **HTTP** 提交**签名订单**（限价/市价）与**签名撤单**。
2. 网关**验证 EIP-712 签名**恢复账户身份，拒绝伪造；以 **nonce + 过期时间**防重放。
3. 网关按**账户**做**令牌桶限流**，超限拒绝。
4. 一个**内存串行化层（排序器）**把并发请求串行化后逐个送入 `OrderBookService`，保证指令顺序确定、可重放。
5. 一个**请求生命周期状态机**跟踪每个请求（received → accepted/rejected），供查询与推送。
6. 通过 **WebSocket** 推送订单状态、成交、深度快照（按市场 + 账户过滤）。

### 1.4 非目标（明确不做，留给后续子项目）

链上批量结算与索引器（子项目 5）；永续/保证金/杠杆/资金费率/强平（子项目 6）；行情深度聚合、K 线、
深度图（子项目 7）；订单 TTL/退出即自动撤单（沿用子项目 3 的残余信任假设，运营方响应退出事件撤单）；
多签治理；TLS 终止 / 反向代理（部署层）；API key 会话（v1 采用签名订单，无会话、无状态）。

---

## 2. 关键裁定与信任边界

### 2.1 本子项目裁定（已与 owner 确认）

| 议题 | 结论 | 理由 |
|------|------|------|
| 鉴权 | 签名订单（ECDSA） | 无会话、无状态、防伪造；dYdX/Vertex 标准 |
| 签名方案 | EIP-712 类型化数据 | 钱包友好、可读、类型安全、带 nonce/expiry |
| 接口形态 | HTTP 读写 + WS 推送 | 写操作同步返回结果，推送走长连接 |
| 排序器 | 纯内存串行化层 | 保证指令顺序确定；不引入新持久化（指令 WAL 已由子项目 3 承担） |
| 限流 | 按账户令牌桶 | 简单、公平、抗单账户刷量 |
| 状态机 | 请求生命周期状态机 | 跟踪 received → accepted/rejected；订单状态仍以撮合内核为真相 |
| 防重放 | nonce + 过期时间 | EIP-712 标准；nonce 单调递增 |
| nonce 持久化 | 持久化到磁盘 | 重启后拒绝旧 nonce 重放 |
| WS 推送 | 订单 + 成交 + 深度 | 订阅按市场 + 账户过滤 |
| 读接口鉴权 | v1 读接口不鉴权 | 余额/深度/订单数据非敏感，简化实现；写接口才要求签名 |

### 2.2 v1 保证

1. **身份真实**：写请求必须携带合法签名，网关恢复出的账户即请求方；伪造签名一律拒绝。
2. **无重放**：同一签名订单只能被接受一次（nonce 单调 + 过期时间）。
3. **顺序确定**：并发请求经排序器串行化后，`OrderBookService` 收到的指令顺序即请求到达顺序；
   该顺序确定撮合内核的 `OrderID`/`TradeID`，与子项目 3 的可重放性一致。
4. **公平限流**：单账户超发被 429 拒绝，不拖累其他账户。
5. **崩溃自洽**：nonce 上界持久化，重启后重放旧签名被拒；撮合侧沿用子项目 3 的指令 WAL 自愈。

### 2.3 残余信任假设（必须在 README 与文档中显式声明）

1. **读接口不鉴权**：余额、深度、订单状态可被任何人查询。v1 接受这一信息泄露（订单簿本应公开）；
   后续若需要私有化再收紧。
2. **签名不可否认性依赖客户端保管私钥**：服务端只验签，不接触私钥。
3. **nonce 持久化是追加日志**：若该文件整体损坏，重放窗口内的旧签名可能被再次接受，但**不会造成资金
   损失**——撮合侧账本幂等键（`trade:<TradeID>`）仍然阻止重复结算；最坏情形是多挂了一笔新订单。
4. **排序器串行化是单进程内存行为**：v1 单实例运行，不提供跨实例排序；横向扩展留待后续。
5. **运营方可拒绝服务/限流/停摆**：与子项目 1/3 信任模型一致（不可作恶致资金损失，但可拒绝服务）。

---

## 3. 架构概览

```
客户端（EIP-712 签名订单）
   │ HTTP REST（下单/撤单/查询）        │ WebSocket（订阅推送）
   ▼                                    ▼
┌────────────────────────────────────────────────────────┐
│  Gateway（新包 backend/gateway）                        │
│  ├─ Auth 中间件：EIP-712 验签 + nonce + 过期时间        │
│  ├─ RateLimiter：按账户令牌桶                           │
│  ├─ Sequencer：内存串行化 + 分配请求序号                  │
│  ├─ StateMachine：请求生命周期（received→accepted/rejected）│
│  ├─ HTTP handlers：下单/撤单/查询/深度/余额               │
│  └─ WS Hub：推送订单/成交/深度（按市场+账户过滤）          │
└──────────────┬─────────────────────────────────────────┘
               │ Place/Cancel（复用现有公开方法）
               ▼
┌────────────────────────────────────────────────────────┐
│  OrderBookService（子项目 3，不改）                      │
│  冻结→落日志→撮合→账本结算→释放→EventBus 发布            │
└──────────────┬─────────────────────────────────────────┘
               │ EventBus（orderbook.order/.trade/.degraded）
               ▼
        WS Hub 订阅 → 转发给客户端
```

**分层约束**：`backend/gateway` 只依赖 `OrderBookService` 的公开方法（`Place`/`Cancel`/`Engine()`/
`Holds()`）与 `EventBus` 事件，**不直接碰账本、不碰撮合内核内部、不 import `matching` 的内部状态**
（仅使用其导出类型构造 `PlaceRequest`/读取 `Result`）。排序器不引入新持久化。

---

## 4. 网关 `backend/gateway`

### 4.1 包职责与依赖方向

```
gateway ──► services.OrderBookService（公开方法 + EventBus）
        ──► matching（仅导出类型：PlaceRequest/Result/Order/Trade/Market/Side/Type/TIF/Price）
        ──► ledger（仅导出类型：AccountID/AssetID）
```

`gateway` 不 import `services` 的私有实现，不 import `matching` 的 `book`/`level` 内部结构。

### 4.2 EIP-712 域与类型

实现沿用 go-ethereum 的 `signer/core/apitypes`（`TypedData`/`TypedDataDomain`/`TypedDataMessage`）：
域与哈希由 `apitypes.TypedData.HashStruct` / `EncodeTypedData` 计算，验签用 `crypto.Ecrecover`。

域（纯链下验签，无承载合约，`VerifyingContract` 用零地址占位）：

```go
domain := apitypes.TypedDataDomain{
    Name:              "Web3 Orderbook DEX",
    Version:           "1",
    ChainId:           big.NewInt(cfg.ChainID), // 13700
    VerifyingContract: "0x0000000000000000000000000000000000000000",
}
```

**下单类型**（EIP-712 类型化数据，字段与 `matching.PlaceRequest` 一一对应 + nonce/expiry）：

```go
type SignedOrder struct {
    Market      uint32   // matching.MarketID
    Side        uint8    // 0=buy 1=sell
    Type        uint8    // 0=limit 1=market
    TIF         uint8    // 0=GTC 1=IOC
    Price       uint64   // 限价单必填
    Qty         string   // base 数量，十进制字符串
    MaxNotional string   // 市价买必填，十进制字符串；其余可空
    Nonce       uint64   // 账户内单调递增
    Expiry      uint64   // unix 秒；过期即拒
}

type SignedOrderEnvelope struct {
    Order     SignedOrder
    Signature []byte // 65 字节 r||s||v
}
```

**撤单类型**（同域、同 nonce 空间，独立 struct）：

```go
type SignedCancel struct {
    Market  uint32 // matching.MarketID
    OrderID uint64 // matching.OrderID
    Nonce   uint64
    Expiry  uint64
}

type SignedCancelEnvelope struct {
    Cancel    SignedCancel
    Signature []byte
}
```

- 验签：`hash` 由 `apitypes` 对上述 struct 计算 → `crypto.Ecrecover(hash, sig)` → 得 `Account`。
- `Qty`/`MaxNotional` 以十进制字符串传输，验签前 `uint256.FromDecimal` 解析，失败即 400。
- 验签失败返回 401；`Expiry < now` 返回 400（`order_expired`）；`Nonce <= 已用上界` 返回 409（`nonce_reused`）。
- **下单与撤单共用同一 nonce 空间**：`NonceStore.CheckAndAdvance(account, nonce)` 对两者一视同仁，
  一个签名（无论下单还是撤单）消耗一个 nonce，杜绝跨类型重放。

### 4.3 nonce 持久化

```go
type NonceStore struct {
    mu     sync.Mutex
    dir    string
    f      *os.File
    upper  map[ledger.AccountID]uint64 // 账户 → 已用 nonce 上界
}

func OpenNonceStore(dir string) (*NonceStore, error)  // 打开/创建 nonces.wal，重放重建 upper
func (n *NonceStore) CheckAndAdvance(a ledger.AccountID, nonce uint64) error  // nonce > upper 才通过并推进
func (n *NonceStore) Upper(a ledger.AccountID) uint64
func (n *NonceStore) Close() error
```

- 文件 `nonces.wal`，JSON 行 `{account, nonce}`，追加 + fsync（沿用子项目 3 `Journal` 的落盘风格）。
- `CheckAndAdvance` 在同一互斥锁内完成"校验 + 推进 + 落盘"，并发安全。
- 重启：`OpenNonceStore` 逐行重放，取每账户最大 nonce 作为上界。
- 目录为空（`dir == ""`）时为纯内存，不持久化（与 `Journal` 的内存模式一致）。

### 4.4 限流（按账户令牌桶）

```go
type RateLimiter struct {
    mu     sync.Mutex
    rate   float64   // 每秒补充 token
    burst  float64   // 桶容量
    tokens map[ledger.AccountID]float64
    last   map[ledger.AccountID]time.Time
}

func NewRateLimiter(rate, burst float64) *RateLimiter
func (r *RateLimiter) Allow(a ledger.AccountID) bool  // 取 1 token；不足返回 false
```

- 标准令牌桶：按账户独立桶，`now - last` 补充 `rate * elapsed`（封顶 `burst`）。
- `Allow` 返回 false 时 handler 返回 429。
- 桶条目惰性创建；`rate <= 0` 视为不限流。

### 4.5 排序器（纯内存串行化）

```go
type Sequencer struct {
    mu  sync.Mutex
    seq uint64
    ob  *services.OrderBookService
}

func NewSequencer(ob *services.OrderBookService) *Sequencer
func (s *Sequencer) Place(req matching.PlaceRequest) (matching.Result, uint64, error)
func (s *Sequencer) Cancel(market matching.MarketID, a ledger.AccountID, id matching.OrderID) (matching.Result, uint64, error)
```

- 一个互斥锁串行化所有写操作；`seq` 为网关分配的请求序号（单调递增，仅用于请求关联与日志，**不**参与撮合）。
- 撮合的 `OrderID`/`TradeID` 仍由子项目 3 内核按指令顺序分配（确定性不变）。
- 排序器不落盘、不缓冲队列；只是"先到先得"的串行入口。
- **`seq` 即请求 ID**：排序器返回的 `uint64` 就是 `StateMachine` 记录的 `RequestID`，二者是同一个值，
  不在排序器与状态机之间各维护一套独立序号。

### 4.6 请求生命周期状态机

```go
type RequestState uint8
const (
    StateReceived  RequestState = iota  // 已接收，待处理
    StateAccepted                       // 已转发给撮合
    StateRejected                       // 验签/校验/限流失败
)

type RequestRecord struct {
    RequestID uint64
    State     RequestState
    OrderID   matching.OrderID // StateAccepted 且有成交/挂单时非零
    Err       string           // StateRejected 时的原因
}

type StateMachine struct {
    mu       sync.Mutex
    requests map[uint64]RequestRecord
    byOrder  map[matching.OrderID]uint64 // orderID → requestID（用于撤单/查询回链）
}

func (sm *StateMachine) Begin(requestID uint64)                 // 记录 received（requestID 由排序器分配）
func (sm *StateMachine) Accept(requestID uint64, orderID matching.OrderID)
func (sm *StateMachine) Reject(requestID uint64, err error)
func (sm *StateMachine) Get(requestID uint64) (RequestRecord, bool)
func (sm *StateMachine) RequestForOrder(orderID matching.OrderID) (RequestRecord, bool)
```

- 仅跟踪**网关视角**的生命周期；订单撮合状态（new/filled/cancelled…）由内核的 `Result.Order.Status`
  与事件给出，状态机不复制。
- 状态机内存态，不持久化；崩溃后由指令 WAL 重建订单簿，网关请求记录丢弃（不影响正确性）。
- `requestID` 由排序器统一分配（见 4.5），状态机不自增、不另发序号。

### 4.7 HTTP API（gin）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/v1/orders` | 签名 | 提交签名订单；同步返回订单与成交 |
| DELETE | `/v1/orders/:id` | 签名 | 签名撤单 |
| GET | `/v1/orders/:id` | 无 | 查询订单状态 |
| GET | `/v1/markets` | 无 | 市场列表 |
| GET | `/v1/book/:market?depth=N` | 无 | 深度快照（买/卖各前 N 级） |
| GET | `/v1/accounts/:account/balances` | 无 | 余额与可用余额 |

- 下单响应：`{requestId, order:{...}, trades:[...]}`；错误映射：验签 401、过期/参数 400、重放 409、
  限流 429、余额不足 422、降级 503。
- 撤单请求体：`{signature, nonce, expiry, market, orderId}`（同为 EIP-712 签名，见 4.2 `SignedCancel`）。

### 4.8 WebSocket（`/ws`）

- 上行订阅消息：`{op:"subscribe", market?:number, account?:string}` / `{op:"unsubscribe", ...}`。
- 下行推送帧：`{type:"order", ...}` / `{type:"trade", ...}` / `{type:"depth", ...}` /
  `{type:"degraded", ...}`。
- Hub 维护 `client → 订阅过滤器集合`；订阅 `EventBus` 的 `orderbook.order`/`orderbook.trade`/
  `orderbook.degraded` 事件，按市场 + 账户过滤后广播。
- 深度推送：在下单/成交导致簿变化后，读取 `OrderBookService.Engine().Book(market, depth)` 生成快照广播
  （v1 简化：每次有变化就推全量深度，不做增量压缩）。
- 客户端断线即移除；写阻塞时断开连接避免反压拖垮 hub。

---

## 5. 服务装配

`backend/gateway` 暴露一个装配函数，`main` 在其后调用：

```go
type Gateway struct { /* router, hub, sequencer, state, nonce, limiter */ }

func New(ob *services.OrderBookService, bus *services.EventBus, cfg Config) (*Gateway, error)
func (g *Gateway) Router() *gin.Engine   // 供 main 挂载到现有 gin server 或独立端口
func (g *Gateway) Close() error          // 关闭 hub、nonce store
```

- `main` 在 `BuildSettlementStack` 成功后、`OrderBook != nil` 时装配 Gateway，挂载 `/v1/*` 与 `/ws`。
- 配置经 `services.Config` 桥接（见第 7 节），不新开进程。

---

## 6. 错误码约定

| HTTP 状态 | 含义 | 错误标识 |
|-----------|------|----------|
| 400 | 参数/签名结构/过期 | `bad_request` / `order_expired` / `bad_signature` |
| 401 | 签名无效（恢复账户失败） | `unauthorized` |
| 409 | nonce 重放 | `nonce_reused` |
| 422 | 余额不足 / 订单被撮合拒绝 | `insufficient_balance` / `order_rejected` |
| 429 | 限流 | `rate_limited` |
| 503 | 订单簿致命降级 | `service_degraded` |

---

## 7. 配置

| 键 | 含义 | 默认 |
|----|------|------|
| `GATEWAY_ADDR` | 网关监听地址（若与 AMM API 分离则独立端口） | `:8080` |
| `EIP712_DOMAIN_NAME` | EIP-712 域名 | `Web3 Orderbook DEX` |
| `EIP712_DOMAIN_VERSION` | EIP-712 版本 | `1` |
| `GATEWAY_RATE` | 每账户每秒下单/撤单 token 补充速率 | `10` |
| `GATEWAY_BURST` | 每账户令牌桶容量 | `20` |
| `GATEWAY_NONCE_DIR` | nonce 持久化目录（空则内存） | 空（内存） |

`CHAIN_ID` 沿用现有 `config.SettlementConfig.ChainID`（默认 13700）。

---

## 8. 测试策略

### 8.1 单元测试（Go，表驱动）

- **EIP-712**：合法签名恢复正确账户；篡改字段验签失败；过期拒绝；`Qty`/`MaxNotional` 非法拒绝；
  下单与撤单签名互不通用（不同类型化数据）。
- **nonce**：`nonce > upper` 通过并推进；`nonce == upper` 拒绝；`nonce < upper` 拒绝；重启重放重建上界；
  并发 `CheckAndAdvance` 不重复通过；下单与撤单共用同一 nonce 空间。
- **限流**：桶耗尽返回 false；补充后恢复；不同账户互不影响；`rate <= 0` 不限流。
- **排序器**：并发 Place 串行化（seq 严格递增）；返回的撮合结果与顺序一致。
- **状态机**：received → accepted/rejected 转换；`RequestForOrder` 回链。
- **handlers**：下单/撤单/查询/深度/余额各路径的成功与错误码映射。

### 8.2 集成测试

- **端到端签名下单**：签名 → POST /v1/orders → 账本余额变化正确、`Σ余额` 守恒。
- **重放拒绝**：同一签名提交两次，第二次 409。
- **限流**：单账户超发 429。
- **WS 推送**：订阅某市场后，下单/成交触发对应推送帧，未订阅账户/市场不收到。
- **降级传播**：注入 `OrderBookService` 致命降级 → 下单返回 503。
- **崩溃自洽**：nonce 落盘后重启，旧 nonce 被拒。

### 8.3 与子项目 3 的一致性

- 签名订单字段 → `matching.PlaceRequest` 的映射是**无损且一一对应**的（金额用十进制字符串，避免
  uint256 浮点/科学计数歧义）。
- 网关不改变撮合内核的 `OrderID`/`TradeID` 分配，重放指令 WAL 的确定性不被网关破坏。

---

## 9. 文件布局

```
backend/
  gateway/
    gateway.go          # 装配：router + hub + 中间件
    eip712.go           # EIP-712 域、订单类型、验签、恢复账户
    eip712_test.go
    nonce.go            # nonce 持久化 + 重放拒绝
    nonce_test.go
    ratelimit.go        # 按账户令牌桶
    ratelimit_test.go
    sequencer.go        # 内存串行化 + 请求序号
    sequencer_test.go
    state.go            # 请求生命周期状态机
    state_test.go
    ws.go               # WebSocket hub + 订阅过滤
    ws_test.go
    handlers.go         # HTTP handlers
    handlers_test.go
  config/config.go      # 改：GATEWAY_* 配置
  services/stack.go     # 改：可选装配 Gateway（或由 main 装配）
  main.go               # 改：挂载 /v1/* 与 /ws
  README.md             # 改：网关接口、签名格式、残余假设
docs/superpowers/specs/2026-10-18-orderbook-dex-gateway-design.md      # 本文档
docs/superpowers/plans/2026-10-18-orderbook-dex-gateway.md             # 实施计划
```

---

## 10. 已定决策记录

| 决策 | 结论 |
|------|------|
| 鉴权 | 签名订单（ECDSA），无会话、无状态 |
| 签名方案 | EIP-712 类型化数据；域 `Web3 Orderbook DEX` v1 |
| 接口形态 | HTTP 读写 + WebSocket 推送 |
| 排序器 | 纯内存串行化层，分配请求序号（不参与撮合） |
| 限流 | 按账户令牌桶 |
| 状态机 | 请求生命周期 received → accepted/rejected |
| 防重放 | nonce（账户内单调）+ 过期时间（unix 秒） |
| nonce 持久化 | 追加文件 `nonces.wal`，重启重放重建上界；空目录 = 内存 |
| WS 推送 | 订单 + 成交 + 深度，按市场 + 账户过滤 |
| 读接口鉴权 | v1 读接口不鉴权，写接口要求签名 |
| 包定位 | 新包 `backend/gateway`，旧 `api`/`handlers`（AMM）不动 |
| 分层约束 | gateway 只依赖 OrderBookService 公开方法 + EventBus + 导出类型 |
| 本轮未做（显式延后） | 链上批量结算、永续/风险、行情/K 线、订单 TTL、跨实例排序、API key、TLS |
