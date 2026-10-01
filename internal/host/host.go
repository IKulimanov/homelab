// Package host читает метрики сервера из /proc и /sys. В контейнере это данные хоста:
// /proc/meminfo, /proc/stat и sysfs не изолируются Docker.
package host

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Roots — где искать файлы. В тестах — временные каталоги, в контейнере — /proc, /sys и /hostfs.
type Roots struct {
	Proc string
	Sys  string
	// FS — корень файловой системы хоста для statfs: диск хоста смонтирован в контейнер сюда.
	FS string
}

// Unknown — значение метрики, которую прочитать не удалось (нет датчика, нет файла).
var Unknown = math.NaN()

func Known(v float64) bool { return !math.IsNaN(v) }

type Disk struct {
	Path    string
	UsedPct float64
	Free    uint64
	Total   uint64
}

type Battery struct {
	Percent     float64
	Status      string // Charging, Discharging, Full, Not charging
	Discharging bool
}

type Snapshot struct {
	CPUTemp    float64
	SSDTemp    float64
	CPUBusyPct float64
	MemPct     float64
	SwapPct    float64
	Load1      float64
	Load5      float64
	Load15     float64
	CPUs       int
	Uptime     time.Duration
	Disks      []Disk
	Battery    *Battery // nil — батареи нет
}

// Collector хранит прошлый снимок /proc/stat: загрузка CPU считается по разнице двух замеров.
type Collector struct {
	Roots Roots
	Disks []string

	prevBusy, prevTotal uint64
}

// Collect читает всё, что получится; отсутствующий датчик даёт Unknown, а не ошибку.
func (c *Collector) Collect() Snapshot {
	s := Snapshot{CPUTemp: Unknown, SSDTemp: Unknown, CPUBusyPct: Unknown, MemPct: Unknown, SwapPct: Unknown,
		Load1: Unknown, Load5: Unknown, Load15: Unknown}
	if m, sw, err := MemInfo(c.Roots.Proc); err == nil {
		s.MemPct, s.SwapPct = m, sw
	}
	if l, err := LoadAvg(c.Roots.Proc); err == nil {
		s.Load1, s.Load5, s.Load15 = l[0], l[1], l[2]
	}
	if busy, total, cpus, err := CPUStat(c.Roots.Proc); err == nil {
		s.CPUs = cpus
		if c.prevTotal > 0 && total > c.prevTotal {
			s.CPUBusyPct = 100 * float64(busy-c.prevBusy) / float64(total-c.prevTotal)
		}
		c.prevBusy, c.prevTotal = busy, total
	}
	if up, err := Uptime(c.Roots.Proc); err == nil {
		s.Uptime = up
	}
	s.CPUTemp, s.SSDTemp = Temps(c.Roots.Sys)
	s.Battery = ReadBattery(c.Roots.Sys)
	for _, p := range c.Disks {
		if d, err := DiskUsage(c.Roots.FS, p); err == nil {
			s.Disks = append(s.Disks, d)
		}
	}
	return s
}

// MemInfo — занятая память и swap в процентах. Занято = всего − доступно: кэш ядра свободной памятью считается.
func MemInfo(proc string) (mem, swap float64, err error) {
	data, err := os.ReadFile(filepath.Join(proc, "meminfo"))
	if err != nil {
		return 0, 0, err
	}
	v := map[string]float64{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		if n, err := strconv.ParseFloat(f[0], 64); err == nil {
			v[name] = n
		}
	}
	if v["MemTotal"] == 0 {
		return 0, 0, fmt.Errorf("meminfo: нет MemTotal")
	}
	mem = 100 * (v["MemTotal"] - v["MemAvailable"]) / v["MemTotal"]
	if v["SwapTotal"] > 0 {
		swap = 100 * (v["SwapTotal"] - v["SwapFree"]) / v["SwapTotal"]
	}
	return mem, swap, nil
}

func LoadAvg(proc string) ([3]float64, error) {
	var l [3]float64
	data, err := os.ReadFile(filepath.Join(proc, "loadavg"))
	if err != nil {
		return l, err
	}
	f := strings.Fields(string(data))
	if len(f) < 3 {
		return l, fmt.Errorf("loadavg: %q", data)
	}
	for i := range 3 {
		if l[i], err = strconv.ParseFloat(f[i], 64); err != nil {
			return l, fmt.Errorf("loadavg: %w", err)
		}
	}
	return l, nil
}

