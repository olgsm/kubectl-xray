package xray

import (
	"testing"

	"github.com/spf13/pflag"
)

func TestResolveSteps(t *testing.T) {
	cases := []struct {
		label                   string
		args                    []string
		thread, histogram, heap bool
	}{
		{label: "no flags enables the default set", args: nil, thread: true, histogram: true, heap: true},
		{label: "one flag selects only it", args: []string{"--heap"}, heap: true},
		{label: "two flags select only those", args: []string{"--heap", "--thread"}, thread: true, heap: true},
		{label: "explicit false selects nothing", args: []string{"--heap=false"}},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			var thread, histogram, heap bool
			fs := pflag.NewFlagSet("jvm-dump", pflag.ContinueOnError)
			fs.BoolVar(&thread, "thread", false, "")
			fs.BoolVar(&histogram, "histogram", false, "")
			fs.BoolVar(&heap, "heap", false, "")
			if err := fs.Parse(c.args); err != nil {
				t.Fatalf("parsing %v: %v", c.args, err)
			}

			resolveSteps(fs, []string{"thread", "histogram", "heap"}, &thread, &histogram, &heap)

			if thread != c.thread || histogram != c.histogram || heap != c.heap {
				t.Errorf("thread/histogram/heap = %v/%v/%v, want %v/%v/%v",
					thread, histogram, heap, c.thread, c.histogram, c.heap)
			}
		})
	}
}
