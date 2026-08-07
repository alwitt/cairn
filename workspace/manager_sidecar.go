package workspace

import (
	"context"
	"fmt"
	"strings"

	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/alwitt/goutils/runtime"
	"github.com/apex/log"
)

// ======================================================================================
// Workspace Volume Preparation Sidecar
//
// A freshly created Docker volume's root is `root:root 0755`, and nothing about the mounting
// container changes that - `--user` sets the process identity, never the mount's ownership,
// and there is no mount option that does. Left as created, no tool container could write to
// the volume, and neither could cairn's own transfer sidecars.
//
// cairn cannot fix that by choosing a UID: a workspace is the shared scratch space for a whole
// scope of work (see DESIGN §2.1), and the UIDs its participants run as are neither known nor
// controlled here. So provisioning ends by handing the mount root to a short-lived sidecar that
// opens it to every one of them (see DESIGN §4.2).

const (
	// volumePrepEntrypoint the shell the volume preparation command runs under.
	//
	// Unlike the artifact sidecars, this one runs no cairn command - the work is two coreutils
	// calls, so there is nothing to add to the sidecar image for it.
	volumePrepEntrypoint = "/bin/sh"

	// volumePrepSubject names what the preparation run was for, in its container name
	volumePrepSubject = "volume-prep"

	// volumePrepNetworkMode the network a preparation sidecar gets.
	//
	// Named explicitly rather than taken from `ArtifactSidecarConfig.TransferNetworkMode()`,
	// which defaults to a routable mode for the transfer sidecars' benefit. This one touches
	// nothing but the mount.
	volumePrepNetworkMode = "none"

	// maxReportedOutputBytes how much of a sidecar's raw output is quoted back in an error.
	//
	// Enough to diagnose a broken image from the message alone, bounded so a chatty container
	// cannot flood the caller's error with its entire log.
	maxReportedOutputBytes = 2048
)

/*
volumePrepCommand build the shell command that opens a workspace volume's mount root.

`chown` runs first so the `chmod` is what settles the final mode: changing ownership can clear
a file's special bits, and doing it second would silently undo the mode just set.

Neither call is recursive. Only the mount root is cairn's to set - everything below it was
created by the workspace's own participants and belongs to them, so a recursive pass would
stomp modes they chose (see DESIGN §7.5.1).

The mount path is a compile-time constant of this service's own (see DESIGN §4.4), so nothing
caller-supplied reaches the shell.

	@returns the container command to run
*/
func volumePrepCommand() runtime.ContainerCommand {
	return runtime.ContainerCommand{
		Entrypoint: []string{volumePrepEntrypoint},
		Commands: []string{"-c", fmt.Sprintf(
			"chown %s:%s %s && chmod 0777 %s",
			models.SidecarRunAsUser,
			models.SidecarRunAsGroup,
			models.WorkspaceMountPath,
			models.WorkspaceMountPath,
		)},
	}
}

/*
buildVolumePrepParams assemble the container runtime parameters for a preparation sidecar.

Only `Image` and `TimeoutSecs` are read from the sidecar config; the rest of it describes the
transfer sidecars' needs. In particular the extra envs and hosts are deliberately not applied -
they exist to point a transfer sidecar at an object store, and this container reaches nothing.

The security posture is the runtime's defaults - read-only root filesystem, all capabilities
dropped, no new privileges - with only the run-as identity changed. No capability is added:
the mount root is already root-owned, so a root process may `chmod` and `chown` it on owner
match alone. That is the whole reason this step is cheap, and it is why a volume whose root is
somehow NOT root-owned fails here loudly rather than being silently repaired.

	@param volumeName string - the persistent volume to mount
	@returns the container runtime parameters
*/
func (m *managerImpl) buildVolumePrepParams(volumeName string) runtime.DockerRuntimeParams {
	return runtime.DockerRuntimeParams{
		ContainerRuntimeParams: runtime.ContainerRuntimeParams{
			Image: m.sidecarConfig.Image,
			// Left nil: a batch run, so the outcome is the exit code rather than a stream.
			Streaming: nil,
			VolumeMounts: []runtime.ContainerVolumeMount{
				{Name: volumeName, MountPath: models.WorkspaceMountPath},
			},
			TimeoutSecs: m.sidecarConfig.TimeoutSecs,
		},
		RunAsUser:   models.SidecarRunAsUser,
		RunAsGroup:  models.SidecarRunAsGroup,
		NetworkMode: volumePrepNetworkMode,
	}
}

/*
prepareVolumePermissions run the sidecar that opens a workspace volume's mount root.

The exit code is the entire result. Unlike the artifact sidecars, which report what went wrong
in a framed result line, this one runs plain coreutils - so a non-zero exit is the failure, and
the container's output is quoted only as evidence of what the shell complained about.

The container is always cleaned up, including when the run failed; the runtime's own cleanup is
idempotent, so this is safe regardless of how far the launch got.

	@param ctx context.Context - execution context
	@param workspace models.Workspace - the workspace whose volume to prepare
*/
func (m *managerImpl) prepareVolumePermissions(
	ctx context.Context, workspace models.Workspace,
) error {
	logTags := m.GetLogTagsForContext(ctx)

	name := fmt.Sprintf("%s.%s.%s", m.appName, volumePrepSubject, workspace.ID)

	sidecar, err := m.defineRuntime(
		ctx, name, volumePrepCommand(), m.buildVolumePrepParams(workspace.VolumeName), true,
	)
	if err != nil {
		return goutils.NewDockerError(
			"failed to define volume preparation sidecar runtime", err, true,
		)
	}

	defer func() {
		// Logged rather than returned: the run's own outcome is what the caller needs, and a
		// leftover container must not mask it.
		if err := sidecar.Cleanup(ctx); err != nil {
			log.
				WithError(err).
				WithFields(logTags).
				WithField("volume", workspace.VolumeName).
				Warn("Failed to clean up volume preparation sidecar container")
		}
	}()

	if err := sidecar.Start(ctx); err != nil {
		return goutils.NewDockerError("failed to start volume preparation sidecar", err, true)
	}

	resp, err := sidecar.Wait(ctx)
	if err != nil {
		return goutils.NewDockerError(
			"failed to run volume preparation sidecar to completion", err, true,
		)
	}

	if resp.ExitCode != 0 {
		return goutils.NewDockerError(
			fmt.Sprintf(
				"volume preparation sidecar failed (exit code %d); output: %s",
				resp.ExitCode, truncateOutput(resp.Output),
			),
			nil,
			true,
		)
	}

	log.
		WithFields(logTags).
		WithField("volume", workspace.VolumeName).
		Debug("Prepared workspace persistent volume permissions")

	return nil
}

/*
truncateOutput bound how much of a sidecar's raw output is quoted back in an error.

	@param output string - the sidecar's captured output
	@returns the output, trimmed and bounded
*/
func truncateOutput(output string) string {
	trimmed := strings.TrimSpace(output)
	if len(trimmed) <= maxReportedOutputBytes {
		return trimmed
	}
	return trimmed[:maxReportedOutputBytes] + "... (truncated)"
}
