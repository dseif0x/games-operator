import { useEffect, useState } from "preact/hooks";
import { api, type Pairing, type PendingPair, type User } from "../api";
import { Nav } from "../components/Nav";
import { timeAgo } from "../util";

// Pair lists Moonlight clients waiting for their PIN and the ones already
// paired with this account.
export function Pair(props: { user: User; onLogout: () => void }) {
  const [pairings, setPairings] = useState<Pairing[]>([]);
  const [pending, setPending] = useState<PendingPair[]>([]);
  const [pins, setPins] = useState<Record<string, string>>({});
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");

  const load = () =>
    api
      .pairings()
      .then((r) => {
        setPairings(r.pairings);
        setPending(r.pending);
      })
      .catch((e) => setError((e as Error).message));

  useEffect(() => {
    load();
    const t = setInterval(load, 2000);
    return () => clearInterval(t);
  }, []);

  const submit = async (secret: string) => {
    setError("");
    setMsg("");
    try {
      await api.submitPin(secret, pins[secret] || "");
      setMsg("PIN sent. Moonlight finishes the pairing on its own.");
      setPins((p) => ({ ...p, [secret]: "" }));
      setTimeout(load, 1500);
    } catch (e) {
      setError((e as Error).message);
    }
  };

  const unpair = async (p: Pairing) => {
    if (!confirm(`Forget ${p.name || p.id.slice(0, 12)}?`)) return;
    try {
      await api.deletePairing(p.id);
      load();
    } catch (e) {
      setError((e as Error).message);
    }
  };

  return (
    <div class="page">
      <Nav user={props.user} onLogout={props.onLogout} />
      <h2 style="margin:0 0 12px">Pair a Moonlight client</h2>
      <p class="muted">
        In Moonlight, add this host and press pair. It shows a four-digit PIN; type it below. The client is then bound to <b>{props.user.username}</b>{" "}
        and sees your apps.
      </p>
      {error && <div class="error">{error}</div>}
      {msg && <div class="ok">{msg}</div>}
      <div class="card">
        <h3>Waiting for a PIN</h3>
        {pending.length === 0 ? (
          <div class="muted">No client is pairing right now.</div>
        ) : (
          pending.map((p) => (
            <form
              key={p.secret}
              class="row"
              onSubmit={(e) => {
                e.preventDefault();
                submit(p.secret);
              }}
            >
              <span>
                Client at <code>{p.client_ip}</code> · {timeAgo(p.started_at)}
              </span>
              <input
                class="pin"
                inputMode="numeric"
                pattern="[0-9]{4}"
                maxLength={4}
                placeholder="PIN"
                value={pins[p.secret] || ""}
                onInput={(e) => setPins((cur) => ({ ...cur, [p.secret]: (e.target as HTMLInputElement).value }))}
                autoFocus
              />
              <button class="btn primary small">Pair</button>
            </form>
          ))
        )}
      </div>
      <div class="card" style="margin-top:12px">
        <h3>Paired clients</h3>
        {pairings.length === 0 ? (
          <div class="muted">None yet.</div>
        ) : (
          <table class="kv">
            {pairings.map((p) => (
              <tr key={p.id}>
                <td>{p.name || "client"}</td>
                <td class="mono muted">{p.id.slice(0, 16)}…</td>
                <td>paired {timeAgo(p.created_at)}</td>
                <td>seen {timeAgo(p.last_seen_at)}</td>
                <td>
                  <button class="btn small danger" onClick={() => unpair(p)}>
                    Forget
                  </button>
                </td>
              </tr>
            ))}
          </table>
        )}
      </div>
    </div>
  );
}
