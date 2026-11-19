# Git CDN

[English](README.md) | 简体中文

使用 Go 和原生 Git 实现的 HTTP Git 缓存代理。每次本地 fetch 都向原始服务执行实时 `ls-refs`，从本地对象库打包，缺失对象时按精确 OID 补齐。

Git CDN 使用独立域名，例如 `git-cdn.example`；原始服务仍使用 `gitlab.com`。客户端连接 CDN，CDN 使用请求对应的上游凭据回源。

已实现 HTTP smart protocol v2 和可配置 SSH 密码代理；配置及适用条件见 [SSH 说明](docs/ssh.md) 和 [样例](config.ssh.example.yaml)。

SSH 仅接受密码/token 认证。仅使用公钥认证的客户端会在认证阶段被拒绝，并收到说明支持凭证类型的 banner。

上游 host key 默认严格校验。可选 `host_key_policy: accept-new` 会把首次连接观察到的公钥写入可写、持久化的 `known_hosts`，之后公钥发生变化时仍拒绝连接。

## 运行

本地容器快速启动：

```sh
docker compose up -d --build --wait
```

HTTP 地址为 `http://127.0.0.1:8080/github/owner/repository.git` 或 `/gitlab/group/repository.git`。命名卷保留缓存和 SSH host identity。自定义配置、可选 SSH 和数据管理见 [Docker Compose](docs/docker.md)。

需要 Go 1.26+、原生 Git，以及 Linux 或 macOS。需要 Git 2.45+，支持 reftable、protocol v2、`fetch --no-auto-maintenance` 和 cruft pack。启动时会检查 reftable 支持，Git 无法创建该格式时会明确报错。

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

非回环 HTTP 监听默认需要 TLS 或配置 `trusted_proxy_cidrs`；受信代理必须重写 `X-Forwarded-Proto: https`，未受信地址不能通过伪造此头绕过检查。非回环上游默认要求 HTTPS。仅在受控内部网络使用 `allow_insecure_http: true`。

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
    credential_host: git-cdn-gitlab.example
  github:
    url: https://github.com
    credential_host: git-cdn-github.example
```

```text
https://git-cdn-gitlab.example/gitlab/group/repo[.git]
    → https://gitlab.com/group/repo.git
https://git-cdn-github.example/github/owner/repo[.git]
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

多站点模式使用对应的 `credential_host`，路径为 `/gitlab/group/repo.git`，改写规则改为 `url."https://git-cdn-gitlab.example/gitlab/".insteadOf`。每个站点分别配置对应的原站前缀。

此规则同时影响 push；CDN 会透传 receive-pack discovery 和 push 请求。若采用逐 remote 的 fetch/push URL，可将 fetch 指向 CDN、push 指向原站，具体配置及全局改写之间的关系见设计文档。

Git credential helper 需要为 **CDN 的域名**提供目标上游接受的凭据。凭据不会从原站 host 自动迁移。默认只转发 `Authorization`，支持上游接受的 Basic/PAT 或 Bearer；需要 Cookie 或其他认证头时将其明确添加到 `auth_headers`。两个以上上游默认需要不同的 `credential_host`，带凭据请求的 Host 必须与目标站点匹配，错误匹配返回 421 且不回源。需要共享域名时显式配置 `allow_shared_credential_host: true`，并启用 `credential.useHttpPath`、按站点路径配置 helper。`X-Forwarded-*`、`X-Remote-User` 等代理身份头不能配置为客户端凭据头。代理不保存明文凭据，也不提供 CDN token 到原站身份的兑换服务。

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

按**原站 URL、规范仓库路径和对象格式**共享对象库，存放于 `repos/<repository-hash>/repo.git`。不同用户及轮换后的 token 复用同一份对象。每次本地 fetch 仍使用自己的凭据实时鉴权，缺失对象也由本次调用者的凭据补齐；凭据值仅在请求和子进程环境中存活。共享存储适用于仓库级 Git 读取权限。每仓库的写入/本地生成受锁保护；同一个 cache 目录只能由一个服务进程持有，多个实例使用各自的 cache 目录。启动时合并旧凭据目录中的对象和 roots，验证后移除旧目录及 scope key。

对象在 staging bare 仓库内获取并校验，再发布 pack/index 和 GC roots。完整性检查遍历依赖，包含“commit 存在但 blob 缺失”的情况；已验证健康的旧 roots 可参与增量获取协商。缓存 refs 不作为上游可见 refs 返回。

