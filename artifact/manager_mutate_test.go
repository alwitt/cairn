package artifact_test

import (
	"context"
	"fmt"
	"testing"

	mockdb "github.com/alwitt/cairn/mocks/db"
	"github.com/alwitt/cairn/models"
	"github.com/apex/log"
	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// ======================================================================================
// RenameArtifact

// TestManagerRenameArtifact validates renaming, the read-back which turns the write into the
// returned entry, and that a rename never touches the object store.
func TestManagerRenameArtifact(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("renames and returns the updated entry", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "old-name")
		newName := "new-name"

		renamed := entry
		renamed.Name = newName

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().UpdateArtifactName(mock.Anything, entry.ID, newName).Return(nil).Once()
		mockDatabase.EXPECT().GetArtifact(mock.Anything, entry.ID).Return(renamed, nil).Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		// No object store expectations at all: the object key carries a random suffix rather
		// than the name, so a rename is a pure DB update (see DESIGN §2.2). Any call onto the
		// S3 mock would fail this case.
		got, err := manager.RenameArtifact(context.Background(), entry.ID, newName, nil)

		assert.Nil(err)
		// The returned entry is the read-back, so the caller sees the new name rather than
		// having to re-fetch.
		assert.Equal(renamed, got)
		assert.Equal(newName, got.Name)
		// The rename leaves the backing object exactly where it was.
		assert.Equal(entry.ObjectKey, got.ObjectKey)
	})

	t.Run("renames within an active session", func(t *testing.T) {
		assert := assert.New(t)
		manager, _ := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "old-name")
		newName := "in-session"

		renamed := entry
		renamed.Name = newName

		activeSession := mockdb.NewDatabase(t)
		activeSession.EXPECT().
			UpdateArtifactName(mock.Anything, entry.ID, newName).
			Return(nil).
			Once()
		activeSession.EXPECT().GetArtifact(mock.Anything, entry.ID).Return(renamed, nil).Once()

		// No UseDatabaseInTransaction expectation: with an active session the manager must
		// reuse the caller's transaction, so the write and its read-back stay inside the
		// caller's unit of work.
		got, err := manager.RenameArtifact(
			context.Background(), entry.ID, newName, activeSession,
		)

		assert.Nil(err)
		assert.Equal(renamed, got)
	})

	t.Run("surfaces a name collision", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		artifactID := ulid.Make().String()
		dbErr := fmt.Errorf("artifact name already taken")

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateArtifactName(mock.Anything, artifactID, "taken").
			Return(dbErr).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		// No GetArtifact expectation: the read-back is reached only after a successful write.
		// Uniqueness within the workspace is the DB constraint's job, and it surfaces here as
		// a persistence failure rather than being pre-checked by the manager.
		got, err := manager.RenameArtifact(context.Background(), artifactID, "taken", nil)

		assertManagerPersistenceError(assert, err, dbErr)
		assert.Equal(models.Artifact{}, got)
	})

	t.Run("surfaces a read back failure", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		artifactID := ulid.Make().String()
		dbErr := fmt.Errorf("artifact vanished")

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateArtifactName(mock.Anything, artifactID, "renamed").
			Return(nil).
			Once()
		mockDatabase.EXPECT().
			GetArtifact(mock.Anything, artifactID).
			Return(models.Artifact{}, dbErr).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		// The read-back is inside the transaction, so its failure fails the whole call rather
		// than returning a half-known entry.
		got, err := manager.RenameArtifact(context.Background(), artifactID, "renamed", nil)

		assertManagerPersistenceError(assert, err, dbErr)
		assert.Equal(models.Artifact{}, got)
	})
}

// ======================================================================================
// UpdateArtifactDescription

