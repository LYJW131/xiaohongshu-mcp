# Cloudflare Containers 私有试跑

基于 `LYJW131/xiaohongshu-mcp` 的固定提交
`a5c8f7799980ba1fdd501999843eb2d17e4c9a9f` 完成了首次私有试跑。
当时新增的 `cloudflare/Dockerfile` 仅将 Ubuntu 下载源保留为官方源并加下载超时，避免 Cloudflare 构建跨地域拉取阿里云镜像过慢；该次试跑没有修改 Go 程序、编译参数和浏览器版本。
当前分支增加了 Go 远程会话存储与下述私有 Durable Object 桥接，尚未部署或进行新一轮云端验收。

## 已验证结果（2026-10-01 UTC）

真实 Cloudflare Containers 试跑已通过：

- `standard-1` 的 Linux/amd64 镜像构建、发布、启动成功
- `/health` 返回健康，程序版本匹配固定源提交
- MCP `initialize` 成功，协商协议 `2025-03-26`
- `tools/list` 返回 18 个工具
- 项目自带浏览器成功打开 `about:blank`
- 14:47:48.562 开始验收，14:47:57.146 完成并停止容器，历时 8.584 秒
- 已移除临时 Cron、解除 Worker 激活，核验容器为 `inactive`

首次试跑时的本地模拟测试 15 项通过。上述历史云端结果不包含小红书账号登录、搜索/发布、cookies 持久化或空闲唤醒后恢复。

注意：更新运行变量时，Cloudflare 的 script settings PATCH 会省略 `containers` 运行元数据，导致 `ctx.container` 缺失。应使用完整 Wrangler 部署或完整 Worker 上传，保留 `exports`、`containers`、绑定和原截止时间；上传后从 Worker version 读回 `resources.script_runtime.containers` 核验。

## 试跑范围

- 独立 Worker / Container：`xhs-container-trial-20261001`
- `standard-1`，最多 1 个运行实例，容器运行最多 20 分钟
- 不开 `workers.dev`、预览 URL、自定义域名、SSH 或公共 MCP 路由
- 禁止容器出站网络；不导入 cookies，不登录账号，不调用任何小红书业务工具
- 通过临时 Cron 内部触发 `/health`、MCP `initialize`、`tools/list` 和浏览器 `about:blank`
- 检查健康接口返回的版本必须等于固定源提交，避免误测上游 `latest` 镜像
- 结果写入私有 Worker 日志与 Durable Object 存储；无公共结果接口

`tools/list` 只获取工具清单；浏览器测试只打开空白页。这些检查通过也不代表已经验证小红书登录、搜索、发布或外部网络可达性。

## 防止重复运行和失控

配置默认 `TRIAL_APPROVED=false`、空截止时间、空 Cron，部署后不会自行启动。
执行前填入批准的有效截止时间并启动临时 Cron。
Durable Object 先保存一次性标记，再启动容器；重复触发、进程重启或失败均不自动重新运行。
一旦测试返回，无论成功失败都调用 `destroy()`；同时使用 Durable Object alarm 和容器内 `timeout` 限制最长运行时间。

运维必须在 `finally` 清理路径移除临时 Cron，并核对实例已停止。
Worker 不持有 Cloudflare API 凭据，所以不会尝试自行修改账户 Cron 配置。
若 Cron 尚未移除，一次性标记仍会阻止再次启动容器，但定时 Worker 调用仍需清理。

## 私有登录态持久化

容器磁盘不是持久卷。启用 `XHS_SESSION_STORE=cloudflare` 后，Go 后端通过固定虚拟主机
`http://xhs-session.internal` 访问同一个 `XhsTrial` 的 SQLite 存储。
`WorkerEntrypoint` 桥接由 `ctx.exports.XhsSessionBridge` 创建，可信 props 仅携带当前 DO 的 ID。
`ctx.container.interceptOutboundHttp` 只拦截这一主机；容器每次获批启动前重新安装并等待完成。
桥接没有公共入口，不新增凭据或独立 AUTH_TOKEN，也不接收客户端指定账号、实例或存储目标。
Cloudflare 官方说明容器到出站处理器的 HTTP 由平台网络栈加密。

