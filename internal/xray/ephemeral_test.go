package xray

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestParseStatusUID(t *testing.T) {
	tests := []struct {
		name    string
		status  string
		want    int64
		wantErr bool
	}{
		{
			name:   "typical proc status",
			status: "Name:\tepp\nState:\tS\nUid:\t65532\t65532\t65532\t65532\nGid:\t1\t1\t1\t1\n",
			want:   65532,
		},
		{
			name:   "uid zero",
			status: "Uid:\t0\t0\t0\t0\n",
			want:   0,
		},
		{
			name:    "no uid line",
			status:  "Name:\tx\nState:\tS\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseStatusUID(tt.status)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("uid = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDeriveUser(t *testing.T) {
	uid := int64(1000)
	gid := int64(2000)

	podWithContainerSC := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:            "app",
				SecurityContext: &corev1.SecurityContext{RunAsUser: &uid, RunAsGroup: &gid},
			}},
		},
	}
	if u, g := deriveUser(podWithContainerSC, "app"); u == nil || *u != 1000 || g == nil || *g != 2000 {
		t.Fatalf("container SC not picked up: u=%v g=%v", u, g)
	}

	podWithPodSC := &corev1.Pod{
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{RunAsUser: &uid},
			Containers:      []corev1.Container{{Name: "app"}},
		},
	}
	if u, _ := deriveUser(podWithPodSC, "app"); u == nil || *u != 1000 {
		t.Fatalf("pod-level SC fallback failed: u=%v", u)
	}

	podNoSC := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}
	if u, g := deriveUser(podNoSC, "app"); u != nil || g != nil {
		t.Fatalf("expected nil/nil when nothing set: u=%v g=%v", u, g)
	}
}
