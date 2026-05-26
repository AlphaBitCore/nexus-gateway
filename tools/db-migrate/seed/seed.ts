/**
 * Seed entry point — loads the baseline seed snapshot, then redacts sensitive
 * fields and resets ephemeral state (Thing online status, credential ciphertext).
 *
 * Workflow:
 *   1. Apply the baseline seed (tools/db-migrate/seed/data/seed-baseline.sql)
 *      as a single multi-statement query. It is a `pg_dump --data-only
 *      --column-inserts --disable-triggers` snapshot of the operational
 *      source database, with ciphertext columns redacted and high-cardinality
 *      event tables excluded (traffic_event*, thing_metric_rollup_*,
 *      metric_ops_*, rollup_watermark, job_run, config_change_event,
 *      AdminAuditLog, RefreshToken, RevokedToken, ScimToken,
 *      thing_diag_event). See seed/data/README.md for the regeneration
 *      procedure.
 *
 *   2. Overwrite every Credential row's encryptedKey/encryptionIv/encryptionTag
 *      with a fresh AES-256-GCM encryption of a fake plaintext string using
 *      the local CREDENTIAL_ENCRYPTION_KEY. Real provider API keys are never
 *      committed to the snapshot.
 *
 *   3. Mark every Thing row offline. Snapshot captures whatever status the
 *      source DB had at dump time; on a fresh local boot only services that
 *      actually start should report online. Real services re-register and
 *      flip themselves to online on first heartbeat (~1 s after boot).
 *
 * Usage: `npm run seed` (called by `prisma migrate reset` / `prisma db seed`).
 *
 * Regenerating the snapshot: see tools/db-migrate/seed/data/README.md.
 */

import 'dotenv/config';
import { readFileSync } from 'fs';
import { dirname, resolve } from 'path';
import { fileURLToPath } from 'url';
import { randomBytes, createCipheriv } from 'crypto';
import pg from 'pg';

const __dirname = dirname(fileURLToPath(import.meta.url));
const SEED_SQL = resolve(__dirname, 'data/seed-baseline.sql');
const TIME_SENSITIVE_RULES_JSON = resolve(__dirname, 'data/time-sensitive-rules.json');

function fakeEncrypt(plaintext: string): { ciphertext: string; iv: string; tag: string } {
  const keyHex = process.env.CREDENTIAL_ENCRYPTION_KEY;
  if (!keyHex || keyHex.length !== 64) {
    throw new Error(
      'seed: CREDENTIAL_ENCRYPTION_KEY must be a 64-char hex string ' +
        '(AES-256 key). Set it in tools/db-migrate/.env.',
    );
  }
  const masterKey = Buffer.from(keyHex, 'hex');
  const iv = randomBytes(12);
  const cipher = createCipheriv('aes-256-gcm', masterKey, iv, { authTagLength: 16 });
  const encrypted = Buffer.concat([cipher.update(plaintext, 'utf8'), cipher.final()]);
  const tag = cipher.getAuthTag();
  return {
    ciphertext: encrypted.toString('hex'),
    iv: iv.toString('hex'),
    tag: tag.toString('hex'),
  };
}

