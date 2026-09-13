-- Legacy migration slot retained for databases that already recorded
-- 001_timescaledb. Backend-neutral PostgreSQL setup intentionally performs no
-- extension work here; TimescaleDB setup lives in migrations/timescaledb.
SELECT 1;
