# Project decisions

## 2026-08-13: MMBridge remains independently deployed

SparkRoute will retain MMBridge as an independently versioned internal
service. The gateway Helm chart owns the typed, authenticated client
configuration, probes, status, and routing policy, but does not package or
manage the Python workload.

This keeps the inference gateway image language-neutral and multi-architecture,
allows MMBridge to evolve and scale on its own release schedule, and avoids
coupling gateway rollout safety to Python/model dependencies. A future umbrella
deployment may compose both independently versioned charts without changing
this service boundary.

## 2026-08-13: defer module and image namespace changes (superseded)

The initial provisional module, binary, image, and chart names were retained
while the joint project selected its canonical identity. This decision was
superseded before publication by the decision below.

## 2026-08-14: SparkRoute canonical identity

The converged product, binary, image, chart, and Go module use `SparkRoute`,
with canonical source at `https://github.com/sparksq/sparkroute` and clone URL
`git@github.com:sparksq/sparkroute.git`. Because no converged release was
published under the provisional identity, the rename carries no compatibility
aliases or migration surface.
