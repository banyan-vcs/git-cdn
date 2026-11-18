# Git CDN

[English](README.md) | 简体中文

使用 Go 和原生 Git 实现的 HTTP Git 缓存代理。每次本地 fetch 都向原始服务执行实时 `ls-refs`，从本地对象库打包，缺失对象时按精确 OID 补齐。

Git CDN 使用独立域名，例如 `git-cdn.example`；原始服务仍使用 `gitlab.com`。客户端连接 CDN，CDN 使用请求对应的上游凭据回源。

已实现 HTTP smart protocol v2 首版；SSH 仍处于设计阶段。

## 运行

需要 Go 1.24.1+、原生 Git，以及 Linux 或 macOS。已安装的 Git 必须支持 protocol v2 和 `fetch --no-auto-maintenance`。

```sh
mkdir -p ~/.git-cdn
cp config.example.yaml ~/.git-cdn/config.yaml
go build -o bin/git-cdn ./cmd/git-cdn
./bin/git-cdn
```

默认读取 `~/.git-cdn/config.yaml`，可用 `-config /path/to/config.yaml` 指定其他文件。默认缓存目录为 `~/.git-cdn/caches`，可在 YAML 中通过 `cache_dir` 调整。配置文件参数和 `cache_dir` 都支持 `~/` 展开；显式配置的相对路径按服务启动时的工作目录解析。

使用自定义配置文件：

```sh
./bin/git-cdn -config ./config.yaml
```

使用自定义缓存目录：

```yaml
cache_dir: /data/git-cdn/caches
```

默认监听 `127.0.0.1:8080`。在独立 CDN 域名的 TLS 反向代理后运行，或在 YAML 中配置 `tls_cert`、`tls_key` 直接提供 HTTPS。前置代理需要传递 `Git-Protocol` 和配置的认证头，并允许 Git 上传和下载的请求体。

## 单站点和多站点

单站点使用 [config.example.yaml](config.example.yaml)，仓库路径直接映射到一个固定上游：

```yaml
mode: single
upstream:
  url: https://gitlab.com
```

```text
https://git-cdn.example/group/repo
https://git-cdn.example/group/repo.git
    → https://gitlab.com/group/repo.git
```

多站点使用 [config.multi.example.yaml](config.multi.example.yaml)，将它复制为 `~/.git-cdn/config.yaml`；路径第一段指定上游：

```yaml
mode: multi
upstreams:
  gitlab:
    url: https://gitlab.com
  github:
    url: https://github.com
```

```text
https://git-cdn.example/gitlab/group/repo[.git]
    → https://gitlab.com/group/repo.git
https://git-cdn.example/github/owner/repo[.git]
    → https://github.com/owner/repo.git
```

`mode` 可省略，此时 `upstream` 推断为 single，`upstreams` 推断为 multi；即使 `upstreams` 只有一个条目，也仍使用站点前缀。两种字段不能混用，配置中的未知字段、重复键及多个 YAML 文档会在启动时拒绝。

两种模式均保留仓库路径大小写，并把带/不带 `.git` 的地址规范化为相同的上游路径及缓存身份。上游 URL 可包含路径前缀。规范化覆盖 discovery、ls-refs、fetch、push 和标准 `info/lfs` 路由，客户端 POST 无需先跟随补后缀的重定向。单站点重定向保持直接仓库路径，多站点重定向保留对应站点前缀。

## 客户端接入

单站点直接使用 CDN 的仓库 URL：

```sh
git clone https://git-cdn.example/group/repo.git
```

也可以配置全局 URL 改写：

```sh
git config --global url."https://git-cdn.example/".insteadOf "https://gitlab.com/"
```

多站点模式将 CDN URL 中的路径改为 `/gitlab/group/repo.git`，改写规则改为 `url."https://git-cdn.example/gitlab/".insteadOf`。每个站点分别配置对应的原站前缀。

此规则同时影响 push；CDN 会透传 receive-pack discovery 和 push 请求。若采用逐 remote 的 fetch/push URL，可将 fetch 指向 CDN、push 指向原站，具体配置及全局改写之间的关系见设计文档。

