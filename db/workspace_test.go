package db_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// getUnitTestPersistence spin up a fresh Sqlite-backed persistence client with the
// schema migrated, for use in a single unit test.
func getUnitTestPersistence(ctx context.Context, t *testing.T, dbFile string) db.Client {
	assert := assert.New(t)

	persistence, err := db.NewConnection(db.GetSqliteDialector(dbFile), logger.Info)
	assert.Nil(err)

	// migrate the schema
	assert.Nil(persistence.RunSQLInTransaction(
		ctx, func(ctx context.Context, tx *gorm.DB) error {
			return db.DefineTables(ctx, tx)
		},
	))

	return persistence
}

// getUnitTestValidator define a validator with the application's custom macros installed, for
// exercising audit metadata `ParseMetadata` round-trips.
func getUnitTestValidator(t *testing.T) *validator.Validate {
	assert := assert.New(t)

	validate := validator.New()
	assert.Nil(models.RegisterWithValidator(validate))

	return validate
}

func TestWorkspaceDefineNewWorkspace(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtx := context.Background()
	testDB := fmt.Sprintf("/tmp/cairn_ut_%s.db", ulid.Make().String())
	log.WithField("db", testDB).Debug("Test database")

	persistence := getUnitTestPersistence(utCtx, t, testDB)
	validate := getUnitTestValidator(t)

	appName := fmt.Sprintf("ut-app-%s", ulid.Make().String())

	// Case 0: define a workspace without a description
	var workspace0 models.Workspace
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			var err error
			workspace0, err = dbClient.DefineNewWorkspace(ctx, db.NewWorkspaceParameter{
				Name: "unit-test-workspace", AppName: appName,
			})
			return err
		},
	))
	assert.NotEmpty(workspace0.ID)
	assert.Equal("unit-test-workspace", workspace0.Name)
	assert.Nil(workspace0.Description)
	// the volume name is derived from the app name and the immutable workspace ID (DESIGN §2.1)
	assert.Equal(fmt.Sprintf("%s-%s", appName, workspace0.ID), workspace0.VolumeName)
	// a new workspace starts with no persistent volume (DESIGN §4.2)
	assert.Equal(models.WorkspaceVolumeStateNone, workspace0.VolumeState)

	// Case 1: the entry persisted and reads back identically
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			readBack, err := dbClient.GetWorkspace(ctx, workspace0.ID)
			if err != nil {
				return err
			}
			assert.Equal(workspace0.ID, readBack.ID)
			assert.Equal(workspace0.Name, readBack.Name)
			assert.Equal(workspace0.VolumeName, readBack.VolumeName)
			assert.Equal(models.WorkspaceVolumeStateNone, readBack.VolumeState)
			assert.Nil(readBack.Description)
			return nil
		},
	))

	// Case 2: the NEW_WORKSPACE audit event was recorded with the derived volume name
	{
		var events []models.SystemEventAudit
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				events, err = dbClient.ListSystemEvents(ctx, db.SystemEventQueryFilter{
					EventTypes: []models.SystemEventTypeENUM{models.SystemEventTypeNewWorkspace},
				})
				return err
			},
		))
		assert.Len(events, 1)
		parsed, err := events[0].ParseMetadata(validate)
		assert.Nil(err)
		meta, ok := parsed.(models.SystemEventNewWorkspace)
		assert.True(ok, "expected SystemEventNewWorkspace, got %T", parsed)
		assert.Equal(workspace0.ID, meta.WorkspaceID)
		assert.Equal(workspace0.Name, meta.WorkspaceName)
		assert.Equal(workspace0.VolumeName, meta.VolumeName)
	}

	// Case 3: define a workspace with a description
	description := "unit test description"
	var workspace3 models.Workspace
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			var err error
			workspace3, err = dbClient.DefineNewWorkspace(ctx, db.NewWorkspaceParameter{
				Name: "unit-test-workspace-described", Description: &description, AppName: appName,
			})
			return err
		},
	))
	assert.NotNil(workspace3.Description)
	assert.Equal(description, *workspace3.Description)
	// each workspace gets its own volume name, even under the same app name
	assert.NotEqual(workspace0.VolumeName, workspace3.VolumeName)

	// Case 4: workspace names are unique across the deployment
	assert.NotNil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			_, err := dbClient.DefineNewWorkspace(ctx, db.NewWorkspaceParameter{
				Name: "unit-test-workspace", AppName: appName,
			})
			return err
		},
	))

	// Case 5: the name must satisfy the `valid_name` macro
	for _, badName := range []string{"", "has space", "has/slash", "has.dot"} {
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				_, err := dbClient.DefineNewWorkspace(ctx, db.NewWorkspaceParameter{
					Name: badName, AppName: appName,
				})
				return err
			},
		)
		assert.NotNil(err, "name '%s' should be rejected", badName)
		var validationError goutils.ValidationError
		assert.True(errors.As(err, &validationError), "expected ValidationError, got %T", err)
	}
}

