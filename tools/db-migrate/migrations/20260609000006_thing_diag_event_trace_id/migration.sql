-- thing_diag_event.trace_id — typed correlation column.
--
-- Today DiagEvent.Attrs (a JSONB map) carries the request id by
-- convention under the key "trace_id". That works for one-off log
-- inspection but "give me every diag row that belongs to this trace"
-- has to walk the JSONB on every row and there is no index to lean on.
--
-- Promote it to a first-class column so the typical correlation query
-- ("trace X failed; what diag rows did each Thing emit while handling
-- it?") hits a real btree. Column is NULL-able because in-flight events
-- emitted by old Things before the upgrade do not stamp it; new emits
-- always carry it (SlogSink.Handle auto-extracts from slog attrs).
--
-- Index shape — (thing_id, trace_id, occurred_at DESC):
--   * thing_id is the most selective single column (every row carries
--     it, distribution matches active fleet size).
--   * trace_id narrows to the request inside that Thing.
--   * occurred_at DESC matches the admin /infrastructure/errors page
--     sort order so the index doubles as a covering sort.
ALTER TABLE thing_diag_event
  ADD COLUMN trace_id TEXT NULL;

CREATE INDEX thing_diag_event_thing_id_trace_id_occurred_at_idx
  ON thing_diag_event (thing_id, trace_id, occurred_at DESC);
