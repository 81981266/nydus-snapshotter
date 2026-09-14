- 日期：2026-07-10
- 影响范围：约 200+ 台节点中 30+ 台命中，涉及镜像 `harbor.shopeemobile.com/sts_ai_agent_sandbox/base:v0.1.2-nydus`
- 现象：pod 持续 `CreateContainerError`，报错 `blob.meta ... is empty`，不人工干预永不自愈
- 取证节点：10.131.218.1（cls-98vnx4k8），快照 122
- 版本：nydusd v2.4.3（commit 6c11431e），nydus-snapshotter v0.15.15-sp1（基于上游 main）

## 一、结论（TL;DR）

两个独立 bug 叠加：

**Bug 1（引信）：nydusd 首次启动异常退出。** 镜像拉完后第一次为它启动 nydusd 时，FUSE mount 和 INIT 都成功了，但紧接着 `calc_fuse_conn` 去 `stat` 自己的挂载点拿到 `ENOTCONN`，nydusd 认为启动失败、直接退出（`fusedev.rs:774` → `main.rs:535`）。退出时不清理自己刚挂的 FUSE → 留下一个 `Transport endpoint is not connected` 的死挂载。非必现（约 200+ 台节点中 30+ 台命中），根因还没最终定位（用户态启动竞态最可疑），实测命中节点 `rmi` 重拉后第二次启动均正常。

**Bug 2（放大器）：copyFile 自毁。** snapshotter 每次挂载前会把 snapshot 目录里的 blob.meta 同步到 cache 目录（`copyBlobMetaFiles`，`fs.go:506`）：优先 `os.Link` 硬链接，失败就回退 `copyFile`。问题在于：

1. 第 1 次挂载：硬链接成功 → cache 里的文件和 snapshot 里的文件是**同一个 inode**；
2. nydusd 死了（Bug 1），kubelet 重试，触发第 2 次挂载；
3. 第 2 次挂载再跑 `copyBlobMetaFiles`：`os.Link` 报 `file exists`（第 1 次已经链过）→ 回退 `copyFile(src, dst)`；
4. `copyFile` 用 `os.Create(dst)` 打开目标 = `O_TRUNC` 截断。但 dst 和 src 是同一个 inode——**截断 dst 的同时把 src 也清零了**；
5. 随后 `io.Copy` 从已经空了的 src 读，拷贝 0 字节。结果：snapshot 和 cache 两侧的 16 个 blob.meta 全部变成 0 字节。

**无法自愈**：后续每次挂载，mount 前校验读到 0 字节 blob.meta 永久失败；镜像"已存在"（IfNotPresent）不会重新 unpack，只能人工 `rmi`。

关系：Bug 1 决定会不会发生（概率性的），Bug 2 决定发生后无法自愈。修复重点是 Bug 2。

## 二、完整时间线（节点 10.131.218.1，全部证据可复核）

| 时刻 | 事件 |
|---|---|
| 10:30:52.968 | bootstrap 层 unpack 完成，快照 122 commit，16 个 blob.meta 内容完好（见下方校验日志中的真实大小） |
| 10:30:52.980 | 挂载 #1：校验通过，blob.meta 硬链接进 cache 目录（nlink=2，同 inode） |
| 10:30:53.080 | 拉起 nydusd（pid 34517） |
| 10:30:53.740 | nydusd：FUSE mount 成功、FUSE INIT 协商成功、fuse server 线程启动 |
| 10:30:53.741 | nydusd：stat 挂载点 ENOTCONN → 判定启动失败 → 进程退出（Bug 1），死挂载残留 |
| 10:30:56.878 | snapshotter 销毁该 daemon；umount 死挂载失败（lstat 先报 ENOTCONN）；删除 nydusd 的 config/logs/socket（日志证据被销毁） |
| 10:30:58.856 | 挂载 #2：校验通过（文件仍完好）；`os.Link` 报 file exists → 回退 copyFile |
| 10:30:58.902~906 | **copyFile 把 16 个 blob.meta 全部截断为 0 字节**（Bug 2，与文件 mtime 精确吻合） |
| 10:30:58.954 | 挂载 #2 失败：`mkdir 122/mnt: file exists`（被死挂载挡住） |
| 10:30:58.965 起 | 挂载 #3 及以后：校验读到空文件，`blob.meta is empty` × 20，永久失败循环 |
| 10:50:44 | cache GC unlink 掉 cache 侧硬链接（快照侧文件 nlink 2→1，ctime 变化，与 stat 吻合） |

