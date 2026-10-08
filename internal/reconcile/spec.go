// Package reconcile turns app rows into Kubernetes objects and keeps them
// converged. The spec builders in this file are pure functions so they can
// be unit-tested without a cluster.
package reconcile

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/dseif0x/games-operator/internal/bridge"
	"github.com/dseif0x/games-operator/internal/config"
	"github.com/dseif0x/games-operator/internal/k8s"
	"github.com/dseif0x/games-operator/internal/preset"
	"github.com/dseif0x/games-operator/internal/store"
)

// Config is what the reconciler needs from the hub configuration.
type Config struct {
	Namespace       string
	WolfImage       string
	InitImage       string
	BridgeImage     string // image:tag
	ImagePullPolicy string
	RuntimeClass    string
	WolfGPURequest  bool
	UinputResource  string
	RenderNode      string
	TimeZone        string
	// MoonlightHostname is written into Wolf's config.
	MoonlightHostname string

	DefaultStorageClass string
	DefaultPVCSize      string
	DefaultResources    config.Resources
	MaxResources        config.Resources
	NodeSelector        map[string]string
	Tolerations         []config.Toleration
	ExtraEnv            map[string]string

	LBSharingKey   string
	LBIP           string
	StreamPortBase int
	MaxConcurrent  int

	// OrphanGrace is how old a labelled object without a row must be
	// before it is deleted.
	OrphanGrace time.Duration
	// StartingTimeout marks an app failed when its stream is not up in time
	// (the GPU node may have to boot first).
	StartingTimeout time.Duration
}

// Defaults fills unset fields.
func (c Config) Defaults() Config {
	if c.OrphanGrace == 0 {
		c.OrphanGrace = 2 * time.Minute
	}
	if c.StartingTimeout == 0 {
		c.StartingTimeout = 10 * time.Minute
	}
	if c.ImagePullPolicy == "" {
		c.ImagePullPolicy = "IfNotPresent"
	}
	if c.StreamPortBase == 0 {
		c.StreamPortBase = 48100
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 10
	}
	if c.TimeZone == "" {
		c.TimeZone = "Etc/UTC"
	}
	return c
}

// Container names in a session pod.
const (
	ContainerApp    = "app"
	ContainerWolf   = "wolf"
	ContainerBridge = "wolf-bridge"
	ContainerInit   = "init"

	// RuntimeDir is the shared XDG_RUNTIME_DIR with Wolf's sockets.
	RuntimeDir = "/tmp/.X11-unix"
	// HomeDir is where the app's PVC is mounted.
	HomeDir = "/home/retro"
	// WolfDir holds Wolf's socket and config inside the pod.
	WolfDir = "/etc/wolf"
	// SecretKeyConfig is the key of config.toml in the app Secret.
	SecretKeyConfig = "config.toml"
	// AppUID is the user GOW images drop to.
	AppUID int64 = 1000
)

// PortSet is one app's stream ports on the shared LoadBalancer IP.
type PortSet struct {
	RTSP    int32
	Control int32
	Video   int32
	Audio   int32
}

// Ports returns the port set of a slot: base+10*slot (RTSP, TCP), +1
// (control, UDP), +2 (video, UDP), +3 (audio, UDP).
func Ports(base, slot int) PortSet {
	p := int32(base + 10*slot) //nolint:gosec // bounded by config validation
	return PortSet{RTSP: p, Control: p + 1, Video: p + 2, Audio: p + 3}
}

// ObjectName is the name shared by an app's PVC, Secret, Service and Pod.
func ObjectName(appID string) string { return "games-operator-" + appID }

// AppIDFromName is the inverse of ObjectName.
func AppIDFromName(name string) (string, bool) { return strings.CutPrefix(name, "games-operator-") }

// Labels returns the labels every per-app object carries.
func Labels(a *store.App) map[string]string {
	return map[string]string{
		k8s.LabelApp:       a.ID,
		k8s.LabelOwner:     a.OwnerID,
		k8s.LabelManagedBy: k8s.ManagedBy,
		k8s.LabelName:      "games-operator-app",
		k8s.LabelComponent: "app",
	}
}

