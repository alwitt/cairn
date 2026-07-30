package workspace_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/alwitt/cairn/db"
	mockdb "github.com/alwitt/cairn/mocks/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/cairn/workspace"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// unitTestAppName the application name the harness manager is built with. It prefixes every
// workspace's derived volume name (see DESIGN §2.1), so tests assert it reaches persistence.
const unitTestAppName = "unit-test-app"

// runTxForManager returns a RunAndReturn body that invokes the transaction closure against the
// supplied mock Database, mirroring Client.UseDatabaseInTransaction. This is the path taken
// whenever a manager call is made with a nil activeSession.
func runTxForManager(
	mockDatabase *mockdb.Database,
) func(context.Context, func(context.Context, db.Database) error) error {
	return func(ctx context.Context, core func(context.Context, db.Database) error) error {
		return core(ctx, mockDatabase)
	}
}

// newUnitTestManager build a Manager backed by a mock persistence client, returning both so a
// test can set expectations on the client.
func newUnitTestManager(t *testing.T) (workspace.Manager, *mockdb.Client) {
	assert := assert.New(t)

	mockClient := mockdb.NewClient(t)
	manager, err := workspace.NewManager(unitTestAppName, mockClient)
	assert.Nil(err)
	assert.NotNil(manager)

	return manager, mockClient
}

// sampleWorkspace build a workspace entry of the shape persistence returns, with the volume name
// derived the way DefineNewWorkspace derives it (see DESIGN §2.1).
func sampleWorkspace(name string) models.Workspace {
	workspaceID := uuid.NewString()
	return models.Workspace{
		ID:          workspaceID,
		Name:        name,
		VolumeName:  fmt.Sprintf("%s-%s", unitTestAppName, workspaceID),
		VolumeState: models.WorkspaceVolumeStateNone,
	}
}

// assertManagerError verify an error is a WorkspaceMangerError wrapping a PersistenceError which
// in turn still carries the original persistence error. The manager stacks both layers, and the
// chain must stay walkable so callers can `errors.As` for the underlying goutils error kind.
//
// The innermost error is matched on its rendered message rather than with `errors.Is`: the
// goutils constructors take the core error by value, so the chain carries a copy of it and
// identity comparison would never match.
func assertManagerError(assert *assert.Assertions, err error, wrapped error) {
	assert.NotNil(err)

	var managerErr models.WorkspaceMangerError
	assert.True(
		errors.As(err, &managerErr), "expected WorkspaceMangerError, got %T: %v", err, err,
	)

	var persistenceErr goutils.PersistenceError
	assert.True(
		errors.As(err, &persistenceErr), "expected PersistenceError, got %T: %v", err, err,
	)

	assert.Contains(
		err.Error(), wrapped.Error(), "persistence error should survive to the top of the chain",
	)
}

// TestNewManager validates the constructor's input guards.
func TestNewManager(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)

		manager, err := workspace.NewManager(unitTestAppName, mockdb.NewClient(t))
		assert.Nil(err)
		assert.NotNil(manager)
	})

	// The application name becomes the prefix of every workspace's volume name, so it is held
	// to the same charset a volume name must satisfy - rejected at construction rather than on
	// the first workspace define.
	t.Run("invalid application name rejected", func(t *testing.T) {
		assert := assert.New(t)

		for _, appName := range []string{"", "has space", "has/slash", "has.dot"} {
			manager, err := workspace.NewManager(appName, mockdb.NewClient(t))
			assert.Nil(manager, "application name '%s' should be rejected", appName)
			assert.NotNil(err, "application name '%s' should be rejected", appName)

			var validationErr goutils.ValidationError
			assert.True(
				errors.As(err, &validationErr), "expected ValidationError, got %T: %v", err, err,
			)
		}
	})

	t.Run("nil persistence client rejected", func(t *testing.T) {
		assert := assert.New(t)

		manager, err := workspace.NewManager(unitTestAppName, nil)
		assert.Nil(manager)
		assert.NotNil(err)
	})
}

