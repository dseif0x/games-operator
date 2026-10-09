package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"

	"github.com/dseif0x/games-operator/internal/auth"
	"github.com/dseif0x/games-operator/internal/bridge"
	"github.com/dseif0x/games-operator/internal/k8s"
	"github.com/dseif0x/games-operator/internal/store"
	"github.com/dseif0x/games-operator/internal/wolf"
)

// Orchestrator is the seam between the app service and whatever creates
// the pods.
type Orchestrator interface {
	// Notify asks for the app to be converged soon.
	Notify(appID string)
	// PodLogs returns the tail of one container's logs.
	PodLogs(ctx context.Context, appID, container string, tailLines int64) ([]byte, error)
	// BridgeStatus asks the running pod's bridge whether a client streams.
	BridgeStatus(ctx context.Context, appID string) (bridge.Status, error)
}

// Notifier is told about every state change so the API can fan it out.
type Notifier interface {
	AppChanged(ctx context.Context, a *store.App)
	AppDeleted(ctx context.Context, appID, ownerID string)
}

// NopNotifier ignores notifications.
type NopNotifier struct{}

func (NopNotifier) AppChanged(context.Context, *store.App)     {}
func (NopNotifier) AppDeleted(context.Context, string, string) {}

// WolfClientFunc builds a Wolf API client for a pod; swapped in tests.
type WolfClientFunc func(baseURL, token string) WolfAPI

// WolfAPI is the subset of the Wolf client the reconciler uses.
type WolfAPI interface {
	Apps(ctx context.Context) ([]wolf.App, error)
	ListSessions(ctx context.Context) ([]wolf.RunningSession, error)
	StopSession(ctx context.Context, sessionID string) error
}

// WolfMoonlight is Wolf's own Moonlight HTTPS side, which the hub uses as
// a paired client to launch and resume streams (see wolf.Moonlight).
type WolfMoonlight interface {
	Launch(ctx context.Context, p wolf.LaunchParams) (string, error)
	Cancel(ctx context.Context) error
}

// MoonlightClientFunc builds the HTTPS client for one Wolf.
type MoonlightClientFunc func(baseURL string) WolfMoonlight

// Reconciler is a level-triggered controller for app objects.
type Reconciler struct {
	cfg      Config
	store    store.Store
	cs       kubernetes.Interface
	inf      *k8s.Informers
	notifier Notifier
	log      *slog.Logger
	queue    workqueue.TypedRateLimitingInterface[string]
	interval time.Duration
	now      func() time.Time
	wolfFor  WolfClientFunc
	mlFor    MoonlightClientFunc
	http     *http.Client

	startOnce sync.Once
}

// New wires a reconciler. Run must be called to process work.
func New(cfg Config, st store.Store, cs kubernetes.Interface, inf *k8s.Informers, notifier Notifier, interval time.Duration, log *slog.Logger) *Reconciler {
	if notifier == nil {
		notifier = NopNotifier{}
	}
	r := &Reconciler{
		cfg: cfg.Defaults(), store: st, cs: cs, inf: inf, notifier: notifier, log: log,
		queue:    workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		interval: interval, now: time.Now,
		wolfFor: func(baseURL, token string) WolfAPI { return wolf.NewClient(baseURL, token) },
		http:    &http.Client{Timeout: 5 * time.Second},
	}
	r.mlFor = func(baseURL string) WolfMoonlight {
		return wolf.NewMoonlight(baseURL, "games-operator", r.cfg.ClientCert)
	}
	if r.interval == 0 {
		r.interval = 30 * time.Second
	}
	inf.OnChange(r.Notify)
	return r
}

// Notify implements Orchestrator.
func (r *Reconciler) Notify(appID string) { r.queue.Add(appID) }

// Run processes the queue until ctx is done. The informers must be synced
// before this is called.
func (r *Reconciler) Run(ctx context.Context) {
	r.startOnce.Do(func() {
		go func() {
			<-ctx.Done()
			r.queue.ShutDown()
		}()
		go r.tick(ctx)
		for i := 0; i < 2; i++ {
			go r.worker(ctx)
		}
	})
	<-ctx.Done()
}

