//go:build linux

package main

import (
	"bufio"
	cryptorand "crypto/rand"
	"fmt"
	"log"
	"math"
	"math/big"
	"os"
	"os/exec"
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

// 三种角色（由 STRESS_ROLE 区分）：
//   ""    master 进程：控制环 + 监控所有 worker，命令通过 unix socketpair 推送
//   "cpu" CPU worker：单核占空比循环，从 fd 3 读目标 load
//   "mem" MEM worker：内存占位 + 释放，从 fd 3 读目标 bytes
//
// 进程结构：
//   stress (master)
//   ├── <随机业务名>     每个对应一个逻辑核
//   └── <随机业务名>     内存占位
//
// 命名机制：父进程通过 exec.Cmd.Args[0] 设 argv[0]，
// 子进程启动后写 /proc/self/comm 同步 comm，ps/top 两个视图都一致。
//
// 进程名从 workerNamePool 里随机抽取（每个 worker spawn 时独立 crypto/rand 选）。
// 想改名只改这个数组即可。注意：
//   - /proc/self/comm 上限 15 字符，超出会被截断
//   - 不要使用方括号包裹的内核线程名（如 [rcu_sched]、[kworker/...]），
//     这是冒充内核线程，运维和监控会误判
var workerNamePool = []string{
	"stressd-cpu",
	"stressd-mem",
	"stressd-io",
	"loadgen",
	"loadgen-cpu",
	"loadgen-mem",
	"probe-cpu",
	"probe-mem",
	"bench-runner",
	"capacity-test",
	"sysbench-go",
	"perf-worker",
	"throttle-cpu",
	"throttle-mem",
}

// pickWorkerName 用 crypto/rand 从池子里随机选一个名字。
// /proc/self/comm 上限 15 字节，超长会被 setComm 截断。
func pickWorkerName() string {
	if len(workerNamePool) == 0 {
		return "stressd"
	}
	n, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(len(workerNamePool))))
	if err != nil {
		// 极少见的熵源失败，回落到时间戳取模
		return workerNamePool[int(time.Now().UnixNano())%len(workerNamePool)]
	}
	return workerNamePool[n.Int64()]
}

// ---- 配置 ----

const (
	roleEnv    = "STRESS_ROLE"
	roleIdxEnv = "STRESS_ROLE_IDX"
	roleNameEnv = "STRESS_ROLE_NAME" // master 把选中的进程名通过环境变量传给 worker
	roleCPU    = "cpu"
	roleMEM    = "mem"
	workerFD   = 3 // master 通过 ExtraFiles 把 socketpair 的一端给 worker 当作 fd 3
)

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

// 改写本进程 comm（/proc/self/comm 上限 15 字符 + NUL）
func setComm(name string) {
	if len(name) > 15 {
		name = name[:15]
	}
	_ = os.WriteFile("/proc/self/comm", []byte(name), 0644)
}

// ---- /proc 读取 ----

type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func (c cpuTimes) total() uint64 {
	return c.user + c.nice + c.system + c.idle + c.iowait + c.irq + c.softirq + c.steal
}
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

// 把 PID 的 utime+stime 累加（/proc/<pid>/stat），用于 master 汇总所有 worker 的 own CPU
func readPidCPU(pid int) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(data)
	rp := strings.LastIndex(s, ")")
	if rp < 0 {
		return 0
	}
	fs := strings.Fields(s[rp+1:])
	if len(fs) < 13 {
		return 0
	}
	u, _ := strconv.ParseUint(fs[11], 10, 64)
	k, _ := strconv.ParseUint(fs[12], 10, 64)
	return u + k
}

func readPidRSS(pid int) uint64 {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
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
			return 0
		}
		n, _ := strconv.ParseUint(fs[1], 10, 64)
		return n * 1024
	}
	return 0
}

// ---- CPU worker ----

