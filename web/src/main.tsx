import { render } from "preact";
import { App } from "./App";
import { applyTheme, currentTheme } from "./theme";
import "./styles.css";

applyTheme(currentTheme());
render(<App />, document.getElementById("app")!);
