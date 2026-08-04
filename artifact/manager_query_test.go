package artifact_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/alwitt/cairn/db"
	mockdb "github.com/alwitt/cairn/mocks/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// ======================================================================================
// Test harness

// sampleArtifact build an artifact entry of the shape persistence returns.
func sampleArtifact(workspace models.Workspace, name string) models.Artifact {
	artifactID := ulid.Make().String()
	return models.Artifact{
		ID:          artifactID,
		WorkspaceID: workspace.ID,
		Name:        name,
		ObjectKey:   fmt.Sprintf("%s/%s/%s", unitTestStorePrefix, workspace.ID, artifactID),
		MIMEType:    "text/plain; charset=utf-8",
		Size:        128,
		State:       models.ArtifactStateRecorded,
	}
}

// assertManagerPersistenceError verify an error is an ArtifactMangerError wrapping a
// PersistenceError which in turn still carries the original persistence failure. The manager
// stacks both layers, and the chain must stay walkable so callers can `errors.As` for the
// underlying goutils error kind.
func assertManagerPersistenceError(assert *assert.Assertions, err error, wrapped error) {
	assertManagerError(assert, err, wrapped)

	var persistenceErr goutils.PersistenceError
	assert.True(
		errors.As(err, &persistenceErr), "expected PersistenceError, got %T: %v", err, err,
	)
}

// ======================================================================================
// ListWorkspaceArtifacts

// TestManagerListWorkspaceArtifacts validates that listing scopes to the workspace, passes the
// caller's filter through untouched, and honors an active session.
func TestManagerListWorkspaceArtifacts(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("lists artifacts scoped to the workspace", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		expected := []models.Artifact{
			sampleArtifact(workspace, "first"), sampleArtifact(workspace, "second"),
		}

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			ListWorkspaceArtifacts(mock.Anything, workspace.ID, db.ArtifactQueryFilter{}).
			Return(expected, nil).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		entries, err := manager.ListWorkspaceArtifacts(
			context.Background(), workspace, db.ArtifactQueryFilter{}, nil,
		)

		assert.Nil(err)
		assert.Equal(expected, entries)
	})

	t.Run("passes the caller's filter through untouched", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		limit := 25
		// The state selection is a listing option, not a hardcoded filter: the manager must
		// forward whatever the caller asked for rather than defaulting it (see DESIGN §7.1).
		filters := db.ArtifactQueryFilter{
			CommonListEntryQueryFilter: db.CommonListEntryQueryFilter{Limit: &limit},
			ArtifactStates: []models.ArtifactStateENUM{
				models.ArtifactStateMissingObject,
			},
			TargetNames: []string{"triage-me"},
		}

		var forwarded db.ArtifactQueryFilter
		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			ListWorkspaceArtifacts(mock.Anything, workspace.ID, mock.Anything).
			Run(func(_ context.Context, _ string, got db.ArtifactQueryFilter) {
				forwarded = got
			}).
			Return([]models.Artifact{}, nil).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		_, err := manager.ListWorkspaceArtifacts(
			context.Background(), workspace, filters, nil,
		)

		assert.Nil(err)
		assert.Equal(filters, forwarded)
	})

	t.Run("lists within an active session", func(t *testing.T) {
		assert := assert.New(t)
		manager, _ := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		expected := []models.Artifact{sampleArtifact(workspace, "in-session")}

		// No UseDatabaseInTransaction expectation: with an active session the manager must
		// reuse the caller's transaction rather than opening its own.
		activeSession := mockdb.NewDatabase(t)
		activeSession.EXPECT().
			ListWorkspaceArtifacts(mock.Anything, workspace.ID, db.ArtifactQueryFilter{}).
			Return(expected, nil).
			Once()

		entries, err := manager.ListWorkspaceArtifacts(
			context.Background(), workspace, db.ArtifactQueryFilter{}, activeSession,
		)

		assert.Nil(err)
		assert.Equal(expected, entries)
	})

	t.Run("surfaces a persistence failure", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		dbErr := fmt.Errorf("artifact table unavailable")

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			ListWorkspaceArtifacts(mock.Anything, workspace.ID, mock.Anything).
			Return(nil, dbErr).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		entries, err := manager.ListWorkspaceArtifacts(
			context.Background(), workspace, db.ArtifactQueryFilter{}, nil,
		)

		assertManagerPersistenceError(assert, err, dbErr)
		assert.Nil(entries)
	})
}

