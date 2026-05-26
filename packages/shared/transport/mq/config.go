package mq

// Config holds MQ configuration. The Driver field selects which
// implementation to use.
type Config struct {
	// Driver selects the MQ implementation: "nats", "redis", "kafka".
	Driver string `yaml:"driver"`

	// Namespace is the Prometheus metrics namespace for this service
	// (e.g. "nexus_hub", "nexus_ai_gateway"). Defaults to "nexus" if empty.
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
