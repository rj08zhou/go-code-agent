package repl

import (
	"context"

	"go-code-agent/internal/application"
)

type Loop struct {
	built  *application.BuiltRunner
	rtCtx  context.Context
	readFn func() (string, error)
	next   *application.BuildOptions
}

func New(built *application.BuiltRunner, rtCtx context.Context, readFn func() (string, error)) *Loop {
	return &Loop{built: built, rtCtx: rtCtx, readFn: readFn}
}

func (r *Loop) NextBuild() *application.BuildOptions { return r.next }
