-- DropForeignKey
ALTER TABLE "cache_provider_config" DROP CONSTRAINT "cache_provider_config_provider_id_fkey";

-- CreateTable
CREATE TABLE "thing_metric_rollup_5m" (
    "id" TEXT NOT NULL,
    "bucketStart" TIMESTAMPTZ(3) NOT NULL,
    "thing_id" TEXT NOT NULL,
    "metricName" TEXT NOT NULL,
    "dimensionKey" TEXT NOT NULL DEFAULT '',
    "subDimension" TEXT NOT NULL DEFAULT '',
    "value" DECIMAL(24,6) NOT NULL DEFAULT 0,
    "metadata" JSONB,
    "updatedAt" TIMESTAMPTZ(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,

    CONSTRAINT "thing_metric_rollup_5m_pkey" PRIMARY KEY ("id")
);

-- CreateTable
CREATE TABLE "thing_metric_rollup_1h" (
    "id" TEXT NOT NULL,
    "bucketStart" TIMESTAMPTZ(3) NOT NULL,
    "thing_id" TEXT NOT NULL,
    "metricName" TEXT NOT NULL,
    "dimensionKey" TEXT NOT NULL DEFAULT '',
    "subDimension" TEXT NOT NULL DEFAULT '',
    "value" DECIMAL(24,6) NOT NULL DEFAULT 0,
    "metadata" JSONB,
    "updatedAt" TIMESTAMPTZ(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,

    CONSTRAINT "thing_metric_rollup_1h_pkey" PRIMARY KEY ("id")
);

-- CreateTable
CREATE TABLE "thing_metric_rollup_1d" (
    "id" TEXT NOT NULL,
    "bucketStart" TIMESTAMPTZ(3) NOT NULL,
    "thing_id" TEXT NOT NULL,
    "metricName" TEXT NOT NULL,
    "dimensionKey" TEXT NOT NULL DEFAULT '',
    "subDimension" TEXT NOT NULL DEFAULT '',
    "value" DECIMAL(24,6) NOT NULL DEFAULT 0,
    "metadata" JSONB,
    "updatedAt" TIMESTAMPTZ(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,

    CONSTRAINT "thing_metric_rollup_1d_pkey" PRIMARY KEY ("id")
);

-- CreateTable
CREATE TABLE "thing_metric_rollup_1mo" (
    "id" TEXT NOT NULL,
    "bucketStart" TIMESTAMPTZ(3) NOT NULL,
    "thing_id" TEXT NOT NULL,
    "metricName" TEXT NOT NULL,
    "dimensionKey" TEXT NOT NULL DEFAULT '',
    "subDimension" TEXT NOT NULL DEFAULT '',
    "value" DECIMAL(24,6) NOT NULL DEFAULT 0,
    "metadata" JSONB,
    "updatedAt" TIMESTAMPTZ(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,

    CONSTRAINT "thing_metric_rollup_1mo_pkey" PRIMARY KEY ("id")
);

-- CreateIndex
CREATE INDEX "thing_metric_rollup_5m_thing_id_metricName_bucketStart_idx" ON "thing_metric_rollup_5m"("thing_id", "metricName", "bucketStart");

-- CreateIndex
CREATE INDEX "thing_metric_rollup_5m_thing_id_bucketStart_idx" ON "thing_metric_rollup_5m"("thing_id", "bucketStart");

-- CreateIndex
CREATE UNIQUE INDEX "thing_metric_rollup_5m_bucketStart_thing_id_metricName_dime_key" ON "thing_metric_rollup_5m"("bucketStart", "thing_id", "metricName", "dimensionKey", "subDimension");

-- CreateIndex
CREATE INDEX "thing_metric_rollup_1h_thing_id_metricName_bucketStart_idx" ON "thing_metric_rollup_1h"("thing_id", "metricName", "bucketStart");

-- CreateIndex
CREATE INDEX "thing_metric_rollup_1h_thing_id_bucketStart_idx" ON "thing_metric_rollup_1h"("thing_id", "bucketStart");

-- CreateIndex
CREATE UNIQUE INDEX "thing_metric_rollup_1h_bucketStart_thing_id_metricName_dime_key" ON "thing_metric_rollup_1h"("bucketStart", "thing_id", "metricName", "dimensionKey", "subDimension");

-- CreateIndex
CREATE INDEX "thing_metric_rollup_1d_thing_id_metricName_bucketStart_idx" ON "thing_metric_rollup_1d"("thing_id", "metricName", "bucketStart");

-- CreateIndex
CREATE INDEX "thing_metric_rollup_1d_thing_id_bucketStart_idx" ON "thing_metric_rollup_1d"("thing_id", "bucketStart");

-- CreateIndex
CREATE UNIQUE INDEX "thing_metric_rollup_1d_bucketStart_thing_id_metricName_dime_key" ON "thing_metric_rollup_1d"("bucketStart", "thing_id", "metricName", "dimensionKey", "subDimension");

-- CreateIndex
CREATE INDEX "thing_metric_rollup_1mo_thing_id_metricName_bucketStart_idx" ON "thing_metric_rollup_1mo"("thing_id", "metricName", "bucketStart");

-- CreateIndex
CREATE INDEX "thing_metric_rollup_1mo_thing_id_bucketStart_idx" ON "thing_metric_rollup_1mo"("thing_id", "bucketStart");

-- CreateIndex
CREATE UNIQUE INDEX "thing_metric_rollup_1mo_bucketStart_thing_id_metricName_dim_key" ON "thing_metric_rollup_1mo"("bucketStart", "thing_id", "metricName", "dimensionKey", "subDimension");

-- AddForeignKey
ALTER TABLE "cache_provider_config" ADD CONSTRAINT "cache_provider_config_provider_id_fkey" FOREIGN KEY ("provider_id") REFERENCES "Provider"("id") ON DELETE CASCADE ON UPDATE CASCADE;
