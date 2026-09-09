// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import react from "@vitejs/plugin-react";
import { readFileSync } from "node:fs";
import { defineConfig } from "vite";

const escapeHTML = (text: string) => text.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");
const legalFiles = ["LICENSE", "COPYRIGHT", "NOTICE", "THIRD_PARTY_NOTICES.md", "THIRD_PARTY_LICENSES.txt"];

export default defineConfig({
  base: "/admin/",
  plugins: [react(), {
    name: "sparkroute-license-notices",
    generateBundle() {
      const sections = legalFiles.map(name => `<section><h2>${name}</h2><pre>${escapeHTML(readFileSync(new URL(`../${name}`, import.meta.url), "utf8"))}</pre></section>`).join("\n");
      this.emitFile({type: "asset", fileName: "legal.html", source: `<!doctype html>
<!-- SPDX-FileCopyrightText: 2026 Scitrera LLC; 2026 Fox Engine Ltd.; upstream authors identified below
SPDX-License-Identifier: AGPL-3.0-only AND Apache-2.0 AND BSD-3-Clause AND BSD-2-Clause AND MIT -->
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>SparkRoute — License &amp; notices</title><link rel="stylesheet" href="legal.css"></head>
<body><main><h1>License &amp; notices</h1><p>SparkRoute is distributed under AGPL-3.0-only. Dependencies retain their respective licenses.</p><p><a href="./">Back to SparkRoute</a> · <a href="https://github.com/sparksq/sparkroute">Source repository</a></p>${sections}</main></body></html>\n`});
      this.emitFile({type: "asset", fileName: "legal.css", source: `/* SPDX-FileCopyrightText: 2026 Scitrera LLC; 2026 Fox Engine Ltd.
SPDX-License-Identifier: AGPL-3.0-only */
body{font:16px/1.6 system-ui,sans-serif;margin:2rem auto;padding:0 1rem;max-width:80rem}pre{white-space:pre-wrap;overflow-wrap:anywhere;font:14px/1.5 monospace}section{margin-top:3rem}a{color:#185b9d}\n`});
    },
  }],
  build: {
    outDir: "../pkg/adminui/ui",
    emptyOutDir: true,
    rolldownOptions: {output: {banner: "/*! SparkRoute: AGPL-3.0-only; React, React DOM and Scheduler: MIT. Copyright 2026 Scitrera LLC; Fox Engine Ltd.; Meta Platforms, Inc. and affiliates. See /admin/legal.html. */"}},
  },
  test: {
    environment: "jsdom",
    setupFiles: "./src/test/setup.ts",
  },
});
