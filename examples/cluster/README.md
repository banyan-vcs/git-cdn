# Cluster example

一套 docker-compose 拓扑：一个负载均衡把同一个域名上的 HTTP 和 SSH 分发给可扩缩的
git-cdn 实例，外加一个演示用源站。仓库根的 `docker-compose.yml` 是单实例快速启动；
这份示例验证的是多实例形态。

```text
load balancer ──┬─ HTTP :80  → node ×N（8080，各自缓存卷）
（traefik）      └─ SSH :22  → node ×N（2222，宿主键烘焙进镜像）

origin（演示源站：SSH 密码 + git-shell，HTTP lighttpd + git http-backend）
```

HTTP 和 SSH 在同一个实例上：git-cdn 本来就是单进程双协议，一个实例的两条
前端共用同一份缓存。要独立扩缩两组时，把 compose 里的 `node` 服务拆成两份、
各挂一份去掉 `ssh:` 段或只留 `ssh:` 段的配置即可。

## 集群首页

起集群后浏览器打开 `http://127.0.0.1:8080/`（或你的域名）：介绍、按当前地址生成的
clone 命令、以及监控区——直连 Prometheus 即时查询（每 5 秒刷新；带 metrics profile
起才有数据，否则给出起法提示）。首页只占 `/` 和 `/index.html` 两个精确路径，git
路由不受影响。

## 约定

- 宿主 SSH 键烘焙进节点镜像（`keys/node/`），所有实例对客户端呈现同一身份——
  负载均衡轮到哪个实例都不会触发 host key 变更告警。键目录在镜像之外且
  gitignore，`gen-keys.sh` 重生成后需要 rebuild，不 rebuild 键就不变。
- 每个实例一个独立缓存卷（匿名卷，随容器走）。缓存不共享是 git-cdn 的设计边界：
  目录锁和单写者假设以进程为单位。N 个实例 = N 份磁盘和 N 次冷启动注水，
  鉴权不受影响（每请求实时做）。
- 实例通过 label 自动注册进负载均衡，`--scale node=N` 即扩缩。
- traefik 钉 v3.7：v3.5 的 docker provider 与 Docker Engine 29 的 API 不兼容。

## 起停

先生成键材料（一次），再起：

```sh
cd examples/cluster
sh gen-keys.sh
docker compose up -d --build --wait --scale node=2
git clone http://127.0.0.1:8080/octocat/Hello-World.git
```

单站点模式没有站点前缀：`http://<cdn>/git/git.git` 直接映射
`https://github.com/git/git.git`。

网络到不了 github 时（或在离线环境验证拓扑），换离线上游配置
（指向内置 origin）：

```sh
GIT_CDN_NODE_CONFIG=./node.offline.yaml \
  docker compose up -d --build --wait --scale node=2
git clone http://127.0.0.1:8080/demo/repo.git
```

SSH 永远指向演示 origin（密码 `demo`）。github 的 SSH 只收公钥不收密码，
密码代理这条路在 github 上走不通；换成任何接受密码/token 的 SSH 源站即可：

```sh
GIT_SSH_COMMAND='ssh -o UserKnownHostsFile=client_known_hosts' \
  git clone ssh://git@127.0.0.1:2222/demo/repo.git
# 密码：demo（交互输入，或用 SSH_ASKPASS）
```

改完配置或键重建后，要丢掉旧缓存卷重新播种（compose 的 recreate 默认继承
匿名卷，不 renew 的话卷里旧的宿主键和镜像里新的不一致）：

```sh
docker compose up -d --build --force-recreate --renew-anon-volumes --scale node=2
```

收掉整套：`docker compose down`（加 `-v` 连卷一起删）。

## 用域名

