# Investigation：daemon 恢复失败后永久卡死，无自愈机制

- 日期：2026-07-14
- 影响范围：nydusd 在 snapshotter 重启触发的 `Recover()` 流程中启动失败时
- 现象：pod 持续 `CreateContainerError`，报错 `daemon socket ... not found`，kubelet 重试数千次也不会自愈，僵尸进程永久残留
- 关联但独立于：[docs/nydus-blob-meta-empty-investigation.md](nydus-blob-meta-empty-investigation.md)（那份文档记录的是 blob.meta 数据损坏；本次记录的是"数据完好、只是进程起不来"这个场景）
- 版本：修复落在 nydus-snapshotter `v0.15.15-sp4`（commit `13a04de`、`063954f`、`7b032ef`）

## 一、背景：Bug 1（nydusd 启动 ENOTCONN）不止发生在首次挂载

此前的调查确认了 nydusd 有一个概率性的启动竞态：FUSE mount 成功、FUSE INIT 协商成功后，`stat` 挂载点会拿到 `ENOTCONN`，nydusd 判定启动失败直接退出。本次调查发现，这个竞态**不止在镜像拉取后的首次挂载会触发，snapshotter 自身重启触发的 `Recover()` 流程（为所有持久化 daemon 记录重新拉起 nydusd）同样会触发**。

## 二、根因：一次失败之后，没有任何代码路径会重新尝试

`NewFileSystem` 的 `Recover()` 流程（修复前）对每个持久化 daemon 只尝试一次：`ClearVestige()` → `StartDaemon()` → `WaitUntilState(Running)`，失败就 `return nil`，永久放弃。此后：

- kubelet 的 `CreateContainer` 重试，只会调用 `WaitUntilReady` → `getDaemonByRafs` → `d.WaitUntilState(Running)`，读取的是同一个**已经标记为失败、从未被重启**的 daemon 记录，不会触发新的 `StartDaemon`；
- 周期性 metrics 采集同样只读状态，不会重启。

结果：一次启动失败 = 永久故障，唯一的恢复手段是人工 `crictl rmi` 强制重新拉取镜像——即便镜像数据完全没有损坏。

## 三、生产实例：3 天未回收的僵尸

节点 `10-251-176-83`，daemon `d913gcabhpbvhv15p7dg`（snapshot 163）：

```
Error: failed to create containerd container: wait until daemon is RUNNING: get daemon state: daemon socket /var/lib/containerd/io.containerd.snapshotter.v1.nydus/socket/d913gcabhpbvhv15p7dg/api.sock: not found
```

kubelet 重试 3310+ 次，持续 17 小时以上。`ps` 显示：

```
root     280996  0.0  0.0      0     0 ?        Z    Jul10   0:00          \_ [nydusd] <defunct>
```

pid 280996 正是最初那次 ENOTCONN 崩溃留下的进程，3 天没被回收——因为它的父进程（snapshotter 自身）从未调用过 `wait()`：没有任何代码路径会主动 `Terminate`/`Destroy` 一个"Recover() 里失败了、但从未被显式删除"的 daemon 记录。

## 四、修复：Recover() 里加有限次重试

`pkg/filesystem/fs.go`，[fs.go:70-71](../pkg/filesystem/fs.go) 定义重试参数，`egRecover.Go` 循环体（[fs.go:147-197](../pkg/filesystem/fs.go)）改造为：

```go
const (
	daemonRecoverMaxAttempts = 50
	daemonRecoverRetryDelay  = 500 * time.Millisecond
)
...
for attempt := 1; attempt <= daemonRecoverMaxAttempts; attempt++ {
	d.ClearVestige()

	if err := fsManager.StartDaemon(d); err != nil {
		lastErr = err
		log.L.Warnf("Failed to start daemon %s during recovery (attempt %d/%d): %v",
			d.ID(), attempt, daemonRecoverMaxAttempts, err)
		time.Sleep(daemonRecoverRetryDelay)
		continue
	}

	if err := d.WaitUntilState(types.DaemonStateRunning); err != nil {
		lastErr = err
		log.L.Warnf("Daemon %s did not become running during recovery (attempt %d/%d): %v",
			d.ID(), attempt, daemonRecoverMaxAttempts, err)
		// 进程起来了但没到 RUNNING，必须先回收，否则每次失败都泄漏一个僵尸
		if terr := d.Terminate(); terr != nil {
			log.L.Warnf("Failed to terminate daemon %s after failed recovery attempt: %v", d.ID(), terr)
		}
		if werr := d.Wait(); werr != nil {
			log.L.Warnf("Failed to wait for daemon %s to exit after failed recovery attempt: %v", d.ID(), werr)
		}
		d.ResetState()
		time.Sleep(daemonRecoverRetryDelay)
		continue
	}

	if err := d.RecoverRafsInstances(); err != nil {
		log.L.Warnf("Failed to recover mounts for daemon %s, skipping: %v", d.ID(), err)
		return nil
	}
	fs.TryRetainSharedDaemon(d)
	return nil
}

log.L.Errorf("Daemon %s failed to become running after %d recovery attempts, giving up: %v",
	d.ID(), daemonRecoverMaxAttempts, lastErr)
```

