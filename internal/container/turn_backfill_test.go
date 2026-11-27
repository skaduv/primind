package container

import (
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/dig"

	"github.com/primind/primind/internal/application/service"
	"github.com/primind/primind/internal/types/interfaces"
)

// The backfill command resolves the reconciler from its own small graph, and
// that graph has none of the server's running parts.
func TestTurnBackfillContainerWiresOnlyTheReconciler(t *testing.T) {
	c := BuildTurnBackfillContainer(dig.New(dig.DryRun(true)))

	if err := c.Invoke(func(*service.TurnReconciler) {}); err != nil {
		t.Fatalf("reconciler does not resolve: %v", err)
	}
	if err := c.Invoke(func(*gin.Engine) {}); err == nil {
		t.Fatal("the backfill graph must not build the HTTP router")
	}
	if err := c.Invoke(func(interfaces.ResourceCleaner) {}); err == nil {
		t.Fatal("the backfill graph registers no service cleanups")
	}
}
