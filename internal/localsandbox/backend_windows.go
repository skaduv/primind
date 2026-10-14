//go:build windows

package localsandbox

import (
	"github.com/primind/primind/internal/localsandbox/core"
	"github.com/primind/primind/internal/localsandbox/winhost"
)

func NewBackend() (core.Backend, error) { return winhost.New() }
