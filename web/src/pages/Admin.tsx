import { useEffect, useState } from "preact/hooks";
import { api, subscribeApps, type OwnedApp, type Template, type User, type UserRequest } from "../api";
import { Nav } from "../components/Nav";
import { Link, navigate } from "../router";
import { stateLabel, timeAgo } from "../util";

// Admin: the accounts, the catalog and every app on the hub.
export function Admin(props: { user: User; onLogout: () => void; tab: string }) {
  const tab = ["users", "apps", "catalog"].includes(props.tab) ? props.tab : "users";
  return (
    <div class="page">
      <Nav user={props.user} onLogout={props.onLogout} />
      <div class="row" style="margin-bottom:12px">
        <h2 style="margin:0">Admin</h2>
        {[
          ["users", "Users"],
          ["apps", "All apps"],
          ["catalog", "Catalog"],
        ].map(([key, label]) => (
          <Link key={key} href={`/admin/${key}`} class={`btn small ${tab === key ? "primary" : ""}`}>
            {label}
          </Link>
        ))}
      </div>
      {tab === "users" && <Users me={props.user} />}
      {tab === "apps" && <AllApps />}
      {tab === "catalog" && <Catalog />}
    </div>
  );
}

const limit = (n: number | undefined, s: string | undefined) =>
  n === undefined && s === undefined ? "defaults" : `${n === undefined ? "default" : n === 0 ? "∞" : n} apps · ${s === undefined ? "default" : s || "∞"}`;

function Users({ me }: { me: User }) {
  const [users, setUsers] = useState<User[]>([]);
  const [defaults, setDefaults] = useState({ max_apps: 0, max_storage: "" });
  const [error, setError] = useState("");
  const [msg, setMsg] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<"user" | "admin">("user");
  const [editing, setEditing] = useState<User | null>(null);

  const load = () =>
    api
      .users()
      .then((r) => {
        setUsers(r.users);
        setDefaults(r.defaults);
      })
      .catch((e) => setError((e as Error).message));
  useEffect(() => {
    load();
  }, []);

  const act = async (fn: () => Promise<unknown>, done = "") => {
    setError("");
    setMsg("");
    try {
      await fn();
      if (done) setMsg(done);
      load();
    } catch (e) {
      setError((e as Error).message);
    }
  };

  const create = (e: Event) => {
    e.preventDefault();
    act(() => api.createUser({ username, password, role }), `${username} can log in now.`).then(() => {
      setUsername("");
      setPassword("");
    });
  };

  return (
    <>
      {error && <div class="error">{error}</div>}
      {msg && <div class="ok">{msg}</div>}
      <div class="card">
        <h3>Accounts</h3>
        <p class="muted">
          Users add apps from the catalog within their quota; admins define the catalog, manage accounts and see every app. Default quota:{" "}
          {defaults.max_apps || "∞"} apps, {defaults.max_storage || "∞"} storage.
        </p>
        <table class="kv wide">
          {users.map((u) => (
            <tr key={u.id}>
              <td>
                <b>{u.username}</b>
                {u.id === me.id ? <span class="muted"> (you)</span> : null}
              </td>
              <td>
                <span class="badge">{u.role}</span>
                {u.disabled ? <span class="badge failed">disabled</span> : null}
              </td>
              <td class="muted">
                {u.usage ? `${u.usage.apps} apps · ${u.usage.storage}` : ""}
              </td>
              <td class="muted">quota: {limit(u.quota.max_apps, u.quota.max_storage)}</td>
              <td>
                <div class="row">
                  <button class="btn small" onClick={() => setEditing(u)}>
                    Edit
                  </button>
                  {u.id !== me.id && (
                    <>
                      <button
                        class="btn small"
                        onClick={() => act(() => api.updateUser(u.id, { disabled: !u.disabled }))}
                        title={u.disabled ? "Let the user log in again" : "Keep the user out; apps stay"}
                      >
                        {u.disabled ? "Enable" : "Disable"}
                      </button>
                      <button
                        class="btn small danger"
                        onClick={() => {
                          if (confirm(`Delete ${u.username}, with all their apps and home volumes?`)) act(() => api.deleteUser(u.id));
                        }}
                      >
                        Delete
                      </button>
                    </>
                  )}
                </div>
              </td>
            </tr>
          ))}
        </table>
      </div>
      {editing && (
        <EditUser
          user={editing}
          me={me}
          onClose={() => setEditing(null)}
          onSave={(req) => act(() => api.updateUser(editing.id, req), `${editing.username} updated.`).then(() => setEditing(null))}
        />
      )}
      <form class="card" style="margin-top:12px" onSubmit={create}>
        <h3>New account</h3>
        <div class="form-grid">
          <div>
            <label>Username</label>
            <input value={username} onInput={(e) => setUsername((e.target as HTMLInputElement).value)} autocomplete="off" />
          </div>
          <div>
            <label>Password (8+ characters)</label>
            <input type="password" value={password} onInput={(e) => setPassword((e.target as HTMLInputElement).value)} autocomplete="new-password" />
          </div>
          <div>
            <label>Role</label>
            <select value={role} onChange={(e) => setRole((e.target as HTMLSelectElement).value as "user" | "admin")}>
              <option value="user">user</option>
              <option value="admin">admin</option>
            </select>
          </div>
        </div>
        <div class="row" style="margin-top:12px">
          <button class="btn primary" disabled={!username || !password}>
            Create
          </button>
        </div>
      </form>
    </>
  );
}

