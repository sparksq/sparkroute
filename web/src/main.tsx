// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { mountAdminApp } from "./public";

const root = document.getElementById("sparkroute-admin-root");
if (!root) throw new Error("admin application root is missing");

mountAdminApp(root, {
  productName: "SparkRoute",
});
