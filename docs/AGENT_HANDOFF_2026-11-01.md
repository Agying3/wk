# WorkBuddy2API 交接文档

更新日期：2026-11-01（Asia/Shanghai）。本文记录本地源码及已有验证证据，不代表生产实例状态。

## 1. 接手时先看这里

- 项目目录：`E:\WorkBuddy2API`。
- 仓库：`https://github.com/gao-gao-zai/wk`，分支 `main`。
- 本文记录的代码基线为 `f47422fe3ec35038684655ee683bfaf28661e8e3`，此前已核对远端 main 同一提交；交接文档另行提交，不改变该代码基线及验证结论。
- 本次文档提交前，源码工作区只有未跟踪的 `.build-test.json`，这是本地构建位置偏好，保留，不提交。
- 初稿仅保存在本地。用户于 2026-11-01 明确要求提交推送现有改动，因此将本文件显式纳入 Git；其他被忽略的 `docs/` 文件及构建证据不随本次提交上传。
- **部署仍暂停。** 本轮没有部署、重启服务或执行真实上游登录；线上当前版本未核验，不能把 Git 已推送当成线上已更新。
- 用户最新请求是提交推送现有改动。下述待办是后续工作建议，不代表已经实施，也不授权自动恢复部署或操作真实账号。

参考项目是 `yreidev/workbuddy2api-panel`，本轮比较固定在：