function EditUser({ user, me, onClose, onSave }: { user: User; me: User; onClose: () => void; onSave: (req: UserRequest) => void }) {
  const [role, setRole] = useState(user.role);
  const [password, setPassword] = useState("");
  const [maxApps, setMaxApps] = useState(user.quota.max_apps === undefined ? "" : String(user.quota.max_apps));
  const [maxStorage, setMaxStorage] = useState(user.quota.max_storage === undefined ? "" : user.quota.max_storage);
  const [useDefaults, setUseDefaults] = useState(user.quota.max_apps === undefined && user.quota.max_storage === undefined);

  const save = (e: Event) => {
    e.preventDefault();
    const req: UserRequest = { role };
    if (password) req.password = password;
    req.quota = useDefaults ? {} : { max_apps: maxApps.trim() === "" ? 0 : Number(maxApps), max_storage: maxStorage.trim() };
    onSave(req);
  };

  return (
    <form class="card" style="margin-top:12px" onSubmit={save}>
      <h3>Edit {user.username}</h3>
      <div class="form-grid">
        <div>
          <label>Role</label>
          <select value={role} disabled={user.id === me.id} onChange={(e) => setRole((e.target as HTMLSelectElement).value as "user" | "admin")}>
            <option value="user">user</option>
            <option value="admin">admin</option>
          </select>
        </div>
        <div>
          <label>New password (empty = unchanged)</label>
          <input type="password" value={password} onInput={(e) => setPassword((e.target as HTMLInputElement).value)} autocomplete="new-password" />
        </div>
      </div>
      <label class="checkbox">
        <input type="checkbox" checked={useDefaults} onChange={(e) => setUseDefaults((e.target as HTMLInputElement).checked)} /> Default quota
      </label>
      {!useDefaults && (
        <div class="form-grid">
          <div>
            <label>Max apps (0 = unlimited)</label>
            <input value={maxApps} inputMode="numeric" onInput={(e) => setMaxApps((e.target as HTMLInputElement).value)} placeholder="0" />
          </div>
          <div>
            <label>Max storage (empty = unlimited)</label>
            <input value={maxStorage} onInput={(e) => setMaxStorage((e.target as HTMLInputElement).value)} placeholder="500Gi" />
          </div>
        </div>
      )}
      <div class="row" style="margin-top:12px">
        <button class="btn primary">Save</button>
        <button type="button" class="btn" onClick={onClose}>
          Cancel
        </button>
      </div>
    </form>
  );
}

