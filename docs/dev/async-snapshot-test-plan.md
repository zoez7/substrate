# Async Snapshot 场景测试计划：怎么触发、怎么在 log / request log / actor status 里验证

> [!NOTE]
> 本计划配合 [async_snapshot_error_analysis.md](async-snapshot-error-analysis.md) 使用，场景编号（A1…C9）沿用那份文档。所有结论基于 `main@d2b97509` 的代码。

> [!IMPORTANT]
> **更正上一版 §6.3**：两侧都用真实代码的 harness 无法放进一个 Go package。`cmd/atelet/internal/...` 只能被 `cmd/atelet/...` 下的代码 import，`cmd/ateapi/internal/...` 同理。所以跨组件测试要拆成三块：
> - **H-api**：ateapi 侧的 functionaltest，扩展现有的 [FakeAteletServer](../../cmd/ateapi/internal/controlapi/functionaltest/main_test.go#L116-L146)（bufconn + 真实 postgres）。
> - **H-let**：atelet 侧，在 [lifecycle_test.go](../../cmd/atelet/lifecycle_test.go) 和 snapshottracker 测试里加上 fake reporter / fake object store / gate。
> - **E**：kind e2e，用真实的二进制和 fault injection 做端到端确认。
>
> H-api 和 H-let 之间用**契约**对齐：同一个场景在两侧各写一半，一侧断言"我发出了什么"，另一侧断言"收到这个请求时我会怎么做"。

---

## 1. 可观测面：每一类证据去哪里看

| 证据 | 去哪看 | 命令 / 字段 |
|---|---|---|
| **RL：Request log** | ateapi、atelet、worker 的 `"Handle RPC"` 行（JSON） | `kubectl logs -n ate-system deploy/ate-api-server`、`kubectl logs -n ate-system <atelet-pod> -c atelet`；字段：`method`、`req`、`resp`、`err`、`elapsed-time`、`principal`（仅 ateapi） |
| **CL：Component log** | 同上，外加 `-c objectstore-plugin` | 见各场景卡片里列出的 `msg` |
| **AS：Actor status** | 控制面 DB | `kubectl ate get actor <a> -a <ns> -o yaml`；字段：`status.state`、`status.crash.message`、`status.assigned_node`、`status.last_assigned_generation`、`status.worker_assignment`、`status.snapshots[].{generation,uuid,locality,durable_snapshot.status,durable_snapshot.object.snapshot_uri}` |
| **NS：Node state** | atelet 所在 node | `kubectl debug node/<n> -it --image=busybox -- sh`，然后 `cat /host/var/lib/ate/actors/<uid>/snapshot-state.json`、`ls /host/var/lib/ate/actors/<uid>/local-checkpoint/` |
| **OS：Object storage** | snapshot URI 前缀 | manifest 是否存在（manifest 最后写入 = 上传已 commit） |
| **EV：Actor event** | OTLP log | `ate.actor.state_changed`、`ate.actor.crashed` |
| **M：Metrics** | collector | `atelet.snapshot.upload.duration`、`atelet.snapshot.detach.duration`、`atelet.snapshot.{upload,detach}.overdue.duration`、`atelet.snapshot.upload.overdue.actors`、`atelet.snapshot.undurable.age.max`、`ate.actor.crashes` |
| **TR：Trace** | ateapi 的 workflow step span | `step.CallAteletSuspend`、`step.FinalizeSuspended`、`step.CallAteletRestore` 等 |

**时间线工具（建议新增，放在 `hack/debug/actor-timeline.sh`）**：输入 `atespace/actor`，从 ateapi、atelet（所有 node）、objectstore-plugin 的 log 中按 actor uid 过滤，按时间合并；同时 dump AS、NS、OS。每个 E 测试结束时自动跑一次并存档。
过滤示例：`jq -c 'select((.req|tostring|test("<uid>")) or (.resp|tostring|test("<uid>")) or .actorUID=="<uid>" or ."ate.actor.uid"=="<uid>")'`

---

## 2. Request logging 现状与缺口

### 2.1 服务端（谁在接收请求）

| 服务 | 进程 | 现状 | 缺口 | 加在哪里 |
|---|---|---|---|---|
| `ateapipb.Control`（外部 API） | ate-api-server | ✅ [ServerUnaryInterceptor](../../internal/ateinterceptors/ateinterceptors.go#L43-L75) 记录 req/resp/err/principal | 没有 stream RPC，但 stream 链里也没有日志 | — |
| `ateapipb.WorkerService`（含 **ReportActorSnapshotState**） | ate-api-server | ✅ 同上 | — | — |
| `ateletpb.Atelet`（Checkpoint/Restore/Run/Terminate/Promote/Detach） | atelet | ✅ [InternalServerUnaryInterceptor](../../cmd/atelet/main.go#L529-L535) | — | — |
| `ateletpb.WorkerSupport`（RequestActorSuspend、MintActorCertificate、RegisterWorker、GetActorEgressPolicy） | atelet | ❌ [workerFacingSrv 没有 interceptor](../../cmd/atelet/main.go#L471) | **L1** | `grpc.NewServer(..., grpc.UnaryInterceptor(ateinterceptors.InternalServerUnaryInterceptor))`。加之前先做 §2.4 的 secret 审计 |
| `ateworkerpb.Worker` | ateworker-gvisor / ateworker-microvm | ✅ | — | — |
| `objectstorev1.{Node,Asset,Control}Provider`（Upload/Fetch/Cleanup/Copy/FetchAsset） | objectstore-plugin | ❌ [只有 otel stats](../../cmd/objectstore-plugin/main.go#L95) | **L2**（A3/A4/B2/C5 判断"manifest 到底有没有写、cleanup 到底删了什么"都依赖它） | 同上加 interceptor；`actor_jwt`、`asset_uri` 已标记 `debug_redact` |
| `credproviderpb.CredentialProvider` | kubernetes-secrets / gsm provider | ❌ [k8s](../../cmd/credential-provider/kubernetes-secrets/main.go#L120-L123)、[gsm](../../internal/plugins/gcp-secret-manager/cmd/gsm-credential-provider/main.go#L131) | **L3**（不在 async 流程里，但属于"所有 API"） | 加 interceptor；`opaque_bytes` 已标记 redact |
| atenet xDS / ext_proc / SDS | atenet | 都是 stream，并且在 Envoy 热路径上 | 不在本计划范围 | 如果要覆盖，单独设计 stream 日志（每条消息记录会撑爆日志量） |

### 2.2 客户端（谁在发请求）：目前全部没有

只有服务端日志不够，有两类情况必须靠客户端日志：
1. **服务端成功、客户端看到失败**（response 丢失）。这正是 A1 的触发条件，只看服务端会显示"一切成功"。
2. **请求根本没到服务端**：dial 失败、TLS 失败、deadline 在发出前就到了。

| 调用链 | 位置 | 缺口 |
|---|---|---|
| ateapi → atelet | [dialer.go grpc.NewClient](../../cmd/ateapi/internal/controlapi/dialer.go#L157) | **L4a** |
| atelet → ateapi（report、RegisterWorker 等） | [main.go ateapiConn](../../cmd/atelet/main.go#L349) | **L4b** |
| atelet → worker | [main.go worker dial](../../cmd/atelet/main.go#L2018) | **L4c** |
| ateapi/atelet → objectstore-plugin | [pluginsocket.Dial](../../internal/pluginsocket/pluginsocket.go#L96) | **L4d** |
| atelet stats poller | [statspoller.go](../../cmd/atelet/statspoller.go#L614) | 高频，建议 Debug 级别或排除 |

做法：在 `internal/ateinterceptors` 新增 `ClientUnaryInterceptor`，记录 `method`、`target`、`req`、`resp`、`err`、`code`、`elapsed`，以及 `ServerElapsedTrailer`（用来区分网络延迟和服务端处理时间），然后在上面每个 `grpc.NewClient` 都用 `grpc.WithChainUnaryInterceptor` 挂上。
注意：`ErrNoAteletOnNode` 发生在 dial 阶段，在任何 RPC 之前，client interceptor 也看不到它，所以还需要 L6。

### 2.3 非 RPC 的状态变化：request log 覆盖不到

后台的 upload、detach、report 和 workflow 的内部决策都不是 RPC，必须单独加日志。

| ID | 位置 | 现状 | 要加什么（全部带上 actor uid、snapshot uuid、gen） |
|---|---|---|---|
| **L0** | 所有 proto log 的渲染方式 | slog `JSONHandler` 用 `encoding/json` 渲染 proto。按 protobuf-go 的行为推断，**enum 会显示成数字**，Timestamp 显示成 `{seconds,nanos}` [需验证：看一行真实日志即可] | 在 [contextlogging](../../internal/contextlogging/contextlogging.go#L155-L185) 里把 `proto.Message` 转成 protojson（`json.RawMessage`），并且在 redact 之后再转。否则 `state:3`、`mode:2` 这样的日志没法 grep |
| **L5** | [report.go L196-L228](../../cmd/atelet/internal/snapshottracker/report.go#L196-L228) | 只记录 latestLocal 和 latestDurable 的 uuid | 记录完整的 `sent` state、`dueSince`、第几次尝试、gRPC code（如果 L4b 已经做了，这里只需补 dueSince 和尝试次数） |
| **L6** | [stepSpan](../../cmd/ateapi/internal/controlapi/workflow.go#L50-L61) | step 失败只写进 span | 失败时 `slog.WarnContext("Workflow step failed", step, actor, code, err)`；`markSkipped` 时打 Debug |
| **L7** | [ApplyReportedSnapshotState](../../cmd/ateapi/internal/controlapi/snapshot_report.go#L73-L89)；[FinalizeSuspended](../../cmd/ateapi/internal/controlapi/workflow_suspend.go#L386-L415)、[FinalizeReverted](../../cmd/ateapi/internal/controlapi/workflow_revert.go#L277-L298)、[assignWorkerAttempt](../../cmd/ateapi/internal/controlapi/workflow_resume.go#L548-L577) | `logActorStateChanged` 只记录 state | `"Actor snapshots changed"`：before/after 的 `snapshots[]{gen,uuid,locality,durable.status}` 和 `assigned_node`。report 没有改变任何东西时打 Debug `"Snapshot state report changed nothing"`。用普通 slog，不改 actorevent 的 key（改 key 需要同步更新 `docs/metrics/registry/events.yaml` 并跑 `hack/verify/metrics.sh`） |
| **L8** | tracker：[CommitLocal / CommitDurable / CommitPromoted](../../cmd/atelet/internal/snapshottracker/lifecycle.go#L35-L156)、[BeginStart](../../cmd/atelet/internal/snapshottracker/lifecycle.go#L168-L193)、[beginUploadLocked](../../cmd/atelet/internal/snapshottracker/upload.go#L28-L46)、[beginDetachLocked](../../cmd/atelet/internal/snapshottracker/detach.go#L28-L37)、[backOffLocked](../../cmd/atelet/internal/snapshottracker/background.go#L494-L497)、[clearSupersededUploading](../../cmd/atelet/internal/snapshottracker/actor.go#L307-L318)、[queueReportLocked 延迟](../../cmd/atelet/internal/snapshottracker/report.go#L123-L133)、[waitForDetachLocked](../../cmd/atelet/internal/snapshottracker/background.go#L415-L430)、[Start](../../cmd/atelet/internal/snapshottracker/background.go#L127-L156) | 只有 op 结束时的 `logOp`，以及 report 和 save 失败 | `"Snapshot committed"`（kind=local/durable/promoted，记录 before/after）、`"Background upload started"`、`"Background detach started"`、`"Detach unscheduled by start"`、`"Background work backed off"`（retryAt）、`"Dropped superseded uploading snapshot"`、`"Report deferred while held"`、`"Waiting for detach to finish"`（谁在等、等了多久）、`"Recovered snapshot state at startup"`（每个 actor 一行汇总） |
| **L9** | [Restore 选择 local 还是 object](../../cmd/atelet/main.go#L1442-L1469) | 只有 local 不可用时有 Warn | `"Restore source chosen"`：source=local/object/none、local 读取错误、请求里有没有 Object、当时 tracker 的 `LatestLocal`/`LatestDurable` |
| **L10** | Checkpoint 幂等短路（A1 修复之后） | — | `"Checkpoint already committed; returning state"` |
| **L11** | [handleAteletError](../../cmd/ateapi/internal/controlapi/crash.go#L88-L107) | crash 分支有 Error 日志，但不带 code 和 rpc；不 crash 的分支完全没有日志 | 两个分支都记录 `"Atelet error classified"`：rpc、code、decision（crash/retry）、原因 |
| **L12** | 统一日志 key | tracker 用 `actorUID`，ateapi 用 `ateattr.ActorLogAttrs` | 统一用 ateattr 的 key，§1 的 timeline 才能用一个过滤条件串起所有组件 |
| **L13** | node 丢失检测（B1 修复时新增） | 不存在 | `"Node gone; actor snapshots on it"`，并带上 decision（cleared/crashed） |
| **L14** | atelet 启动对账（B3 修复时新增） | 不存在 | `"Local snapshot not tracked"` / `"Tracked snapshot missing on disk"` |
| **L15** | 永久性 upload 失败（B2） | 只会重复打 `"Snapshot operation failed"` | 分类为 permanent 时打 Error `"Snapshot unrecoverable"`；配合新的 metric 或 report 字段 |

### 2.4 "记录完整 input/response" 的两个前提（我的意见）

1. **Secret 审计必须先做**：redaction 依赖 `debug_redact` 标注，见 [contextlogging](../../internal/contextlogging/contextlogging.go#L43-L60)。目前只标了约 10 个字段。在 L1/L2/L3 上线之前，需要逐个检查 `GetActorEgressPolicyResponse`、`RegisterWorker*`、`MintActorCertificate*`、egress credential 相关 message。
   **建议加一个守护测试**：遍历所有 service 的 request/response descriptor，字段名匹配 `token|secret|password|private|jwt|credential|key` 的必须带 `debug_redact`，否则测试失败。
   另外 contextlogging 文档里写明了覆盖不到的情况：`google.protobuf.Any`、slice、`fmt.Sprintf` 格式化后的 message。所以新加的日志要把 message 作为直接的 attribute 传入。
2. **热路径的日志量**：`Control.ResumeActor` 会被 router 对每个路由请求调用一次（RUNNING 快路径），目前已经在 Info 级别完整记录。`MintWorkerActorJwt`、`FetchAsset` 也是高频。建议对这些 method 用 allowlist 降到 Debug 或者采样，生命周期类 RPC 保持 Info。这一点需要你决定（见 §6）。

---

## 3. 触发工具

| 层 | 工具 |
|---|---|
| **U** | 现有的 fake Store/Reporter + `synctest`；ateapi 的 [TestHandleAteletError](../../cmd/ateapi/internal/controlapi/crash_test.go#L358) 表驱动测试 |
| **H-api** | 扩展 `FakeAteletServer`：<br>① `CheckpointHook func(call int, req) (resp, err)`，可以按调用次数编排响应；<br>② "commit then fail"：记录 commit 后返回 `Unavailable`，用来模拟 response 丢失；<br>③ 让 Checkpoint 返回真实的 `State`（现在[返回的是空的](../../cmd/ateapi/internal/controlapi/functionaltest/main_test.go#L242)）；<br>④ 一个本地 snapshot 存在与否的小模型，Restore 时按 local/Object/none 返回结果；<br>⑤ gate（`chan struct{}`）；<br>⑥ 直接调用 `controlSrv.ApplyActorSnapshotState` 来注入 report；<br>⑦ store hook：让指定的 `UpdateActor` 返回一次 `ErrVersionConflict` |
| **H-let** | lifecycle_test 加上：fake worker（可以编排 `CheckpointWorkload` 的返回和阻塞）、fake object store plugin（可以阻塞、失败、只写 manifest、检查 cancel 是否传播）、fake Reporter（记录收到的 report，按脚本返回各种 code） |
| **E（kind）** | atelet flags：`--snapshot-upload-delay=20s --snapshot-detach-delay=60s --snapshot-report-give-up-after=60s`，需要时加 `--snapshot-report-workers=0`。<br>故障手段：<br>- `kubectl delete pod` 删除 atelet 或 ate-api-server；<br>- `kubectl scale deploy/ate-api-server --replicas=0`；<br>- NetworkPolicy 阻断 atelet pod 到 object storage（rustfs）的 egress；<br>- `kubectl debug node` 删除或截断 local-checkpoint 文件、把 `snapshot-state.json` 替换成目录（让写入失败）、写入垃圾内容；<br>- `docker rm -f <kind-worker>` 模拟 node 丢失 |

---

## 4. 场景卡片

每张卡片包含五部分：**触发方式**、**今天的预期**（红灯，用来证明问题确实存在）、**修复后的预期**（绿灯）、**验证点**、**依赖的日志缺口**。

### H0 基线：async suspend → upload → report → detach → report → resume（其他场景之前先跑通）

- **触发（E）**：创建 actor → resume → `kubectl ate suspend actor X`（默认 async）→ 等 25s → 等 65s → `kubectl ate resume actor X`。H-let/H-api 各写一份同样的流程。
- **验证**：

| 阶段 | RL | CL | AS | NS / OS / M |
|---|---|---|---|---|
| suspend | ateapi `Control/SuspendActor`，resp.state=SUSPENDED；atelet `Atelet/Checkpoint`，req `snapshot{uuid U, gen G, object.uri}` + `upload_config.mode=DEFERRED`，resp `state.latest_local.uuid=U`；worker `CheckpointWorkload` | `Checkpoint timing breakdown`；`Actor state changed` ×2（SUSPENDING、SUSPENDED）；L8 `Snapshot committed kind=local` | `SUSPENDED`、`assigned_node=N`、`snapshots[G]{uuid U, locality N, durable PENDING}` | NS：`latestLocal.uuid=U`、`detachAt`、`undurableSince`；`local-checkpoint/U/` 存在 |
| upload | objectstore `UploadSnapshot`（L2）；ateapi `WorkerService/ReportActorSnapshotState`，req `latest_local=U`、`latest_durable=U`，resp OK | `Snapshot operation finished op=upload background=true uploaded=true`；`Reported the actor's snapshot state`；L7 | `durable COMPLETED`，`locality` 仍为 N | OS：manifest 存在；M：`upload.duration` 有一个样本 |
| detach | report req `latest_local` 为空、`uploading` 为空 | `Snapshot operation finished op=detach` | `locality=""`、`assigned_node=""` | NS：local-checkpoint 为空，`latestLocal` 已清除；M：`detach.duration` |
| resume | atelet `Restore`，req 带 `snapshot.object.snapshot_uri` | `Actor has durable snapshot; Restoring from snapshot`；`Restore timing breakdown`；L9 source=object | `RUNNING` | — |

### A1 Checkpoint 在已经 commit 之后被重试 → 不必要的 crash

- **触发**：
  - **H-api**：`CheckpointHook` 第一次调用记录 commit 并返回 `Unavailable`，第二次调用返回 worker 错误（模拟 workload 已经不在）。client 拿到 Unavailable 后重试 `SuspendActor`。
  - 变体：FinalizeSuspended 的 `UpdateActor` 返回一次 `ErrVersionConflict`；sync 模式下做同样的事。
  - **H-let**：用同一个 `CheckpointRequest` 连续调用两次 `Atelet.Checkpoint`，fake worker 第二次返回 NotFound。
  - **E**（近似，时序不确定）：用 `kubectl logs -f` 盯 atelet 的 `Handle RPC method=.../Checkpoint`，一出现成功的 resp 就删除 ate-api-server pod，然后 client 重试。
- **今天的预期（红灯）**：
  - RL：atelet 有两条 Checkpoint，uuid 相同，第二条有 err；worker 第二条 `CheckpointWorkload` 有 err。
  - CL：`Setting Actor to crashed due to Atelet error`；EV `ate.actor.crashed`。
  - AS：`CRASHED`，`crash.message` 以 `suspend failed: atelet Checkpoint:` 开头。
  - **NS：`latestLocal.uuid=U` 依然存在**，这一点证明数据完好、crash 是不必要的。
- **修复后**：第二次 Checkpoint 的 resp 带 `latest_local=U`，没有 worker RL；出现 L10；AS 为 `SUSPENDED`。
- **日志缺口**：L4a（必须能看到"服务端成功、客户端 Unavailable"）、L10、L11。

### A2 Stale generation 被拒绝 → crash 了一个正在运行的 actor

- **触发**：
  - **U**：在 crash_test 加一行：`rpc=Checkpoint`、`code=FailedPrecondition`，期望不 crash。
  - **H-api**：`FailCheckpoint = FailedPrecondition("... below the actor's newest snapshot ...")`。
  - **H-let**：先用 gen=G+5 commit 一次，再用 gen=G+1 调 Checkpoint。
  - **E**（可选）：`kubectl debug node` 修改 `snapshot-state.json` 里 `latestLocal.snapshot.generation` 为一个更大的值，重启 atelet，然后 suspend。
- **今天的预期**：atelet RL Checkpoint 返回 FailedPrecondition；**没有** worker `CheckpointWorkload` RL（证明 workload 没被碰过）；ateapi 把 actor 设为 CRASHED。
- **修复后**：不 crash。actor 应该处于什么状态需要设计：留在 SUSPENDING 的话，重试会再次失败。一个选项是 atelet 在错误 detail 里返回它所知的最新 gen，ateapi 据此重新分配 generation。
- **日志缺口**：L11。

### A3 Resume 与 background detach 赛跑 → 不必要的 crash

- **触发**：
  - **H-let**：fake object store 的 upload 阻塞在 gate 上（detach 正在进行），这时用一个**不带 Object** 的 RestoreRequest 调 Restore（它会在 `BeginStart` 里等 detach）；然后打开 gate。断言 Restore 返回 NotFound。
  - **H-api**：fake atelet 的本地模型设为"已 detach"，ateapi 记录仍是 PENDING，然后 resume。断言 RestoreRequest 里没有 Object（今天），或者有 Object（修复后）。
  - **E**：`--snapshot-upload-delay=1h --snapshot-detach-delay=30s`，用大内存 actor 让 upload 慢一些；看到 L8 `Background detach started` 后立即 resume。
- **今天的预期**：
  - RL：atelet `Restore` 的 req 里没有 `snapshot.object`，err 为 NotFound `has no usable local copy and no snapshot.object to fall back to`。
  - AS：resume 前是 PENDING、locality N；resume 后是 CRASHED。
  - **OS：manifest 存在**（证明数据完好）；NS：`latestDurable.uuid=U`。
  - CL：L8 `Waiting for detach to finish`。
- **修复后**：req 带 Object；L9 显示 source=object；AS 为 `RUNNING`。
- **日志缺口**：L8、L9、L2（证明 manifest 写入的时间早于 Restore）。

### A3' crash 之后 revert 删掉了唯一的好副本

- **触发**：A3 之后执行 `kubectl ate revert actor X`（H-api 和 E 都可以）。
- **今天的预期**：ateapi → objectstore 有 `CleanupSnapshot`，prefix 是 U 的（L2/L4d）；OS 里 U 的 manifest 消失；AS 变成 `SUSPENDED`，指向更旧的 COMPLETED snapshot，或者没有 snapshot。**这一步确认了真正的数据丢失。**
- **修复后**：A3 本身不再 crash。另外 revert 在删除 PENDING 之前，如果 manifest 存在，应该先打日志并拒绝删除（取决于设计）。
- **日志缺口**：L2、L4d、L7。

### A4 Report 放弃 → ateapi 状态陈旧 → resume 时 crash

- **触发**：
  - **U**（snapshottracker report_test）：Reporter 一直返回 Unavailable 直到超过 giveUpAfter，然后 detach，Reporter 继续失败。断言 `ReportDueSince` 被清零，之后再也不会发送。
  - **E（可稳定复现）**：方式一，`--snapshot-report-workers=0 --snapshot-detach-delay=30s`，suspend，等 40s，resume。方式二，更接近真实：`--snapshot-report-give-up-after=60s`，在 upload 和 detach 窗口内把 ate-api-server scale 到 0 并保持 90s 以上，再 scale 回来，然后 resume。
- **今天的预期**：
  - CL：多次 `Failed to report the actor's snapshot state`（带 retryAfter），然后 `Giving up on reporting the actor's snapshot state`；detach 之后同样的序列再出现一次。
  - RL：ateapi 上没有 `ReportActorSnapshotState`。
  - AS：一直是 PENDING + locality N。
  - resume 时的结果和 A3 相同。
- **修复后**：resume 成功；如果实现了"放弃之后在下次变化时重新上报"，还会看到第二次 `Reported`。
- **日志缺口**：L4b、L5。

### A5 Checkpoint 在 commit 之后 cleanup 失败 → crash

- **触发**：**H-let**：fake volume plugin 的 unmount 返回错误；或者把 actor dir 的父目录设为只读，让 `resetActorDirs` 失败。
- **今天的预期**：atelet RL Checkpoint err `while unmounting external volumes` 或 `while resetting actor dirs`；NS 里 `latestLocal=U`；ateapi 把 actor 设为 CRASHED。
- **修复后**：Checkpoint 成功返回 state，或者返回一个不会触发 crash 的 code；AS 为 `SUSPENDED`。
- **日志缺口**：L11。

### A6 Restore 遇到 node 本地 FS 错误 → crash

- **触发**：**H-let**：让 `resetActorDirs` 或 `resetSandboxAssetsDir` 失败（chmod）。**U**：crash 分类表。
- **今天的预期**：Restore RL err 的 code 是 Unknown；AS 为 CRASHED。
- **修复后**：返回 `Unavailable`；AS 停留在 `RESUMING`，重试后成功。
- **日志缺口**：L11。

### A7 以后接入 DetachActor / PromoteSnapshot 时的错误分类

- **触发**：**U**：crash 分类表增加 `rpc=DetachActor` 和 `rpc=PromoteSnapshot`，code 覆盖 `Aborted`、`FailedPrecondition`、`Unimplemented`。
- **今天的预期**：全部 crash（红灯）。**修复后**：全部不 crash。这是接入这两个 RPC 之前的门槛。

### B1 Node 丢失，而最新 snapshot 只在本地

- **触发**：
  - **E**（kind 多 node）：`--snapshot-upload-delay=1h`，async suspend，然后 `docker rm -f <kind-worker-N>` 并 `kubectl delete node N`。之后依次 resume、revert、delete。
  - 变体 B1b：先等 upload 被 report 成 COMPLETED，再删除 node。
  - **H-api**：fake dialer 对 N 返回 `ErrNoAteletOnNode`，再加上一个 node lister 显示 N 已经不存在。
- **今天的预期**：
  - RL：`ResumeActor` 返回 `ResourceExhausted: no worker has room` 且一直如此；`RevertActor` 和 `DeleteActor` 的 err 都是 `while connecting to atelet on node "N": no atelet pod found on node`。
  - AS：`SUSPENDED`、locality=N、PENDING，并且永远不变。
  - **没有** `ate.actor.crashed`。
  - B1b 今天同样卡住，尽管 durable 副本已经 COMPLETED。
- **修复后**：B1 → `CRASHED`，message 说明数据丢失（L13），之后可以 revert 到最近的 COMPLETED。B1b → 清除 locality 和 assigned_node，在其他 node 上 resume 成功，**不 crash**。
- **日志缺口**：L6（今天只有 RL 的 err 里能看到问题）、L13。

### B2 本地 snapshot 丢失或损坏，而且还没上传

- **触发（E）**：`--snapshot-upload-delay=1h`，async suspend；然后 `kubectl debug node` 执行 `rm -rf .../local-checkpoint/U`（变体：截断某个镜像文件或 manifest）；重启 atelet 并把 `--snapshot-upload-delay` 改成 10s。**H-let**：直接删除本地目录，fake object store 的 FetchSnapshot 返回 NotFound。
- **今天的预期**：
  - CL：`Snapshot operation failed op=upload`，err 为 `local snapshot ... is gone and no uploaded copy exists`，按 1m、2m、…、10m 的 backoff 无限重复；detach 也同样失败。
  - RL：没有 report；objectstore RL 只有 Fetch manifest 返回 NotFound（L2）。
  - M：`undurable.age.max` 持续增长。
  - AS：SUSPENDED、PENDING，直到 resume 时才因为 `NotFound` 变成 CRASHED（这一步是正确的）。
- **修复后**：出现 L15 `Snapshot unrecoverable`；ateapi 主动 crash 或者触发 alert（取决于设计）。

### B3 State file 写失败或不可读

- **触发**：
  - **H-let**：在 suspend 之前把 `snapshot-state.json` 替换成一个目录，然后重启 tracker。
  - 变体：写入垃圾内容，然后重启。
  - **E**：同样的操作通过 `kubectl debug node` 完成。
- **今天的预期**：
  - CL：`Failed to save the actor's snapshot state`（或者 `Skipping an unreadable snapshot state file`）。
  - 重启之后**没有任何** op 日志，也没有 report。
  - AS 永远是 PENDING。
  - ⚠️ M：重启后 undurable tally 是从 state file 重建的，所以这个 actor **不会出现在 `undurable.age.max` 里**。也就是说，监控也看不到这个问题。
- **修复后**：出现 L14 `Local snapshot not tracked`，并且 state 被重建或者触发 alert。

### B4 Sync 上传失败 → crash（今天的行为是正确的，作为回归测试）

- **触发**：
  - **E**：用 NetworkPolicy 阻断 atelet 到 object storage 的流量，然后执行 `kubectl ate suspend actor X --mode=sync`。
  - **H-let**：让 fake plugin 的 upload 失败。
- **预期**：
  - RL：Checkpoint 的 req 里 `mode=IMMEDIATE`，err 为 `while uploading external snapshot`。
  - AS：`CRASHED`。
  - NS：没有 local-checkpoint。
- **日志缺口**：L2（证明 upload 确实到达过 plugin）。

### B5 atelet 在 worker checkpoint 之后、commit 之前重启

- **触发**：
  - **H-let**：fake worker 的 `CheckpointWorkload` 返回之后，在 gate 上 cancel 掉 server ctx 并重建 Atelet，模拟重启。
  - **E**：worker RL 一出现 `CheckpointWorkload` 的 resp，就立即删除 atelet pod。
- **预期**：client 收到 Unavailable，重试后走到 A1 的路径，最后 crash。文件留在 checkpoint-state 目录里但没有记录。作为已知限制记录下来。
- **日志缺口**：L4a、L8（没有出现 `Snapshot committed`，可以确认停在了哪一步）。

### C1 Report 拿着 lease 时，用户操作失败

- **触发**：**H-api**：store hook 让 report 内部的 `UpdateActor` 阻塞在 gate 上（此时 report 持有 lease），然后发起 `ResumeActor`。
- **预期**：RL `ResumeActor` 返回 `Aborted: another operation is in progress for this actor`。同时检查调用方（router、kubectl、SDK）是否会重试：router 在这种情况下应该重试而不是把错误返回给终端用户。

### C2 Report 碰到 transitioning 状态

- **触发**：
  - **U**：ateapi 侧遍历 SUSPENDING、RESUMING、DELETING、REVERTING，每种状态都应返回 Aborted（应该已有测试覆盖，需要确认）。
  - **U**：tracker 侧，Reporter 连续返回 N 次 Aborted。
- **预期**：CL `Failed to report ...` 是 Info 级别（因为是 Aborted），retryAfter 逐次增长；超过 giveUpAfter 之后放弃，进入 A4。

### C3 Lease lapse 导致同一个 uuid 的 Checkpoint 并发执行

- **触发**：**H-api**：把 lease TTL 设短；fake atelet 的 Checkpoint 阻塞在 gate 上；等 lease 过期后发起第二次 `SuspendActor`（会重新进入 SUSPENDING，发出同 uuid 的 Checkpoint）；然后打开 gate。H-let 侧：并发调用两次同一个 Checkpoint。
- **今天的预期**：
  - RL：atelet 上两条同 uuid 的 Checkpoint，时间重叠；worker 上两条 `CheckpointWorkload`，其中一条有 err。
  - AS：被第二个 workflow 设为 CRASHED。
- **修复后**：第二个请求等待第一个完成，或者被去重，返回同一个 state。
- **日志缺口**：L4a、L8。

### C4 Restore 被 detach 中的 upload 阻塞

- **触发**：**H-let**：fake upload 阻塞，发起一个 deadline 很短的 Restore。
- **预期**：Restore RL 的 err 为 `DeadlineExceeded`；AS 停留在 `RESUMING`，不 crash。L8 `Waiting for detach to finish` 能看到等了多久。gate 打开之后会进入 A3 的路径。
- **日志缺口**：L8。

### C5 Paused actor 在 upload 进行中被 revert

- **触发**：
  - **H-let**：fake plugin 的 UploadSnapshot 阻塞，此时调用 Terminate（会 Forget）；检查 upload 的 ctx 是否被 cancel，以及 fake plugin 在 cancel 之后是否还在写对象。
  - **E**：大 snapshot，看到 L8 `Background upload started` 之后立即 revert。
- **预期**：
  - RL 的顺序：atelet `Terminate` → objectstore `CleanupSnapshot`（ateapi）。
  - 两者都完成之后，**OS 里 U 的 prefix 应该为空**。如果不为空，就是 orphan objects，属于泄漏。
  - AS 指向最近的 COMPLETED snapshot。
- **日志缺口**：L2、L4d、L8。

### C6 对 paused actor 调 sync suspend

- **触发（E/H-api）**：async suspend，然后立即执行 `kubectl ate suspend actor X --mode=sync`。
- **今天的预期**：RL `SuspendActor` 的 req 是 `mode=SYNCHRONOUS`，resp 是 SUSPENDED，但 snapshot 的 durable 状态仍然是 PENDING；atelet 上没有任何 RL。需要决定这个行为是否可以接受。

### C7 Upload 被记录之前给 paused actor 打 tag

- **触发（E）**：async suspend，然后立即打 tag；轮询直到出现 `ReportActorSnapshotState` RL，再重试 tag。
- **预期**：第一次 tag 返回 `FailedPrecondition ... not uploaded yet`，report 之后的 tag 成功。记录下这个窗口有多长（upload delay + 上传时间 + report 延迟）。

### C8 快速 suspend/resume 循环

- **触发（E / 以后的 model-based 测试）**：`--snapshot-upload-delay=20s`，用 20ms 到 2s 的随机间隔做 20 轮 resume/suspend 循环，最后停在 SUSPENDED 并等待收敛。
- **验证（invariant）**：
  - 每一轮 AS 的 `last_assigned_generation` 都 +1。
  - 每次 upload op 的 uuid 都是当时的 LatestLocal。
  - 被 supersede 的 gen 出现 `Ignoring a durable snapshot older than...` 或 `...no longer records`，并且**不 crash**。
  - local-checkpoint 目录数始终 ≤ 2（LatestLocal 加上正在上传的那份）。
  - 最终状态：最新 gen 是 COMPLETED，然后被 detach。
- **日志缺口**：L7、L8。

### C9 正在上传的 local dir 不被修改

- **触发**：**U**：在 microVM 的 merge 测试里，对 preserved 的 base 计算 hash，merge 前后各一次，断言不变；对 Nlink>1 的情况也做同样的断言。
- **预期**：hash 不变。

### D 回归保护

保持分析文档 §4 D 中列出的现有测试。另外加三条：
- out-of-date report（旧 uid、其他 node、旧 gen）**不会** crash，断言 RL 的 err code 和 AS 都没有变化。
- CRASHED actor 收到 report 时返回 `FailedPrecondition`，atelet 丢弃这个 report（CL `The control plane refused ...`）。
- atelet 重启之后 upload 能恢复：E 测试在 upload 进行中删除 atelet pod，之后应该出现 `Recovered snapshot state at startup`（L8）以及第二次 upload。

---

## 5. 执行顺序和完成标准

| 里程碑 | 内容 | 完成标准 |
|---|---|---|
| **M0 日志** | L0、L1、L2、L4a–d、L5、L6、L7、L8、L9、L11、L12，加上 §2.4 的 secret 守护测试 | H0 在 E 环境里，可以通过 timeline 工具从 ateapi、atelet、objectstore 三处串起完整的 actor 时间线，并且每一步都能对上 §4 H0 表格里的证据 |
| **M1 红灯** | 写出 A1、A2、A3、A4、A5、A6、A7、B1、B2、B3、C3、C5 的测试，先让它们失败 | 每个测试失败的原因与卡片里"今天的预期"一致。按照 AGENTS.md 的要求，红灯必须被亲眼看到过 |
| **M2 修复** | 按上一份文档 §7 的顺序逐个修复 | 对应的测试变绿；再做一次 mutation 检查：把修复回退，测试应该重新变红 |
| **M3 E2E** | H0、A4、B1、B2、B4、C5、C8 进入 kind e2e，使用短 delay 的 flags | 在 CI 中稳定通过；每次运行都存档 timeline |

---

## 6. 需要你决定的问题

1. **热路径日志量**：`ResumeActor`（RUNNING 快路径）、`MintWorkerActorJwt`、`FetchAsset`、stats poller 这几个高频 method，是降到 Debug、做采样，还是保持 Info 完整记录？
2. **L0 proto 渲染方式改成 protojson**：这会改变所有现有日志的格式，可能影响已有的日志查询和 dashboard。是否接受？
3. **WorkerSupport（L1）的 secret 审计**：由谁来负责？另外 `GetActorEgressPolicy` 的 response 里是否带有凭据，需要确认。
