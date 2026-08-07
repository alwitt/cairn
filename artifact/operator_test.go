package artifact_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/alwitt/cairn/artifact"
	mockartifact "github.com/alwitt/cairn/mocks/artifact"
	mocktest "github.com/alwitt/cairn/mocks/test"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	mockruntime "github.com/alwitt/goutils/mocks/runtime"
	"github.com/alwitt/goutils/runtime"
	"github.com/apex/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// ======================================================================================
// Operator test harness

const (
	// unitTestSidecarImage the sidecar image the harness operator is configured with
	unitTestSidecarImage = "unit-test/cairn-sidecar:latest"
	// unitTestSidecarTimeoutSec the sidecar timeout the harness operator is configured with
	unitTestSidecarTimeoutSec = 120
)

// unitTestSidecarConfig build the sidecar config the harness operator is constructed with.
func unitTestSidecarConfig() models.ArtifactSidecarConfig {
	return models.ArtifactSidecarConfig{
		Image:       unitTestSidecarImage,
		TimeoutSecs: unitTestSidecarTimeoutSec,
	}
}

// unitTestOperatorMocks the collaborators the harness operator is built over, so a test can set
// expectations on whichever ones its case exercises.
type unitTestOperatorMocks struct {
	manager   *mockartifact.Manager
	callbacks *mocktest.UnitTestCallbackCollector
}

// newUnitTestOperator build an Operator over a mocked artifact manager and a mocked container
// runtime factory. No expectations are set, so any call a case did not arrange for fails that
// case - including launching a sidecar at all, which is what several cases assert by omission.
func newUnitTestOperator(t *testing.T) (artifact.Operator, unitTestOperatorMocks) {
	return newUnitTestOperatorWithConfig(t, unitTestSidecarConfig())
}

// newUnitTestOperatorWithConfig build a harness Operator over a specific sidecar config.
func newUnitTestOperatorWithConfig(
	t *testing.T, sidecarConfig models.ArtifactSidecarConfig,
) (artifact.Operator, unitTestOperatorMocks) {
	assert := assert.New(t)

	mocks := unitTestOperatorMocks{
		manager:   mockartifact.NewManager(t),
		callbacks: mocktest.NewUnitTestCallbackCollector(t),
	}

	operator, err := artifact.NewDockerOperator(
		unitTestAppName,
		mocks.manager,
		sidecarConfig,
		mocks.callbacks.DefineSystemCallDockerRuntime,
	)
	assert.Nil(err)
	assert.NotNil(operator)

	return operator, mocks
}

// sidecarOutput frame a result payload the way a sidecar emits it: one line of compact JSON
// with a blank line either side. Tests build their expected output through this rather than
// hand-writing the framing, so what they feed the parser is what the sidecar really produces.
func sidecarOutput(payload map[string]any) string {
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return "\n" + string(encoded) + "\n"
}

// expectSidecarRun arrange a sidecar launch that runs to completion with the supplied output
// and exit code, capturing the runtime params it was launched with.
//
// Cleanup is always expected: the operator must tear the container down on every path.
func expectSidecarRun(
	t *testing.T, mocks unitTestOperatorMocks, output string, exitCode int,
) *runtime.DockerRuntimeParams {
	captured := new(runtime.DockerRuntimeParams)

	sidecar := mockruntime.NewSystemCallRuntime(t)
	sidecar.EXPECT().Start(mock.Anything).Return(nil).Once()
	sidecar.EXPECT().
		Wait(mock.Anything).
		Return(runtime.SystemCallResp{ExitCode: exitCode, Output: output}, nil).
		Once()
	sidecar.EXPECT().Cleanup(mock.Anything).Return(nil).Once()

	mocks.callbacks.EXPECT().
		DefineSystemCallDockerRuntime(
			mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
		).
		RunAndReturn(func(
			_ context.Context,
			_ string,
			_ runtime.ContainerCommand,
			params runtime.DockerRuntimeParams,
			_ bool,
		) (runtime.SystemCallRuntime, error) {
			*captured = params
			return sidecar, nil
		}).
		Once()

	return captured
}

