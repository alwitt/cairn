package artifact_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/alwitt/cairn/artifact"
	"github.com/alwitt/cairn/db"
	mockdb "github.com/alwitt/cairn/mocks/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// ======================================================================================
// Text tool harness

// unitTestTextMIMEType the MIME type the harness sniffer reports for plain text
const unitTestTextMIMEType = "text/plain; charset=utf-8"

// stagingKeyArg match any staging object key the manager mints
func stagingKeyArg() any {
	return mock.MatchedBy(func(key string) bool {
		return strings.HasPrefix(key, unitTestStagingPrefix+"/")
	})
}

// storeKeyArg match any final storage object key the manager mints
func storeKeyArg() any {
	return mock.MatchedBy(func(key string) bool {
		return strings.HasPrefix(key, unitTestStorePrefix+"/")
	})
}

// sampleTextArtifact a RECORDED plain text artifact entry holding content, at a revision.
func sampleTextArtifact(
	workspace models.Workspace, content string, revision int64,
) models.Artifact {
	entry := sampleArtifact(workspace, "unit-test-text-artifact")
	entry.Size = int64(len(content))
	entry.Revision = revision
	return entry
}

// atNewRevision the same artifact after another writer committed new content: a new backing
// object, one revision higher.
func atNewRevision(entry models.Artifact, content string) models.Artifact {
	entry.ObjectKey = fmt.Sprintf(
		"%s/%s/%s", unitTestStorePrefix, entry.WorkspaceID, ulid.Make().String(),
	)
	entry.Size = int64(len(content))
	entry.Revision++
	return entry
}

// revisionConflict the error the persistence layer returns when a guarded update finds the entry
// has moved on.
func revisionConflict(artifactID string) error {
	return models.NewArtifactRevisionConflictError(
		fmt.Sprintf("artifact %s moved on", artifactID),
		goutils.NewConsistencyError("content changed", nil, false),
		false,
	)
}

// expectEntryRead arrange for one artifact entry read, in its own transaction.
func expectEntryRead(
	mocks unitTestManagerMocks, mockDatabase *mockdb.Database, entry models.Artifact,
) {
	mocks.persistence.EXPECT().
		UseDatabaseInTransaction(mock.Anything, mock.Anything).
		RunAndReturn(runTxForManager(mockDatabase)).
		Once()
	mockDatabase.EXPECT().
		GetArtifact(mock.Anything, entry.ID).
		Return(entry, nil).
		Once()
}

// expectObjectContent arrange for one fetch of an object's content.
func expectObjectContent(mocks unitTestManagerMocks, objectKey string, content string) {
	mocks.s3.EXPECT().
		GetObject(mock.Anything, unitTestBucket, objectKey).
		Return(goutils.S3ObjectStat{}, newFakeObjectReader([]byte(content)), nil).
		Once()
}

// stagedText what the manager wrote as a text staging object
type stagedText struct {
	key         string
	content     string
	size        int64
	contentType *string
}

// expectTextStaged arrange for one text blob to be staged: the in-memory sniff reports
// mimeType, and the PutObject is captured.
func expectTextStaged(mocks unitTestManagerMocks, mimeType string) *stagedText {
	staged := &stagedText{}
	mocks.callbacks.EXPECT().
		EstimateMIMEType(mock.Anything).
		Return(mimeType).
		Once()
	mocks.s3.EXPECT().
		PutObject(
			mock.Anything, unitTestBucket, stagingKeyArg(), mock.Anything, mock.Anything, mock.Anything,
		).
		Run(func(
			_ context.Context,
			_ string,
			key string,
			data io.Reader,
			size int64,
			contentType *string,
		) {
			raw, err := io.ReadAll(data)
			if err != nil {
				panic(err)
			}
			staged.key, staged.content, staged.size, staged.contentType = key, string(raw), size, contentType
		}).
		Return(nil).
		Once()
	return staged
}

