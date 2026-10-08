import { useState } from "preact/hooks";
import { api, type User } from "../api";
import { Nav } from "../components/Nav";

// Account manages the API token moonlight-web uses to pair on its own.
export function Account(props: { user: User; onLogout: () => void; onUser: (u: User) => void }) {
  const [token, setToken] = useState<{ token: string; api_url: string } | null>(null);
  const [error, setError] = useState("");

  const generate = async () => {
    setError("");
    try {
      const t = await api.newApiToken();
      setToken(t);
      props.onUser({ ...props.user, has_api_token: true });
    } catch (e) {
      setError((e as Error).message);
    }
  };
  const revoke = async () => {
    setError("");
    try {
      await api.deleteApiToken();
      setToken(null);
      props.onUser({ ...props.user, has_api_token: false });
    } catch (e) {
      setError((e as Error).message);
    }
  };

  return (
    <div class="page">
      <Nav user={props.user} onLogout={props.onLogout} />
      <h2 style="margin:0 0 12px">Account</h2>
      {error && <div class="error">{error}</div>}
      <div class="card">
        <h3>moonlight-web auto-pairing</h3>
        <p class="muted">
          moonlight-web can pair with this host without you typing a PIN: give it a <b>Wolf</b> backend with the API URL and token below (host card → ⋯ →
          Backend). Pairings it creates belong to <b>{props.user.username}</b>.
        </p>
        {token ? (
          <table class="kv">
            <tr>
              <td>API URL</td>
              <td class="mono">{token.api_url}</td>
            </tr>
            <tr>
              <td>Token</td>
              <td class="mono">{token.token}</td>
            </tr>
          </table>
        ) : (
          <p class="muted">{props.user.has_api_token ? "A token exists. Generating a new one replaces it." : "No token yet."}</p>
        )}
        <div class="row" style="margin-top:12px">
          <button class="btn primary" onClick={generate}>
            {props.user.has_api_token ? "Regenerate token" : "Generate token"}
          </button>
          {props.user.has_api_token && (
            <button class="btn danger" onClick={revoke}>
              Revoke
            </button>
          )}
        </div>
      </div>
    </div>
  );
}