// capturedLaunch what a sidecar was launched with, recorded at define time.
//
// The command is captured alongside the parameters because the two-sidecar write path has to
// assert WHICH entrypoint ran in which position, not just what each was handed.
type capturedLaunch struct {
	name    string
	command runtime.ContainerCommand
	params  runtime.DockerRuntimeParams
}

// entrypoint the single entrypoint the captured launch was given.
func (c capturedLaunch) entrypoint() string {
	if len(c.command.Entrypoint) != 1 {
		return ""
	}
	return c.command.Entrypoint[0]
}

// sidecarRun how one arranged sidecar run in a sequence behaves.
type sidecarRun struct {
	// output the combined container output the run produces
	output string
	// exitCode the container's exit code
	exitCode int
	// startErr when set, the run fails at Start and never reaches Wait
	startErr error
	// waitErr when set, the run fails at Wait
	waitErr error
}

// expectSidecarSequence arrange a sequence of sidecar launches, capturing what each was
// launched with.
//
// The expectations are consumed in declaration order, so the returned slice is positional:
// index 0 is the first sidecar the operator defines. Each is `.Once()`, so a run the operator
// makes but this did not arrange fails the case, which is how the "no second sidecar was ever
// launched" assertions are made - by omission rather than by a negative check.
//
// Cleanup is arranged for every run including the ones that fail to start: the operator
// registers teardown as soon as the runtime is defined, and a leftover container is exactly
// what that ordering exists to prevent.
func expectSidecarSequence(
	t *testing.T, mocks unitTestOperatorMocks, runs ...sidecarRun,
) []*capturedLaunch {
	captured := make([]*capturedLaunch, 0, len(runs))

	for _, run := range runs {
		record := new(capturedLaunch)
		captured = append(captured, record)

		sidecar := mockruntime.NewSystemCallRuntime(t)
		sidecar.EXPECT().Start(mock.Anything).Return(run.startErr).Once()
		if run.startErr == nil {
			sidecar.EXPECT().
				Wait(mock.Anything).
				Return(
					runtime.SystemCallResp{ExitCode: run.exitCode, Output: run.output},
					run.waitErr,
				).
				Once()
		}
		sidecar.EXPECT().Cleanup(mock.Anything).Return(nil).Once()

		mocks.callbacks.EXPECT().
			DefineSystemCallDockerRuntime(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			RunAndReturn(func(
				_ context.Context,
				name string,
				command runtime.ContainerCommand,
				params runtime.DockerRuntimeParams,
				_ bool,
			) (runtime.SystemCallRuntime, error) {
				record.name = name
				record.command = command
				record.params = params
				return sidecar, nil
			}).
			Once()
	}

	return captured
}

// assertOperatorError verify an error is an ArtifactOperatorError that still carries the
// original failure at the top of the chain.
//
// The innermost error is matched on its rendered message rather than with `errors.Is`: the
// goutils constructors take the core error by value, so the chain carries a copy of it and
// identity comparison would never match.
func assertOperatorError(assert *assert.Assertions, err error, wrapped error) {
	assert.NotNil(err)

	var operatorErr models.ArtifactOperatorError
	assert.True(
		errors.As(err, &operatorErr), "expected ArtifactOperatorError, got %T: %v", err, err,
	)

	assert.Contains(
		err.Error(), wrapped.Error(), "wrapped error should survive to the top of the chain",
	)
}

// assertOperatorBadInputError verify an error is an ArtifactOperatorError wrapping a
// BadInputError. The operator reports caller-supplied violations this way so the API layer can
// map them onto a 4xx.
func assertOperatorBadInputError(assert *assert.Assertions, err error) {
	assert.NotNil(err)

	var operatorErr models.ArtifactOperatorError
	assert.True(
		errors.As(err, &operatorErr), "expected ArtifactOperatorError, got %T: %v", err, err,
	)

	var badInputErr goutils.BadInputError
	assert.True(errors.As(err, &badInputErr), "expected BadInputError, got %T: %v", err, err)
}

// envValue read one environment variable out of a set of container env vars.
func envValue(env []runtime.ContainerEnvVar, name string) (string, bool) {
	for _, entry := range env {
		if entry.Name == name {
			return entry.Value, true
		}
	}
	return "", false
}

// ======================================================================================
// Construction

func TestNewOperator(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("rejects a missing artifact manager", func(t *testing.T) {
		assert := assert.New(t)

		callbacks := mocktest.NewUnitTestCallbackCollector(t)
		_, err := artifact.NewDockerOperator(
			unitTestAppName, nil, unitTestSidecarConfig(),
			callbacks.DefineSystemCallDockerRuntime,
		)

		assert.NotNil(err)
	})

	// The runtime factory is required rather than defaulted, so the choice of runtime driver
	// stays explicit at the wiring site.
	t.Run("rejects a missing runtime factory", func(t *testing.T) {
		assert := assert.New(t)

		manager := mockartifact.NewManager(t)
		_, err := artifact.NewDockerOperator(
			unitTestAppName, manager, unitTestSidecarConfig(), nil,
		)

		assert.NotNil(err)
	})

	// A sidecar image that was never configured must fail here, not at the first artifact
	// operation - by which point a caller is already waiting on a transfer.
	t.Run("rejects a sidecar config with no image", func(t *testing.T) {
		assert := assert.New(t)

		manager := mockartifact.NewManager(t)
		callbacks := mocktest.NewUnitTestCallbackCollector(t)

		config := unitTestSidecarConfig()
		config.Image = ""

		_, err := artifact.NewDockerOperator(
			unitTestAppName, manager, config, callbacks.DefineSystemCallDockerRuntime,
		)

		assert.NotNil(err)
	})

	t.Run("rejects a sidecar config with no timeout", func(t *testing.T) {
		assert := assert.New(t)

		manager := mockartifact.NewManager(t)
		callbacks := mocktest.NewUnitTestCallbackCollector(t)

		config := unitTestSidecarConfig()
		config.TimeoutSecs = 0

		_, err := artifact.NewDockerOperator(
			unitTestAppName, manager, config, callbacks.DefineSystemCallDockerRuntime,
		)

		assert.NotNil(err)
	})
}

// ======================================================================================
// Workspace path validation
//
// Driven through DownloadArtifact because that is where the check actually guards something.
// Every case here must be rejected before a container is ever defined - which needs no
// explicit assertion: `DefineSystemCallDockerRuntime` is left unarranged on the collector, so
// any launch at all fails the test.

func TestOperatorWorkspacePathValidation(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	rejected := map[string]string{
		"a relative path":              "some/relative/path.txt",
		"a path outside the mount":     "/etc/passwd",
		"traversal escaping the mount": models.WorkspaceMountPath + "/../../etc/passwd",
		// A sibling directory whose name merely starts with the mount's. Containment is a
		// path-component test, so `<mount>X` must not read as a match.
		"a prefix-alike sibling": models.WorkspaceMountPath + "X/out.txt",
		// The mount root is a directory, never a file to write.
		"the mount root itself":                    models.WorkspaceMountPath,
		"the mount root with a trailing separator": models.WorkspaceMountPath + "/",
	}

	for name, path := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			assert := assert.New(t)

			operator, mocks := newUnitTestOperator(t)
			workspace := sampleWorkspace("unit-test-workspace")
			entry := sampleArtifact(workspace, "unit-test-artifact")

			err := operator.DownloadArtifact(utCtx, workspace, entry, path)

			assertOperatorBadInputError(assert, err)
			// Nothing was minted either - a rejected path must not cost a presigned URL.
			mocks.manager.AssertNotCalled(t, "GenerateGetURLForArtifact")
		})
	}

	// The counterpart: a legitimate nested path must survive validation and reach a sidecar.
	t.Run("accepts a nested path under the mount", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")
		target := models.WorkspaceMountPath + "/nested/dir/out.txt"

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("https://store.unit-test/get", nil).
			Once()
		expectSidecarRun(t, mocks, sidecarOutput(map[string]any{"ok": true}), 0)

		assert.Nil(operator.DownloadArtifact(utCtx, workspace, entry, target))
	})

	// `..` that stays inside the mount is legitimate - it is only an escape when it lands
	// outside. Rejecting it would be over-strict.
	t.Run("accepts traversal that stays inside the mount", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")
		target := models.WorkspaceMountPath + "/sub/../out.txt"

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("https://store.unit-test/get", nil).
			Once()
		captured := expectSidecarRun(t, mocks, sidecarOutput(map[string]any{"ok": true}), 0)

		assert.Nil(operator.DownloadArtifact(utCtx, workspace, entry, target))

		// The path reaches the sidecar as the caller spelled it. The sidecar resolves it
		// itself, and re-spelling it here would only hide what was actually asked for.
		reached, ok := envValue(captured.Environment, "CAIRN_TARGET_PATH")
		assert.True(ok)
		assert.Equal(target, reached)
	})
}

