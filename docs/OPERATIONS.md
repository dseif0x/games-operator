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
    publicUrl: https://games.homelab.example.com/hub   # the hub below /hub, moonlight-web at the root
    browser: { enabled: true }
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

## moonlight-web

`browser.enabled: true` deploys `ghcr.io/linckosz/moonlight-web` and makes the hub reverse-proxy it: the root of `publicUrl`'s host is moonlight-web, the hub's UI and API live under the path in `publicUrl` (say `/hub`). One host name, one Ingress, one certificate, and it works through a Cloudflare tunnel or across a firewall, because the stream falls back to moonlight-web's WebSocket transport when WebRTC's UDP cannot reach the browser. `browser.hostNetwork: true` additionally exposes the UDP media ports on the node for browsers on the same LAN.

First run, once:

```sh
kubectl -n games exec deploy/games-operator-moonlight-web -- moonlightweb --new-pin        # a PIN per device to log in
kubectl -n games exec -i deploy/games-operator-moonlight-web -- moonlightweb --set-admin-password   # reads the password twice from stdin
```

Open the host's root, enter the PIN, add the Moonlight LoadBalancer IP as a host, then host card → ⋯ → **Backend**: type *Wolf*, API URL and token from the hub's Account page. From then on moonlight-web pairs and streams your apps in the browser. Apps start when moonlight-web (or any Moonlight client) launches them; the hub's "Browser" button only opens moonlight-web.

moonlight-web sees every browser behind the hub's address, so its per-peer flood protection counts all users together.

## GPU node that sleeps, and client timeouts

A launch creates a pod requesting `nvidia.com/gpu`; a pending pod is what node auto-provisioners react to. Booting a node and pulling the images takes minutes, while Moonlight clients give `/launch` 20 s (moonlight-web) to two minutes (Moonlight Qt) and then send `/cancel`. The hub is built for that:

- `/launch` holds the request until the stream is up or the client goes away. While the app is `starting`, every retry replaces the pending stream keys, so the stream that comes up matches the client that is waiting now.
- `/cancel` while the app is `starting` is ignored, with an event on the app. Only a running app is quit by `/cancel`.
- **Start** in the UI (`POST /api/v1/apps/{id}/start`) warms an app up without a client: the pod comes up and the app shows *Ready*. A launch then streams at once. Use it before sitting down to play, or when the node is asleep.
- After the stream ends, or if no client ever comes, the app is stopped `apps.idleStopAfter` later (default 15 min), the pod disappears and the node can power off.

## Storage

Game libraries are large and re-downloadable: put app home volumes on the GPU node's local disk (`apps.storageClass: local-path`) rather than NFS. The hub's own volume (`persistence.*`, 100Mi) holds the Moonlight server certificate; losing it un-pairs every client.

## Troubleshooting

| symptom | look at |
|---|---|
| Moonlight: "host unreachable" | `kubectl get svc -n games`: does the `-moonlight` Service have an IP? Is 47989 reachable from the client? |
| pairing never completes | the UI's Pair page must show the pending request; if not, the HTTPS port (47984) is blocked. PIN timeout is 2 minutes. |
| app stuck in `starting` | the app page shows the reason (Unschedulable, ImagePullBackOff, "waiting for LoadBalancer IP"); logs tab per container. 10-minute timeout → `failed`. |
| stream starts, black screen | `wolf` container logs: encoder (nvcodec) found? `runtimeClassName: nvidia` and `NVIDIA_VISIBLE_DEVICES` in place? |
| no controller input | `/dev/uinput` and `/dev/input` must exist on the node; with a generic device plugin set `apps.uinputResource`. |
| second user cannot start | all `moonlight.maxConcurrent` slots in use, or a time-sliced GPU with too few replicas. |

Metrics: `/metrics` (Go/process metrics; `metrics.serviceMonitor.enabled` for Prometheus). Logs: JSON on stdout, `logLevel: debug` for reconcile details.

## Releasing

The git tag is the only version source. `git tag v0.1.0 && git push --tags` builds the two images (`ghcr.io/dseif0x/games-operator`, `…-bridge`) for amd64/arm64, creates the GitHub release, then packages the chart with that version and pushes `index.yaml` to the `gh-pages` branch. One-time setup: an empty `gh-pages` branch, GitHub Pages serving it, workflow permissions "read and write", the GHCR packages public.
