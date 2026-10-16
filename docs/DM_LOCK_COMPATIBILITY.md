# DM8 迁移锁兼容性排查（2026-10-16）

## 结论与边界

migrate-plus 将旧实现的 SELECT + Scan 改成匿名块 + sql.Out，同时绑定输出
返回值和输入锁 ID，在本次真实 DM8 环境中引入兼容性回归。独立 database/sql
复现不使用 GORM、应用 bootstrap 或业务迁移 SQL。SELECT 绑定同一锁 ID 可成功，
匿名块混合 OUT/IN 绑定则在 Execute 阶段返回 6001/EOF。无需增加重试或修改 go-image。

这一结论定位到调用形式与执行阶段，不等同于已经确认 DM 服务端或客户端的具体
内部缺陷。没有取得服务器同一时刻日志或协议抓包，因此无法进一步确认服务端
为何关闭连接，也不能断言用户原日志中的 broken pipe 就是首个底层错误。

## 环境与复现

- 服务端：`DmConnection.ServerVersion=8.1.4.80`；`V$VERSION` 返回
  `DM Database Server 64 V8`、`DB Version: 0x7000d`、
  `03134284368-20260318-318144-20149`、`Msg Version: 44`。
- `COMPATIBLE_MODE=2`，大小写敏感，`BACKSLASH_ESCAPE=0`，`DDLAutoCommit=true`。
- 原生 Go 客户端：`gitee.com/chunanyong/dm v1.8.23`；未引入 GORM。
- 地址脱敏为 `172.16.x.x:5237`。原生连接指定 schema，并统一使用
  `autoCommit=true&rwSeparate=0&doSwitch=0&driverReconnect=false&maxRows=0&enRsCache=false&escapeProcess=false`。
  不记录用户名、密码或完整 DSN。
- 第一轮对照时间：北京时间 2026-10-16 10:49:58–10:49:59
  （UTC 02:49:58–02:49:59）。每个用例日志记录 UTC 纳秒时间和服务端 SessionID，
  可与服务器日志对齐。示例失败 session：`139805144977192`、`139805883109928`。

复现入口：`database/dm/lock_compatibility_test.go`，单独使用环境变量
`MIGRATE_PLUS_DM_LOCK_PROBE_DSN`。每个用例新建独立 sql.DB，预留一条 sql.Conn，
空闲连接上限为 0，结束时销毁物理会话；不会复用失败连接。
每个成功用例执行 SELECT 1、申请锁、释放锁、再次 SELECT 1；任一步失败立即停止
该用例，只清理资源。锁 ID 统一为旧实现的 `483917264`。

| 调用形式 | 等待秒数 | Context | 结果 |
|---|---:|---|---|
| 旧实现原样：SELECT、X_MODE、常量 ID | 60 | Background | 锁/解锁均 0，前后 SELECT 1 成功 |
| SELECT、模式 6、常量 ID | 60 | Background | 成功 |
| 匿名块仅 OUT 绑定，ID 为常量 | 60 | Background | 成功 |
| SELECT、模式 6、输入绑定 ID | 60 | Background | 成功 |
| 匿名块 OUT 返回值 + IN 锁 ID | 60 | Background | REQUEST 6001/EOF |
| SELECT、模式 6、输入绑定 ID | 0 | Background | 成功 |
| 匿名块 OUT 返回值 + IN 锁 ID | 0 | Background | REQUEST 6001/EOF |
| SELECT、模式 6、输入绑定 ID | 0 | 可取消，90 秒期限 | 成功 |
| 匿名块 OUT 返回值 + IN 锁 ID | 0 | 可取消，90 秒期限 | REQUEST 6001/EOF，Context 未取消 |
| 同上，显式 Prepare/Execute/Close | 0 | 可取消，90 秒期限 | Prepare 成功，Execute 6001/EOF，Close 随后同样报错 |

对比 SELECT 与混合绑定匿名块时使用相同 DSN、ID、模式、等待时间和 Context 类型。
因此，本次故障不依赖客户端轮询、动态 ID 或 Context 取消。
不能将“驱动支持 sql.Out”推导为“此匿名块在该服务器正常”。

## 初始化与驱动源码追踪

`Open → WithInstance → initialize → ensureVersionTable → lock`：读取版本、
schema、兼容模式和转义设置后，先获取初始化锁，再检查/创建版本表，最后解锁。
后续 Up 等操作再通过 golang-migrate 获取迁移锁。

在临时 schema 上，修改前调用真实 migrate-plus 初始化，重现相同匿名块错误，
尚未执行版本表 DDL 或业务迁移。修改后相同入口初始化成功。
元数据 Rows 在取消 Context 前关闭；构造函数的 Context 只在返回时取消；
未发现这些阶段提前关闭连接、泄漏 Rows 或提前取消 Context 的证据。

