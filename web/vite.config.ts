import { fileURLToPath, URL } from "node:url"
import react from "@vitejs/plugin-react"
import tailwindcss from "@tailwindcss/vite"
import { defineConfig } from "vite"

// The build is embedded into the yoho binary (internal/webui/dist).
// `npm run dev` proxies /api and /login to a running `yoho ui --port 7300`.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  build: {
    outDir: "../internal/webui/dist",
    emptyOutDir: true,
    assetsDir: "assets",
    sourcemap: false,
    chunkSizeWarningLimit: 600,
  },
  server: {
    proxy: {
      "/api": { target: "http://127.0.0.1:7300", headers: { host: "127.0.0.1:7300" } },
      "/login": { target: "http://127.0.0.1:7300", headers: { host: "127.0.0.1:7300" } },
    },
  },
})
