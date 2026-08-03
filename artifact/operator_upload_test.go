package artifact_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/alwitt/cairn/artifact"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// ======================================================================================
// UploadArtifact / UpdateArtifact test harness

const (
	// unitTestSourceDigest a base64 SHA-256 of the shape the stat sidecar reports. Tests
	// compare against it byte-for-byte: the value the sidecar produces is what the presigned
	// PUT binds, so any transcoding on the way through is a defect.
	unitTestSourceDigest = "n4bQgYhMfWWaL+qgxVrQFaO/TxsrC4Is0V1sFbDwCgg="
	// unitTestSourceSize a source file size of the shape the stat sidecar reports
	unitTestSourceSize = int64(4096)
)

// validStatOutput frame a stat sidecar result line accepting a source file.
func validStatOutput(resolvedPath string) string {
	return sidecarOutput(map[string]any{
		"resolved_path": resolvedPath,
		"valid":         true,
		"size":          unitTestSourceSize,
		"sha256_b64":    unitTestSourceDigest,
	})
}

// invalidStatOutput frame a stat sidecar result line rejecting a source file. A rejection
// still carries every field, so this mirrors that rather than emitting a bare error.
func invalidStatOutput(reason string) string {
	return sidecarOutput(map[string]any{
		"resolved_path": nil,
		"valid":         false,
		"size":          0,
		"sha256_b64":    "",
		"error":         reason,
	})
}

// okTransferOutput frame an upload sidecar result line reporting success.
func okTransferOutput(resolvedPath string) string {
	return sidecarOutput(map[string]any{
		"ok": true, "uploaded_path": resolvedPath, "size": unitTestSourceSize,
	})
}

// artifactNotFoundError build the error the manager produces for an artifact name that is
// free, wrapped the way the manager really wraps it.
//
// Nesting it rather than returning a bare NotFoundError is the point: the operator has to
// find the not-found through two layers of wrapping, which is what the real call gives it.
func artifactNotFoundError(workspace models.Workspace, name string) error {
	return models.NewArtifactMangerError(
		fmt.Sprintf("failed to fetch artifact '%s' of workspace %s", name, workspace.ID),
		goutils.NewPersistenceError(
			fmt.Sprintf("failed to read artifact '%s' of workspace %s", name, workspace.ID),
			goutils.NewNotFoundError(
				fmt.Sprintf("artifact '%s/%s' does not exist", workspace.ID, name), nil, true,
			),
			true,
		),
		true,
	)
}

// expectNameIsFree arrange the name pre-check to report the target name as available.
func expectNameIsFree(mocks unitTestOperatorMocks, workspace models.Workspace, name string) {
	mocks.manager.EXPECT().
		GetArtifactByName(mock.Anything, workspace, name, mock.Anything).
		Return(models.Artifact{}, artifactNotFoundError(workspace, name)).
		Once()
}

// stagingBundle build the mint result the manager returns for a workspace.
func stagingBundle(workspace models.Workspace) artifact.StagingUploadBundle {
	return artifact.StagingUploadBundle{
		StagingObjectKey: stagingKeyFor(workspace),
		PutURL: fmt.Sprintf(
			"https://store.unit-test/%s?X-Amz-Signature=deadbeefcafe", workspace.ID,
		),
	}
}

// mintCall the arguments the staging mint was called with.
type mintCall struct {
	size        int64
	digest      string
	contentType *string
}

// expectStagingMint arrange the staging PUT URL mint, capturing what it was asked to bind.
func expectStagingMint(
	mocks unitTestOperatorMocks, bundle artifact.StagingUploadBundle,
) *mintCall {
	captured := new(mintCall)

	mocks.manager.EXPECT().
		GetArtifactStagingPutURL(
			mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
		).
		RunAndReturn(func(
			_ context.Context,
			_ models.Workspace,
			size int64,
			digest string,
			contentType *string,
		) (artifact.StagingUploadBundle, error) {
			captured.size = size
			captured.digest = digest
			captured.contentType = contentType
			return bundle, nil
		}).
		Once()

	return captured
}

// ======================================================================================
// UploadArtifact

