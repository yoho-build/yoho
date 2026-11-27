# 有状态服务与数据库高可用：Kamal、Uncloud 能教 Yoho 什么

日期：2026-11-27。语言：中文。相关：ADR 0003、0005、0016、0017、0018，
`docs/site/high-availability.md`，`docs/dev/plan-mesh-ci-and-pg-replication-recipe.md`。

## 问题

用户问：“Yoho 能不能做多台 Server 加私有网络，比如 PG 主从（master/slave）？”

main 上的现状如下：

- 多 Server 摆放和私有网络已经有了，分别是 Roles（ADR 0017/0019）和可选的 WireGuard mesh（ADR 0018）。
- `high-availability.md` 写明数据库复制不在范围内：
  - Stateful Service 只跑在一台 Server 上（ADR 0005）。
  - Yoho 不搭建 streaming replication 或 Patroni，也不做 failover 和 split-brain 处理。
  - 建议用托管数据库，或者自己运维集群、当作外部依赖。
  - 恢复方式是异地 Backup 加 `yoho backup drill`。

这份报告看 Kamal 和 Uncloud 在这件事上的做法，以及 Yoho 能借鉴什么。

## Kamal（basecamp/kamal 2.x）

- **数据库是 accessory。** 通过 `host`、`hosts`、`role`、`roles`、`tag` 或 `tags` 选择机器，支持 `files`、`directories`、`volumes`、`env`、`port`、`options`、`proxy` 等字段。
  来源：<https://kamal-deploy.org/docs/configuration/accessories/>
- **一个 accessory 放到多台机器，就是 N 份独立副本。** 它们之间没有共享存储，也没有复制。
  - deploy 时 accessory 不会跟着更新，也不是零停机。
  - `kamal accessory remove` 会删掉容器、镜像和服务目录。
- **App 怎么找到数据库，Kamal 不管。** Docker network 只在一台机器内部有效，跨机器时用户自己在 env 里写私有 IP。Kamal 也不会等 accessory 就绪才启动 App。
  来源：<https://www.ivanturkovic.com/2026/02/06/honest-take-kamal-rails-deployment/>
- **没有内置复制。** 最接近的是一篇社区文章：
  1. 主库 accessory 用 `cmd: postgres -c wal_level=replica ...`。
  2. 从库 accessory 通过 `files` 把 `init-replica.sh` 放进 `docker-entrypoint-initdb.d`，脚本里跑 `pg_basebackup -R`。
  3. replicator 角色和 `pg_hba.conf` 要手工改，容器重建就丢了。
  4. 没有主从切换（promote）流程。

  来源：<https://dennmart.com/articles/high-availability-postgresql-replication-with-kamal/>
- **37signals 自己的生产 MySQL** 用 Orchestrator 管 23 个集群，新副本用 clone plugin 建。文章没有提 Kamal accessory，所以“生产数据库不走 accessory”只是推断，未验证。
  来源：<https://dev.37signals.com/leaving-aurora-without-losing-automatic-failover/>
- **相关 issue** 都是关于命名和共享，不是 HA：
  - accessory 名字不含 destination：<https://github.com/basecamp/kamal/issues/1415>
  - 两个 App 共用一个 PG：<https://github.com/basecamp/kamal/discussions/1178>

  没找到维护者对数据库 HA 的正式回应（搜索不完全，未验证）。

## Uncloud（psviderski/uncloud）

- **volume 只存在于单台机器。** 带 volume 的 service 会被固定到已有该 volume 的机器上；`x-machines` 可以限定放在哪几台。
  来源：<https://uncloud.run/docs/compose-file-reference/extensions/>
- **`mode: global` 加 `x-machines`**：每台列出的机器各跑一个容器，各有一个 volume，同样是独立副本。
  来源：<https://uncloud.run/docs/compose-file-reference/support-matrix/>