func TestWorkspaceGetWorkspace(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtx := context.Background()
	testDB := fmt.Sprintf("/tmp/cairn_ut_%s.db", ulid.Make().String())
	log.WithField("db", testDB).Debug("Test database")

	persistence := getUnitTestPersistence(utCtx, t, testDB)

	appName := fmt.Sprintf("ut-app-%s", ulid.Make().String())

	var workspace0 models.Workspace
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			var err error
			workspace0, err = dbClient.DefineNewWorkspace(ctx, db.NewWorkspaceParameter{
				Name: "unit-test-get", AppName: appName,
			})
			return err
		},
	))

	// Case 0: fetch by ID
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			readBack, err := dbClient.GetWorkspace(ctx, workspace0.ID)
			if err != nil {
				return err
			}
			assert.Equal(workspace0.ID, readBack.ID)
			assert.Equal(workspace0.Name, readBack.Name)
			return nil
		},
	))

	// Case 1: fetch by name - backs the MCP layer's name -> ID resolution (DESIGN §3)
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			readBack, err := dbClient.GetWorkspaceByName(ctx, workspace0.Name)
			if err != nil {
				return err
			}
			assert.Equal(workspace0.ID, readBack.ID)
			assert.Equal(workspace0.VolumeName, readBack.VolumeName)
			return nil
		},
	))

	// Case 2: an unknown ID surfaces a NotFoundError, not a SQLError
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				_, err := dbClient.GetWorkspace(ctx, uuid.NewString())
				return err
			},
		)
		assert.NotNil(err)
		var notFound goutils.NotFoundError
		assert.True(errors.As(err, &notFound), "expected NotFoundError, got %T", err)
	}

	// Case 3: an unknown name surfaces a NotFoundError as well
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				_, err := dbClient.GetWorkspaceByName(ctx, "unit-test-does-not-exist")
				return err
			},
		)
		assert.NotNil(err)
		var notFound goutils.NotFoundError
		assert.True(errors.As(err, &notFound), "expected NotFoundError, got %T", err)
	}

	// Case 4: reading a workspace records no audit event
	{
		var events []models.SystemEventAudit
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				events, err = dbClient.ListSystemEvents(ctx, db.SystemEventQueryFilter{})
				return err
			},
		))
		// only the one NEW_WORKSPACE from the setup
		assert.Len(events, 1)
		assert.Equal(models.SystemEventTypeNewWorkspace, events[0].EventType)
	}
}

