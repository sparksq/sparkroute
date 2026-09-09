// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Shared structured-UI choices. Other declarations remain supported in JSON
// and must survive edits to these visible capabilities.
export const capabilityOptions = [
  { value: "vision", label: "Vision" },
  { value: "file_input", label: "Files" },
] as const;

export function capabilitySelectionSummary(values: string[]) {
  const visible = capabilityOptions.filter(({ value }) => values.includes(value)).length;
  const hidden = values.length - visible;
  return `${visible} selected${hidden ? ` · ${hidden} in JSON` : ""}`;
}
