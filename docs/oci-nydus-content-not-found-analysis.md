# OCI 镜像 + nydus runtime 报 `content digest not found` 根因分析与修复方案

> 背景:`kata-clh` runtime 的 pod 反复 `CreateContainerError`,报错
> `failed to extract layer ... to nydus ... failed to get reader from content store: content digest sha256:xxx: not found`。
> 普通 OCI 镜像用默认 runtime(overlayfs)能正常起,一旦走 nydus runtime 就报这个错。
> 本文记录完整根因和最终修复方向。

---

## 1. 结论先行(TL;DR)

- **根因不在 nydus-snapshotter**。真正读原始层数据(blob)、解压的是 **containerd 自带的 `walking` differ**,不是 nydus。
- 触发链:**kubelet 磁盘压力删镜像 → 连带删掉被多镜像共享的基础层 blob → 后续要用该层的普通镜像在 unpack 时读不到 blob → not found**。
- 失败的充要条件(三条同时成立):**①镜像 manifest 还在(containerd 认为镜像已存在,不重新 pull)+ ②该层的本地 snapshot 不存在(要重新解压)+ ③blob 也不存在**。overlayfs 那个镜像因为 ② 不成立(snapshot 早已建好并复用)所以没事;nydus 这条 ② 成立,要重新解压,一解压就撞上 ③。
- **不能在 nydus-snapshotter 里"检测缺失后 pull"**——因为读 blob/解压根本不经过 nydus-snapshotter 的代码路径(见第 7 节)。
- **最终方案:改 containerd,在 pull 决定"跳过下载"之前校验 blob 真实存在,不存在就回源重新下载**(见第 8 节)。

---

## 2. 基础概念

| 概念 | 是什么 | 存在哪 |
|---|---|---|
| **blob** | content store 中所有内容寻址存储的统称,包括三层:① **manifest** — 记录镜像由哪些层组成(JSON,几 KB);② **config** — 镜像运行参数 CMD/ENV 等(JSON,几百 B);③ **layer** — 各层的原始数据,一个 gzip 压缩的 tar 包(media type `...tar.gzip` / `tar+gzip`,几十到几百 MB)。三类 blob 不区分类别,全部按内容哈希(digest)寻址、混存在同一目录下,内容相同则 digest 相同、可跨镜像/跨节点复用 | containerd 的 **content store**(`/var/lib/containerd/io.containerd.content.v1.content/blobs/sha256/<digest>`)+ 元数据索引(boltdb) |
| **snapshot** | 某一层"解压/转换后"的本地成果,是一个可被复用的、不可变的层。容器 rootfs = 若干 snapshot 叠起来 | `/var/lib/containerd/io.containerd.snapshotter.v1.<snapshotter>/snapshots/<id>/` |
| **differ** | containerd 里真正干"读 blob → 解压 → 把文件写进 snapshot 目录"的组件。默认是内置的 `walking` differ | containerd 进程内 |
| **snapshotter** | 管 snapshot 生命周期(Prepare/Commit/Remove)。overlayfs、nydus 各是一种 snapshotter | 独立组件 / 进程 |
| **RAFS** | nydus 自己的镜像格式(`bootstrap` 元数据 + `blob` 数据块),支持按需(懒)加载 | nydus snapshot 目录下的 `fs/image/image.boot` 等 |
| **nydusd** | nydus 的 FUSE daemon,把 RAFS 挂载成一个文件系统供容器使用 | 常驻进程 `nydusd fuse --bootstrap ... --mountpoint ...` |

**核心通用原理**:任何镜像层,第一次使用都要"读 blob → 解压/转换成本地 snapshot"。**一旦这个 snapshot 成功生成,以后就复用 snapshot,不再需要 blob**;但生成 snapshot 那一刻,blob 是必需的一次性原料。

**一个容易混淆的点:"extract" 这个词在 snapshotter 日志里,但"读 blob" 不在。** 一层的 unpack 由两个组件分工完成:

