#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Scitrera LLC
# SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
# SPDX-License-Identifier: AGPL-3.0-only
"""Run PostgreSQL adapter integration tests in an isolated disposable container."""
import argparse
import os
from pathlib import Path
import secrets
import subprocess
import time

ROOT = Path(__file__).resolve().parents[1]
PACKAGES = ("ledger", "runtime", "config", "clientcredentials", "savedtrace")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default="go")
    parser.add_argument("--image", default="postgres:17.11-alpine")
    args = parser.parse_args()
    name = "sparkroute-postgres-test-" + secrets.token_hex(6)
    # Disposable synthetic credential, used only by this loopback test container.
    password = secrets.token_hex(24)
    subprocess.run(["docker", "run", "-d", "--name", name,
                    "--label", "purpose=sparkroute-postgres-tests",
                    "-p", "127.0.0.1::5432", "--tmpfs", "/var/lib/postgresql/data:rw,size=512m",
                    "-e", "POSTGRES_PASSWORD=" + password, args.image], check=True, stdout=subprocess.DEVNULL)
    try:
        deadline = time.monotonic() + 45
        while subprocess.run(["docker", "exec", name, "pg_isready", "-U", "postgres"],
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode:
            if time.monotonic() > deadline:
                raise RuntimeError("disposable PostgreSQL did not become ready")
            time.sleep(0.25)
        port = subprocess.check_output(["docker", "port", name, "5432/tcp"], text=True).strip().rsplit(":", 1)[1]
        for package in PACKAGES:
            database = "test_" + package
            subprocess.run(["docker", "exec", name, "createdb", "-U", "postgres", database], check=True)
            url = "postgres://postgres:" + password + "@127.0.0.1:" + port + "/" + database + "?sslmode=disable"
            env = {**os.environ, "GOWORK": "off", "SPARKROUTE_TEST_POSTGRES_BACKEND": "postgres",
                   "SPARKROUTE_TEST_POSTGRES_URL": url, "SPARKROUTE_TEST_TRACE_POSTGRES_URL": url}
            subprocess.run([args.go, "test", "-count=1", "./pkg/" + package + "/postgres"],
                           cwd=ROOT, env=env, check=True)
    finally:
        subprocess.run(["docker", "rm", "-f", name], check=True, stdout=subprocess.DEVNULL)


if __name__ == "__main__":
    main()
