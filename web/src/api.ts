import { href } from "./router";
// Thin fetch wrapper for /api/v1. Cookie auth, CSRF header on writes.

export type Role = "admin" | "user";

/** A user's own limits; a missing field means the server default. */
export interface Quota {
  max_apps?: number;
  max_storage?: string;
}

/** Effective limits (0 / "" = unlimited) next to what the user has. */
export interface Usage {
  max_apps: number;
  max_storage: string;
  apps: number;
  storage: string;
}

export interface User {
  id: string;
  username: string;
  role: Role;
  has_api_token: boolean;
  disabled: boolean;
  quota: Quota;
  usage?: Usage;
}

export const isAdmin = (u: User | null | undefined) => !!u && u.role === "admin";

/** A Kubernetes-style resource list: cpu, memory and extended resources such as nvidia.com/gpu. */
export type ResourceList = { cpu?: string; memory?: string } & Record<string, string | undefined>;

export interface Resources {
  requests: ResourceList;
  limits: ResourceList;
}

export type AppState = "stopped" | "starting" | "running" | "stopping" | "failed" | "deleting";

export interface StreamInfo {
  client_ip: string;
  client_name: string;
  width: number;
  height: number;
  fps: number;
  started_at: string;
}

export interface App {
  id: string;
  owner_id: string;
  moonlight_id: number;
  /** The catalog entry this app follows ("" for an admin's custom app). */
  template_id: string;
  template_name?: string;
  name: string;
  preset: string;
  image: string;
  icon_url: string;
  hdr: boolean;
  command: string;
  pvc_size: string;
  storage_class: string;
  resources: Resources;
  env: Record<string, string>;
  host_ipc: boolean;
  capabilities: string[];
  state: AppState;
  state_reason: string;
  streaming: boolean;
  slot: number;
  stream_url: string;
  stream?: StreamInfo;
  pod_name: string;
  created_at: string;
  updated_at: string;
  last_active_at: string | null;
}

export interface Preset {
  key: string;
  title: string;
  description: string;
  image: string;
  icon_url: string;
  env: Record<string, string>;
  capabilities: string[];
  host_ipc: boolean;
  unconfined: boolean;
  custom: boolean;
}

export interface Defaults {
  pvc_size: string;
  storage_class: string;
  resources: Resources;
  max_resources: Resources;
  max_concurrent: number;
  browser_url: string;
  moonlight_host: string;
  max_apps: number;
  max_storage: string;
}

/** A catalog entry: an app definition users create instances of. */
export interface Template {
  id: string;
  name: string;
  description: string;
  preset: string;
  image: string;
  icon_url: string;
  hdr: boolean;
  command: string;
  pvc_size: string;
  storage_class: string;
  resources: Resources;
  env: Record<string, string>;
  host_ipc: boolean;
  capabilities: string[];
  enabled: boolean;
  instances: number;
}

/** An app as the admin overview lists it, with its owner's name. */
export interface OwnedApp extends App {
  owner: string;
}

export interface UserRequest {
  username?: string;
  password?: string;
  role?: Role;
  disabled?: boolean;
  quota?: Quota;
}

export interface AppEvent {
  id: number;
  at: string;
  kind: string;
  message: string;
}

export interface Pairing {
  id: string;
  name: string;
  created_at: string;
  last_seen_at: string | null;
}

export interface PendingPair {
  secret: string;
  client_ip: string;
  started_at: string;
}

export interface CreateAppRequest {
  /** Set: an instance of that catalog entry (only name is read besides). */
  template_id?: string;
  name: string;
  preset?: string;
  description?: string;
  enabled?: boolean;
  image?: string;
  icon_url?: string;
  hdr?: boolean;
  command?: string;
  pvc_size?: string;
  storage_class?: string;
  resources?: Resources;
  env?: Record<string, string>;
  host_ipc?: boolean;
  capabilities?: string[];
}

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

let csrf = "";