func (r *Reconciler) tick(ctx context.Context) {
	r.ReconcileAll(ctx)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.ReconcileAll(ctx)
		}
	}
}

func (r *Reconciler) worker(ctx context.Context) {
	for {
		id, shutdown := r.queue.Get()
		if shutdown {
			return
		}
		if err := r.ReconcileOne(ctx, id); err != nil {
			r.log.Warn("reconcile failed", "app", id, "err", err)
			r.queue.AddRateLimited(id)
		} else {
			r.queue.Forget(id)
		}
		r.queue.Done(id)
	}
}

// ReconcileAll enqueues every row and every labelled object, so orphans
// (objects without a row) get visited too.
func (r *Reconciler) ReconcileAll(ctx context.Context) {
	ids := map[string]struct{}{}
	rows, err := r.store.Apps().ListAll(ctx)
	if err != nil {
		r.log.Warn("list apps failed", "err", err)
	}
	for _, a := range rows {
		ids[a.ID] = struct{}{}
	}
	sel := labels.Everything()
	if pods, err := r.inf.Pods.Pods(r.cfg.Namespace).List(sel); err == nil {
		for _, p := range pods {
			ids[p.Labels[k8s.LabelApp]] = struct{}{}
		}
	}
	if pvcs, err := r.inf.PVCs.PersistentVolumeClaims(r.cfg.Namespace).List(sel); err == nil {
		for _, p := range pvcs {
			ids[p.Labels[k8s.LabelApp]] = struct{}{}
		}
	}
	if secrets, err := r.inf.Secrets.Secrets(r.cfg.Namespace).List(sel); err == nil {
		for _, s := range secrets {
			ids[s.Labels[k8s.LabelApp]] = struct{}{}
		}
	}
	if svcs, err := r.inf.Services.Services(r.cfg.Namespace).List(sel); err == nil {
		for _, s := range svcs {
			ids[s.Labels[k8s.LabelApp]] = struct{}{}
		}
	}
	delete(ids, "")
	for id := range ids {
		r.queue.Add(id)
	}
}

// observed is what exists in the cluster for one app.
type observed struct {
	pod    *corev1.Pod
	pvc    *corev1.PersistentVolumeClaim
	secret *corev1.Secret
	svc    *corev1.Service
}

func (r *Reconciler) observe(id string) observed {
	name := ObjectName(id)
	var o observed
	if p, err := r.inf.Pods.Pods(r.cfg.Namespace).Get(name); err == nil {
		o.pod = p
	}
	if p, err := r.inf.PVCs.PersistentVolumeClaims(r.cfg.Namespace).Get(name); err == nil {
		o.pvc = p
	}
	if s, err := r.inf.Secrets.Secrets(r.cfg.Namespace).Get(name); err == nil {
		o.secret = s
	}
	if s, err := r.inf.Services.Services(r.cfg.Namespace).Get(name); err == nil {
		o.svc = s
	}
	return o
}

// requeueSoon asks for another pass once the informer cache catches up.
func (r *Reconciler) requeueSoon(id string) { r.queue.AddAfter(id, 2*time.Second) }

