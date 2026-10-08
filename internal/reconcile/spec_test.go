package reconcile

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/dseif0x/games-operator/internal/bridge"
	"github.com/dseif0x/games-operator/internal/config"
	"github.com/dseif0x/games-operator/internal/store"
)

func testCfg() Config {
	return Config{
		Namespace: "games", WolfImage: "wolf:test", PulseImage: "pulse:test", InitImage: "init:test", BridgeImage: "bridge:test",
		DefaultPVCSize: "50Gi", DefaultStorageClass: "nfs-fast", RuntimeClass: "nvidia", LBSharingKey: "games", LBIP: "10.13.254.9",
		DefaultResources: config.Resources{
			Requests: config.ResourceList{CPU: "2", Memory: "4Gi"},
			Limits:   config.ResourceList{CPU: "8", Memory: "16Gi", Extended: map[string]string{"nvidia.com/gpu": "1"}},
		},
		NodeSelector: map[string]string{"kubernetes.io/hostname": "visus"},
		Tolerations:  []config.Toleration{{Key: "nvidia.com/gpu", Operator: "Exists", Effect: "NoSchedule"}},
	}.Defaults()
}

func testApp() *store.App {
	return &store.App{
		ID: "11111111-2222-3333-4444-555555555555", OwnerID: "owner", MoonlightID: 42, Name: "Steam", Preset: "steam",
		Image: "ghcr.io/games-on-whales/steam:edge", PVCSize: "", Generation: 3, Slot: 2, State: store.StateStarting,
		Env:    map[string]string{"PROTON_LOG": "0", "MY_VAR": "x"},
		Stream: &store.Stream{ClientIP: "10.0.0.7", Width: 2560, Height: 1440, FPS: 120},
	}
}

func TestPorts(t *testing.T) {
	p := Ports(48100, 3)
	if p.RTSP != 48130 || p.Control != 48131 || p.Video != 48132 || p.Audio != 48133 {
		t.Fatalf("%+v", p)
	}
}

func TestBuildPVC(t *testing.T) {
	pvc := BuildPVC(testApp(), testCfg())
	if pvc.Name != "games-operator-11111111-2222-3333-4444-555555555555" || *pvc.Spec.StorageClassName != "nfs-fast" {
		t.Fatalf("%+v", pvc.Spec)
	}
	if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.String() != "50Gi" {
		t.Fatalf("size %s", q.String())
	}
}

func TestBuildService(t *testing.T) {
	svc := BuildService(testApp(), testCfg())
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer || svc.Annotations["metallb.io/allow-shared-ip"] != "games" || svc.Annotations["metallb.io/loadBalancerIPs"] != "10.13.254.9" {
		t.Fatalf("%+v", svc)
	}
	if len(svc.Spec.Ports) != 4 || svc.Spec.Ports[0].Port != 48120 || svc.Spec.Ports[1].Protocol != corev1.ProtocolUDP {
		t.Fatalf("ports %+v", svc.Spec.Ports)
	}
	if ServiceIP(&corev1.Service{Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "1.2.3.4"}}}}}) != "1.2.3.4" {
		t.Fatal("service ip")
	}
}