// CPUStat — счётчики занятого и общего времени CPU и число ядер.
func CPUStat(proc string) (busy, total uint64, cpus int, err error) {
	data, err := os.ReadFile(filepath.Join(proc, "stat"))
	if err != nil {
		return 0, 0, 0, err
	}
	for line := range strings.Lines(string(data)) {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		if f[0] != "cpu" {
			cpus++
			continue
		}
		for i, s := range f[1:] {
			n, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return 0, 0, 0, fmt.Errorf("stat: %w", err)
			}
			total += n
			// idle и iowait — простой; остальное — работа.
			if i != 3 && i != 4 {
				busy += n
			}
		}
	}
	if total == 0 {
		return 0, 0, 0, fmt.Errorf("stat: нет строки cpu")
	}
	return busy, total, cpus, nil
}

func Uptime(proc string) (time.Duration, error) {
	data, err := os.ReadFile(filepath.Join(proc, "uptime"))
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(data))
	if len(f) == 0 {
		return 0, fmt.Errorf("uptime: пусто")
	}
	sec, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, fmt.Errorf("uptime: %w", err)
	}
	return time.Duration(sec) * time.Second, nil
}

var (
	cpuSensors  = []string{"coretemp", "k10temp", "zenpower", "cpu_thermal"}
	cpuLabels   = []string{"Package id 0", "Tctl", "Tdie"}
	ssdSensors  = []string{"nvme", "drivetemp"}
	cpuThermals = []string{"x86_pkg_temp", "TCPU", "acpitz"}
)

// Temps — температура CPU и самого горячего SSD в градусах. Порядок поиска как в simply-monitoring:
// hwmon с меткой пакета, иначе максимум по ядрам, иначе thermal_zone.
func Temps(sys string) (cpu, ssd float64) {
	cpu, ssd = Unknown, Unknown
	dirs, _ := filepath.Glob(filepath.Join(sys, "class", "hwmon", "hwmon*"))
	for _, dir := range dirs {
		name := readTrim(filepath.Join(dir, "name"))
		switch {
		case slices.Contains(cpuSensors, name):
			if t := hwmonTemp(dir, cpuLabels); Known(t) && (!Known(cpu) || t > cpu) {
				cpu = t
			}
		case slices.Contains(ssdSensors, name):
			if t := hwmonTemp(dir, nil); Known(t) && (!Known(ssd) || t > ssd) {
				ssd = t
			}
		}
	}
	if Known(cpu) {
		return cpu, ssd
	}
	zones, _ := filepath.Glob(filepath.Join(sys, "class", "thermal", "thermal_zone*"))
	for _, z := range zones {
		if slices.Contains(cpuThermals, readTrim(filepath.Join(z, "type"))) {
			if t, ok := milli(filepath.Join(z, "temp")); ok && (!Known(cpu) || t > cpu) {
				cpu = t
			}
		}
	}
	return cpu, ssd
}

// hwmonTemp — датчик с одной из меток labels, а если такого нет — максимум по всем temp*_input.
func hwmonTemp(dir string, labels []string) float64 {
	inputs, _ := filepath.Glob(filepath.Join(dir, "temp*_input"))
	best := Unknown
	for _, in := range inputs {
		t, ok := milli(in)
		if !ok {
			continue
		}
		label := readTrim(strings.TrimSuffix(in, "_input") + "_label")
		if slices.Contains(labels, label) {
			return t
		}
		if !Known(best) || t > best {
			best = t
		}
	}
	return best
}

// ReadBattery — первая батарея; nil, если её нет. Работа от батареи — это отключённое питание.
func ReadBattery(sys string) *Battery {
	supplies, _ := filepath.Glob(filepath.Join(sys, "class", "power_supply", "*"))
	for _, s := range supplies {
		if readTrim(filepath.Join(s, "type")) != "Battery" {
			continue
		}
		pct, err := strconv.ParseFloat(readTrim(filepath.Join(s, "capacity")), 64)
		if err != nil {
			continue
		}
		status := readTrim(filepath.Join(s, "status"))
		return &Battery{Percent: pct, Status: status, Discharging: status == "Discharging"}
	}
	return nil
}

// DiskUsage — как df: процент от места, доступного обычным пользователям, без резерва root.
func DiskUsage(fsRoot, path string) (Disk, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(filepath.Join(fsRoot, path), &st); err != nil {
		return Disk{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	bs := uint64(st.Bsize)
	used := (st.Blocks - st.Bfree) * bs
	avail := st.Bavail * bs
	d := Disk{Path: path, Free: avail, Total: st.Blocks * bs}
	if used+avail > 0 {
		d.UsedPct = 100 * float64(used) / float64(used+avail)
	}
	return d, nil
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// milli читает значение в тысячных долях градуса, как пишет ядро.
func milli(path string) (float64, bool) {
	n, err := strconv.ParseFloat(readTrim(path), 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n / 1000, true
}