// ======================================================================================
// Sidecar result line parsing
//
// The container runtime merges STDOUT and STDERR, so the result has to be found in a stream
// that may carry arbitrary other lines. Driven through DownloadArtifact, which is where a
// misparse would do damage.

func TestOperatorSidecarResultParsing(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	// runWithOutput drive a download whose sidecar produced the supplied combined output.
	runWithOutput := func(t *testing.T, output string, exitCode int) error {
		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("https://store.unit-test/get", nil).
			Once()
		expectSidecarRun(t, mocks, output, exitCode)

		return operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		)
	}

	t.Run("finds the result line among unrelated noise", func(t *testing.T) {
		assert := assert.New(t)

		output := "a warning from some library\n" +
			sidecarOutput(map[string]any{"ok": true, "downloaded_path": "/mnt/cairn/ws/out.txt"}) +
			"a trailing progress line\n"

		assert.Nil(runWithOutput(t, output, 0))
	})

	// The sidecar emits its result last, so a library that happens to log JSON earlier must
	// not pre-empt the real record.
	t.Run("the last JSON object line wins", func(t *testing.T) {
		assert := assert.New(t)

		output := sidecarOutput(map[string]any{"ok": true, "note": "an earlier json log line"}) +
			sidecarOutput(map[string]any{"ok": false, "error": "the real outcome"})

		err := runWithOutput(t, output, 1)

		assert.NotNil(err)
		assert.Contains(err.Error(), "the real outcome")
	})

	// Valid JSON that is not an object is noise, not a result record.
	t.Run("skips JSON lines that are not objects", func(t *testing.T) {
		assert := assert.New(t)

		output := "3\n\"done\"\n[1,2,3]\n" + sidecarOutput(map[string]any{"ok": true})

		assert.Nil(runWithOutput(t, output, 0))
	})

	// No result line means a broken image or a crash before the sidecar could emit one. A
	// zero exit code does not make the work have happened.
	t.Run("no parseable line fails even on a zero exit code", func(t *testing.T) {
		assert := assert.New(t)

		err := runWithOutput(t, "Traceback (most recent call last):\n  ImportError\n", 0)

		assert.NotNil(err)
		assert.Contains(err.Error(), "no parseable result line")
		// The raw output is the only evidence of what went wrong, so it must survive.
		assert.Contains(err.Error(), "ImportError")
	})

	t.Run("empty output fails", func(t *testing.T) {
		assert := assert.New(t)

		err := runWithOutput(t, "", 0)

		assert.NotNil(err)
		assert.Contains(err.Error(), "no parseable result line")
	})

	// A chatty container must not flood the caller's error with its entire log.
	t.Run("truncates very long output in the error", func(t *testing.T) {
		assert := assert.New(t)

		noise := ""
		for range 500 {
			noise += "a line of sidecar noise that is not json\n"
		}

		err := runWithOutput(t, noise, 1)

		assert.NotNil(err)
		assert.Contains(err.Error(), "truncated")
		assert.Less(len(err.Error()), 4096, "the error must stay bounded")
	})

	t.Run("reports a result line that is not the expected shape", func(t *testing.T) {
		assert := assert.New(t)

		// A JSON object, so it is found - but `ok` is not a bool, so the decode fails.
		err := runWithOutput(t, sidecarOutput(map[string]any{"ok": "yes"}), 0)

		assert.NotNil(err)
		assert.Contains(err.Error(), "failed to parse")
	})
}

