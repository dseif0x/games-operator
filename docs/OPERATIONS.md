# Operations

## Homelab install (Flux)

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata: { name: games-operator, namespace: flux-system }
spec: { interval: 10m, url: https://dseif0x.github.io/games-operator/ }
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: { name: games-operator, namespace: games }
spec:
  interval: 5m
  chart:
    spec: { chart: games-operator, version: 0.1.0, sourceRef: { kind: HelmRepository, name: games-operator, namespace: flux-system } }
  values:
    publicUrl: https://games.homelab.example.com
    ingress: { host: games.homelab.example.com, annotations: { cert-manager.io/cluster-issuer: letsencrypt-dns } }
    moonlight: { loadBalancerIP: 10.13.254.10, maxConcurrent: 4 }
    apps:
      storageClass: local-path          # the GPU node's own disk
      nodeSelector: { kubernetes.io/hostname: gpu-node }
    publicUrl: https://games.homelab.example.com
    browser: { enabled: true }   # the player at /play
    postgresql: { enabled: false }
    database: { existingSecret: games-operator-db-app, existingSecretKey: uri }   # CloudNativePG
```

Label the namespace `pod-security.kubernetes.io/enforce=privileged` (app pods need hostIPC, host device paths and capabilities).

## Installing without RBAC rights

The chart creates a namespace-scoped Role and RoleBinding for the hub. An operator who holds only the `edit` ClusterRole in the namespace cannot create those; install with `--set rbac.create=false` and have a cluster admin apply the two objects once:

```sh
helm template games-operator charts/games-operator --namespace games-operator \
  --set postgresql.enabled=false --set database.url=postgres://unused \
  --show-only templates/rbac.yaml | kubectl apply -f -
```

For the default names (release and namespace `games-operator`) the rendered result is checked in as `hack/rbac.yaml`:

```sh
kubectl apply -f https://raw.githubusercontent.com/dseif0x/games-operator/main/hack/rbac.yaml
```

## Networking

- The chart's `-moonlight` Service (47989/47984 TCP) and every per-app Service share one LoadBalancer IP through `metallb.io/allow-shared-ip` / `lbipam.cilium.io/sharing-key`. Set `moonlight.loadBalancerIP` to pin it: that IP is what users add in Moonlight, and the hub needs it before the first Service exists.
- Per-app ports: `streamPortBase + 10×slot` (RTSP, TCP) and `+1/+2/+3` (control, video, audio; UDP). With `maxConcurrent: 10` that is 48100–48199. Open them on the firewall if clients come from another network.
- Moonlight clients see the app list only after pairing; `serverinfo` without a certificate reports the host as unpaired.

## In-browser play (moonlight-web)

`browser.enabled: true` deploys the games-operator fork of [moonlight-web](https://github.com/dseif0x/moonlight-web) (branch `games-operator`, image `ghcr.io/dseif0x/moonlight-web:go-<version>`) in its *embedded mode*: the hub serves it below `browser.path` (`/play`) on its own host, requires the hub login for it, and marks every forwarded request with a secret moonlight-web trusts (`MW_EMBEDDED_SECRET`, key `browserSecret` of the auth Secret). There is no PIN, no host list and no admin page.

**Play in browser** on an app opens `/play/#app=<id>`. The player asks the hub to prepare the app (`POST /api/v1/apps/{id}/play`: starts the pod, mints the user's browser token, makes the shared moonlight-web pairing follow the user), adds and pairs the Moonlight host on its own, waits for the pod while showing the hub's progress, then launches. One host name, one Ingress, one certificate; the stream falls back to moonlight-web's WebSocket transport through the hub where WebRTC's UDP cannot reach the browser (a Cloudflare tunnel, a firewall between subnets). `browser.hostNetwork: true` additionally exposes the UDP media ports on the node for browsers on the same LAN.

The player uses moonlight-web's WebSocket transport by default (`browser.transport: wss`): it rides the same HTTPS connection as everything else, so it works through the hub, a tunnel or a firewall between subnets. `auto` tries WebRTC first and falls back when its UDP path fails; each fallback is a Moonlight resume, which Wolf handles without disturbing the app.

The player asks for H.264 by default (`browser.codec: h264`); `auto` lets moonlight-web negotiate HEVC or AV1, and a browser that cannot decode what it claimed (desktop Firefox and HEVC) makes it re-launch the session with H.264, a resume Wolf handles without disturbing the app.

A client's `/cancel` ends its stream and never stops the app: clients send it after a launch they gave up on, before a fallback re-launch, and for Quit alike. The idle stop reclaims the app, and the hub's Stop button is explicit.

**Resuming keeps the app.** Streams are started through Wolf's own Moonlight HTTPS side: the hub is a paired client of every Wolf (its client certificate is written into each pod's Wolf config), and a client reconnecting, a browser reload or a transport or codec fallback becomes a Wolf resume, which creates the session with the new keys but keeps the compositor and the input devices. The app never notices.

moonlight-web is one Moonlight client for every browser. Its pairing is bound to whoever pressed Play last, so two different hub users cannot play through it at the same time; the same user on several devices can.

