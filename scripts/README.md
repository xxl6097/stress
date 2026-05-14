以下是一个可调节 CPU 和内存消耗比例的 Shell 脚本。它通过启动受控的忙等进程和内存占用来实现资源消耗，并提供了详细的命令行参数。

```bash
#!/usr/bin/env bash
#===============================================================================
# 资源消耗脚本 - 可调节 CPU 和内存的占用比例
# 用法: ./consume.sh [选项]
# 选项:
#   -c, --cpu CORES      使用的 CPU 核心数（支持小数，如 1.5 表示 1 个满核 + 一个 50% 的核）
#   -m, --memory SIZE    占用的内存大小（单位 MB）
#   -t, --timeout SEC    持续时间（秒），默认无限运行，直到手动终止
#   -h, --help           显示帮助信息
# 示例:
#   ./consume.sh -c 2 -m 1024 -t 60    # 占用 2 个完整核心和 1GB 内存，持续 60 秒
#   ./consume.sh -c 0.5 -m 256         # 占用半个核心和 256MB 内存，直到手动停止
#===============================================================================

set -euo pipefail

# ------------------------------ 默认值 ---------------------------------------
CPU_CORES=0
MEMORY_MB=0
TIMEOUT=0

# ------------------------------ 工具函数 ------------------------------------
usage() {
    cat <<EOF
用法: $0 [选项]

选项:
  -c, --cpu CORES      使用的 CPU 核心数（支持小数，例如 1.5）
  -m, --memory SIZE    占用的内存大小（单位 MB）
  -t, --timeout SEC    持续时间（秒），默认无限
  -h, --help           显示此帮助信息

示例:
  $0 -c 2 -m 1024 -t 60
  $0 -c 0.5 -m 256
EOF
    exit 0
}

cleanup() {
    echo "正在终止所有子进程..."
    # 杀掉本脚本的所有子进程
    pkill -P $$ 2>/dev/null || true
    exit 0
}
trap cleanup INT TERM EXIT

# ------------------------------ 参数解析 ------------------------------------
ARGS=$(getopt -o c:m:t:h --long cpu:,memory:,timeout:,help -n "$0" -- "$@")
if [[ $? -ne 0 ]]; then
    usage
fi
eval set -- "$ARGS"

while true; do
    case "$1" in
        -c|--cpu) CPU_CORES="$2"; shift 2 ;;
        -m|--memory) MEMORY_MB="$2"; shift 2 ;;
        -t|--timeout) TIMEOUT="$2"; shift 2 ;;
        -h|--help) usage; shift ;;
        --) shift; break ;;
        *) echo "未知选项: $1"; usage ;;
    esac
done

# 至少指定一种资源消耗
if [[ $(echo "$CPU_CORES == 0 && $MEMORY_MB == 0" | bc) -eq 1 ]]; then
    echo "错误: 请至少通过 -c 或 -m 指定一种资源消耗"
    usage
fi

# ------------------------------ CPU 消耗函数 --------------------------------
# 消耗单个 CPU 核心，可指定负载比例 (0-100)
# 参数: $1 = 负载百分比 (0-100)
consume_cpu_with_load() {
    local load_percent=$1
    if [[ $load_percent -le 0 ]]; then return; fi
    if [[ $load_percent -ge 100 ]]; then
        # 满负载：无限循环
        python3 -c "while True: pass" 2>/dev/null ||
        perl -e 'while(1){}' 2>/dev/null ||
        while :; do :; done
    else
        # 部分负载：按时间片忙等和休眠，优先用 python 获取较高精度
        python3 -c "
import time
load = $load_percent / 100.0
while True:
    start = time.time()
    # 忙等
    while time.time() - start < load * 0.1:
        pass
    # 休眠
    time.sleep((1 - load) * 0.1)
" 2>/dev/null ||
        # 回退到 bash 的粗略控制（每秒循环调节，精度较低）
        while true; do
            # 忙等一段时间，通过循环次数大致控制
            # 假设 100000 次空循环约消耗 0.1 秒（机器相关，可按需调整）
            for ((i=0; i<$((load_percent * 1000)); i++)); do :; done
            sleep 0.$((100 - load_percent))
        done
    fi
}

# 根据小数核心数启动进程
start_cpu_consumers() {
    local cores=$1
    local full_cores fractional fractional_percent

    # 分离整数和小数部分
    full_cores=$(echo "$cores" | awk '{print int($1)}')
    fractional=$(echo "$cores - $full_cores" | bc)
    fractional_percent=$(echo "$fractional * 100" | bc | awk '{print int($1+0.5)}')

    echo "启动 $full_cores 个满核心 CPU 消费者..."
    for ((i=0; i<full_cores; i++)); do
        consume_cpu_with_load 100 &
    done

    if [[ $fractional_percent -gt 0 ]]; then
        echo "启动 1 个 ${fractional_percent}% 负载的 CPU 消费者..."
        consume_cpu_with_load "$fractional_percent" &
    fi
}

# ------------------------------ 内存消耗函数 --------------------------------
start_memory_consumer() {
    local size_mb=$1
    echo "分配 ${size_mb}MB 内存..."

    python3 -c "
import time
# 分配指定大小的字节数组（每个元素 1 字节）
data = bytearray($size_mb * 1024 * 1024)
# 每隔一段时间访问一下防止被换出（实际保持存在即可）
while True:
    time.sleep(60)
    # 轻量触碰保持活跃
    data[0] = 0
" 2>/dev/null &
    
    if [[ $? -ne 0 ]]; then
        # 无 Python，尝试使用 bash 分配大变量（较慢，慎用较大内存）
        echo "警告: 未找到 Python，使用 Bash 分配内存（可能较慢且占用 CPU）"
        (
            # 读取指定大小的零字节流，并转化为可存储的字符以占用内存
            # 此处使用 tr 将 '\0' 转为 'x'，实际占用的内存大小约为 size_mb MB
            mem_data=$(head -c "${size_mb}M" /dev/zero | tr '\0' 'x')
            while true; do
                sleep 60
                # 防止变量被优化掉
                : "${mem_data:0:1}"
            done
        ) &
    fi
}

# ------------------------------ 主逻辑 --------------------------------------
echo "资源消耗脚本启动："
echo "  - CPU 核心数: $CPU_CORES"
echo "  - 内存: ${MEMORY_MB}MB"
echo "  - 持续时间: ${TIMEOUT}秒 (0 表示无限)"

# 启动内存消耗
if [[ $MEMORY_MB -gt 0 ]]; then
    start_memory_consumer "$MEMORY_MB"
fi

# 启动 CPU 消耗
if [[ $(echo "$CPU_CORES > 0" | bc) -eq 1 ]]; then
    start_cpu_consumers "$CPU_CORES"
fi

# 处理运行时长
if [[ $TIMEOUT -gt 0 ]]; then
    echo "将在 $TIMEOUT 秒后自动终止..."
    sleep "$TIMEOUT"
    echo "时间到，正在退出..."
    cleanup
else
    echo "正在消耗资源，按 Ctrl+C 终止..."
    # 无限等待，直到收到信号
    while true; do
        sleep 60 &
        wait $!
    done
fi
```