// BuildPVC returns the desired PersistentVolumeClaim (the app's home).
func BuildPVC(a *store.App, cfg Config) *corev1.PersistentVolumeClaim {
	size := a.PVCSize
	if size == "" {
		size = cfg.DefaultPVCSize
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: ObjectName(a.ID), Namespace: cfg.Namespace, Labels: Labels(a)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
	sc := a.StorageClass
	if sc == "" {
		sc = cfg.DefaultStorageClass
	}
	if sc != "" {
		pvc.Spec.StorageClassName = ptr.To(sc)
	}
	return pvc
}

// BuildSecret returns the per-app Secret: the bridge token and Wolf's
// config.toml, both tied to the generation.
func BuildSecret(a *store.App, cfg Config, token string, wolfConfig []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: ObjectName(a.ID), Namespace: cfg.Namespace, Labels: Labels(a),
			Annotations: map[string]string{k8s.AnnotationGeneration: strconv.Itoa(a.Generation)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{bridge.EnvToken: []byte(token), SecretKeyConfig: wolfConfig},
	}
}

// BuildService returns the LoadBalancer Service that exposes the app's
// stream ports on the IP shared with the Moonlight front door.
func BuildService(a *store.App, cfg Config) *corev1.Service {
	ports := Ports(cfg.StreamPortBase, a.Slot)
	ann := map[string]string{
		"metallb.io/allow-shared-ip":   cfg.LBSharingKey,
		"lbipam.cilium.io/sharing-key": cfg.LBSharingKey,
	}
	if cfg.LBIP != "" {
		ann["metallb.io/loadBalancerIPs"] = cfg.LBIP
		ann["lbipam.cilium.io/ips"] = cfg.LBIP
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: ObjectName(a.ID), Namespace: cfg.Namespace, Labels: Labels(a), Annotations: ann},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{k8s.LabelApp: a.ID},
			Ports: []corev1.ServicePort{
				{Name: "rtsp", Protocol: corev1.ProtocolTCP, Port: ports.RTSP, TargetPort: intstr.FromInt32(ports.RTSP)},
				{Name: "control", Protocol: corev1.ProtocolUDP, Port: ports.Control, TargetPort: intstr.FromInt32(ports.Control)},
				{Name: "video", Protocol: corev1.ProtocolUDP, Port: ports.Video, TargetPort: intstr.FromInt32(ports.Video)},
				{Name: "audio", Protocol: corev1.ProtocolUDP, Port: ports.Audio, TargetPort: intstr.FromInt32(ports.Audio)},
			},
		},
	}
}

// ServiceIP returns the Service's LoadBalancer address, or "" if none yet.
func ServiceIP(svc *corev1.Service) string {
	if svc == nil {
		return ""
	}
	for _, in := range svc.Status.LoadBalancer.Ingress {
		if in.IP != "" {
			return in.IP
		}
		if in.Hostname != "" {
			return in.Hostname
		}
	}
	return ""
}

// ClampResources turns an app's wishes into pod resources: every limit is
// the app's value if given, else the default, and never more than the
// maximum (a maximum left empty falls back to the default, which then
// doubles as the cap). Requests never exceed limits. Extended resources
// (nvidia.com/gpu …) get request = limit, as Kubernetes requires.
func ClampResources(want, def, ceiling config.Resources) corev1.ResourceRequirements {
	limitCPU := minQty(firstNonEmpty(want.Limits.CPU, def.Limits.CPU), firstNonEmpty(ceiling.Limits.CPU, def.Limits.CPU))
	limitMem := minQty(firstNonEmpty(want.Limits.Memory, def.Limits.Memory), firstNonEmpty(ceiling.Limits.Memory, def.Limits.Memory))
	reqCPU := minQty(firstNonEmpty(want.Requests.CPU, def.Requests.CPU), limitCPU)
	reqMem := minQty(firstNonEmpty(want.Requests.Memory, def.Requests.Memory), limitMem)
	out := corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}
	set := func(list corev1.ResourceList, name corev1.ResourceName, v string) {
		if v == "" {
			return
		}
		if q, err := resource.ParseQuantity(v); err == nil {
			list[name] = q
		}
	}
	set(out.Requests, corev1.ResourceCPU, reqCPU)
	set(out.Requests, corev1.ResourceMemory, reqMem)
	set(out.Limits, corev1.ResourceCPU, limitCPU)
	set(out.Limits, corev1.ResourceMemory, limitMem)
	names := map[string]bool{}
	for name := range want.Limits.Extended {
		names[name] = true
	}
	for name := range def.Limits.Extended {
		names[name] = true
	}
	for name := range names {
		v := minQty(firstNonEmpty(want.Limits.Extended[name], def.Limits.Extended[name]),
			firstNonEmpty(ceiling.Limits.Extended[name], def.Limits.Extended[name]))
		q, err := resource.ParseQuantity(v)
		if err != nil || q.Sign() <= 0 {
			continue
		}
		out.Limits[corev1.ResourceName(name)] = q
		out.Requests[corev1.ResourceName(name)] = q
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// minQty returns the smaller of two quantities; an empty or unparsable
// side yields the other.
func minQty(a, b string) string {
	qa, errA := resource.ParseQuantity(a)
	qb, errB := resource.ParseQuantity(b)
	switch {
	case errA != nil && errB != nil:
		return ""
	case errA != nil:
		return b
	case errB != nil:
		return a
	}
	if qa.Cmp(qb) <= 0 {
		return a
	}
	return b
}

func small(reqCPU, reqMem, limCPU, limMem string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(reqCPU), corev1.ResourceMemory: resource.MustParse(reqMem)},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(limCPU), corev1.ResourceMemory: resource.MustParse(limMem)},
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func envList(m map[string]string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, corev1.EnvVar{Name: k, Value: m[k]})
	}
	return out
}