// expectStagedPromotion arrange for a staged text blob to be promoted, the part register and
// commit share: the staging object is measured, sniffed, and copied to a new final key.
func expectStagedPromotion(mocks unitTestManagerMocks, staged *stagedText) {
	// The staged bytes only exist once the stage step has run, so these are served from the
	// capture at call time rather than fixed at arrangement time.
	mocks.s3.EXPECT().
		GetObjectStat(mock.Anything, unitTestBucket, stagingKeyArg()).
		RunAndReturn(func(context.Context, string, string) (goutils.S3ObjectStat, error) {
			return goutils.S3ObjectStat{Size: int64(len(staged.content))}, nil
		}).
		Once()
	mocks.s3.EXPECT().
		GetObject(mock.Anything, unitTestBucket, stagingKeyArg()).
		RunAndReturn(func(
			context.Context, string, string,
		) (goutils.S3ObjectStat, goutils.S3ObjectReader, error) {
			return goutils.S3ObjectStat{}, newFakeObjectReader([]byte(staged.content)), nil
		}).
		Once()
	mocks.callbacks.EXPECT().
		EstimateMIMEType(mock.Anything).
		Return(unitTestTextMIMEType).
		Once()
	mocks.s3.EXPECT().
		CopyObject(
			mock.Anything, unitTestBucket, stagingKeyArg(), unitTestBucket, storeKeyArg(), mock.Anything,
		).
		Return(nil).
		Once()
}

// expectNameFree arrange for one name lookup, in its own transaction, that finds no artifact.
func expectNameFree(
	mocks unitTestManagerMocks,
	mockDatabase *mockdb.Database,
	workspace models.Workspace,
	name string,
) {
	mocks.persistence.EXPECT().
		UseDatabaseInTransaction(mock.Anything, mock.Anything).
		RunAndReturn(runTxForManager(mockDatabase)).
		Once()
	mockDatabase.EXPECT().
		GetArtifactByName(mock.Anything, workspace.ID, name).
		Return(models.Artifact{}, artifactNameFreeError(workspace.ID, name)).
		Once()
}

// expectStagedRegister arrange for the registration of a staged text blob: the staging object
// is promoted, then the insert reports defineErr. A successful insert returns created and
// deletes the staging object. The insert's parameters are captured.
func expectStagedRegister(
	mocks unitTestManagerMocks,
	mockDatabase *mockdb.Database,
	staged *stagedText,
	defineErr error,
	created models.Artifact,
) *db.NewArtifactParameter {
	recorded := &db.NewArtifactParameter{}

	expectStagedPromotion(mocks, staged)

	mocks.persistence.EXPECT().
		UseDatabaseInTransaction(mock.Anything, mock.Anything).
		RunAndReturn(runTxForManager(mockDatabase)).
		Once()
	mockDatabase.EXPECT().
		DefineNewArtifact(mock.Anything, mock.Anything).
		Run(func(_ context.Context, params db.NewArtifactParameter) {
			*recorded = params
		}).
		Return(created, defineErr).
		Once()

	if defineErr == nil {
		mocks.s3.EXPECT().
			DeleteObject(mock.Anything, unitTestBucket, stagingKeyArg()).
			Return(nil).
			Once()
	}

	return recorded
}

// committedEdit what the guarded commit of a staged edit was called with
type committedEdit struct {
	expectedRevision *int64
}

// expectStagedCommit arrange for the commit of a staged edit: the staging object is promoted,
// then the entry update reports commitErr. A successful commit reads the entry back as updated
// and deletes the staging object.
func expectStagedCommit(
	mocks unitTestManagerMocks,
	mockDatabase *mockdb.Database,
	staged *stagedText,
	commitErr error,
	updated models.Artifact,
) *committedEdit {
	commit := &committedEdit{}

	expectStagedPromotion(mocks, staged)

	mocks.persistence.EXPECT().
		UseDatabaseInTransaction(mock.Anything, mock.Anything).
		RunAndReturn(runTxForManager(mockDatabase)).
		Once()
	mockDatabase.EXPECT().
		UpdateArtifactObject(
			mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
		).
		Run(func(
			_ context.Context, _ string, _ string, _ string, _ int64, expectedRevision *int64,
		) {
			commit.expectedRevision = expectedRevision
		}).
		Return(commitErr).
		Once()

	if commitErr == nil {
		mockDatabase.EXPECT().
			GetArtifact(mock.Anything, updated.ID).
			Return(updated, nil).
			Once()
		mocks.s3.EXPECT().
			DeleteObject(mock.Anything, unitTestBucket, stagingKeyArg()).
			Return(nil).
			Once()
	}

	return commit
}

