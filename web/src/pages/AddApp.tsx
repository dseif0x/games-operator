import { useEffect, useState } from "preact/hooks";
import { api, type Template, type User } from "../api";
import { Nav } from "../components/Nav";
import { Link, navigate } from "../router";

// AddApp creates an instance of a catalog entry: the admin's definition,
// the user's own name and home volume.
export function AddApp(props: { user: User; onLogout: () => void }) {
  const [templates, setTemplates] = useState<Template[] | null>(null);
  const [picked, setPicked] = useState<Template | null>(null);
  const [name, setName] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const usage = props.user.usage;

  useEffect(() => {
    api
      .catalog()
      .then((r) => setTemplates(r.templates.filter((t) => t.enabled)))
      .catch((e) => setError((e as Error).message));
  }, []);

  const submit = async (e: Event) => {
    e.preventDefault();
    if (!picked) return;
    setBusy(true);
    setError("");
    try {
      const a = await api.createApp({ template_id: picked.id, name: name.trim() || picked.name });
      navigate(`/apps/${a.id}`);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="page">
      <Nav user={props.user} onLogout={props.onLogout} />
      <h2 style="margin:0 0 12px">Add an app</h2>
      <p class="muted hint">
        Pick one from the catalog. It gets its own home volume; what it runs is defined by the admin and follows the catalog.
        {usage && (usage.max_apps > 0 || usage.max_storage) ? (
          <>
            {" "}
            You have {usage.apps}
            {usage.max_apps > 0 ? ` of ${usage.max_apps}` : ""} apps and {usage.storage}
            {usage.max_storage ? ` of ${usage.max_storage}` : ""} of storage.
          </>
        ) : null}
      </p>
      {error && <div class="error">{error}</div>}
      {templates === null ? (
        <div class="muted">Loading…</div>
      ) : templates.length === 0 ? (
        <div class="card empty">
          The catalog is empty. {props.user.role === "admin" ? <Link href="/admin/catalog/new">Add an entry</Link> : "Ask your admin to add apps."}
        </div>
      ) : (
        <form class="card" onSubmit={submit}>
          <div class="presets">
            {templates.map((t) => (
              <button
                type="button"
                key={t.id}
                class={`preset-card ${picked?.id === t.id ? "selected" : ""}`}
                onClick={() => {
                  setPicked(t);
                  if (!name.trim() || templates.some((x) => x.name === name)) setName(t.name);
                }}
              >
                {t.icon_url ? <img src={t.icon_url} alt="" /> : <span class="icon placeholder">{t.name.slice(0, 1)}</span>}
                <span>
                  <b>{t.name}</b>
                  <small>{t.description || `${t.preset} · ${t.pvc_size || "default size"}`}</small>
                </span>
              </button>
            ))}
          </div>
          <label>Name (shown in Moonlight)</label>
          <input value={name} placeholder={picked?.name || "Pick an app above"} onInput={(e) => setName((e.target as HTMLInputElement).value)} />
          <div class="row" style="margin-top:16px">
            <button class="btn primary" disabled={busy || !picked}>
              {busy ? "Adding…" : "Add"}
            </button>
            <button type="button" class="btn" onClick={() => history.back()}>
              Cancel
            </button>
          </div>
        </form>
      )}
    </div>
  );
}
