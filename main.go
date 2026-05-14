//go:build linux

package main

import (
	"bufio"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// 所有配置通过环境变量读取。
//   STRESS_TARGET      目标系统总占用比例 (0-1)                默认 0.8
//   STRESS_INTERVAL    控制环采样/调节周期  (Go Duration)       默认 2s
//   STRESS_CPU_PERIOD  CPU worker 占空比周期 (Go Duration)      默认 100ms
//   STRESS_MEM_STEP    每周期内存目标最大变化量 (MemTotal 比例) 默认 0.05
//   STRESS_CPU_ONLY    仅调节 CPU   (true/false)               默认 false
//   STRESS_MEM_ONLY    仅调节内存   (true/false)               默认 false
//   STRESS_QUIET       关闭日志     (true/false)               默认 false

type config struct {
	Target   float64
	Interval time.Duration
	Period   time.Duration
	MemStep  float64
	CPUOnly  bool
	MemOnly  bool
	Quiet    bool
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	s := envStr(key, "")
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		log.Fatalf("环境变量 %s=%q 解析失败: %v", key, s, err)
	}
	return v
}

func envDuration(key string, def time.Duration) time.Duration {
	s := envStr(key, "")
	if s == "" {
		return def
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		log.Fatalf("环境变量 %s=%q 解析失败: %v", key, s, err)
	}
	return v
}

func envBool(key string, def bool) bool {
	s := envStr(key, "")
	if s == "" {
		return def
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		log.Fatalf("环境变量 %s=%q 解析失败: %v", key, s, err)
	}
	return v
}

func loadConfig() config {
	return config{
		Target:   envFloat("STRESS_TARGET", 0.8),
		Interval: envDuration("STRESS_INTERVAL", 2*time.Second),
		Period:   envDuration("STRESS_CPU_PERIOD", 100*time.Millisecond),
		MemStep:  envFloat("STRESS_MEM_STEP", 0.20),
		CPUOnly:  envBool("STRESS_CPU_ONLY", false),
		MemOnly:  envBool("STRESS_MEM_ONLY", false),
		Quiet:    envBool("STRESS_QUIET", false),
	}
}

var cfg config

// ---- CPU 指标 ----

type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func (c cpuTimes) total() uint64  { return c.user + c.nice + c.system + c.idle + c.iowait + c.irq + c.softirq + c.steal }
func (c cpuTimes) active() uint64 { return c.total() - c.idle - c.iowait }

func readCPUStat() (cpuTimes, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fs := strings.Fields(line)
		var t cpuTimes
		dst := []*uint64{&t.user, &t.nice, &t.system, &t.idle, &t.iowait, &t.irq, &t.softirq, &t.steal}
		for i, p := range dst {
			if i+1 >= len(fs) {
				break
			}
			*p, _ = strconv.ParseUint(fs[i+1], 10, 64)
		}
		return t, nil
	}
	return cpuTimes{}, fmt.Errorf("未找到 cpu 汇总行")
}

func readOwnCPU() (uint64, error) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, err
	}
	s := string(data)
	rp := strings.LastIndex(s, ")")
	if rp < 0 {
		return 0, fmt.Errorf("/proc/self/stat 格式异常")
	}
	fs := strings.Fields(s[rp+1:])
	if len(fs) < 13 {
		return 0, fmt.Errorf("/proc/self/stat 字段不足")
	}
	u, _ := strconv.ParseUint(fs[11], 10, 64)
	k, _ := strconv.ParseUint(fs[12], 10, 64)
	return u + k, nil
}

// ---- 内存指标 ----

func readMemInfo() (total, available uint64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 2 {
			continue
		}
		switch fs[0] {
		case "MemTotal:":
			total, _ = strconv.ParseUint(fs[1], 10, 64)
		case "MemAvailable:":
			available, _ = strconv.ParseUint(fs[1], 10, 64)
		}
	}
	return total * 1024, available * 1024, nil
}

func readOwnRSS() (uint64, error) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fs := strings.Fields(line)
		if len(fs) < 2 {
			return 0, fmt.Errorf("VmRSS 字段异常")
		}
		n, _ := strconv.ParseUint(fs[1], 10, 64)
		return n * 1024, nil
	}
	return 0, fmt.Errorf("未找到 VmRSS")
}

