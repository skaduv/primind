package container

import (
	"context"
	"io"

	"github.com/redis/go-redis/v9"
	"go.uber.org/dig"
	"gorm.io/gorm"

	"github.com/primind/primind/internal/application/repository"
	"github.com/primind/primind/internal/application/service"
	"github.com/primind/primind/internal/config"
	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/stream"
	"github.com/primind/primind/internal/turn"
	"github.com/primind/primind/internal/types/interfaces"
)

// BuildTurnBackfillContainer wires only what the one-off turn backfill
// (BF-03 §4.6) needs: configuration, the database, Redis, the stream manager,
// the message repository and the turn reconciler. It starts nothing: no HTTP
// server, task workers, IM adapters, schedulers or reconciler loop, so the
// command cannot take jobs or messages from the serving replicas. Release
// what it opened with CloseTurnBackfillContainer.
//
// The reconciler gets an empty turn registry: this process generates nothing,
// so only the live-run markers (agent turns of serving replicas) and the
// idle age protect turns still in flight.
func BuildTurnBackfillContainer(c *dig.Container) *dig.Container {
	must(c.Provide(config.LoadConfig))
	must(c.Provide(initDatabase))
	must(c.Provide(initRedisClient))
	must(c.Provide(stream.NewStreamManager))
	must(c.Provide(repository.NewMessageRepository))
	must(c.Provide(turn.NewRegistry))
	must(c.Provide(service.NewTurnReconciler))
	return c
}

// CloseTurnBackfillContainer closes the connections BuildTurnBackfillContainer
// opened. Only what was actually constructed is closed.
func CloseTurnBackfillContainer(c *dig.Container) {
	ctx := context.Background()
	err := c.Invoke(func(db *gorm.DB, rdb *redis.Client, streams interfaces.StreamManager) {
		if closer, ok := streams.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				logger.Warnf(ctx, "[BF-03 backfill] closing stream manager: %v", err)
			}
		}
		if rdb != nil {
			if err := rdb.Close(); err != nil {
				logger.Warnf(ctx, "[BF-03 backfill] closing redis: %v", err)
			}
		}
		if sqlDB, err := db.DB(); err == nil {
			if err := sqlDB.Close(); err != nil {
				logger.Warnf(ctx, "[BF-03 backfill] closing database: %v", err)
			}
		}
	})
	if err != nil {
		logger.Warnf(ctx, "[BF-03 backfill] releasing resources: %v", err)
	}
}