// TestManagerActiveSession validates the activeSession branch shared by every Manager API: a
// caller supplying an already-open transaction has its work performed within that transaction,
// and the manager must NOT open a second one. Both directions are asserted here once, rather
// than repeated across every API's test.
func TestManagerActiveSession(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	// Case 1: with an activeSession, the persistence client is never asked to open a
	// transaction - the manager runs against the session it was handed. The mock client has no
	// expectations set, so any call to it fails the test.
	t.Run("existing session is used directly", func(t *testing.T) {
		assert := assert.New(t)

		manager, _ := newUnitTestManager(t)
		entry := sampleWorkspace("unit-test-workspace")

		activeSession := mockdb.NewDatabase(t)
		activeSession.EXPECT().GetWorkspace(mock.Anything, entry.ID).Return(entry, nil)

		got, _, err := manager.GetWorkspace(utCtx, entry.ID, activeSession)
		assert.Nil(err)
		assert.Equal(entry.ID, got.ID)
	})

	// Case 2: without one, the manager opens its own transaction through the persistence client.
	t.Run("nil session opens a transaction", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		entry := sampleWorkspace("unit-test-workspace")

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().GetWorkspace(mock.Anything, entry.ID).Return(entry, nil)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		got, _, err := manager.GetWorkspace(utCtx, entry.ID, nil)
		assert.Nil(err)
		assert.Equal(entry.ID, got.ID)
	})

	// Case 3: a multi-statement API performs ALL of its persistence work in the one transaction
	// it was handed. UpdateWorkspaceName both writes and reads back, and neither may escape the
	// caller's session - a read-back outside it could observe another writer's change.
	t.Run("multi-statement work stays in the one session", func(t *testing.T) {
		assert := assert.New(t)

		manager, _ := newUnitTestManager(t)
		entry := sampleWorkspace("unit-test-renamed")
		newName := "unit-test-renamed"

		activeSession := mockdb.NewDatabase(t)
		activeSession.EXPECT().UpdateWorkspaceName(mock.Anything, entry.ID, newName).Return(nil)
		activeSession.EXPECT().GetWorkspace(mock.Anything, entry.ID).Return(entry, nil)

		got, err := manager.UpdateWorkspaceName(utCtx, entry.ID, newName, activeSession)
		assert.Nil(err)
		assert.Equal(newName, got.Name)
	})
}

// TestManagerDefineNewWorkspace validates workspace creation.
func TestManagerDefineNewWorkspace(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	// Case 1: the manager supplies its configured application name to persistence, which is
	// what makes the derived volume name deployment-namespaced (see DESIGN §2.1). The caller
	// never provides it.
	t.Run("application name is supplied by the manager", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		name := "unit-test-workspace"
		description := "unit test description"
		created := sampleWorkspace(name)

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			DefineNewWorkspace(mock.Anything, db.NewWorkspaceParameter{
				Name: name, Description: &description, AppName: unitTestAppName,
			}).
			Return(created, nil)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		got, err := manager.DefineNewWorkspace(utCtx, name, &description, nil)
		assert.Nil(err)
		assert.Equal(created.ID, got.ID)
		assert.Equal(name, got.Name)
		// The new workspace starts with no volume; the operator provisions it (DESIGN §4.2).
		assert.Equal(models.WorkspaceVolumeStateNone, got.VolumeState)
	})

	// Case 2: the optional description passes through untouched when absent.
	t.Run("description is optional", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		name := "unit-test-workspace"

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			DefineNewWorkspace(mock.Anything, db.NewWorkspaceParameter{
				Name: name, Description: nil, AppName: unitTestAppName,
			}).
			Return(sampleWorkspace(name), nil)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		_, err := manager.DefineNewWorkspace(utCtx, name, nil, nil)
		assert.Nil(err)
	})

	// Case 3: a persistence failure surfaces wrapped, with the original error still reachable.
	t.Run("persistence failure is wrapped", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		simErr := fmt.Errorf("simulated DB failure")

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			DefineNewWorkspace(mock.Anything, mock.Anything).
			Return(models.Workspace{}, simErr)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		got, err := manager.DefineNewWorkspace(utCtx, "unit-test-workspace", nil, nil)
		assertManagerError(assert, err, simErr)
		assert.Empty(got.ID)
	})
}