func TestWorkspaceVolumeStateChange(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtx := context.Background()
	testDB := fmt.Sprintf("/tmp/cairn_ut_%s.db", ulid.Make().String())
	log.WithField("db", testDB).Debug("Test database")

	persistence := getUnitTestPersistence(utCtx, t, testDB)
	validate := getUnitTestValidator(t)

	appName := fmt.Sprintf("ut-app-%s", ulid.Make().String())

	var workspace0 models.Workspace
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			var err error
			workspace0, err = dbClient.DefineNewWorkspace(ctx, db.NewWorkspaceParameter{
				Name: "unit-test-volume-state", AppName: appName,
			})
			return err
		},
	))

	// helper: read the workspace's current volume state
	readState := func() models.WorkspaceVolumeStateENUM {
		var workspace models.Workspace
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				workspace, err = dbClient.GetWorkspace(ctx, workspace0.ID)
				return err
			},
		))
		return workspace.VolumeState
	}

	// helper: list the recorded volume state change events
	volumeStateEvents := func() []models.SystemEventAudit {
		var events []models.SystemEventAudit
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				events, err = dbClient.ListSystemEvents(ctx, db.SystemEventQueryFilter{
					EventTypes: []models.SystemEventTypeENUM{
						models.SystemEventTypeWorkspaceVolumeState,
					},
				})
				return err
			},
		))
		return events
	}

	assert.Equal(models.WorkspaceVolumeStateNone, readState())
	assert.Empty(volumeStateEvents())

	// Case 0: NONE -> READY, recorded once Docker has actually created the volume (DESIGN §4.2)
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.MarkWorkspaceVolumeReady(ctx, workspace0.ID)
		},
	))
	assert.Equal(models.WorkspaceVolumeStateReady, readState())

	// the transition recorded a WORKSPACE_VOLUME_STATE event carrying the new state
	{
		events := volumeStateEvents()
		assert.Len(events, 1)
		parsed, err := events[0].ParseMetadata(validate)
		assert.Nil(err)
		meta, ok := parsed.(models.SystemEventWorkspaceVolumeState)
		assert.True(ok, "expected SystemEventWorkspaceVolumeState, got %T", parsed)
		assert.Equal(workspace0.ID, meta.WorkspaceID)
		assert.Equal(workspace0.Name, meta.WorkspaceName)
		assert.Equal(workspace0.VolumeName, meta.VolumeName)
		assert.Equal(models.WorkspaceVolumeStateReady, meta.NewState)
	}

	// Case 1: READY -> NONE, recorded once Docker has actually removed the volume
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.MarkWorkspaceVolumeNone(ctx, workspace0.ID)
		},
	))
	assert.Equal(models.WorkspaceVolumeStateNone, readState())

	{
		events := volumeStateEvents()
		assert.Len(events, 2)
		// ListSystemEvents orders by id (ULID), so the last entry is the most recent
		parsed, err := events[1].ParseMetadata(validate)
		assert.Nil(err)
		meta, ok := parsed.(models.SystemEventWorkspaceVolumeState)
		assert.True(ok, "expected SystemEventWorkspaceVolumeState, got %T", parsed)
		assert.Equal(models.WorkspaceVolumeStateNone, meta.NewState)
	}

	// Case 2: a repeat transition to the current state is permitted - it is how the volume
	// reconciliation re-affirms what it observed in Docker (DESIGN §8.2.2) - and is recorded
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.MarkWorkspaceVolumeNone(ctx, workspace0.ID)
		},
	))
	assert.Equal(models.WorkspaceVolumeStateNone, readState())
	assert.Len(volumeStateEvents(), 3)

	// Case 3: transitioning an unknown workspace surfaces a NotFoundError and records nothing
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.MarkWorkspaceVolumeReady(ctx, uuid.NewString())
			},
		)
		assert.NotNil(err)
		var notFound goutils.NotFoundError
		assert.True(errors.As(err, &notFound), "expected NotFoundError, got %T", err)
		assert.Len(volumeStateEvents(), 3)
	}
}

