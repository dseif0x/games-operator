package k8s

import (
	"context"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DevicesSelector selects the device plugin's pods (the chart's labels).
const DevicesSelector = "app.kubernetes.io/name=games-operator,app.kubernetes.io/component=devices"

// RollDevicePods replaces device plugin pods that run another image than
// image, one pass. The plugin's DaemonSet updates OnDelete, so that a
// Helm upgrade does not wait for the pod on a sleeping GPU node; a pod is
// only replaced once it is running and ready, i.e. its node is up, and
// the DaemonSet then creates it anew from the current template. Returns
// how many pods were deleted.
func RollDevicePods(ctx context.Context, cs kubernetes.Interface, namespace, image string, log *slog.Logger) (int, error) {
	list, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: DevicesSelector})
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range list.Items {
		p := &list.Items[i]
		if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning || len(p.Spec.Containers) == 0 || p.Spec.Containers[0].Image == image {
			continue
		}
		ready := len(p.Status.ContainerStatuses) > 0
		for _, c := range p.Status.ContainerStatuses {
			ready = ready && c.Ready
		}
		if !ready {
			continue
		}
		if err := cs.CoreV1().Pods(namespace).Delete(ctx, p.Name, metav1.DeleteOptions{}); err != nil {
			return n, err
		}
		log.Info("device plugin pod replaced", "pod", p.Name, "node", p.Spec.NodeName, "image", p.Spec.Containers[0].Image, "wanted", image)
		n++
	}
	return n, nil
}

// RunDevicePodRoller runs RollDevicePods every interval until ctx ends.
func RunDevicePodRoller(ctx context.Context, cs kubernetes.Interface, namespace, image string, every time.Duration, log *slog.Logger) {
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := RollDevicePods(ctx, cs, namespace, image, log); err != nil && ctx.Err() == nil {
			log.Warn("device plugin pods not checked", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