// ======================================================================================
// GetArtifact

// TestManagerGetArtifact validates the ID-addressed fetch.
func TestManagerGetArtifact(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("fetches an artifact by ID", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		expected := sampleArtifact(sampleWorkspace("test-workspace"), "fetch-me")

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			GetArtifact(mock.Anything, expected.ID).
			Return(expected, nil).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		entry, err := manager.GetArtifact(context.Background(), expected.ID, nil)

		assert.Nil(err)
		assert.Equal(expected, entry)
	})

	t.Run("fetches within an active session", func(t *testing.T) {
		assert := assert.New(t)
		manager, _ := newUnitTestManager(t)

		expected := sampleArtifact(sampleWorkspace("test-workspace"), "in-session")

		activeSession := mockdb.NewDatabase(t)
		activeSession.EXPECT().
			GetArtifact(mock.Anything, expected.ID).
			Return(expected, nil).
			Once()

		entry, err := manager.GetArtifact(context.Background(), expected.ID, activeSession)

		assert.Nil(err)
		assert.Equal(expected, entry)
	})

	t.Run("surfaces a not found artifact", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		artifactID := ulid.Make().String()
		notFound := goutils.NewNotFoundError(
			fmt.Sprintf("artifact '%s' does not exist", artifactID), nil, true,
		)

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			GetArtifact(mock.Anything, artifactID).
			Return(models.Artifact{}, notFound).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		entry, err := manager.GetArtifact(context.Background(), artifactID, nil)

		assertManagerPersistenceError(assert, err, notFound)

		// The not-found kind must stay walkable through both wrapping layers, so the API can
		// map it onto a 404 rather than a generic failure.
		var notFoundErr goutils.NotFoundError
		assert.True(errors.As(err, &notFoundErr), "expected NotFoundError, got %T: %v", err, err)
		assert.Equal(models.Artifact{}, entry)
	})
}

// ======================================================================================
// GetArtifactByName

// TestManagerGetArtifactByName validates the name-addressed fetch that backs the MCP layer's
// name -> ID resolution.
func TestManagerGetArtifactByName(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("resolves a workspace and name to one entry", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		expected := sampleArtifact(workspace, "by-name")

		// The name is scoped by the workspace ID: artifact names are only unique within a
		// workspace, so the lookup must carry both (see DESIGN §3).
		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			GetArtifactByName(mock.Anything, workspace.ID, "by-name").
			Return(expected, nil).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		entry, err := manager.GetArtifactByName(
			context.Background(), workspace, "by-name", nil,
		)

		assert.Nil(err)
		assert.Equal(expected, entry)
	})

	t.Run("fetches within an active session", func(t *testing.T) {
		assert := assert.New(t)
		manager, _ := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		expected := sampleArtifact(workspace, "in-session")

		activeSession := mockdb.NewDatabase(t)
		activeSession.EXPECT().
			GetArtifactByName(mock.Anything, workspace.ID, "in-session").
			Return(expected, nil).
			Once()

		entry, err := manager.GetArtifactByName(
			context.Background(), workspace, "in-session", activeSession,
		)

		assert.Nil(err)
		assert.Equal(expected, entry)
	})

	t.Run("surfaces a not found artifact", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		notFound := goutils.NewNotFoundError("artifact does not exist", nil, true)

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			GetArtifactByName(mock.Anything, workspace.ID, "absent").
			Return(models.Artifact{}, notFound).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		entry, err := manager.GetArtifactByName(context.Background(), workspace, "absent", nil)

		assertManagerPersistenceError(assert, err, notFound)

		var notFoundErr goutils.NotFoundError
		assert.True(errors.As(err, &notFoundErr), "expected NotFoundError, got %T: %v", err, err)
		assert.Equal(models.Artifact{}, entry)
	})
}

// ======================================================================================
// GenerateGetURLForArtifact