// assertRevisionConflict verify an error is a revision conflict, which also reads as an
// actionable precondition failure.
func assertRevisionConflict(assert *assert.Assertions, err error) {
	assert.NotNil(err)
	var conflict models.ArtifactRevisionConflictError
	assert.True(
		errors.As(err, &conflict), "expected ArtifactRevisionConflictError, got %T: %v", err, err,
	)
	var consistency goutils.ConsistencyError
	assert.True(errors.As(err, &consistency), "expected ConsistencyError, got %T: %v", err, err)
}

// assertManagerErrorOfKind verify an error is a manager error whose chain holds target's type.
func assertManagerErrorOfKind[T error](assert *assert.Assertions, err error) {
	assert.NotNil(err)
	var managerErr models.ArtifactMangerError
	assert.True(errors.As(err, &managerErr), "expected ArtifactMangerError, got %T: %v", err, err)
	var target T
	assert.True(errors.As(err, &target), "expected %T in the chain, got %T: %v", target, err, err)
}

// ======================================================================================
// StageTextBlob

// TestManagerStageTextBlob validates text is staged verbatim under the workspace's staging
// prefix, and that content the text tools could not later read is refused before any write.
func TestManagerStageTextBlob(t *testing.T) {
	workspace := sampleWorkspace("unit-test-text-workspace")

	t.Run("stages the exact text under the workspace staging prefix", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		content := "line one\r\nline two\r\n"
		staged := expectTextStaged(mocks, unitTestTextMIMEType)

		stagingObjKey, err := manager.StageTextBlob(context.Background(), workspace, content)
		assert.Nil(err)
		assert.Equal(staged.key, stagingObjKey)
		assert.True(
			strings.HasPrefix(stagingObjKey, fmt.Sprintf("%s/%s/", unitTestStagingPrefix, workspace.ID)),
			"staging key %s is outside the workspace staging prefix", stagingObjKey,
		)
		assert.Equal(content, staged.content)
		assert.Equal(int64(len(content)), staged.size)
		// the artifact's MIME type comes from the server-side sniff, never from the staging write
		assert.Nil(staged.contentType)
	})

	// None of these may sniff or write: each is refused on the content alone, or on the sniff.
	t.Run("refuses text over the text size cap", func(t *testing.T) {
		manager, _ := newUnitTestManager(t)
		_, err := manager.StageTextBlob(
			context.Background(), workspace, strings.Repeat("a", int(unitTestMaxTextSize)+1),
		)
		assertManagerErrorOfKind[goutils.BadInputError](assert.New(t), err)
	})

	t.Run("refuses text holding a control character", func(t *testing.T) {
		manager, _ := newUnitTestManager(t)
		_, err := manager.StageTextBlob(context.Background(), workspace, "colored \x1b[1mtext\n")
		assertManagerErrorOfKind[goutils.BadInputError](assert.New(t), err)
	})

	t.Run("refuses text that sniffs as a non-text format", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		mocks.callbacks.EXPECT().
			EstimateMIMEType(mock.Anything).
			Return("application/pdf").
			Once()

		_, err := manager.StageTextBlob(context.Background(), workspace, "%PDF-1.7 looks like text\n")
		assertManagerErrorOfKind[goutils.BadInputError](assert, err)
		assert.Contains(err.Error(), "application/pdf")
	})
}

// ======================================================================================
// CreateTextArtifact