func TestOperatorUploadArtifact(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	source := models.WorkspaceMountPath + "/build/report.json"

	// Case 1: the happy path. Two sidecars in sequence, then the existing staging core - the
	// staging key the mint produced is what registration must be handed.
	t.Run("uploads a volume file as a new artifact", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		bundle := stagingBundle(workspace)
		registered := sampleArtifact(workspace, "unit-test-artifact")

		expectNameIsFree(mocks, workspace, "unit-test-artifact")
		launches := expectSidecarSequence(
			t, mocks,
			sidecarRun{output: validStatOutput(source)},
			sidecarRun{output: okTransferOutput(source)},
		)
		expectStagingMint(mocks, bundle)
		mocks.manager.EXPECT().
			RegisterNewArtifact(
				mock.Anything, workspace, bundle.StagingObjectKey, "unit-test-artifact",
				mock.Anything, mock.Anything,
			).
			Return(registered, nil).
			Once()

		entry, err := operator.UploadArtifact(
			utCtx, workspace, source, "unit-test-artifact", nil, nil,
		)

		assert.Nil(err)
		assert.Equal(registered, entry)

		// The order is the contract: the hash has to exist before the URL it binds can be
		// minted, so stat cannot follow upload.
		assert.Equal("cairn-stat", launches[0].entrypoint())
		assert.Equal("cairn-upload", launches[1].entrypoint())
	})

	// Case 2: the stat sidecar's posture. It reads the volume and answers a question; it has
	// no reason to reach anything, and is handed nothing that would let it.
	t.Run("hands the stat sidecar no network and no credential", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		bundle := stagingBundle(workspace)

		expectNameIsFree(mocks, workspace, "unit-test-artifact")
		launches := expectSidecarSequence(
			t, mocks,
			sidecarRun{output: validStatOutput(source)},
			sidecarRun{output: okTransferOutput(source)},
		)
		expectStagingMint(mocks, bundle)
		mocks.manager.EXPECT().
			RegisterNewArtifact(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything,
			).
			Return(sampleArtifact(workspace, "unit-test-artifact"), nil).
			Once()

		_, err := operator.UploadArtifact(
			utCtx, workspace, source, "unit-test-artifact", nil, nil,
		)
		assert.Nil(err)

		stat := launches[0].params

		// Empty rather than routable: the container runtime reads an unset network mode as
		// "none", which is exactly what this sidecar should get.
		assert.Empty(stat.NetworkMode)

		// No URL means no object store credential in the container at all.
		_, hasURL := envValue(stat.Environment, "CAIRN_URL")
		assert.False(hasURL, "the stat sidecar must hold no presigned URL")

		reachedSource, ok := envValue(stat.Environment, "CAIRN_SOURCE_PATH")
		assert.True(ok)
		assert.Equal(source, reachedSource)

		reachedMount, ok := envValue(stat.Environment, "CAIRN_MOUNT_ROOT")
		assert.True(ok)
		assert.Equal(models.WorkspaceMountPath, reachedMount)

		assert.Len(stat.VolumeMounts, 1)
		assert.Equal(workspace.VolumeName, stat.VolumeMounts[0].Name)
		assert.Equal(models.WorkspaceMountPath, stat.VolumeMounts[0].MountPath)
	})

	// Case 3: the values the stat sidecar produced have to reach the mint and then the upload
	// sidecar unaltered. The URL's signature covers them, so a transcode anywhere along the
	// way produces a PUT the object store rejects.
	t.Run("carries the stat block through the mint to the upload", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		bundle := stagingBundle(workspace)

		expectNameIsFree(mocks, workspace, "unit-test-artifact")
		launches := expectSidecarSequence(
			t, mocks,
			sidecarRun{output: validStatOutput(source)},
			sidecarRun{output: okTransferOutput(source)},
		)
		mint := expectStagingMint(mocks, bundle)
		mocks.manager.EXPECT().
			RegisterNewArtifact(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything,
			).
			Return(sampleArtifact(workspace, "unit-test-artifact"), nil).
			Once()

		_, err := operator.UploadArtifact(
			utCtx, workspace, source, "unit-test-artifact", nil, nil,
		)
		assert.Nil(err)

		// The mint binds what the sidecar measured, base64 digest included - hex is what
		// `sha256sum` prints and what the object store would refuse.
		assert.Equal(unitTestSourceSize, mint.size)
		assert.Equal(unitTestSourceDigest, mint.digest)

		// No Content-Type is signed on this path: MIME is sniffed server-side at register, so
		// there is nothing verified to bind here.
		assert.Nil(mint.contentType)

		upload := launches[1].params

		reachedURL, ok := envValue(upload.Environment, "CAIRN_URL")
		assert.True(ok)
		assert.Equal(bundle.PutURL, reachedURL)

		reachedSize, ok := envValue(upload.Environment, "CAIRN_OBJECT_SIZE")
		assert.True(ok)
		assert.Equal("4096", reachedSize)

		reachedDigest, ok := envValue(upload.Environment, "CAIRN_SHA256_B64")
		assert.True(ok)
		assert.Equal(unitTestSourceDigest, reachedDigest)

		// Nothing signed a Content-Type, so sending one would put an unsigned header on a
		// signature-bound PUT.
		_, hasContentType := envValue(upload.Environment, "CAIRN_CONTENT_TYPE")
		assert.False(hasContentType, "the upload sidecar must send no unsigned Content-Type")

		// Unlike the stat sidecar, this one has to reach the object store.
		assert.Equal(models.DefaultSidecarNetworkMode, upload.NetworkMode)
	})

	// Case 4: an invalid source is an answer, not a failed run - and it must be acted on
	// before anything is minted.
	t.Run("rejects an invalid source before minting anything", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		reason := fmt.Sprintf("source path '%s' is a directory, not a single uploadable file", source)

		expectNameIsFree(mocks, workspace, "unit-test-artifact")
		// One run only. A second would have no expectation and would fail this case, which is
		// how "the upload sidecar never ran" is asserted.
		expectSidecarSequence(
			t, mocks, sidecarRun{output: invalidStatOutput(reason), exitCode: 1},
		)

		_, err := operator.UploadArtifact(
			utCtx, workspace, source, "unit-test-artifact", nil, nil,
		)

		assertOperatorBadInputError(assert, err)
		assert.Contains(err.Error(), "is a directory")
		mocks.manager.AssertNotCalled(t, "GetArtifactStagingPutURL")
		mocks.manager.AssertNotCalled(t, "RegisterNewArtifact")
	})

	// Case 4b: the verdict is the answer, and the exit code is not a second opinion on it. A
	// stat sidecar that reports an invalid source while exiting zero is contradicting itself,
	// and the safe reading is the one that refuses - accepting it would mint a PUT URL bound
	// to the size 0 and empty digest a rejection carries.
	t.Run("rejects an invalid source even on a zero exit code", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")

		expectNameIsFree(mocks, workspace, "unit-test-artifact")
		expectSidecarSequence(t, mocks, sidecarRun{
			output:   invalidStatOutput(fmt.Sprintf("source file '%s' does not exist", source)),
			exitCode: 0,
		})

		_, err := operator.UploadArtifact(
			utCtx, workspace, source, "unit-test-artifact", nil, nil,
		)

		assertOperatorBadInputError(assert, err)
		assert.Contains(err.Error(), "does not exist")
		mocks.manager.AssertNotCalled(t, "GetArtifactStagingPutURL")
	})

	// Case 5: the name pre-check. Two container runs are expensive to spend on a name the
	// database is going to refuse.
	t.Run("refuses a name already in use", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")

		mocks.manager.EXPECT().
			GetArtifactByName(mock.Anything, workspace, "unit-test-artifact", mock.Anything).
			Return(sampleArtifact(workspace, "unit-test-artifact"), nil).
			Once()

		_, err := operator.UploadArtifact(
			utCtx, workspace, source, "unit-test-artifact", nil, nil,
		)

		assertOperatorBadInputError(assert, err)
		assert.Contains(err.Error(), "already has an artifact named")
		mocks.manager.AssertNotCalled(t, "GetArtifactStagingPutURL")
	})

	// Case 6: the branch that makes the pre-check honest. A database that is down has not told
	// us the name is free, and treating its error as permission to proceed would spend both
	// sidecars before failing anyway.
	t.Run("propagates a name lookup failure rather than assuming free", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		lookupErr := goutils.NewPersistenceError("database is unreachable", nil, true)

		mocks.manager.EXPECT().
			GetArtifactByName(mock.Anything, workspace, "unit-test-artifact", mock.Anything).
			Return(models.Artifact{}, lookupErr).
			Once()

		_, err := operator.UploadArtifact(
			utCtx, workspace, source, "unit-test-artifact", nil, nil,
		)

		assertOperatorError(assert, err, lookupErr)
		// No sidecar is arranged, so any launch at all fails this case.
		mocks.manager.AssertNotCalled(t, "GetArtifactStagingPutURL")
	})

	// Case 7: the size cap lives inside the mint, so this is also how an over-cap file
	// surfaces - after one sidecar, never two.
	t.Run("surfaces a failure to mint the staging URL", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		mintErr := fmt.Errorf("artifact is 4096 bytes, over the 1024 byte single-PUT size cap")

		expectNameIsFree(mocks, workspace, "unit-test-artifact")
		expectSidecarSequence(t, mocks, sidecarRun{output: validStatOutput(source)})
		mocks.manager.EXPECT().
			GetArtifactStagingPutURL(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(artifact.StagingUploadBundle{}, mintErr).
			Once()

		_, err := operator.UploadArtifact(
			utCtx, workspace, source, "unit-test-artifact", nil, nil,
		)

		assertOperatorError(assert, err, mintErr)
		mocks.manager.AssertNotCalled(t, "RegisterNewArtifact")
	})

	// Case 8: a file that changed on the volume between the two sidecars. The object store
	// rejects the PUT because the bytes no longer match the signed checksum, and the sidecar's
	// account of that is far more useful than the exit code.
	t.Run("surfaces the upload sidecar's reported failure", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")

		reason := "the object store rejected the upload (HTTP 400); the source file " +
			"'/mnt/cairn/ws/build/report.json' most likely changed while the upload was in " +
			"flight, so its content no longer matches the checksum the upload was signed for"

		expectNameIsFree(mocks, workspace, "unit-test-artifact")
		expectSidecarSequence(
			t, mocks,
			sidecarRun{output: validStatOutput(source)},
			sidecarRun{
				output:   sidecarOutput(map[string]any{"ok": false, "error": reason}),
				exitCode: 1,
			},
		)
		expectStagingMint(mocks, stagingBundle(workspace))

		_, err := operator.UploadArtifact(
			utCtx, workspace, source, "unit-test-artifact", nil, nil,
		)

		assertOperatorBadInputError(assert, err)
		assert.Contains(err.Error(), "changed while the upload was in flight")
		mocks.manager.AssertNotCalled(t, "RegisterNewArtifact")
	})

	// Case 9: teardown on the failure paths. Both containers are arranged with exactly one
	// Cleanup each, so a missed teardown fails the case rather than leaking quietly.
	t.Run("tears down both containers when the upload fails", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		waitErr := fmt.Errorf("container wait was interrupted")

		expectNameIsFree(mocks, workspace, "unit-test-artifact")
		expectSidecarSequence(
			t, mocks,
			sidecarRun{output: validStatOutput(source)},
			sidecarRun{waitErr: waitErr},
		)
		expectStagingMint(mocks, stagingBundle(workspace))

		_, err := operator.UploadArtifact(
			utCtx, workspace, source, "unit-test-artifact", nil, nil,
		)

		assertOperatorError(assert, err, waitErr)
	})

	// Case 10: the volume precondition, and the path front gate. Neither can be repaired by
	// the caller mid-call, so both are legible refusals rather than container failures.
	t.Run("refuses a workspace with no runtime volume", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		workspace.VolumeState = models.WorkspaceVolumeStateNone

		expectNameIsFree(mocks, workspace, "unit-test-artifact")

		_, err := operator.UploadArtifact(
			utCtx, workspace, source, "unit-test-artifact", nil, nil,
		)

		assertOperatorBadInputError(assert, err)
		assert.Contains(err.Error(), "no runtime volume")
	})

	t.Run("rejects a source path outside the workspace volume", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")

		expectNameIsFree(mocks, workspace, "unit-test-artifact")

		_, err := operator.UploadArtifact(
			utCtx, workspace, "/etc/shadow", "unit-test-artifact", nil, nil,
		)

		assertOperatorBadInputError(assert, err)
		assert.Contains(err.Error(), "outside the workspace volume")
	})

	// Case 11: a registration failure after both sidecars ran. The staged object is left
	// behind for the sweep, so what matters is that the caller learns the upload failed.
	t.Run("surfaces a registration failure", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		registerErr := fmt.Errorf("artifact name is already taken")

		expectNameIsFree(mocks, workspace, "unit-test-artifact")
		expectSidecarSequence(
			t, mocks,
			sidecarRun{output: validStatOutput(source)},
			sidecarRun{output: okTransferOutput(source)},
		)
		expectStagingMint(mocks, stagingBundle(workspace))
		mocks.manager.EXPECT().
			RegisterNewArtifact(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything,
			).
			Return(models.Artifact{}, registerErr).
			Once()

		_, err := operator.UploadArtifact(
			utCtx, workspace, source, "unit-test-artifact", nil, nil,
		)

		assertOperatorError(assert, err, registerErr)
	})
}

