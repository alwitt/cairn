package artifact_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alwitt/cairn/models"
	"github.com/apex/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// ======================================================================================
// DownloadArtifact

func TestOperatorDownloadArtifact(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	// Case 1: the happy path. One sidecar, no registration step - a GET binds no size or
	// checksum up front the way the upload path's PUT does.
	t.Run("downloads into the workspace volume", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")
		target := models.WorkspaceMountPath + "/out.txt"
		getURL := "https://store.unit-test/get?X-Amz-Signature=deadbeef"

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return(getURL, nil).
			Once()
		captured := expectSidecarRun(t, mocks, sidecarOutput(map[string]any{
			"ok": true, "downloaded_path": target, "size": entry.Size,
		}), 0)

		assert.Nil(operator.DownloadArtifact(utCtx, workspace, entry, target))

		// Both parameters reach the sidecar by environment variable. The URL in particular
		// must never travel as an argument, where its signature would land in
		// /proc/<pid>/cmdline.
		reachedTarget, ok := envValue(captured.Environment, "CAIRN_TARGET_PATH")
		assert.True(ok)
		assert.Equal(target, reachedTarget)

		reachedURL, ok := envValue(captured.Environment, "CAIRN_URL")
		assert.True(ok)
		assert.Equal(getURL, reachedURL)
	})

	// Case 2: the volume precondition. A caller cannot provision a volume - that is an
	// operator's job over REST - so this is reported as a legible precondition rather than
	// surfacing later as a raw container mount failure.
	t.Run("refuses a workspace with no runtime volume", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		workspace.VolumeState = models.WorkspaceVolumeStateNone
		entry := sampleArtifact(workspace, "unit-test-artifact")

		err := operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		)

		assertOperatorBadInputError(assert, err)
		assert.Contains(err.Error(), "no runtime volume")
		// Nothing is minted for a workspace that cannot be mounted. The mock has no
		// expectation for it, so any call would fail this test on its own.
		mocks.manager.AssertNotCalled(t, "GenerateGetURLForArtifact")
	})

	// Case 3: a failure to mint. The manager refuses a MISSING_OBJECT artifact here, so this
	// is also how "the backing object is gone" surfaces - and no sidecar should be launched.
	t.Run("surfaces a failure to mint the GET URL", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")
		mintErr := fmt.Errorf("artifact is not in a servable state")

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("", mintErr).
			Once()

		err := operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		)

		assertOperatorError(assert, err, mintErr)
	})

	// Case 4: the sidecar's own message is what the caller needs. "the destination directory
	// does not exist" tells them what to do; "exit status 1" does not.
	t.Run("surfaces the sidecar's reported failure", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")
		reason := "destination directory '/mnt/cairn/ws/nope' does not exist; " +
			"create it before downloading into it"

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("https://store.unit-test/get", nil).
			Once()
		expectSidecarRun(t, mocks, sidecarOutput(map[string]any{
			"ok": false, "error": reason,
		}), 1)

		err := operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/nope/out.txt",
		)

		// Reported as bad input: the caller supplied a destination they had not prepared.
		assertOperatorBadInputError(assert, err)
		assert.Contains(err.Error(), reason)
	})

	// Case 5: the fallback. A sidecar that failed without explaining itself must still fail
	// the operation rather than passing for success.
	t.Run("fails a non-zero exit code carrying no reason", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("https://store.unit-test/get", nil).
			Once()
		expectSidecarRun(t, mocks, sidecarOutput(map[string]any{"ok": false}), 3)

		err := operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		)

		assert.NotNil(err)
		assert.Contains(err.Error(), "without a reason")
		assert.Contains(err.Error(), "3", "the exit code is all there is to report")
	})

	// Case 6: `ok: true` with a non-zero exit code is a self-contradicting sidecar. Trusting
	// the payload alone would report a failed transfer as a success.
	t.Run("fails when the payload and the exit code disagree", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("https://store.unit-test/get", nil).
			Once()
		expectSidecarRun(t, mocks, sidecarOutput(map[string]any{"ok": true}), 1)

		err := operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		)

		assert.NotNil(err)
	})

	// Case 7: the GET URL's TTL only has to outlive the transfer it was minted for.
	t.Run("mints a URL that outlives the sidecar timeout", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")

		var mintedTTL time.Duration
		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			RunAndReturn(func(
				_ context.Context, _ models.Artifact, ttl time.Duration,
			) (string, error) {
				mintedTTL = ttl
				return "https://store.unit-test/get", nil
			}).
			Once()
		expectSidecarRun(t, mocks, sidecarOutput(map[string]any{"ok": true}), 0)

		assert.Nil(operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		))

		// A URL that expires before the transfer it was minted for would fail every
		// download of a large artifact.
		sidecarTimeout := time.Second * time.Duration(unitTestSidecarTimeoutSec)
		assert.Greater(mintedTTL, sidecarTimeout, "the URL must outlive the transfer")
	})
}

