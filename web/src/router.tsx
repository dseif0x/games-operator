// A very small path router: the URL is the state.
import { useEffect, useState } from "preact/hooks";

export interface Route {
  path: string;
  params: Record<string, string>;
}

const listeners = new Set<() => void>();

/** Where the app is mounted, from <base href> ("" at the root, "/hub" below it). */
export const BASE = (() => {
  try {
    return new URL(document.baseURI).pathname.replace(/\/$/, "");
  } catch {
    return "";
  }
})();

/** Absolute URL path for an app-relative path such as "/apps/1". */
export function href(path: string): string {
  return BASE + path;
}

function current(): string {
  const p = location.pathname;
  return BASE && p.startsWith(BASE) ? p.slice(BASE.length) || "/" : p;
}

export function navigate(path: string, replace = false) {
  if (replace) history.replaceState(null, "", href(path));
  else history.pushState(null, "", href(path));
  listeners.forEach((l) => l());
}

export function useRoute(): Route {
  const [path, setPath] = useState(current());
  useEffect(() => {
    const update = () => setPath(current());
    listeners.add(update);
    addEventListener("popstate", update);
    return () => {
      listeners.delete(update);
      removeEventListener("popstate", update);
    };
  }, []);
  return match(path);
}

function match(path: string): Route {
  const parts = path.split("/").filter(Boolean);
  if (parts.length === 0) return { path: "/", params: {} };
  if (parts[0] === "login") return { path: "/login", params: {} };
  if (parts[0] === "account") return { path: "/account", params: {} };
  if (parts[0] === "pair") return { path: "/pair", params: {} };
  if (parts[0] === "new") return { path: "/new", params: {} };
  if (parts[0] === "add") return { path: "/add", params: {} };
  if (parts[0] === "admin" && parts[1] === "catalog" && parts[2] === "new") return { path: "/admin/catalog/new", params: {} };
  if (parts[0] === "admin" && parts[1] === "catalog" && parts[2] && parts[3] === "edit")
    return { path: "/admin/catalog/:id/edit", params: { id: parts[2] } };
  if (parts[0] === "admin") return { path: "/admin", params: { tab: parts[1] || "users" } };
  if (parts[0] === "apps" && parts[1] && parts[2] === "edit") return { path: "/apps/:id/edit", params: { id: parts[1] } };
  if (parts[0] === "apps" && parts[1]) return { path: "/apps/:id", params: { id: parts[1] } };
  return { path: "/", params: {} };
}

/** Intercept in-app link clicks so the SPA does not reload. */
export function Link(props: { href: string; class?: string; children: preact.ComponentChildren; title?: string }) {
  const onClick = (e: MouseEvent) => {
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
    e.preventDefault();
    navigate(props.href);
  };
  return (
    <a href={href(props.href)} class={props.class} title={props.title} onClick={onClick}>
      {props.children}
    </a>
  );
}