后端请求必须携带 `X-XHS-Session-Client: 1`。这是防止浏览器简单请求的协议标记，不是密码。
任何 `Origin`、`Referer`、`Sec-Fetch-*` 请求都会被拒绝；不返回 CORS 许可头。
路径、方法、scheme、host、port、query 均固定检查，Worker 与 DO 的普通 `fetch()` 始终返回 404。
因此容器内打开的第三方网页不能借虚拟主机读取或修改 cookies。

会话协议：

- `GET /v1/session`：返回 `{ "revision": 0, "session": null }`，或当前版本和会话
- `PUT /v1/session`：JSON 为 `{ "expected_revision": 0, "operation_id": "UUID", "session": { "version": 2, "seed": 23088, "saved_at": "2026-10-01T14:00:00Z", "cookies": [] } }`
- `DELETE /v1/session`：JSON 只包含 `expected_revision` 与 `operation_id`
- `POST /v1/activity`：空 body，返回 204；只刷新已运行容器的 60 秒空闲窗口，不启动容器，不写存储，不改变 alarm 或容器内 timeout 的硬截止时间

会话 JSON 最多 64 KiB，完整 mutation body 最多 65 KiB；采用流式字节限制，不能靠缺少或伪造 Content-Length 绕过。
会话必须为 v2、非负安全整数 seed、RFC3339 saved_at 和 cookie 对象数组；cookie 的 name/value 必须为字符串，domain/path 若存在也必须是字符串，其他 CDP 字段原样保留。
GET/PUT/DELETE 的成功响应均为 `{ "revision": 整数, "session": 对象或null }`，不包含内部幂等记录。

每个 DO 只用一条会话记录，在 `ctx.storage.transaction()` 中完成读、CAS 与写入。
成功写入和删除都推进 revision；删除保留 null 墓碑，不删除版本信息。
最后一次操作的相同 UUID 和相同内容可安全重试；同 UUID 不同内容、旧版本覆盖、旧删除、旧保存复活均返回 409。
发生后续写入后，旧操作的原始请求也返回 409，不把旧响应当作当前状态。
存储或内部 RPC 失败返回 503，不回显异常，不记录 cookie 值，也不以本地临时文件冒充成功。
不同 DO 的存储互相隔离；DO 重建、容器停止或删除容器磁盘不影响已提交会话。

当前仍保留 `TRIAL_APPROVED=false`、空 Cron、空 routes、关闭 workers.dev 和预览 URL。
此分支不会自动启动旧试跑，不提供正式 MCP 公共入口，也尚未验证真实账号恢复登录。
上线前仍需另行批准并验证：实际私有出站桥接、合成会话跨容器重启、存储故障、删除后旧进程防复活，以及真实登录会话恢复。
固定源版本配置仍指向历史试跑提交；部署前必须先更新并核验新镜像对应的 source commit，不能把新代码继续标为历史镜像。

## 本地检查

需要 Node.js 22.15 或更新版本（实际 Worker 导出测试使用 Node 模块解析钩子）。

```sh
cd cloudflare
npm test
```

测试只使用本机 Node 和模拟的 Container/DO，验证一次性标记、重复触发、关闭逻辑、响应限制及配置，
并覆盖会话 CAS、重试、墓碑、并发、DO 重建、实例隔离、读写提交失败、浏览器拒绝、body 限制和活动心跳。
它们不执行仓库 Go 服务、Chromium 或真实 Cloudflare 容器，不能替代镜像构建与云端验收。

## Cloudflare Workers Builds

使用仓库根目录作为 Build root，连接独立试验分支，勿改现有生产 Worker。

Build command:

```sh
npm --prefix cloudflare install --ignore-scripts && npm --prefix cloudflare test
```