要用 `git-cdn.example.com` 而不是 127.0.0.1：宿主 hosts 加一条
`127.0.0.1 git-cdn.example.com`，再用标准端口起（80/22 被占用时改回环境变量
默认值 8080/2222）。真实部署把 DNS 指到机器即可，HTTPS 由 traefik 终结或配
git-cdn 的 `tls_cert`/`tls_key`（示例配置是 `allow_insecure_http` 明文）：

```sh
echo '127.0.0.1 git-cdn.example.com' >> /c/Windows/System32/drivers/etc/hosts
HTTP_PORT=80 SSH_PORT=22 docker compose up -d --wait --scale node=2
git clone http://git-cdn.example.com/octocat/Hello-World.git
```

客户端无感改写（可选，对已有检出生效，push 也会走 CDN 透传）：

```sh
git config --global url."http://git-cdn.example.com/".insteadOf "https://github.com/"
```

## 查看存储与指标

每个实例的 `/metrics` 在内部网络的 8080 上（traefik 只对外分发 80→HTTP、
22→SSH）；聚合计数（`cache_entries`、`cache_bytes`、`repository_count`）从这里取，
过 LB 的 `/metrics` 会轮到随机实例。具体 scope 在实例的缓存卷里：

```sh
docker compose exec --index=1 node ls /var/lib/git-cdn/caches/repos/
```

目录名是身份哈希（上游 URL + 仓库路径 + 对象格式），不直接暴露仓库名；
这是设计——指标和存储都不带仓库名。`/metrics` 与本示例都只做聚合视图。

要常驻采集就带上 metrics profile，Prometheus 按容器名抓每个实例，
UI 和外部 Grafana 的接入点在 `127.0.0.1:19090`（`PROM_PORT` 可改），
时序数据保留 15 天：

```sh
docker compose --profile metrics up -d --build --wait --scale node=2
```

扩到 3 个实例以上时，在 `prometheus.yml` 的 targets 里补 `node-3` 起
的容器名。看板在仓库的 `dashboards/git-cdn.json`，导入任何 Grafana
后把数据源指到 `http://<docker-host>:19090` 即可。

## 换真实源站

- HTTP：改 `node.yaml` 的 `upstream.url`（或改 `mode: multi` 配多个站点）。
- SSH：改 `node.yaml` 的 `ssh.upstream`（`address`、`user`、`known_hosts`、
  `host_key_policy`）。演示 origin 的公钥钉在镜像里的
  `/etc/git-cdn/origin_known_hosts`，换源站时换成新源站的公钥
  （`gen-keys.sh` 里 `origin_known_hosts` 的生成方式可参考），或改用
  `host_key_policy: accept-new`。

## 边界

- 单机编排。多主机要换 swarm/k8s/nomad，但节点侧配置不变。
- SSH 经 traefik TCP 转发不带 PROXY protocol，节点看到的来源地址是 LB；
  涉及按源地址的认证限频时要知道这一点。HTTP 侧由 traefik 带 X-Forwarded-For。
- 宿主键打进镜像意味着任何能拉镜像的人都能拿到它；镜像只在本地构建时无所谓，
  推送到共享 registry 前考虑换成构建时注入或只读挂载。

## 文件

| 文件 | 作用 |
| --- | --- |
| `compose.yaml` | 拓扑：lb、node、origin；`metrics` profile 附带 Prometheus |
| `Dockerfile.node` | 节点镜像：从仓库根构建二进制 + 烘焙宿主键 |
| `Dockerfile.origin` | 演示源站：SSH 密码 + git-shell，HTTP lighttpd + git http-backend |
| `node.yaml` / `node.offline.yaml` | 实例配置（github 单站点 / 内置 origin），`GIT_CDN_NODE_CONFIG` 切换 |
| `prometheus.yml` | 按容器名抓每个 node 实例（metrics profile 用） |
| `gen-keys.sh` | 生成/重生成全部键材料和 known_hosts |
| `keys/`、`origin_known_hosts`、`client_known_hosts` | 生成的键与信任文件（gitignore，不入库） |