// TestManagerCreateTextArtifact validates the create path: the name is checked before anything
// is staged, the text is staged verbatim, and the registration records what was staged.
func TestManagerCreateTextArtifact(t *testing.T) {
	workspace := sampleWorkspace("unit-test-text-workspace")
	name := "notes/todo.txt"
	description := "a text artifact"

	t.Run("stages the text and registers it as a new artifact", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)
		mockDatabase := mockdb.NewDatabase(t)

		content := "line one\r\nline two\r\n"
		created := sampleTextArtifact(workspace, content, 1)
		created.Name = name

		expectNameFree(mocks, mockDatabase, workspace, name)
		staged := expectTextStaged(mocks, unitTestTextMIMEType)
		recorded := expectStagedRegister(mocks, mockDatabase, staged, nil, created)

		entry, err := manager.CreateTextArtifact(
			context.Background(), workspace, content, name, &description, nil,
		)
		assert.Nil(err)
		assert.Equal(created, entry)

		// the text is staged exactly as given, line endings included
		assert.Equal(content, staged.content)

		// the entry describes what was staged, measured and sniffed server side
		assert.Equal(workspace.ID, recorded.WorkspaceID)
		assert.Equal(name, recorded.Name)
		assert.Equal(&description, recorded.Description)
		assert.Equal(int64(len(content)), recorded.Size)
		assert.Equal(unitTestTextMIMEType, recorded.MIMEType)
		assert.True(
			strings.HasPrefix(recorded.ObjectKey, fmt.Sprintf("%s/%s/", unitTestStorePrefix, workspace.ID)),
			"object key %s is outside the workspace store prefix", recorded.ObjectKey,
		)
	})

	// No PutObject is arranged in the refusals below, so any staging write fails the test.
	t.Run("refuses a taken name before staging anything", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)
		mockDatabase := mockdb.NewDatabase(t)

		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()
		mockDatabase.EXPECT().
			GetArtifactByName(mock.Anything, workspace.ID, name).
			Return(sampleArtifact(workspace, name), nil).
			Once()

		_, err := manager.CreateTextArtifact(
			context.Background(), workspace, "hello\n", name, nil, nil,
		)
		assertManagerErrorOfKind[goutils.BadInputError](assert, err)
		assert.Contains(err.Error(), fmt.Sprintf("already has an artifact named '%s'", name))
	})

	t.Run("stages nothing when the name cannot be checked", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)
		mockDatabase := mockdb.NewDatabase(t)

		dbErr := fmt.Errorf("database is unreachable")
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()
		mockDatabase.EXPECT().
			GetArtifactByName(mock.Anything, workspace.ID, name).
			Return(models.Artifact{}, dbErr).
			Once()

		_, err := manager.CreateTextArtifact(
			context.Background(), workspace, "hello\n", name, nil, nil,
		)
		assertManagerErrorOfKind[goutils.PersistenceError](assert, err)
		assert.ErrorIs(err, dbErr)
	})

	t.Run("refuses text that is not plain text", func(t *testing.T) {
		manager, mocks := newUnitTestManager(t)
		mockDatabase := mockdb.NewDatabase(t)

		expectNameFree(mocks, mockDatabase, workspace, name)

		_, err := manager.CreateTextArtifact(
			context.Background(), workspace, "colored \x1b[1mtext\n", name, nil, nil,
		)
		assertManagerErrorOfKind[goutils.BadInputError](assert.New(t), err)
	})

	// No staging DeleteObject is arranged, so a cleanup attempt fails the test: the staged
	// object is left for the aged-staging sweep.
	t.Run("leaves the staged text when the insert fails", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)
		mockDatabase := mockdb.NewDatabase(t)

		dbErr := fmt.Errorf("unique constraint failed")
		expectNameFree(mocks, mockDatabase, workspace, name)
		staged := expectTextStaged(mocks, unitTestTextMIMEType)
		expectStagedRegister(mocks, mockDatabase, staged, dbErr, models.Artifact{})

		_, err := manager.CreateTextArtifact(
			context.Background(), workspace, "hello\n", name, nil, nil,
		)
		assertManagerErrorOfKind[goutils.PersistenceError](assert, err)
		assert.ErrorIs(err, dbErr)
	})
}