// ======================================================================================
// UpdateArtifact

func TestOperatorUpdateArtifact(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	source := models.WorkspaceMountPath + "/build/report.json"

	// Case 1: the happy path. The same staged middle as upload, ending at the update core
	// rather than the register one - and no name pre-check, because the caller resolved the
	// artifact and that IS the existence check.
	t.Run("replaces an existing artifact's content", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")
		bundle := stagingBundle(workspace)
		updated := entry
		updated.Size = unitTestSourceSize

		launches := expectSidecarSequence(
			t, mocks,
			sidecarRun{output: validStatOutput(source)},
			sidecarRun{output: okTransferOutput(source)},
		)
		expectStagingMint(mocks, bundle)
		mocks.manager.EXPECT().
			UpdateArtifactContent(
				mock.Anything, entry, bundle.StagingObjectKey, mock.Anything,
			).
			Return(updated, nil).
			Once()

		result, err := operator.UpdateArtifact(utCtx, workspace, entry, source, nil)

		assert.Nil(err)
		assert.Equal(updated, result)
		mocks.manager.AssertNotCalled(t, "GetArtifactByName")

		// An update names its containers after the artifact it is replacing; an upload has no
		// artifact ID yet and names them after the workspace.
		assert.Contains(launches[0].name, entry.ID)
		assert.Contains(launches[1].name, entry.ID)
	})

	// Case 2: the pair the caller resolved separately, checked against each other exactly
	// once. The workspace decides which volume is read; the artifact decides which row is
	// rewritten. A mismatch would publish one workspace's bytes under another's artifact.
	t.Run("refuses an artifact belonging to another workspace", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		other := sampleWorkspace("unit-test-other-workspace")
		entry := sampleArtifact(other, "unit-test-artifact")

		_, err := operator.UpdateArtifact(utCtx, workspace, entry, source, nil)

		assertOperatorBadInputError(assert, err)
		assert.Contains(err.Error(), "belongs to workspace")
		// Nothing runs and nothing is minted for a mismatched pair.
		mocks.manager.AssertNotCalled(t, "GetArtifactStagingPutURL")
		mocks.manager.AssertNotCalled(t, "UpdateArtifactContent")
	})

	// Case 3: the repair path. An artifact quarantined as MISSING_OBJECT has no servable
	// object, which is precisely why re-uploading its content must be allowed - this is the
	// one write that fixes it.
	t.Run("accepts a MISSING_OBJECT artifact", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")
		entry.State = models.ArtifactStateMissingObject
		bundle := stagingBundle(workspace)

		repaired := entry
		repaired.State = models.ArtifactStateRecorded

		expectSidecarSequence(
			t, mocks,
			sidecarRun{output: validStatOutput(source)},
			sidecarRun{output: okTransferOutput(source)},
		)
		expectStagingMint(mocks, bundle)
		mocks.manager.EXPECT().
			UpdateArtifactContent(
				mock.Anything, entry, bundle.StagingObjectKey, mock.Anything,
			).
			Return(repaired, nil).
			Once()

		result, err := operator.UpdateArtifact(utCtx, workspace, entry, source, nil)

		assert.Nil(err)
		assert.Equal(models.ArtifactStateRecorded, result.State)
	})

	t.Run("rejects an invalid source before minting anything", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")

		expectSidecarSequence(t, mocks, sidecarRun{
			output:   invalidStatOutput(fmt.Sprintf("source file '%s' does not exist", source)),
			exitCode: 1,
		})

		_, err := operator.UpdateArtifact(utCtx, workspace, entry, source, nil)

		assertOperatorBadInputError(assert, err)
		assert.Contains(err.Error(), "does not exist")
		mocks.manager.AssertNotCalled(t, "GetArtifactStagingPutURL")
	})

	t.Run("surfaces an update failure", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")
		updateErr := fmt.Errorf("staged object is over the size cap")

		expectSidecarSequence(
			t, mocks,
			sidecarRun{output: validStatOutput(source)},
			sidecarRun{output: okTransferOutput(source)},
		)
		expectStagingMint(mocks, stagingBundle(workspace))
		mocks.manager.EXPECT().
			UpdateArtifactContent(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(models.Artifact{}, updateErr).
			Once()

		_, err := operator.UpdateArtifact(utCtx, workspace, entry, source, nil)

		assertOperatorError(assert, err, updateErr)
	})
}

