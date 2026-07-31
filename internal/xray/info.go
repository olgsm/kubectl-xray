package xray

import (
	"fmt"
	"strings"
)

// infoScript reports what the target process is and what the kernel is
// enforcing on it. Everything here is world- or owner-readable under the
// target's UID, so it needs no capabilities.
//
// Limits come from the target's own cgroup view via /proc/1/root: each
// container gets its own cgroup namespace, so the toolbox's /sys/fs/cgroup
// describes the toolbox, not the app.
const infoScript = `
echo "== process =="
tr '\0' ' ' < /proc/1/cmdline; echo
grep -E '^(Name|State|PPid|Threads|VmRSS|VmSwap|VmSize|FDSize):' /proc/1/status

echo
echo "== memory =="
grep -E '^(Rss|Pss|Shared_Clean|Private_Dirty|Swap):' /proc/1/smaps_rollup 2>/dev/null ||
	echo "(smaps_rollup unavailable)"

echo
echo "== cgroup =="
C=/proc/1/root/sys/fs/cgroup
for f in memory.max memory.current memory.peak cpu.max cpu.pressure; do
	[ -r "$C/$f" ] && echo "$f: $(cat "$C/$f")"
done
for f in memory/memory.limit_in_bytes memory/memory.usage_in_bytes cpu/cpu.cfs_quota_us; do
	[ -r "$C/$f" ] && echo "$f: $(cat "$C/$f")"
done
grep -E '^(anon|file|slab|sock) ' "$C/memory.stat" 2>/dev/null

echo
echo "== fds =="
echo "open: $(ls /proc/1/fd 2>/dev/null | wc -l)"
grep -E '^Max open files' /proc/1/limits

echo
echo "== system =="
uname -a
echo "cpus: $(nproc 2>/dev/null || echo '?')"
cat /proc/loadavg
`

// buildInfoCommand assembles the info shell command. A non-empty metricsPort
// appends a scrape of the app's metrics endpoint over localhost — every
// container in a pod shares the network namespace, so no extra plumbing.
func buildInfoCommand(metricsPort, metricsPath string) []string {
	var b strings.Builder
	b.WriteString(infoScript)
	if metricsPort != "" {
		_, _ = fmt.Fprintf(&b, `
echo
echo "== metrics =="
wget -q -O - "http://localhost:%s%s" || echo "(scrape of port %s failed)"
`, metricsPort, metricsPath, metricsPort)
	}
	b.WriteString("\ntrue\n") // optional reads above may fail; the capture itself did not
	return []string{"sh", "-c", b.String()}
}
