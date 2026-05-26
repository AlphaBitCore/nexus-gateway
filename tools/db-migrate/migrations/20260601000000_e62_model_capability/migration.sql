-- E62 multimodal foundation — model capability matrix
ALTER TABLE "Model"
    ADD COLUMN "inputModalities"  TEXT[] NOT NULL DEFAULT ARRAY['text'],
    ADD COLUMN "outputModalities" TEXT[] NOT NULL DEFAULT ARRAY['text'],
    ADD COLUMN "lifecycle"        TEXT   NOT NULL DEFAULT 'ga',
    ADD COLUMN "capabilityJson"   JSONB;

-- Embedding models: outputModalities=['embedding']; capabilityJson per-model
UPDATE "Model" SET "outputModalities" = ARRAY['embedding']
  WHERE "type" = 'embedding';

-- OpenAI text-embedding-3-small (https://platform.openai.com/docs/guides/embeddings, observed 2026-05-19)
UPDATE "Model"
  SET "capabilityJson" = '{"embeddings":{"max_input_tokens":8191,"supported_dimensions":[512,1024,1536],"default_dimension":1536,"max_batch_size":2048,"supported_encoding_formats":["float","base64"]}}'::jsonb
  WHERE "code" = 'text-embedding-3-small';

-- OpenAI text-embedding-3-large (observed 2026-05-19)
UPDATE "Model"
  SET "capabilityJson" = '{"embeddings":{"max_input_tokens":8191,"supported_dimensions":[256,512,1024,3072],"default_dimension":3072,"max_batch_size":2048,"supported_encoding_formats":["float","base64"]}}'::jsonb
  WHERE "code" = 'text-embedding-3-large';

-- OpenAI text-embedding-ada-002 (legacy — no dimensions; observed: "400 Unrecognized request argument supplied: dimensions" — 2026-05-19)
UPDATE "Model"
  SET "capabilityJson" = '{"embeddings":{"max_input_tokens":8191,"default_dimension":1536,"max_batch_size":2048,"supported_encoding_formats":["float","base64"]}}'::jsonb
  WHERE "code" = 'text-embedding-ada-002';

-- Gemini text-embedding-004 (https://ai.google.dev/gemini-api/docs/embeddings, observed 2026-05-19)
UPDATE "Model"
  SET "capabilityJson" = '{"embeddings":{"max_input_tokens":2048,"supported_dimensions":[768],"default_dimension":768,"max_batch_size":100,"supported_task_types":["RETRIEVAL_QUERY","RETRIEVAL_DOCUMENT","SEMANTIC_SIMILARITY","CLASSIFICATION","CLUSTERING","QUESTION_ANSWERING","FACT_VERIFICATION"]}}'::jsonb
  WHERE "code" = 'text-embedding-004';

-- Gemini gemini-embedding-001 (observed 2026-05-19)
UPDATE "Model"
  SET "capabilityJson" = '{"embeddings":{"max_input_tokens":2048,"supported_dimensions":[768,1536,3072],"default_dimension":3072,"max_batch_size":100,"supported_task_types":["RETRIEVAL_QUERY","RETRIEVAL_DOCUMENT","SEMANTIC_SIMILARITY","CLASSIFICATION","CLUSTERING","QUESTION_ANSWERING","FACT_VERIFICATION"]}}'::jsonb
  WHERE "code" = 'gemini-embedding-001';
