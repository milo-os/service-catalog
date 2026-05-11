import { vitePlugin as remix } from "@remix-run/dev";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";
import { resolve } from "path";

export default defineConfig({
  plugins: [
    tailwindcss(),
    remix({
      ssr: true,
    }),
  ],
  resolve: {
    alias: {
      "~": resolve(__dirname, "./app"),
    },
  },
  optimizeDeps: {
    exclude: ["@remix-run/react"],
    include: [
      "@datum-cloud/datum-ui/alert",
      "@datum-cloud/datum-ui/badge",
      "@datum-cloud/datum-ui/breadcrumb",
      "@datum-cloud/datum-ui/button",
      "@datum-cloud/datum-ui/card",
      "@datum-cloud/datum-ui/checkbox",
      "@datum-cloud/datum-ui/dialog",
      "@datum-cloud/datum-ui/empty-content",
      "@datum-cloud/datum-ui/input",
      "@datum-cloud/datum-ui/label",
      "@datum-cloud/datum-ui/multi-select",
      "@datum-cloud/datum-ui/page-title",
      "@datum-cloud/datum-ui/radio-group",
      "@datum-cloud/datum-ui/select",
      "@datum-cloud/datum-ui/separator",
      "@datum-cloud/datum-ui/sidebar",
      "@datum-cloud/datum-ui/table",
      "@datum-cloud/datum-ui/tag-input",
      "@datum-cloud/datum-ui/textarea",
      "@datum-cloud/datum-ui/toast",
      "lucide-react",
      "js-yaml",
    ],
  },
  server: {
    host: "0.0.0.0",
    port: 3000,
  },
});
