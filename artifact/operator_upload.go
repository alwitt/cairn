package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/alwitt/goutils/runtime"
	"github.com/apex/log"
)

// ======================================================================================
// Artifact Upload & Update
//
// The write direction: workspace volume to object store, over a presigned PUT URL, in TWO
// sidecars rather than one (see DESIGN §6.4).
//
// The second sidecar is not an accident of layering. A staging PUT URL binds the object's
// exact size and base64 SHA-256 into its signature before it can be minted, and a
// volume-based caller cannot supply either: the bytes live in the volume, reachable only
// from a container. So a stat/hash sidecar derives the pair first, and the upload sidecar
// then sends the file with exactly the headers that were signed.
//
// Everything past the upload is the manager's existing staging core, unchanged.

// statResult the result line the stat sidecar emits (see DESIGN §7.5.3).
//
// The verdict field is `valid`, not the `ok` a transfer sidecar reports - a deliberate
// difference (see DESIGN §5.3). "This file cannot be uploaded" is an answer the service acts
// on before minting anything, not a failed run, so the two are decoded into their own shapes
// rather than reconciled into one envelope.
//
// A rejection still carries every field, so only the two genuinely nullable ones are
// pointers: an invalid source reports `size` 0 and `sha256_b64` "" rather than omitting them.
type statResult struct {
	ResolvedPath *string `json:"resolved_path"`
	Valid        bool    `json:"valid"`
	Size         int64   `json:"size"`
	SHA256B64    string  `json:"sha256_b64"`
	Error        *string `json:"error"`
}

/*
UploadArtifact record a new artifact from a file in a workspace's persistent volume.

See the Operator interface for the full contract.

	@param ctx context.Context - execution context
	@param workspace models.Workspace - workspace this is for
	@param sourcePath string - the file to upload, within the workspace volume
	@param name string - name a user will reference the artifact by
	@param description *string - an optional description for the artifact
	@param activeSession db.Database - if set, an existing open DB transaction to work within
	@returns the new artifact entry
*/
func (o *dockerOperatorImpl) UploadArtifact(
	ctx context.Context,
	workspace models.Workspace,
	sourcePath string,
	name string,
	description *string,
	activeSession db.Database,
) (models.Artifact, error) {
	logTags := o.GetLogTagsForContext(ctx)

	failure := func(err error) error {
		return models.NewArtifactOperatorError(
			fmt.Sprintf(
				"failed to upload '%s' as artifact '%s' of workspace %s",
				sourcePath, name, workspace.ID,
			), err, true,
		)
	}

	if err := o.verifyArtifactNameFree(ctx, workspace, name, activeSession); err != nil {
		return models.Artifact{}, failure(err)
	}

	// The workspace ID is the container-name subject: there is no artifact ID to name these
	// runs after until the registration at the end succeeds.
	stagingObjKey, err := o.stageFromVolume(ctx, workspace, sourcePath, workspace.ID)
	if err != nil {
		return models.Artifact{}, failure(err)
	}

	// A failure from here on leaves the staged object unreferenced. It is deliberately not
	// cleaned up: the aged-staging sweep reclaims it, and the caller has already been told the
	// upload failed, so a cleanup call would add a failure mode without adding a guarantee
	// (see DESIGN §8.2.1).
	entry, err := o.manager.RegisterNewArtifact(
		ctx, workspace, stagingObjKey, name, description, activeSession,
	)
	if err != nil {
		return models.Artifact{}, failure(err)
	}

	log.
		WithFields(logTags).
		WithField("workspace", workspace.ID).
		WithField("artifact", entry.ID).
		WithField("source_path", sourcePath).
		Debug("Uploaded new artifact from workspace volume")

	return entry, nil
}

/*
UpdateArtifact replace an existing artifact's content from a file in a workspace's persistent
volume.

See the Operator interface for the full contract.

	@param ctx context.Context - execution context
	@param workspace models.Workspace - workspace this is for
	@param artifact models.Artifact - the artifact whose content is replaced
	@param sourcePath string - the file to upload, within the workspace volume
	@param activeSession db.Database - if set, an existing open DB transaction to work within
	@returns the updated artifact entry
*/
func (o *dockerOperatorImpl) UpdateArtifact(
	ctx context.Context,
	workspace models.Workspace,
	artifact models.Artifact,
	sourcePath string,
	activeSession db.Database,
) (models.Artifact, error) {
	logTags := o.GetLogTagsForContext(ctx)

	failure := func(err error) error {
		return models.NewArtifactOperatorError(
			fmt.Sprintf(
				"failed to update artifact %s content from '%s'", artifact.ID, sourcePath,
			), err, true,
		)
	}

	// The caller resolved the workspace and the artifact independently, and this is the only
	// place the pair is ever checked against each other. It matters because the two feed
	// different halves of the operation: the workspace supplies the volume that gets mounted
	// and read, the artifact supplies the row that gets rewritten. A mismatch would publish
	// one workspace's bytes as another workspace's artifact.
	if artifact.WorkspaceID != workspace.ID {
		return models.Artifact{}, failure(goutils.NewBadInputError(
			fmt.Sprintf(
				"artifact %s belongs to workspace %s, not workspace %s",
				artifact.ID, artifact.WorkspaceID, workspace.ID,
			), nil, true,
		))
	}

	// No artifact-state gate. A `MISSING_OBJECT` artifact is an explicitly legitimate target -
	// re-uploading content is how one is repaired - which is why this differs from the read
	// path, where only a `RECORDED` artifact has an object to serve (see DESIGN §6.3).
	stagingObjKey, err := o.stageFromVolume(ctx, workspace, sourcePath, artifact.ID)
	if err != nil {
		return models.Artifact{}, failure(err)
	}

	// As in UploadArtifact, a staged object orphaned past this point is left for the sweep.
	entry, err := o.manager.UpdateArtifactContent(ctx, artifact, stagingObjKey, activeSession)
	if err != nil {
		return models.Artifact{}, failure(err)
	}

	log.
		WithFields(logTags).
		WithField("workspace", workspace.ID).
		WithField("artifact", entry.ID).
		WithField("source_path", sourcePath).
		Debug("Updated artifact content from workspace volume")

	return entry, nil
}