async function main() {
  if (!process.env.DATABASE_URL) {
    throw new Error('seed: DATABASE_URL is required. Set it in tools/db-migrate/.env.');
  }
  const client = new pg.Client({ connectionString: process.env.DATABASE_URL });
  await client.connect();
  try {
    console.log(`[seed] Loading baseline seed from ${SEED_SQL}`);
    const sql = readFileSync(SEED_SQL, 'utf8');
    // Wrap in a single transaction so a partial failure rolls back instead of
    // leaving the DB half-seeded. Without this, a duplicate-key error mid-file
    // would leave hundreds of inserted rows behind that conflict with the next
    // seed attempt.
    await client.query('BEGIN');
    try {
      await client.query(sql);
      await client.query('COMMIT');
    } catch (err) {
      await client.query('ROLLBACK');
      throw err;
    }
    console.log('[seed] Baseline seed applied.');

    // pg_dump output may set search_path to '' for the duration of the load;
    // restore it so subsequent unqualified table refs resolve in public.
    await client.query('SET search_path = public, pg_catalog');

    // Reset Thing online status. The snapshot captures whatever the source
    // DB had at dump time — typically a mix of online/offline/enrolled rows
    // from agents and services that exist there. On a fresh local boot
    // none of those are actually reachable, so the UI's "Nodes" page would
    // misleadingly show them as online. Real services re-register and flip
    // themselves to "online" on first heartbeat (~1 s after boot); agents
    // stay offline until enrolled. This UPDATE is idempotent.
    const thingReset = await client.query(
      `UPDATE "thing" SET status = 'offline'
        WHERE status IN ('online', 'enrolled')`,
    );
    console.log(`[seed] Reset ${thingReset.rowCount ?? 0} Thing row(s) to offline (services re-flip on boot).`);

    console.log('[seed] Redacting Credential ciphertext with local key...');
    const { rows: creds } = await client.query<{ id: string; name: string }>(
      'SELECT id, name FROM "Credential" ORDER BY name',
    );
    for (const c of creds) {
      const enc = fakeEncrypt(`sk-fake-${c.name}-${c.id.slice(0, 8)}`);
      await client.query(
        'UPDATE "Credential" SET "encryptedKey" = $1, "encryptionIv" = $2, "encryptionTag" = $3 WHERE id = $4',
        [enc.ciphertext, enc.iv, enc.tag, c.id],
      );
    }
    console.log(`[seed] Redacted ${creds.length} Credential row(s).`);

    // E48 emergency passthrough defaults — idempotent.
    //
    // The 3-tier passthrough config (E48-S1 migration) requires one
    // singleton row in gateway_passthrough_config_global plus one
    // thing_config_template row for ai-gateway/gateway_passthrough.
    // Both are inserted inside the migration file
    // (`20260517000000_e48_gateway_passthrough_config_3tier/migration.sql`)
    // on a fresh `prisma migrate deploy` AND already exist in the baseline. The
    // pg_dump snapshot (seed-baseline.sql) hasn't been regenerated since
    // these rows landed, so a fresh `npm run seed` against a DB that
    // somehow lost its post-migration data would end up without them.
    // These two ON CONFLICT DO NOTHING inserts are the belt-and-braces
    // guarantee that the rows always exist after seed, regardless of
    // which order/path got us here. No-op on a fresh-migrated DB.
    // PR-4 (configkey rename): the thing_config_template row uses the
    // new "gateway_passthrough" key; the gateway_passthrough_config_*
    // table family keeps its historical names (DB schema, not configKey).
    await client.query(`
      INSERT INTO gateway_passthrough_config_global (id, enabled, config)
      VALUES ('singleton', FALSE,
        '{"bypassHooks": false, "bypassCache": false, "bypassNormalize": false}'::jsonb)
      ON CONFLICT (id) DO NOTHING
    `);
    await client.query(`
      INSERT INTO thing_config_template (type, config_key, state, version, updated_at)
      VALUES (
        'ai-gateway',
        'gateway_passthrough',
        '{
          "global": {
            "enabled": false,
            "bypassHooks": false,
            "bypassCache": false,
            "bypassNormalize": false,
            "expiresAt": null,
            "enabledBy": null,
            "reason": null
          },
          "adapters": {},
          "providers": {}
        }'::jsonb,
        1,
        NOW()
      )
      ON CONFLICT (type, config_key) DO NOTHING
    `);
    console.log('[seed] Ensured E48 passthrough defaults (gateway_passthrough_config_global singleton + thing_config_template row).');

    // forceQUICFallbackBundles default (incident 2026-05-15 follow-up).
    //
    // The macOS NE proxy reads this list to decide which apps' UDP flows
    // to close, forcing a QUIC → TCP downgrade so our TLS-bump path can
    // see the request. Without a populated list, browsers prefer h3 to
    // ChatGPT/Cloudflare-fronted AI services and our TCP path never sees
    // the request — agent appears to capture nothing despite being
    // wired correctly. The 8 entries below cover the major desktop
    // browsers plus three Electron-based AI desktop apps that ship
    // Chromium's QUIC stack. Admins can extend or clear via the CP UI
    // (Settings → Agent → QUIC fallback bundles).
    //
    // Idempotent: jsonb_set with create_if_missing=false leaves the list
    // alone if admin has already customised it. The COALESCE arm handles
    // first-seed when the agent.settings row doesn't yet exist.
    const defaultQUICBundles = JSON.stringify([
      'com.google.Chrome',
      'com.google.Chrome.canary',
      'com.microsoft.edgemac',
      'com.brave.Browser',
      'company.thebrowser.Browser',
      'org.mozilla.firefox',
      'com.vivaldi.Vivaldi',
      'com.apple.Safari',
    ]);
    await client.query(
      `
      INSERT INTO system_metadata (key, value, updated_by, updated_at)
      VALUES ('agent.settings', jsonb_build_object('forceQUICFallbackBundles', $1::jsonb), 'seed', NOW())
      ON CONFLICT (key) DO UPDATE
      SET value = CASE
            WHEN system_metadata.value ? 'forceQUICFallbackBundles'
              THEN system_metadata.value
            ELSE jsonb_set(COALESCE(system_metadata.value, '{}'::jsonb), '{forceQUICFallbackBundles}', $1::jsonb, true)
          END,
          updated_at = NOW()
      `,
      [defaultQUICBundles],
    );
    console.log('[seed] Ensured agent.settings.forceQUICFallbackBundles default (8 browsers).');

    // E61-S5: embedding provider seed rows.
    //
    // (a) OpenAI text-embedding-3-small — added under the existing OpenAI
    //     provider row.  The Provider row already exists in the snapshot;
    //     we only add a Model row for the embedding model.
    // (b) Local-inference provider — a disabled OpenAI-compatible provider
    //     with a placeholder baseURL that admins edit at deploy time, plus
    //     a 384-dim placeholder embedding model.
    //
    // Both rows are idempotent (ON CONFLICT DO NOTHING) so re-running
    // seed on a populated DB is safe.
    //
    // NOTE: Dimension is NOT carried on the Model row by design — the
    // embedding model's effective dimension lives on the fleet-wide
    // semantic_cache_config singleton (set at probe time on the Cache
    // Embedding Settings page, E61-S6c). Per-Model dimension is therefore
    // intentionally omitted from these inserts.

    // Find the OpenAI provider id by name (stable across environments).
    const { rows: openaiRows } = await client.query<{ id: string }>(
      `SELECT id FROM "Provider" WHERE name = 'openai' LIMIT 1`,
    );
    if (openaiRows.length > 0) {
      const openaiProviderId = openaiRows[0].id;
      await client.query(
        `INSERT INTO "Model" (
           id, code, name, description, "providerId", "providerModelId",
           type, features, "inputPricePerMillion", "outputPricePerMillion",
           "maxContextTokens", aliases, enabled, "createdAt", "updatedAt"
         )
         VALUES (
           gen_random_uuid(),
           'text-embedding-3-small',
           'Text Embedding 3 Small',
           'OpenAI text-embedding-3-small (1536-dim). Used by the E61 semantic cache.',
           $1,
           'text-embedding-3-small',
           'embedding',
           '{}',
           0.02,
           0.0,
           8191,
           '{}',
           true,
           NOW(),
           NOW()
         )
         ON CONFLICT DO NOTHING`,
        [openaiProviderId],
      );
      console.log('[seed] Ensured text-embedding-3-small model under openai provider.');
    } else {
      console.log('[seed] openai provider not present in baseline — skipping text-embedding-3-small model (expected if you customised the snapshot).');
    }

    // Local-inference provider + model (disabled by default).
    await client.query(`
      INSERT INTO "Provider" (
        id, name, "displayName", description, adapter_type, "baseUrl",
        "pathPrefix", "apiVersion", region, enabled, headers,
        "createdAt", "updatedAt"
      )
      VALUES (
        gen_random_uuid(),
        'local-inference',
        'Local Inference Server',
        'OpenAI-compatible local inference server (vLLM / Ollama / LiteLLM). ' ||
        'Admin sets baseUrl to the server address at deploy time. ' ||
        'One server may host embedding, routing-decision LLM, and ai-guard endpoints.',
        'openai',
        'http://localhost:9001/v1',
        '',
        NULL,
        NULL,
        false,
        '{}',
        NOW(),
        NOW()
      )
      ON CONFLICT DO NOTHING
    `);

    const { rows: localRows } = await client.query<{ id: string }>(
      `SELECT id FROM "Provider" WHERE name = 'local-inference' LIMIT 1`,
    );
    if (localRows.length > 0) {
      const localProviderId = localRows[0].id;
      await client.query(
        `INSERT INTO "Model" (
           id, code, name, description, "providerId", "providerModelId",
           type, features, "inputPricePerMillion", "outputPricePerMillion",
           "maxContextTokens", aliases, enabled, "createdAt", "updatedAt"
         )
         VALUES (
           gen_random_uuid(),
           'local-bge-small',
           'Local BGE Small',
           'Placeholder embedding model for the local inference server. ' ||
           'Admin reconfigures providerModelId and dimension after deploying ' ||
           'their inference server.',
           $1,
           'BAAI/bge-small-en-v1.5',
           'embedding',
           '{}',
           0.0,
           0.0,
           512,
           '{}',
           false,
           NOW(),
           NOW()
         )
         ON CONFLICT DO NOTHING`,
        [localProviderId],
      );
      console.log('[seed] Ensured local-inference provider and local-bge-small model.');
    }

    // GLM (Zhipu AI) embedding models — embedding-2 and embedding-3 under the
    // existing GLM provider row. Both use the /api/paas/v4/embeddings endpoint.
    // Models: embedding-2 (1024-dim, stable), embedding-3 (1024-dim, latest).
    // Auth: JWT bearer signed from api_id.api_secret credential (handled by
    // the GLM transport layer; the embedding codec rejects integer token inputs).
    //
    // GLM embedding pricing as of 2026-05-20 (admin-configurable after seed):
    //   embedding-2: $0.0001 / M tokens (placeholder)
    //   embedding-3: $0.0001 / M tokens (placeholder)
    //
    // ON CONFLICT DO NOTHING: safe to re-run on a populated DB.
    const { rows: glmRows } = await client.query<{ id: string }>(
      `SELECT id FROM "Provider" WHERE name = 'glm' LIMIT 1`,
    );
    if (glmRows.length > 0) {
      const glmProviderId = glmRows[0].id;
      const capabilityJson = JSON.stringify({
        embeddings: {
          max_input_tokens: 8192,
          default_dimension: 1024,
          supported_dimensions: [1024],
        },
      });
      for (const model of [
        {
          code: 'embedding-2',
          name: 'GLM Embedding 2',
          providerModelId: 'embedding-2',
          description:
            'GLM embedding-2 (1024-dim). Stable ZhipuAI embedding model. ' +
            'Endpoint: /api/paas/v4/embeddings. Does not support integer token inputs.',
        },
        {
          code: 'embedding-3',
          name: 'GLM Embedding 3',
          providerModelId: 'embedding-3',
          description:
            'GLM embedding-3 (1024-dim). Latest ZhipuAI embedding model. ' +
            'Endpoint: /api/paas/v4/embeddings. Does not support integer token inputs.',
        },
      ]) {
        await client.query(
          `INSERT INTO "Model" (
             id, code, name, description, "providerId", "providerModelId",
             type, features, "inputPricePerMillion", "outputPricePerMillion",
             "maxContextTokens", aliases, enabled, "createdAt", "updatedAt",
             "capabilityJson"
           )
           VALUES (
             gen_random_uuid(),
             $1, $2, $3, $4, $5,
             'embedding',
             '{}',
             0.0001,
             0.0,
             8192,
             '{}',
             true,
             NOW(),
             NOW(),
             $6::jsonb
           )
           ON CONFLICT DO NOTHING`,
          [model.code, model.name, model.description, glmProviderId, model.providerModelId, capabilityJson],
        );
      }
      console.log('[seed] Ensured GLM embedding-2 and embedding-3 models under glm provider.');
    } else {
      console.log('[seed] glm provider not present in baseline — skipping embedding-2 / embedding-3 (expected; add the provider via CP UI to enable).');
    }

    // D-2: Voyage AI embedding provider + 5 models.
    //
    // Voyage AI is an embedding-only provider. Auth: Bearer API key.
    // Base URL: https://api.voyageai.com (handled by the voyage transport).
    // Models as of 2026-05-20 (all 1024-dim default, configurable via output_dimension):
    //   voyage-3-large:   1024-dim, highest accuracy
    //   voyage-3:         1024-dim, general purpose
    //   voyage-3-lite:    1024-dim, fast/low-cost
    //   voyage-code-3:    1024-dim, code embedding
    //   voyage-finance-2: 1024-dim, finance domain
    //
    // Provider adapter_type = 'voyage' (registered in FormatVoyage, builtins).
    // ON CONFLICT DO NOTHING: safe to re-run.
    await client.query(`
      INSERT INTO "Provider" (
        id, name, "displayName", description, adapter_type, "baseUrl",
        "pathPrefix", "apiVersion", region, enabled, headers,
        "createdAt", "updatedAt"
      )
      VALUES (
        gen_random_uuid(),
        'voyage',
        'Voyage AI',
        'Voyage AI embedding-only provider. Serves /v1/embeddings with string or array input. ' ||
        'Auth: Bearer API key. Models: voyage-3-large, voyage-3, voyage-3-lite, voyage-code-3, voyage-finance-2.',
        'voyage',
        'https://api.voyageai.com',
        '',
        NULL,
        NULL,
        true,
        '{}',
        NOW(),
        NOW()
      )
      ON CONFLICT DO NOTHING
    `);

    const { rows: voyageRows } = await client.query<{ id: string }>(
      `SELECT id FROM "Provider" WHERE name = 'voyage' LIMIT 1`,
    );
    if (voyageRows.length > 0) {
      const voyageProviderId = voyageRows[0].id;
      const voyageCapabilityJson = JSON.stringify({
        embeddings: {
          max_input_tokens: 32000,
          default_dimension: 1024,
          supported_dimensions: [256, 512, 1024],
        },
      });
      for (const model of [
        {
          code: 'voyage-3-large',
          name: 'Voyage 3 Large',
          providerModelId: 'voyage-3-large',
          description:
            'Voyage AI voyage-3-large (1024-dim). Highest accuracy general-purpose embedding model. ' +
            'Endpoint: /v1/embeddings. Auth: Bearer API key.',
        },
        {
          code: 'voyage-3',
          name: 'Voyage 3',
          providerModelId: 'voyage-3',
          description:
            'Voyage AI voyage-3 (1024-dim). General-purpose embedding model balancing accuracy and speed. ' +
            'Endpoint: /v1/embeddings. Auth: Bearer API key.',
        },
        {
          code: 'voyage-3-lite',
          name: 'Voyage 3 Lite',
          providerModelId: 'voyage-3-lite',
          description:
            'Voyage AI voyage-3-lite (1024-dim). Fast and low-cost embedding model. ' +
            'Endpoint: /v1/embeddings. Auth: Bearer API key.',
        },
        {
          code: 'voyage-code-3',
          name: 'Voyage Code 3',
          providerModelId: 'voyage-code-3',
          description:
            'Voyage AI voyage-code-3 (1024-dim). Optimized for code embedding (retrieval, search). ' +
            'Endpoint: /v1/embeddings. Auth: Bearer API key.',
        },
        {
          code: 'voyage-finance-2',
          name: 'Voyage Finance 2',
          providerModelId: 'voyage-finance-2',
          description:
            'Voyage AI voyage-finance-2 (1024-dim). Domain-specific embedding model for financial text. ' +
            'Endpoint: /v1/embeddings. Auth: Bearer API key.',
        },
      ]) {
        await client.query(
          `INSERT INTO "Model" (
             id, code, name, description, "providerId", "providerModelId",
             type, features, "inputPricePerMillion", "outputPricePerMillion",
             "maxContextTokens", aliases, enabled, "createdAt", "updatedAt",
             "capabilityJson"
           )
           VALUES (
             gen_random_uuid(),
             $1, $2, $3, $4, $5,
             'embedding',
             '{}',
             0.12,
             0.0,
             32000,
             '{}',
             true,
             NOW(),
             NOW(),
             $6::jsonb
           )
           ON CONFLICT DO NOTHING`,
          [model.code, model.name, model.description, voyageProviderId, model.providerModelId, voyageCapabilityJson],
        );
      }
      console.log('[seed] Ensured Voyage AI provider and 5 embedding models.');
    } else {
      console.log('[seed] voyage provider not present in baseline — skipping voyage-3-* models (expected; add the provider via CP UI to enable).');
    }

    // D-2: Bedrock embedding models — Titan and Cohere Embed.
    //
    // These are seeded under the existing 'bedrock' provider row.
    // Provider adapter_type = 'bedrock'; Auth: AWS SigV4.
    // The 'bedrock' provider row should already exist from an earlier seed.
    //
    // Models as of 2026-05-20:
    //   amazon.titan-embed-text-v2:0 (1024-dim, Titan V2)
    //   amazon.titan-embed-text-v1   (1536-dim, Titan V1)
    //   cohere.embed-english-v3      (1024-dim, Cohere English)
    //   cohere.embed-multilingual-v3 (1024-dim, Cohere Multilingual)
    //
    // ON CONFLICT DO NOTHING: safe to re-run.
    const { rows: bedrockRows } = await client.query<{ id: string }>(
      `SELECT id FROM "Provider" WHERE name = 'bedrock' LIMIT 1`,
    );
    if (bedrockRows.length > 0) {
      const bedrockProviderId = bedrockRows[0].id;
      for (const model of [
        {
          code: 'amazon-titan-embed-text-v2',
          name: 'Amazon Titan Embed Text V2',
          providerModelId: 'amazon.titan-embed-text-v2:0',
          description:
            'Amazon Titan Embeddings V2 (1024-dim default, supports 256/512/1024). ' +
            'Bedrock InvokeModel endpoint. Auth: AWS SigV4 with bedrock-runtime service.',
          capabilityJson: JSON.stringify({
            embeddings: {
              max_input_tokens: 8192,
              default_dimension: 1024,
              supported_dimensions: [256, 512, 1024],
            },
          }),
        },
        {
          code: 'amazon-titan-embed-text-v1',
          name: 'Amazon Titan Embed Text V1',
          providerModelId: 'amazon.titan-embed-text-v1',
          description:
            'Amazon Titan Embeddings V1 (1536-dim, fixed). ' +
            'Bedrock InvokeModel endpoint. Auth: AWS SigV4 with bedrock-runtime service.',
          capabilityJson: JSON.stringify({
            embeddings: {
              max_input_tokens: 8192,
              default_dimension: 1536,
              supported_dimensions: [1536],
            },
          }),
        },
        {
          code: 'cohere-embed-english-v3',
          name: 'Cohere Embed English V3',
          providerModelId: 'cohere.embed-english-v3',
          description:
            'Cohere Embed English V3 on Bedrock (1024-dim). Requires input_type parameter. ' +
            'Bedrock InvokeModel endpoint. Auth: AWS SigV4 with bedrock-runtime service.',
          capabilityJson: JSON.stringify({
            embeddings: {
              max_input_tokens: 512,
              default_dimension: 1024,
              supported_dimensions: [1024],
            },
          }),
        },
        {
          code: 'cohere-embed-multilingual-v3',
          name: 'Cohere Embed Multilingual V3',
          providerModelId: 'cohere.embed-multilingual-v3',
          description:
            'Cohere Embed Multilingual V3 on Bedrock (1024-dim). Supports 100+ languages. ' +
            'Bedrock InvokeModel endpoint. Auth: AWS SigV4 with bedrock-runtime service.',
          capabilityJson: JSON.stringify({
            embeddings: {
              max_input_tokens: 512,
              default_dimension: 1024,
              supported_dimensions: [1024],
            },
          }),
        },
      ]) {
        await client.query(
          `INSERT INTO "Model" (
             id, code, name, description, "providerId", "providerModelId",
             type, features, "inputPricePerMillion", "outputPricePerMillion",
             "maxContextTokens", aliases, enabled, "createdAt", "updatedAt",
             "capabilityJson"
           )
           VALUES (
             gen_random_uuid(),
             $1, $2, $3, $4, $5,
             'embedding',
             '{}',
             0.02,
             0.0,
             8192,
             '{}',
             true,
             NOW(),
             NOW(),
             $6::jsonb
           )
           ON CONFLICT DO NOTHING`,
          [model.code, model.name, model.description, bedrockProviderId, model.providerModelId, model.capabilityJson],
        );
      }
      console.log('[seed] Ensured Bedrock embedding models (Titan V2/V1, Cohere English/Multilingual).');
    } else {
      console.log('[seed] bedrock provider not present in baseline — skipping Titan / Cohere Embed models (expected; add the provider via CP UI to enable).');
    }

    // Time-sensitive freshness rules (E61-S6 — response cache freshness gate).
    //
    // The rule list is the single source of truth for what counts as a
    // time-sensitive prompt (stock prices, weather, news, …). Stored as a
    // JSONB blob on the semantic_cache_config singleton; AIGW pulls it via
    // Hub shadow on boot and on every change. There are no Go-side default
    // fallbacks — if this seed step doesn't run, the freshness gate is off
    // (no rules → no skips → semantic cache returns potentially stale hits).
    //
    // Idempotent: writes only when the existing rule list is empty. This
    // preserves admin edits / deletions across re-seed (so a customer who
    // disabled "weather" doesn't get it resurrected on the next deploy).
    {
      const raw = readFileSync(TIME_SENSITIVE_RULES_JSON, 'utf8');
      const parsed = JSON.parse(raw) as { rules: unknown[] };
      const blob = { rules: parsed.rules };
      const result = await client.query(
        `UPDATE semantic_cache_config
            SET time_sensitive_overrides = $1::jsonb
          WHERE id = 'singleton'
            AND (
              time_sensitive_overrides IS NULL
              OR jsonb_array_length(COALESCE(time_sensitive_overrides->'rules', '[]'::jsonb)) = 0
            )`,
        [JSON.stringify(blob)],
      );
      if ((result.rowCount ?? 0) > 0) {
        console.log(`[seed] Wrote ${parsed.rules.length} time-sensitive rules into semantic_cache_config.`);
      } else {
        console.log('[seed] Time-sensitive rules already populated (admin edits preserved); leaving as-is.');
      }
    }
  } finally {
    await client.end();
  }
  console.log('[seed] Done.');
}

main().catch((err) => {
  console.error('[seed] FAILED:', err);
  process.exit(1);
});