// ======================================================================================
// Sidecar container parameters
//
// Assembled once for every sidecar, so a regression here breaks every artifact operation at
// once. Driven through DownloadArtifact.

func TestOperatorSidecarParameters(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	t.Run("mounts the workspace volume at the canonical path", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("https://store.unit-test/get", nil).
			Once()
		captured := expectSidecarRun(t, mocks, sidecarOutput(map[string]any{"ok": true}), 0)

		assert.Nil(operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		))

		assert.Equal(unitTestSidecarImage, captured.Image)
		assert.Equal(unitTestSidecarTimeoutSec, captured.TimeoutSecs)
		// A batch run: the result is collected from the container's output rather than
		// streamed, so `Wait` returns it.
		assert.Nil(captured.Streaming)

		assert.Len(captured.VolumeMounts, 1)
		assert.Equal(workspace.VolumeName, captured.VolumeMounts[0].Name)
		// The path every container agrees on. A file written here by a tool container must
		// be visible to a sidecar at the same path, or nothing round-trips.
		assert.Equal(models.WorkspaceMountPath, captured.VolumeMounts[0].MountPath)
	})

	// The security posture, asserted once because `buildSidecarParams` assembles it for every
	// sidecar - so a regression here silently breaks all three at once (see DESIGN §5.1).
	t.Run("runs as root with only DAC_OVERRIDE added", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("https://store.unit-test/get", nil).
			Once()
		captured := expectSidecarRun(t, mocks, sidecarOutput(map[string]any{"ok": true}), 0)

		assert.Nil(operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		))

		// The volume's mount root is root-owned (see DESIGN §4.2), so the runtime's `nobody`
		// default could not write to the volume at all.
		assert.Equal(models.SidecarRunAsUser, captured.RunAsUser)
		assert.Equal(models.SidecarRunAsGroup, captured.RunAsGroup)

		// Exactly one capability. Root alone is not enough - with everything dropped, uid 0 may
		// only touch what it owns, so it could neither write into an agent-created 0755
		// directory nor read an agent-created 0600 file.
		assert.Equal([]string{"DAC_OVERRIDE"}, captured.AddCapabilities)

		// CHOWN and FOWNER are deliberately withheld: after provisioning, cairn never re-owns
		// or re-modes anything in the volume (see DESIGN §7.5.1).
		assert.NotContains(captured.AddCapabilities, "CHOWN")
		assert.NotContains(captured.AddCapabilities, "FOWNER")

		// The rest of the posture is untouched. Left nil so the runtime's own secure defaults
		// apply; a non-nil value here would mean someone had opted out of one of them.
		assert.Nil(captured.ReadOnlyRootFS, "read-only rootfs must stay at the runtime default")
		assert.Nil(captured.DropAllCapabilities, "drop-all must stay at the runtime default")
		assert.Nil(captured.NoNewPrivileges, "no-new-privileges must stay at the runtime default")
	})

	// The mount path is supplied to the sidecar rather than compiled into its image.
	t.Run("passes the mount root to the sidecar", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("https://store.unit-test/get", nil).
			Once()
		captured := expectSidecarRun(t, mocks, sidecarOutput(map[string]any{"ok": true}), 0)

		assert.Nil(operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		))

		mountRoot, ok := envValue(captured.Environment, "CAIRN_MOUNT_ROOT")
		assert.True(ok, "the sidecar must be told where the volume is mounted")
		assert.Equal(models.WorkspaceMountPath, mountRoot)
	})

	// Every sidecar takes a fixed command and no arguments. With no argv there is nothing for
	// an agent-influenced value to reach, and a presigned URL never lands in
	// /proc/<pid>/cmdline.
	t.Run("runs a fixed command with no arguments", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")

		var command runtime.ContainerCommand
		sidecar := mockruntime.NewSystemCallRuntime(t)
		sidecar.EXPECT().Start(mock.Anything).Return(nil).Once()
		sidecar.EXPECT().
			Wait(mock.Anything).
			Return(runtime.SystemCallResp{
				Output: sidecarOutput(map[string]any{"ok": true}),
			}, nil).
			Once()
		sidecar.EXPECT().Cleanup(mock.Anything).Return(nil).Once()

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("https://store.unit-test/get", nil).
			Once()
		mocks.callbacks.EXPECT().
			DefineSystemCallDockerRuntime(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			RunAndReturn(func(
				_ context.Context,
				_ string,
				cmd runtime.ContainerCommand,
				_ runtime.DockerRuntimeParams,
				_ bool,
			) (runtime.SystemCallRuntime, error) {
				command = cmd
				return sidecar, nil
			}).
			Once()

		assert.Nil(operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		))

		assert.Equal([]string{"cairn-download"}, command.Entrypoint)
		assert.Empty(command.Commands, "a sidecar takes no arguments")
	})

	// Deployment-wide extras reach every sidecar. The host mapping in particular changes
	// shape on the way - the config carries one host per entry, the runtime groups several
	// hostnames against one address - which is easy to get wrong silently.
	t.Run("threads configured extra envs and hosts through", func(t *testing.T) {
		assert := assert.New(t)

		config := unitTestSidecarConfig()
		config.ExtraEnvs = []models.ArtifactSidecarExtraEnvVar{
			{Name: "HTTPS_PROXY", Value: "http://proxy.unit-test:3128"},
		}
		config.ExtaHosts = []models.ArtifactSidecarExtraHost{
			{Host: "store.unit-test", Address: "10.1.2.3"},
			{Host: "other.unit-test", Address: "10.1.2.4"},
		}

		operator, mocks := newUnitTestOperatorWithConfig(t, config)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("https://store.unit-test/get", nil).
			Once()
		captured := expectSidecarRun(t, mocks, sidecarOutput(map[string]any{"ok": true}), 0)

		assert.Nil(operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		))

		proxy, ok := envValue(captured.Environment, "HTTPS_PROXY")
		assert.True(ok)
		assert.Equal("http://proxy.unit-test:3128", proxy)

		assert.Equal([]runtime.ContainerExtraHost{
			{Hosts: []string{"store.unit-test"}, Address: "10.1.2.3"},
			{Hosts: []string{"other.unit-test"}, Address: "10.1.2.4"},
		}, captured.ExtraHosts)
	})
}

