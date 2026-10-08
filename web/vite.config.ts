import { defineConfig } from "vite";
import preact from "@preact/preset-vite";

// The build lands in internal/ui/dist so `go build -tags ui` can embed it.
export default defineConfig({
  plugins: [preact()],
  // Relative asset URLs: index.html carries a <base href> the hub sets to
  // its mount path at runtime, so one build serves / and /hub alike.
  base: "./",
  build: {
    outDir: "../internal/ui/dist",
    emptyOutDir: true,
    sourcemap: false,
  },
  server: {
    port: 5173,
    proxy: {
      "/api": { target: "http://localhost:8080", changeOrigin: false },
    },
  },
});