## 设计说明

1. **CPU 消耗**
    - 通过 `-c` 指定占用的核心数，支持小数（例如 `1.5` 表示 1 个核心跑满，另一个核心跑 50%）。
    - 满核心负载使用 `python3 -c "while True: pass"` 或 `perl -e 'while(1){}'` 实现，若都不可用则退化为 Shell 死循环。
    - 部分负载采用 **时间片忙等 + 休眠** 的策略，在 0.1 秒的周期内忙等固定比例的时间，剩余时间休眠。Python 版本可提供较精确的控制，Bash 回退版本通过空循环次数模拟，精度较低但无需额外依赖。

2. **内存消耗**
    - 使用 `-m` 指定占用内存的 MB 数。
    - 优先使用 Python 分配 `bytearray` 并无限期保持，进程持续占用物理内存。
    - 如果没有 Python，则通过读取 `/dev/zero` 并将 `\0` 替换为可见字符存入 Shell 变量，以近似占用等量内存（**注意**：大量分配时速度慢且可能受 Shell 限制，实际环境建议安装 Python）。

3. **超时与清理**
    - 通过 `-t` 设置运行秒数，超时后自动终止所有子进程。
    - 脚本捕获 `INT`、`TERM` 和 `EXIT` 信号，确保退出时清理所有由它启动的后台进程。

4. **使用示例**
   ```bash
   # 占用 1.5 核 CPU 和 512MB 内存，持续 30 秒
   chmod +x consume.sh
   ./consume.sh -c 1.5 -m 512 -t 30

   # 仅占用 2GB 内存，直到手动终止
   ./consume.sh -m 2048
   ```

你可以根据实际环境中的可用工具（如系统是否装有 Python）灵活调节脚本，也可替换成更精确的工具（如 `stress-ng`），但此脚本保持了纯 Shell + 常见解释器的最大兼容性。