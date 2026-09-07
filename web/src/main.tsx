import { mountAdminApp } from "./public";

const root = document.getElementById("sparkroute-admin-root");
if (!root) throw new Error("admin application root is missing");

mountAdminApp(root, {
  productName: "SparkRoute",
});
