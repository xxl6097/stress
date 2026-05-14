# stress

[![Go](https://img.shields.io/badge/go-1.25-blue.svg)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-linux-lightgrey.svg)](#系统要求)

让 Linux 整机 CPU / 内存占用率**稳定保持**在你设定的目标值的小工具。

`STRESS_TARGET=0.8` 设到 80%，它就实时观测整机已用 + 自身已用，把整机总量稳在 80% 上下：其它进程吃多了它就让位，其它进程空闲了它就补上。

适合容量评估、调度器与告警阈值验证、HPA / VPA 演练、可观测系统压测等"需要恒定负载、不扰动现网"的场景。

<!-- TOC -->

- [一、它解决什么问题](#一它解决什么问题)
- [二、工作原理](#二工作原理)
- [三、配置](#三配置)
- [四、快速开始](#四快速开始)
- [五、docker-compose.yaml 详解](#五docker-composeyaml-详解)
- [六、多架构镜像构建 & 推送](#六多架构镜像构建--推送)
- [七、系统要求](#七系统要求)
- [八、项目结构](#八项目结构)
- [九、常见问题](#九常见问题)
- [十、License](#十license)

<!-- /TOC -->

---

## 一、它解决什么问题

普通的 stress / stress-ng 给定的是"我占多少"——**目标进程的绝对占用**。系统总占用 = 目标进程 + 其它进程，永远跟着其它进程波动。

当你需要"整机恒定 80% 占用"，传统做法是人肉守 `top` 反复调参。本工具用一个简单闭环替你做：

```
其他进程占用 20%  ──►  本进程补 60%   ──►  整机 80%
其他进程占用 50%  ──►  本进程补 30%   ──►  整机 80%
其他进程占用 90%  ──►  本进程补  0%   ──►  整机 90%（让位，不挤兑）
```

CPU 和内存各自独立按这个逻辑调节。

实测在 28 核 / 111 GiB 的 Ubuntu 22.04 机器上跑 2 小时（3588 个采样）：

| 指标 | 平均 | 标准差 | min | p99 | 漂移（4×30min） |
|---|---|---|---|---|---|
| 整机 CPU `sys` | **79.36%** | 4.85% | 66.2% | 88.2% | +0.004% |
| 整机 MEM `used` | **80.54%** | 0.16% | 80.1% | 80.9% | +0.29% |
| `VmHWM` 增长 | +303 MB（0.4%）/2h | | | | 无内存泄漏 |
| 控制器饱和次数 | 0 | | | | |

退出后 `free` 显示完全归还，72 GiB 占位 100% 还给 OS。

---

## 二、工作原理

### 指标采集

只读 `/proc`，没有第三方库依赖：

| 来源 | 字段 | 作用 |
|---|---|---|
| `/proc/stat` | `cpu` 行 | 全局 CPU 时间累计，做差得 `sysUsage` |
| `/proc/self/stat` | `utime + stime` | 本进程 CPU 累计，做差得 `ownUsage` |
| `/proc/meminfo` | `MemTotal`、`MemAvailable` | 整机内存基准 + 可用 |
| `/proc/self/status` | `VmRSS` | 本进程驻留内存 |

### 控制环（默认每 2s 一次）

```
otherUsage  = sysUsage - ownUsage           # 其他进程占用
desiredOwn  = clamp(target - otherUsage)    # 本进程目标占用 ∈ [0, 1]
```

### CPU 占位

为每个逻辑核起一个 worker，每 100ms 一个占空比周期：

- `desiredOwn` 比例时间忙转（一个简单的 LCG 内循环，避免被编译器优化掉）
- 剩余时间 `time.Sleep`
- `desiredOwn ≤ 0.0005` 时整周期休眠，几乎不占 CPU

`atomic.Uint64` 存浮点 load，控制环每 2s 写一次，所有 worker 无锁读取。

### 内存占位

按 `MemTotal × target − otherUsed` 算目标字节数。**slab 化**分配，每块 64 MiB：

- 增加：只新分配差值，不复制历史；新块按 `runtime.NumCPU()` 并行 page touch，确保**真正占用物理内存**而不是仅占地址空间
- 减少：从尾部丢弃完整 slab，最后一块按差值裁剪，调 `debug.FreeOSMemory()` 归还
- 防 swap：每 15s 触碰所有页一次

为什么要 slab 化？早期版本用 `make + copy + 整体 touch`，调整一次几十 GiB 的内存要 20~50s，把 2s 控制周期拖成 30s。slab 化后每次只动差值，10 分钟稳态标准差从无法收敛降到 0.11%。

### 安全护栏

- 每控制周期内存目标变化量 ≤ `STRESS_MEM_STEP`（默认 20% MemTotal），避免抖动激发雪崩
- 始终保留 3% MemTotal 余量，绝不耗尽 `MemAvailable`，防 OOM
- CPU `desiredOwn` clamp 到 `[0, 1]`
- 收到 `SIGINT` / `SIGTERM`：停止控制环 → `setMemBytes(0)` 归还所有占位 → 等所有 worker 退出

---

## 三、配置

所有配置走环境变量，没有命令行 flag：

| 变量 | 默认 | 类型 | 说明 |
|---|---|---|---|
| `STRESS_TARGET` | `0.8` | float `0~1` | 目标整机总占用比例 |
| `STRESS_INTERVAL` | `2s` | duration | 控制环采样/调节周期 |
| `STRESS_CPU_PERIOD` | `100ms` | duration | CPU worker 占空比周期 |
| `STRESS_MEM_STEP` | `0.20` | float `0~1` | 每周期内存目标最大变化量（占 MemTotal 比例） |
| `STRESS_CPU_ONLY` | `false` | bool | 只调 CPU |
| `STRESS_MEM_ONLY` | `false` | bool | 只调内存 |
| `STRESS_QUIET` | `false` | bool | 关闭周期日志 |

Duration 接受 Go 的 `time.ParseDuration` 格式：`500ms`、`3s`、`1m30s`。
Bool 接受 `true / false / 1 / 0`。

---

## 四、快速开始

### 方式 A：Docker Compose（推荐）

仓库里 `docker-compose.yaml` 已经配好。两步：

```bash
# 1) 准备 .env（覆盖镜像坐标）
cat > .env <<'EOF'
REGISTRY=docker.cnb.cool
REPO=your-org/stress
IMAGE_TAG=v0.1.1
EOF

# 2) 起容器
docker compose pull
docker compose up -d
docker compose logs -f stress
```

调整目标占用：编辑 `docker-compose.yaml` 里的 `environment.STRESS_TARGET`，或临时：

```bash
STRESS_TARGET=0.5 docker compose up -d
```

### 方式 B：直接运行二进制

```bash
# 必须是 Linux（main.go 顶部有 //go:build linux）
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o stress .

STRESS_TARGET=0.8 ./stress
```

启动日志：

```
启动：target=80% cores=8 memTotal=31.2GiB interval=2s
CPU sys= 42.1% own= 35.0% other=  7.1% -> load= 72.9%
MEM used= 41.2% own= 10.3% other= 30.9% -> buf= 49.1% (15.31GiB)
```

字段含义：

| 字段 | 含义 |
|---|---|
| `sys` | 整机 CPU / MEM 当前使用率 |
| `own` | 本进程贡献 |
| `other` | 其他进程占用（= sys − own） |
| `load` | CPU 控制器输出（worker 占空比） |
| `buf` | 内存控制器输出（占位字节占 MemTotal 比例） |

---

## 五、docker-compose.yaml 详解

仓库自带的 `docker-compose.yaml` 是按"压一个**没有 cgroup 限制**的容器、影响整机"这个用途调过的。逐项说明：

```yaml
services:
  stress:
    image: ${REGISTRY:-docker.cnb.cool}/${REPO:-abber/i/stress}:${IMAGE_TAG:-v0.1.1}
    container_name: stress

    pid: host          # /proc/self 用宿主 PID namespace，top/ps 能看到本进程
    network_mode: host # 不需要网络隔离；按需可去掉

    restart: unless-stopped
    stop_grace_period: 5s

    environment:
      STRESS_TARGET: "0.8"
      STRESS_INTERVAL: "2s"
      STRESS_CPU_PERIOD: "100ms"
      STRESS_MEM_STEP: "0.20"

    oom_score_adj: -500           # 内存紧张时内核优先 kill 别的进程
    cap_drop: [ALL]
    cap_add: [SETUID, SETGID, CHOWN]   # entrypoint.sh 要 chown + su-exec
    security_opt:
      - no-new-privileges:true

    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
```

### 关键点

**1. `image` 用三段变量拼出来**

```
${REGISTRY:-docker.cnb.cool}/${REPO:-abber/i/stress}:${IMAGE_TAG:-v0.1.1}
```

把这三个写进 `.env`（compose 自动读）就能切换部署目标，不必改 compose 本身。

**2. 不要给容器加 CPU / MEM 限制**

程序读的 `/proc/stat` / `/proc/meminfo` 在容器里默认还是**宿主视角**（cgroup v1/v2 都不会改写这两个文件）。如果给容器加 `mem_limit: 4g` 而宿主是 32 GiB：

- 程序按 32 GiB × 80% = 25.6 GiB 去申请
- 超过容器的 4g 上限 → 容器立刻被 OOM kill

如果你**真的就想限制本容器的爆炸半径**，把 `mem_limit` 打开，并把 `STRESS_TARGET` 同步调小到对应比例（例如 `mem_limit: 4g` + `STRESS_TARGET=0.1` 大约是 4 GiB 占用上限）。

**3. `pid: host`**

让本进程的 `/proc/self/stat` 看到的 PID 和宿主一致：

- `htop` / `ps` / 监控里看到的就是同一个 PID
- 不会拿到容器内 PID=1 的伪状态

如果安全策略不允许 `pid: host`，去掉这一行也能跑——只是 `top` 里看到的 PID 是容器 namespace 内的。

**4. `oom_score_adj: -500`**

这是个故意吃满整机的程序，内存紧张时**别先杀它**，否则资源还得太快、压力曲线就破了。要让它在 OOM 时优先被杀，改成 `1000`。

**5. `cap_add: [SETUID, SETGID, CHOWN]`**

仓库的 `entrypoint.sh` 用 `su-exec` 切换到 `${PUID}:${PGID}` 跑主程序，需要 SETUID/SETGID/CHOWN。`cap_drop: ALL` 之后只补这三个最小必需 cap。

如果你不需要切换用户、直接以 root 跑，可以把 entrypoint 换成 `["/app/main"]`，cap_add 全删。

**6. 日志限额**

控制环每 2s 一行，一天约 43k 行。`json-file` 不限会一直涨。`max-size: 10m × max-file: 3` 上限 30 MB，足够长跑。

### 部署到目标机

```bash
# 1) 在目标机上准备目录
ssh user@your.server.tld 'mkdir -p /opt/stress'

# 2) 上传 compose 和 .env
scp docker-compose.yaml user@your.server.tld:/opt/stress/
ssh user@your.server.tld 'cat > /opt/stress/.env' <<'EOF'
REGISTRY=docker.cnb.cool
REPO=your-org/stress
IMAGE_TAG=v0.1.1
EOF

# 3) 登录私有 registry（密码用 PAT，不是 web 密码）
ssh user@your.server.tld 'docker login docker.cnb.cool -u <CNB_USER>'

# 4) 拉镜像、起容器
ssh user@your.server.tld 'cd /opt/stress && docker compose pull && docker compose up -d'

# 5) 看日志
ssh user@your.server.tld 'cd /opt/stress && docker compose logs -f stress'
```

### 停止 & 卸载

```bash
# 停容器，保留镜像和 compose 文件
docker compose down

# 停容器并删本地镜像（彻底卸载）
docker compose down --rmi all

# 删除部署目录
rm -rf /opt/stress
```

---

## 六、多架构镜像构建 & 推送

`scripts/build.sh` 一键完成：

1. host 上用本地 Go 工具链交叉编译出 `dist/stress-linux-amd64`、`dist/stress-linux-arm64`
2. 启动 / 复用名为 `stress-builder` 的 buildx 构建器
3. `docker buildx build --platform linux/amd64,linux/arm64 --push` 一次性生成 manifest list 推送

为什么先在 host 编译再 `COPY` 进镜像？因为 `Dockerfile` 走的是 `COPY dist/${REPO_NAME}-${TARGETOS}-${TARGETARCH}` 的预编译产物模式，buildx 不需要在每个目标平台下用 QEMU 跑 go 编译器，**速度比镜像内 build 快很多**。代价是 host 必须装 Go 1.25。

### 首次准备

```bash
# 登录 CNB（密码填 CNB Personal Access Token）
docker login docker.cnb.cool -u <CNB_USER>

# 给 docker 装上多架构支持（一次即可）
docker run --privileged --rm tonistiigi/binfmt --install all
```

### 构建 + 推送

```bash
# 用脚本默认值
./scripts/build.sh

# 自定义仓库路径和 tag
REPO=your-org/stress IMAGE_TAG=v0.1.1 ./scripts/build.sh

# 只构建不推送（只能单平台 + 本地 --load）
PUSH=0 PLATFORMS=linux/arm64 ./scripts/build.sh
```

### 脚本参数

| 变量 | 默认 | 说明 |
|---|---|---|
| `REGISTRY` | `docker.cnb.cool` | 镜像仓库主机 |
| `REPO` | `abber/i/stress` | CNB 上的仓库路径 `<org>/<repo>` |
| `IMAGE_TAG` | `latest` | 镜像 tag |
| `PLATFORMS` | `linux/amd64,linux/arm64` | 目标平台，逗号分隔 |
| `PUSH` | `1` | `1`=推 CNB，`0`=本地 `--load`（仅单平台） |
| `REPO_NAME` | `stress` | `dist/` 下产物前缀，与 Dockerfile 对齐 |
| `BUILDER` | `stress-builder` | buildx builder 名称 |

### 验证 manifest

```bash
docker buildx imagetools inspect docker.cnb.cool/your-org/stress:v0.1.1
```

期望看到 `linux/amd64` + `linux/arm64` 两条 manifest 条目。不同架构的目标机 `docker compose pull` 时会自动选对应那条。

> Dockerfile 当前 base 是 CNB 私有镜像 `docker.cnb.cool/clife/golang/dokcer/alpine:latest`，外部贡献者无法直接 build。要在公开环境构建，把 `FROM` 改成 `alpine:3.20` 这类公开镜像即可。

---

## 七、系统要求

- **OS**：Linux 内核（macOS / Windows 无法运行；`main.go` 顶部 `//go:build linux`）
- **Go**：编译需要 1.25+；运行只需要 Linux 任意内核
- **容器内运行**：宿主机或运行时**不能屏蔽** `/proc/stat` / `/proc/meminfo`。LXCFS、gVisor 这类沙箱会改写 `/proc/meminfo`，让程序拿到容器视角而不是宿主视角；这种环境下要么去掉沙箱，要么打开 `mem_limit` + 调小 `STRESS_TARGET`
- **架构**：amd64 / arm64 都已验证

---

## 八、项目结构

```
.
├── main.go               # 程序主体（控制环 + CPU/MEM 占位）
├── entrypoint.sh         # chown + su-exec 套壳启动
├── Dockerfile            # 按架构 COPY 预编译好的二进制
├── docker-compose.yaml   # 部署编排（pid:host / oom_score_adj / cap_add）
├── scripts/
│   └── build.sh          # 交叉编译 + buildx 多架构推送
├── go.mod
├── .dockerignore
├── .gitignore
└── README.md
```

`dist/` 不入仓库，由 `scripts/build.sh` 生成。

---

## 九、常见问题

**Q: 启动后整机占用没到目标值？**

看日志里的 `load` 和 `buf` 字段——如果一直在涨，说明控制环在追但还没追上。`STRESS_MEM_STEP=0.20` 时内存大约 8s 收敛到 80%；旧默认 `0.05` 大约要 60s。如果已经打满还不够：

- CPU 达不到 100%：worker 数 < 核数，一般不会出现（默认 `runtime.NumCPU()`）
- 内存达不到目标：`MemAvailable` 不够（保留了 3% MemTotal 余量不吃）

**Q: 在容器里看到的 `memTotal` 比宿主小？**

确认没设 `mem_limit`。cgroup 本身不会改写 `/proc/meminfo`，但 LXCFS、gVisor 会。

**Q: 想立刻把资源还回去？**

发 `SIGTERM`（`docker compose down` 默认就是这个）。程序会调 `setMemBytes(0)` + `FreeOSMemory`，CPU worker 立刻停。`stop_grace_period: 5s` 一般够。

**Q: 监控曲线？**

本程序只负责控制不做监控。建议搭配 node_exporter + Prometheus 在宿主上采。

**Q: 为什么稳态 CPU 标准差比 MEM 大很多？**

CPU 是快变量，`other` 进程的亚秒级抖动直接传到 `sys`，2s 采样追不上瞬态。MEM 是慢变量，`/proc/meminfo` 读到的 `used` 是精确数字，调一次就稳。两者特性不同，不是控制环 bug。要把 CPU 标准差压下去得用更短的 `STRESS_INTERVAL`（如 `500ms`）+ EMA 滤波，但收益有限。

**Q: 怎么验证收敛精度？**

跑半小时，提取日志 `CPU sys=` 和 `MEM used=`，对**第二个半小时**的样本算均值。本工具实测在 28 核 / 111 GiB 机器上 2 小时跑：

- CPU sys avg = 79.36%（target 80%，偏差 -0.64%）
- MEM used avg = 80.54%（偏差 +0.54%）
- 4×30min 分段 CPU 漂移 +0.004%

---

## 十、License

MIT