// TestManagerGetWorkspace validates fetching one workspace by ID.
func TestManagerGetWorkspace(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	t.Run("fetch by ID", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		entry := sampleWorkspace("unit-test-workspace")

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().GetWorkspace(mock.Anything, entry.ID).Return(entry, nil)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		got, mountCount, err := manager.GetWorkspace(utCtx, entry.ID, nil)
		assert.Nil(err)
		assert.Equal(entry.ID, got.ID)
		assert.Equal(entry.VolumeName, got.VolumeName)
		// Only Docker can answer the mount count, and the manager holds no VolumeManager yet,
		// so it reports the unavailable sentinel rather than a count it can't substantiate
		// (see DESIGN §4.3).
		assert.Equal(-1, mountCount)
	})

	// A workspace which does not exist surfaces as a NotFoundError still reachable through both
	// wrapping layers, so a caller can distinguish it from a genuine DB failure.
	t.Run("unknown workspace", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		workspaceID := uuid.NewString()
		notFound := goutils.NewNotFoundError(
			fmt.Sprintf("workspace '%s' does not exist", workspaceID), nil, true,
		)

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			GetWorkspace(mock.Anything, workspaceID).
			Return(models.Workspace{}, notFound)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		got, mountCount, err := manager.GetWorkspace(utCtx, workspaceID, nil)
		assert.NotNil(err)
		assert.Empty(got.ID)
		assert.Equal(-1, mountCount)

		var notFoundErr goutils.NotFoundError
		assert.True(
			errors.As(err, &notFoundErr), "expected NotFoundError, got %T: %v", err, err,
		)
	})
}

// TestManagerGetWorkspaceByName validates name -> entry resolution, which backs the MCP layer's
// name -> ID resolution (see DESIGN §3).
func TestManagerGetWorkspaceByName(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	t.Run("fetch by name", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		entry := sampleWorkspace("unit-test-workspace")

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().GetWorkspaceByName(mock.Anything, entry.Name).Return(entry, nil)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		got, mountCount, err := manager.GetWorkspaceByName(utCtx, entry.Name, nil)
		assert.Nil(err)
		assert.Equal(entry.ID, got.ID)
		assert.Equal(entry.Name, got.Name)
		assert.Equal(-1, mountCount)
	})

	t.Run("unknown workspace name", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		name := "unit-test-does-not-exist"
		notFound := goutils.NewNotFoundError(
			fmt.Sprintf("workspace '%s' does not exist", name), nil, true,
		)

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			GetWorkspaceByName(mock.Anything, name).
			Return(models.Workspace{}, notFound)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		got, mountCount, err := manager.GetWorkspaceByName(utCtx, name, nil)
		assert.NotNil(err)
		assert.Empty(got.ID)
		assert.Equal(-1, mountCount)

		var notFoundErr goutils.NotFoundError
		assert.True(
			errors.As(err, &notFoundErr), "expected NotFoundError, got %T: %v", err, err,
		)
	})
}