// ======================================================================================
// Sidecar network mode
//
// Its own case because the failure is silent and total: the container runtime defaults an
// unset network mode to `none`, which suits the stat sidecar but leaves a transfer sidecar
// unable to reach the object store at all.

func TestOperatorSidecarNetworkMode(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	runAndCaptureParams := func(
		t *testing.T, config models.ArtifactSidecarConfig,
	) *runtime.DockerRuntimeParams {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperatorWithConfig(t, config)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("https://store.unit-test/get", nil).
			Once()
		captured := expectSidecarRun(t, mocks, sidecarOutput(map[string]any{"ok": true}), 0)

		assert.Nil(operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		))

		return captured
	}

	t.Run("a transfer sidecar gets a routable network", func(t *testing.T) {
		assert := assert.New(t)

		captured := runAndCaptureParams(t, unitTestSidecarConfig())

		// Empty would inherit the runtime's `none` default, and the download could not
		// reach the object store at all.
		assert.NotEmpty(
			captured.NetworkMode, "an unset network mode means no networking at all",
		)
		assert.NotEqual("none", captured.NetworkMode)
		assert.Equal(models.DefaultSidecarNetworkMode, captured.NetworkMode)
	})

	t.Run("a configured network mode overrides the default", func(t *testing.T) {
		assert := assert.New(t)

		config := unitTestSidecarConfig()
		config.NetworkMode = "cairn-unit-test-net"

		captured := runAndCaptureParams(t, config)

		assert.Equal("cairn-unit-test-net", captured.NetworkMode)
	})
}