- nydus-snapshotter 负责 **snapshot 生命周期**:`[Prepare] extract-xxx`(建临时快照)→ … → `[Commit] extract-xxx`(定型)/ `[Remove]`(失败回滚)。这些都记在 **nydus-snapshotter 日志**里,所以你会在里面看到 `extract` 字样。
- containerd 的 `walking` differ 负责 **中间那步**:真正读 blob、解压 tar.gz、把文件写进快照目录。这一步失败(`content ... not found`)记在 **containerd 日志**里。

所以 nydus-snapshotter 日志里有 `extract`(它在管快照),但没有 `not found`(它没读 blob)——两者不矛盾。这也是第 7 节"不能在 snapshotter 里修"的依据。

---

## 3. 磁盘目录结构（完整）

以 Node 上 `/var/lib/containerd/` 为例：

```
/var/lib/containerd/
│
├── io.containerd.metadata.v1.bolt/
│   └── meta.db                              ← 全局元数据（BoltDB 单文件）
│       ├── bucket "images"                   ← 镜像名 → manifest digest 映射
│       │     "nginx:latest" → sha256:aaa...
│       │     "redis:7"      → sha256:fff...
│       │
│       └── bucket "containers"              ← 容器定义
│
├── io.containerd.content.v1.content/
│   ├── meta.db                              ← blob 索引（BoltDB 单文件）
│   │     sha256:aaa → {size: 2048,  labels: {...}}    ← manifest
│   │     sha256:bbb → {size: 800,   labels: {...}}    ← config
│   │     sha256:ccc → {size: 27MB,  labels: {...}}    ← layer1.tar.gz
│   │     sha256:ddd → {size: 8MB,   labels: {...}}    ← layer2.tar.gz
│   │
│   └── blobs/sha256/                        ← 实际文件（扁平日录，不分子目录）
│       ├── aaa...   (2KB)   ← manifest (JSON)
│       ├── bbb...   (800B)  ← config (JSON)
│       ├── ccc...   (27MB)  ← layer1.tar.gz
│       └── ddd...   (8MB)   ← layer2.tar.gz
│
├── io.containerd.snapshotter.v1.overlayfs/
│   ├── metadata.db                          ← overlayfs snapshot 树
│   └── snapshots/
│       └── 4896/fs/                         ← 解压后的真实文件 + overlay work/
│
└── io.containerd.snapshotter.v1.nydus/
    ├── metadata.db                          ← nydus snapshot 树（与 overlayfs 完全独立）
    └── snapshots/
        ├── 213/fs/image/image.boot          ← nydus 原生镜像的 RAFS 元数据
        └── 388/fs/image/image.boot
```

### 三个 db 文件的分工

| db 文件 | 存什么 | 谁写谁读 | "镜像在不在"用它吗 |
|---|---|---|---|
| `metadata.v1.bolt/meta.db` | 镜像名 → manifest digest、容器定义、GC 租约 | containerd 核心 | ✅ **第一步查这个** |
| `content.v1.content/meta.db` | 每个 blob 的 digest/size/labels，不区分类别(manifest、config、layer 混存) | content store 插件 | ✅ **第二步：确认 manifest blob 有记录** |
| `snapshotter.xxx/metadata.db` | 该 snapshotter 自己的 snapshot 树 | 对应 snapshotter 插件 | ❌ 不参与 |

### "镜像已存在"的完整判断逻辑

```
步骤1：查 metadata.v1.bolt/meta.db → bucket "images"
        "nginx:latest" → manifest digest = sha256:aaa . . . . . . ✅

步骤2：查 content.v1.content/meta.db
        sha256:aaa（manifest blob）有记录？. . . . . . . . . . . ✅

→ 结论：镜像已存在，不 pull

        /// 它不会去验证 layer blob（sha256:ccc, sha256:ddd）
```

### 关键细节

1. **blob 文件不区分类别**：`blobs/sha256/` 下所有文件按 sha256 命名混存，肉眼分不出哪个是 manifest(2KB)、哪个是 config(800B)、哪个是 layer(200MB)，只能靠 meta.db 里的记录或文件大小推断。

