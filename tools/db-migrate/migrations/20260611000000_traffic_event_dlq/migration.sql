-- traffic_event_dlq — dead-letter queue for traffic_event MQ messages.
--
-- The Hub TrafficEventWriter consumer (packages/nexus-hub/internal/jobs/
-- consumer/traffic.go) previously NAK'd every non-poison-pill batch error,
-- which made the broker redeliver forever until JetStream's 6h MaxAge ran
-- out. A single persistent DB issue (constraint violation, missing column,
-- bad enum cast) thus turned into ever-growing redeliveries pinning the
-- stream until eviction.
--
-- The new flow: after redeliveryThresholdAttempts (default 5) of the
-- consumer pulling the same message, the consumer ACKs it and writes the
-- raw bytes here instead. Operators inspect the DLQ via psql (admin UI
-- surface is a follow-up); after the underlying bug is fixed they can
-- requeue the messages back to nexus.event.* or hand-fix the resulting
-- traffic_event row directly.
--
-- Indexes:
--   msg_id          — humans correlating a DLQ row back to a known
--                     external request id pasted from a support ticket.
--   dlq_inserted_at — admin UI's "what just broke?" view sorts by insert
--                     time descending; partial DESC index doubles as a
--                     covering sort.
CREATE TABLE traffic_event_dlq (
  id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  msg_id          TEXT        NOT NULL,
  subject         TEXT        NOT NULL,
  payload         BYTEA       NOT NULL,
  delivery_count  INT         NOT NULL,
  last_error      TEXT,
  first_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  dlq_inserted_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX traffic_event_dlq_msg_id_idx
  ON traffic_event_dlq (msg_id);

CREATE INDEX traffic_event_dlq_inserted_at_idx
  ON traffic_event_dlq (dlq_inserted_at DESC);
