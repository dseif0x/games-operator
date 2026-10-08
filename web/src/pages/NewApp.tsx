import { useEffect, useState } from "preact/hooks";
import { api, type App, type CreateAppRequest, type Defaults, type Preset, type User } from "../api";
import { Nav } from "../components/Nav";
import { navigate } from "../router";
import { keyValueLines, parseKeyValues } from "../util";

// NewApp creates an app, or with `edit` set replaces the settings of a
// stopped one: same form, the fields that shape the volume locked.
export function NewApp(props: { user: User; onLogout: () => void; edit?: string }) {
  const editing = !!props.edit;
  const [presets, setPresets] = useState<Preset[]>([]);
  const [defaults, setDefaults] = useState<Defaults | null>(null);
  const [name, setName] = useState("");
  const [preset, setPreset] = useState("steam");
  const [image, setImage] = useState("");
  const [iconUrl, setIconUrl] = useState("");
  const [hdr, setHdr] = useState(false);
  const [pvcSize, setPvcSize] = useState("");
  const [storageClass, setStorageClass] = useState("");
  const [cpu, setCpu] = useState("");
  const [memory, setMemory] = useState("");
  const [extended, setExtended] = useState("");
  const [env, setEnv] = useState("");
  const [command, setCommand] = useState("");
  const [hostIpc, setHostIpc] = useState<boolean | null>(null);
  const [caps, setCaps] = useState("");
  const [editable, setEditable] = useState<boolean | null>(editing ? null : true);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api
      .apps()
      .then((r) => {
        setPresets(r.presets);
        setDefaults(r.defaults);
      })
      .catch((e) => setError((e as Error).message));
  }, []);

  useEffect(() => {
    if (!props.edit) return;
    api
      .app(props.edit)
      .then((a: App) => {
        setName(a.name);
        setPreset(a.preset);
        setImage(a.image);
        setIconUrl(a.icon_url);
        setHdr(a.hdr);
        setPvcSize(a.pvc_size);
        setStorageClass(a.storage_class);
        setCpu(a.resources?.limits?.cpu || "");
        setMemory(a.resources?.limits?.memory || "");
        setExtended(keyValueLines(a.resources?.limits, ["cpu", "memory"]));
        setEnv(keyValueLines(a.env));
        setCommand(a.command);
        setHostIpc(a.host_ipc);
        setCaps(a.capabilities.join(" "));
        setEditable(a.state === "stopped" || a.state === "failed");
      })
      .catch((e) => setError((e as Error).message));
  }, [props.edit]);

  const current = presets.find((p) => p.key === preset);

  const submit = async (e: Event) => {
    e.preventDefault();
    setError("");
    const envParsed = parseKeyValues(env);
    if (envParsed.error) return setError(envParsed.error);
    const ext = parseKeyValues(extended);
    if (ext.error) return setError(ext.error);
    const req: CreateAppRequest = {
      name,
      preset,
      image: image.trim() || undefined,
      icon_url: iconUrl.trim() || undefined,
      hdr,
      command: command.trim() || undefined,
      env: envParsed.values,
      resources: { requests: {}, limits: { ...(cpu ? { cpu } : {}), ...(memory ? { memory } : {}), ...ext.values } },
      capabilities: caps.trim() ? caps.trim().split(/[\s,]+/) : undefined,
    };
    if (hostIpc !== null) req.host_ipc = hostIpc;
    if (!editing) {
      if (pvcSize.trim()) req.pvc_size = pvcSize.trim();
      if (storageClass.trim()) req.storage_class = storageClass.trim();
    }
    setBusy(true);
    try {
      const a = editing ? await api.updateApp(props.edit!, req) : await api.createApp(req);
      navigate(`/apps/${a.id}`);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const input = (value: string, set: (v: string) => void, placeholder = "", disabled = false) => (
    <input value={value} placeholder={placeholder} disabled={disabled} onInput={(e) => set((e.target as HTMLInputElement).value)} />
  );

  return (
    <div class="page">
      <Nav user={props.user} onLogout={props.onLogout} />
      <h2 style="margin:0 0 12px">{editing ? "Edit app" : "New app"}</h2>
      {editable === false && <div class="error">Stop the app before editing it.</div>}
      <form class="card" onSubmit={submit}>
        <label>Name (shown in Moonlight)</label>
        {input(name, setName, "Steam")}
        <label>Preset</label>
        <div class="presets">
          {presets.map((p) => (
            <button
              type="button"
              key={p.key}
              class={`preset-card ${preset === p.key ? "selected" : ""}`}
              onClick={() => {
                setPreset(p.key);
                if (!editing && !name) setName(p.custom ? "" : p.title);
              }}
            >
              {p.icon_url ? <img src={p.icon_url} alt="" /> : <span class="icon placeholder">?</span>}
              <span>
                <b>{p.title}</b>
                <small>{p.description}</small>
              </span>
            </button>
          ))}
        </div>
        {current?.custom || image ? (
          <>
            <label>Image</label>
            {input(image, setImage, current?.image || "ghcr.io/games-on-whales/…:edge")}
          </>
        ) : null}
        <div class="form-grid">
          <div>
            <label>Icon URL</label>
            {input(iconUrl, setIconUrl, current?.icon_url || "https://…/icon.png")}
          </div>
          <div>
            <label>Home volume size {editing ? "(fixed)" : ""}</label>
            {input(pvcSize, setPvcSize, defaults?.pvc_size || "50Gi", editing)}
          </div>
          <div>
            <label>Storage class {editing ? "(fixed)" : ""}</label>
            {input(storageClass, setStorageClass, defaults?.storage_class || "cluster default", editing)}
          </div>
          <div>
            <label>CPU limit</label>
            {input(cpu, setCpu, defaults?.resources?.limits?.cpu || "8")}
          </div>
          <div>
            <label>Memory limit</label>
            {input(memory, setMemory, defaults?.resources?.limits?.memory || "16Gi")}
          </div>
        </div>
        <label class="checkbox">
          <input type="checkbox" checked={hdr} onChange={(e) => setHdr((e.target as HTMLInputElement).checked)} /> Advertise HDR support
        </label>
        <details>
          <summary>Advanced</summary>
          <label>Extended resources (one per line, e.g. nvidia.com/gpu=1)</label>
          <textarea value={extended} onInput={(e) => setExtended((e.target as HTMLTextAreaElement).value)} placeholder={"nvidia.com/gpu=1"} />
          <label>Environment (one KEY=value per line)</label>
          <textarea value={env} onInput={(e) => setEnv((e.target as HTMLTextAreaElement).value)} placeholder={"PROTON_LOG=1"} />
          <label>Capabilities (space separated; empty = preset)</label>
          {input(caps, setCaps, (current?.capabilities || []).join(" ") || "none")}
          <label class="checkbox">
            <input
              type="checkbox"
              checked={hostIpc ?? current?.host_ipc ?? false}
              onChange={(e) => setHostIpc((e.target as HTMLInputElement).checked)}
            />{" "}
            Share the node's IPC namespace (hostIPC)
          </label>
          <label>Launch command (empty = the standard wrapper that waits for Wolf's sockets, then runs the image entrypoint)</label>
          <textarea value={command} onInput={(e) => setCommand((e.target as HTMLTextAreaElement).value)} style="min-height:120px" />
        </details>
        {error && <div class="error">{error}</div>}
        <div class="row" style="margin-top:16px">
          <button class="btn primary" disabled={busy || editable === false}>
            {busy ? "Saving…" : editing ? "Save" : "Create app"}
          </button>
          <button type="button" class="btn" onClick={() => history.back()}>
            Cancel
          </button>
        </div>
      </form>
    </div>
  );
}
