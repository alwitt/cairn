package artifact

import (
	"context"
	"fmt"

	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils/runtime"
	"github.com/apex/log"
)

// ======================================================================================
// Sidecar Execution
//
// Byte movement between a workspace volume and the object store is done by short-lived,
// service-launched sidecar containers - never by a client, and never by the tool container
// that produced or consumes the file (see DESIGN §5).
//
// Every sidecar runs a fixed command over server-supplied values. None of them take an
// argument: each reads its parameters from the environment, which is what keeps a presigned
// URL out of `/proc/<pid>/cmdline` and leaves no argv for an agent-influenced value to reach
// (see DESIGN §5.1, §5.2).

// sidecarCapabilityDACOverride the one Linux capability an artifact sidecar is granted on top
// of the runtime's drop-everything default. See `buildSidecarParams` for why root alone does
// not cover it.
const sidecarCapabilityDACOverride = "DAC_OVERRIDE"

const (
	// sidecarStatEntrypoint the stat/hash sidecar's entrypoint
	sidecarStatEntrypoint = "cairn-stat"
	// sidecarUploadEntrypoint the upload sidecar's entrypoint
	sidecarUploadEntrypoint = "cairn-upload"
	// sidecarDownloadEntrypoint the download sidecar's entrypoint
	sidecarDownloadEntrypoint = "cairn-download"
)

const (
	// sidecarEnvMountRoot names the volume mount path for the sidecar
	sidecarEnvMountRoot = "CAIRN_MOUNT_ROOT"
	// sidecarEnvSourcePath names the source file the stat and upload sidecars read
	sidecarEnvSourcePath = "CAIRN_SOURCE_PATH"
	// sidecarEnvTargetPath names the destination file the download sidecar writes
	sidecarEnvTargetPath = "CAIRN_TARGET_PATH"
	// sidecarEnvURL carries the presigned URL. Never logged (see DESIGN §5.2).
	sidecarEnvURL = "CAIRN_URL"
	// sidecarEnvObjectSize carries the byte size the upload sidecar sends as `Content-Length`
	sidecarEnvObjectSize = "CAIRN_OBJECT_SIZE"
	// sidecarEnvSHA256B64 carries the base64 SHA-256 the upload sidecar sends as
	// `x-amz-checksum-sha256`
	sidecarEnvSHA256B64 = "CAIRN_SHA256_B64"

	// There is deliberately no constant for the sidecar's `CAIRN_CONTENT_TYPE`. The upload
	// sidecar sends a `Content-Type` header only when that variable is set, and the
	// volume-based path signs none - MIME is sniffed server-side at register, so there is no
	// verified value to sign here (see DESIGN §6.4). Setting it would put an unsigned header
	// on a signature-bound PUT, which the object store rejects.
)

// sidecarContainerName build the container name for a sidecar run.
//
// The deployment's application name prefixes it so a shared docker host's containers stay
// attributable to the cairn instance that launched them, and the subject identifies what the
// run was for. The runtime appends its own unique suffix, so this need not be unique itself.
func (o *dockerOperatorImpl) sidecarContainerName(entrypoint string, subject string) string {
	return fmt.Sprintf("%s.%s.%s", o.appName, entrypoint, subject)
}