// ======================================================================================
// StageUpdatedArtifactText

// TestManagerStageUpdatedArtifactText validates one edit attempt: the edits land on the content
// in its own line ending, nothing is committed, and every refusal happens before any write.
func TestManagerStageUpdatedArtifactText(t *testing.T) {
	workspace := sampleWorkspace("unit-test-text-workspace")
	edits := []models.TextEdit{{OldText: "port: 8080", NewText: "port: 9090"}}

	t.Run("stages the edit in the artifact's own line ending", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)
		mockDatabase := mockdb.NewDatabase(t)

		content := "server:\r\n  port: 8080\r\n"
		entry := sampleTextArtifact(workspace, content, 4)
		expectEntryRead(mocks, mockDatabase, entry)
		expectObjectContent(mocks, entry.ObjectKey, content)
		staged := expectTextStaged(mocks, unitTestTextMIMEType)

		// an edit written in LF matches CRLF content
		read, stagingObjKey, err := manager.StageUpdatedArtifactText(
			context.Background(),
			entry.ID,
			[]models.TextEdit{{OldText: "server:\n  port: 8080", NewText: "server:\n  port: 9090"}},
			nil,
			nil,
		)
		assert.Nil(err)
		// the entry handed back is the one the edits were applied to, revision included
		assert.Equal(entry, read)
		assert.Equal(staged.key, stagingObjKey)
		assert.Equal("server:\r\n  port: 9090\r\n", staged.content)
	})

	t.Run("refuses a stale base revision before touching the object store", func(t *testing.T) {
		manager, mocks := newUnitTestManager(t)
		mockDatabase := mockdb.NewDatabase(t)

		entry := sampleTextArtifact(workspace, "port: 8080\n", 5)
		expectEntryRead(mocks, mockDatabase, entry)

		baseRevision := int64(4)
		_, _, err := manager.StageUpdatedArtifactText(
			context.Background(), entry.ID, edits, &baseRevision, nil,
		)
		assertRevisionConflict(assert.New(t), err)
	})

	// Each of these is refused from the entry alone, so none may fetch the content.
	refusedOnEntry := []struct {
		name   string
		mutate func(*models.Artifact)
		check  func(*assert.Assertions, error)
	}{
		{
			name:   "a quarantined artifact",
			mutate: func(entry *models.Artifact) { entry.State = models.ArtifactStateMissingObject },
			check:  assertManagerErrorOfKind[goutils.ConsistencyError],
		},
		{
			name:   "a non-text MIME type",
			mutate: func(entry *models.Artifact) { entry.MIMEType = "application/pdf" },
			check:  assertManagerErrorOfKind[goutils.BadInputError],
		},
		{
			name:   "an artifact over the text size cap",
			mutate: func(entry *models.Artifact) { entry.Size = unitTestMaxTextSize + 1 },
			check:  assertManagerErrorOfKind[goutils.BadInputError],
		},
	}
	for _, testCase := range refusedOnEntry {
		t.Run(fmt.Sprintf("refuses %s", testCase.name), func(t *testing.T) {
			manager, mocks := newUnitTestManager(t)
			mockDatabase := mockdb.NewDatabase(t)

			entry := sampleTextArtifact(workspace, "port: 8080\n", 1)
			testCase.mutate(&entry)
			expectEntryRead(mocks, mockDatabase, entry)

			_, _, err := manager.StageUpdatedArtifactText(
				context.Background(), entry.ID, edits, nil, nil,
			)
			testCase.check(assert.New(t), err)
		})
	}

	t.Run("refuses stored content that is not plain text", func(t *testing.T) {
		manager, mocks := newUnitTestManager(t)
		mockDatabase := mockdb.NewDatabase(t)

		content := "port: 8080 \x1b[0m\n"
		entry := sampleTextArtifact(workspace, content, 1)
		expectEntryRead(mocks, mockDatabase, entry)
		expectObjectContent(mocks, entry.ObjectKey, content)

		_, _, err := manager.StageUpdatedArtifactText(
			context.Background(), entry.ID, edits, nil, nil,
		)
		assertManagerErrorOfKind[goutils.BadInputError](assert.New(t), err)
	})

	t.Run("stages nothing when an edit does not match", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)
		mockDatabase := mockdb.NewDatabase(t)

		content := "port: 8080\n"
		entry := sampleTextArtifact(workspace, content, 1)
		expectEntryRead(mocks, mockDatabase, entry)
		expectObjectContent(mocks, entry.ObjectKey, content)

		_, _, err := manager.StageUpdatedArtifactText(
			context.Background(),
			entry.ID,
			[]models.TextEdit{{OldText: "port: 1234", NewText: "port: 9090"}},
			nil,
			nil,
		)
		assertManagerErrorOfKind[goutils.BadInputError](assert, err)
		assert.Contains(err.Error(), "edit 1: old_text not found")
	})

	t.Run("reports a conflict when the object is replaced mid-read", func(t *testing.T) {
		manager, mocks := newUnitTestManager(t)
		mockDatabase := mockdb.NewDatabase(t)

		entry := sampleTextArtifact(workspace, "port: 8080\n", 1)
		moved := atNewRevision(entry, "port: 7070\n")
		expectEntryRead(mocks, mockDatabase, entry)
		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, entry.ObjectKey).
			Return(goutils.S3ObjectStat{}, nil, fmt.Errorf("object reaped")).
			Once()
		// the failed fetch is checked against the entry, which has since moved on
		expectEntryRead(mocks, mockDatabase, moved)

		_, _, err := manager.StageUpdatedArtifactText(
			context.Background(), entry.ID, edits, nil, nil,
		)
		assertRevisionConflict(assert.New(t), err)
	})

	t.Run("keeps a fetch failure when the entry has not moved", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)
		mockDatabase := mockdb.NewDatabase(t)

		entry := sampleTextArtifact(workspace, "port: 8080\n", 1)
		storeErr := fmt.Errorf("object store unreachable")
		expectEntryRead(mocks, mockDatabase, entry)
		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, entry.ObjectKey).
			Return(goutils.S3ObjectStat{}, nil, storeErr).
			Once()
		expectEntryRead(mocks, mockDatabase, entry)

		_, _, err := manager.StageUpdatedArtifactText(
			context.Background(), entry.ID, edits, nil, nil,
		)
		assertManagerError(assert, err, storeErr)
		var conflict models.ArtifactRevisionConflictError
		assert.False(errors.As(err, &conflict), "an unmoved entry is not a conflict")
	})
}