func TestWorkspaceUpdateName(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtx := context.Background()
	testDB := fmt.Sprintf("/tmp/cairn_ut_%s.db", ulid.Make().String())
	log.WithField("db", testDB).Debug("Test database")

	persistence := getUnitTestPersistence(utCtx, t, testDB)
	validate := getUnitTestValidator(t)

	appName := fmt.Sprintf("ut-app-%s", ulid.Make().String())

	var workspace0 models.Workspace
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			var err error
			workspace0, err = dbClient.DefineNewWorkspace(ctx, db.NewWorkspaceParameter{
				Name: "unit-test-original-name", AppName: appName,
			})
			return err
		},
	))

	// helper: list the recorded rename events
	renameEvents := func() []models.SystemEventAudit {
		var events []models.SystemEventAudit
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				events, err = dbClient.ListSystemEvents(ctx, db.SystemEventQueryFilter{
					EventTypes: []models.SystemEventTypeENUM{models.SystemEventTypeRenameWorkspace},
				})
				return err
			},
		))
		return events
	}

	// Case 0: rename the workspace
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.UpdateWorkspaceName(ctx, workspace0.ID, "unit-test-new-name")
		},
	))

	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			readBack, err := dbClient.GetWorkspace(ctx, workspace0.ID)
			if err != nil {
				return err
			}
			assert.Equal("unit-test-new-name", readBack.Name)
			// the volume name is derived from the immutable ID, so a rename never moves it
			assert.Equal(workspace0.VolumeName, readBack.VolumeName)
			return nil
		},
	))

	// Case 1: the rename event captured both the old and the new name
	{
		events := renameEvents()
		assert.Len(events, 1)
		parsed, err := events[0].ParseMetadata(validate)
		assert.Nil(err)
		meta, ok := parsed.(models.SystemEventRenameWorkspace)
		assert.True(ok, "expected SystemEventRenameWorkspace, got %T", parsed)
		assert.Equal(workspace0.ID, meta.WorkspaceID)
		assert.Equal("unit-test-original-name", meta.OldWorkspaceName)
		assert.Equal("unit-test-new-name", meta.NewWorkspaceName)
	}

	// Case 2: the old name is free to be reused by another workspace
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			_, err := dbClient.DefineNewWorkspace(ctx, db.NewWorkspaceParameter{
				Name: "unit-test-original-name", AppName: appName,
			})
			return err
		},
	))

	// Case 3: renaming onto a name already in use fails, and records no event
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.UpdateWorkspaceName(ctx, workspace0.ID, "unit-test-original-name")
			},
		)
		assert.NotNil(err)
		assert.Len(renameEvents(), 1)
	}

	// Case 4: an invalid new name is rejected before the update, and records no event
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.UpdateWorkspaceName(ctx, workspace0.ID, "not a valid name")
			},
		)
		assert.NotNil(err)
		var validationError goutils.ValidationError
		assert.True(errors.As(err, &validationError), "expected ValidationError, got %T", err)
		assert.Len(renameEvents(), 1)

		// the stored name is untouched
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				readBack, err := dbClient.GetWorkspace(ctx, workspace0.ID)
				if err != nil {
					return err
				}
				assert.Equal("unit-test-new-name", readBack.Name)
				return nil
			},
		))
	}

	// Case 5: renaming an unknown workspace surfaces a NotFoundError
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.UpdateWorkspaceName(ctx, uuid.NewString(), "unit-test-orphan-rename")
			},
		)
		assert.NotNil(err)
		var notFound goutils.NotFoundError
		assert.True(errors.As(err, &notFound), "expected NotFoundError, got %T", err)
	}
}

func TestWorkspaceUpdateDescription(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtx := context.Background()
	testDB := fmt.Sprintf("/tmp/cairn_ut_%s.db", ulid.Make().String())
	log.WithField("db", testDB).Debug("Test database")

	persistence := getUnitTestPersistence(utCtx, t, testDB)

	appName := fmt.Sprintf("ut-app-%s", ulid.Make().String())

	originalDescription := "unit test original description"
	var workspace0 models.Workspace
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			var err error
			workspace0, err = dbClient.DefineNewWorkspace(ctx, db.NewWorkspaceParameter{
				Name:        "unit-test-description",
				Description: &originalDescription,
				AppName:     appName,
			})
			return err
		},
	))

	// helper: read the workspace's current description
	readDescription := func() *string {
		var workspace models.Workspace
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				workspace, err = dbClient.GetWorkspace(ctx, workspace0.ID)
				return err
			},
		))
		return workspace.Description
	}

	// Case 0: change the description
	newDescription := "unit test new description"
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.UpdateWorkspaceDescription(ctx, workspace0.ID, &newDescription)
		},
	))
	readBack := readDescription()
	assert.NotNil(readBack)
	assert.Equal(newDescription, *readBack)

	// Case 1: a nil description clears the column
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.UpdateWorkspaceDescription(ctx, workspace0.ID, nil)
		},
	))
	assert.Nil(readDescription())

	// Case 2: describing an unknown workspace surfaces a NotFoundError
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.UpdateWorkspaceDescription(ctx, uuid.NewString(), &newDescription)
			},
		)
		assert.NotNil(err)
		var notFound goutils.NotFoundError
		assert.True(errors.As(err, &notFound), "expected NotFoundError, got %T", err)
	}

	// Case 3: a description change is not an audited event, so only the NEW_WORKSPACE from the
	// setup was ever recorded
	{
		var events []models.SystemEventAudit
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				events, err = dbClient.ListSystemEvents(ctx, db.SystemEventQueryFilter{})
				return err
			},
		))
		assert.Len(events, 1)
		assert.Equal(models.SystemEventTypeNewWorkspace, events[0].EventType)
	}
}

