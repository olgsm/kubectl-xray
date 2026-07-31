package xray

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestBuildInfoCommand(t *testing.T) {
	t.Run("metrics only when a port is given", func(t *testing.T) {
		plain := strings.Join(buildInfoCommand("", "/metrics"), " ")
		if strings.Contains(plain, "wget") {
			t.Error("no --metrics-port must mean no scrape")
		}
		withPort := strings.Join(buildInfoCommand("8080", "/actuator/prometheus"), " ")
		if !strings.Contains(withPort, "http://localhost:8080/actuator/prometheus") {
			t.Errorf("want the scrape URL in the script; got %q", withPort)
		}
	})

	// The script reads files that may not exist (cgroup v1 vs v2, no smaps_rollup).
	// It must still exit 0, since capture() reports a non-zero exit as a failure.
	t.Run("exits 0 when nothing is readable", func(t *testing.T) {
		for _, args := range [][]string{
			buildInfoCommand("", "/metrics"),
			buildInfoCommand("9999", "/metrics"),
		} {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			cmd := exec.CommandContext(ctx, args[0], args[1:]...)
			out, err := cmd.CombinedOutput()
			cancel()
			if err != nil {
				t.Fatalf("script exited non-zero: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), "== system ==") {
				t.Errorf("want the system section in the output; got:\n%s", out)
			}
		}
	})
}