/*
verifyArtifactNameFree confirm no artifact by this name exists in the workspace yet.

A pre-sidecar fail-fast (see DESIGN §7.3): spending two container runs only to be rejected by
the database's uniqueness constraint is the waste this exists to avoid. The constraint is
still the real guard - this loses to a caller that races another between here and the insert,
and is meant to.

The three-way branch is the point of the function. Treating any error as "the name is free"
would turn a database outage into two wasted container runs followed by a constraint
violation, which is exactly the outcome being avoided.

	@param ctx context.Context - execution context
	@param workspace models.Workspace - workspace this is for
	@param name string - the artifact name to check
	@param activeSession db.Database - if set, an existing open DB transaction to work within
*/
func (o *dockerOperatorImpl) verifyArtifactNameFree(
	ctx context.Context, workspace models.Workspace, name string, activeSession db.Database,
) error {
	_, err := o.manager.GetArtifactByName(ctx, workspace, name, activeSession)

	if err == nil {
		return goutils.NewBadInputError(
			fmt.Sprintf(
				"workspace %s already has an artifact named '%s'", workspace.ID, name,
			), nil, true,
		)
	}

	// Not-found is the success case here. It survives the manager's wrapping because the
	// goutils error types unwrap, so matching on the type reaches it through the chain.
	var notFound goutils.NotFoundError
	if errors.As(err, &notFound) {
		return nil
	}

	return err
}

/*
stageFromVolume run the two sidecars that move a volume-resident file into a staging object.

Returns the staging object key the register/update core then consumes. This is the entire
shared middle of the write path: upload and update differ only in what they do before it and
what they call after it.

	@param ctx context.Context - execution context
	@param workspace models.Workspace - workspace whose volume holds the file
	@param sourcePath string - the file to stage, within the workspace volume
	@param subject string - what these sidecar runs are for, for the container names
	@returns the staging object key the bytes were uploaded to
*/
func (o *dockerOperatorImpl) stageFromVolume(
	ctx context.Context, workspace models.Workspace, sourcePath string, subject string,
) (string, error) {
	logTags := o.GetLogTagsForContext(ctx)

	if err := validateWorkspacePath(sourcePath); err != nil {
		return "", err
	}

	// The volume must exist before anything is launched. A caller cannot provision it - that
	// is an operator's job over REST - so this is reported as a legible precondition rather
	// than surfacing later as a raw container mount failure (see DESIGN §7.5).
	if workspace.VolumeState != models.WorkspaceVolumeStateReady {
		return "", goutils.NewBadInputError(
			fmt.Sprintf(
				"workspace %s has no runtime volume (volume state '%s')",
				workspace.ID, workspace.VolumeState,
			), nil, true,
		)
	}

	stat, err := o.statSourceFile(ctx, workspace, sourcePath, subject)
	if err != nil {
		return "", err
	}

	// The size cap fail-fast (see DESIGN §6.4) lives inside this call: the manager rejects an
	// over-cap size before minting anything, so there is no cap to re-check here.
	//
	// The digest passes through verbatim - base64 in, base64 out. That is what the presigned
	// PUT binds as `x-amz-checksum-sha256`, so transcoding it to hex would be rejected by the
	// object store on every upload (see DESIGN §6.4).
	//
	// No Content-Type is signed. MIME is sniffed server-side at register, so there is no
	// value here that anything has verified, and signing a guess would bind the upload to a
	// header the service invented (see DESIGN §6.1, §6.4).
	staging, err := o.manager.GetArtifactStagingPutURL(
		ctx, workspace, stat.Size, stat.SHA256B64, nil,
	)
	if err != nil {
		return "", err
	}

	if err := o.uploadSourceFile(ctx, workspace, sourcePath, subject, stat, staging); err != nil {
		return "", err
	}

	log.
		WithFields(logTags).
		WithField("workspace", workspace.ID).
		WithField("staging_object_key", staging.StagingObjectKey).
		WithField("source_path", sourcePath).
		Debug("Staged workspace volume file for artifact registration")

	return staging.StagingObjectKey, nil
}

