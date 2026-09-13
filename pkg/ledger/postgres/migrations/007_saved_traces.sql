-- Legacy migration slot retained for databases that already recorded
-- 007_saved_traces. The independently reusable saved-trace PostgreSQL package
-- now owns this schema and its migration history.
SELECT 1;