// ======================================================================================
// EditTextArtifact

// TestManagerEditTextArtifact validates the edit loop: every commit is guarded on the revision
// the edits were applied to, a conflict starts over against the newer content, and a base
// revision or the attempt limit stops it.
func TestManagerEditTextArtifact(t *testing.T) {
	workspace := sampleWorkspace("unit-test-text-workspace")
	edits := []models.TextEdit{{OldText: "a: 1", NewText: "a: 9"}}

	t.Run("commits guarded on the revision the edits were applied to", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)
		mockDatabase := mockdb.NewDatabase(t)

		content := "a: 1\nb: 2\n"
		entry := sampleTextArtifact(workspace, content, 1)
		updated := atNewRevision(entry, "a: 9\nb: 2\n")

		expectEntryRead(mocks, mockDatabase, entry)
		expectObjectContent(mocks, entry.ObjectKey, content)
		staged := expectTextStaged(mocks, unitTestTextMIMEType)
		commit := expectStagedCommit(mocks, mockDatabase, staged, nil, updated)

		result, err := manager.EditTextArtifact(context.Background(), entry.ID, edits, nil)
		assert.Nil(err)
		assert.Equal(updated, result)
		assert.Equal("a: 9\nb: 2\n", staged.content)
		if assert.NotNil(commit.expectedRevision) {
			assert.Equal(entry.Revision, *commit.expectedRevision)
		}
	})

	t.Run("starts over against the newer content after a conflict", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)
		mockDatabase := mockdb.NewDatabase(t)

		// Attempt 1 applies the edit to revision 1, but another writer commits revision 2 -
		// changing a different line - before the guarded commit lands.
		first := sampleTextArtifact(workspace, "a: 1\nb: 2\n", 1)
		expectEntryRead(mocks, mockDatabase, first)
		expectObjectContent(mocks, first.ObjectKey, "a: 1\nb: 2\n")
		firstStaged := expectTextStaged(mocks, unitTestTextMIMEType)
		firstCommit := expectStagedCommit(
			mocks, mockDatabase, firstStaged, revisionConflict(first.ID), models.Artifact{},
		)

		// Attempt 2 re-reads, re-applies the edit to revision 2, and commits.
		second := atNewRevision(first, "a: 1\nb: 3\n")
		updated := atNewRevision(second, "a: 9\nb: 3\n")
		expectEntryRead(mocks, mockDatabase, second)
		expectObjectContent(mocks, second.ObjectKey, "a: 1\nb: 3\n")
		secondStaged := expectTextStaged(mocks, unitTestTextMIMEType)
		secondCommit := expectStagedCommit(mocks, mockDatabase, secondStaged, nil, updated)

		result, err := manager.EditTextArtifact(context.Background(), first.ID, edits, nil)
		assert.Nil(err)
		assert.Equal(updated, result)

		// the other writer's change survives under this one
		assert.Equal("a: 9\nb: 3\n", secondStaged.content)
		if assert.NotNil(firstCommit.expectedRevision) {
			assert.Equal(int64(1), *firstCommit.expectedRevision)
		}
		if assert.NotNil(secondCommit.expectedRevision) {
			assert.Equal(int64(2), *secondCommit.expectedRevision)
		}
	})

	t.Run("gives up after the configured number of attempts", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)
		mockDatabase := mockdb.NewDatabase(t)

		entry := sampleTextArtifact(workspace, "a: 1\n", 1)
		for attempt := 0; attempt < unitTestTextMaxAttempts; attempt++ {
			expectEntryRead(mocks, mockDatabase, entry)
			expectObjectContent(mocks, entry.ObjectKey, "a: 1\n")
			staged := expectTextStaged(mocks, unitTestTextMIMEType)
			expectStagedCommit(mocks, mockDatabase, staged, revisionConflict(entry.ID), models.Artifact{})
		}

		_, err := manager.EditTextArtifact(context.Background(), entry.ID, edits, nil)
		assertRevisionConflict(assert, err)
		assert.Contains(err.Error(), "kept changing")
	})

	t.Run("does not retry a conflict under a base revision", func(t *testing.T) {
		manager, mocks := newUnitTestManager(t)
		mockDatabase := mockdb.NewDatabase(t)

		entry := sampleTextArtifact(workspace, "a: 1\n", 3)
		expectEntryRead(mocks, mockDatabase, entry)
		expectObjectContent(mocks, entry.ObjectKey, "a: 1\n")
		staged := expectTextStaged(mocks, unitTestTextMIMEType)
		expectStagedCommit(mocks, mockDatabase, staged, revisionConflict(entry.ID), models.Artifact{})

		baseRevision := entry.Revision
		_, err := manager.EditTextArtifact(context.Background(), entry.ID, edits, &baseRevision)
		assertRevisionConflict(assert.New(t), err)
	})

	t.Run("does not retry an edit that does not match", func(t *testing.T) {
		manager, mocks := newUnitTestManager(t)
		mockDatabase := mockdb.NewDatabase(t)

		entry := sampleTextArtifact(workspace, "b: 2\n", 1)
		expectEntryRead(mocks, mockDatabase, entry)
		expectObjectContent(mocks, entry.ObjectKey, "b: 2\n")

		_, err := manager.EditTextArtifact(context.Background(), entry.ID, edits, nil)
		assertManagerErrorOfKind[goutils.BadInputError](assert.New(t), err)
	})
}