2. **db 查询不碰磁盘文件**：containerd 判断"blob 是否存在"只查 boltdb(`Info(digest)`)，不会 `os.Stat` 或 `os.Open` 去验证实际文件。所以 **meta.db 有记录 ≠ 文件真在磁盘上**。

3. **snapshotter 之间完全隔离**：用 overlayfs pull 的镜像，nydus 的 `metadata.db` 毫不知情；反过来也一样。不存在"复用对方 snapshot"的可能。

4. **manifest 不是"所有镜像的汇总文件"**：每个镜像一个 manifest（独立 JSON），和其他 blob 一样存在 `blobs/sha256/` 下。`metadata.v1.bolt/meta.db` 的 images bucket 只是记录了"镜像名 → manifest digest"的映射关系，manifest 文件本身在 content store 里。

---

## 4. 三种组合对比

| 维度 | OCI 镜像 + overlayfs | OCI 镜像 + nydus(on-the-fly) | nydus 原生镜像 + nydus |
|---|---|---|---|
| registry 里存什么 | 标准 tar.gz 层 | 标准 tar.gz 层 | RAFS(构建期 `nydusify` 转好) |
| 层的本地形态 | 解压出的**真实文件** + overlay 目录 | **同 overlayfs**:解压出真实文件 + overlay 目录 | RAFS bootstrap(`fs/image/image.boot`),真实文件不落地 |
| 生成 snapshot 时读 blob 吗 | 是 | 是 | 否(数据源是 registry/blob backend,不走 content store) |
| 有常驻 nydusd 吗 | 无 | **无**(本案例实测确认) | 有(每层/共享一个 nydusd) |
| 有 RAFS 吗 | 无 | **无** | 有 |
| 依赖 content store 的 blob 吗 | 是(生成 snapshot 时) | **是**(生成 snapshot 时) | **否** |
| 会踩本次 GC 坑吗 | 本案例中没踩(snapshot 早已建好、直接复用) | **踩了**(需重新解压该层时 blob 已被删) | **不会**(不依赖该 blob) |
| 启动速度 | 慢(下载+解压全量) | 慢(同左,甚至更慢) | 快(懒加载) |

### 本案例实测证据(关键)

在节点上验证运行中的普通镜像 `migoo/sandbox-agent:v0.1.66-spfs-test`(无 `-nydus` 后缀):

- 它的层 snapshot(id 4896)目录 `snapshots/4896/fs/` 里是**实打实的真实文件**(`opt/sandbox-persist/...`)+ overlay `work/` 目录,**没有 `image.boot`,没有对应 nydusd**。
- 而 `-nydus` 原生镜像的层(如 snapshot 213/388/1345)目录 `fs/` 里**只有 `image/image.boot`(RAFS 元数据)**,且各有一个常驻 nydusd 挂载。
- `ctr plugins ls` 显示 differ 只有内置的 `walking`(`ok`)和 `erofs`(`skip`),**没有任何 nydus 自己的 differ**。
- 失败层的 `extract-xxx`(每次随机 id)在**每一次 CreateContainer 重试**时都重新出现一遍(50 分钟内上千次 `[Prepare]...[Remove]`),说明该层的解压是**"需要这个 snapshot 时被(重)触发"**的,不是"一次 pull 就永久搞定"。

**结论**:这套集成里,普通 OCI 镜像走 nydus snapshotter 时,层的处理其实**退化成了 overlayfs 式的"解压物化 + overlay 叠层"**,由 containerd 的 `walking` differ 完成,与 RAFS/nydusd 无关。只有 `-nydus` 原生镜像才真正用 RAFS + nydusd。

> 关于 unpack 具体在 pull 时还是 create 时:本文不对此下定论。我们的日志只能证明"该层需要 snapshot 而本地没有时,会去解压、去读 blob",不足以精确断定 pull 与解压之间的时间跨度。真正决定成败的是下一节的"三条件",而非某个精确的时间窗口。

> "物化"= 把压缩包(blob)解压成一个个真实文件平铺到 snapshot 目录里(等价于 `tar -xzf`);"物化 overlay"= 再把这些目录用 overlayfs 叠起来给容器用。

