import { useEffect, useState } from "preact/hooks";
import { api, isAdmin, subscribeApps, type App, type AppEvent, type User } from "../api";
import { Nav } from "../components/Nav";
import { Link, navigate } from "../router";
import { stateLabel, timeAgo } from "../util";

const containers = ["app", "wolf", "wolf-bridge", "init"];

export function AppPage(props: { id: string; user: User; onLogout: () => void }) {
  const [app, setApp] = useState<App | null>(null);
  const [events, setEvents] = useState<AppEvent[]>([]);
  const [logs, setLogs] = useState("");
  const [container, setContainer] = useState("app");
  const [error, setError] = useState("");
  const [browserUrl, setBrowserUrl] = useState("");

  const load = () => {
    api.app(props.id).then(setApp).catch((e) => setError((e as Error).message));
    api.apps().then((r) => setBrowserUrl(r.defaults?.browser_url || "")).catch(() => undefined);
    api.appEvents(props.id).then((r) => setEvents(r.events)).catch(() => undefined);
  };

  useEffect(() => {
    load();
    return subscribeApps((type, id, a) => {
      if (id !== props.id) return;
      if (type === "deleted") navigate("/", true);
      else if (a) {
        setApp(a);
        api.appEvents(props.id).then((r) => setEvents(r.events)).catch(() => undefined);
      }
    });
  }, [props.id]);

  const fetchLogs = (c = container) => {
    setContainer(c);
    api.appLogs(props.id, c).then(setLogs).catch((e) => setLogs(`(${(e as Error).message})`));
  };

  const act = async (fn: () => Promise<unknown>) => {
    setError("");
    try {
      await fn();
    } catch (e) {
      setError((e as Error).message);
    }
  };

  if (!app) {
    return (
      <div class="page">
        <Nav user={props.user} onLogout={props.onLogout} />
        {error ? <div class="error">{error}</div> : <div class="muted">Loading…</div>}
      </div>
    );
  }
  const active = app.state === "running" || app.state === "starting";
  return (
    <div class="page">
      <Nav user={props.user} onLogout={props.onLogout} />
      <div class="card app-head">
        {app.icon_url ? <img src={app.icon_url} alt="" class="icon big" /> : <div class="icon big placeholder">{app.name.slice(0, 1)}</div>}
        <div style="min-width:0;flex:1">
          <h2 style="margin:0">{app.name}</h2>
          <div class="sub">
            <span class={`dot ${app.state}`} /> {stateLabel(app.state, app.streaming)}
            {app.state_reason ? ` · ${app.state_reason}` : ""} · {app.template_name ? `catalog: ${app.template_name}` : app.preset} · {app.image}
          </div>
          {app.streaming && app.stream && (
            <div class="sub">
              {app.stream.width}×{app.stream.height}@{app.stream.fps} · client {app.stream.client_name || app.stream.client_ip} · since{" "}
              {timeAgo(app.stream.started_at)}
              {app.stream_url ? ` · ${app.stream_url}` : ""}
            </div>
          )}
        </div>
        <div class="row">
          {!active && (
            <button class="btn primary" onClick={() => act(() => api.startApp(app.id))} title="Start the pod now so a Moonlight launch streams at once">
              Start
            </button>
          )}
          {browserUrl && !app.streaming && (
            <a class="btn primary" href={`${browserUrl}#app=${app.id}`} title="Start the app if needed and stream it in this browser">
              ▶ Play in browser
            </a>
          )}
          {active && (
            <button class="btn danger" onClick={() => act(() => api.stopApp(app.id))}>
              Stop
            </button>
          )}
          {isAdmin(props.user) && !app.template_id && (
            <Link href={`/apps/${app.id}/edit`} class="btn">
              Edit
            </Link>
          )}
          <button
            class="btn danger"
            onClick={() => {
              if (confirm(`Delete ${app.name} and its home volume?`)) act(() => api.deleteApp(app.id));
            }}
          >
            Delete
          </button>
        </div>
      </div>
      {error && <div class="error">{error}</div>}
      <div class="two-col">
        <div class="card">
          <h3>Details</h3>
          <table class="kv">
            <tr>
              <td>Moonlight id</td>
              <td class="mono">{app.moonlight_id}</td>
            </tr>
            <tr>
              <td>Pod</td>
              <td class="mono">{app.pod_name}</td>
            </tr>
            <tr>
              <td>Home volume</td>
              <td>
                {app.pvc_size} {app.storage_class ? `(${app.storage_class})` : ""}
              </td>
            </tr>
            <tr>
              <td>Limits</td>
              <td class="mono">
                {Object.entries(app.resources?.limits || {})
                  .map(([k, v]) => `${k}=${v}`)
                  .join(" ") || "defaults"}
              </td>
            </tr>
            <tr>
              <td>Stream slot</td>
              <td>{app.slot >= 0 ? app.slot : "—"}</td>
            </tr>
            <tr>
              <td>Last active</td>
              <td>{timeAgo(app.last_active_at)}</td>
            </tr>
            <tr>
              <td>Created</td>
              <td>{timeAgo(app.created_at)}</td>
            </tr>
          </table>
        </div>
        <div class="card">
          <h3>Events</h3>
          <ul class="events">
            {events.map((e) => (
              <li key={e.id}>
                <span class="when">{timeAgo(e.at)}</span> <span class="badge">{e.kind}</span> {e.message}
              </li>
            ))}
            {events.length === 0 && <li class="muted">No events yet.</li>}
          </ul>
        </div>
      </div>
      <div class="card" style="margin-top:12px">
        <div class="row">
          <h3 style="margin:0">Logs</h3>
          {containers.map((c) => (
            <button key={c} class={`btn small ${container === c ? "primary" : ""}`} onClick={() => fetchLogs(c)}>
              {c}
            </button>
          ))}
          <button class="btn small" onClick={() => fetchLogs()}>
            Refresh
          </button>
        </div>
        <pre class="logs">{logs || "(pick a container)"}</pre>
      </div>
    </div>
  );
}