// Preset resolves the app's preset, falling back to custom.
func Preset(a *store.App) preset.Preset {
	if p, ok := preset.Get(a.Preset); ok {
		return p
	}
	p, _ := preset.Get("custom")
	return p
}

// BuildPod returns the desired Pod: the app container next to Wolf (which
// brings its own PulseAudio) and the bridge, sharing one runtime directory.
//
// Game containers run as root, share the host IPC namespace, see
// /dev/input and /dev/uinput and ask for capabilities; the namespace must
// allow that (Pod Security "privileged"). The preset decides how far an
// app goes; nothing here is privileged in the Kubernetes sense.
func BuildPod(a *store.App, cfg Config) *corev1.Pod {
	cfg = cfg.Defaults()
	p := Preset(a)
	ports := Ports(cfg.StreamPortBase, a.Slot)
	name := ObjectName(a.ID)
	pull := corev1.PullPolicy(cfg.ImagePullPolicy)

	// App environment: preset, chart extras, then the app's own.
	appEnv := map[string]string{
		"XDG_RUNTIME_DIR": RuntimeDir, "WAYLAND_DISPLAY": "wayland-1", "DISPLAY": ":0",
		"PULSE_SERVER": "unix:" + RuntimeDir + "/pulse-socket",
		"UNAME":        "retro", "PUID": strconv.FormatInt(AppUID, 10), "PGID": strconv.FormatInt(AppUID, 10),
		"HOME": HomeDir, "TZ": cfg.TimeZone,
		"NVIDIA_DRIVER_CAPABILITIES": "all", "NVIDIA_VISIBLE_DEVICES": "all",
		"GAMESCOPE_WIDTH": "1920", "GAMESCOPE_HEIGHT": "1080", "GAMESCOPE_REFRESH": "60",
	}
	if a.Stream != nil && a.Stream.Width > 0 {
		appEnv["GAMESCOPE_WIDTH"] = strconv.Itoa(a.Stream.Width)
		appEnv["GAMESCOPE_HEIGHT"] = strconv.Itoa(a.Stream.Height)
		appEnv["GAMESCOPE_REFRESH"] = strconv.Itoa(a.Stream.FPS)
	}
	for k, v := range p.Env {
		appEnv[k] = v
	}
	for k, v := range cfg.ExtraEnv {
		appEnv[k] = v
	}
	for k, v := range a.Env {
		appEnv[k] = v
	}

	caps := append([]string{}, p.Capabilities...)
	if len(a.Capabilities) > 0 {
		caps = a.Capabilities
	}
	capList := make([]corev1.Capability, 0, len(caps))
	for _, c := range caps {
		capList = append(capList, corev1.Capability(c))
	}
	appSec := &corev1.SecurityContext{
		RunAsUser:                ptr.To[int64](0),
		RunAsGroup:               ptr.To[int64](0),
		AllowPrivilegeEscalation: ptr.To(true),
		Capabilities:             &corev1.Capabilities{Add: capList},
	}
	if p.Unconfined {
		appSec.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
		appSec.AppArmorProfile = &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}
	}
	command := a.Command
	if command == "" {
		command = preset.DefaultCommand
	}
	appResources := ClampResources(a.Resources, cfg.DefaultResources, cfg.MaxResources)
	if cfg.UinputResource != "" {
		q := resource.MustParse("1")
		appResources.Limits[corev1.ResourceName(cfg.UinputResource)] = q
		appResources.Requests[corev1.ResourceName(cfg.UinputResource)] = q
	}
	appMounts := []corev1.VolumeMount{
		{Name: "runtime", MountPath: RuntimeDir},
		{Name: "home", MountPath: HomeDir},
		{Name: "input", MountPath: "/dev/input"},
		{Name: "shm", MountPath: "/dev/shm"},
	}
	if cfg.UinputResource == "" {
		appMounts = append(appMounts, corev1.VolumeMount{Name: "uinput", MountPath: "/dev/uinput"})
	}

	wolfEnv := map[string]string{
		// No PULSE_SERVER: Wolf then runs its own PulseAudio (supervisord in
		// the image) with the socket in XDG_RUNTIME_DIR, which the app
		// container shares and points PULSE_SERVER at.
		"XDG_RUNTIME_DIR":            RuntimeDir,
		"HOST_APPS_STATE_FOLDER":     "/mnt/data/wolf",
		bridge.EnvSocket:             bridge.DefaultSocket,
		"WOLF_CFG_FILE":              WolfDir + "/cfg/config.toml",
		"WOLF_PRIVATE_KEY_FILE":      WolfDir + "/cfg/key.pem",
		"WOLF_PRIVATE_CERT_FILE":     WolfDir + "/cfg/cert.pem",
		"WOLF_LOG_LEVEL":             "INFO",
		"WOLF_RTSP_SETUP_PORT":       strconv.Itoa(int(ports.RTSP)),
		"WOLF_CONTROL_PORT":          strconv.Itoa(int(ports.Control)),
		"WOLF_VIDEO_PING_PORT":       strconv.Itoa(int(ports.Video)),
		"WOLF_AUDIO_PING_PORT":       strconv.Itoa(int(ports.Audio)),
		"NVIDIA_DRIVER_CAPABILITIES": "all", "NVIDIA_VISIBLE_DEVICES": "all",
		"GST_DEBUG": "2", "TZ": cfg.TimeZone,
	}
	if cfg.RenderNode != "" {
		wolfEnv["WOLF_RENDER_NODE"] = cfg.RenderNode
	}
	wolfResources := small("500m", "1Gi", "4", "4Gi")
	if cfg.WolfGPURequest {
		q := resource.MustParse("1")
		wolfResources.Limits["nvidia.com/gpu"] = q
		wolfResources.Requests["nvidia.com/gpu"] = q
	}
	wolfMounts := []corev1.VolumeMount{
		{Name: "runtime", MountPath: RuntimeDir},
		{Name: "wolf", MountPath: WolfDir},
		{Name: "wolf-state", MountPath: "/mnt/data/wolf"},
		{Name: "input", MountPath: "/dev/input"},
		{Name: "uinput", MountPath: "/dev/uinput"},
	}

	nodeSelector := map[string]string{}
	for k, v := range cfg.NodeSelector {
		nodeSelector[k] = v
	}
	var tolerations []corev1.Toleration
	for _, t := range cfg.Tolerations {
		tolerations = append(tolerations, corev1.Toleration{
			Key: t.Key, Operator: corev1.TolerationOperator(t.Operator), Value: t.Value,
			Effect: corev1.TaintEffect(t.Effect), TolerationSeconds: t.TolerationSeconds,
		})
	}
	hostIPC := p.HostIPC || a.HostIPC

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: cfg.Namespace, Labels: Labels(a),
			Annotations: map[string]string{k8s.AnnotationGeneration: strconv.Itoa(a.Generation)},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  ptr.To(false),
			EnableServiceLinks:            ptr.To(false),
			TerminationGracePeriodSeconds: ptr.To[int64](30),
			HostIPC:                       hostIPC,
			NodeSelector:                  nodeSelector,
			Tolerations:                   tolerations,
			InitContainers: []corev1.Container{{
				// Wolf and the app share the runtime dir as different users;
				// the config comes from the Secret but Wolf wants to write
				// next to it (certificates), hence the copy.
				Name: ContainerInit, Image: cfg.InitImage, ImagePullPolicy: pull,
				Command: []string{"/bin/sh", "-ec", strings.Join([]string{
					"chown -R " + strconv.FormatInt(AppUID, 10) + ":" + strconv.FormatInt(AppUID, 10) + " " + RuntimeDir,
					"chmod 1777 " + RuntimeDir,
					"mkdir -p " + WolfDir + "/cfg",
					"cp /cfg/" + SecretKeyConfig + " " + WolfDir + "/cfg/config.toml",
					"chmod -R 777 " + WolfDir,
				}, "\n")},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "runtime", MountPath: RuntimeDir},
					{Name: "wolf", MountPath: WolfDir},
					{Name: "config", MountPath: "/cfg", ReadOnly: true},
				},
				Resources:       small("10m", "16Mi", "200m", "64Mi"),
				SecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To[int64](0), RunAsGroup: ptr.To[int64](0)},
			}},
			Containers: []corev1.Container{
				{
					Name: ContainerApp, Image: a.Image, ImagePullPolicy: pull,
					Command:         []string{"/bin/bash", "-c", command},
					Env:             envList(appEnv),
					Resources:       appResources,
					VolumeMounts:    appMounts,
					SecurityContext: appSec,
				},
				{
					Name: ContainerWolf, Image: cfg.WolfImage, ImagePullPolicy: pull,
					Env:          envList(wolfEnv),
					Resources:    wolfResources,
					VolumeMounts: wolfMounts,
					Ports: []corev1.ContainerPort{
						{Name: "rtsp", ContainerPort: ports.RTSP, Protocol: corev1.ProtocolTCP},
						{Name: "control", ContainerPort: ports.Control, Protocol: corev1.ProtocolUDP},
						{Name: "video", ContainerPort: ports.Video, Protocol: corev1.ProtocolUDP},
						{Name: "audio", ContainerPort: ports.Audio, Protocol: corev1.ProtocolUDP},
					},
					SecurityContext: &corev1.SecurityContext{
						RunAsUser: ptr.To[int64](0), RunAsGroup: ptr.To[int64](0),
						Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_RAW", "MKNOD", "NET_ADMIN", "SYS_ADMIN", "SYS_NICE"}},
					},
				},
				{
					Name: ContainerBridge, Image: cfg.BridgeImage, ImagePullPolicy: pull,
					Env: []corev1.EnvVar{
						{Name: bridge.EnvSocket, Value: bridge.DefaultSocket},
						{Name: bridge.EnvToken, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: bridge.EnvToken,
						}}},
					},
					Ports:        []corev1.ContainerPort{{Name: "bridge", ContainerPort: bridge.Port, Protocol: corev1.ProtocolTCP}},
					Resources:    small("10m", "32Mi", "200m", "64Mi"),
					VolumeMounts: []corev1.VolumeMount{{Name: "wolf", MountPath: WolfDir}},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(bridge.Port)}},
						InitialDelaySeconds: 2, PeriodSeconds: 3, FailureThreshold: 2,
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(bridge.Port)}},
						InitialDelaySeconds: 10, PeriodSeconds: 20, FailureThreshold: 3,
					},
					SecurityContext: &corev1.SecurityContext{
						RunAsUser: ptr.To[int64](0), RunAsGroup: ptr.To[int64](0),
						AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
						Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
				},
			},
			Volumes: []corev1.Volume{
				{Name: "runtime", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "wolf", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "wolf-state", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName: name, Items: []corev1.KeyToPath{{Key: SecretKeyConfig, Path: SecretKeyConfig}},
				}}},
				{Name: "home", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}}},
				{Name: "input", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/input", Type: ptr.To(corev1.HostPathDirectory)}}},
				{Name: "uinput", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/uinput", Type: ptr.To(corev1.HostPathCharDev)}}},
				{Name: "shm", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium: corev1.StorageMediumMemory, SizeLimit: ptr.To(resource.MustParse("4Gi")),
				}}},
			},
		},
	}
	if cfg.RuntimeClass != "" {
		pod.Spec.RuntimeClassName = ptr.To(cfg.RuntimeClass)
	}
	return pod
}

// PodReady reports whether the pod is Running with Ready=True (which
// means Wolf's socket is up, through the bridge's readiness probe).
func PodReady(pod *corev1.Pod) bool {
	if pod == nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// PodTerminal reports whether the pod has finished for good.
func PodTerminal(pod *corev1.Pod) bool {
	return pod != nil && (pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded)
}

// PodReason summarises why a pod is not ready, for the state_reason column.
func PodReason(pod *corev1.Pod) string {
	if pod == nil {
		return "pod missing"
	}
	if pod.DeletionTimestamp != nil {
		return "pod terminating"
	}
	for _, cs := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && cs.State.Waiting.Reason != "PodInitializing" {
			return fmt.Sprintf("%s %s: %s", cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
		}
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			return fmt.Sprintf("%s exited with code %d (%s)", cs.Name, cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return fmt.Sprintf("%s: %s", c.Reason, c.Message)
		}
	}
	if pod.Status.Reason != "" {
		return pod.Status.Reason + ": " + pod.Status.Message
	}
	return "pod " + strings.ToLower(string(pod.Status.Phase))
}
