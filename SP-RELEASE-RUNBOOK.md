# nydus-snapshotter 自建 "sp" release 操作手册

> 背景:给 containerd/nydus-snapshotter 提交的 PR(支持 `stream_prefetch` 配置字段)在合并、
> 发新版本之前,我们在自己的 fork(`81981266/nydus-snapshotter`)上打补丁、自建 release,
> 供线上 pod 启动脚本下载使用。
>
> 命名规则:`v<上游基线版本>-sp<N>`,例如 `v0.15.15-sp2`。每次重新发布就把 N 加 1。
> 本文档只涉及 `containerd-nydus-grpc` 这一个二进制的重建,其余二进制
> (`nydus-overlayfs`、`optimizer-*`)始终原样使用官方 release 里的文件。

---

## 0. 一次性检查(每次开始前过一遍)

```bash
# 确认在本地仓库目录
cd /Users/tianfeng.wang/Documents/EKS/nydus-snapshotter

# 确认 remote 配置(fork 必须指向你自己的仓库)
git remote -v
# 期望:
#   fork    https://github.com/81981266/nydus-snapshotter.git (fetch/push)
#   origin  https://github.com/containerd/nydus-snapshotter.git (fetch/push)

# 工作区必须干净,否则先处理掉
git status
```

⚠️ **永远 push 到 `fork`,不要 push 到 `origin`**(origin 是上游,没有写权限,会 403)。

---

## 1. 设置本次版本变量

```bash
export BASE_TAG="v0.15.15"          # 官方基线版本(取其余二进制用)
export NEW_TAG="v0.15.15-sp3"       # 本次要发的新版本号,记得递增 N
export BUILD_DIR="$HOME/nydus-sp-build/${NEW_TAG}"   # 持久化构建目录,不要用 Claude 的临时 scratchpad
mkdir -p "$BUILD_DIR"
```

---

## 2. 确定要打包的 commit,打 tag 并推到 fork

```bash
# 选你要发布的代码状态:
#   - 通常就是 feat/fs-prefetch-stream-prefetch 分支的最新 HEAD
#   - 也可以指定某个具体 commit hash
git log --oneline -5 feat/fs-prefetch-stream-prefetch

export BUILD_COMMIT=$(git rev-parse feat/fs-prefetch-stream-prefetch)
echo "本次构建基于 commit: $BUILD_COMMIT"

# 打 tag 指向这个 commit
git tag "$NEW_TAG" "$BUILD_COMMIT"

# 推到 fork(不是 origin!)
git push fork "$NEW_TAG"
```

如果重新发布同一个 tag 名字(极少见,一般每次都递增 N),需要先删旧 tag:
```bash
git tag -d "$NEW_TAG"
git push fork ":refs/tags/$NEW_TAG"
```

---

## 3. 把仓库切到目标 commit,静态编译 containerd-nydus-grpc

```bash
git checkout "$BUILD_COMMIT"

export BUILD_TIMESTAMP=$(date -u '+%Y-%m-%dT%H:%M:%S')

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -ldflags "-s -w \
    -X github.com/containerd/nydus-snapshotter/version.Version=${NEW_TAG} \
    -X github.com/containerd/nydus-snapshotter/version.Revision=${BUILD_COMMIT} \
    -X github.com/containerd/nydus-snapshotter/version.BuildTimestamp=${BUILD_TIMESTAMP} \
    -extldflags -static" \
  -o "${BUILD_DIR}/containerd-nydus-grpc" \
  ./cmd/containerd-nydus-grpc

# 确认是静态链接的 linux/amd64 二进制
file "${BUILD_DIR}/containerd-nydus-grpc"

# 编完记得切回工作分支
git checkout feat/fs-prefetch-stream-prefetch
```

预期输出包含 `ELF 64-bit LSB executable, x86-64 ... statically linked`。

补全了 `Revision`/`BuildTimestamp` 这两个 ldflag 后,在目标机器上跑 `./containerd-nydus-grpc --version` 就能直接看到真实 commit 和构建时间,不用再靠 `go version -m` 反查 VCS 信息。

---

## 4. 下官方基线 tarball,只替换这一个二进制,重新打包

