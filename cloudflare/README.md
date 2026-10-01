# Cloudflare Containers 私有试跑

基于 `LYJW131/xiaohongshu-mcp` 的固定提交
`a5c8f7799980ba1fdd501999843eb2d17e4c9a9f`，保留原 Go 源码和 Dockerfile。
新增的 `cloudflare/Dockerfile` 仅将 Ubuntu 下载源保留为官方源并加下载超时，避免 Cloudflare 构建跨地域拉取阿里云镜像过慢；Go 程序、编译参数和浏览器版本均未改变。

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

## 本地检查

```sh
cd cloudflare
npm test
```

测试只使用本机 Node 和模拟的 Container/DO，验证一次性标记、重复触发、关闭逻辑、响应限制及配置。
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
默认容器磁盘不持久；cookies 和指纹 seed 必须单独设计持久化与恢复。
Cloudflare 的新快照能力仅支持 `durable_object` 调度策略，保存文件系统而非进程内存，且存在镜像绑定及有效期约束；不能当作普通持久卷直接替代。
本次使用 `default` 策略以便配置级强制单实例，不创建含登录信息的快照。

## 官方参考

- https://developers.cloudflare.com/containers/guides/deploy/
- https://developers.cloudflare.com/containers/examples/cron/
- https://developers.cloudflare.com/containers/api/durable-object-container/
- https://developers.cloudflare.com/containers/platform/pricing/
- https://developers.cloudflare.com/containers/guides/snapshots/