// TestManagerListWorkspaces validates workspace listing.
func TestManagerListWorkspaces(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	// Case 1: the filter reaches persistence unaltered - the manager adds no opinion of its own
	// to what the caller asked for.
	t.Run("filters pass through unaltered", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		one := sampleWorkspace("unit-test-one")
		two := sampleWorkspace("unit-test-two")
		limit := 10
		filters := db.WorkspaceQueryFilter{
			CommonListEntryQueryFilter: db.CommonListEntryQueryFilter{Limit: &limit},
			TargetNames:                []string{one.Name, two.Name},
			VolumeStates:               []models.WorkspaceVolumeStateENUM{models.WorkspaceVolumeStateNone},
		}

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			ListWorkspaces(mock.Anything, filters).
			Return([]models.Workspace{one, two}, nil)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		got, err := manager.ListWorkspaces(utCtx, filters, nil)
		assert.Nil(err)
		assert.Len(got, 2)
		assert.Equal(one.ID, got[0].ID)
		assert.Equal(two.ID, got[1].ID)
	})

	t.Run("empty listing", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			ListWorkspaces(mock.Anything, db.WorkspaceQueryFilter{}).
			Return([]models.Workspace{}, nil)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		got, err := manager.ListWorkspaces(utCtx, db.WorkspaceQueryFilter{}, nil)
		assert.Nil(err)
		assert.Empty(got)
	})

	t.Run("persistence failure is wrapped", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		simErr := fmt.Errorf("simulated DB failure")

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().ListWorkspaces(mock.Anything, mock.Anything).Return(nil, simErr)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		got, err := manager.ListWorkspaces(utCtx, db.WorkspaceQueryFilter{}, nil)
		assertManagerError(assert, err, simErr)
		assert.Nil(got)
	})
}

// TestManagerUpdateWorkspaceName validates renaming, and the read-back which turns the
// persistence layer's error-only update into the updated entry the interface returns.
func TestManagerUpdateWorkspaceName(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	// Case 1: the manager returns the re-read entry, so the caller observes the new name
	// without a second round trip of its own.
	t.Run("returns the updated entry", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		entry := sampleWorkspace("unit-test-original")
		newName := "unit-test-renamed"

		renamed := entry
		renamed.Name = newName

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().UpdateWorkspaceName(mock.Anything, entry.ID, newName).Return(nil)
		mockDatabase.EXPECT().GetWorkspace(mock.Anything, entry.ID).Return(renamed, nil)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		got, err := manager.UpdateWorkspaceName(utCtx, entry.ID, newName, nil)
		assert.Nil(err)
		assert.Equal(newName, got.Name)
		// The volume name is derived from the immutable ID, so a rename never disturbs it
		// (see DESIGN §2.1, §7.1).
		assert.Equal(entry.VolumeName, got.VolumeName)
	})

	// Case 2: when the update itself fails there is nothing to read back, so the manager must
	// not attempt it. The mock Database has no GetWorkspace expectation, so a read-back here
	// fails the test.
	t.Run("no read-back when the update fails", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		workspaceID := uuid.NewString()
		collision := goutils.NewSQLError("name already taken", nil, true)

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateWorkspaceName(mock.Anything, workspaceID, "unit-test-taken").
			Return(collision)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		got, err := manager.UpdateWorkspaceName(utCtx, workspaceID, "unit-test-taken", nil)
		assertManagerError(assert, err, collision)
		assert.Empty(got.ID)
	})

	// Case 3: a read-back failure is surfaced too, rather than returning a zero entry with a
	// nil error - the caller would otherwise read the rename as having produced nothing.
	t.Run("read-back failure is surfaced", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		workspaceID := uuid.NewString()
		simErr := fmt.Errorf("simulated read-back failure")

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateWorkspaceName(mock.Anything, workspaceID, "unit-test-renamed").
			Return(nil)
		mockDatabase.EXPECT().
			GetWorkspace(mock.Anything, workspaceID).
			Return(models.Workspace{}, simErr)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		got, err := manager.UpdateWorkspaceName(utCtx, workspaceID, "unit-test-renamed", nil)
		assertManagerError(assert, err, simErr)
		assert.Empty(got.ID)
	})
}