```bash
cd "$BUILD_DIR"

curl -fsSL -O "https://github.com/containerd/nydus-snapshotter/releases/download/${BASE_TAG}/nydus-snapshotter-${BASE_TAG}-linux-amd64.tar.gz"

rm -rf bin
tar xzf "nydus-snapshotter-${BASE_TAG}-linux-amd64.tar.gz"
ls -l bin/          # 应该有 4 个文件:containerd-nydus-grpc / nydus-overlayfs / optimizer-nri-plugin / optimizer-server

# 只替换这一个
cp -f "${BUILD_DIR}/containerd-nydus-grpc" bin/containerd-nydus-grpc
chmod +x bin/containerd-nydus-grpc

# 按新版本号重新打包
# --no-xattrs: macOS bsdtar 默认会把 com.apple.provenance 等扩展属性写进 PAX header,
# Linux 上的 GNU tar 解压时会报 "Ignoring unknown extended header keyword" 警告(无害,
# 但加这个参数打出来的包对 Linux 更干净)
ARCHIVE="nydus-snapshotter-${NEW_TAG}-linux-amd64.tar.gz"
tar --no-xattrs -C "$BUILD_DIR" -czf "${BUILD_DIR}/${ARCHIVE}" bin

# 生成校验和(macOS 用 shasum,不是 sha256sum)
#
# ⚠️ 必须先 cd 进目录再用裸文件名跑 shasum,不能直接传绝对路径!
# shasum/sha256sum 会把传给它的参数字符串原样写进校验文件第二列。如果传的是
# "${BUILD_DIR}/${ARCHIVE}" 这种绝对路径,生成的 .sha256sum 文件里就会记录这台
# 构建机上的绝对路径。之后不管在哪台机器、哪个目录下用 `sha256sum -c` 校验,
# 它都会去找这个字面路径(cd 到哪都没用),在其它机器上必然是
# "No such file or directory"。entrypoint.sh 的 download_asset() 校验失败时
# 因为 `set -eu` 没有兜底,会导致整个脚本退出、pod crash-loop —— sp3 就是
# 因为这里传了绝对路径栽的跟头。
(
  cd "$BUILD_DIR"
  shasum -a 256 "$ARCHIVE" > "${ARCHIVE}.sha256sum"
)
cat "${BUILD_DIR}/${ARCHIVE}.sha256sum"

# 发布前自查:确认第二列只有裸文件名,不含任何 "/"
if grep -q "/" "${BUILD_DIR}/${ARCHIVE}.sha256sum"; then
  echo "❌ 校验文件里混进了路径,重新生成!" >&2
else
  echo "✅ 校验文件格式正确(裸文件名)"
fi
```

产物:
```
${BUILD_DIR}/nydus-snapshotter-${NEW_TAG}-linux-amd64.tar.gz
${BUILD_DIR}/nydus-snapshotter-${NEW_TAG}-linux-amd64.tar.gz.sha256sum
```

---

## 5. 建 GitHub release,上传附件

### 方式 A:网页手动(最简单,推荐手动执行时用这个)

1. 打开(把 tag 换成 `$NEW_TAG` 实际值):
   ```
   https://github.com/81981266/nydus-snapshotter/releases/new?tag=v0.15.15-sp3
   ```
2. **Choose a tag**:确认选中的是**已存在**的 tag(不是 "Create new tag"),因为第 2 步已经推过了。
3. 标题填,例如:`v0.15.15 + stream_prefetch (sp3)`
4. 把步骤 4 生成的两个文件拖进 **Attach binaries**
5. 点 **Publish release**

### 方式 B:命令行(用缓存的 git 凭证调 GitHub API,免装 gh)

```bash
# 从 macOS Git Credential Manager 里取已缓存的 token(前提:之前 git push 成功过一次)
TOKEN=$(printf 'protocol=https\nhost=github.com\n\n' | git credential fill 2>/dev/null | grep '^password=' | cut -d= -f2-)

# 建 release
RESP=$(curl -sS -X POST \
  -H "Authorization: token ${TOKEN}" \
  -H "Accept: application/vnd.github+json" \
  https://api.github.com/repos/81981266/nydus-snapshotter/releases \
  -d "{\"tag_name\": \"${NEW_TAG}\", \"name\": \"${BASE_TAG} + stream_prefetch (${NEW_TAG##*-})\", \"body\": \"containerd-nydus-grpc rebuilt from commit ${BUILD_COMMIT}. Other binaries unchanged from official ${BASE_TAG} release.\", \"draft\": false, \"prerelease\": false}")
RELEASE_ID=$(echo "$RESP" | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
echo "release id = $RELEASE_ID"

# 上传 tarball
curl -sS -X POST \
  -H "Authorization: token ${TOKEN}" \
  -H "Content-Type: application/gzip" \
  --data-binary @"${BUILD_DIR}/${ARCHIVE}" \
  "https://uploads.github.com/repos/81981266/nydus-snapshotter/releases/${RELEASE_ID}/assets?name=${ARCHIVE}"

# 上传 sha256sum
curl -sS -X POST \
  -H "Authorization: token ${TOKEN}" \
  -H "Content-Type: text/plain" \
  --data-binary @"${BUILD_DIR}/${ARCHIVE}.sha256sum" \
  "https://uploads.github.com/repos/81981266/nydus-snapshotter/releases/${RELEASE_ID}/assets?name=${ARCHIVE}.sha256sum"

unset TOKEN
```

---

## 6. 验证发布结果