// TestManagerUpdateArtifactDescription validates description changes, including clearing, and
// the read-back which turns the write into the returned entry.
func TestManagerUpdateArtifactDescription(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("updates and returns the updated entry", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "described")
		newDescription := "a freshly described artifact"

		updated := entry
		updated.Description = &newDescription

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateArtifactDescription(mock.Anything, entry.ID, &newDescription).
			Return(nil).
			Once()
		mockDatabase.EXPECT().GetArtifact(mock.Anything, entry.ID).Return(updated, nil).Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		// No object store expectations: a description change is metadata only.
		got, err := manager.UpdateArtifactDescription(
			context.Background(), entry.ID, &newDescription, nil,
		)

		assert.Nil(err)
		assert.Equal(updated, got)
		assert.Equal(&newDescription, got.Description)
	})

	t.Run("clears the description when passed nil", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		existing := "a description about to be cleared"
		entry := sampleArtifact(workspace, "described")
		entry.Description = &existing

		cleared := entry
		cleared.Description = nil

		// A nil description is passed straight through rather than being treated as "no
		// change" - clearing is a legitimate update.
		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateArtifactDescription(mock.Anything, entry.ID, (*string)(nil)).
			Return(nil).
			Once()
		mockDatabase.EXPECT().GetArtifact(mock.Anything, entry.ID).Return(cleared, nil).Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		got, err := manager.UpdateArtifactDescription(context.Background(), entry.ID, nil, nil)

		assert.Nil(err)
		assert.Nil(got.Description)
	})

	t.Run("updates within an active session", func(t *testing.T) {
		assert := assert.New(t)
		manager, _ := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "described")
		newDescription := "in session"

		updated := entry
		updated.Description = &newDescription

		activeSession := mockdb.NewDatabase(t)
		activeSession.EXPECT().
			UpdateArtifactDescription(mock.Anything, entry.ID, &newDescription).
			Return(nil).
			Once()
		activeSession.EXPECT().GetArtifact(mock.Anything, entry.ID).Return(updated, nil).Once()

		// No UseDatabaseInTransaction expectation: the caller's transaction must be reused.
		got, err := manager.UpdateArtifactDescription(
			context.Background(), entry.ID, &newDescription, activeSession,
		)

		assert.Nil(err)
		assert.Equal(updated, got)
	})

	t.Run("surfaces an update failure", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		artifactID := ulid.Make().String()
		dbErr := fmt.Errorf("no such artifact")

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateArtifactDescription(mock.Anything, artifactID, mock.Anything).
			Return(dbErr).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		// No GetArtifact expectation: the read-back is reached only after a successful write.
		got, err := manager.UpdateArtifactDescription(context.Background(), artifactID, nil, nil)

		assertManagerPersistenceError(assert, err, dbErr)
		assert.Equal(models.Artifact{}, got)
	})

	t.Run("surfaces a read back failure", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		artifactID := ulid.Make().String()
		dbErr := fmt.Errorf("artifact vanished")
		newDescription := "a description"

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateArtifactDescription(mock.Anything, artifactID, &newDescription).
			Return(nil).
			Once()
		mockDatabase.EXPECT().
			GetArtifact(mock.Anything, artifactID).
			Return(models.Artifact{}, dbErr).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		got, err := manager.UpdateArtifactDescription(
			context.Background(), artifactID, &newDescription, nil,
		)

		assertManagerPersistenceError(assert, err, dbErr)
		assert.Equal(models.Artifact{}, got)
	})
}

// ======================================================================================
// DeleteArtifact

// TestManagerDeleteArtifact validates deletion, and that the backing object is deliberately
// left for the object-reaping GC rather than removed here.
func TestManagerDeleteArtifact(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("deletes the entry without touching the object store", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "doomed")

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().DeleteArtifact(mock.Anything, entry.ID).Return(nil).Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		// No object store expectations. The object the entry referenced is left in the store
		// and reclaimed later by the object-reaping GC; deleting it here would place a second
		// reclaimer alongside the GC (see DESIGN §4.1, §8.2.1). A DeleteObject call would fail
		// this case.
		assert.Nil(manager.DeleteArtifact(context.Background(), entry.ID, nil))

		mocks.s3.AssertNotCalled(
			t, "DeleteObject", mock.Anything, unitTestBucket, entry.ObjectKey,
		)
	})

	t.Run("deletes a MISSING_OBJECT entry", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "quarantined")
		entry.State = models.ArtifactStateMissingObject

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().DeleteArtifact(mock.Anything, entry.ID).Return(nil).Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		// Deleting the row is exactly how an operator clears a quarantined artifact once the
		// data loss has been triaged, so the manager must not gate on state (see DESIGN §7.1).
		assert.Nil(manager.DeleteArtifact(context.Background(), entry.ID, nil))
	})

	t.Run("deletes within an active session", func(t *testing.T) {
		assert := assert.New(t)
		manager, _ := newUnitTestManager(t)

		artifactID := ulid.Make().String()

		activeSession := mockdb.NewDatabase(t)
		activeSession.EXPECT().DeleteArtifact(mock.Anything, artifactID).Return(nil).Once()

		// No UseDatabaseInTransaction expectation: the caller's transaction must be reused, so
		// a rollback there takes this delete with it.
		assert.Nil(manager.DeleteArtifact(context.Background(), artifactID, activeSession))
	})

	t.Run("treats an absent entry as a no-op", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		artifactID := ulid.Make().String()

		// Idempotence is the persistence layer's: it reports success for an entry that is
		// already gone, and the manager passes that through rather than manufacturing an error.
		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().DeleteArtifact(mock.Anything, artifactID).Return(nil).Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		assert.Nil(manager.DeleteArtifact(context.Background(), artifactID, nil))
	})

	t.Run("surfaces a delete failure", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		artifactID := ulid.Make().String()
		dbErr := fmt.Errorf("delete rejected")

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().DeleteArtifact(mock.Anything, artifactID).Return(dbErr).Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		err := manager.DeleteArtifact(context.Background(), artifactID, nil)

		assertManagerPersistenceError(assert, err, dbErr)
	})
}
