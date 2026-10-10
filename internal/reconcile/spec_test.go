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
		Namespace: "games", WolfImage: "wolf:test", InitImage: "init:test", BridgeImage: "bridge:test",
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
	pod, err := BuildPod(app, testCfg())
	if err != nil {
		t.Fatal(err)
	}
	if pod.Annotations["games-operator.io/generation"] != "3" || pod.Spec.HostIPC || *pod.Spec.RuntimeClassName != "nvidia" {
		t.Fatalf("pod meta: %+v", pod.Spec)
	}
	names := map[string]corev1.Container{}
	for _, c := range pod.Spec.Containers {
		names[c.Name] = c
	}
	for _, want := range []string{ContainerApp, ContainerWolf, ContainerBridge} {
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
	pod, err := BuildPod(app, testCfg())
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.HostIPC {
		t.Fatal("firefox does not need hostIPC")
	}
	a := pod.Spec.Containers[0]
	if a.SecurityContext.SeccompProfile == nil || a.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || len(a.SecurityContext.Capabilities.Add) != 0 || a.SecurityContext.AppArmorProfile != nil {
		t.Fatalf("firefox security: %+v", a.SecurityContext)
	}
}

func TestNoContainerHasSysAdmin(t *testing.T) {
	cfg := testCfg()
	cfg.InputResource = "games-operator.io/input"
	pod, err := BuildPod(testApp(), cfg) // the Steam preset
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range pod.Spec.Containers {
		sc := c.SecurityContext
		if sc.Capabilities != nil {
			for _, cap := range sc.Capabilities.Add {
				if cap == "SYS_ADMIN" {
					t.Fatalf("%s must not have SYS_ADMIN", c.Name)
				}
			}
		}
		switch c.Name {
		case ContainerWolf:
			if sc.Capabilities != nil && len(sc.Capabilities.Add) != 0 {
				t.Fatalf("Wolf runs with the default capabilities, got %v", sc.Capabilities.Add)
			}
			if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				t.Fatal("Wolf runs under the default seccomp profile")
			}
		case ContainerApp:
			// Proton's sandbox: no seccomp, but the capabilities stay small.
			if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined {
				t.Fatal("Steam runs without seccomp (user namespaces)")
			}
			if n := len(sc.Capabilities.Add); n == 0 || n > 3 {
				t.Fatalf("Steam capabilities: %v", sc.Capabilities.Add)
			}
		}
	}
	for _, c := range pod.Spec.InitContainers {
		if c.SecurityContext.SeccompProfile == nil {
			t.Fatalf("%s runs under the default seccomp profile", c.Name)
		}
	}
}

func TestBuildPodDevicePlugin(t *testing.T) {
	cfg := testCfg()
	cfg.InputResource = "games-operator.io/input"
	pod, err := BuildPod(testApp(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range pod.Spec.Containers {
		switch c.Name {
		case ContainerApp, ContainerWolf:
			if q := c.Resources.Limits["games-operator.io/input"]; q.String() != "1" {
				t.Fatalf("%s must request the input device: %v", c.Name, c.Resources.Limits)
			}
			if c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
				t.Fatalf("%s must not be privileged with the device plugin", c.Name)
			}
		default:
			if _, ok := c.Resources.Limits["games-operator.io/input"]; ok {
				t.Fatalf("%s needs no input device", c.Name)
			}
		}
		for _, m := range c.VolumeMounts {
			if m.MountPath == "/dev" || m.MountPath == "/dev/input" || m.MountPath == "/dev/uinput" || m.Name == "dev" {
				t.Fatalf("%s must not mount the node's devices: %+v", c.Name, m)
			}
		}
	}
	for _, v := range pod.Spec.Volumes {
		if v.HostPath != nil {
			t.Fatalf("no hostPath with the device plugin: %+v", v)
		}
	}
	if pod.Spec.HostIPC {
		t.Fatal("the pod's own IPC namespace is enough")
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

func TestPodCrashing(t *testing.T) {
	waiting := func(reason string) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}}}}
	}
	if PodCrashing(nil) || PodCrashing(waiting("ContainerCreating")) || PodCrashing(waiting("PodInitializing")) {
		t.Fatal("benign waits are not crashes")
	}
	for _, reason := range []string{"StartError", "RunContainerError", "ImagePullBackOff"} {
		if !PodCrashing(waiting(reason)) {
			t.Fatalf("%s must count as crashing", reason)
		}
	}
	// A crash loop after clean exits (the compositor rebuild) is not a crash; after a failure it is.
	loop := waiting("CrashLoopBackOff")
	loop.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
	if PodCrashing(loop) {
		t.Fatal("a backed-off container that exited cleanly is not crashing")
	}
	loop.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 128
	if !PodCrashing(loop) {
		t.Fatal("a backed-off container that failed is crashing")
	}
	running := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	if PodCrashing(running) {
		t.Fatal("a running container is not crashing")
	}
}

