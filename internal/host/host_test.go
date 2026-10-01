package host

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestMemInfoUsesAvailable(t *testing.T) {
	proc := t.TempDir()
	write(t, proc, map[string]string{"meminfo": `MemTotal:       16000000 kB
MemFree:          500000 kB
MemAvailable:    4000000 kB
Buffers:          100000 kB
SwapTotal:       2000000 kB
SwapFree:        1500000 kB
`})
	mem, swap, err := MemInfo(proc)
	if err != nil {
		t.Fatal(err)
	}
	if !near(mem, 75) || !near(swap, 25) {
		t.Fatalf("mem=%v swap=%v, ждали 75 и 25", mem, swap)
	}
}

func TestMemInfoWithoutSwap(t *testing.T) {
	proc := t.TempDir()
	write(t, proc, map[string]string{"meminfo": "MemTotal: 1000 kB\nMemAvailable: 1000 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n"})
	if _, swap, err := MemInfo(proc); err != nil || swap != 0 {
		t.Fatalf("swap=%v err=%v", swap, err)
	}
}

func TestLoadAvgAndCPUStat(t *testing.T) {
	proc := t.TempDir()
	write(t, proc, map[string]string{
		"loadavg": "0.52 1.10 2.00 1/345 12345\n",
		"stat": `cpu  100 0 100 700 100 0 0 0 0 0
cpu0 50 0 50 350 50 0 0 0 0 0
cpu1 50 0 50 350 50 0 0 0 0 0
intr 1 2 3
`,
	})
	l, err := LoadAvg(proc)
	if err != nil || l != [3]float64{0.52, 1.10, 2.00} {
		t.Fatalf("load=%v err=%v", l, err)
	}
	busy, total, cpus, err := CPUStat(proc)
	if err != nil || busy != 200 || total != 1000 || cpus != 2 {
		t.Fatalf("busy=%d total=%d cpus=%d err=%v", busy, total, cpus, err)
	}
}

func TestCollectorCPUBusyFromDelta(t *testing.T) {
	proc := t.TempDir()
	c := &Collector{Roots: Roots{Proc: proc, Sys: t.TempDir()}}
	write(t, proc, map[string]string{"stat": "cpu 100 0 0 900 0 0 0 0 0 0\n"})
	if s := c.Collect(); Known(s.CPUBusyPct) {
		t.Fatalf("первый замер без базы, а получили %v", s.CPUBusyPct)
	}
	write(t, proc, map[string]string{"stat": "cpu 150 0 0 950 0 0 0 0 0 0\n"})
	if s := c.Collect(); !near(s.CPUBusyPct, 50) {
		t.Fatalf("CPU = %v, ждали 50", s.CPUBusyPct)
	}
}

func TestTempsPreferPackageLabelAndHottestSSD(t *testing.T) {
	sys := t.TempDir()
	write(t, sys, map[string]string{
		"class/hwmon/hwmon0/name":        "acpitz\n",
		"class/hwmon/hwmon0/temp1_input": "99000\n",
		"class/hwmon/hwmon1/name":        "coretemp\n",
		"class/hwmon/hwmon1/temp1_label": "Package id 0\n",
		"class/hwmon/hwmon1/temp1_input": "61000\n",
		"class/hwmon/hwmon1/temp2_label": "Core 0\n",
		"class/hwmon/hwmon1/temp2_input": "66000\n",
		"class/hwmon/hwmon2/name":        "nvme\n",
		"class/hwmon/hwmon2/temp1_input": "41850\n",
		"class/hwmon/hwmon3/name":        "drivetemp\n",
		"class/hwmon/hwmon3/temp1_input": "38000\n",
	})
	cpu, ssd := Temps(sys)
	if !near(cpu, 61) || !near(ssd, 41.85) {
		t.Fatalf("cpu=%v ssd=%v", cpu, ssd)
	}
}

func TestTempsFallBackToThermalZone(t *testing.T) {
	sys := t.TempDir()
	write(t, sys, map[string]string{
		"class/thermal/thermal_zone0/type": "iwlwifi_1\n",
		"class/thermal/thermal_zone0/temp": "80000\n",
		"class/thermal/thermal_zone1/type": "x86_pkg_temp\n",
		"class/thermal/thermal_zone1/temp": "55000\n",
	})
	cpu, ssd := Temps(sys)
	if !near(cpu, 55) || Known(ssd) {
		t.Fatalf("cpu=%v ssd=%v", cpu, ssd)
	}
}

func TestBattery(t *testing.T) {
	sys := t.TempDir()
	if ReadBattery(sys) != nil {
		t.Fatal("батареи нет, а прочитали")
	}
	write(t, sys, map[string]string{
		"class/power_supply/AC/type":       "Mains\n",
		"class/power_supply/AC/online":     "0\n",
		"class/power_supply/BAT0/type":     "Battery\n",
		"class/power_supply/BAT0/capacity": "18\n",
		"class/power_supply/BAT0/status":   "Discharging\n",
	})
	b := ReadBattery(sys)
	if b == nil || b.Percent != 18 || !b.Discharging {
		t.Fatalf("battery = %+v", b)
	}
}

func TestDiskUsage(t *testing.T) {
	d, err := DiskUsage(t.TempDir(), "/")
	if err != nil {
		t.Fatal(err)
	}
	if d.Total == 0 || d.UsedPct <= 0 || d.UsedPct > 100 {
		t.Fatalf("disk = %+v", d)
	}
}