---

## 5. 为什么 OCI + nydus 会失败,而 OCI + overlayfs 成功

### 5.1 失败的充要条件:三条同时成立

```
① 镜像 manifest 在  →  containerd 认为"镜像已存在",不重新 pull
② 该层本地 snapshot 不在  →  必须重新解压才能得到这层(要读 blob)
③ 该层 blob 不在  →  解压时读不到原料
──────────────────────────────────────────
① ∧ ② ∧ ③  ⇒  content not found,且无法自愈
```

只要三条同时成立就炸;缺任何一条都不会出问题:
- 缺 ②(snapshot 还在)→ 直接复用 snapshot,压根不解压、不看 blob。
- 缺 ③(blob 还在)→ 解压正常,能建出 snapshot。
- 缺 ①(镜像 manifest 也没了)→ 会触发重新 pull,把 blob 拉回来。

### 5.2 套到本案例:同一个镜像,为什么 overlayfs 成、nydus 败

overlayfs 和 nydus 各维护**独立的一套 snapshot**,互不共享。

- **overlayfs 成功**:你测的那个镜像,它的层 snapshot 早已建好并留在本地(② 不成立)→ 直接复用,不解压、不碰 blob → 成功。
- **nydus 失败**:同一层在 nydus 这套里本地 snapshot 不存在(② 成立)→ 需要重新解压 → 撞上 ③(blob 已被 GC 删)→ 失败。

**这不是谁强谁弱**:
- 若该普通镜像从没用 overlayfs 跑过、其 snapshot 也不在、且 blob 已删 → overlayfs 一样会失败。
- 若 nydus 在 blob 被删前就成功建过该层 snapshot → nydus 也不会报错。

### 5.3 "为什么 nydus 这边的 snapshot 此刻不在"——诚实标注:未完全定论

这一点我们的日志**不足以干净地二选一**,有几个候选原因,都可能导致 ② 成立:
- 这台节点上第一次用 nydus 跑该镜像,此层从没在 nydus 侧建过 snapshot;
- 建过,但 nydus 侧的 snapshot 后来被 GC 清理了;
- pull 时 nydus 作为 remote snapshotter 让 containerd"跳过"了该层的下载/解压(基于别处已有的判断),导致本地既没 snapshot 也没 blob。

无论是哪一种,落点都一样:**到了需要这层 snapshot 的时刻,②③ 同时成立 → 失败**。所以修复不针对"为什么 snapshot 不在"(那有多种成因),而是针对 ③——**保证需要解压时 blob 能被补回来**(见第 8 节)。

> 关于"为什么以前用 overlayfs 从没出过":最直接的解释就是 4.2 —— 那些镜像在 overlayfs 侧的 snapshot 一直在、能复用,从不触发"重新解压读 blob"这一步,因此永远撞不上 ③。是否还叠加了"overlayfs 在 pull 时就解压完、窗口更短"这类时机因素,本文不做断言。

### 常见疑问:"blob 没了,snapshot 还在吧?复用 snapshot 不就行?"

- 前提:**能复用 snapshot,必须这个 snapshot 之前 Commit 成功过一次。**
- 本次失败的情况是:这一层在这台节点上**从没 Commit 成功过**(第一次用),要生成 snapshot 得走 `Prepare → 解压 → Commit`,而解压的输入 blob 在生成之前就没了 → **永远走不到 Commit → snapshot 压根没被创建**,自然无从复用。
- 日志对照:
  - 成功层:`[Prepare] extract-xxx` → `[Commit] ... snapshot id 4896` ✅
  - 失败层:`[Prepare] extract-xxx` → (读 blob not found) → `[Remove] ...` ❌(**没有 `[Commit]`**)

`blob` 的角色 = **生成 snapshot 时的一次性原料**:生成成功后可丢,复用不看它;生成成功前就没了,则 snapshot 永远生不出来,每次重试都缺料、每次都失败。这也是 `crictl rmi` + 重新拉能修复的原因——重拉把原料补回来了,这次解压得以成功、Commit 出 snapshot,之后就正常。