// statSourceFile run the stat/hash sidecar over a source file and return its verdict.
//
// The pair it produces is about to be signed into a PUT URL as a matched set, which is why
// the sidecar derives both from a single streaming read - a stat-then-read would leave a
// window for the file to change between them (see DESIGN §6.4).
func (o *dockerOperatorImpl) statSourceFile(
	ctx context.Context, workspace models.Workspace, sourcePath string, subject string,
) (statResult, error) {
	sidecarEnv := []runtime.ContainerEnvVar{
		{Name: sidecarEnvSourcePath, Value: sourcePath},
	}

	resultLine, exitCode, err := o.runSidecar(
		ctx,
		o.sidecarContainerName(sidecarStatEntrypoint, subject),
		sidecarStatEntrypoint,
		workspace,
		sidecarEnv,
		// Empty on purpose, and the one place in this package where that is right: an unset
		// network mode inherits the container runtime's `none`, and the stat sidecar is meant
		// to have no network at all. It neither reaches the object store nor calls back into
		// this service, and it is handed no URL, so it holds no credential worth a route (see
		// DESIGN §5.1).
		"",
	)
	if err != nil {
		return statResult{}, err
	}

	var result statResult
	if err := json.Unmarshal(resultLine, &result); err != nil {
		return statResult{}, fmt.Errorf(
			"failed to parse the stat sidecar's result line: %w", err,
		)
	}

	if !result.Valid {
		return statResult{}, describeStatRejection(result, sidecarEnv)
	}

	// A valid verdict from a sidecar that then exited non-zero is incoherent - `cairn-stat`
	// exits zero exactly when `valid` is true - so the block is not trusted despite reading
	// well.
	if exitCode != 0 {
		return statResult{}, fmt.Errorf(
			"the stat sidecar reported a valid source but exited %d", exitCode,
		)
	}

	return result, nil
}

// uploadSourceFile run the upload sidecar to send a source file to a staging PUT URL.
func (o *dockerOperatorImpl) uploadSourceFile(
	ctx context.Context,
	workspace models.Workspace,
	sourcePath string,
	subject string,
	stat statResult,
	staging StagingUploadBundle,
) error {
	// Everything travels by environment variable, never argv, so the URL's signature stays out
	// of `/proc/<pid>/cmdline` (see DESIGN §5.2).
	//
	// The size is the one the URL was signed with rather than a fresh measurement: re-deriving
	// it in the sidecar would only mask the drift the checksum bind exists to catch (see
	// DESIGN §6.4).
	sidecarEnv := []runtime.ContainerEnvVar{
		{Name: sidecarEnvSourcePath, Value: sourcePath},
		{Name: sidecarEnvURL, Value: staging.PutURL},
		{Name: sidecarEnvObjectSize, Value: strconv.FormatInt(stat.Size, 10)},
		{Name: sidecarEnvSHA256B64, Value: stat.SHA256B64},
	}

	resultLine, exitCode, err := o.runSidecar(
		ctx,
		o.sidecarContainerName(sidecarUploadEntrypoint, subject),
		sidecarUploadEntrypoint,
		workspace,
		sidecarEnv,
		// Unlike the stat sidecar, this one must reach the object store. Leaving this unset
		// would inherit the runtime's `none` default and leave it unable to connect at all.
		o.sidecarConfig.TransferNetworkMode(),
	)
	if err != nil {
		return err
	}

	var result transferResult
	if err := json.Unmarshal(resultLine, &result); err != nil {
		return fmt.Errorf("failed to parse the upload sidecar's result line: %w", err)
	}

	if !result.OK || exitCode != 0 {
		// This is where a file that changed on the volume between the two sidecars lands: the
		// bytes no longer match the checksum the PUT was signed for, and the object store's
		// rejection arrives as the sidecar's own legible message (see DESIGN §6.4).
		return describeTransferFailure(result, exitCode, sidecarEnv)
	}

	return nil
}

// describeStatRejection turn an invalid source verdict into an error worth reading.
//
// The sidecar's own message names what is actually wrong with the file - missing, a
// directory, a device node, a symlink pointing out of the volume - so it is preferred over
// anything reconstructable here. It is a caller-supplied path that was wrong, so this reports
// bad input and the API layer maps it onto a 4xx.
//
// The message is redacted even though the stat sidecar is never handed a URL. The invariant
// worth keeping is that every sidecar-supplied string is redacted before it is surfaced, with
// no exceptions to remember; the no-op cost today is what makes a future change that does
// hand this sidecar a URL safe by default.
func describeStatRejection(result statResult, env []runtime.ContainerEnvVar) error {
	if result.Error != nil && *result.Error != "" {
		return goutils.NewBadInputError(redactSecrets(*result.Error, env), nil, true)
	}
	return goutils.NewBadInputError(
		"the stat sidecar rejected the source file without a reason", nil, true,
	)
}
