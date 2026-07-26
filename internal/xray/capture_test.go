package xray

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseTarget(t *testing.T) {
	cases := []struct {
		arg, kind, name string
		wantErr         bool
	}{
		{arg: "foo", kind: "", name: "foo"},
		{arg: "pod/foo", kind: "pod", name: "foo"},
		{arg: "po/foo", kind: "pod", name: "foo"},
		{arg: "deploy/foo", kind: "deployment", name: "foo"},
		{arg: "deployments/foo", kind: "deployment", name: "foo"},
		{arg: "svc/foo", wantErr: true},
		{arg: "deploy/", wantErr: true},
	}
	for _, c := range cases {
		kind, name, err := parseTarget(c.arg)
		if (err != nil) != c.wantErr {
			t.Fatalf("parseTarget(%q) err = %v, wantErr %v", c.arg, err, c.wantErr)
		}
		if err == nil && (kind != c.kind || name != c.name) {
			t.Errorf("parseTarget(%q) = %q, %q; want %q, %q", c.arg, kind, name, c.kind, c.name)
		}
	}
}

func TestPickPod(t *testing.T) {
	pod := func(name string, phase corev1.PodPhase, ready bool, ageMin int) corev1.Pod {
		p := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Duration(ageMin) * time.Minute)),
			},
			Status: corev1.PodStatus{Phase: phase},
		}
		if ready {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}
		return p
	}

	if got := pickPod(nil); got != nil {
		t.Errorf("pickPod(nil) = %v, want nil", got)
	}

	cases := []struct {
		name string
		pods []corev1.Pod
		want string
	}{
		{"ready beats running", []corev1.Pod{
			pod("a", corev1.PodRunning, false, 1),
			pod("b", corev1.PodRunning, true, 10),
		}, "b"},
		{"running beats pending", []corev1.Pod{
			pod("a", corev1.PodPending, false, 1),
			pod("b", corev1.PodRunning, false, 10),
		}, "b"},
		{"newest among ready", []corev1.Pod{
			pod("a", corev1.PodRunning, true, 10),
			pod("b", corev1.PodRunning, true, 1),
		}, "b"},
		{"falls back to newest non-running", []corev1.Pod{
			pod("a", corev1.PodPending, false, 10),
			pod("b", corev1.PodFailed, false, 1),
		}, "b"},
	}
	for _, c := range cases {
		if got := pickPod(c.pods); got == nil || got.Name != c.want {
			t.Errorf("%s: pickPod = %v, want %s", c.name, got, c.want)
		}
	}
}