- 提交：`39049fcc479c1240d0d80d0404e0308f651424de`。
- [客户端研究说明](https://github.com/yreidev/workbuddy2api-panel/blob/39049fcc479c1240d0d80d0404e0308f651424de/docs/official-client.md)。
- 更早提到的 `linguo2625469/workbuddy2api-panel` 不是本轮的主要比较基准。
- 接手后若比较新的参考版本，应重新确认其提交；不能将本文中的固定版本比较称为“最新完整对比”。

## 2. 用户约定与工作范围

用户最初关注多账号请求的关联特征，后续要求逐步同步参考项目的部分行为。当前已明确区分协议兼容、隐私隔离和客户端外观模拟，不能混为一谈。

必须保留的约定：

1. 优先保证正常使用，保留主对话原有重试、换号策略及账号日常代理粘性；不要为了模仿参考项目直接改成强制失败。
2. 已有账号的机器标识和硬件档案保持不变；不能因软件升级、重新登录、刷新令牌或代理变化重新分配。
3. 服务端首次添加新账号时默认建立 Mac 档案；已有 Windows 档案继续支持。尚未开放新增 Windows 设备选择。
4. 兼容任务行为按用户要求保留；有真实对话的统计使用真实数据，不能把未执行的动作补成“真实发生”。
5. 用户先前已要求提交推送，代码改动已按该要求推送；部署后来被明确暂停，未恢复。
6. 用户已表示不需要操作运行实例的测速设置，不要擅自切换测速模式或启动测速。

最近明确的可继续范围：网页业务的真实来源与认证隔离、实际压缩能力与解码一致性、真实事件统计、登录兼容性及凭据正确保存。

## 3. 已提交的代码进度

| 提交 | 已完成内容 | 重要限制 |
| --- | --- | --- |
| `8d6c62f` | 主服务接入 uTLS；同一逻辑对话的出站会话、追踪关联在换号时延续 | 不是所有 HTTP 调用点都走统一 Client；关联 ID 仍可串联这一轮请求 |
| `161f592` | 集中管理区域客户端版本、隐私偏好；刷新请求合并及可选身份检查；授权元数据保存 | 刷新默认兼容模式；不是默认全面严格拒绝 |
| `0de7858` | 每账号持久设备档案；服务端新增账号默认 Mac，旧 Windows 档案保留 | 不能覆盖既有设备；独立脚本另有待审计事项，见第 7 节 |
| `3481784` | 秒/毫秒到期时间兼容；专家响应解析；个人资源新接口；模型目录；压缩响应处理；刷新并发修正 | 企业余额仍走兼容路径；新接口失败有受限回退 |
| `11a4411` | 完成上一轮选定的七项核心适配，见下节 | 七项完成不等于整个参考项目的全部请求外观一致 |
| `f47422f` | 正常浏览器授权迁移；旧 CLI state 兼容；登录等待与失败区分；前端处理和产物更新 | 未迁移短信直登或自动加号；未做真实扫码联调 |

### 3.1 `11a4411` 的七项范围

1. **模型能力缓存隔离**：按区域、UID、企业、账号类型、入口隔离；UID 为空时另用令牌摘要隔离。选定账号后再处理推理档位，不改共享请求体。缓存有边界及过期时间。
2. **授权元数据贯通**：扫码、短信、服务端入库、刷新和迁移中保留实际上游返回的账号类型、授权来源、部门、令牌类型等字段；不虚构缺失字段。
3. **业务码提取**：支持常见嵌套和数字/字符串业务码，错误对象保留业务码；没有整体替换原换号、冷却策略。
4. **网页 Cookie 认证与核心适配**：按账号及授权上下文隔离内存 Cookie；换登录码、导航、业务请求分开；只在明确 401 时重建一次认证并保持原业务载荷/幂等标识；不因网络超时盲目重放写请求。
5. **入口集中配置**：提供 legacy/product 及显式入口覆盖；默认仍保留旧入口，没有添加代理绕过或自动直连回退。
6. **真实专家属性**：从专家返回数据决定有关业务标志，不凭专家 ID 编造付费或云端属性。
7. **条件推理摘要**：明确请求推理档位时补充默认摘要行为，保留调用方显式设置，不自动添加长期缓存保留字段。

### 3.2 当前版本及默认行为

- `client_version_cn=5.9.3`，`client_version_global=5.9.4`，`cli_version=2.165.0`。
- 主服务默认 `header_profile=desktop`，但旧兼容实现仍存在，不能全仓搜索到旧常量就断言当前所有请求都使用旧版本。
- `refresh_identity_check=best_effort`，可选 `strict` / `off`。
- 服务端默认 `web_auth_mode=auto`；库的零值行为与服务端显式默认配置不同。
- `endpoint_profile` 默认 legacy；支持配置 product，尚未强制切换生产入口。
- 主服务 `tls_fingerprint` 默认开启；独立扫码辅助程序、TTFB 等自行创建客户端的路径不受该开关全面覆盖。
- 设备档案保存在原凭据文件的 `_device_identity`、`_device_profile` 中；不要输出原始凭据文件。

## 4. 最近一次扫码迁移的确切结果

修改范围是正常浏览器授权辅助程序和管理界面：

- `cmd/login/main.go`：新授权按国内/国际区域使用产品授权平台；国内读取 `/v2/plugin/account`，国际保留对应 state 账号读取，并执行有时限的可选账号快照读取。
- 新 state 保存 `flow=desktop`。升级前没有 flow 的 state 继续使用原 CLI 请求及账号读取方式，不强迫重新开始。
- 新登录辅助程序**当时**自报 `workbuddy2api-login`；2026-11-01 已按官方后台服务形状对齐（UA、请求框架头、登录观测链路头），见第 11 节。
- API 请求不自动跟随重定向；错误不回显上游响应体、授权 URL、令牌或任意上游消息。
- `internal/auth/login_failure.go` 定义辅助程序与服务端之间的非敏感错误格式。
- 只有获取令牌阶段明确返回等待授权码时，才作为待授权结果；其他错误保留失败阶段、HTTP 状态及业务码。
- `internal/server/handler.go` 区分待授权和失败；`react/src/pages/AccountSettings.jsx` 只对明确待授权继续自动轮询。
- 前端已在 tdog 重建，已提交对应 `frontend/` 产物。

**自动加号没有跟着变成产品授权：**

- `internal/server/autoenroll.go` 仍调用 `smslogin.Send` / `Verify`。
- `internal/smslogin/smslogin.go` 仍是 OneID → Keycloak → Console → CLI 票据交换链路。
- 其中旧 CLI 平台及 UA 仍存在。这里的 CLI 是 HTTP 授权协议，不是启动 CLI 进程。
- 没有证据证明只替换平台参数就能迁移这条短信链路；不要宣称已完整迁移所有登录入口。

## 5. 与参考项目仍存在的可观察差异

这一节是事实比较，不是待实施的隐藏关联清单，也不能据此断言上游实际用了某项特征关联或限制账号。

| 类别 | 本项目当前情况 | 参考项目固定版本的情况 |
| --- | --- | --- |
| 扫码登录身份 | 2026-11-01 起按官方后台服务形状发（UA、框架头、登录观测）；`X-Device-Token` 仍缺 | 模拟更多官方客户端身份及登录观测字段 |
| 网页请求 | 2026-11-01 起按业务分来源页并补浏览器元数据头；`Sec-CH-UA` 为推算值 | 区分业务来源页面，并补充更多浏览器元数据；其中部分字段作者注明未抓包确认 |
| 压缩协商 | 2026-11-01 起按请求类别分别声明，并支持 `compress` 解码 | 按不同请求客户端类别分别声明 |
| 构建信息上报 | 现代模式省略尚未核实的构建哈希和日期 | 填写其研究提取的构建信息 |
| 设备档案 | 保留旧 Windows，新账号持久 Mac 档案，允许硬件组合重复 | 统一选择一套 Mac 硬件配置，标识按账号固定 |
| 兼容事件 | 部分未真实执行任务仍有历史兼容事件和固定数值 | 不能据其说明假定所有事件都来自真实动作；要逐个验证 |

uTLS、稳定账号设备档案、Cookie 按账号隔离已经有实现，不要重复列为“完全没同步”。主对话会话 ID 在逻辑请求换号时继续关联，也不能宣传成这些请求互不关联。

其他功能差异仍包括：专家搜索/翻页、完整业务错误分类、容量排队、网页部分状态预查询、模型附加目录信息、独立联网搜索等。这些属于功能和兼容范围，不能直接包装成“反追踪优化”。

## 6. 验证证据及产物

所有检查在 SSH `tdog` 宿主机执行，没有在本机编译或测试，没有真实供应商请求。

### 6.1 `11a4411`

- 本地证据：`.build-out/protocol-sync-20261101-r4/`、`.build-out/protocol-sync-20261101-r5/`。
- r4：全项目 `go test ./...`、相关 race、全项目 vet、Linux server/login 构建通过。
- r4 后还有两项小修正：空 UID 缓存隔离、网页登录码客户端丢弃继承的 Cookie jar。
- 最终 r5：auth/endpoints/upstream/smslogin/cmd 相关包回归、相关 race、全项目 vet、Linux server/login 构建通过。
- **不能把 r4 的全项目回归称为最终 r5 源码的全项目回归。** 具体命令和差异写在 r5 的 `verification.txt`。
- 最终源码快照 SHA256：`720ccb66439e2d19871d765883294c9609b239b54e6bf076e94de685487de59e`。
- 远端最终产物：`/srv/build-server/artifacts/workbuddy2api/protocol-sync-20261101-r5/`。

### 6.2 `f47422f`

- 本地证据：`.build-out/login-migrate-20261101-r1/verification.txt`、`check.log`、`check.sh`、`snapshot-files.txt`、`SHA256SUMS`。
- 对应基线：`11a4411` 加本次迁移源码；源码快照 SHA256：`6a16108a9246dcfc71ebbbb99fdaca9e87100e8b9c39f8fb87e9f1c87dbf9baa`。
- 前端既有单元测试：56 通过、0 失败；生产构建通过。
- `go test ./cmd/login ./internal/auth` 通过。
- server 的登录、凭据保存、设备、混合区域和原子写入相关定向回归通过。
- `go test -race ./cmd/login` 通过。
- `go vet ./cmd/login ./internal/auth ./internal/server` 通过。
- Linux amd64、CGO 关闭的 login/server 构建通过。
- 没有执行最终版本的全项目 `go test ./...`，不要扩大验证结论。
- 登录测试使用本地 `httptest` 桩；真实国内、国际扫码结果仍待实际用户联调。

远端目录：

```text
/srv/build-server/sources/workbuddy2api/login-migrate-20261101-r1/src/
/srv/build-server/logs/workbuddy2api/login-migrate-20261101-r1/
/srv/build-server/artifacts/workbuddy2api/login-migrate-20261101-r1/
```

最终产物 SHA256：

```text
login             05959029e4c1367d2ce4be9dd846713f4a0176bbe2d2d207b461694e2e8d95d8
wb2api            5ce247294e2dafa2f1c621a974179651072a864f0a260e4e5e290d990d58133e
frontend.tar.gz   fe6e583d0e5891248d88a451c51c80cca6475c87eda8548d4a72b002026fc2c6
```

前端依赖锁与已有构建目录完全相同，本轮使用任务目录内的依赖链接复用，并用 Vite `configLoader=native` 避免配置打包写入共享依赖目录；未升级依赖。构建过程和该依赖来源记录在 `check.sh` 中，不假定接手时目录仍可用。

## 7. 下一步建议及验收标准

### P0：审计独立 `login.sh` 的凭据保存一致性

这是编写本文时的静态检查发现，**已于 2026-11-01 晚按第 161 行的验收要求修复**（见第 11.6 节第 1 条）：落盘改走 `login save`，设备档案与授权元数据由 Go 侧沿用，并有隔离联调记录。以下保留当时的原始记录：

- `login.sh` 目前重新构造只含 `auth` 和 `account` 的文档，然后覆盖原授权文件。
- 该路径可能丢失既有 `_device_identity`、`_device_profile`，也未完整保存辅助程序返回的授权元数据。
- 服务端 `persistAccount` 路径已经保留既有设备，不能把它的保证直接推广到独立 shell 包装脚本。
- 脚本还包含自动编译、真实签到、配置修改和 Docker 重启步骤，**不能把直接运行整个脚本当成离线测试**。

后续可在用户继续要求修改时，先把脚本落盘行为与服务端一致化。验收应覆盖：已有 Mac/Windows 档案逐字段保留、授权字段采用本次真实返回值、坏文件不静默覆盖、写入原子性及权限、全程使用假凭据。独立测试应隔离落盘代码，不能触发脚本中的签到或服务重启。

### P1：网页来源与认证隔离的兼容性核对

以实际业务页面为依据审计来源字段，检查 Cookie 是否只用于所属账号/入口，确认跳转及失败路径不传播令牌。补充本地桩测试，再考虑必要改动。不要把补造浏览器身份或未知客户端字段作为验收目标。

### P1：实际压缩能力与声明一致性

核对主服务和独立登录程序的能力声明、解压、体积限制、取消和 SSE 流式行为。所有声明应由实际支持的能力决定；不为模拟其他运行时而声明未实现能力。现有压缩解码已经完成，避免重复实现。

### P1：真实事件数据及版本信息

已有真实调用时，核对耗时、token、成功状态的观测来源；缺乏真实动作的兼容事件不补造随机“真实统计”。构建哈希/日期只使用已核实的信息。保留用户此前的兼容任务约定，若要改变业务行为，先明确新的范围与消耗。

### 后续可选的普通功能任务

- 专家列表搜索和分页：当前第一页上限 20 条。
- 更完整的业务码分类和错误提示：错误码提取已有，处置策略没有全面更换。
- 容量排队：当前没有完整排队流程；先明确等待、超时、取消与既有换号逻辑如何衔接。
- 网页业务状态预查询、模型目录可见范围及默认能力：按实际使用需求评估。
- 多进程共享凭据时的刷新恢复：有真实场景再评估，不能未经核对增加刷新重试。

上述功能没有在本文中实现；不能据此前“继续同步”的笼统历史要求，将所有项目一次性投入生产。

## 8. 关键代码入口

| 路径 | 用途 |
| --- | --- |
| `cmd/login/main.go`、`main_test.go` | 正常浏览器授权、旧 state 兼容、模拟接口测试 |
| `internal/auth/login_failure.go` | 非敏感登录错误契约 |
| `internal/auth/login_account.go` | 账号及授权元数据解析 |
| `internal/server/handler.go` | 登录辅助程序调用、账号持久化 |
| `internal/server/login_test.go`、`account_device_test.go` | 登录错误及设备保留回归 |
| `react/src/pages/AccountSettings.jsx` | 管理界面登录轮询 |
| `internal/smslogin/smslogin.go`、`internal/server/autoenroll.go` | 保留的短信及自动加号链路 |
| `internal/device/profile.go`、`internal/auth/device_profile.go` | 已有设备记录与持久化 |
| `internal/upstream/client_settings.go` | 区域客户端版本和请求头配置 |
| `internal/upstream/website_session.go` | 网页认证、Cookie 隔离与受限重试 |
| `internal/upstream/model_capabilities.go` | 模型能力缓存隔离 |
| `internal/upstream/business_code.go` | 业务码读取 |
| `internal/upstream/response_encoding.go` | 压缩协商及惰性解码 |
| `internal/endpoints/settings.go` | 入口集中配置 |
| `internal/upstream/fingerprint_transport.go`、`chat_trace.go` | 现有 TLS 与出站关联实现 |
| `login.sh` | 旧 shell 包装脚本，含外部副作用，不可直接用于离线验证 |

## 9. 接续工作的操作约定

1. 先读取适用的 AGENTS 和 README，检查 `git status`、HEAD、相关代码与证据；不要重做已经完成的七项。
2. 构建、依赖安装、自动化测试、vet、前端构建都在 **SSH tdog 宿主机**执行。本机仅编辑、Git、打包、同步与读取日志，格式化允许本地执行。
3. 执行前读取 `C:/Users/10403/.codex/skills/tdog-build-test/SKILL.md`；本项目 `.build-test.json` 已选择 tdog，不重复询问位置。
4. 每轮编译/测试前先检查：`ssh -o BatchMode=yes -o ConnectTimeout=12 tdog 'true'`。连不上则报告未运行的检查，不回退本机或其他服务器。
5. 远端显式加载 `/srv/build-server/env/workbuddy2api.sh`，使用新的隔离任务目录。同步本次未提交改动，记录清单、基线和快照 SHA256。
6. 不上传真实凭据、运行配置、账号池、数据库、备份或登录态；测试使用假凭据和本地桩。
7. 最近检查 tdog 根分区只剩约 600 MB，构建完成后情况可能变化。接手先看磁盘，不删除其他任务、项目缓存或历史产物来腾空间。
8. 前端源码有改动时，在 tdog 构建并回传对应产物；不留下源码已改但嵌入资源未更新的提交。
9. 不在交接、日志或命令输出中包含令牌、Cookie、验证码、原始账号凭据或手机号。构建哈希和源码提交哈希可以记录。
10. 纯文档变更无需编译测试。新增可复用 `tools/` 脚本需要用户明确要求；临时文件放 `tmp/` 并在使用后清理。
11. 部署保持暂停；恢复部署须有新的明确请求，并读取 `server-deploy` 技能。生产版本、入口和持久数据都要重新核实，不能凭历史对话猜测。

## 10. 给接手 agent 的简短起始任务

> 先核验本地与远端提交以及本文证据。当前已推送 f47422f，正常扫码协议已迁移，短信/自动加号未迁移，生产部署暂停。优先审计 login.sh 是否丢失既有设备与授权元数据，再按用户明确的新请求推进兼容、隐私隔离或真实统计方面的工作。验证只在 tdog 使用隔离源码和本地桩执行。不要把参考项目全部外观差异直接转成待实施任务。

## 11. 2026-11-01 追加：网页请求特征、登录身份与压缩协商

用户明确要求同步这三项。**改动尚未提交、未部署**；仍以 `f47422f` 为已推送基线。

### 11.1 改动

- 新增 `internal/upstream/web_headers.go`：官网网页业务请求、网页顶层导航、网页登录码（Node 内置 fetch）、小程序通道的头集中在这里。
  - 来源页按业务分：成长中心 `/profile/growth-center`；积分补偿、新手礼包 `/profile/usage`；参数里若出现 trial 走 `/profile`，account 走 `/profile/account-settings`。
  - 网页业务只带 `X-Enterprise-Id`（企业号）；**不发** `X-User-Id`、`X-Tenant-Id`、`X-Domain`、`X-Product`、`X-Request-ID`、`X-Requested-With`；靠 Cookie 认人，`Authorization` 只用于退回 bearer。
  - 补浏览器自动头：`Sec-Fetch-Site/Mode/Dest`、`Sec-CH-UA`（按 Chrome 152 推算，未抓包）、`Sec-CH-UA-Mobile/Platform`（平台跟设备档案走）、`Accept-Language`（成长中心接口显式 zh-CN/en-US，其余 Chrome 默认）。
  - 网页登录码请求改用 undici 形状：`User-Agent: node`、`Accept: */*`、`Accept-Language: *`、`Sec-Fetch-Mode: cors`。此前该请求**没有设 UA**，实际发出的是 `Go-http-client/1.1`。
  - 小程序通道 `applyMPHeaders`：`X-Client-Platform: miniprogram` + `Bearer`、微信 iOS web-view 的 UA、`Accept-Language: zh-CN`、iOS WebKit 编码组，不带 UA-CH / `Sec-Fetch-*`。用于 `ReportMPEvent` 与开学季接口（`schoolJSON`，此前它们走请求框架那一组）。
- 压缩协商（`response_encoding.go`）：四组常量（axios / Node fetch / Chrome / iOS WebKit），**四类都已接到具体调用点**：axios = 对话/后台服务/billing/模型目录（`commonHeaders`、`billingHeaders`、`doUpstream` 兜底）；Node fetch = 网页登录码（`applyNodeFetchHeaders`）；Chrome = 网页业务与顶层导航（`applyWebHeaders`、`applyWebNavigationHeaders`）；iOS WebKit = 小程序上报与开学季（`applyMPHeaders`）。新增 `compress` 解码（照 axios 的 zlib.createUnzip，按头部区分 gzip 与 zlib）；`AcceptEncodingAxios` 导出给登录辅助程序共用。
- 扫码登录身份（`cmd/login/main.go`）：UA 改为后台服务形状 `<应用名>/<版本> <应用名>/<版本> CLI/<cli>`；加 `X-Requested-With`、`X-Request-ID`、`X-Product: SaaS`、发起授权的 `X-Custom-Channel-Id: unknown`；**去掉** `Origin`/`Referer`（官方这两类请求不发）；新增登录观测：`loginSessionId` 存进 state、每次登录一条根 span（`X-Trace-ID`/`b3`/`X-B3-*`）、每类请求一个子 span 的 `traceparent`（由会话 id 派生，多次轮询共用）、授权链接追加 `version`（国内另加 `traceparent`、`loginSessionId`）。国际版发起授权按官方只带 `traceparent`。因为显式声明了 `Accept-Encoding`，`doJSON` 现在自己按 `Content-Encoding` 解码。旧 state 的 CLI 形状（`legacyHeaders`）原样保留。
- `header_profile=legacy` 的分支一律保持历史形状，不随本次对齐改动。

### 11.2 仍然没同步的（有意）

- `X-Device-Token`：腾讯图灵盾原生 SDK 只有 macOS/Windows 实现，Linux 上做不出真令牌；参考项目同样不做，只在用户填了令牌时才发。不伪造 `X-Device-Token-Error`。
- 小程序 UA：仍是微信 iOS web-view 形状，但**小程序 appid 未核实**，不带 `miniProgram/<appid>` 段；iOS 与微信版本号是挑选的近期值，未抓包。
- 事件公共字段的 `commit`/`releaseDate`：桌面模式继续省略（未核实）；legacy 模式的硬编码旧值仍在，属遗留问题。
- 设备配置：不改存量账号，新账号仍随机 8/16/24 GB。

### 11.3 验证

全部在 tdog 隔离目录执行，源码为上述未提交工作树：

- 路径：`/srv/build-server/sources/workbuddy2api/web-login-sync-20261101-r1/src`
- 日志：`/srv/build-server/logs/workbuddy2api/web-login-sync-20261101-r1`
- 产物：`/srv/build-server/artifacts/workbuddy2api/web-login-sync-20261101-r1`
- 源码树 SHA256：`237352af1ab2a58f2b1ac8a515b79991f418860fbee650e563d726aa646f21a0`
- `go test ./...` exit 0（26 包）、`go vet ./...` exit 0、`-race`（upstream + cmd/login + server）exit 0、Linux amd64 CGO 关闭的 login 与 server 构建均 exit 0、`bash -n login.sh` exit 0。
- 产物 SHA256：`login 5253d54b0843113cf3374a88f2d476282e0946d6d2d0b791dc58ba928fdd62d6`、`wb2api a11ad3255f027c53eb741337e2d8c49a69abf7cc271f40cdb885aa3acf9eef11`。
- 隔离落盘联调（假凭据，只跑 `login save`，不触发签到/重启）：新账号得到 macos 档案且权限 0600；已有 Windows 档案重新登录后逐字段保留；uid 不匹配时拒绝写入且原文件未改。
- 本地证据副本：`.build-out/web-login-sync-20261101-r1/`。
- 未执行：前端构建（无前端源码改动）、真实上游请求、登录脚本的签到与容器重启步骤、部署。

### 11.4 验证状态（不要当成已确认可用）

| 项 | 代码 | 单元/桩测试 | 真实上游联调 |
| --- | --- | --- | --- |
| 压缩声明与解码（四类） | 已完成 | 已覆盖（含 `compress` 的 gzip/zlib 两种包装） | 未做 |
| 网页业务头与来源页 | 已完成 | 已覆盖 | 未做 |
| 网页登录码（undici 形状） | 已完成 | 已覆盖 | 未做 |
| 小程序通道（编码/平台/UA） | 已完成 | 已覆盖 | 未做 |
| 扫码登录身份与观测字段 | 已完成 | 已覆盖 | 未做（无真实扫码） |
| `login.sh` 落盘一致性 | 已完成（改走 `login save`） | 已覆盖 + 隔离 shell 联调 | 未做（真实账号） |

- `Sec-CH-UA`、`Sec-Fetch-*`、子 span 的 `traceparent`、小程序的 iOS/微信版本号：都来自官方代码/规范的推算，**没有抓包或真机对照**。
- 上游是否接受新形状、登录是否照常完成，只有本地 `httptest` 桩验证，不能据此宣称"已对齐并可用"。

### 11.5 本轮自查修正（2026-11-01 晚）

本节初稿有两处与代码不符，已修正：

1. 声称"四组常量按调用点声明"，实际当时 `acceptEncodingWebKit` 只有定义、没有任何调用点，`ReportMPEvent` 与开学季接口仍走 `billingJSON` 的 axios 组。现已补 `applyMPHeaders` 并接入这两条路径，补了三条测试（单元、上报端到端、开学季 API）。
2. 本节 11.2 曾写"小程序编码与平台标记已是对的"，实际当时平台标记仍是 `web`、编码是 axios 组；现已改对。

教训：这类"常量定义了但没接线"的缺口，`go test` 和 `go vet` 都不会报（Go 允许未使用的包级常量），只能靠逐条 grep 调用点确认。

### 11.6 第二批修正（2026-11-01 深夜）

用户逐条指出五处缺口，均已修：

1. **独立登录脚本丢设备档案**：`login.sh` 原来用内联 Python 重建只含 `account`/`auth` 的文档后覆盖，会丢 `_device_identity`、`_device_profile`，新账号也不会建 Mac 档案。改为新增 `login save` 子命令（`cmd/login/save.go`）：已有账号沿用磁盘设备与授权元数据、新账号 `device.NewMac()`、写入走 `auth.Auth.SaveAtomic()`（0600 原子替换、空 token 拒绝、uid 不匹配拒绝）。脚本落盘段改为把 poll 结果管道给 `login save`，不再自己拼 JSON。
2. **网页上报的来源页没贯通 Cookie 路径**：`websiteJSON` 增加 `page` 参数，`applyWebHeaders` 也接受显式来源页；`ReportWebEvent` 在 Cookie 路径与 Bearer 回退路径都用调用方给的 `pageURL`。此前 Cookie 路径被统一改写成成长中心。
3. **扫码版本配置没贯通**：`loginCommand` 现在把 `WB2A_CLIENT_VERSION_CN`、`WB2A_CLIENT_VERSION_GLOBAL`、`WB2A_CLI_VERSION` 一起传给登录程序，配置里改版本时两处 UA 不再各说各话。
4. **登录链路字段不完整**：按官方结构改成「每次登录一条根 span + 每类请求一个子 span」——根 span 头（`X-Trace-ID`/`b3`/`X-B3-*`，父 span 为空）每类请求都带，`traceparent` 永远是该类请求自己的子 span；打开授权页单独用 `auth-page` 子 span；补上 `login_owner_id`（每次登录一个 UUID），state 文件存 `login_session_id`/`login_owner_id`/`root_span_id`。
5. **小程序请求内平台自相矛盾**：请求头是 iPhone 微信 UA，正文却写 `windows`/`x64`。`mpEventBase` 改为 `os: ios`、`osVersion: mpIOSVersion`、`arch: arm64`，与 UA 同源；新增测试断言 UA 与 `mpIOSVersion` 一致、正文与平台一致。这同时废掉了旧测试 `TestAccountDeviceStableAcrossCredentialAndClientChanges` 里"小程序和桌面端 os/arch 相同"的断言——小程序与桌面共用的是账号身份（`machineId`），不是平台字段。

新增/调整的测试：`cmd/login/save_test.go`（保留设备、新账号建 Mac、拒绝不安全覆盖、默认路径）、`TestLoginTraceRootAndChildSpans`（根 span 一致、子 span 互不相同、授权页独立子 span）、`TestWebReportKeepsRealPageOnCookiePath`、`TestMPFingerprintMatchesRuntime`。

仍未做：真实上游联调（含真实扫码）。脚本主体（签到、容器重启）按约定未执行，只做了 `bash -n` 与隔离的 `login save` 联调。
