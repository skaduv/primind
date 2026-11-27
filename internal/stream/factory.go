package stream

import (
	"os"
	"strconv"
	"time"

	"github.com/primind/primind/internal/types/interfaces"
)

// 流管理器类型
const (
	TypeMemory = "memory"
	TypeRedis  = "redis"
)

// NewStreamManager 创建流管理器
func NewStreamManager() (interfaces.StreamManager, error) {
	switch os.Getenv("STREAM_MANAGER_TYPE") {
	case TypeRedis:
		db, err := strconv.Atoi(os.Getenv("REDIS_DB"))
		if err != nil {
			db = 0
		}
		// Default 1h. Live-run keys are refreshed only by the generating
		// run (AppendEvent, steer writes and the handler's TouchLiveRun
		// heartbeat), so a run that lasts longer than this TTL does not look
		// idle to /steer, while a killed run's marker still expires. Reads
		// never refresh it (BF-03).
		ttl := time.Hour
		return NewRedisStreamManager(
			os.Getenv("REDIS_ADDR"),
			os.Getenv("REDIS_USERNAME"),
			os.Getenv("REDIS_PASSWORD"),
			db,
			os.Getenv("REDIS_PREFIX"),
			ttl,
		)
	default:
		return NewMemoryStreamManager(), nil
	}
}
