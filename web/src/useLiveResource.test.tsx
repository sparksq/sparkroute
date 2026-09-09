import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { useLiveResource } from "./useLiveResource";

afterEach(cleanup);

it("ignores an old poll that completes after an explicit refresh", async () => {
  const pending: ((value: number) => void)[] = [];
  const loader = vi.fn((_signal: AbortSignal) => new Promise<number>(resolve => pending.push(resolve)));
  const { result } = renderHook(() => useLiveResource(true, loader));
  await waitFor(() => expect(loader).toHaveBeenCalledTimes(1));
  act(() => { void result.current.refresh(); });
  expect(loader.mock.calls[0]![0].aborted).toBe(true);
  await act(async () => { pending[1]!(2); });
  expect(result.current.data).toBe(2);
  await act(async () => { pending[0]!(1); });
  expect(result.current.data).toBe(2);
});