export function setCsrf(token: string) {
  csrf = token;
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET" && csrf) headers["X-CSRF-Token"] = csrf;
  const res = await fetch(href("/api/v1" + path), {
    method,
    headers,
    credentials: "same-origin",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let data: unknown = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = { error: text };
  }
  if (!res.ok) {
    const msg = (data as { error?: string } | null)?.error || res.statusText || `HTTP ${res.status}`;
    throw new ApiError(res.status, msg);
  }
  return data as T;
}

export const api = {
  login: (username: string, password: string) =>
    request<{ user: User; csrf: string }>("POST", "/auth/login", { username, password }),
  logout: () => request<{ ok: boolean }>("POST", "/auth/logout"),
  me: () => request<{ user: User; csrf: string }>("GET", "/auth/me"),

  apps: () => request<{ apps: App[]; presets: Preset[]; defaults: Defaults }>("GET", "/apps"),
  app: (id: string) => request<App>("GET", `/apps/${id}`),
  createApp: (req: CreateAppRequest) => request<App>("POST", "/apps", req),
  updateApp: (id: string, req: CreateAppRequest) => request<App>("PATCH", `/apps/${id}`, req),
  deleteApp: (id: string) => request<App>("DELETE", `/apps/${id}`),
  startApp: (id: string) => request<App>("POST", `/apps/${id}/start`),
  stopApp: (id: string) => request<App>("POST", `/apps/${id}/stop`),
  appEvents: (id: string) => request<{ events: AppEvent[] }>("GET", `/apps/${id}/events`),
  appLogs: async (id: string, container: string) => {
    const res = await fetch(href(`/api/v1/apps/${id}/logs?container=${encodeURIComponent(container)}`), { credentials: "same-origin" });
    const text = await res.text();
    if (!res.ok) {
      try {
        throw new ApiError(res.status, JSON.parse(text).error);
      } catch (e) {
        if (e instanceof ApiError) throw e;
        throw new ApiError(res.status, text);
      }
    }
    return text;
  },

  pairings: () => request<{ pairings: Pairing[]; pending: PendingPair[] }>("GET", "/pairings"),
  submitPin: (secret: string, pin: string) => request<{ ok: boolean }>("POST", "/pairings/pin", { secret, pin }),
  deletePairing: (id: string) => request<{ ok: boolean }>("DELETE", `/pairings/${id}`),

  newApiToken: () => request<{ token: string; api_url: string }>("POST", "/me/api-token"),
  deleteApiToken: () => request<{ ok: boolean }>("DELETE", "/me/api-token"),
  changePassword: (current_password: string, new_password: string) =>
    request<{ ok: boolean }>("POST", "/me/password", { current_password, new_password }),

  catalog: () => request<{ templates: Template[]; presets: Preset[]; defaults: Defaults }>("GET", "/catalog"),
  createTemplate: (req: CreateAppRequest) => request<Template>("POST", "/catalog", req),
  updateTemplate: (id: string, req: CreateAppRequest) => request<Template>("PATCH", `/catalog/${id}`, req),
  deleteTemplate: (id: string) => request<{ ok: boolean }>("DELETE", `/catalog/${id}`),

  users: () => request<{ users: User[]; defaults: { max_apps: number; max_storage: string } }>("GET", "/users"),
  createUser: (req: UserRequest) => request<User>("POST", "/users", req),
  updateUser: (id: string, req: UserRequest) => request<User>("PATCH", `/users/${id}`, req),
  deleteUser: (id: string) => request<{ ok: boolean }>("DELETE", `/users/${id}`),

  allApps: () => request<{ apps: OwnedApp[] }>("GET", "/admin/apps"),
  stopAnyApp: (id: string) => request<App>("POST", `/admin/apps/${id}/stop`),
  deleteAnyApp: (id: string) => request<App>("DELETE", `/admin/apps/${id}`),
};

/** Subscribe to app state changes over SSE (every user's with `all`). Returns a stop function. */
export function subscribeApps(onEvent: (type: "app" | "deleted", id: string, app?: App) => void, onOpen?: () => void, all = false): () => void {
  let es: EventSource | null = null;
  let stopped = false;
  let retry = 1000;
  const connect = () => {
    if (stopped) return;
    es = new EventSource(href(all ? "/api/v1/admin/events" : "/api/v1/apps/events"));
    es.onopen = () => {
      retry = 1000;
      onOpen?.();
    };
    const handler = (type: "app" | "deleted") => (ev: MessageEvent) => {
      try {
        const data = JSON.parse(ev.data) as { id: string; app?: App };
        onEvent(type, data.id, data.app);
      } catch {
        /* ignore malformed frames */
      }
    };
    es.addEventListener("app", handler("app"));
    es.addEventListener("deleted", handler("deleted"));
    es.onerror = () => {
      es?.close();
      es = null;
      if (!stopped) {
        setTimeout(connect, retry);
        retry = Math.min(retry * 2, 30000);
      }
    };
  };
  connect();
  return () => {
    stopped = true;
    es?.close();
  };
}
