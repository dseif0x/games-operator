export function timeAgo(iso: string | null | undefined): string {
  if (!iso) return "never";
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return "?";
  const s = Math.max(0, Math.round((Date.now() - t) / 1000));
  if (s < 5) return "just now";
  if (s < 60) return `${s}s ago`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.round(m / 60);
  if (h < 48) return `${h}h ago`;
  return `${Math.round(h / 24)}d ago`;
}

export function stateLabel(state: string, streaming?: boolean): string {
  if (state === "running") return streaming ? "Streaming" : "Ready";
  return state.charAt(0).toUpperCase() + state.slice(1);
}

/** Parse `key=value` lines (a bare key means an empty value). */
export function parseKeyValues(text: string): { values: Record<string, string>; error?: string } {
  const values: Record<string, string> = {};
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    const i = line.indexOf("=");
    const key = (i < 0 ? line : line.slice(0, i)).trim();
    if (!key) return { values, error: `missing key: ${line}` };
    values[key] = i < 0 ? "" : line.slice(i + 1).trim();
  }
  return { values };
}

export const keyValueLines = (m: Record<string, string | undefined> | undefined, skip: string[] = []) =>
  Object.entries(m || {})
    .filter(([k, v]) => !skip.includes(k) && v !== undefined)
    .map(([k, v]) => `${k}=${v}`)
    .join("\n");
