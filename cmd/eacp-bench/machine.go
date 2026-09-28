package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/bench"
)

// machine describes the host, Docker and PostgreSQL. A field that cannot be
// read stays empty or 0 and is reported as such.
func machine(ctx context.Context, db *pgxpool.Pool) bench.Machine {
	m := bench.Machine{HostCores: runtime.NumCPU(), OS: runtime.GOOS + "/" + runtime.GOARCH, Go: runtime.Version()}
	switch runtime.GOOS {
	case "windows":
		m.HostCPU = output(ctx, "powershell", "-NoProfile", "-Command", "(Get-CimInstance Win32_Processor | Select-Object -First 1).Name")
		m.HostMemBytes, _ = strconv.ParseInt(output(ctx, "powershell", "-NoProfile", "-Command",
			"(Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory"), 10, 64)
	case "darwin":
		m.HostCPU = output(ctx, "sysctl", "-n", "machdep.cpu.brand_string")
		m.HostMemBytes, _ = strconv.ParseInt(output(ctx, "sysctl", "-n", "hw.memsize"), 10, 64)
	default:
		m.HostCPU, m.HostMemBytes = procInfo()
	}
	if f := strings.Fields(output(ctx, "docker", "info", "--format", "{{.NCPU}} {{.MemTotal}} {{.ServerVersion}}")); len(f) == 3 {
		m.DockerCPUs, _ = strconv.Atoi(f[0])
		m.DockerMemBytes, _ = strconv.ParseInt(f[1], 10, 64)
		m.Docker = f[2]
	}
	_ = db.QueryRow(ctx, `SHOW server_version`).Scan(&m.Postgres)
	return m
}

func output(ctx context.Context, name string, args ...string) string {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func procInfo() (cpu string, mem int64) {
	if f, err := os.Open("/proc/cpuinfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == "model name" {
				cpu = strings.TrimSpace(v)
				break
			}
		}
		f.Close()
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if fs := strings.Fields(sc.Text()); len(fs) >= 2 && fs[0] == "MemTotal:" {
				kb, _ := strconv.ParseInt(fs[1], 10, 64)
				mem = kb * 1024
				break
			}
		}
		f.Close()
	}
	return cpu, mem
}

// git returns the short commit and whether the worktree has changes.
func git(ctx context.Context) (sha string, dirty bool) {
	return output(ctx, "git", "rev-parse", "--short=7", "HEAD"), output(ctx, "git", "status", "--porcelain") != ""
}