---

## 6. 完整触发链(根因)

```
1. 节点镜像文件系统磁盘用量越过 kubelet imageGC 高水位阈值
   (实测日志:"Disk usage on image filesystem is over the high threshold" usage=65 highThreshold=65)
        ↓
2. kubelet 内置 imageGC 按 LRU 删除"最近没被用"的镜像腾空间
   (实测日志:image_gc_manager.go "Removing image to free bytes",连删多个 500MB+ 镜像)
        ↓
3. kubelet 按"整个镜像"删除(它不感知层共享),连带使某个被删镜像独占引用的
   共享基础层失去引用 → containerd content GC 回收该层 blob
        ↓
4. 该 blob 实际被 7~10 个不同团队的镜像共享(同一个基础层,l.0 / l.7 位置)
   但删除时未正确保住"仍被其它 manifest 引用"的事实
        ↓
5. 之后某个普通镜像(走 nydus / 走未缓存的 overlayfs)第一次要 unpack 这一层,
   walking differ 去 content store 读 blob → not found → CreateContainer 失败
        ↓
6. containerd 认为"镜像已存在"(manifest 元数据还在),不会自动重新 pull
   → 无限重试、无法自愈,直到人工 crictl rmi + 重拉
```

要点:
- 触发在 **kubelet imageGC + containerd content GC**;
- 撞上的是"此刻需要重新解压该层、但本地 snapshot 不在(② 成立)"的负载;本案例中 overlayfs 侧该层 snapshot 一直在、能复用,不触发重新解压,所以没事(见第 5 节);
- 不自愈,是因为 containerd "镜像已存在就跳过 pull" 的逻辑——manifest 元数据还在,它不知道底下的 blob 已经没了。

---

## 7. 为什么不能在 nydus-snapshotter 里"检测缺失后 pull"

这是一个直觉上很自然、但实际走不通的方案,原因如下:

1. **读 blob / 解压不在 nydus-snapshotter 的代码路径里。**
   containerd unpack 一层的分工是:
   ```
   containerd unpacker
     → snapshotter.Prepare()   建 active snapshot     (nydus 干,记 [Prepare])
     → differ.Apply()          读 blob、解压、写文件   (containerd 的 walking differ 干)  ← 报 not found 在这
     → snapshotter.Commit()    定型                   (nydus 干,记 [Commit])
   ```
   `content not found` 是 `walking` differ 在 `Apply()` 里读 blob 失败时抛出的,**发生在 containerd 进程里**,nydus-snapshotter 全程没碰过 blob。

2. **实测佐证**:
   - `not found` 只出现在 `containerd.log`,`nydus-snapshotter.log` 里**只有 `[Prepare]`/`[Remove]`,没有 not found**——因为它根本没参与那一步。
   - `ctr plugins ls` 确认 differ 是内置 `walking`,**没有 nydus 自己的 differ**。

3. **推论**:在 nydus-snapshotter 里加再多错误捕获/重试,也拦不到这个错——它不流经 nydus。除非把架构改成"nydus 注册自己的 differ 接管 Apply",但本集成没有这么做,且那是更大的改动。

**所以修复必须落在真正读 blob 的一方——containerd。**

---

## 8. 最终方案:改 containerd —— pull 时校验 blob 真实存在,不存在则回源

### 8.1 缺陷本质

containerd 判断"某层要不要下载"时,查的是 **content store 的元数据索引(boltdb)**:只要 boltdb 里有该 digest 的记录,就判定"已存在,跳过下载"。**它不校验对应的 blob 文件是否真的还在磁盘上。**

于是出现"**索引说有、blob 文件已被 GC 删**"的不一致,而这个不一致在 pull 阶段发现不了,一直拖到 differ.Apply 真正读数据时才炸。

### 8.2 修复思路

在 containerd 决定"跳过下载"之前,增加一次 **blob 存在性校验**:

- 不只查"元数据是否存在",而是查"元数据存在 **且** blob 在 content store 里真实可读"。
- 校验失败(blob 缺失/不可读)则**当作不存在处理,走正常的回源下载路径**,把 blob 重新拉回来。
- 这样无论 GC 何时删了 blob,下次用到时 pull 会自动补齐,differ.Apply 时数据就在了。

### 8.3 大致实现位置(基于 containerd v2.x,`ske` 定制版为 v2.3.0-ske.0,以实际代码为准)

- **判断"跳过下载"的决策点**:CRI / image 处理里的 unpack 前置检查,以及 `remotes`/`images` 的 handler 链中对"层是否已在 content store"的判断(通常表现为对 `content.Store.Info(ctx, digest)` 返回成功即认为已存在)。
- **改动**:把"`Info` 成功即视为已存在"升级为"`Info` 成功 **且** 能成功打开 `ReaderAt`(或做一次轻量 `Stat`/校验 blob 落盘完整)"。失败则不走跳过分支,回到正常 fetch。
- **回源能力复用**:containerd 已有完整的 fetch/pull 下载逻辑,校验失败后复用它即可,无需新造下载通路。

### 8.4 关键设计取舍

- **只在"命中已存在"的分支加校验**,不影响首次下载路径,额外开销仅一次存在性检查(很轻)。
- 该修复修的是 **content store 一致性**这个所有 snapshotter 共用的底座,因此 **overlayfs 和 nydus 都同时受益**,不是只救 nydus。
- 它是"**治标但最实用**":不消除"blob 会被 GC 删"这个根因,但让"删了 → 用到时自动补"成为默认行为,现象消失、无需人工介入。

### 8.5 与其它 containerd 改动思路的对比

| 思路 | 改哪 | 性质 | 风险 | 采用 |
|---|---|---|---|---|
| **1. pull 校验 blob 存在 → 回源**(本方案) | CRI/image unpack 决策处 | 治标,自动补数据 | 低 | ✅ **采用** |
| 2. differ.Apply 读 not found → 回源重试 | walking differ / unpacker | 兜底自愈 | 中(differ 引入网络/凭证依赖,层次不纯) | 备选 |
| 3. 修 content GC 引用可达性(别删仍被 manifest 引用的 blob) | content GC mark-sweep | 治本源头 | 高(GC 逻辑敏感,易引入新 bug) | 长期谨慎 |

选思路 1 的理由:精准命中"索引说有、文件没了、pull 却不重下"的现象;改动局部、风险低;对所有 snapshotter 生效。思路 3 是"为什么会删"的根,但最敏感,实践中通常先用思路 1 兜住后果。

---

## 9. 附:治本方向(不改代码的长期项,供并行推进)

- **镜像原生化**:把这些普通镜像逐步转成 `-nydus` 原生格式(`nydusify convert`)。原生 nydus 镜像数据源不经过会被 GC 的 content store,**从机制上免疫本问题**,并获得懒加载启动加速。需保证与现存普通镜像并行兼容、平滑迁移。
- **registry 侧清理陈旧 tag**:减少"一层被几十个旧版本共享"的连坐面。

> 注:磁盘扩容 / 调 kubelet 阈值只能降低触发频率,磁盘终会再次超限,不作为根治手段。

---

## 10. 一句话总结

> `content not found` 的直接触发是 kubelet/containerd 的 GC 删掉了被多镜像共享的基础层 blob。失败的充要条件是三条同时成立:**①镜像 manifest 还在(不重新 pull)+ ②该层本地 snapshot 不在(要重新解压)+ ③blob 也不在**。本案例中 overlayfs 侧该层 snapshot 一直在(② 不成立)所以没事,nydus 侧 snapshot 不在、需重新解压才撞上 ③。修复不能落在 nydus-snapshotter(它不读 blob,读 blob 的是 containerd 的 walking differ),应改 containerd —— **在 pull 跳过下载前校验 blob 真实存在,缺失则回源重下**,消除条件 ③(也即修复 content store 的"索引说有、数据已丢"的不一致),让所有 snapshotter 都能自动补齐缺失的层。