// ReconcileOne converges a single app. It is exported for tests.
func (r *Reconciler) ReconcileOne(ctx context.Context, id string) error {
	app, err := r.store.Apps().Get(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	o := r.observe(id)
	if app == nil {
		return r.reconcileOrphan(ctx, id, o)
	}
	switch app.State {
	case store.StateStarting:
		return r.reconcileStarting(ctx, app, o)
	case store.StateRunning:
		return r.reconcileRunning(ctx, app, o)
	case store.StateStopping:
		return r.reconcileStopping(ctx, app, o)
	case store.StateStopped:
		return r.reconcileStopped(ctx, app, o)
	case store.StateFailed:
		// Free the shared IP's ports at once; keep the pod for its logs for
		// a while, then let go of it (and with it the GPU and the node).
		if o.svc != nil && o.svc.DeletionTimestamp == nil {
			return r.deleteService(ctx, app.ID)
		}
		if o.pod != nil && o.pod.DeletionTimestamp == nil {
			if left := r.cfg.FailedPodGrace - r.now().Sub(app.UpdatedAt); left > 0 {
				r.queue.AddAfter(app.ID, left)
				return nil
			}
			r.event(ctx, app, "pod", "deleted after failure")
			return r.deletePod(ctx, app.ID)
		}
		return nil
	case store.StateDeleting:
		return r.reconcileDeleting(ctx, app, o)
	default:
		return fmt.Errorf("unknown state %q", app.State)
	}
}

func (r *Reconciler) reconcileOrphan(ctx context.Context, id string, o observed) error {
	cutoff := r.now().Add(-r.cfg.OrphanGrace)
	old := func(t metav1.Time) bool { return t.Time.Before(cutoff) }
	name := ObjectName(id)
	ns := r.cfg.Namespace
	if o.pod != nil && old(o.pod.CreationTimestamp) && o.pod.DeletionTimestamp == nil {
		r.log.Warn("deleting orphaned pod", "pod", name)
		if err := r.deletePod(ctx, id); err != nil {
			return err
		}
	}
	if o.svc != nil && old(o.svc.CreationTimestamp) && o.svc.DeletionTimestamp == nil {
		r.log.Warn("deleting orphaned service", "service", name)
		if err := r.deleteService(ctx, id); err != nil {
			return err
		}
	}
	if o.pvc != nil && old(o.pvc.CreationTimestamp) && o.pvc.DeletionTimestamp == nil {
		r.log.Warn("deleting orphaned pvc", "pvc", name)
		if err := ignoreNotFound(r.cs.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
			return err
		}
	}
	if o.secret != nil && old(o.secret.CreationTimestamp) && o.secret.DeletionTimestamp == nil {
		r.log.Warn("deleting orphaned secret", "secret", name)
		if err := ignoreNotFound(r.cs.CoreV1().Secrets(ns).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) reconcileStarting(ctx context.Context, app *store.App, o observed) error {
	if app.Slot < 0 {
		return r.fail(ctx, app, "no stream slot assigned")
	}
	ns := r.cfg.Namespace
	created := false
	if o.pvc == nil {
		pvc := BuildPVC(app, r.cfg)
		if _, err := r.cs.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return r.fail(ctx, app, "create pvc: "+err.Error())
		}
		r.event(ctx, app, "pvc", "created "+pvc.Name)
		created = true
	}
	gen := strconv.Itoa(app.Generation)
	if o.secret == nil {
		token, err := auth.NewToken()
		if err != nil {
			return err
		}
		if _, err := r.cs.CoreV1().Secrets(ns).Create(ctx, BuildSecret(app, r.cfg, token), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return r.fail(ctx, app, "create secret: "+err.Error())
		}
		r.event(ctx, app, "secret", "bridge token issued")
		created = true
	}
	if o.svc == nil {
		svc := BuildService(app, r.cfg)
		if _, err := r.cs.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return r.fail(ctx, app, "create service: "+err.Error())
		}
		r.event(ctx, app, "service", fmt.Sprintf("created %s (slot %d, ports %d-%d)", svc.Name, app.Slot, Ports(r.cfg.StreamPortBase, app.Slot).RTSP, Ports(r.cfg.StreamPortBase, app.Slot).Audio))
		created = true
	} else if o.svc.Spec.Ports[0].Port != Ports(r.cfg.StreamPortBase, app.Slot).RTSP {
		// Slot changed since the Service was made (a restart on a different
		// slot): replace it.
		if err := r.deleteService(ctx, app.ID); err != nil {
			return err
		}
		r.requeueSoon(app.ID)
		return nil
	}
	if o.pod != nil {
		switch {
		case o.pod.DeletionTimestamp != nil:
			r.requeueSoon(app.ID)
			return nil
		case o.pod.Annotations[k8s.AnnotationGeneration] != gen || PodTerminal(o.pod):
			if err := r.deletePod(ctx, app.ID); err != nil {
				return err
			}
			r.requeueSoon(app.ID)
			return nil
		case PodReady(o.pod):
			return r.ensureStream(ctx, app, o)
		default:
			reason := PodReason(o.pod)
			if r.timedOut(app) {
				return r.fail(ctx, app, "pod not ready after "+r.cfg.StartingTimeout.String()+": "+reason)
			}
			if reason != app.StateReason {
				return r.setState(ctx, app, store.StateStarting, reason)
			}
			return nil
		}
	}
	if created {
		r.requeueSoon(app.ID)
		return nil
	}
	pod, err := BuildPod(app, r.cfg)
	if err != nil {
		return r.fail(ctx, app, err.Error())
	}
	if _, err := r.cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			r.requeueSoon(app.ID)
			return nil
		}
		return r.fail(ctx, app, "create pod: "+err.Error())
	}
	r.event(ctx, app, "pod", "created "+pod.Name)
	r.k8sEvent(ctx, pod.Name, "Created", "games-operator created app pod")
	return nil
}