## 三、Bug 1：nydusd 启动异常退出（ENOTCONN 自杀）

### 现象的准确描述

不是"每次首挂必死"。准确说法：**镜像拉取完成后，该镜像在该节点上的第一个 nydusd 实例，在启动收尾阶段有一定概率（本次集群 200+ 台中 30+ 台命中）遇到 FUSE 连接已中止，主动退出**。同一节点 `rmi` 后重拉，第二个 nydusd 实例均正常启动（多节点验证）。

### 死亡证据（nydusd 日志，日志平台采集，原文完整行）

```
[/src/fusedev.rs:697] Stat mountpoint "/var/lib/containerd/io.containerd.snapshotter.v1.nydus/snapshots/122/mnt", Socket not connected (os error 107)

[src/bin/nydusd/main.rs:535] Failed in starting daemon: Socket not connected (os error 107)
```

死前 1ms 内 mount 和 INIT 都是成功的（同一日志流）：

```
mount source rafs dest /var/lib/containerd/io.containerd.snapshotter.v1.nydus/snapshots/122/mnt with fstype fuse opts default_permissions,fd=6,rootmode=40000,user_id=0,group_id=0,max_read=1052672,allow_other fd 6
FUSE INIT major 7 minor 34
start fuse servers with 4 worker threads
State machine(pid=34517): from Ready to Running, input [Start], output [Some(StartService)]
```

snapshotter 侧看到的对应现象（nydus-snapshotter.log，原文完整行）：

```
time="2026-07-10T10:30:53.802716014+08:00" level=error msg="Process 34517 has been a zombie"
time="2026-07-10T10:30:53.802782688+08:00" level=error msg="Nydusd d985in8f6n68b3acmh30 probably not started"
```

### 关键代码（nydusd，Rust，commit 6c11431e）

启动收尾：Mount → Start 之后，stat 挂载点计算 FUSE connection ID（供 failover 用），stat 失败则 `?` 直接上抛，启动流程整体失败。`service/src/fusedev.rs:765-774`：

```rust
        daemon
            .on_event(DaemonStateMachineInput::Mount)
            .map_err(|e| eother!(e))?;
        daemon
            .on_event(DaemonStateMachineInput::Start)
            .map_err(|e| eother!(e))?;
        daemon
            .service
            .conn
            .store(calc_fuse_conn(mnt)?, Ordering::Relaxed);
```

`calc_fuse_conn` 就是 stat 挂载点取设备号，`service/src/fusedev.rs:695-704`：

```rust
fn calc_fuse_conn(mp: impl AsRef<Path>) -> Result<u64> {
    let st = metadata(mp.as_ref()).inspect_err(|e| {
        error!("Stat mountpoint {:?}, {}", mp.as_ref(), &e);
    })?;
    let dev = st.st_dev();
    let (major, minor) = (major(dev), minor(dev));

    // According to kernel formula:  MKDEV(ma,mi) (((ma) << 20) | (mi))
    Ok(major << 20 | minor)
}
```

错误最终在 `src/bin/nydusd/main.rs:534-536` 打出并退出：

```rust
            .inspect_err(|e| {
                error!("Failed in starting daemon: {}", e);
            })?
```

注意：这条失败路径**不会 umount 自己 20ms 前刚挂好的 FUSE**，死挂载（`Transport endpoint is not connected`）由此产生，内核残留连接可在 `/sys/fs/fuse/connections/<minor>/` 观察到（本例为 253）。