`browser.externalUrl` (with `enabled: false`) only adds a link to a stock moonlight-web you run elsewhere; pair it by hand through the Pair page or the Account page's API token.

## GPU node that sleeps, and client timeouts

A launch creates a pod requesting `nvidia.com/gpu`; a pending pod is what node auto-provisioners react to. Booting a node and pulling the images takes minutes, while Moonlight clients give `/launch` 20 s (moonlight-web) to two minutes (Moonlight Qt) and then send `/cancel`. The hub is built for that:

- `/launch` holds the request until the stream is up or the client goes away. While the app is `starting`, every retry replaces the pending stream keys, so the stream that comes up matches the client that is waiting now.
- `/cancel` while the app is `starting` is ignored, with an event on the app. Only a running app is quit by `/cancel`.
- **Start** in the UI (`POST /api/v1/apps/{id}/start`) warms an app up without a client: the pod comes up and the app shows *Ready*. A launch then streams at once. Use it before sitting down to play, or when the node is asleep.
- After the stream ends, or if no client ever comes, the app is stopped `apps.idleStopAfter` later (default 15 min), the pod disappears and the node can power off.

## Input devices

Wolf creates a virtual mouse, keyboard and one controller per client on the node through `/dev/uinput` and `/dev/uhid` (DualSense), and the app reads the `/dev/input` and `/dev/hidraw*` nodes as they appear. A hostPath puts those nodes into a container, but the device cgroup still refuses to open them, so the Wolf and app containers run privileged; the bridge does not. This is why the namespace needs the `privileged` Pod Security level. The app container mounts the node's whole `/dev`: a privileged container's own `/dev` is a snapshot taken when it starts, and the controller's nodes only exist once a client connects.

A pod has no udevd, and the node's udevd never speaks into the pod's network namespace, so the bridge stands in for it (`internal/udev`, the equivalent of Wolf's `fake-udev` for its docker runner). It watches the node's `/dev` and, for every virtual device, writes the udev database entry into a `/run/udev` shared with Wolf and the app (libinput in Wolf's compositor and SDL in the app both refuse a device udev has not "initialized"), opens the node to everyone, and multicasts the hotplug message libudev clients listen for, so a Steam that is already running picks the controller up. The bridge needs `NET_ADMIN` for that multicast; without it (or without the `/host/dev` mount of an older pod spec) the database is still written and only apps started after the device see it. `wolf-bridge` logs `input device plugged` with the node and its class.

## When the GPU node dies

A GPU that falls off the bus (NVIDIA Xid 79) or a node reboot under a running pod leaves containers that cannot be created any more. The reconciler marks such an app failed as soon as a container reports a crash loop, a start error or a pull error, and deletes the failed app's pod after `FailedPodGrace` (5 min; long enough to read its logs in the UI) so the GPU and the node are released. Play, or Start, then builds a fresh pod. A running app whose pod stops being ready and does not come back within the starting timeout is failed as well.

## Storage

Game libraries are large and re-downloadable: put app home volumes on the GPU node's local disk (`apps.storageClass: local-path`) rather than NFS. The hub's own volume (`persistence.*`, 100Mi) holds the Moonlight server certificate; losing it un-pairs every client.

## Troubleshooting

| symptom | look at |
|---|---|
| Moonlight: "host unreachable" | `kubectl get svc -n games`: does the `-moonlight` Service have an IP? Is 47989 reachable from the client? |
| pairing never completes | the UI's Pair page must show the pending request; if not, the HTTPS port (47984) is blocked. PIN timeout is 2 minutes. |
| app stuck in `starting` | the app page shows the reason (Unschedulable, ImagePullBackOff, "waiting for LoadBalancer IP"); logs tab per container. 10-minute timeout → `failed`. |
| stream starts, black screen | `wolf` container logs: encoder (nvcodec) found? `runtimeClassName: nvidia` and `NVIDIA_VISIBLE_DEVICES` in place? |
| no controller input | `wolf-bridge` logs: `input device plugged … class=joystick` after the client connected? If not, `/dev/uinput` and `/dev/uhid` must exist on the node and the `wolf` logs show `Creating … joypad`. If so but the game ignores it, check the app's `/run/udev/data/c13:*` entries and the client (browser gamepad tester under Settings). |
| second user cannot start | all `moonlight.maxConcurrent` slots in use, or a time-sliced GPU with too few replicas. |

Metrics: `/metrics` (Go/process metrics; `metrics.serviceMonitor.enabled` for Prometheus). Logs: JSON on stdout, `logLevel: debug` for reconcile details.

## Releasing

The git tag is the only version source. `git tag v0.1.0 && git push --tags` builds the two images (`ghcr.io/dseif0x/games-operator`, `…-bridge`) for amd64/arm64, creates the GitHub release, then packages the chart with that version and pushes `index.yaml` to the `gh-pages` branch. One-time setup: an empty `gh-pages` branch, GitHub Pages serving it, workflow permissions "read and write", the GHCR packages public.
