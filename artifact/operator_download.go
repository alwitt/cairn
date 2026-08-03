package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/alwitt/goutils/runtime"
	"github.com/apex/log"
)

// ======================================================================================
// Artifact Download
//
// The read direction: object store to workspace volume, over a presigned GET URL, in a single
// sidecar (see DESIGN §7.4).

// transferResult the result line a transfer sidecar emits.
//
// `Error` is a pointer so an absent field stays distinguishable from an empty one - a
// sidecar reporting failure without saying why is a different situation from one that said
// nothing at all, and the two produce different messages.
type transferResult struct {
	OK    bool    `json:"ok"`
	Error *string `json:"error"`
}

// getURLTTLMargin how much longer than the sidecar's own timeout a presigned GET URL lives.
//
// The URL only has to outlive the transfer it was minted for (see DESIGN §5.2), and the
// sidecar timeout is exactly how long that transfer may take. The margin covers container
// startup ahead of the first request.
const getURLTTLMargin = time.Minute

/*
DownloadArtifact download an artifact's content into a workspace's persistent volume.

The read direction of the artifact transfer: a single sidecar, and no registration step, since
a GET binds no size or checksum up front the way the upload path's PUT does (see DESIGN §7.4).

The sidecar mounts the workspace volume and pulls the artifact over a presigned GET URL. It
never calls back into this service, and holds no object store credential beyond that URL - a
short-lived bearer token scoped to one key and one operation (see DESIGN §5.1, §5.2).

The destination's parent directory must already exist. Creating it is deliberately not
attempted: this service does not control the UID the tool containers run as, so any directory
the sidecar created would be owned by the sidecar's UID and unusable by them (see DESIGN
§7.5.1).

A partially written file may remain in the volume after a mid-transfer failure. The volume is
disposable scratch and the caller is told the download failed, so cleanup would add a failure
mode without adding a guarantee (see DESIGN §7.5.1).

The parent workspace and the artifact are taken as already resolved by the caller (see DESIGN
§3).

	@param ctx context.Context - execution context
	@param workspace models.Workspace - workspace this is for
	@param artifact models.Artifact - the artifact to download
	@param targetPath string - where to write the artifact within the workspace volume. Must
	    be absolute, and within the volume mount.
*/
func (o *dockerOperatorImpl) DownloadArtifact(
	ctx context.Context,
	workspace models.Workspace,
	artifact models.Artifact,
	targetPath string,
) error {
	logTags := o.GetLogTagsForContext(ctx)

	failure := func(err error) error {
		return models.NewArtifactOperatorError(
			fmt.Sprintf(
				"failed to download artifact %s to '%s'", artifact.ID, targetPath,
			), err, true,
		)
	}

	if err := validateWorkspacePath(targetPath); err != nil {
		return failure(err)
	}

	// The volume must exist before anything is minted or launched. A caller cannot provision
	// it - that is an operator's job over REST - so this is reported as a legible
	// precondition rather than surfacing later as a raw container mount failure (see DESIGN
	// §7.5).
	if workspace.VolumeState != models.WorkspaceVolumeStateReady {
		return failure(goutils.NewBadInputError(
			fmt.Sprintf(
				"workspace %s has no runtime volume (volume state '%s')",
				workspace.ID, workspace.VolumeState,
			), nil, true,
		))
	}

	// Minted with `Content-Disposition: attachment` forced, and refused outright for an
	// artifact whose backing object is gone - both already handled by the manager, so neither
	// is re-implemented here (see DESIGN §6.5, §7.1).
	getURL, err := o.manager.GenerateGetURLForArtifact(
		ctx, artifact, o.sidecarConfig.SidecarTimeout()+getURLTTLMargin,
	)
	if err != nil {
		return failure(err)
	}

	// The URL travels by environment variable, never argv, so its signature stays out of
	// `/proc/<pid>/cmdline` (see DESIGN §5.2).
	sidecarEnv := []runtime.ContainerEnvVar{
		{Name: sidecarEnvTargetPath, Value: targetPath},
		{Name: sidecarEnvURL, Value: getURL},
	}

	resultLine, exitCode, err := o.runSidecar(
		ctx,
		o.sidecarContainerName(sidecarDownloadEntrypoint, artifact.ID),
		sidecarDownloadEntrypoint,
		workspace,
		sidecarEnv,
		// A transfer sidecar must reach the object store. Leaving this unset would inherit
		// the runtime's `none` default and leave it unable to connect at all.
		o.sidecarConfig.TransferNetworkMode(),
	)
	if err != nil {
		return failure(err)
	}

	var result transferResult
	if err := json.Unmarshal(resultLine, &result); err != nil {
		return failure(fmt.Errorf(
			"failed to parse the download sidecar's result line: %w", err,
		))
	}

	if !result.OK || exitCode != 0 {
		return failure(describeTransferFailure(result, exitCode, sidecarEnv))
	}

	log.
		WithFields(logTags).
		WithField("workspace", workspace.ID).
		WithField("artifact", artifact.ID).
		WithField("target_path", targetPath).
		Debug("Downloaded artifact into workspace volume")

	return nil
}

// describeTransferFailure turn a failed transfer sidecar run into an error worth reading.
//
// The sidecar's own message is preferred whenever it supplied one: it names what actually went
// wrong - a missing destination directory, an object store rejection - where the exit code
// alone says only that something did. The exit code is the fallback for a sidecar that failed
// without explaining itself.
//
// The message is redacted before it is surfaced. The sidecar is written not to echo its URL,
// but this error is about the case where the sidecar misbehaved, so its output is not trusted
// to have honored that (see DESIGN §5.2).
func describeTransferFailure(
	result transferResult, exitCode int, env []runtime.ContainerEnvVar,
) error {
	if result.Error != nil && *result.Error != "" {
		return goutils.NewBadInputError(redactSecrets(*result.Error, env), nil, true)
	}
	return fmt.Errorf(
		"the transfer sidecar reported failure without a reason (exit code %d)", exitCode,
	)
}
