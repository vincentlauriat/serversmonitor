package collect

import (
	"context"
	"strings"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/sensors"
	"github.com/vincentlauriat/serversmonitor/internal/proto"
)

type gopsutilReader struct{}

// NewGopsutil reads the local machine through gopsutil.
func NewGopsutil() SystemReader { return gopsutilReader{} }

func (gopsutilReader) CPUPercent(ctx context.Context) (float64, error) {
	// interval 0 means "since the previous call", which matches our sampling loop.
	v, err := cpu.PercentWithContext(ctx, 0, false)
	if err != nil {
		return 0, err
	}
	if len(v) == 0 {
		return 0, ErrUnavailable
	}
	return v[0], nil
}

func (gopsutilReader) Memory(ctx context.Context) (int64, int64, int64, int64, error) {
	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	sw, err := mem.SwapMemoryWithContext(ctx)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return int64(vm.Used), int64(vm.Total), int64(sw.Used), int64(sw.Total), nil
}

func (gopsutilReader) Load(ctx context.Context) (float64, float64, float64, error) {
	l, err := load.AvgWithContext(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	return l.Load1, l.Load5, l.Load15, nil
}

func (gopsutilReader) Uptime(ctx context.Context) (int64, error) {
	u, err := host.UptimeWithContext(ctx)
	return int64(u), err
}

var pseudoFS = map[string]bool{"tmpfs": true, "devtmpfs": true, "overlay": true, "squashfs": true, "proc": true,
	"sysfs": true, "cgroup": true, "cgroup2": true, "devpts": true, "autofs": true, "efivarfs": true, "nsfs": true,
	"fusectl": true, "debugfs": true, "tracefs": true, "securityfs": true, "pstore": true, "bpf": true,
	"configfs": true, "hugetlbfs": true, "mqueue": true, "ramfs": true, "binfmt_misc": true, "rpc_pipefs": true}

// noisyMounts are paths nobody wants a disk gauge for: read-only system
// volumes, recovery partitions, simulator images, snap loops. They matter
// because the disk alert takes the fullest mount — a simulator image at 97 %
// would otherwise fire an alert about a disk the user cannot act on.
var noisyMounts = []string{
	"/dev",
	"/System/Volumes/",
	"/Volumes/Recovery",
	"/private/var/folders/",
	"/Library/Developer/CoreSimulator/Volumes/",
	"/snap/",
	"/var/lib/docker/",
	"/run/",
	"/boot/efi",
}

func noisyMount(mount string) bool {
	for _, p := range noisyMounts {
		if mount == p || strings.HasPrefix(mount, p) {
			return true
		}
	}
	return false
}

func (gopsutilReader) Disks(ctx context.Context) ([]DiskUsage, error) {
	parts, err := disk.PartitionsWithContext(ctx, false)
	if err != nil {
		return nil, err
	}
	counters, _ := disk.IOCountersWithContext(ctx) // best effort: absent on some platforms
	var out []DiskUsage
	seenMount := map[string]bool{}
	// Several mount points can be views of one volume (on macOS, "/" and
	// "/System/Volumes/Data" share an APFS container and report identical
	// numbers). Keep the first and drop the rest, or the dashboard shows the
	// same disk three times.
	seenUsage := map[[2]int64]bool{}
	for _, p := range parts {
		if pseudoFS[p.Fstype] || seenMount[p.Mountpoint] || noisyMount(p.Mountpoint) {
			continue
		}
		u, err := disk.UsageWithContext(ctx, p.Mountpoint)
		if err != nil || u.Total == 0 {
			continue
		}
		key := [2]int64{int64(u.Used), int64(u.Total)}
		if seenUsage[key] {
			continue
		}
		seenMount[p.Mountpoint], seenUsage[key] = true, true
		d := DiskUsage{Mount: p.Mountpoint, Used: int64(u.Used), Total: int64(u.Total)}
		dev := strings.TrimPrefix(p.Device, "/dev/")
		if c, ok := counters[dev]; ok {
			d.ReadBytes, d.WriteBytes = int64(c.ReadBytes), int64(c.WriteBytes)
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, ErrUnavailable
	}
	return out, nil
}

// virtualIface skips loopback and the bridges Docker creates, which would
// otherwise double-count container traffic as host traffic.
func virtualIface(name string) bool {
	if name == "lo" || name == "lo0" {
		return true
	}
	for _, p := range []string{"docker", "veth", "br-", "virbr", "utun", "awdl", "llw", "bridge"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (gopsutilReader) Net(ctx context.Context) (int64, int64, error) {
	cs, err := net.IOCountersWithContext(ctx, true)
	if err != nil {
		return 0, 0, err
	}
	var sent, recv int64
	for _, c := range cs {
		if virtualIface(c.Name) {
			continue
		}
		sent += int64(c.BytesSent)
		recv += int64(c.BytesRecv)
	}
	return sent, recv, nil
}

func (gopsutilReader) Temps(ctx context.Context) ([]proto.Temp, error) {
	ts, err := sensors.TemperaturesWithContext(ctx)
	if err != nil && len(ts) == 0 {
		return nil, err
	}
	out := make([]proto.Temp, 0, len(ts))
	for _, t := range ts {
		if t.Temperature <= 0 {
			continue
		}
		out = append(out, proto.Temp{Sensor: t.SensorKey, Celsius: t.Temperature})
	}
	if len(out) == 0 {
		return nil, ErrUnavailable
	}
	return out, nil
}
