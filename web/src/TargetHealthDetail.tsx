// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import type { TargetStatus } from "./types";
import { humanize, formatTime as formatStatusTime } from "./RuntimeWorkspace";

export function TargetHealthDetail({
  target,
}: {
  target: TargetStatus;
}) {
  const failures = target.recent_failures ?? [];
  const concurrency = target.max_concurrency > 0
    ? `${target.active_requests} / ${target.max_concurrency}`
    : `${target.active_requests} / unlimited`;
  return (
    <section className="target-health-detail" aria-label={`${target.deployment} target health`}>
      <div className="target-detail-heading">
        <h4>Routing health</h4>
      </div>
      <dl className="target-detail-grid">
        <TargetFact label="Admission" value={target.admission_available ? "Admitting" : "Unavailable"} />
        <TargetFact label="Circuit" value={humanize(target.circuit_state)} />
        <TargetFact label="Concurrency" value={concurrency} />
        <TargetFact label="Consecutive failures" value={String(target.consecutive_failures)} />
        <TargetFact label="Rolling samples" value={`${target.failures} failures / ${target.samples}`} />
        <TargetFact label="Ejection count" value={String(target.ejection_count)} />
        <TargetFact label="Ejected until" value={target.ejected_until ? formatStatusTime(target.ejected_until) : "Not ejected"} />
        <TargetFact label="Half-open probe" value={target.half_open_probe_active ? "In flight" : "Inactive"} />
      </dl>
      <div className="recent-failure-heading">
        <div>
          <h4>Recent target failures</h4>
          <p>Newest first · maximum eight retained per target</p>
        </div>
        <span>{failures.length}</span>
      </div>
      {failures.length ? (
        <ol className="recent-failure-list">
          {failures.map((failure, index) => (
            <li key={`${failure.occurred_at}-${failure.failure_class}-${index}`}>
              <code>{failure.failure_class}</code>
              <span>{formatStatusTime(failure.occurred_at)}</span>
            </li>
          ))}
        </ol>
      ) : (
        <p className="recent-failure-empty">No target failures are retained on this replica.</p>
      )}
      <p className="content-boundary-note">
        This projection contains normalized failure classes and timestamps only. It excludes request content, raw errors, upstream bodies, request IDs, URLs, headers, and credentials, and resets with replica-local target state.
      </p>
    </section>
  );
}

export function TargetFact({ label, value }: { label: string; value: string }) {
  return <div><dt>{label}</dt><dd>{value}</dd></div>;
}
