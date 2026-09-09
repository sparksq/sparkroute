// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { useCallback, useEffect, useRef, useState } from "react";

// Each projection refreshes independently. A failed refresh retains its last
// snapshot and marks it stale; an older response cannot replace a newer one.
export function useLiveResource<T>(
  enabled: boolean,
  loader: (signal: AbortSignal) => Promise<T>,
  initial?: T,
  refreshKey = 0,
) {
  const [data, setData] = useState(initial);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(enabled && initial === undefined);
  const active = useRef<AbortController | undefined>(undefined);
  const inFlight = useRef(false);
  const refresh = useCallback(async () => {
    if (!enabled) return;
    active.current?.abort();
    const request = new AbortController();
    active.current = request;
    inFlight.current = true;
    setLoading(true);
    try {
      const next = await loader(request.signal);
      if (request.signal.aborted) return;
      setData(next);
      setError("");
    } catch (caught) {
      if (request.signal.aborted) return;
      setError(caught instanceof Error ? caught.message : "Status request failed");
    } finally {
      if (!request.signal.aborted) { inFlight.current = false; setLoading(false); }
    }
  }, [enabled, loader]);

  useEffect(() => {
    if (!enabled) return;
    if (initial === undefined || refreshKey !== 0) void refresh();
    const timer = setInterval(() => {
      if (!document.hidden && !inFlight.current) void refresh();
    }, 5000);
    return () => { clearInterval(timer); active.current?.abort(); inFlight.current = false; };
  }, [enabled, initial, refresh, refreshKey]);

  return { data, error, loading, refresh };
}
