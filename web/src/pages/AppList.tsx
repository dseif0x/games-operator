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

  const startApp = (a: App) =>
    api
      .startApp(a.id)
      .then(load)
      .catch((e) => setError(String(e)));

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
          Apps start when a Moonlight client launches them. Add <code>{defaults.moonlight_host || "the LoadBalancer IP"}</code> as a host in
          Moonlight, <Link href="/pair">pair it</Link>, and pick the app there.
          {defaults.browser_url ? (
            <>
              {" "}
              In the browser,{" "}
              <a href={defaults.browser_url} target="_blank" rel="noopener">
                moonlight-web
              </a>{" "}
              is the client: add the same host once (give it a <em>Wolf</em> backend from <Link href="/account">Account</Link> so it pairs by itself),
              then launch apps from its list.
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
            <AppCard key={a.id} a={a} browserUrl={defaults?.browser_url || ""} onStart={() => startApp(a)} onStop={() => stopApp(a)} />
          ))}
        </div>
      )}
    </div>
  );
}

function AppCard({ a, browserUrl, onStart, onStop }: { a: App; browserUrl: string; onStart: () => void; onStop: () => void }) {
  const active = a.state === "running" || a.state === "starting";
  return (
    <div class="card app-card">
      <Link href={`/apps/${a.id}`} class="icon-wrap">
        {a.icon_url ? <img src={a.icon_url} alt="" class="icon" loading="lazy" /> : <div class="icon placeholder">{a.name.slice(0, 1)}</div>}
      </Link>
      <div style="min-width:0">
        <div class="title">
          <Link href={`/apps/${a.id}`}>{a.name}</Link>
          <span class={`dot ${a.state}`} title={stateLabel(a.state, a.streaming)} />
          <span class="badge">{stateLabel(a.state, a.streaming)}</span>
          <span class="badge preset">{a.preset}</span>
        </div>
        <div class="sub">
          {a.streaming && a.stream
            ? `streaming ${a.stream.width}×${a.stream.height}@${a.stream.fps} to ${a.stream.client_name || a.stream.client_ip}`
            : a.state_reason || `last active ${timeAgo(a.last_active_at)}`}
        </div>
      </div>
      <div class="row">
        {!active && (
          <button class="btn small primary" onClick={onStart} title="Start the pod now so a Moonlight launch streams at once">
            Start
          </button>
        )}
        {browserUrl && a.state === "running" && !a.streaming && (
          <a class="btn small" href={browserUrl} target="_blank" rel="noopener" title="Open moonlight-web and pick this app">
            ▶ Play in browser
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