// ---- CPU 占位 ----

// 每个 worker 对应一个逻辑核，负载 0..1 表示该核的占用率。
// N 个 worker 全部以 load=x 运行 => 系统总 CPU 贡献 ≈ x。
var cpuLoadBits atomic.Uint64

func setCPULoad(v float64) {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	cpuLoadBits.Store(math.Float64bits(v))
}

func getCPULoad() float64 {
	return math.Float64frombits(cpuLoadBits.Load())
}

func cpuWorker(stop <-chan struct{}) {
	period := cfg.Period
	for {
		select {
		case <-stop:
			return
		default:
		}
		load := getCPULoad()
		if load <= 0.0005 {
			// 目标为 0：完全让出 CPU
			select {
			case <-stop:
				return
			case <-time.After(period):
			}
			continue
		}
		busy := time.Duration(float64(period) * load)
		idle := period - busy
		if busy > 0 {
			start := time.Now()
			x := uint64(0)
			for time.Since(start) < busy {
				for i := 0; i < 2048; i++ {
					x = x*1103515245 + 12345
				}
				_ = x
			}
		}
		if idle > 0 {
			time.Sleep(idle)
		}
	}
}

// ---- 内存占位 ----

// 切片式占位：每次调整只分配/释放"差值"块，不复制历史数据，
// 避免 O(N) 的 make+copy 把控制环拖垮。
var (
	memMu    sync.Mutex
	memSlabs [][]byte
	memBytes uint64
)

const (
	pageSize = 4096
	slabSize = 64 << 20 // 64MiB/块，平衡 GC 开销和调整粒度
)

func touchParallel(b []byte) {
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	chunk := (len(b) + n - 1) / n
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		start := i * chunk
		if start >= len(b) {
			break
		}
		end := start + chunk
		if end > len(b) {
			end = len(b)
		}
		wg.Add(1)
		go func(s, e int) {
			defer wg.Done()
			for j := s; j < e; j += pageSize {
				b[j] = byte(j)
			}
		}(start, end)
	}
	wg.Wait()
}

func setMemBytes(target uint64) {
	memMu.Lock()
	defer memMu.Unlock()
	if target == memBytes {
		return
	}
	if target > memBytes {
		need := target - memBytes
		// 按 slabSize 切块分配，最后一块按差值分配
		for need > 0 {
			sz := uint64(slabSize)
			if need < sz {
				sz = need
			}
			b := make([]byte, sz)
			touchParallel(b)
			memSlabs = append(memSlabs, b)
			memBytes += sz
			need -= sz
		}
		return
	}
	// 收缩：从尾部丢弃完整 slab，最后一块按差值裁剪
	for memBytes > target && len(memSlabs) > 0 {
		last := memSlabs[len(memSlabs)-1]
		lastLen := uint64(len(last))
		if memBytes-lastLen >= target {
			memSlabs = memSlabs[:len(memSlabs)-1]
			memBytes -= lastLen
		} else {
			keep := lastLen - (memBytes - target)
			memSlabs[len(memSlabs)-1] = last[:keep]
			memBytes = target
		}
	}
	runtime.GC()
	debug.FreeOSMemory()
}

func memRefresher(stop <-chan struct{}) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			memMu.Lock()
			for _, b := range memSlabs {
				for i := 0; i < len(b); i += pageSize {
					b[i]++
				}
			}
			memMu.Unlock()
		}
	}
}

