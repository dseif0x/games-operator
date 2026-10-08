import { useEffect, useState } from "preact/hooks";
import { api, setCsrf, type User } from "./api";
import { navigate, useRoute } from "./router";
import { Login } from "./pages/Login";
import { AppList } from "./pages/AppList";
import { NewApp } from "./pages/NewApp";
import { AppPage } from "./pages/AppPage";
import { Pair } from "./pages/Pair";
import { Account } from "./pages/Account";

export function App() {
  const route = useRoute();
  const [user, setUser] = useState<User | null | undefined>(undefined);

  useEffect(() => {
    api
      .me()
      .then((r) => {
        setCsrf(r.csrf);
        setUser(r.user);
      })
      .catch(() => setUser(null));
  }, []);

  useEffect(() => {
    if (user === null && route.path !== "/login") navigate("/login", true);
    if (user && route.path === "/login") navigate("/", true);
  }, [user, route.path]);

  if (user === undefined) return <div class="page muted">Loading…</div>;

  if (!user || route.path === "/login") {
    return (
      <Login
        onLogin={(u, csrf) => {
          setCsrf(csrf);
          setUser(u);
          // The player (/play/…) lives outside this app: a login that started
          // there goes back there with a full navigation.
          const next = new URLSearchParams(location.search).get("next") || "";
          if (next.startsWith("/") && !next.startsWith("//")) {
            location.href = next;
            return;
          }
          navigate("/", true);
        }}
      />
    );
  }

  const logout = async () => {
    await api.logout().catch(() => undefined);
    setUser(null);
  };

  switch (route.path) {
    case "/apps/:id":
      return <AppPage id={route.params.id} user={user} onLogout={logout} />;
    case "/new":
      return <NewApp user={user} onLogout={logout} />;
    case "/apps/:id/edit":
      return <NewApp user={user} onLogout={logout} edit={route.params.id} />;
    case "/pair":
      return <Pair user={user} onLogout={logout} />;
    case "/account":
      return <Account user={user} onLogout={logout} onUser={setUser} />;
    default:
      return <AppList user={user} onLogout={logout} />;
  }
}