### 根因分析（未最终定位，按可能性排序）

`stat` FUSE 挂载点返回 ENOTCONN 的唯一前提是内核已将该连接标记为断开，触发路径只有三种：

1. **用户态自身竞态（最可能）**：nydusd/fuse-backend-rs 启动时序问题——fuse server 线程刚启动的窗口内，某线程对 `/dev/fuse` fd 的操作异常导致 session 被 abort。旁证：失败集中在拉取刚完成、stream 预取（BlobPrefetcher）已并发开跑、节点 IO/CPU 繁忙的首次启动，竞态窗口被放大；重试即成功。
2. **FUSE INIT reply 协商问题**：内核 `process_init_reply` 若不接受 daemon 的应答会主动 `fuse_abort_conn`。节点内核为定制版 5.15.0-189.012-s，定制补丁属排查对象。
3. **内核 FUSE 自身 bug（可能性最低）**：5.15 的 FUSE 成熟稳定，同内核 lxcfs 正常，dmesg 无任何 fuse 异常记录。

定位手段（下次复现时）：`bpftrace -e 'kprobe:fuse_abort_conn { printf("pid=%d comm=%s\n", pid, comm); @[kstack]=count(); }'`——调用栈来自 `fuse_dev_release` 则为路径 1，来自 `process_init_reply` 则为路径 2。

## 四、Bug 2：copyFile 截断硬链接，清零 blob.meta

### 触发证据（nydus-snapshotter.log，原文完整行）

挂载 #2 开始时文件仍然完好（校验日志给出 16 个文件的真实大小）：

```
time="2026-07-10T10:30:58.857021436+08:00" level=warning msg="bootstrap is not ready, retrying... attempt=1/20 error=bootstrap is not ready, current state: {7176192 [{10a42202a49fdbd896bbaf6fb8d79b21838655cfbae1888a943e72c24cfe36f6.blob.meta 98304} {10e0967b6991f3080cd8e06ac9caf0d4ba4d80f34dc2c5d41a5ce887d26a501f.blob.meta 24576} {201e4ef5ec06891975d2a4da24681d5ef0960e910bcf04585176175b3c77d7f7.blob.meta 45056} {2c3b6bd5e6738f966671b8ccf2a16e39a70cac3961316b7ea2ce30b3adcfddcc.blob.meta 8192} {318922acacbcd488bb60dc89a7becaad8838e7d7d9e4404b102b1e1798745568.blob.meta 8192} {37c24e8c758578129a2d01a74dcf4d809affb88c6a5e6ab8398528c2165006d0.blob.meta 8192} {4a1aa094bf0094e8d3072dc7f416e4d5bfb67f5109c20757612c628615f9aee6.blob.meta 36864} {67bd85a151fa208b748379d460542b9cd3da4b1366798ad712e9459b10108b05.blob.meta 12288} {8605228345321a941d00aa464f06f501bf440a1847fe85f29c91f47fb68a3ad1.blob.meta 73728} {8e31c8c24e3760314ac1d0efdc731ab9a33d4d2ada34dbcf6a27e2e86dcb257a.blob.meta 16384} {8f09190b1413fca16a778e622fe791e4bf68465a7e69bbf489d9b67539595efd.blob.meta 8192} {97552ee9dd4e49c34e6d0bd072e61ae59a35b5a496ef657ac9f2c1feafcc1626.blob.meta 8192} {98547159589519ef5153ba72b34bd794df193d96984331decf425fd312513f11.blob.meta 200704} {c734e84ffdcce0a524499b15709f58299bf6ed53365ff749a6f545ce4c25379a.blob.meta 8192} {ce89a580888ff552464790d0492eba5b82c21c402e9e18c31a619c64280fd37d.blob.meta 8192} {f33841e974035bd75c4450bdaa245c18bc08d0d22f9a9fd0da2ce5b45ef76492.blob.meta 8192}]}"
```

随后 hardlink 因 cache 中已存在（挂载 #1 建的硬链接）而失败，回退 copy（16 个文件各一条，示例一条）：