func TestWorkspaceDeleteWorkspace(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtx := context.Background()
	testDB := fmt.Sprintf("/tmp/cairn_ut_%s.db", ulid.Make().String())
	log.WithField("db", testDB).Debug("Test database")

	persistence := getUnitTestPersistence(utCtx, t, testDB)
	validate := getUnitTestValidator(t)

	appName := fmt.Sprintf("ut-app-%s", ulid.Make().String())

	// helper: define a fresh workspace and return it
	defineWorkspace := func(name string) models.Workspace {
		var workspace models.Workspace
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				workspace, err = dbClient.DefineNewWorkspace(ctx, db.NewWorkspaceParameter{
					Name: name, AppName: appName,
				})
				return err
			},
		))
		return workspace
	}

	// helper: list the recorded workspace delete events
	deleteEvents := func() []models.SystemEventAudit {
		var events []models.SystemEventAudit
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				events, err = dbClient.ListSystemEvents(ctx, db.SystemEventQueryFilter{
					EventTypes: []models.SystemEventTypeENUM{models.SystemEventTypeDeleteWorkspace},
				})
				return err
			},
		))
		return events
	}

	// Case 0: delete a workspace whose volume is already gone
	workspace0 := defineWorkspace("unit-test-delete-clean")
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.DeleteWorkspace(ctx, workspace0.ID)
		},
	))

	// the entry is gone
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				_, err := dbClient.GetWorkspace(ctx, workspace0.ID)
				return err
			},
		)
		assert.NotNil(err)
		var notFound goutils.NotFoundError
		assert.True(errors.As(err, &notFound), "expected NotFoundError, got %T", err)
	}

	// Case 1: the delete event outlives the deleted row
	{
		events := deleteEvents()
		assert.Len(events, 1)
		parsed, err := events[0].ParseMetadata(validate)
		assert.Nil(err)
		meta, ok := parsed.(models.SystemEventDeleteWorkspace)
		assert.True(ok, "expected SystemEventDeleteWorkspace, got %T", parsed)
		assert.Equal(workspace0.ID, meta.WorkspaceID)
		assert.Equal(workspace0.Name, meta.WorkspaceName)
	}

	// Case 2: teardown is bottom-up - a workspace still holding a volume is refused, since the
	// row is the only thing that could later identify its ID-derived volume (DESIGN §4.3)
	workspace2 := defineWorkspace("unit-test-delete-with-volume")
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.MarkWorkspaceVolumeReady(ctx, workspace2.ID)
		},
	))
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.DeleteWorkspace(ctx, workspace2.ID)
			},
		)
		assert.NotNil(err)
		var consistencyError goutils.ConsistencyError
		assert.True(errors.As(err, &consistencyError), "expected ConsistencyError, got %T", err)
		// the refused delete recorded nothing, and the row survives
		assert.Len(deleteEvents(), 1)
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				readBack, err := dbClient.GetWorkspace(ctx, workspace2.ID)
				if err != nil {
					return err
				}
				assert.Equal(models.WorkspaceVolumeStateReady, readBack.VolumeState)
				return nil
			},
		))
	}

	// Case 3: once the volume is recorded gone, the same delete succeeds
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.MarkWorkspaceVolumeNone(ctx, workspace2.ID)
		},
	))
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.DeleteWorkspace(ctx, workspace2.ID)
		},
	))
	assert.Len(deleteEvents(), 2)

	// Case 4: deleting an unknown workspace surfaces a NotFoundError and records nothing
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.DeleteWorkspace(ctx, uuid.NewString())
			},
		)
		assert.NotNil(err)
		var notFound goutils.NotFoundError
		assert.True(errors.As(err, &notFound), "expected NotFoundError, got %T", err)
		assert.Len(deleteEvents(), 2)
	}

	// Case 5: the delete cascades to the workspace's artifact rows (DESIGN §4.1). No
	// object-store interaction - the objects they referenced are left for the reaping GC.
	workspace5 := defineWorkspace("unit-test-delete-cascade")
	var artifact5 models.Artifact
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			var err error
			artifact5, err = dbClient.DefineNewArtifact(ctx, db.NewArtifactParameter{
				WorkspaceID: workspace5.ID,
				Name:        "unit-test-cascade-artifact",
				ObjectKey:   fmt.Sprintf("%s/cascade-%s", workspace5.ID, ulid.Make().String()),
				MIMEType:    "text/plain",
				Size:        42,
			})
			return err
		},
	))

	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.DeleteWorkspace(ctx, workspace5.ID)
		},
	))

	// the artifact row went with its parent
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				_, err := dbClient.GetArtifact(ctx, artifact5.ID)
				return err
			},
		)
		assert.NotNil(err)
		var notFound goutils.NotFoundError
		assert.True(errors.As(err, &notFound), "expected NotFoundError, got %T", err)
	}

	// the cascade is a DB-level constraint, so it emits no per-artifact delete event - only
	// the workspace's own DELETE_WORKSPACE was recorded
	{
		var artifactDeletes []models.SystemEventAudit
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				artifactDeletes, err = dbClient.ListSystemEvents(ctx, db.SystemEventQueryFilter{
					EventTypes: []models.SystemEventTypeENUM{models.SystemEventTypeDeleteArtifact},
				})
				return err
			},
		))
		assert.Empty(artifactDeletes)
		assert.Len(deleteEvents(), 3)
	}
}