// timedOut reports whether the app has been starting for too long.
func (r *Reconciler) timedOut(app *store.App) bool {
	started := app.UpdatedAt
	if app.Stream != nil {
		if t, err := time.Parse(time.RFC3339, app.Stream.StartedAt); err == nil {
			started = t
		}
	}
	return r.now().Sub(started) > r.cfg.StartingTimeout
}

// WarmReason is the state reason of a running app that has no client yet.
const WarmReason = "ready, waiting for a Moonlight client"

// ensureStream registers the Moonlight stream with Wolf once the pod is
// ready and the Service has its address, then marks the app running.
func (r *Reconciler) ensureStream(ctx context.Context, app *store.App, o observed) error {
	if app.WolfSessionID != "" && app.StreamURL != "" {
		if app.State != store.StateRunning {
			return r.setState(ctx, app, store.StateRunning, "")
		}
		return nil
	}
	// The stream ports are reachable once the Service has its address.
	ip := r.cfg.LBIP
	if ip == "" {
		ip = ServiceIP(o.svc)
	}
	if ip == "" {
		if r.timedOut(app) {
			return r.fail(ctx, app, "service got no LoadBalancer IP in "+r.cfg.StartingTimeout.String())
		}
		if app.StateReason != "waiting for LoadBalancer IP" {
			return r.setState(ctx, app, app.State, "waiting for LoadBalancer IP")
		}
		r.requeueSoon(app.ID)
		return nil
	}
	if app.Stream == nil {
		// Started without a client: the pod is up and Wolf waits. A launch
		// later sets the stream parameters and lands here again.
		if app.State != store.StateRunning || app.StateReason != WarmReason {
			return r.setState(ctx, app, store.StateRunning, WarmReason)
		}
		return nil
	}
	client, err := r.wolfClient(o)
	if err != nil {
		return err
	}
	// The stream is started through Wolf's own Moonlight HTTPS side, with
	// the hub as the paired client: a first launch creates the session, a
	// launch for a client that already has one is a resume, and Wolf then
	// keeps the compositor and the input devices, so the app survives the
	// client's new keys. The RTSP URL Wolf answers carries a per-session
	// marker as its host; the client sends it in every RTSP request, which
	// is how Wolf finds the session behind our shared address.
	apps, err := client.Apps(ctx)
	if err != nil || len(apps) == 0 {
		if err == nil {
			err = errors.New("wolf lists no app")
		}
		if r.timedOut(app) {
			return r.fail(ctx, app, "wolf: "+err.Error())
		}
		return fmt.Errorf("wolf apps: %w", err)
	}
	ml := r.mlFor(fmt.Sprintf("https://%s:%d", o.pod.Status.PodIP, r.cfg.WolfHTTPSPort))
	url, err := ml.Launch(ctx, wolf.LaunchParams{
		AppID: apps[0].ID, AESKey: app.Stream.AESKey, AESIV: app.Stream.AESIV,
		Width: app.Stream.Width, Height: app.Stream.Height, FPS: app.Stream.FPS, Surround: app.Stream.Surround,
	})
	if err != nil {
		if r.timedOut(app) {
			return r.fail(ctx, app, "wolf: "+err.Error())
		}
		return fmt.Errorf("wolf launch: %w", err)
	}
	id := "moonlight"
	if sessions, err := client.ListSessions(ctx); err == nil && len(sessions) > 0 {
		id = sessions[len(sessions)-1].ClientID
	}
	updated, err := r.store.Apps().SetRuntime(ctx, app.ID, id, url)
	if err != nil {
		return err
	}
	r.event(ctx, app, "stream", "wolf session "+id+" at "+url)
	return r.setState(ctx, updated, store.StateRunning, "")
}