```
time="2026-07-10T10:30:58.907525887+08:00" level=warning msg="Failed to create hardlink for 10a42202a49fdbd896bbaf6fb8d79b21838655cfbae1888a943e72c24cfe36f6.blob.meta: link /var/lib/containerd/io.containerd.snapshotter.v1.nydus/snapshots/122/fs/image/10a42202a49fdbd896bbaf6fb8d79b21838655cfbae1888a943e72c24cfe36f6.blob.meta /var/lib/containerd/io.containerd.snapshotter.v1.nydus/cache/10a42202a49fdbd896bbaf6fb8d79b21838655cfbae1888a943e72c24cfe36f6.blob.meta: file exists, falling back to copy"
```

文件系统证据（stat）：16 个 blob.meta 全部 size=0，mtime 集中在 10:30:58.902~906，与上述日志时刻精确吻合：

```
/var/lib/containerd/io.containerd.snapshotter.v1.nydus/snapshots/122/fs/image/10a42202a49fdbd896bbaf6fb8d79b21838655cfbae1888a943e72c24cfe36f6.blob.meta nlink=1 size=0 mtime=2026-07-10 10:30:58.902933622 +0800 ctime=2026-07-10 10:50:44.477529922 +0800
```

### 关键代码（nydus-snapshotter，Go，上游 main 同款）

`pkg/filesystem/fs.go:506-530`（节选）——hardlink 优先，EEXIST 时回退 copy：

```go
func (fs *Filesystem) copyBlobMetaFiles(bootstrap, cacheDir string) error {
	...
	if err := os.Link(srcPath, dstPath); err != nil {
		log.L.Warnf("Failed to create hardlink for %s: %v, falling back to copy", fileName, err)
		if err := fs.copyFile(srcPath, dstPath); err != nil {
			return errors.Wrapf(err, "copy blob meta file %s", fileName)
		}
	...
}
```

`pkg/filesystem/fs.go:532-546`——致命点在 `os.Create`：

```go
func (fs *Filesystem) copyFile(src, dst string) error {
	source, err := os.Open(src)
	...
	destination, err := os.Create(dst)   // O_CREATE|O_WRONLY|O_TRUNC
	...
	_, err = io.Copy(destination, source)
	return err
}
```

机制：dst（cache 文件）与 src（快照文件）是**同一 inode**（此前硬链接）。`os.Create(dst)` 的 `O_TRUNC` 把共享 inode 截断为 0——src 同时被清空；`io.Copy` 再从已空的 src 拷贝 0 字节。结果两侧全部归零。

### 永久失败的形成：校验拦截 + 无重新 unpack 机会

mount 前校验 `pkg/filesystem/bootstrap.go:203`：

```go
	if info.Size() <= 0 {
		return blobMetaState{}, fmt.Errorf("blob.meta %s is empty", path)
	}
```

之后每次 CreateContainer 都停在这里（原文完整行）：

```
time="2026-07-10T10:30:59.920596262+08:00" level=warning msg="bootstrap is not ready, retrying... attempt=20/20 error=blob.meta /var/lib/containerd/io.containerd.snapshotter.v1.nydus/snapshots/122/fs/image/10a42202a49fdbd896bbaf6fb8d79b21838655cfbae1888a943e72c24cfe36f6.blob.meta is empty"
```

kubelet 侧最终报错（原文完整行）：

```
Error: failed to create containerd container: wait for bootstrap file snapshot 122: bootstrap is not ready after 20 attempts: blob.meta /var/lib/containerd/io.containerd.snapshotter.v1.nydus/snapshots/122/fs/image/10a42202a49fdbd896bbaf6fb8d79b21838655cfbae1888a943e72c24cfe36f6.blob.meta is empty
```

镜像已在本机（imagePullPolicy: IfNotPresent），kubelet 不会重新 pull/unpack → 无自愈可能。

## 五、次生问题（两处，均在失败清理路径）