/*
runSidecar launch a sidecar, wait for it to finish, and return its result line.

The container is always cleaned up, including when the run failed - the runtime's own cleanup
is idempotent, so this is safe regardless of how far the launch got.

A non-zero exit code is NOT treated as a failure here. The sidecar reports what went wrong in
its result line, and that message is far more useful than the exit code it accompanies
("destination directory does not exist" versus "exit status 1"). Returning both leaves the
judgment to the caller, which is the only party that can decode the payload.

	@param ctx context.Context - execution context
	@param name string - container name
	@param entrypoint string - the sidecar command to run
	@param workspace models.Workspace - workspace whose volume is mounted
	@param env []runtime.ContainerEnvVar - the sidecar's parameters
	@param networkMode string - the container network mode. A sidecar that must not reach
	    anything is given "none", which is also what the container runtime defaults to - so
	    a sidecar that DOES need the network must be given a routable mode explicitly.
	@returns the raw result line, and the sidecar's exit code
*/
func (o *dockerOperatorImpl) runSidecar(
	ctx context.Context,
	name string,
	entrypoint string,
	workspace models.Workspace,
	env []runtime.ContainerEnvVar,
	networkMode string,
) ([]byte, int, error) {
	logTags := o.GetLogTagsForContext(ctx)

	params := o.buildSidecarParams(workspace, env, networkMode)

	sidecar, err := o.defineRuntime(
		ctx, name, runtime.ContainerCommand{Entrypoint: []string{entrypoint}}, params, true,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to define '%s' sidecar runtime: %w", entrypoint, err)
	}

	defer func() {
		// Logged rather than returned: the run's own outcome is what the caller needs, and
		// a leftover container must not mask it.
		if err := sidecar.Cleanup(ctx); err != nil {
			log.
				WithError(err).
				WithFields(logTags).
				WithField("sidecar", entrypoint).
				Warn("Failed to clean up sidecar container")
		}
	}()

	if err := sidecar.Start(ctx); err != nil {
		return nil, 0, fmt.Errorf("failed to start '%s' sidecar: %w", entrypoint, err)
	}

	resp, err := sidecar.Wait(ctx)
	if err != nil {
		return nil, resp.ExitCode, fmt.Errorf(
			"failed to run '%s' sidecar to completion: %w", entrypoint, err,
		)
	}

	resultLine, found := findSidecarResultLine(resp.Output)
	if !found {
		// No result line means a broken image or a crash before the sidecar could emit one.
		// A zero exit code does not make the work have happened, so this fails either way -
		// and quotes the raw output, which is the only evidence of what went wrong.
		//
		// Redacted first: a sidecar that dies mid-transfer may well have echoed its own URL
		// into a traceback, and quoting that verbatim would put a live signature into
		// whatever logs this error (see DESIGN §5.2).
		return nil, resp.ExitCode, fmt.Errorf(
			"'%s' sidecar produced no parseable result line (exit code %d); output: %s",
			entrypoint, resp.ExitCode, truncateOutput(redactSecrets(resp.Output, env)),
		)
	}

	log.
		WithFields(logTags).
		WithField("workspace", workspace.ID).
		WithField("sidecar", entrypoint).
		WithField("exit_code", resp.ExitCode).
		Debug("Sidecar run complete")

	return resultLine, resp.ExitCode, nil
}

// buildSidecarParams assemble the container runtime parameters for a sidecar.
//
// The security posture is the runtime's own defaults - a read-only root filesystem, all
// capabilities dropped, no new privileges - with exactly two relaxations, applied to every
// sidecar because every one of them touches the workspace volume (see DESIGN §5.1).
//
// The sidecar runs as `root` because the volume's mount root is root-owned (see DESIGN §4.2);
// a `nobody` sidecar could not write to it at all. Root alone is not enough, though: with all
// capabilities dropped, uid 0 may only touch what it *owns*, so it can neither write into a
// `0755` directory the agent created nor read a `0600` file the agent produced. `DAC_OVERRIDE`
// is what supplies that, and both directions need it - the download writes into a directory the
// agent laid out, and the stat/upload sidecars read a file it produced.
//
// `CHOWN` and `FOWNER` are deliberately NOT granted: after provisioning, cairn never re-owns or
// re-modes anything in the volume (see DESIGN §7.5.1). The read-only root filesystem is
// untouched by any of this - that is a mount flag, which no capability bypasses.
func (o *dockerOperatorImpl) buildSidecarParams(
	workspace models.Workspace, env []runtime.ContainerEnvVar, networkMode string,
) runtime.DockerRuntimeParams {
	// The mount path is supplied to the sidecar rather than compiled into its image, so the
	// same image keeps working if the canonical path ever becomes configurable (DESIGN §4.4).
	environment := []runtime.ContainerEnvVar{
		{Name: sidecarEnvMountRoot, Value: models.WorkspaceMountPath},
	}
	environment = append(environment, env...)
	for _, extra := range o.sidecarConfig.ExtraEnvs {
		environment = append(environment, runtime.ContainerEnvVar{
			Name: extra.Name, Value: extra.Value,
		})
	}

	// Note the shape change: the config carries one host per entry, while the runtime groups
	// several hostnames against a single address.
	extraHosts := make([]runtime.ContainerExtraHost, 0, len(o.sidecarConfig.ExtaHosts))
	for _, extra := range o.sidecarConfig.ExtaHosts {
		extraHosts = append(extraHosts, runtime.ContainerExtraHost{
			Hosts: []string{extra.Host}, Address: extra.Address,
		})
	}

	return runtime.DockerRuntimeParams{
		ContainerRuntimeParams: runtime.ContainerRuntimeParams{
			Image: o.sidecarConfig.Image,
			// Left nil: a batch run, so the result is collected from the container's output
			// rather than streamed out as it is produced.
			Streaming: nil,
			VolumeMounts: []runtime.ContainerVolumeMount{
				{Name: workspace.VolumeName, MountPath: models.WorkspaceMountPath},
			},
			AddCapabilities: []string{sidecarCapabilityDACOverride},
			ExtraHosts:      extraHosts,
			Environment:     environment,
			TimeoutSecs:     o.sidecarConfig.TimeoutSecs,
		},
		RunAsUser:   models.SidecarRunAsUser,
		RunAsGroup:  models.SidecarRunAsGroup,
		NetworkMode: networkMode,
	}
}
