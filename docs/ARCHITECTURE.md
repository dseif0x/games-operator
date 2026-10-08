# Architecture

## Components

```
                ┌──────────────────────── hub (1 replica) ────────────────────────┐
browser ──HTTPS─▶ web UI + REST API (:8080); /play proxies to the embedded moonlight-web with a trust header                                         │
Moonlight ─TCP──▶ Moonlight HTTP (:47989) / HTTPS + client cert (:47984)            │
moonlight-web ──▶ /wolf/api/v1/pair/* (bearer token)                                │
                │ reconciler ◀── informers (pods, pvcs, secrets, services)          │
                │ idle poller ──▶ wolf-bridge /status                               │
                └───────────────┬────────────────────────────────────────┬─────────┘
                                ▼                                        ▼
                           Postgres                            app pod (per running app)
                      users, apps, pairings, events      ┌────────────────────────────┐
                                                         │ app (GOW image)            │
                      LoadBalancer IP (shared)           │ wolf  ◀─ RTSP/RTP/ENet ────┼── Moonlight
                        :47989/:47984 → hub              │ pulseaudio                 │
                        :48100+10·slot → app pod         │ wolf-bridge (:8443) ◀──────┼── hub
                                                         └────────────────────────────┘
```

**Hub.** One process, one replica. It serves the SPA and REST API, speaks the Moonlight protocol on two extra ports, reconciles app rows into Kubernetes objects, and answers moonlight-web's Wolf-style pairing calls. No leader election: the informers, the pairing handshakes and the reconcile queue are in-memory and single-instance.

**wolf-bridge.** Wolf's API is a unix socket. The bridge serves it over TCP inside the pod, guarded by a bearer token that the hub mints per launch, reports readiness once the socket answers (that is the pod's readiness), and follows Wolf's event stream to know whether a client is streaming.

**App pod.** Built by `reconcile.BuildPod`: an init container prepares the shared runtime directory and copies Wolf's `config.toml`; `wolf` renders and encodes; `pulseaudio` provides the virtual sink; `app` is the user's image, started by a wrapper that waits for Wolf's Wayland and Pulse sockets before running the image entrypoint. The app's home (`/home/retro`) is a PVC; everything else is ephemeral.

## Data model

| table | what |
|---|---|
| `users` | login accounts; `api_token_hash` for the Wolf-compatible API |
| `apps` | one row per app: preset, image, resources, env, PVC, plus runtime columns (`state`, `generation`, `stream` JSON, `slot`, `wolf_session_id`, `stream_url`) |
| `pairings` | Moonlight client certificates (by SHA-256 fingerprint) bound to a user |
| `app_events` | per-app event log, pruned to 200 |

An app is **stopped** until a Moonlight client launches it, or a user starts it from the UI to have it warm (running, no stream, reason "ready, waiting for a Moonlight client"). States:

```
stopped ─launch/start→ starting ─pod ready (+stream up)→ running ─stop/cancel/idle→ stopping ─objects gone→ stopped

A `/cancel` during `starting` does not stop the app: clients send it when `/launch` outruns their timeout, and the pod is seconds from usable. A retried `/launch` during `starting` replaces the pending stream keys.
                    │                    │
                    └──timeout/error─────┴──→ failed (pod kept for logs; stop or delete clears it)
any ─delete→ deleting → row deleted
```

`generation` increments on every launch: the Secret (bridge token + config.toml) and the Pod carry it as an annotation, so stale objects from a previous launch are replaced, never reused.

## A launch, step by step

1. Moonlight calls `GET /launch?appid=…&rikey=…&rikeyid=…&mode=1920x1080x60` over HTTPS with its client certificate. The hub maps the certificate fingerprint to a pairing and its user, and the app id to one of that user's apps.
2. `apps.Service.Launch` stops any other app the user runs (one stream per user), picks the lowest free **slot**, stores the stream parameters (client IP, AES key/IV, mode) and sets the app to `starting`.
3. The reconciler creates the PVC (once), the Secret, a per-app **LoadBalancer Service** with the slot's four ports and the sharing key, and the Pod. Wolf inside the pod is told its ports through `WOLF_RTSP_SETUP_PORT` and friends.
4. When the pod is Ready (bridge sees Wolf's socket) and the Service has its IP, the hub calls Wolf's `sessions/add` through the bridge with the stream parameters and `rtsp_fake_ip` = the LoadBalancer IP. Wolf answers with a session id; the hub records `rtsp://<ip>:<rtsp port>` and sets `running`.
5. `/launch` has been polling the row; it returns the RTSP URL. Moonlight connects to it; the RTSP handshake tells it the control/video/audio ports, which land on the same IP and are routed to the pod.
6. `/resume` or a second `/launch` for a running app re-keys: new AES material is stored, the old Wolf session is stopped and a new one added, the URL stays.
7. `/cancel`, "Stop" in the UI, or the idle policy set `stopping`; the Pod and Service are deleted, the slot freed, the row goes back to `stopped`. The PVC survives until the app is deleted.

## Why these choices

- **No CRDs.** Apps are user data with a form; the reconciler is level-triggered over rows + labelled objects, exactly like agents-operator. Orphaned objects (labelled, no row) are deleted after a grace period.
- **Per-slot ports instead of one fixed set.** fenrir could stream one session per cluster because Wolf's ports were fixed. Wolf takes its ports from the environment, so each slot gets its own set and its own Service on the shared IP.
- **Dummy app in Wolf.** Wolf's `sessions/add` without `app_id` creates a no-op "process" app and a dummy client, which is what we want: the real application is a sibling container attached to Wolf's compositor and audio sockets. The generated `config.toml` is Wolf's own default (encoder pipelines included) with the profiles replaced.
- **Pairing in the hub.** The hub implements the four-phase PIN handshake itself, so pairings are rows bound to users and the PIN is typed into the hub UI (or posted by moonlight-web via the Wolf-compatible API). Wolf never sees the real client.
- **Idle stop via the bridge.** Wolf emits `PauseStreamEvent`/`StopStreamEvent`; the bridge turns them into a `streaming` flag the hub polls, so an app whose client went away is stopped after `idleStopAfter` and the GPU node can power down.

## Security model

The hub runs non-root, read-only, with no capabilities, in a namespace Role limited to pods, PVCs, secrets, services and events. App pods are the opposite by necessity: root, `hostIPC` (Steam), `/dev/input` + `/dev/uinput` host paths, capabilities such as `SYS_ADMIN` and `SYS_NICE`, seccomp/AppArmor unconfined for Proton. That is why the namespace needs Pod Security `privileged`; nothing is `privileged: true` in the container sense. The Wolf API is reachable only with the per-launch token; Moonlight's HTTPS side authenticates by client certificate; the UI uses argon2id passwords, HMAC cookies and CSRF tokens; moonlight-web uses a per-user bearer token stored hashed.