```bash
# 下载可用性 + 状态码
curl -sL -o /dev/null -w "http_code=%{http_code} size=%{size_download}\n" \
  "https://github.com/81981266/nydus-snapshotter/releases/download/${NEW_TAG}/${ARCHIVE}"

# 内容一致性(本地 vs 线上 sha256)
LOCAL_SHA=$(shasum -a 256 "${BUILD_DIR}/${ARCHIVE}" | awk '{print $1}')
REMOTE_SHA=$(curl -sL "https://github.com/81981266/nydus-snapshotter/releases/download/${NEW_TAG}/${ARCHIVE}" | shasum -a 256 | awk '{print $1}')
echo "local:  $LOCAL_SHA"
echo "remote: $REMOTE_SHA"
[ "$LOCAL_SHA" = "$REMOTE_SHA" ] && echo "✅ 一致" || echo "❌ 不一致,别用,重新查问题"

# 端到端模拟 entrypoint.sh 里 download_asset() 的校验逻辑,
# 这是唯一能提前发现 ".sha256sum 里混进绝对路径" 这类问题的方法
E2E_DIR="$(mktemp -d)"
curl -fsSL -o "${E2E_DIR}/${ARCHIVE}" \
  "https://github.com/81981266/nydus-snapshotter/releases/download/${NEW_TAG}/${ARCHIVE}"
curl -fsSL -o "${E2E_DIR}/${ARCHIVE}.sha256sum" \
  "https://github.com/81981266/nydus-snapshotter/releases/download/${NEW_TAG}/${ARCHIVE}.sha256sum"
( cd "$E2E_DIR" && shasum -a 256 -c "${ARCHIVE}.sha256sum" ) \
  && echo "✅ pod 校验逻辑会通过" \
  || echo "❌ pod 里会 crash-loop,别发布,先查 .sha256sum 内容"
rm -rf "$E2E_DIR"
```

---

## 7. 更新 pod 启动脚本变量

```bash
NYDUS_SNAPSHOTTER_REPO="81981266/nydus-snapshotter"
NYDUS_SNAPSHOTTER_VERSION="v0.15.15-sp3"   # 换成本次实际的 NEW_TAG
```

`nydus-static`(nydusd 本体)那条**不用改**——补丁只涉及 snapshotter 侧的配置透传。

---

## 8. Pod 起来后确认修复生效

```bash
grep -r stream_prefetch /var/lib/containerd-nydus/config/    # 路径以实际 --root 配置为准
```

看到 `"stream_prefetch": true` 就说明配置已经透传给 nydusd。

---

## 常见坑

- **`git push origin ...` 403**:origin 是上游仓库,没写权限。永远用 `git push fork ...`。
- **`sha256sum: command not found`**:macOS 没有这个命令,用 `shasum -a 256` 代替。
- **Linux 上 `tar xzf` 报 `Ignoring unknown extended header keyword 'LIBARCHIVE.xattr.com.apple.provenance'`**:无害警告,macOS 打包时带的扩展属性,GNU tar 会自动忽略并正常解压。第 4 步打包命令已加 `--no-xattrs` 避免这个警告。
- **`.sha256sum` 里混进了绝对路径,导致 pod crash-loop(sp3 踩过)**:`shasum -a 256 <path>` 会把参数原样写进校验文件第二列。传绝对路径就会生成一个只在你构建机上有效的校验文件,`entrypoint.sh` 的 `download_asset()` 在 pod 里 `sha256sum -c` 时必然报 "No such file or directory",而脚本 `set -eu` 没有兜底,整个容器直接退出。**必须 `cd` 进目录后用裸文件名生成校验文件**(第 4 步已修正),发布前用第 6 步的端到端模拟确认一遍。
- **怎么修一个已经发错的 release 附件**:GitHub 不允许同名覆盖,得先 `DELETE /repos/{owner}/{repo}/releases/assets/{asset_id}` 删掉旧的,再用 `POST https://uploads.github.com/.../assets?name=...` 传新的。哈希值本身不用重算(文件内容没变,只是重写校验文件里的文件名那一列)。
- **`--version` 显示 `Revision: unknown` / `Build time: unknown`**:说明编译时没传 `-X version.Revision=...` / `-X version.BuildTimestamp=...`。第 3 步命令已经补全,如果用的是旧版命令记得更新。
- **linux 二进制在 mac 本地跑不起来(`exec format error`)**:是正常现象,交叉编译产物只能在 Linux/amd64 上执行,`chmod` 无法解决。想在 mac 上验证可以用 `go version -m <bin>` 读嵌入的 VCS 信息,或用 `docker run --platform linux/amd64` 跑。
- **release 页面看不到新建的 tag**:tag 推送和 release 创建是两件事,tag 存在 ≠ release 存在。用 `releases/new?tag=<tag>` 直达,或确认 tag 确实已经 `git push fork` 成功(`git ls-remote --tags fork`)。
- **改动范围**:如果基于分支 HEAD 打包,记得看一下 HEAD 比 `stream_prefetch` 那个 commit 多带了哪些其它改动(`git log <fix-commit>..HEAD --oneline`),必要时在 release notes 里注明,避免以后排查问题时忘记这批" bonus" 修复的存在。
- **PR 状态**:这套自建包只是临时方案。等上游 PR 合并、官方发新版本后,把 `NYDUS_SNAPSHOTTER_REPO` 改回 `containerd/nydus-snapshotter`、`NYDUS_SNAPSHOTTER_VERSION` 改成官方新版本号,自建的 sp 系列就可以停止维护了。