func (r *Reconciler) wolfClient(o observed) (WolfAPI, error) {
	if o.pod == nil || o.pod.Status.PodIP == "" {
		return nil, errors.New("pod has no IP")
	}
	if o.secret == nil {
		return nil, errors.New("secret missing")
	}
	return r.wolfFor(fmt.Sprintf("http://%s:%d", o.pod.Status.PodIP, bridge.Port), string(o.secret.Data[bridge.EnvToken])), nil
}

func (r *Reconciler) reconcileRunning(ctx context.Context, app *store.App, o observed) error {
	switch {
	case o.pod == nil:
		return r.fail(ctx, app, "pod disappeared")
	case o.pod.DeletionTimestamp != nil:
		return r.fail(ctx, app, "pod was deleted outside games-operator")
	case PodTerminal(o.pod):
		return r.fail(ctx, app, PodReason(o.pod))
	case PodCrashing(o.pod):
		// Containers that cannot run any more (the GPU fell off the bus,
		// a node rebooted under the pod): this pod will not recover.
		return r.fail(ctx, app, PodReason(o.pod))
	case app.WolfSessionID == "":
		// Re-keyed by a resume: register the new stream once the pod is
		// ready again (the app container restarts after a compositor
		// rebuild). Not ready for as long as a start may take: give up.
		if !PodReady(o.pod) {
			if r.now().Sub(app.UpdatedAt) > r.cfg.StartingTimeout {
				return r.fail(ctx, app, "pod not ready after "+r.cfg.StartingTimeout.String()+": "+PodReason(o.pod))
			}
			r.requeueSoon(app.ID)
			return nil
		}
		return r.ensureStream(ctx, app, o)
	}
	return nil
}

func (r *Reconciler) reconcileStopping(ctx context.Context, app *store.App, o observed) error {
	pending := false
	if o.pod != nil {
		pending = true
		if o.pod.DeletionTimestamp == nil {
			if err := r.deletePod(ctx, app.ID); err != nil {
				return err
			}
		}
	}
	if o.svc != nil {
		pending = true
		if o.svc.DeletionTimestamp == nil {
			if err := r.deleteService(ctx, app.ID); err != nil {
				return err
			}
		}
	}
	if pending {
		r.requeueSoon(app.ID)
		return nil
	}
	cleared, err := r.store.Apps().ClearRuntime(ctx, app.ID)
	if err != nil {
		return err
	}
	return r.setState(ctx, cleared, store.StateStopped, "")
}

func (r *Reconciler) reconcileStopped(ctx context.Context, app *store.App, o observed) error {
	if o.pod != nil && o.pod.DeletionTimestamp == nil {
		return r.deletePod(ctx, app.ID)
	}
	if o.svc != nil && o.svc.DeletionTimestamp == nil {
		return r.deleteService(ctx, app.ID)
	}
	return nil
}

func (r *Reconciler) reconcileDeleting(ctx context.Context, app *store.App, o observed) error {
	name := ObjectName(app.ID)
	ns := r.cfg.Namespace
	pending := false
	del := func(exists bool, terminating bool, fn func() error) error {
		if !exists {
			return nil
		}
		pending = true
		if terminating {
			return nil
		}
		return fn()
	}
	if err := del(o.pod != nil, o.pod != nil && o.pod.DeletionTimestamp != nil, func() error { return r.deletePod(ctx, app.ID) }); err != nil {
		return err
	}
	if err := del(o.svc != nil, o.svc != nil && o.svc.DeletionTimestamp != nil, func() error { return r.deleteService(ctx, app.ID) }); err != nil {
		return err
	}
	if err := del(o.pvc != nil, o.pvc != nil && o.pvc.DeletionTimestamp != nil, func() error {
		return ignoreNotFound(r.cs.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, name, metav1.DeleteOptions{}))
	}); err != nil {
		return err
	}
	if err := del(o.secret != nil, o.secret != nil && o.secret.DeletionTimestamp != nil, func() error {
		return ignoreNotFound(r.cs.CoreV1().Secrets(ns).Delete(ctx, name, metav1.DeleteOptions{}))
	}); err != nil {
		return err
	}
	if pending {
		r.requeueSoon(app.ID)
		return nil
	}
	if err := r.store.Apps().Delete(ctx, app.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	r.log.Info("app deleted", "app", app.ID)
	r.notifier.AppDeleted(ctx, app.ID, app.OwnerID)
	return nil
}