Deploy command:

```sh
cd cloudflare && npx wrangler deploy
```

首次构建会下载项目所需 Go 模块、Ubuntu 官方源软件包以及项目自带的定制浏览器。
定制浏览器来自 `https://cdn.one-world.ai/browsers/148.0.7778.215/`，原 Dockerfile 校验同站发布的 SHA256。
校验用于完整性检查，不等于独立安全审计。

需要已有 Workers Paid 资格及适当的 Workers Builds / Containers 权限。不得为本次试跑自动升级订阅或创建新凭据。

## 常规按需 MCP 的后续设计

本试跑验证运行能力，尚未实现用户请求触发的正式入口。
正式使用时，应在通过鉴权后唤醒单账号固定实例，等待浏览器就绪，待所有正在执行的 MCP 工具完成后才开始空闲倒计时。
长任务不能仅按“没有新 HTTP 请求”判断为空闲。
默认容器磁盘不持久；当前分支使用上述 SQLite 会话桥接保存 cookies 和指纹 seed，实际云端恢复仍需验收。
Cloudflare 的新快照能力仅支持 `durable_object` 调度策略，保存文件系统而非进程内存，且存在镜像绑定及有效期约束；不能当作普通持久卷直接替代。
本次使用 `default` 策略以便配置级强制单实例，不创建含登录信息的快照。

## 官方参考

- https://developers.cloudflare.com/containers/guides/deploy/
- https://developers.cloudflare.com/containers/examples/cron/
- https://developers.cloudflare.com/containers/api/durable-object-container/
- https://developers.cloudflare.com/containers/configuration/outbound-traffic/
- https://developers.cloudflare.com/durable-objects/api/sqlite-storage-api/
- https://developers.cloudflare.com/containers/platform/pricing/
- https://developers.cloudflare.com/containers/guides/snapshots/

### Go 与实际 JS 协议验收（不启动云容器）

```sh
# 仓库根目录，Go 1.24+、Node 22.15+
go test ./...
go test -race ./cookies ./configs ./browser .
go vet ./...
go test -c -o /tmp/xhs-cookies-contract.test ./cookies
node cloudflare/scripts/test-go-contract.mjs /tmp/xhs-cookies-contract.test
(cd cloudflare && npm test)
```

协议验收启动本机 HTTP 适配器，调用实际 `handleSessionRequest`，依次用四个独立 Go 进程执行假 cookie 写入、重启恢复、退出、再重启确认空状态。没有真实 Cookie、浏览器登录或 Cloudflare 资源操作。测试结束移除 `/tmp/xhs-cookies-contract.test` 即可。

Go 启动入口先恢复远端 cookie 和 seed，再启动浏览器；恢复或 seed 写入失败直接停止，不读取本地残留。每个浏览器保留自己的版本快照。二维码准备、替换和退出串行；等待扫码期间暂停普通浏览器刷新，避免登录状态轮询抢占待提交的版本。二维码提交仍需 CAS，成功保存后才能报告持久化成功。普通操作关闭浏览器前会提交刷新后的 cookie；写失败会记录脱敏错误，不覆盖远端旧快照。

### 部署及真实登录前仍需确认

此分支只在本地验证，未推送、构建或部署。再次云验收须使用新的授权时段、预算及独立测试对象；已有一次性 trial 记录不应清除或重新武装。建议单次最多20分钟、US$1，只用假 cookie 做启动/停止/恢复/退出检查，结束停止实例、关闭触发器。

真实扫码须先有用户专用的已鉴权入口、明确的小红书外网访问和会话保留授权。不能把公共 `/mcp` 或 Cookie 接口直接开放，也不能复用已结束的试跑授权。真实账号 Cookie 不得经过聊天、仓库、构建变量或日志。用户在安全登录流程中亲自扫码；服务把生成的会话留在对应 DO。业务站点可能使 cookie 过期或撤销，因此“可恢复保存的数据”不等于保证长期免登录。