客户端 v1.8.23 源码（模块缓存内）：

- `m.go: execContext`：有参数时先 prepare，再转换参数，再执行，最后 defer
  `stmt.close()` 和 `finish()`；隐式 Statement.Close 的返回错误未向上传递。
- `u.go: prepare/encodeArgs/executeInner`：参数方向、类型取自预编译返回的
  serverParams，支持 sql.Out，执行后解码 OUT 返回数据。这只能证明存在支持代码。
- `u.go: free`：释放 Statement 会发送协议请求；连接已异常时可能再次报通信错误。
- `m.go: watchCancel/startWatcher/finish`：取消会关闭物理会话；Background 路径
  同样可复现失败，故 Context 取消不是本次失败的必要条件。
- `a.go`：读写协议失败可被包装为 6001。“网络通信异常”本身无法区分 SQL 调用
  触发的会话终止、驱动协议错误和外部网络断连。

本次显式执行路径中第一个可见客户端错误是 Execute 的 EOF，Close 错误在后。
没有修改客户端或声称已经定位到其某一行编码缺陷。

## 最小修复

只在 `database/dm` 将 REQUEST/RELEASE 改成：

```sql
SELECT DBMS_LOCK.REQUEST(?, 6, 0, FALSE) FROM DUAL
SELECT DBMS_LOCK.RELEASE(?) FROM DUAL
```

通过同一预留 sql.Conn 的 QueryRowContext().Scan() 读取返回码。
保留 schema ID/迁移表派生的动态锁 ID、`release_on_commit=FALSE`、原有争用
轮询、初始化期限、StatementTimeout、Close 取消、断连丢弃和返回码检查。
不对未知错误、权限错误、断连或非零返回码返回成功；不重连续用旧会话锁。
保留 v0.1.1 的 `SF_GET_PARA_VALUE(2, 'BACKSLASH_ESCAPE')` 修复。
公开 Driver 接口及 version/dirty 逻辑没有变更。

## 真实数据库验证范围

业务 GOIMAGE schema 只用于无 DDL 的原生调用对比和元数据读取。
迁移和含 Drop 的测试仅使用本次新建的独立临时 schema
`MP_DM_LOCK_20261016_1100`，用管理员连接切换到该 schema 后运行。
普通 RESOURCE 测试账户读取 INI 参数曾返回 -5730，属于明确权限错误，
没有将其归类为通信错误或当作成功；未为该账户授予 DBA。
测试结束后删除了本次临时用户/schema，查询系统目录确认对应条目数为 0。

通过的真实库测试：初始化、upstream version/dirty conformance、两连接互斥、
COMMIT/ROLLBACK/DDL 后保持锁、显式解锁与 Close 后其他连接可获取锁、
正常 Up、重复 Up 返回 ErrNoChange、失败迁移持久化 dirty、Force/Down、
执行超时终止会话并释放锁、从原生客户端关闭会话后拒绝替换连接且锁可重新获取。
后者是受控关闭原生连接，不冒充真实网络分区或服务端故障注入。

完整 `TestIntegration` 有一项既有失败：`native Drop dependencies` 残留 4 个对象。
实查残留为 `MIGRATE_PLUS_DROP_SEQ`（SEQ，PID=150995947）、
`MIGRATE_PLUS_FN`（PROC，PID=50331748）、`MIGRATE_PLUS_SYN`（SYNOM，PID=NULL）、
`MIGRATE_PLUS_VIEW`（VIEW，PID=NULL）；原 Drop 查询的 `PID = -1` 排除了这些对象。
这与锁兼容性回归分开记录，本补丁未修改 Drop 策略，也未修改断言掩盖失败。

PG 兼容模式（7）、其他服务器版本、集群故障转移、真实网络中断、服务器同期日志、
跨 schema 依赖及可选物化视图测试未验证。未更改现有服务器的 COMPATIBLE_MODE。

单元回归另覆盖返回码（含未知值）、空/NULL/非法结果、Rows 关闭错误、权限错误、
争用、Close 取消进行中的申请、锁/解锁断连，以及初始化锁失败时停止后续版本表操作。
这些 mock 仅验证错误处理，不作为原生兼容性证据。

最终本地检查通过：`go build ./...`、`go test ./... -count=1`（无数据库环境变量，
集成测试跳过）、`golangci-lint run ./...`（0 issues）及 `git diff --check`。
Go 编译、模块及 lint 缓存均放在仓库忽略的 `.cache` 下。
真实库另外运行上述 7 组锁/迁移生命周期集成子测试，全部通过；不将这次定向通过
描述为包含 Drop 的完整集成套件通过。
