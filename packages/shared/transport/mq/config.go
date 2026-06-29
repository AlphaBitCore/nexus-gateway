package mq

// Config holds MQ configuration. The Driver field selects which
// implementation to use.
type Config struct {
	// Driver selects the MQ implementation: "nats", "redis", "kafka".
	Driver string `yaml:"driver"`

	// Namespace is the Prometheus metrics namespace for this service
	// (e.g. "nexus"). Defaults to "nexus" if empty.
	Namespace string `yaml:"namespace"`

	// NATS holds NATS-specific configuration.
	NATS NATSConfig `yaml:"nats"`

	// Redis holds Redis-specific configuration.
	Redis RedisConfig `yaml:"redis"`

	// Kafka holds Kafka-specific configuration (reserved for future use).
	Kafka KafkaConfig `yaml:"kafka"`
}

// NATSConfig holds NATS JetStream connection settings.
type NATSConfig struct {
	// URL is the NATS server URL (e.g. "nats://localhost:4222").
	URL string `yaml:"url"`

	// PublishPoolSize is the number of NATS connections the producer opens and
	// publishes across. A single connection + the per-batch PublishAsyncComplete
	// drain-to-zero barrier serialises async publishes, which caps audit publish
	// throughput far below what NATS can sustain (measured: ~1.3k rec/s on one
	// connection vs ~7.9k that the marshal stage alone produces). The producer
	// fans EnqueueBatchAsync across the pool — each connection has its own
	// JetStream context and ack barrier, so they pipeline independently and
	// throughput scales ~linearly with the pool size. 0 falls back to a sensible
	// default (see defaultPublishPoolSize). Env: MQ_NATS_PUBLISH_POOL_SIZE.
	PublishPoolSize int `yaml:"publishPoolSize"`
}

// RedisConfig holds Redis connection settings for the Redis MQ driver.
type RedisConfig struct {
	// Addr is the Redis server address (e.g. "localhost:6379").
	Addr string `yaml:"addr"`

	// Password is the Redis AUTH password (optional).
	Password string `yaml:"password"`

	// DB is the Redis database number (default 0).
	DB int `yaml:"db"`
}

// KafkaConfig holds Apache Kafka connection settings (placeholder for future).
type KafkaConfig struct {
	// Brokers is the list of Kafka bootstrap broker addresses.
	Brokers []string `yaml:"brokers"`
}