// ======================================================================================
// Sidecar lifecycle failures

func TestOperatorSidecarLifecycle(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	t.Run("surfaces a failure to define the runtime", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")
		defineErr := fmt.Errorf("no such image")

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("https://store.unit-test/get", nil).
			Once()
		mocks.callbacks.EXPECT().
			DefineSystemCallDockerRuntime(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(nil, defineErr).
			Once()

		err := operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		)

		assertOperatorError(assert, err, defineErr)
	})

	// The container must be torn down even when the run failed, or a failed download leaks a
	// container onto the host.
	t.Run("cleans up after a failed start", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")
		startErr := fmt.Errorf("failed to start container")

		sidecar := mockruntime.NewSystemCallRuntime(t)
		sidecar.EXPECT().Start(mock.Anything).Return(startErr).Once()
		sidecar.EXPECT().Cleanup(mock.Anything).Return(nil).Once()

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("https://store.unit-test/get", nil).
			Once()
		mocks.callbacks.EXPECT().
			DefineSystemCallDockerRuntime(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(sidecar, nil).
			Once()

		err := operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		)

		assertOperatorError(assert, err, startErr)
	})

	t.Run("cleans up after a failed wait", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")
		waitErr := fmt.Errorf("container timed out")

		sidecar := mockruntime.NewSystemCallRuntime(t)
		sidecar.EXPECT().Start(mock.Anything).Return(nil).Once()
		sidecar.EXPECT().
			Wait(mock.Anything).
			Return(runtime.SystemCallResp{ExitCode: 124}, waitErr).
			Once()
		sidecar.EXPECT().Cleanup(mock.Anything).Return(nil).Once()

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("https://store.unit-test/get", nil).
			Once()
		mocks.callbacks.EXPECT().
			DefineSystemCallDockerRuntime(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(sidecar, nil).
			Once()

		err := operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		)

		assertOperatorError(assert, err, waitErr)
	})

	// A cleanup failure is logged, never returned: the run's own outcome is what the caller
	// needs, and a leftover container must not mask a successful transfer.
	t.Run("a cleanup failure does not fail a successful run", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")

		sidecar := mockruntime.NewSystemCallRuntime(t)
		sidecar.EXPECT().Start(mock.Anything).Return(nil).Once()
		sidecar.EXPECT().
			Wait(mock.Anything).
			Return(runtime.SystemCallResp{
				Output: sidecarOutput(map[string]any{"ok": true}),
			}, nil).
			Once()
		sidecar.EXPECT().
			Cleanup(mock.Anything).
			Return(fmt.Errorf("failed to remove container")).
			Once()

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("https://store.unit-test/get", nil).
			Once()
		mocks.callbacks.EXPECT().
			DefineSystemCallDockerRuntime(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(sidecar, nil).
			Once()

		assert.Nil(operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		))
	})
}