// ---- 控制环 ----

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func main() {
	cfg = loadConfig()
	if cfg.Target < 0 || cfg.Target > 1 {
		log.Fatalf("STRESS_TARGET 必须在 0~1 之间: %v", cfg.Target)
	}
	if cfg.CPUOnly && cfg.MemOnly {
		log.Fatalf("STRESS_CPU_ONLY 和 STRESS_MEM_ONLY 不能同时为 true")
	}

	runtime.GOMAXPROCS(runtime.NumCPU())
	nCores := runtime.NumCPU()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	if !cfg.MemOnly {
		for i := 0; i < nCores; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); cpuWorker(stop) }()
		}
	}
	if !cfg.CPUOnly {
		wg.Add(1)
		go func() { defer wg.Done(); memRefresher(stop) }()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	prevStat, err := readCPUStat()
	if err != nil {
		log.Fatalf("读取 /proc/stat: %v", err)
	}
	prevOwn, err := readOwnCPU()
	if err != nil {
		log.Fatalf("读取 /proc/self/stat: %v", err)
	}
	memTotal, _, err := readMemInfo()
	if err != nil {
		log.Fatalf("读取 /proc/meminfo: %v", err)
	}

	if !cfg.Quiet {
		log.Printf("启动：target=%.0f%% cores=%d memTotal=%.1fGiB interval=%s",
			cfg.Target*100, nCores, float64(memTotal)/(1<<30), cfg.Interval)
	}

	tick := time.NewTicker(cfg.Interval)
	defer tick.Stop()

loop:
	for {
		select {
		case <-sig:
			break loop
		case <-tick.C:
		}

		// --- CPU 调节 ---
		if !cfg.MemOnly {
			curStat, err := readCPUStat()
			if err != nil {
				log.Printf("cpu stat: %v", err)
				continue
			}
			curOwn, err := readOwnCPU()
			if err != nil {
				log.Printf("own cpu: %v", err)
				continue
			}
			totalD := float64(curStat.total() - prevStat.total())
			activeD := float64(curStat.active() - prevStat.active())
			ownD := float64(curOwn - prevOwn)
			prevStat, prevOwn = curStat, curOwn

			if totalD > 0 {
				sysUsage := activeD / totalD
				ownUsage := ownD / totalD
				otherUsage := clamp(sysUsage-ownUsage, 0, 1)
				desired := clamp(cfg.Target-otherUsage, 0, 1)
				setCPULoad(desired)

				if !cfg.Quiet {
					log.Printf("CPU sys=%5.1f%% own=%5.1f%% other=%5.1f%% -> load=%5.1f%%",
						sysUsage*100, ownUsage*100, otherUsage*100, desired*100)
				}
			}
		}

		// --- 内存调节 ---
		if !cfg.CPUOnly {
			_, memAvail, err := readMemInfo()
			if err != nil {
				log.Printf("meminfo: %v", err)
				continue
			}
			ownRSS, err := readOwnRSS()
			if err != nil {
				log.Printf("own rss: %v", err)
				continue
			}
			used := int64(memTotal) - int64(memAvail)
			otherUsed := used - int64(ownRSS)
			if otherUsed < 0 {
				otherUsed = 0
			}
			tgtTotal := int64(float64(memTotal) * cfg.Target)
			desiredOwn := tgtTotal - otherUsed
			if desiredOwn < 0 {
				desiredOwn = 0
			}

			memMu.Lock()
			curBuf := int64(memBytes)
			memMu.Unlock()
			maxStep := int64(float64(memTotal) * cfg.MemStep)
			diff := desiredOwn - curBuf
			if diff > maxStep {
				diff = maxStep
			} else if diff < -maxStep {
				diff = -maxStep
			}
			newBuf := curBuf + diff
			if newBuf < 0 {
				newBuf = 0
			}
			// 不要让目标 > MemAvailable，避免触发 OOM
			safeCap := int64(memAvail) + curBuf - int64(float64(memTotal)*0.03)
			if newBuf > safeCap {
				newBuf = safeCap
			}
			if newBuf < 0 {
				newBuf = 0
			}
			setMemBytes(uint64(newBuf))

			if !cfg.Quiet {
				log.Printf("MEM used=%5.1f%% own=%5.1f%% other=%5.1f%% -> buf=%5.1f%% (%.2fGiB)",
					float64(used)/float64(memTotal)*100,
					float64(ownRSS)/float64(memTotal)*100,
					float64(otherUsed)/float64(memTotal)*100,
					float64(newBuf)/float64(memTotal)*100,
					float64(newBuf)/(1<<30))
			}
		}
	}

	if !cfg.Quiet {
		log.Printf("收到退出信号，清理…")
	}
	close(stop)
	setMemBytes(0)
	wg.Wait()
}