// TestManagerGenerateGetURLForArtifact validates the serving path: the RECORDED gate and the
// unconditional attachment disposition that neutralizes stored XSS (see DESIGN §6.5, §7.1).
func TestManagerGenerateGetURLForArtifact(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("mints an attachment URL for a recorded artifact", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		entry := sampleArtifact(sampleWorkspace("test-workspace"), "servable")
		presigned, err := url.Parse("https://s3.unit-test/stored?signature=abc")
		assert.Nil(err)
		ttl := time.Minute * 5

		var disposition *string
		mocks.s3.EXPECT().
			GeneratePresignedGetURL(
				mock.Anything, unitTestBucket, entry.ObjectKey, ttl, mock.Anything,
			).
			Run(func(_ context.Context, _ string, _ string, _ time.Duration, got *string) {
				disposition = got
			}).
			Return(presigned, nil).
			Once()

		getURL, err := manager.GenerateGetURLForArtifact(context.Background(), entry, ttl)

		assert.Nil(err)
		assert.Equal(presigned.String(), getURL)

		// A browser honors `attachment` by downloading rather than rendering, which is what
		// neutralizes a stored-XSS payload regardless of the object's Content-Type.
		assert.NotNil(disposition)
		assert.Equal("attachment", *disposition)
	})

	t.Run("mints attachment even for a renderable MIME type", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		// The disposition never branches on what the object claims to be - a text/html
		// artifact is exactly the case the safeguard exists for.
		entry := sampleArtifact(sampleWorkspace("test-workspace"), "page-html")
		entry.MIMEType = "text/html"
		presigned, err := url.Parse("https://s3.unit-test/stored")
		assert.Nil(err)

		var disposition *string
		mocks.s3.EXPECT().
			GeneratePresignedGetURL(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Run(func(_ context.Context, _ string, _ string, _ time.Duration, got *string) {
				disposition = got
			}).
			Return(presigned, nil).
			Once()

		_, err = manager.GenerateGetURLForArtifact(context.Background(), entry, time.Minute)

		assert.Nil(err)
		assert.NotNil(disposition)
		assert.Equal("attachment", *disposition)
	})

	t.Run("refuses an artifact quarantined as missing object", func(t *testing.T) {
		assert := assert.New(t)
		manager, _ := newUnitTestManager(t)

		entry := sampleArtifact(sampleWorkspace("test-workspace"), "quarantined")
		entry.State = models.ArtifactStateMissingObject

		// No object store expectation: a quarantined artifact has no servable object, so a URL
		// must never be minted for it - it would only resolve to a not-found (see DESIGN §7.1).
		getURL, err := manager.GenerateGetURLForArtifact(
			context.Background(), entry, time.Minute,
		)

		assert.NotNil(err)

		var managerErr models.ArtifactMangerError
		assert.True(
			errors.As(err, &managerErr), "expected ArtifactMangerError, got %T: %v", err, err,
		)

		var consistencyErr goutils.ConsistencyError
		assert.True(
			errors.As(err, &consistencyErr), "expected ConsistencyError, got %T: %v", err, err,
		)
		assert.Empty(getURL)
	})

	t.Run("surfaces an object store client acquisition failure", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManagerCore(t)

		entry := sampleArtifact(sampleWorkspace("test-workspace"), "unservable")
		acquireErr := fmt.Errorf("object store client unavailable")

		// No client to mint with, so no object store expectation: the failure surfaces from
		// the acquisition itself, wrapped the same way a mint failure would be.
		mocks.s3Manager.EXPECT().
			GetClient(mock.Anything, mock.Anything).
			Return(nil, acquireErr).
			Once()

		getURL, err := manager.GenerateGetURLForArtifact(
			context.Background(), entry, time.Minute,
		)

		assertManagerError(assert, err, acquireErr)
		assert.Empty(getURL)
	})

	t.Run("surfaces a mint failure", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		entry := sampleArtifact(sampleWorkspace("test-workspace"), "unmintable")
		storeErr := fmt.Errorf("presign rejected")

		mocks.s3.EXPECT().
			GeneratePresignedGetURL(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(nil, storeErr).
			Once()

		getURL, err := manager.GenerateGetURLForArtifact(
			context.Background(), entry, time.Minute,
		)

		assertManagerError(assert, err, storeErr)
		assert.Empty(getURL)
	})
}
