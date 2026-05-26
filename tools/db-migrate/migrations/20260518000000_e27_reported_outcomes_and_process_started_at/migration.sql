-- E27 (audit-driven): standardise the Reported channel so the Nodes page can
-- show per-key apply outcomes and per-process uptime without crawling logs.
--
-- reported_outcomes : { key: { appliedAt, appliedVersion, applyError } }
--                     Reset to {} on Thing process restart; repopulated by
--                     the next successful OnConfigChanged dispatch.
-- process_started_at: wall-clock the Thing process started, captured by Hub
--                     on the offline→online transition. Powers uptime + lets
--                     operators interpret an empty reported_outcomes as
--                     "fresh process, no apply yet" rather than "broken".

ALTER TABLE "thing"
  ADD COLUMN "reported_outcomes" JSONB NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN "process_started_at" TIMESTAMPTZ(3);