Git credential helper 需要为 **CDN 的域名**提供目标上游接受的凭据。凭据不会从原站 host 自动迁移。默认只转发 `Authorization`，支持上游接受的 Basic/PAT 或 Bearer；需要 Cookie 或其他认证头时将其明确添加到 `auth_headers`。多上游共享域名时，helper 应按路径选择凭据，可开启 `credential.useHttpPath`。代理不保存明文凭据，也不提供 CDN token 到原站身份的兑换服务。

## 已实现行为

| 请求 | 行为 |
| --- | --- |
| discovery、ls-refs | 实时代理原站，保留 Git 协议能力和 refs |
| HTTP v2 clone/fetch，want 是新鲜 refs 的 tip/tag/peeled OID | 实时鉴权后检查依赖，缺失时获取精确 roots，再本地 upload-pack |
| 普通 have 协商 | have 必须为已授权 wanted roots 的祖先 commit，否则回源 |
| include-tag | 校验当前标签关系，补齐相关标签并在请求专用 refs 视图中执行 |
| shallow、filter、want-ref、独立历史/blob/tree OID、未知扩展、SHA-256、v0/v1 | 完整回源，由原站处理 |
| receive-pack / push、Git LFS 等仓库端点 | 完整回源，不进入对象缓存 |

每个要本地处理的 fetch 都自行发送 ls-refs，包括未先做 discovery 的直接 POST。401/403/404 等原站响应保留状态和必要 challenge；登录页或损坏的 refs 响应不会授权缓存读取。

GitHub 的 Git 接口也会对不存在的仓库返回 401。若公开 owner API 确认 owner 不存在，CDN 返回 404 并去掉登录挑战；owner 存在、元数据查询失败/被限流或仓库可见性无法确认时保留 401。仓库 API 的 404 本身可能隐藏私有仓库，不能单独作为不存在的证据。其他站点保留原站状态。

默认按**原站 URL、规范仓库身份和凭据 HMAC**隔离对象库。HMAC key 存储于 cache 目录，凭据值仅在请求和子进程环境中存活。每仓库的写入/本地生成受锁保护；同一个 cache 目录只能由一个服务进程持有，多个实例使用各自的 cache 目录。

对象在 staging bare 仓库内获取并校验，再发布 pack/index 和 GC roots。完整性检查遍历依赖，包含“commit 存在但 blob 缺失”的情况；已验证健康的旧 roots 可参与增量获取协商。缓存 refs 不作为上游可见 refs 返回。

本地响应先写入有大小上限的临时文件，Git 成功退出后设置 Content-Length 和状态头，再传输给客户端。本地生成失败、超时或超限时，在发送响应前完整回源。这个实现优先保证回源边界，代价是本地响应的首字节要等待生成完成；尚未实现 pack-objects 输出缓存。

`X-GitCDN-Cache-Status`：`HIT` 表示所需对象已完整存在，`MISS` 表示本次补齐后本地生成，`BYPASS` 表示原站响应。HIT 是对象缓存命中，不代表复用已生成的 pack。

## 配置和运行边界

| 配置 | 默认值 | 用途 |
| --- | --- | --- |
| `cache_dir` | `~/.git-cdn/caches` | 持久对象缓存、staging、响应临时文件及缓存目录锁 |
| `max_cache_bytes` | 20 GiB | 持久仓库缓存作用域内的逻辑文件总字节数，按 LRU 淘汰空闲作用域 |
| `mode` | 根据 upstream/upstreams 推断 | single 为直接仓库路径，multi 为站点前缀路径 |
| `upstream` | 无 | single 模式的固定原站，使用 `url` 字段 |
| `upstreams` | 无 | multi 模式的站点 ID → 原站映射 |
| `max_request_bytes` | 8 MiB | v2 upload-pack POST 的压缩及解压后输入上限 |
| `max_refs_bytes` | 16 MiB | 内部 ls-refs 响应上限 |
| `max_response_bytes` | 1 GiB | 本地生成响应的磁盘暂存上限；超限回源 |
| `max_concurrent_git` | 4 | 原生 Git 子进程并发数 |
| `fetch_timeout` | 2m | 单次对象补齐的期限 |
| `git_timeout` | 2m | 本地操作总期限，含排队、校验、补齐和打包 |
| `upstream_timeout` | 30s | 实时 refs 探测的期限 |

