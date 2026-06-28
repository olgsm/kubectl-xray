package xray

import (
	"strings"
	"testing"
)

func TestBuildJVMDumpScript(t *testing.T) {
	const name = "mypod"
	tests := []struct {
		label                   string
		thread, histogram, heap bool
		wantContains            []string
		wantAbsent              []string
	}{
		{
			label: "all steps", thread: true, histogram: true, heap: true,
			wantContains: []string{"jstack 1", "GC.class_histogram", "jmap -dump:live", "mypod.jstack", "mypod.hprof", "/proc/1/root/tmp/mypod.hprof"},
		},
		{
			label: "thread only", thread: true,
			wantContains: []string{"jstack 1", "mypod.jstack"},
			wantAbsent:   []string{"GC.class_histogram", "jmap"},
		},
		{
			label: "histogram only", histogram: true,
			wantContains: []string{"GC.class_histogram"},
			wantAbsent:   []string{"jstack", "jmap"},
		},
		{
			label: "heap only", heap: true,
			wantContains: []string{"jmap -dump:live"},
			wantAbsent:   []string{"jstack", "GC.class_histogram"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			got := buildJVMDumpScript(tt.thread, tt.histogram, tt.heap, name)
			if !strings.HasPrefix(got, `W="$(mktemp -d)"; `) {
				t.Errorf("script must set up a work dir first; got %q", got)
			}
			if !strings.HasSuffix(got, `tar czf - -C "$W" .`) {
				t.Errorf("script must end by gzip-tarring the work dir to stdout; got %q", got)
			}
			for _, s := range tt.wantContains {
				if !strings.Contains(got, s) {
					t.Errorf("want %q in script; got %q", s, got)
				}
			}
			for _, s := range tt.wantAbsent {
				if strings.Contains(got, s) {
					t.Errorf("did not want %q in script; got %q", s, got)
				}
			}
		})
	}
}
