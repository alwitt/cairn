// Package test - various support components used in unit-testing.
package test

import (
	"context"

	"github.com/alwitt/goutils/runtime"
)

// UnitTestCallbackCollector unit-testing interface for collecting callbacks
type UnitTestCallbackCollector interface {
	// EstimateMIMEType called when artifact manager to need to estimate data MIME type
	EstimateMIMEType(data []byte) string

	// DefineSystemCallDockerRuntime called when the artifact operator needs to define a
	// docker container runtime to run a sidecar with
	DefineSystemCallDockerRuntime(
		ctx context.Context,
		name string,
		command runtime.ContainerCommand,
		params runtime.DockerRuntimeParams,
		clearANSIFromOutput bool,
	) (runtime.SystemCallRuntime, error)
}
