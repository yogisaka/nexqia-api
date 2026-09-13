// internal/cache/redis.go
package cache

import (
	"github.com/redis/go-redis/v9"

	"github.com/yogisaka/nexqia-api/internal/config"
)

func NewRedisClient(cfg config.Config) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:     cfg.RedisHost + ":" + cfg.RedisPort,
		Password: cfg.RedisPassword,
	})
}