关键设计：

- 每次重试前都重新走 `ClearVestige()`，复用死挂载 lazy-detach 逻辑；
- 失败时显式 `Terminate()` + `Wait()` 回收进程，不再让僵尸堆积；
- 按 Bug 1 的实测单次成功率（约 85%），连续多次失败的概率随次数指数下降。

## 五、验证过程中发现的两个衍生问题

### 5.1 有时候连续 5 次都失败

生产观测（节点 `10-251-186-96`，daemon `d9abd0tae5jr87q9v3qg` / snapshot 150）：连续 5 次重试，nydusd 日志显示**完全相同**的崩溃原因（`Stat mountpoint ... ENOTCONN`），确认不是数据损坏（blob.meta 完好），纯粹是同一个启动竞态连续命中。已排除并发抢占导致概率升高的可能（该节点当时只有 2 个 nydusd）。将 `daemonRecoverMaxAttempts` 从 5 提到 **50**（commit `063954f`），给足够的重试余量。

### 5.2 `WaitUntilUnmounted` 没有 ENOTCONN 感知，死连接持续泄漏

排查 5.1 时发现，`d.Wait()`（[daemon.go:597](../pkg/daemon/daemon.go)）在回收僵尸进程之后，还会调用 `mount.WaitUntilUnmounted()` 确认挂载点已释放。这个函数只会调 `IsMountpoint()` 并盲目重试，遇到死挂载的 ENOTCONN 会**傻等 1 秒（20×50ms）然后放弃**，从不真正调用 lazy detach：

```
level=error msg="umount .../150/mnt" error="canonicalise path for .../150/mnt: lstat .../150/mnt: transport endpoint is not connected"
```

死挂载要等到**下一轮**迭代开头的 `ClearVestige()` 才被真正摘除；如果重试耗尽次数放弃（或进程被杀），最后一次失败留下的死挂载就再也没有"下一轮"来清理——永久泄漏。生产验证：同一节点 `/sys/fs/fuse/connections/` 有 **10 个**连接，但当时只有 **2 个**活的 nydusd，多出的 8 个就是历次放弃/重启遗留的死连接。

修复（`pkg/utils/mount/mount.go`，[mount.go:107](../pkg/utils/mount/mount.go)，commit `7b032ef`）：复用已有的 `DetachIfDeadMount` helper，检测到死挂载直接 lazy detach，不再傻等：

```go
func WaitUntilUnmounted(path string) error {
	return retry.Do(func() error {
		if detached, err := DetachIfDeadMount(path); detached {
			return err
		}
		mounted, err := IsMountpoint(path)
		...
	}, ...)
}
```

`d.Wait()` 在每次失败的重试（包括最后一次即将放弃的那次）都会被调用，所以这个修复同时解决了"每次失败多浪费 1 秒"和"最后一次失败的死挂载没人清理"两个问题，不需要在重试循环放弃之后再额外补一次清理。

## 六、如何验证修复生效

日志层面：

```bash
grep -E "attempt [0-9]+/50|failed to become running after.*recovery attempts" nydus-snapshotter.log
```

- 出现 `(attempt 1/50)` 后**没有更多同 daemon 的 warning** → 第 2 次重试悄悄成功了（成功分支不打日志）；
- 出现 `(attempt N/50)` 一路打到 `failed to become running after 50 recovery attempts, giving up` → 50 次都失败，需要人工介入（但概率极低）。

现场事实层面（比日志更可信）：

```bash
# 有没有活的 nydusd 在服务这个 daemon
ps aux | grep nydusd | grep <daemon-id>

# 死连接数量是否等于当前活跃 daemon 数（不应该随失败次数累积）
ls /sys/fs/fuse/connections/

# pod 是否恢复正常
kubectl describe pod <pod-name> | tail -15
```

## 七、修复前后对比

| 环节 | 修复前 | 修复后（sp4） |
|---|---|---|
| Recover() 单次启动失败 | 永久放弃，daemon 记录标记死亡 | 最多重试 50 次，每次约 85% 成功率 |
| 失败进程回收 | 不回收，僵尸永久残留 | 每次失败显式 `Terminate`+`Wait` |
| 死挂载清理（d.Wait 路径） | 傻等 1 秒后放弃，不清理 | 立即检测 ENOTCONN 并 lazy detach |
| 最终结果 | 需人工 `crictl rmi`（即使数据完好） | 自动恢复，无需人工介入 |
| 故障定级 | 单次概率性崩溃 = 永久故障 | 单次概率性崩溃 = 数十秒内自愈的瞬时抖动 |

## 八、遗留事项

- Bug 1（nydusd 启动 ENOTCONN 的根因）仍未定位，参见 [nydus-blob-meta-empty-investigation.md](nydus-blob-meta-empty-investigation.md) 第三节的分析和定位手段。
- 本次三个修复（`13a04de`、`063954f`、`7b032ef`）均已随 `v0.15.15-sp4` 发布。