// TestManagerUpdateWorkspaceDescription validates description changes, including clearing.
func TestManagerUpdateWorkspaceDescription(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	t.Run("returns the updated entry", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		entry := sampleWorkspace("unit-test-workspace")
		newDescription := "unit test description"

		updated := entry
		updated.Description = &newDescription

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateWorkspaceDescription(mock.Anything, entry.ID, &newDescription).
			Return(nil)
		mockDatabase.EXPECT().GetWorkspace(mock.Anything, entry.ID).Return(updated, nil)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		got, err := manager.UpdateWorkspaceDescription(utCtx, entry.ID, &newDescription, nil)
		assert.Nil(err)
		assert.NotNil(got.Description)
		assert.Equal(newDescription, *got.Description)
	})

	// Case 2: a nil description is a clear instruction, not an absent argument - it must reach
	// persistence as nil rather than being skipped.
	t.Run("nil description clears it", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		entry := sampleWorkspace("unit-test-workspace")

		cleared := entry
		cleared.Description = nil

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateWorkspaceDescription(mock.Anything, entry.ID, (*string)(nil)).
			Return(nil)
		mockDatabase.EXPECT().GetWorkspace(mock.Anything, entry.ID).Return(cleared, nil)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		got, err := manager.UpdateWorkspaceDescription(utCtx, entry.ID, nil, nil)
		assert.Nil(err)
		assert.Nil(got.Description)
	})

	// Case 3: as with rename, a failed update means no read-back.
	t.Run("no read-back when the update fails", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		workspaceID := uuid.NewString()
		notFound := goutils.NewNotFoundError(
			fmt.Sprintf("workspace '%s' does not exist", workspaceID), nil, true,
		)

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateWorkspaceDescription(mock.Anything, workspaceID, mock.Anything).
			Return(notFound)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		got, err := manager.UpdateWorkspaceDescription(utCtx, workspaceID, nil, nil)
		assertManagerError(assert, err, notFound)
		assert.Empty(got.ID)

		var notFoundErr goutils.NotFoundError
		assert.True(
			errors.As(err, &notFoundErr), "expected NotFoundError, got %T: %v", err, err,
		)
	})
}

// TestManagerDeleteWorkspace validates deletion, including that the volume guard is left to
// persistence rather than duplicated in the manager.
func TestManagerDeleteWorkspace(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	// Case 1: the delete is a pure pass-through - the manager reads nothing first, it simply
	// hands the ID to persistence.
	t.Run("deletes through to persistence", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		workspaceID := uuid.NewString()

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().DeleteWorkspace(mock.Anything, workspaceID).Return(nil)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		assert.Nil(manager.DeleteWorkspace(utCtx, workspaceID, nil))
	})

	// Case 2: the "volume must be gone first" guard lives in persistence, since VolumeState is
	// the workspace's own column (see DESIGN §4.3). The manager must surface that refusal with
	// the ConsistencyError intact - the operator needs to know to delete the volume first, not
	// merely that the delete failed.
	t.Run("volume guard refusal is surfaced", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		workspaceID := uuid.NewString()
		refusal := goutils.NewConsistencyError(
			fmt.Sprintf("workspace %s still has a persistent volume", workspaceID), nil, true,
		)

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().DeleteWorkspace(mock.Anything, workspaceID).Return(refusal)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		err := manager.DeleteWorkspace(utCtx, workspaceID, nil)
		assertManagerError(assert, err, refusal)

		var consistencyErr goutils.ConsistencyError
		assert.True(
			errors.As(err, &consistencyErr), "expected ConsistencyError, got %T: %v", err, err,
		)
	})

	t.Run("unknown workspace", func(t *testing.T) {
		assert := assert.New(t)

		manager, mockClient := newUnitTestManager(t)
		workspaceID := uuid.NewString()
		notFound := goutils.NewNotFoundError(
			fmt.Sprintf("workspace '%s' does not exist", workspaceID), nil, true,
		)

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().DeleteWorkspace(mock.Anything, workspaceID).Return(notFound)
		mockClient.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase))

		err := manager.DeleteWorkspace(utCtx, workspaceID, nil)
		assertManagerError(assert, err, notFound)

		var notFoundErr goutils.NotFoundError
		assert.True(
			errors.As(err, &notFoundErr), "expected NotFoundError, got %T: %v", err, err,
		)
	})
}