function AllApps() {
  const [apps, setApps] = useState<OwnedApp[] | null>(null);
  const [error, setError] = useState("");

  const load = () =>
    api
      .allApps()
      .then((r) => setApps(r.apps))
      .catch((e) => setError((e as Error).message));

  useEffect(() => {
    load();
    // The feed carries the owner id, not the name: keep names from the list.
    return subscribeApps(
      (type, id, app) => {
        setApps((cur) => {
          if (!cur) return cur;
          if (type === "deleted") return cur.filter((a) => a.id !== id);
          if (!app) return cur;
          const idx = cur.findIndex((a) => a.id === id);
          if (idx < 0) {
            load();
            return cur;
          }
          const next = cur.slice();
          next[idx] = { ...app, owner: cur[idx].owner };
          return next;
        });
      },
      load,
      true,
    );
  }, []);

  const act = async (fn: () => Promise<unknown>) => {
    setError("");
    try {
      await fn();
    } catch (e) {
      setError((e as Error).message);
    }
  };

  return (
    <div class="card">
      <h3>Every app</h3>
      {error && <div class="error">{error}</div>}
      {apps === null ? (
        <div class="muted">Loading…</div>
      ) : apps.length === 0 ? (
        <div class="muted">No apps yet.</div>
      ) : (
        <table class="kv wide">
          {apps.map((a) => {
            const active = a.state === "running" || a.state === "starting";
            return (
              <tr key={a.id}>
                <td>
                  <b>{a.name}</b>
                  <div class="muted">{a.template_name ? `catalog: ${a.template_name}` : `custom · ${a.preset}`}</div>
                </td>
                <td>{a.owner}</td>
                <td>
                  <span class={`dot ${a.state}`} /> {stateLabel(a.state, a.streaming)}
                  {a.streaming && a.stream ? ` to ${a.stream.client_name || a.stream.client_ip}` : ""}
                </td>
                <td class="muted">
                  {a.pvc_size} · last active {timeAgo(a.last_active_at)}
                </td>
                <td>
                  <div class="row">
                    {active && (
                      <button class="btn small danger" onClick={() => act(() => api.stopAnyApp(a.id))}>
                        Stop
                      </button>
                    )}
                    <button
                      class="btn small danger"
                      onClick={() => {
                        if (confirm(`Delete ${a.owner}'s ${a.name} and its home volume?`)) act(() => api.deleteAnyApp(a.id));
                      }}
                    >
                      Delete
                    </button>
                  </div>
                </td>
              </tr>
            );
          })}
        </table>
      )}
    </div>
  );
}

function Catalog() {
  const [templates, setTemplates] = useState<Template[] | null>(null);
  const [error, setError] = useState("");

  const load = () =>
    api
      .catalog()
      .then((r) => setTemplates(r.templates))
      .catch((e) => setError((e as Error).message));
  useEffect(() => {
    load();
  }, []);

  const act = async (fn: () => Promise<unknown>) => {
    setError("");
    try {
      await fn();
      load();
    } catch (e) {
      setError((e as Error).message);
    }
  };

  const toggle = (t: Template) =>
    act(() =>
      api.updateTemplate(t.id, {
        name: t.name,
        description: t.description,
        preset: t.preset,
        image: t.image,
        icon_url: t.icon_url,
        hdr: t.hdr,
        command: t.command,
        pvc_size: t.pvc_size,
        storage_class: t.storage_class,
        resources: t.resources,
        env: t.env,
        host_ipc: t.host_ipc,
        capabilities: t.capabilities,
        enabled: !t.enabled,
      }),
    );

  return (
    <div class="card">
      <div class="row">
        <h3 style="margin:0">Catalog</h3>
        <span class="spacer" />
        <Link href="/admin/catalog/new" class="btn primary small">
          + New entry
        </Link>
      </div>
      <p class="muted">What users can add. Changing an entry reaches its instances on their next start; deleting one leaves them as they last ran.</p>
      {error && <div class="error">{error}</div>}
      {templates === null ? (
        <div class="muted">Loading…</div>
      ) : templates.length === 0 ? (
        <div class="muted">Nothing yet. Users cannot add apps until there is an entry.</div>
      ) : (
        <table class="kv wide">
          {templates.map((t) => (
            <tr key={t.id}>
              <td>
                {t.icon_url ? <img src={t.icon_url} alt="" class="icon small" /> : null}
              </td>
              <td>
                <b>{t.name}</b>
                <div class="muted">{t.description || t.image}</div>
              </td>
              <td class="muted">
                {t.preset} · {t.pvc_size || "default size"}
              </td>
              <td>
                {t.enabled ? <span class="badge running">enabled</span> : <span class="badge">hidden</span>}
                <span class="muted"> · {t.instances} in use</span>
              </td>
              <td>
                <div class="row">
                  <button class="btn small" onClick={() => navigate(`/admin/catalog/${t.id}/edit`)}>
                    Edit
                  </button>
                  <button class="btn small" onClick={() => toggle(t)}>
                    {t.enabled ? "Hide" : "Enable"}
                  </button>
                  <button
                    class="btn small danger"
                    onClick={() => {
                      if (confirm(`Delete ${t.name} from the catalog? ${t.instances} instance(s) keep running as they are.`)) act(() => api.deleteTemplate(t.id));
                    }}
                  >
                    Delete
                  </button>
                </div>
              </td>
            </tr>
          ))}
        </table>
      )}
    </div>
  );
}