func (r *Reconciler) deletePod(ctx context.Context, id string) error {
	err := r.cs.CoreV1().Pods(r.cfg.Namespace).Delete(ctx, ObjectName(id), metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](15)})
	return ignoreNotFound(err)
}

func (r *Reconciler) deleteService(ctx context.Context, id string) error {
	return ignoreNotFound(r.cs.CoreV1().Services(r.cfg.Namespace).Delete(ctx, ObjectName(id), metav1.DeleteOptions{}))
}

func (r *Reconciler) fail(ctx context.Context, app *store.App, reason string) error {
	r.log.Warn("app failed", "app", app.ID, "reason", reason)
	if _, err := r.store.Apps().ClearRuntime(ctx, app.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return r.setState(ctx, app, store.StateFailed, reason)
}

// setState writes the transition, records an event and notifies listeners.
func (r *Reconciler) setState(ctx context.Context, app *store.App, state, reason string) error {
	if app.State == state && app.StateReason == reason {
		return nil
	}
	updated, err := r.store.Apps().SetState(ctx, app.ID, state, reason)
	if err != nil {
		return err
	}
	msg := app.State + " → " + state
	if reason != "" {
		msg += ": " + reason
	}
	r.log.Info("app state", "app", app.ID, "from", app.State, "to", state, "reason", reason)
	r.event(ctx, app, "state", msg)
	r.notifier.AppChanged(ctx, updated)
	return nil
}

func (r *Reconciler) event(ctx context.Context, app *store.App, kind, msg string) {
	if err := r.store.Events().Add(ctx, app.ID, kind, msg); err != nil {
		r.log.Debug("record event failed", "err", err)
		return
	}
	_ = r.store.Events().Prune(ctx, app.ID, store.EventsKeep)
}

// k8sEvent posts a Kubernetes Event on a pod, best effort.
func (r *Reconciler) k8sEvent(ctx context.Context, podName, reason, msg string) {
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{GenerateName: podName + ".", Namespace: r.cfg.Namespace},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: r.cfg.Namespace, Name: podName, APIVersion: "v1"},
		Reason:         reason, Message: msg, Type: corev1.EventTypeNormal,
		Source:         corev1.EventSource{Component: "games-operator"},
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}
	if _, err := r.cs.CoreV1().Events(r.cfg.Namespace).Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		r.log.Debug("post kubernetes event failed", "err", err)
	}
}

// PodLogs implements Orchestrator.
func (r *Reconciler) PodLogs(ctx context.Context, appID, container string, tailLines int64) ([]byte, error) {
	if tailLines <= 0 {
		tailLines = 500
	}
	if container == "" {
		container = ContainerApp
	}
	req := r.cs.CoreV1().Pods(r.cfg.Namespace).GetLogs(ObjectName(appID), &corev1.PodLogOptions{Container: container, TailLines: ptr.To(tailLines)})
	rc, err := req.Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 4<<20))
}

// BridgeStatus implements Orchestrator.
func (r *Reconciler) BridgeStatus(ctx context.Context, appID string) (bridge.Status, error) {
	o := r.observe(appID)
	if o.pod == nil || o.pod.Status.PodIP == "" || o.secret == nil {
		return bridge.Status{}, errors.New("pod not running")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d/status", o.pod.Status.PodIP, bridge.Port), nil)
	if err != nil {
		return bridge.Status{}, err
	}
	req.Header.Set("Authorization", "Bearer "+string(o.secret.Data[bridge.EnvToken]))
	res, err := r.http.Do(req)
	if err != nil {
		return bridge.Status{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return bridge.Status{}, fmt.Errorf("bridge status: %s", res.Status)
	}
	var st bridge.Status
	if err := decodeJSON(res.Body, &st); err != nil {
		return bridge.Status{}, err
	}
	return st, nil
}

func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

var _ Orchestrator = (*Reconciler)(nil)