// ======================================================================================
// ReadTextArtifactContent

// TestManagerReadTextArtifactContent validates a read returns the requested lines in LF with
// the artifact's own line ending reported, refuses what is not plain text, and starts over when
// the object is replaced mid-read.
func TestManagerReadTextArtifactContent(t *testing.T) {
	workspace := sampleWorkspace("unit-test-text-workspace")
	limit := func(value int) *int { return &value }

	t.Run("returns the requested lines", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		content := "one\ntwo\nthree\n"
		entry := sampleTextArtifact(workspace, content, 2)
		expectObjectContent(mocks, entry.ObjectKey, content)

		read, err := manager.ReadTextArtifactContent(context.Background(), entry, 2, limit(1))
		assert.Nil(err)
		assert.Equal(artifact.TextArtifactContent{
			Artifact:   entry,
			Content:    "two\n",
			StartLine:  2,
			TotalLines: 3,
			LineEnding: models.TextLineEndingLF,
		}, read)
	})

	t.Run("returns CRLF content as LF and reports the line ending", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		content := "one\r\ntwo\r\n"
		entry := sampleTextArtifact(workspace, content, 1)
		expectObjectContent(mocks, entry.ObjectKey, content)

		read, err := manager.ReadTextArtifactContent(context.Background(), entry, 1, nil)
		assert.Nil(err)
		assert.Equal("one\ntwo\n", read.Content)
		assert.Equal(2, read.TotalLines)
		assert.Equal(models.TextLineEndingCRLF, read.LineEnding)
	})

	t.Run("rejects a line range that names no line", func(t *testing.T) {
		assert := assert.New(t)
		manager, _ := newUnitTestManager(t)

		entry := sampleTextArtifact(workspace, "one\n", 1)
		_, err := manager.ReadTextArtifactContent(context.Background(), entry, 0, nil)
		assertManagerErrorOfKind[goutils.ValidationError](assert, err)
		_, err = manager.ReadTextArtifactContent(context.Background(), entry, 1, limit(0))
		assertManagerErrorOfKind[goutils.ValidationError](assert, err)
	})

	t.Run("refuses a non-text artifact before fetching it", func(t *testing.T) {
		manager, _ := newUnitTestManager(t)

		entry := sampleTextArtifact(workspace, "one\n", 1)
		entry.MIMEType = "image/png"
		_, err := manager.ReadTextArtifactContent(context.Background(), entry, 1, nil)
		assertManagerErrorOfKind[goutils.BadInputError](assert.New(t), err)
	})

	t.Run("refuses a quarantined artifact before fetching it", func(t *testing.T) {
		manager, _ := newUnitTestManager(t)

		entry := sampleTextArtifact(workspace, "one\n", 1)
		entry.State = models.ArtifactStateMissingObject
		_, err := manager.ReadTextArtifactContent(context.Background(), entry, 1, nil)
		assertManagerErrorOfKind[goutils.ConsistencyError](assert.New(t), err)
	})

	t.Run("starts over when the object is replaced mid-read", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)
		mockDatabase := mockdb.NewDatabase(t)

		entry := sampleTextArtifact(workspace, "one\n", 1)
		moved := atNewRevision(entry, "one\ntwo\n")

		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, entry.ObjectKey).
			Return(goutils.S3ObjectStat{}, nil, fmt.Errorf("object reaped")).
			Once()
		// once to find the entry has moved on, once more to start over from it
		expectEntryRead(mocks, mockDatabase, moved)
		expectEntryRead(mocks, mockDatabase, moved)
		expectObjectContent(mocks, moved.ObjectKey, "one\ntwo\n")

		read, err := manager.ReadTextArtifactContent(context.Background(), entry, 1, nil)
		assert.Nil(err)
		// the content and the entry it is reported with always agree
		assert.Equal(moved, read.Artifact)
		assert.Equal("one\ntwo\n", read.Content)
	})
}