- **官方 Postgres 示例只有单实例**，固定在一台机器上。
  来源：<https://uncloud.run/docs/guides/deployments/deploy-specific-machines/>
- **不复制 volume。** 作者的立场是需要 HA 的应用（比如数据库）自己做复制。
  来源：<https://hn.edgecompute.app/item/43285730>（一年前的 HN 评论）
- **内部 DNS 可以指定某台机器上的实例。**
  - `<service>.internal` 解析到全部实例。
  - `nearest.<service>.internal` 优先返回本机实例。
  - `<machine>.m.<service>.internal` 只返回那台机器上的实例。
  - 不能指定单个容器。

  来源：<https://uncloud.run/docs/concepts/services/internal-dns/>，<https://github.com/psviderski/uncloud/pull/426>

## 其他工具（一句话）

- **Fly Postgres Flex**：用 repmgr，由 repmgrd 自动 failover，生产至少要 3 个节点。现在标为 unmanaged，不再维护。
  来源：<https://fly.io/docs/postgres/getting-started/what-you-should-know/>
- **Dokploy**：没有内置复制，社区建议用 Patroni 或 Pgpool-II。
  来源：<https://github.com/Dokploy/dokploy/discussions/3646>
- **Patroni / CloudNativePG**：真正的自动 failover，都依赖常驻的选主进程（DCS 或 operator）。未逐一核实。

## 结论：三家都不管数据库 HA，Yoho 的差异在于把手工步骤做成可复现

Kamal 和 Uncloud 都**不做**数据库复制和 failover，Yoho 现在“out of scope”的立场和业界一致。真正的自动 failover（Orchestrator、repmgrd、Patroni）都需要常驻进程，和 ADR 0003（无常驻 daemon）冲突。

Yoho 可以做得比 Kamal 社区方案更好，因为现有功能已经补上了 Kamal 缺的那几块：

| Kamal 社区方案的缺口 | Yoho 现有对应 |
| --- | --- |
| 跨机器地址靠手写 IP | Roles Connection：provider 只发布到私有地址，consumer 自动拿到 `db` 和 `db.<app>.yoho.internal` |
| 数据库端口可能暴露 | `mangle PREROUTING` 防火墙，只放行 consumer 的地址 |
| 不等主库就绪 | 先部署 provider，再从每台 consumer Server 检查端口是否就绪 |
| replicator 密码和 `pg_hba` 手工改，重建就丢 | 生成的 secret（`x-yoho.generate` 加 `rotate.strategy: postgres`）配合 init 脚本，每次重建都能复现 |
| 没有备份 | Backup、Backup Target、`backup drill` |

## 建议（按优先级）

1. **Postgres 主从方案（recipe），不新增功能。**
   - `db`（主库）和 `db-standby` 是两个不同的 Stateful Service，各自固定在一台 Server 上，所以 ADR 0005 和 0017 都不用改。
   - Connection `primary` 的 consumer 写成 `[web, db-standby]`。
   - 从库在空目录时跑 `pg_basebackup -R`。
   - 提供文档、示例文件和 CI 集成测试。
2. **主从切换写成文档里的手工步骤，不做自动 failover。** 依次是：
   1. `yoho app exec db-standby -- pg_ctl promote`。
   2. 修改 Connection 指向新主库。
   3. `yoho apply`。
   4. 用空 volume 把旧主库重建成新的从库。
3. **mesh 的 CI。** WireGuard mesh 目前没有自动测试，要补上。
4. **放进 backlog，暂不做：**
   - `yoho db promote` 命令。它是一次性的 SSH 操作，不违反 ADR 0003，但要谨慎设计 Connection 的切换。
   - Uncloud 式的按 Server 指定实例的别名（`<server>.m.<service>`）。
   - Kamal 式按 Server 的 tags 和 env。
5. **明确不做：** 自动 failover，以及在 Yoho 内部实现选主或 CRDT。原因是 ADR 0003 和 0016。
