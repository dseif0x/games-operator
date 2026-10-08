import { useEffect, useState } from "preact/hooks";
import { api, subscribeApps, type App, type Defaults, type User } from "../api";
import { Nav } from "../components/Nav";
import { Link } from "../router";
import { stateLabel, timeAgo } from "../util";

export function AppList(props: { user: User; onLogout: () => void }) {
  const [apps, setApps] = useState<App[] | null>(null);
  const [defaults, setDefaults] = useState<Defaults | null>(null);
  const [error, setError] = useState("");
  const [, tick] = useState(0);

  const load = () =>
    api
      .apps()
      .then((r) => {
        setApps(r.apps);
        setDefaults(r.defaults);
        setError("");
      })
      .catch((e) => setError((e as Error).message));

  useEffect(() => {
    load();
    const stop = subscribeApps((type, id, app) => {
      setApps((cur) => {
        if (!cur) return cur;
        if (type === "deleted") return cur.filter((a) => a.id !== id);
        if (!app) return cur;
        const idx = cur.findIndex((a) => a.id === id);
        if (idx < 0) return [...cur, app].sort((a, b) => a.name.localeCompare(b.name));
        const next = cur.slice();
        next[idx] = app;
        return next;
      });
    }, load);
    const t = setInterval(() => tick((n) => n + 1), 15000);
    return () => {
      stop();
      clearInterval(t);
    };
  }, []);

  const stopApp = async (a: App) => {
    try {
      await api.stopApp(a.id);
    } catch (e) {
      setError((e as Error).message);
    }
  };

  return (
    <div class="page">
      <Nav
        user={props.user}
        onLogout={props.onLogout}
        right={
          <Link href="/new" class="btn primary small">
            + New app
          </Link>
        }
      />
      {error && <div class="error">{error}</div>}
      {defaults && (
        <p class="muted hint">
          Add <code>{defaults.moonlight_host || "the LoadBalancer IP"}</code> as a host in Moonlight, then <Link href="/pair">pair it</Link>.
          {defaults.browser_url ? (
            <>
              {" "}
              Or play in the browser via{" "}
              <a href={defaults.browser_url} target="_blank" rel="noopener">
                moonlight-web
              </a>
              .
            </>
          ) : null}
        </p>
      )}
      {apps === null ? (
        <div class="muted">Loading…</div>
      ) : apps.length === 0 ? (
        <div class="card empty">
          No apps yet. <Link href="/new">Create one</Link> and it shows up in Moonlight.
        </div>
      ) : (
        <div class="apps">
          {apps.map((a) => (
            <AppCard key={a.id} a={a} browserUrl={defaults?.browser_url || ""} onStop={() => stopApp(a)} />
          ))}
        </div>
      )}
    </div>
  );
}

function AppCard({ a, browserUrl, onStop }: { a: App; browserUrl: string; onStop: () => void }) {
  const active = a.state === "running" || a.state === "starting";
  return (
    <div class="card app-card">
      <Link href={`/apps/${a.id}`} class="icon-wrap">
        {a.icon_url ? <img src={a.icon_url} alt="" class="icon" loading="lazy" /> : <div class="icon placeholder">{a.name.slice(0, 1)}</div>}
      </Link>
      <div style="min-width:0">
        <div class="title">
          <Link href={`/apps/${a.id}`}>{a.name}</Link>
          <span class={`dot ${a.state}`} title={stateLabel(a.state)} />
          <span class="badge">{stateLabel(a.state)}</span>
          <span class="badge preset">{a.preset}</span>
        </div>
        <div class="sub">
          {a.state === "running" && a.stream
            ? `streaming ${a.stream.width}×${a.stream.height}@${a.stream.fps} to ${a.stream.client_name || a.stream.client_ip}`
            : a.state_reason || `last active ${timeAgo(a.last_active_at)}`}
        </div>
      </div>
      <div class="row">
        {browserUrl && !active && (
          <a class="btn small" href={browserUrl} target="_blank" rel="noopener" title="Open moonlight-web and pick this app">
            ▶ Browser
          </a>
        )}
        {active && (
          <button class="btn small danger" onClick={onStop}>
            Stop
          </button>
        )}
      </div>
    </div>
  );
}