func TestBuildPod(t *testing.T) {
	app := testApp()
	pod := BuildPod(app, testCfg())
	if pod.Annotations["games-operator.io/generation"] != "3" || !pod.Spec.HostIPC || *pod.Spec.RuntimeClassName != "nvidia" {
		t.Fatalf("pod meta: %+v", pod.Spec)
	}
	names := map[string]corev1.Container{}
	for _, c := range pod.Spec.Containers {
		names[c.Name] = c
	}
	for _, want := range []string{ContainerApp, ContainerWolf, ContainerPulse, ContainerBridge} {
		if _, ok := names[want]; !ok {
			t.Fatalf("container %s missing", want)
		}
	}
	a := names[ContainerApp]
	env := map[string]string{}
	for _, e := range a.Env {
		env[e.Name] = e.Value
	}
	if env["PROTON_LOG"] != "0" || env["MY_VAR"] != "x" || env["RUN_SWAY"] != "true" || env["GAMESCOPE_WIDTH"] != "2560" || env["GAMESCOPE_REFRESH"] != "120" {
		t.Fatalf("app env: %v", env)
	}
	if q := a.Resources.Limits["nvidia.com/gpu"]; q.String() != "1" {
		t.Fatalf("gpu limit: %v", a.Resources.Limits)
	}
	if a.SecurityContext.SeccompProfile == nil || a.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined {
		t.Fatal("steam must be unconfined")
	}
	if !strings.Contains(a.Command[2], "pulse-socket") {
		t.Fatal("default command missing")
	}
	w := names[ContainerWolf]
	wenv := map[string]string{}
	for _, e := range w.Env {
		wenv[e.Name] = e.Value
	}
	if wenv["WOLF_RTSP_SETUP_PORT"] != "48120" || wenv["WOLF_AUDIO_PING_PORT"] != "48123" || wenv["WOLF_SOCKET_PATH"] != bridge.DefaultSocket {
		t.Fatalf("wolf env: %v", wenv)
	}
	if _, ok := w.Resources.Limits["nvidia.com/gpu"]; ok {
		t.Fatal("wolf must not request a gpu by default")
	}
	b := names[ContainerBridge]
	if b.ReadinessProbe == nil || b.ReadinessProbe.HTTPGet.Path != "/readyz" || b.Env[1].ValueFrom.SecretKeyRef.Key != bridge.EnvToken {
		t.Fatalf("bridge: %+v", b)
	}
	if pod.Spec.Tolerations[0].Key != "nvidia.com/gpu" || pod.Spec.NodeSelector["kubernetes.io/hostname"] != "visus" {
		t.Fatalf("scheduling: %+v", pod.Spec)
	}
}

func TestBuildPodFirefoxIsConfined(t *testing.T) {
	app := testApp()
	app.Preset = "firefox"
	app.Env = nil
	pod := BuildPod(app, testCfg())
	if pod.Spec.HostIPC {
		t.Fatal("firefox does not need hostIPC")
	}
	a := pod.Spec.Containers[0]
	if a.SecurityContext.SeccompProfile != nil || len(a.SecurityContext.Capabilities.Add) != 0 {
		t.Fatalf("firefox security: %+v", a.SecurityContext)
	}
}

func TestBuildPodUinputResource(t *testing.T) {
	cfg := testCfg()
	cfg.UinputResource = "squat.ai/uinput"
	pod := BuildPod(testApp(), cfg)
	a := pod.Spec.Containers[0]
	if q := a.Resources.Limits["squat.ai/uinput"]; q.String() != "1" {
		t.Fatalf("uinput resource: %v", a.Resources.Limits)
	}
	for _, m := range a.VolumeMounts {
		if m.MountPath == "/dev/uinput" {
			t.Fatal("hostPath uinput must not be mounted when the resource is used")
		}
	}
}

func TestClampResources(t *testing.T) {
	def := testCfg().DefaultResources
	ceiling := config.Resources{Limits: config.ResourceList{CPU: "16", Memory: "32Gi", Extended: map[string]string{"nvidia.com/gpu": "1"}}}
	got := ClampResources(config.Resources{Limits: config.ResourceList{CPU: "32", Memory: "8Gi", Extended: map[string]string{"nvidia.com/gpu": "2"}}}, def, ceiling)
	if got.Limits.Cpu().String() != "16" || got.Limits.Memory().String() != "8Gi" {
		t.Fatalf("limits %v", got.Limits)
	}
	if q := got.Limits["nvidia.com/gpu"]; q.String() != "1" {
		t.Fatalf("gpu capped: %v", got.Limits)
	}
	if got.Requests.Memory().Cmp(*got.Limits.Memory()) > 0 {
		t.Fatal("request above limit")
	}
}

func TestPodReason(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/4 nodes"}}}}
	if r := PodReason(pod); !strings.Contains(r, "Unschedulable") {
		t.Fatal(r)
	}
	if PodReady(pod) || PodTerminal(pod) {
		t.Fatal("pending pod is neither ready nor terminal")
	}
}