新建缓存、对象补齐的临时仓库及请求专属引用视图默认使用 reftable，通过索引减少大量分支、标签读取和更新时的文件操作。已有 files 格式缓存继续复用已下载对象，不会自动转换；迁移方式见 [引用存储](docs/cache-policy.md#ref-storage)。客户端 `ls-refs` 仍请求原站，以保持实时权限和引用可见性。

包含 wants、`done`、没有 haves 且未过滤的 v2 fetch，会根据完整实时 ls-refs 快照对齐缓存引用。它覆盖普通 clone 和空仓库 fetch；Git 协议没有独立的 clone 标记。已缓存且完整的 tips 写入 `refs/cache/upstream/`，过时名称和 roots 被移除，HEAD 随原站更新；不下载客户端未请求的分支。增量、shallow、filter 及仅协商请求不清理 refs。

本地响应先写入有大小上限的临时文件，Git 成功退出后设置 Content-Length 和状态头，再传输给客户端。本地生成失败、超时或超限时，在发送响应前完整回源。这个实现优先保证回源边界，代价是本地响应的首字节要等待生成完成；尚未实现 pack-objects 输出缓存。

`X-GitCDN-Cache-Status`：`HIT` 表示所需对象已完整存在，`MISS` 表示本次补齐后本地生成，`BYPASS` 表示原站响应。HIT 是对象缓存命中，不代表复用已生成的 pack。

## 配置和运行边界

| 配置 | 默认值 | 用途 |
| --- | --- | --- |
| `cache_dir` | `~/.git-cdn/caches` | 持久对象缓存、staging、响应临时文件及缓存目录锁 |
| `max_cache_bytes` | 20 GiB | 持久仓库缓存作用域内的逻辑文件总字节数，按 LRU 淘汰空闲作用域 |
| `repack_pack_threshold` | 32 | pack 数达到阈值后排队维护空闲仓库，最小为 2 |
| `repack_timeout` | 2m | 后台 repack 和 GC 的执行期限 |
| `gc_interval` | 24h | 成功 GC 之间的间隔，不依赖 pack 数量 |
| `gc_retention` | 336h | 不可达对象和移除 root 历史的保留窗口，最小 14 天 |
| `mode` | 根据 upstream/upstreams 推断 | single 为直接仓库路径，multi 为站点前缀路径 |
| `upstream` | 无 | single 模式的固定原站，使用 `url` 字段 |
| `upstreams` | 无 | multi 模式的站点 ID → 原站映射 |
| `max_request_bytes` | 8 MiB | v2 upload-pack POST 的压缩及解压后输入上限 |
| `max_refs_bytes` | 16 MiB | 内部 ls-refs 响应上限 |
| `max_response_bytes` | 1 GiB | 本地生成响应的磁盘暂存上限；超限回源 |
| `max_concurrent_git` | 4 | 原生 Git 子进程并发数 |
| `max_concurrent_requests` | 16 | 解析、回源和传输全过程的活动 Git 请求上限 |
| `max_requests_per_second` | 64 | 全实例 Git 请求速率，初始突发额度为相同数值 |
| `max_temp_response_bytes` | 4 GiB | 所有未完成本地响应的总磁盘预算，文件删除后释放 |
| `write_idle_timeout` | 30s | 单次写出/flush 的停滞期限；持续写出的长响应可继续 |
| `request_timeout` | 10m | Git 请求总期限，含入站、回源和响应传输 |
| `fetch_timeout` | 2m | 单次对象补齐的期限 |
| `git_timeout` | 2m | 本地操作总期限，含排队、校验、补齐和打包 |
| `upstream_timeout` | 30s | 实时 refs 探测的期限 |

每个 v2 命令最多 4096 个 pkt-line，解析时不建立完整包数组。本地路径最多处理 64 个 wants、128 个 haves，以及 include-tag 请求中的 128 个当前标签；超出时回源。v2 请求支持 gzip，透传时保留原始压缩输入；其他编码直接回源。原站受信路径前缀内的规范化重定向会改写回 CDN 路由。

持久仓库缓存已支持 LRU，默认容量为 20 GiB。访问时间跨重启保留；启动时若旧缓存超过新上限，会先淘汰。发布对象前预留容量并移除最久未使用的空闲仓库。活动租约和排队请求会保护其仓库，避免补齐、alternates 和打包与淘汰竞争。单个仓库超过上限，或所有空间都被活动请求占用时，完整回源；已淘汰的仓库可再次按需获取。

pack 数达到阈值的空闲仓库会执行 `git repack --geometric=2 -d`，维护持有独占租约并使用全局 Git 进程额度。它通过不可变对象的硬链接构造临时 pack 集合，预留额外容量，并每 200 ms 检查新文件增长，验证完成后再发布。失败时保留原 pack；容量不足时跳过维护，不影响已经生成的客户端响应。geometric repack 合并普通 pack 并保留 cruft 对象年龄；到期 GC 机制见下文。指标提供 repack 完成、失败和跳过次数。

GC 按 `gc_interval` 维护空闲仓库，pack 数低于 repack 阈值时也可执行。它在同样的临时仓库/发布流程中运行 `git repack -a -d --cruft --cruft-expiration=<cutoff>` 并清理过期 loose 对象。`gc_retention` 默认 14 天且不能更短：对象 mtime 及 cruft `.mtimes` 保留近期不可达对象及其依赖，geometric repack 保留独立的 cruft pack，避免丢失对象年龄。解除 root pin 的历史额外获得 `refs/cache/retained/<oid>`，至少保留自移除起的 14 天，时间记录在 `.gc-retained`；这些内部 refs 不构成读取授权。`.gc-last` 持久化上次成功 GC 时间。活动和排队租约会阻止 GC；超时或预算失败保留原对象。整体 LRU 淘汰仍独立于 GC 保留窗口，淘汰后的缓存可从原站恢复。

```yaml
max_cache_bytes: 21474836480 # 20 GiB
```

容量统计包括受管理 `repos/` 作用域中的常规文件长度及 Git 元数据，不包括目录/磁盘块开销、进程锁和 `tmp/`。每次 staging 下载在发布前及下载期间每 200 ms 检查相同上限，检查间隔内的临时写入可能短暂超过它。响应暂存按 `max_response_bytes` 限制单文件，并由 `max_temp_response_bytes` 限制所有活动响应文件的总字节数。预算不足时，在响应提交前回源。启动时持有目录锁清理上次进程遗留的临时文件。并发 staging 和响应文件需要额外磁盘空间：这是持久缓存的准入上限，不是整个缓存目录的文件系统硬配额。后台预热和跨节点缓存协调尚未实现。服务独立管理 cache 仓库，运行中不要在其中执行 prune/删除对象。

原站 repo 路径被删除后重新用于另一仓库时，目前尚无稳定 repository ID 校验；应使旧路径的缓存失效。HTTP v2 的本地加速依赖原站支持该协议；部署时需验证所选上游的协议能力及凭据行为。

入口并发或速率超限返回 503 和 `Retry-After: 1`，不读取 Git 请求体。健康检查与指标不占用 Git 请求额度。所有 Git 回源响应也设置 `Cache-Control: no-store`。完整配置迁移及资源边界见 [HTTP 安全说明](docs/http-security.md)。

健康检查 `GET /healthz`。`GET /metrics` 提供 local/hydration/passthrough/auth-denied 计数，以及缓存字节数、上限、容量预留、作用域数量、淘汰次数和淘汰字节数，以及活动请求、限流拒绝和暂存响应字节数。日志不包含认证头或原站 Git stderr。

## 验证

```sh
go test ./...
go test -race ./...
go vet ./...
```

集成测试启动带认证的真实 `git http-backend`，使用标准 Git 客户端执行 clone、增量 fetch 和 push；同时验证 YAML 加载和校验、单站点/多站点的两种 URL 格式及缓存共享、各站点独立鉴权与缓存隔离、陈旧 OID 回源、缺失 blob/pack 恢复、并发 miss、超时/响应超限回源、gzip 和重定向。

引用存储测试覆盖 reftable 初始化、逻辑 HEAD、files 缓存复用及引用更新的容量拒绝。Git 2.45 之前的版本无法运行当前默认的 reftable 模式。

容量测试覆盖 LRU 顺序、重启恢复、降低上限、活动/排队租约保护、并发容量预留、超大仓库回源、淘汰后重新获取和 staging 增长取消。GitHub 状态测试覆盖 owner 确认不存在、可见性未知时保留鉴权，以及元数据服务失败。

协议覆盖、可选真实仓库验证及性能测量方法见 [测试说明](docs/testing.md)。已有 multi 服务运行且二进制已构建时，显式选择公开项目：

```sh
go run ./scripts/live-repository --repository git/git --concurrency 6
```

该脚本访问真实 GitHub，保留结果及客户端仓库于 `bin/live-tests/`，并自行关闭临时测试服务。推荐使用 `git/git`、`redis/redis` 等知名项目。单次耗时仅是观测值，不是性能基线；生成的报告不纳入版本控制。

协议解析器另有 fuzz 入口：

```sh
go test ./internal/protocol -fuzz=FuzzParseFetch -fuzztime=10s -parallel=4
```

## 设计及协议证据

- [设计方案](docs/design.md)：架构、鉴权边界、缓存一致性及性能权衡。
- [缓存策略](docs/cache-policy.md)：容量准入、LRU、租约保护和 GitHub 状态处理。
- [协议实验](docs/protocol-probes.md)：本地 Git 验证及其设计依据。
- [实验脚本](scripts/protocol-probe/main.go)：仅依赖 Go 标准库和原生 Git，不访问网络、不使用个人凭据。

SSH 密码代理复用原站接受的凭据；仅支持公钥的原站仍需委托或签名方案。见 [SSH 说明](docs/ssh.md)。

## 许可证

MIT，见 [LICENSE](LICENSE)。第三方依赖保留各自的许可证。
