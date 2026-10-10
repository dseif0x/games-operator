# games-operator

Game streaming on Kubernetes. One Go binary is a **Moonlight-compatible host**: your Moonlight clients (Android, iOS, TV, PC) or a browser via [moonlight-web](https://github.com/linckosz/moonlight-web) pair with it, see your apps, and press play. Each app (Steam, Firefox, RetroArch, a launcher, any Games on Whales image) runs as its **own pod** next to [Wolf](https://games-on-whales.github.io/wolf/) on the GPU node while someone streams, with its own home volume, and is gone when the stream ends.

It is the sibling of [agents-operator](https://github.com/dseif0x/agents-operator) and a cleaner successor to [fenrir](https://github.com/games-on-whales/fenrir): no CRDs, apps are rows users create in a web UI, several apps can stream at the same time on one LoadBalancer IP.

```
Moonlight client ──47989/47984──▶ hub (Moonlight protocol: pair, applist, launch)
                 ──RTSP/RTP/ENet─▶ app pod: app + wolf (with its own PulseAudio) + wolf-bridge
browser ─▶ hub (/play, reverse proxy) ─▶ moonlight-web (embedded) ─▶ (same path, re-packetised onto WebRTC or a WebSocket)
hub ──▶ Postgres (users, apps, pairings)   hub ──▶ Kubernetes (pods, PVCs, Services)
```

- **Hub** (`cmd/games-operator`): web UI + REST API, the Moonlight HTTP/HTTPS front door, a level-triggered reconciler that turns app rows into pods, and a Wolf-compatible pairing API so moonlight-web pairs without typing a PIN.
- **wolf-bridge** (`cmd/wolf-bridge`): sidecar that exposes Wolf's unix socket API to the hub with a per-launch token and tells the hub whether a client is streaming (idle stop).
- **Apps**: created from presets (Steam, Firefox, Prism Launcher, RetroArch, Lutris, Heroic, Pegasus, custom image) with the launch wrapper, capabilities and devices that are known to work.
- **Streams**: every running app gets its own port set (`48100+10×slot …`) on the IP the Moonlight Service uses (MetalLB `allow-shared-ip` / Cilium sharing key), so N users can play N apps at once.
- **Users**: admins define a **catalog** of apps and manage accounts and quotas; users add catalog apps to their own list, each with its own home volume, and pair their own Moonlight clients. Every user sees only their apps, in the hub and in Moonlight.

Details: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), [docs/OPERATIONS.md](docs/OPERATIONS.md), chart values in [charts/games-operator/values.yaml](charts/games-operator/values.yaml).

## Install

```sh
helm repo add games-operator https://dseif0x.github.io/games-operator/
helm install games-operator games-operator/games-operator -n games --create-namespace \
  --set publicUrl=https://games.example.com --set ingress.host=games.example.com \
  --set moonlight.loadBalancerIP=10.0.0.50 --set apps.storageClass=local-path
kubectl label ns games pod-security.kubernetes.io/enforce=privileged
```

The NOTES print how to read the generated admin password. Then:

1. Open the web UI as the admin: add a catalog entry under **Admin → Catalog** (pick a preset), then **+ Add app** on your own list. Create accounts for the others under **Admin → Users**; they add apps from the catalog within their quota (`quotas.*`).
2. In Moonlight add the LoadBalancer IP as a host and press pair; type the PIN on the UI's **Pair** page. The client is bound to your account and sees your apps.
3. Launch the app in Moonlight. The pod is created (your GPU node wakes up if it is asleep), Wolf starts the stream, Moonlight connects. Quit the app in Moonlight, or wait `apps.idleStopAfter`, and the pod is deleted; the home volume stays.

For the browser: enable `browser.enabled`. The hub then serves the games-operator fork of moonlight-web below `/play` on its own host, behind its own login, and **Play in browser** on any app starts it, pairs and streams without any further setup.

## Requirements

- A node with an NVIDIA GPU, the NVIDIA device plugin and `runtimeClassName: nvidia` (AMD/Intel work with Wolf too; set `apps.runtimeClassName` empty and pass the DRI device by other means).
- MetalLB (or Cilium LB-IPAM) for the shared LoadBalancer IP.
- Pod Security `privileged` on the namespace: game containers run as root with `hostIPC`, the node's `/dev` and extra capabilities (Wolf creates the virtual input devices there). The hub itself is non-root, read-only and capability-less.
- Postgres (chart dependency or `database.existingSecret`).

## Development

`make dev-db && make dev` runs the hub against the compose Postgres and your kubeconfig; `make test`, `make lint`, `make build`. The web UI is Preact + Vite under `web/`, embedded into the binary with `go build -tags ui`.

## License

MIT. The Moonlight protocol implementation is derived from [games-on-whales/fenrir](https://github.com/games-on-whales/fenrir) (MIT).