func cpuWorkerLoop(loadBits *atomic.Uint64, period time.Duration, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		load := math.Float64frombits(loadBits.Load())
		if load <= 0.0005 {
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

// CPU worker 入口：从 fd 3 读 float64 load，写 atomic
func runCPUWorker() {
	idx, _ := strconv.Atoi(envStr(roleIdxEnv, "0"))
	name := envStr(roleNameEnv, "")
	if name == "" {
		name = pickWorkerName()
	}
	setComm(name)
	_ = idx // idx 只用于 master 端日志/管理，子进程内当前不使用

	cfg = loadConfig()
	runtime.GOMAXPROCS(1)

	var loadBits atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); cpuWorkerLoop(&loadBits, cfg.Period, stop) }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	pipe := os.NewFile(uintptr(workerFD), "ctrl")
	if pipe == nil {
		log.Fatalf("worker: fd %d not present", workerFD)
	}
	defer pipe.Close()

	buf := make([]byte, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := pipeReadFull(pipe, buf); err != nil {
				return
			}
			bits := uint64(buf[0]) | uint64(buf[1])<<8 | uint64(buf[2])<<16 | uint64(buf[3])<<24 |
				uint64(buf[4])<<32 | uint64(buf[5])<<40 | uint64(buf[6])<<48 | uint64(buf[7])<<56
			loadBits.Store(bits)
		}
	}()

	select {
	case <-sig:
	case <-done:
	}
	close(stop)
	wg.Wait()
}

// MEM worker 入口：从 fd 3 读 uint64 target_bytes，slab 化分配/释放
func runMEMWorker() {
	name := envStr(roleNameEnv, "")
	if name == "" {
		name = pickWorkerName()
	}
	setComm(name)
	cfg = loadConfig()

	pipe := os.NewFile(uintptr(workerFD), "ctrl")
	if pipe == nil {
		log.Fatalf("mem worker: fd %d not present", workerFD)
	}
	defer pipe.Close()

	stop := make(chan struct{})
	go memRefresher(stop)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	buf := make([]byte, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := pipeReadFull(pipe, buf); err != nil {
				return
			}
			tgt := uint64(buf[0]) | uint64(buf[1])<<8 | uint64(buf[2])<<16 | uint64(buf[3])<<24 |
				uint64(buf[4])<<32 | uint64(buf[5])<<40 | uint64(buf[6])<<48 | uint64(buf[7])<<56
			setMemBytes(tgt)
		}
	}()

	select {
	case <-sig:
	case <-done:
	}
	close(stop)
	setMemBytes(0)
}

func pipeReadFull(f *os.File, buf []byte) (int, error) {
	off := 0
	for off < len(buf) {
		n, err := f.Read(buf[off:])
		if err != nil {
			return off + n, err
		}
		off += n
	}
	return off, nil
}

// ---- 内存占位（slab 化，沿用主分支实现） ----

var (
	memMu    sync.Mutex
	memSlabs [][]byte
	memBytes uint64
)

const (
	pageSize = 4096
	slabSize = 64 << 20
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

// ---- master ----

type cpuChild struct {
	idx  int
	pid  int
	ctrl *os.File // master 端写 fd
}

type memChild struct {
	pid  int
	ctrl *os.File
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func packU64(v uint64) []byte {
	var b [8]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
	return b[:]
}

func spawnCPU(idx int) *cpuChild {
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		log.Fatalf("socketpair: %v", err)
	}
	parent := os.NewFile(uintptr(pair[0]), "ctrl-parent")
	child := os.NewFile(uintptr(pair[1]), "ctrl-child")

	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("os.Executable: %v", err)
	}

	name := pickWorkerName()
	cmd := exec.Command(exe)
	cmd.Args = []string{name} // 让 argv[0] = 池子里随机选的名字
	cmd.Env = append(os.Environ(),
		roleEnv+"="+roleCPU,
		fmt.Sprintf("%s=%d", roleIdxEnv, idx),
		roleNameEnv+"="+name,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{child} // 子进程的 fd 3
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		log.Fatalf("spawn cpu worker %d: %v", idx, err)
	}
	_ = child.Close()
	return &cpuChild{idx: idx, pid: cmd.Process.Pid, ctrl: parent}
}

func spawnMEM() *memChild {
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		log.Fatalf("socketpair: %v", err)
	}
	parent := os.NewFile(uintptr(pair[0]), "ctrl-parent")
	child := os.NewFile(uintptr(pair[1]), "ctrl-child")

	exe, _ := os.Executable()
	name := pickWorkerName()
	cmd := exec.Command(exe)
	cmd.Args = []string{name}
	cmd.Env = append(os.Environ(),
		roleEnv+"="+roleMEM,
		roleNameEnv+"="+name,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{child}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		log.Fatalf("spawn mem worker: %v", err)
	}
	_ = child.Close()
	return &memChild{pid: cmd.Process.Pid, ctrl: parent}
}