func TestWorkspaceListWorkspaces(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtx := context.Background()
	testDB := fmt.Sprintf("/tmp/cairn_ut_%s.db", ulid.Make().String())
	log.WithField("db", testDB).Debug("Test database")

	persistence := getUnitTestPersistence(utCtx, t, testDB)

	appName := fmt.Sprintf("ut-app-%s", ulid.Make().String())

	// helper: run a listing and return it
	listWorkspaces := func(filters db.WorkspaceQueryFilter) []models.Workspace {
		var workspaces []models.Workspace
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				workspaces, err = dbClient.ListWorkspaces(ctx, filters)
				return err
			},
		))
		return workspaces
	}

	// helper: collect the IDs of a listing, for order-insensitive comparison
	idsOf := func(workspaces []models.Workspace) []string {
		ids := []string{}
		for _, workspace := range workspaces {
			ids = append(ids, workspace.ID)
		}
		return ids
	}

	// Case 0: an empty deployment lists nothing - and returns an empty slice, not nil
	{
		listing := listWorkspaces(db.WorkspaceQueryFilter{})
		assert.NotNil(listing)
		assert.Empty(listing)
	}

	// Define four workspaces. They are created in sequence, so `created_at` ordering is the
	// order defined here. Two are then given volumes, so both volume states are represented.
	names := []string{
		"unit-test-list-alpha",
		"unit-test-list-bravo",
		"unit-test-list-charlie",
		"unit-test-list-delta",
	}
	workspaces := []models.Workspace{}
	for _, name := range names {
		var workspace models.Workspace
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				workspace, err = dbClient.DefineNewWorkspace(ctx, db.NewWorkspaceParameter{
					Name: name, AppName: appName,
				})
				return err
			},
		))
		workspaces = append(workspaces, workspace)
	}
	// alpha and charlie hold volumes; bravo and delta do not
	for _, idx := range []int{0, 2} {
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.MarkWorkspaceVolumeReady(ctx, workspaces[idx].ID)
			},
		))
	}

	// Case 1: no filter lists everything, in creation order
	{
		listing := listWorkspaces(db.WorkspaceQueryFilter{})
		assert.Len(listing, 4)
		assert.Equal(idsOf(workspaces), idsOf(listing))
	}

	// Case 2: TargetIDs alone
	{
		listing := listWorkspaces(db.WorkspaceQueryFilter{
			TargetIDs: []string{workspaces[1].ID, workspaces[3].ID},
		})
		assert.Len(listing, 2)
		assert.ElementsMatch([]string{workspaces[1].ID, workspaces[3].ID}, idsOf(listing))
	}

	// Case 3: TargetIDs matching nothing yields an empty listing rather than an error
	{
		listing := listWorkspaces(db.WorkspaceQueryFilter{
			TargetIDs: []string{uuid.NewString()},
		})
		assert.Empty(listing)
	}

	// Case 4: TargetNames alone
	{
		listing := listWorkspaces(db.WorkspaceQueryFilter{
			TargetNames: []string{names[0], names[2]},
		})
		assert.Len(listing, 2)
		assert.ElementsMatch([]string{workspaces[0].ID, workspaces[2].ID}, idsOf(listing))
	}

	// Case 5: VolumeStates alone, each state in turn
	{
		ready := listWorkspaces(db.WorkspaceQueryFilter{
			VolumeStates: []models.WorkspaceVolumeStateENUM{models.WorkspaceVolumeStateReady},
		})
		assert.Len(ready, 2)
		assert.ElementsMatch([]string{workspaces[0].ID, workspaces[2].ID}, idsOf(ready))

		none := listWorkspaces(db.WorkspaceQueryFilter{
			VolumeStates: []models.WorkspaceVolumeStateENUM{models.WorkspaceVolumeStateNone},
		})
		assert.Len(none, 2)
		assert.ElementsMatch([]string{workspaces[1].ID, workspaces[3].ID}, idsOf(none))

		// both states named is equivalent to no state filter at all
		both := listWorkspaces(db.WorkspaceQueryFilter{
			VolumeStates: []models.WorkspaceVolumeStateENUM{
				models.WorkspaceVolumeStateNone, models.WorkspaceVolumeStateReady,
			},
		})
		assert.Len(both, 4)
	}

	// Case 6: an unrecognized volume state is rejected by the `volume_state` macro
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				_, err := dbClient.ListWorkspaces(ctx, db.WorkspaceQueryFilter{
					VolumeStates: []models.WorkspaceVolumeStateENUM{"NOT_A_STATE"},
				})
				return err
			},
		)
		assert.NotNil(err)
		var validationError goutils.ValidationError
		assert.True(errors.As(err, &validationError), "expected ValidationError, got %T", err)
	}

	// Case 7: TargetIDs paired with VolumeStates - the conditions intersect
	{
		// alpha (READY) and bravo (NONE) named, narrowed to READY
		listing := listWorkspaces(db.WorkspaceQueryFilter{
			TargetIDs:    []string{workspaces[0].ID, workspaces[1].ID},
			VolumeStates: []models.WorkspaceVolumeStateENUM{models.WorkspaceVolumeStateReady},
		})
		assert.Len(listing, 1)
		assert.Equal(workspaces[0].ID, listing[0].ID)

		// the same pair narrowed to NONE picks the other one
		listing = listWorkspaces(db.WorkspaceQueryFilter{
			TargetIDs:    []string{workspaces[0].ID, workspaces[1].ID},
			VolumeStates: []models.WorkspaceVolumeStateENUM{models.WorkspaceVolumeStateNone},
		})
		assert.Len(listing, 1)
		assert.Equal(workspaces[1].ID, listing[0].ID)

		// an ID whose state does not match the state filter drops out entirely
		listing = listWorkspaces(db.WorkspaceQueryFilter{
			TargetIDs:    []string{workspaces[1].ID},
			VolumeStates: []models.WorkspaceVolumeStateENUM{models.WorkspaceVolumeStateReady},
		})
		assert.Empty(listing)
	}

	// Case 8: TargetNames paired with VolumeStates
	{
		listing := listWorkspaces(db.WorkspaceQueryFilter{
			TargetNames:  []string{names[2], names[3]},
			VolumeStates: []models.WorkspaceVolumeStateENUM{models.WorkspaceVolumeStateReady},
		})
		assert.Len(listing, 1)
		assert.Equal(workspaces[2].ID, listing[0].ID)

		listing = listWorkspaces(db.WorkspaceQueryFilter{
			TargetNames:  []string{names[2], names[3]},
			VolumeStates: []models.WorkspaceVolumeStateENUM{models.WorkspaceVolumeStateNone},
		})
		assert.Len(listing, 1)
		assert.Equal(workspaces[3].ID, listing[0].ID)
	}

	// Case 9: TargetIDs paired with TargetNames - both conditions must hold, so naming an ID
	// and a name belonging to different entries matches neither
	{
		listing := listWorkspaces(db.WorkspaceQueryFilter{
			TargetIDs:   []string{workspaces[0].ID},
			TargetNames: []string{names[0]},
		})
		assert.Len(listing, 1)
		assert.Equal(workspaces[0].ID, listing[0].ID)

		listing = listWorkspaces(db.WorkspaceQueryFilter{
			TargetIDs:   []string{workspaces[0].ID},
			TargetNames: []string{names[1]},
		})
		assert.Empty(listing)
	}

	// Case 10: pagination walks the full listing in creation order without gaps or repeats
	{
		limit := 3
		firstPage := listWorkspaces(db.WorkspaceQueryFilter{
			CommonListEntryQueryFilter: db.CommonListEntryQueryFilter{Limit: &limit},
		})
		assert.Len(firstPage, 3)
		assert.Equal(idsOf(workspaces[:3]), idsOf(firstPage))

		offset := 3
		secondPage := listWorkspaces(db.WorkspaceQueryFilter{
			CommonListEntryQueryFilter: db.CommonListEntryQueryFilter{
				Limit: &limit, Offset: &offset,
			},
		})
		assert.Len(secondPage, 1)
		assert.Equal(idsOf(workspaces[3:]), idsOf(secondPage))

		// walking past the end is empty, not an error
		offset = 4
		assert.Empty(listWorkspaces(db.WorkspaceQueryFilter{
			CommonListEntryQueryFilter: db.CommonListEntryQueryFilter{
				Limit: &limit, Offset: &offset,
			},
		}))
	}

	// Case 11: pagination composes with the other filters, paging the filtered set
	{
		limit := 1
		offset := 1
		listing := listWorkspaces(db.WorkspaceQueryFilter{
			CommonListEntryQueryFilter: db.CommonListEntryQueryFilter{
				Limit: &limit, Offset: &offset,
			},
			VolumeStates: []models.WorkspaceVolumeStateENUM{models.WorkspaceVolumeStateReady},
		})
		assert.Len(listing, 1)
		// the READY set is alpha then charlie, so offset 1 is charlie
		assert.Equal(workspaces[2].ID, listing[0].ID)
	}

	// Case 12: the pagination bounds are validated - a non-positive limit and a negative
	// offset are both rejected
	{
		badLimit := 0
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				_, err := dbClient.ListWorkspaces(ctx, db.WorkspaceQueryFilter{
					CommonListEntryQueryFilter: db.CommonListEntryQueryFilter{Limit: &badLimit},
				})
				return err
			},
		)
		assert.NotNil(err)
		var validationError goutils.ValidationError
		assert.True(errors.As(err, &validationError), "expected ValidationError, got %T", err)

		badOffset := -1
		err = persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				_, err := dbClient.ListWorkspaces(ctx, db.WorkspaceQueryFilter{
					CommonListEntryQueryFilter: db.CommonListEntryQueryFilter{Offset: &badOffset},
				})
				return err
			},
		)
		assert.NotNil(err)
		assert.True(errors.As(err, &validationError), "expected ValidationError, got %T", err)
	}

	// Case 13: a deleted workspace drops out of the listing
	{
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.DeleteWorkspace(ctx, workspaces[1].ID)
			},
		))
		listing := listWorkspaces(db.WorkspaceQueryFilter{})
		assert.Len(listing, 3)
		assert.NotContains(idsOf(listing), workspaces[1].ID)
	}
}