**死挂载摘不掉**：umount 前先 lstat 路径，而死 FUSE 挂载点 lstat 本身就报 ENOTCONN，umount 永远失败，死挂载长期残留并阻塞后续 mkdir 与快照 GC（原文完整行）：

```
time="2026-07-10T10:30:58.843400920+08:00" level=error msg="umount /var/lib/containerd/io.containerd.snapshotter.v1.nydus/snapshots/122/mnt" error="canonicalise path for /var/lib/containerd/io.containerd.snapshotter.v1.nydus/snapshots/122/mnt: lstat /var/lib/containerd/io.containerd.snapshotter.v1.nydus/snapshots/122/mnt: transport endpoint is not connected"
```

**失败现场被销毁**：daemon 启动失败后清理逻辑连日志目录一起删除，nydusd 死因证据丢失（本次靠日志平台留档才定位到 Bug 1）（原文完整行）：

```
time="2026-07-10T10:30:58.843633387+08:00" level=info msg="Deleting resources [/var/lib/containerd/io.containerd.snapshotter.v1.nydus/config/d985in8f6n68b3acmh30 /var/lib/nydus/logs/d985in8f6n68b3acmh30 /var/lib/containerd/io.containerd.snapshotter.v1.nydus/socket/d985in8f6n68b3acmh30]"
```

## 六、修复方案（三项，均在 nydus-snapshotter）

1. **copyBlobMetaFiles 同 inode 保护（止血，必须）**：`os.Link` 返回 EEXIST 时，`os.SameFile` 判断 src/dst 是否同一 inode——是则跳过（此前已链接，无需操作）；确需覆盖时先 `os.Remove(dst)` 再链接，绝不对可能是硬链接的目标做 `O_TRUNC`。
2. **lazy umount（自愈，强烈建议）**：清理路径改用 `umount2(path, MNT_DETACH)`，不依赖对死挂载点的 stat，确保死挂载必被摘除，后续挂载重试与快照 GC 不再被阻塞。
3. **失败时保留 nydusd 日志（可观测，建议）**：daemon 异常退出/启动失败时保留（或归档改名）`/var/lib/nydus/logs/<id>/` 与 config，正常关闭才删除，为 Bug 1 根因追踪保留现场。

## 七、修复前后对比

| 环节 | 修复前 | 三项修复后 |
|---|---|---|
| Bug 1 触发（nydusd 启动异常退出） | 死挂载残留 | 死挂载被 lazy umount 摘除，日志保留可查 |
| kubelet 第 2 次挂载重试 | copyFile 清零全部 blob.meta；mkdir 撞死挂载失败 | 文件完好（同 inode 跳过）；mnt 干净，正常拉起新 nydusd |
| 最终结果 | 校验永久失败，节点该镜像永久 CreateContainerError，需人工 rmi | 新 nydusd 启动成功（实测重试即活），容器 Running，用户侧仅一次数十秒级延迟 |
| 故障定级 | 节点级永久故障，30+/200+ 台需逐台人工处理 | 单次瞬时抖动，自动恢复 |

## 八、单节点应急 SOP（修复发布前）

```bash
umount -l /var/lib/containerd/io.containerd.snapshotter.v1.nydus/snapshots/<id>/mnt
crictl rmi harbor.shopeemobile.com/sts_ai_agent_sandbox/base:v0.1.2-nydus
# 等待工作负载自动重拉；确认快照目录已被 GC、/sys/fs/fuse/connections 中死连接消失
```

批量排查中毒节点：

```bash
find /var/lib/containerd/io.containerd.snapshotter.v1.nydus/snapshots -name "*.blob.meta" -size -1c 2>/dev/null
```

## 九、遗留事项

- Bug 1 根因未定位（用户态竞态 vs INIT 协商 vs 内核），复现时用 bpftrace 抓 `fuse_abort_conn` 调用栈定案。
- Bug 2 与两处次生问题均为上游 `containerd/nydus-snapshotter` main 代码（非本地定制引入），建议提 issue/PR 回馈上游。
