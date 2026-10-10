package k8s

import (
	"context"
	"io"
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRollDevicePods(t *testing.T) {
	labels := map[string]string{"app.kubernetes.io/name": "games-operator", "app.kubernetes.io/component": "devices"}
	pod := func(name, image string, phase corev1.PodPhase, ready bool, deleting bool) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "games", Labels: labels},
			Spec:       corev1.PodSpec{NodeName: "n-" + name, Containers: []corev1.Container{{Name: "devices", Image: image}}},
			Status:     corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{{Name: "devices", Ready: ready}}},
		}
		if deleting {
			now := metav1.Now()
			p.DeletionTimestamp = &now
			p.Finalizers = []string{"test"}
		}
		return p
	}
	cs := fake.NewSimpleClientset(
		pod("current", "img:2", corev1.PodRunning, true, false),
		pod("stale-up", "img:1", corev1.PodRunning, true, false),
		pod("stale-asleep", "img:1", corev1.PodRunning, false, false),
		pod("stale-pending", "img:1", corev1.PodPending, false, false),
		pod("stale-going", "img:1", corev1.PodRunning, true, true),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "games"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "img:1"}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}},
	)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n, err := RollDevicePods(context.Background(), cs, "games", "img:2", log)
	if err != nil || n != 1 {
		t.Fatalf("rolled %d %v", n, err)
	}
	left, _ := cs.CoreV1().Pods("games").List(context.Background(), metav1.ListOptions{})
	names := map[string]bool{}
	for _, p := range left.Items {
		names[p.Name] = true
	}
	if names["stale-up"] || !names["current"] || !names["stale-asleep"] || !names["stale-pending"] || !names["stale-going"] || !names["other"] {
		t.Fatalf("pods left: %v", names)
	}
	if n, _ := RollDevicePods(context.Background(), cs, "games", "img:2", log); n != 0 {
		t.Fatal("nothing left to roll")
	}
}