本地路径最多处理 64 个 wants、128 个 haves，以及 include-tag 请求中的 128 个当前标签；超出时回源。v2 请求支持 gzip，透传时保留原始压缩输入；其他编码直接回源。原站受信路径前缀内的规范化重定向会改写回 CDN 路由。

持久仓库缓存已支持 LRU，默认容量为 20 GiB。访问时间跨重启保留；启动时若旧缓存超过新上限，会先淘汰。发布对象前预留容量并移除最久未使用的空闲“仓库＋凭据作用域”。活动租约和排队请求会保护其作用域，避免补齐、alternates 和打包与淘汰竞争。单个仓库超过上限，或所有空间都被活动请求占用时，完整回源；已淘汰的仓库可再次按需获取。

```yaml
max_cache_bytes: 21474836480 # 20 GiB
```

容量统计包括受管理 `repos/` 作用域中的常规文件长度及 Git 元数据，不包括目录/磁盘块开销、进程锁、HMAC key 和 `tmp/`。每次 staging 下载在发布前及下载期间每 200 ms 检查相同上限，检查间隔内的临时写入可能短暂超过它。响应暂存仍由 `max_response_bytes` 限制。并发 staging 和响应文件需要额外磁盘空间：这是持久缓存的准入上限，不是整个缓存目录的文件系统硬配额。后台预热、对象级 Git GC 和跨节点缓存协调尚未实现。服务独立管理 cache 仓库，运行中不要在其中执行 prune/删除对象。

原站 repo 路径被删除后重新用于另一仓库时，目前尚无稳定 repository ID 校验；应使旧路径的缓存失效。HTTP v2 的本地加速依赖原站支持该协议；部署时需验证所选上游的协议能力及凭据行为。

健康检查 `GET /healthz`。`GET /metrics` 提供 local/hydration/passthrough/auth-denied 计数，以及缓存字节数、上限、容量预留、作用域数量、淘汰次数和淘汰字节数。日志不包含认证头或原站 Git stderr。

## 验证

```sh
go test ./...
go test -race ./...
go vet ./...
```

集成测试启动带认证的真实 `git http-backend`，使用标准 Git 客户端执行 clone、增量 fetch 和 push；同时验证 YAML 加载和校验、单站点/多站点的两种 URL 格式及缓存共享、各站点独立鉴权与缓存隔离、陈旧 OID 回源、缺失 blob/pack 恢复、并发 miss、超时/响应超限回源、gzip 和重定向。

上述集成测试已分别在 Git 2.54.0 和 2.42.0 上通过，Go race 检查及 vet 通过。

容量测试覆盖 LRU 顺序、重启恢复、降低上限、活动/排队租约保护、并发容量预留、超大仓库回源、淘汰后重新获取和 staging 增长取消。GitHub 状态测试覆盖 owner 确认不存在、可见性未知时保留鉴权，以及元数据服务失败。

```sh
go run ./scripts/live-repository --concurrency 6 --repository git/git
```

该脚本访问真实 GitHub，保留结果及客户端仓库于 `bin/live-tests/`，并自行关闭临时测试服务。

协议解析器另有 fuzz 入口：

```sh
go test ./internal/protocol -fuzz=FuzzParseFetch -fuzztime=10s -parallel=4
```

## 设计及协议证据

- [设计方案](docs/design.md)：Go 架构、鉴权边界、HTTP / SSH 可行性、缓存一致性、实施路线。
- [协议实验](docs/protocol-probes.md)：六项本地验证及运行方法。
- [实验脚本](scripts/protocol-probe/main.go)：仅依赖 Go 标准库和原生 Git，不访问网络、不使用个人凭据。

SSH 后续需要平台用户作用域委托、已配置用户凭据映射或显式 agent forwarding，方案见设计文档。