func runMaster() {
	setComm("stress")
	cfg = loadConfig()
	if cfg.Target < 0 || cfg.Target > 1 {
		log.Fatalf("STRESS_TARGET 必须在 0~1 之间: %v", cfg.Target)
	}
	if cfg.CPUOnly && cfg.MemOnly {
		log.Fatalf("STRESS_CPU_ONLY 和 STRESS_MEM_ONLY 不能同时为 true")
	}

	nCores := runtime.NumCPU()

	var cpuKids []*cpuChild
	var mem *memChild
	if !cfg.MemOnly {
		for i := 1; i <= nCores; i++ {
			cpuKids = append(cpuKids, spawnCPU(i))
		}
	}
	if !cfg.CPUOnly {
		mem = spawnMEM()
	}

	memTotal, _, err := readMemInfo()
	if err != nil {
		log.Fatalf("读取 /proc/meminfo: %v", err)
	}
	prevStat, err := readCPUStat()
	if err != nil {
		log.Fatalf("读取 /proc/stat: %v", err)
	}
	prevOwn := uint64(0)
	for _, c := range cpuKids {
		prevOwn += readPidCPU(c.pid)
	}
	if mem != nil {
		prevOwn += readPidCPU(mem.pid)
	}

	if !cfg.Quiet {
		log.Printf("启动：target=%.0f%% cores=%d memTotal=%.1fGiB interval=%s workers=%d",
			cfg.Target*100, nCores, float64(memTotal)/(1<<30), cfg.Interval, len(cpuKids)+boolToInt(mem != nil))
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	tick := time.NewTicker(cfg.Interval)
	defer tick.Stop()

loop:
	for {
		select {
		case <-sig:
			break loop
		case <-tick.C:
		}

		// CPU 控制：把目标 load 广播给所有 CPU worker
		if !cfg.MemOnly {
			curStat, err := readCPUStat()
			if err != nil {
				log.Printf("cpu stat: %v", err)
				continue
			}
			curOwn := uint64(0)
			for _, c := range cpuKids {
				curOwn += readPidCPU(c.pid)
			}
			if mem != nil {
				curOwn += readPidCPU(mem.pid)
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
				bits := math.Float64bits(desired)
				payload := packU64(bits)
				for _, c := range cpuKids {
					_, _ = c.ctrl.Write(payload)
				}
				if !cfg.Quiet {
					log.Printf("CPU sys=%5.1f%% own=%5.1f%% other=%5.1f%% -> load=%5.1f%%",
						sysUsage*100, ownUsage*100, otherUsage*100, desired*100)
				}
			}
		}

		// MEM 控制：把目标字节数推给 mem worker
		if !cfg.CPUOnly && mem != nil {
			_, memAvail, err := readMemInfo()
			if err != nil {
				log.Printf("meminfo: %v", err)
				continue
			}
			ownRSS := readPidRSS(mem.pid)
			for _, c := range cpuKids {
				ownRSS += readPidRSS(c.pid)
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

			// master 不知道 worker 当前缓冲量，按 RSS 近似（worker 进程 RSS ≈ 占位 + 少量 runtime）
			curBuf := int64(readPidRSS(mem.pid))
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
			safeCap := int64(memAvail) + curBuf - int64(float64(memTotal)*0.03)
			if newBuf > safeCap {
				newBuf = safeCap
			}
			if newBuf < 0 {
				newBuf = 0
			}
			payload := packU64(uint64(newBuf))
			_, _ = mem.ctrl.Write(payload)

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
		log.Printf("收到退出信号，关闭 worker…")
	}
	for _, c := range cpuKids {
		_ = c.ctrl.Close() // 关 fd 让 worker 的 read 返回 EOF
	}
	if mem != nil {
		_ = mem.ctrl.Close()
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		alive := false
		for _, c := range cpuKids {
			if pidAlive(c.pid) {
				alive = true
				break
			}
		}
		if !alive && (mem == nil || !pidAlive(mem.pid)) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, c := range cpuKids {
		if pidAlive(c.pid) {
			_ = syscall.Kill(c.pid, syscall.SIGKILL)
		}
	}
	if mem != nil && pidAlive(mem.pid) {
		_ = syscall.Kill(mem.pid, syscall.SIGKILL)
	}
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func main() {
	switch os.Getenv(roleEnv) {
	case roleCPU:
		runCPUWorker()
	case roleMEM:
		runMEMWorker()
	default:
		runMaster()
	}
}

