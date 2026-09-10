// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { useState } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
import { RecoveryFields } from "./RecoveryFields";

afterEach(cleanup);

function Editor({ initial = {} }: { initial?: Record<string, unknown> }) {
  const [value, setValue] = useState(initial);
  return <><RecoveryFields value={value} onChange={setValue} /><output data-testid="policy">{JSON.stringify(value)}</output></>;
}

it("defaults off and clears hidden policy fields when disabled", () => {
  render(<Editor />);
  expect(screen.getByLabelText("When persistently unhealthy")).toHaveValue("");
  expect(screen.queryByLabelText("Automatic recovery limit")).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("When persistently unhealthy"), { target: { value: "stop" } });
  fireEvent.change(screen.getByLabelText("Automatic recovery limit"), { target: { value: "2" } });
  expect(JSON.parse(screen.getByTestId("policy").textContent!)).toEqual({ action: "stop", max_restarts: 2 });
  fireEvent.change(screen.getByLabelText("When persistently unhealthy"), { target: { value: "" } });
  expect(screen.getByTestId("policy")).toHaveTextContent("{}");
});

it("preserves existing timing settings while changing the recovery action", () => {
  const initial = { action: "restart", failed_probes: 5, unhealthy_for: "7m", drain_timeout: "45s", backoff: "2m", max_backoff: "20m", max_restarts: 4 };
  render(<Editor initial={initial} />);
  expect(screen.getByLabelText("Failed probes before recovery")).toHaveValue(5);
  expect(screen.getByLabelText("Minimum unhealthy duration")).toHaveValue("7m");
  fireEvent.change(screen.getByLabelText("When persistently unhealthy"), { target: { value: "stop" } });
  expect(JSON.parse(screen.getByTestId("policy").textContent!)).toEqual({ ...initial, action: "stop" });
});
