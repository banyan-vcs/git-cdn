# 本地协议实验

执行日期：2026-11-18。测试程序使用临时仓库，运行后自动清理；无外网请求，无个人凭据。

```sh
go run ./scripts/protocol-probe
```

| 实验 | 已验证行为 | 对设计的影响 |
| --- | --- | --- |
| 缓存保留上游已删除 ref 的对象 | 当前上游 `ls-refs` 不包含 secret commit；本地 v2 fetch 仍成功返回其 pack，并可解出 secret 文件。三个 `allow*SHA1InWant` 开关都显式设为 false | `ls-refs` 成功和关闭 Git 开关，都不能替代 Go 层的对象授权判断 |
| commit 存在，但引用的 blob 缺失 | `cat-file --batch-check` 返回 commit；`rev-list --objects --missing=print` 找到缺失 blob；upload-pack 退出码 128，stdout 已含 `packfile` 节 | 需要依赖闭包完整性；流式输出开始后不能重新拼接一次回源结果 |
| 精确 OID 补齐 | cache 不含新 tip；用原生 Git fetch 该 OID 及依赖后，本地 upload-pack 成功 | 可按请求根对象补齐，无需每次 mirror 全部 refs |
| 完整服务端缓存服务浅克隆客户端 | 对完整 cache 发 `deepen 1`，得到 shallow-info 和 pack；服务端未生成 shallow 文件 | 客户端浅克隆不要求把共享服务端缓存也改成 shallow |
| v2 多轮协商 | 带 have、wait-for-done、不带 done 的 fetch 只返回 acknowledgments，无 pack | 不能假定每个 fetch 请求都是最终 pack 响应；不跨轮拼装响应 |
| v2 初始 advertisement | 初始响应含 `version 2` 和 capabilities，不含 branch tip | v2 的 GET info/refs 与 POST ls-refs 是两个阶段，GET 200 不等于已获取 refs |

脚本强制使用 SHA-1，目的是给出可复现的最小验证；生产实现还需要 SHA-256、客户端版本及编码矩阵。实验通过 `git upload-pack --stateless-rpc` 验证协议字节，没有启动 HTTP 或 SSH 网络服务。

关于任意 OID 的结果，还同时检查了当前 Git 文档中的 v2 `want <oid>` 定义，以及 Git 源码的 `parse_want()` 路径。它不应被描述成特定 CDN 的漏洞或特定托管服务的 ACL 结论；风险来自 CDN 在实时鉴权后把共享、保留历史的本地对象库直接交给这个原语。
