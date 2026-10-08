import type { User } from "../api";
import { Link } from "../router";
import { toggleTheme } from "../theme";

export function Nav(props: { user: User; onLogout: () => void; right?: preact.ComponentChildren }) {
  return (
    <nav class="nav">
      <Link href="/" class="brand">
        games-operator
      </Link>
      <Link href="/pair" class="btn small" title="Pair a Moonlight client">
        Pair
      </Link>
      <span class="spacer" />
      {props.right}
      <Link href="/account" class="btn small" title="Account">
        {props.user.username}
      </Link>
      <button class="btn small" onClick={toggleTheme} title="Toggle theme" aria-label="Toggle theme">
        ◐
      </button>
      <button class="btn small" onClick={props.onLogout}>
        Log out
      </button>
    </nav>
  );
}