// ======================================================================================
// Presigned URL containment
//
// The signature travels in the URL's query string, so a URL echoed into an error would leak a
// usable credential into whatever logs that error (see DESIGN §5.2).

func TestOperatorDownloadDoesNotLeakURL(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	// The signature a leak would expose. Distinctive enough that finding it anywhere in an
	// error message is unambiguous.
	const secretSignature = "X-Amz-Signature=00ff00ff00ff00ff-unit-test-secret"
	getURL := "https://store.unit-test/get?" + secretSignature

	// Every way a download can fail after the URL has been minted - the URL is in the
	// operator's hands for all of them.
	t.Run("keeps the URL out of a sidecar failure", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return(getURL, nil).
			Once()
		expectSidecarRun(t, mocks, sidecarOutput(map[string]any{
			"ok": false, "error": "the object store returned HTTP 403 for the download",
		}), 1)

		err := operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		)

		assert.NotNil(err)
		assert.NotContains(err.Error(), secretSignature)
	})

	// The sidecar is written not to echo its URL, but this path exists precisely for the case
	// where it misbehaved, so its output is not trusted to have honored that.
	t.Run("keeps the URL out of a message the sidecar itself reported", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return(getURL, nil).
			Once()
		expectSidecarRun(t, mocks, sidecarOutput(map[string]any{
			"ok": false, "error": "download request failed for " + getURL,
		}), 1)

		err := operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		)

		assert.NotNil(err)
		assert.NotContains(err.Error(), secretSignature)
		assert.Contains(err.Error(), "download request failed")
	})

	t.Run("keeps the URL out of a launch failure", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return(getURL, nil).
			Once()
		mocks.callbacks.EXPECT().
			DefineSystemCallDockerRuntime(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(nil, fmt.Errorf("no such image")).
			Once()

		err := operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		)

		assert.NotNil(err)
		assert.NotContains(err.Error(), secretSignature)
	})

	// The worst case: a sidecar that echoes its own URL back in an unparseable crash dump.
	// The operator quotes raw output when it finds no result line, so it must redact.
	t.Run("keeps the URL out of quoted raw output", func(t *testing.T) {
		assert := assert.New(t)

		operator, mocks := newUnitTestOperator(t)
		workspace := sampleWorkspace("unit-test-workspace")
		entry := sampleArtifact(workspace, "unit-test-artifact")

		mocks.manager.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return(getURL, nil).
			Once()
		expectSidecarRun(t, mocks, "Traceback: failed to GET "+getURL+"\n", 1)

		err := operator.DownloadArtifact(
			utCtx, workspace, entry, models.WorkspaceMountPath+"/out.txt",
		)

		assert.NotNil(err)
		assert.NotContains(
			err.Error(), secretSignature,
			"quoted sidecar output must not carry the presigned URL",
		)
		// The rest of the diagnostic still has to survive, or the redaction has thrown away
		// the evidence along with the secret.
		assert.True(
			strings.Contains(err.Error(), "Traceback") ||
				strings.Contains(err.Error(), "no parseable result line"),
			"the failure must still be diagnosable: %s", err.Error(),
		)
	})
}