func TestPodCarriesWolfConfig(t *testing.T) {
	cfg := testCfg()
	cfg.ClientCertPEM = "-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n"
	pod, err := BuildPod(testApp(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	conf := pod.Annotations[AnnotationWolfConfig]
	if !strings.Contains(conf, "[[paired_clients]]") || !strings.Contains(conf, "BEGIN CERTIFICATE") {
		t.Fatalf("wolf config on the pod lacks the paired client:\n%s", conf)
	}
	var mounted bool
	for _, v := range pod.Spec.Volumes {
		if v.DownwardAPI != nil && len(v.DownwardAPI.Items) == 1 && strings.Contains(v.DownwardAPI.Items[0].FieldRef.FieldPath, AnnotationWolfConfig) {
			mounted = true
		}
		if v.Secret != nil {
			t.Fatal("no Secret volume: the config comes from the pod itself")
		}
	}
	if !mounted || !strings.Contains(pod.Spec.InitContainers[0].Command[2], "cp /podinfo/wolf-config "+WolfStateDir+"/cfg/config.toml") {
		t.Fatal("the init container must copy the config from the downward API volume into Wolf's state folder")
	}
}

func TestUdevSharedWithWolfAndApp(t *testing.T) {
	cfg := testCfg()
	cfg.InputResource = "games-operator.io/input"
	pod, err := BuildPod(testApp(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	mounts := func(name string) map[string]corev1.VolumeMount {
		out := map[string]corev1.VolumeMount{}
		for _, c := range pod.Spec.Containers {
			if c.Name == name {
				for _, m := range c.VolumeMounts {
					out[m.MountPath] = m
				}
			}
		}
		return out
	}
	app, wolf, br := mounts(ContainerApp), mounts(ContainerWolf), mounts(ContainerBridge)
	for name, ms := range map[string]map[string]corev1.VolumeMount{"app": app, "wolf": wolf} {
		if m, ok := ms["/run/udev"]; !ok || m.Name != "udev" || !m.ReadOnly {
			t.Fatalf("%s must read the bridge's udev directory: %+v", name, ms)
		}
	}
	if m, ok := br[bridge.UdevDir]; !ok || m.Name != "udev" || m.ReadOnly {
		t.Fatal("the bridge must write the udev directory")
	}
	if m, ok := app["/dev/shm"]; !ok || m.Name != "shm" {
		t.Fatal("the app's /dev/shm must stay the pod's own")
	}
	var caps []corev1.Capability
	for _, c := range pod.Spec.Containers {
		if c.Name == ContainerBridge {
			caps = c.SecurityContext.Capabilities.Add
		}
	}
	if len(caps) != 1 || caps[0] != "NET_ADMIN" {
		t.Fatalf("the bridge needs NET_ADMIN for netlink multicasts, got %v", caps)
	}
}

func TestPrivilegedFallback(t *testing.T) {
	cfg := testCfg()
	cfg.Privileged = true
	pod, err := BuildPod(testApp(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range pod.Spec.Containers {
		priv := c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged
		switch c.Name {
		case ContainerApp, ContainerWolf:
			if !priv {
				t.Fatalf("%s must be privileged in the fallback layout", c.Name)
			}
		default:
			if priv {
				t.Fatalf("%s must not be privileged", c.Name)
			}
		}
	}
	mounts := func(name string) map[string]corev1.VolumeMount {
		out := map[string]corev1.VolumeMount{}
		for _, c := range pod.Spec.Containers {
			if c.Name == name {
				for _, m := range c.VolumeMounts {
					out[m.MountPath] = m
				}
			}
		}
		return out
	}
	app, wolf, br := mounts(ContainerApp), mounts(ContainerWolf), mounts(ContainerBridge)
	if m, ok := app["/dev"]; !ok || m.Name != "dev" || m.ReadOnly {
		t.Fatalf("the privileged app needs the node's /dev (hotplugged nodes): %+v", app)
	}
	if _, ok := wolf["/dev/uinput"]; !ok {
		t.Fatal("privileged Wolf gets uinput as a hostPath")
	}
	if m, ok := br[bridge.HostDev]; !ok || m.Name != "dev" || m.ReadOnly {
		t.Fatal("the bridge must see the node's /dev read-write (node permissions)")
	}
	vols := map[string]corev1.Volume{}
	for _, v := range pod.Spec.Volumes {
		vols[v.Name] = v
	}
	if v := vols["dev"]; v.HostPath == nil || v.HostPath.Path != "/dev" {
		t.Fatalf("dev volume: %+v", v)
	}
	// Default: neither privileged nor a device plugin, i.e. a misconfigured
	// chart; the pod still builds, without devices.
	plain, err := BuildPod(testApp(), testCfg())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range plain.Spec.Containers {
		if c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
			t.Fatalf("%s privileged by default", c.Name)
		}
	}
}