// ======================================================================================
// Presigned URL containment
//
// The write path's counterpart to the download path's leak test. A PUT URL's signature travels
// in its query string, so a URL that reaches a caller-visible error is a live credential in
// whatever logs that error.

func TestOperatorUploadDoesNotLeakURL(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	source := models.WorkspaceMountPath + "/build/report.json"

	// Case 1: the crash. A sidecar that dies before emitting a result line has its raw output
	// quoted as the only evidence of what went wrong - and a dying upload sidecar is exactly
	// the one that may have printed its own URL into a traceback.
	t.Run("keeps the URL out of quoted raw output", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		bundle := stagingBundle(workspace)

		crash := fmt.Sprintf(
			"Traceback (most recent call last):\n"+
				"  File \"/usr/local/lib/cairn_sidecar/transfer.py\", line 140, in upload\n"+
				"    requests.put(%s, ...)\nMemoryError\n",
			bundle.PutURL,
		)

		expectNameIsFree(mocks, workspace, "unit-test-artifact")
		expectSidecarSequence(
			t, mocks,
			sidecarRun{output: validStatOutput(source)},
			sidecarRun{output: crash, exitCode: 137},
		)
		expectStagingMint(mocks, bundle)

		_, err := operator.UploadArtifact(
			utCtx, workspace, source, "unit-test-artifact", nil, nil,
		)

		assert.NotNil(err)
		assert.NotContains(err.Error(), bundle.PutURL)
		assert.NotContains(err.Error(), "X-Amz-Signature")
		// The rest of the evidence still has to survive, or the redaction has cost the only
		// diagnostic the caller had.
		assert.Contains(err.Error(), "MemoryError")
	})

	// Case 2: the sidecar's own reported message. It is written not to echo its URL, but this
	// is the path taken when the sidecar misbehaved, so its output is not trusted to have.
	t.Run("keeps the URL out of a reported failure", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		bundle := stagingBundle(workspace)

		expectNameIsFree(mocks, workspace, "unit-test-artifact")
		expectSidecarSequence(
			t, mocks,
			sidecarRun{output: validStatOutput(source)},
			sidecarRun{
				output: sidecarOutput(map[string]any{
					"ok":    false,
					"error": fmt.Sprintf("connection to %s failed", bundle.PutURL),
				}),
				exitCode: 1,
			},
		)
		expectStagingMint(mocks, bundle)

		_, err := operator.UploadArtifact(
			utCtx, workspace, source, "unit-test-artifact", nil, nil,
		)

		assert.NotNil(err)
		assert.NotContains(err.Error(), bundle.PutURL)
		assert.True(
			strings.Contains(err.Error(), "[REDACTED]"),
			"the redaction should be visible, not silently dropped",
		)
	})
}
