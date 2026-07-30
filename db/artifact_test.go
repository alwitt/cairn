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
	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
)

// defineUnitTestWorkspace define a parent workspace for artifacts to hang off of
func defineUnitTestWorkspace(
	ctx context.Context, t *testing.T, persistence db.Client, name string,
) models.Workspace {
	assert := assert.New(t)

	var workspace models.Workspace
	assert.Nil(persistence.UseDatabaseInTransaction(
		ctx, func(ctx context.Context, dbClient db.Database) error {
			var err error
			workspace, err = dbClient.DefineNewWorkspace(ctx, db.NewWorkspaceParameter{
				Name: name, AppName: fmt.Sprintf("ut-app-%s", ulid.Make().String()),
			})
			return err
		},
	))

	return workspace
}

// unitTestObjectKey build an object key of the shape the storage layer produces - workspace
// scoped, with a random suffix rather than the artifact name (see DESIGN §2.2)
func unitTestObjectKey(workspaceID string) string {
	return fmt.Sprintf("%s/%s", workspaceID, ulid.Make().String())
}

func TestArtifactDefineNewArtifact(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtx := context.Background()
	testDB := fmt.Sprintf("/tmp/cairn_ut_%s.db", ulid.Make().String())
	log.WithField("db", testDB).Debug("Test database")

	persistence := getUnitTestPersistence(utCtx, t, testDB)
	validate := getUnitTestValidator(t)

	workspace := defineUnitTestWorkspace(utCtx, t, persistence, "unit-test-artifact-parent")

	// Case 0: define an artifact without a description
	objectKey0 := unitTestObjectKey(workspace.ID)
	var artifact0 models.Artifact
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			var err error
			artifact0, err = dbClient.DefineNewArtifact(ctx, db.NewArtifactParameter{
				WorkspaceID: workspace.ID,
				Name:        "unit-test-artifact",
				ObjectKey:   objectKey0,
				MIMEType:    "text/plain",
				Size:        1024,
			})
			return err
		},
	))
	assert.NotEmpty(artifact0.ID)
	assert.Equal(workspace.ID, artifact0.WorkspaceID)
	assert.Equal("unit-test-artifact", artifact0.Name)
	assert.Nil(artifact0.Description)
	assert.Equal(objectKey0, artifact0.ObjectKey)
	assert.Equal("text/plain", artifact0.MIMEType)
	assert.Equal(int64(1024), artifact0.Size)
	// the object is already in place at its final key, so the entry is committed directly as
	// RECORDED - there is no pending state (DESIGN §6.1)
	assert.Equal(models.ArtifactStateRecorded, artifact0.State)

	// Case 1: the entry persisted and reads back identically
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			readBack, err := dbClient.GetArtifact(ctx, artifact0.ID)
			if err != nil {
				return err
			}
			assert.Equal(artifact0.ID, readBack.ID)
			assert.Equal(artifact0.WorkspaceID, readBack.WorkspaceID)
			assert.Equal(artifact0.Name, readBack.Name)
			assert.Equal(artifact0.ObjectKey, readBack.ObjectKey)
			assert.Equal(artifact0.MIMEType, readBack.MIMEType)
			assert.Equal(artifact0.Size, readBack.Size)
			assert.Equal(models.ArtifactStateRecorded, readBack.State)
			assert.Nil(readBack.Description)
			return nil
		},
	))

	// Case 2: the NEW_ARTIFACT event captured the parent workspace and the backing object
	{
		var events []models.SystemEventAudit
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				events, err = dbClient.ListSystemEvents(ctx, db.SystemEventQueryFilter{
					EventTypes: []models.SystemEventTypeENUM{models.SystemEventTypeNewArtifact},
				})
				return err
			},
		))
		assert.Len(events, 1)
		parsed, err := events[0].ParseMetadata(validate)
		assert.Nil(err)
		meta, ok := parsed.(models.SystemEventNewArtifact)
		assert.True(ok, "expected SystemEventNewArtifact, got %T", parsed)
		assert.Equal(workspace.ID, meta.WorkspaceID)
		assert.Equal(artifact0.ID, meta.ArtifactID)
		assert.Equal(artifact0.Name, meta.ArtifactName)
		assert.Equal(objectKey0, meta.ObjectKey)
		assert.Equal("text/plain", meta.MIMEType)
		assert.Equal(int64(1024), meta.Size)
	}

	// Case 3: define an artifact with a description, and a zero-byte object
	description := "unit test artifact description"
	var artifact3 models.Artifact
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			var err error
			artifact3, err = dbClient.DefineNewArtifact(ctx, db.NewArtifactParameter{
				WorkspaceID: workspace.ID,
				Name:        "unit-test-artifact-described",
				Description: &description,
				ObjectKey:   unitTestObjectKey(workspace.ID),
				MIMEType:    "application/octet-stream",
				Size:        0,
			})
			return err
		},
	))
	assert.NotNil(artifact3.Description)
	assert.Equal(description, *artifact3.Description)
	assert.Equal(int64(0), artifact3.Size)
	// artifact IDs are ULIDs, so they are creation-ordered
	assert.Greater(artifact3.ID, artifact0.ID)

	// Case 4: artifact names are unique WITHIN a workspace
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				_, err := dbClient.DefineNewArtifact(ctx, db.NewArtifactParameter{
					WorkspaceID: workspace.ID,
					Name:        "unit-test-artifact",
					ObjectKey:   unitTestObjectKey(workspace.ID),
					MIMEType:    "text/plain",
					Size:        1,
				})
				return err
			},
		)
		assert.NotNil(err)
	}

	// Case 5: ... but the same name is free in a different workspace
	otherWorkspace := defineUnitTestWorkspace(
		utCtx, t, persistence, "unit-test-artifact-other-parent",
	)
	var artifact5 models.Artifact
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			var err error
			artifact5, err = dbClient.DefineNewArtifact(ctx, db.NewArtifactParameter{
				WorkspaceID: otherWorkspace.ID,
				Name:        "unit-test-artifact",
				ObjectKey:   unitTestObjectKey(otherWorkspace.ID),
				MIMEType:    "text/plain",
				Size:        7,
			})
			return err
		},
	))
	assert.Equal(otherWorkspace.ID, artifact5.WorkspaceID)
	assert.NotEqual(artifact0.ID, artifact5.ID)

	// Case 6: the artifact name must satisfy the `valid_name` macro
	for _, badName := range []string{"", "has space", "has/slash", "has.dot"} {
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				_, err := dbClient.DefineNewArtifact(ctx, db.NewArtifactParameter{
					WorkspaceID: workspace.ID,
					Name:        badName,
					ObjectKey:   unitTestObjectKey(workspace.ID),
					MIMEType:    "text/plain",
					Size:        1,
				})
				return err
			},
		)
		assert.NotNil(err, "name '%s' should be rejected", badName)
		var validationError goutils.ValidationError
		assert.True(errors.As(err, &validationError), "expected ValidationError, got %T", err)
	}

	// Case 7: the backing object fields are required, and the size may not be negative
	{
		badParams := map[string]db.NewArtifactParameter{
			"missing object key": {
				WorkspaceID: workspace.ID,
				Name:        "unit-test-artifact-no-key",
				MIMEType:    "text/plain",
				Size:        1,
			},
			"missing MIME type": {
				WorkspaceID: workspace.ID,
				Name:        "unit-test-artifact-no-mime",
				ObjectKey:   unitTestObjectKey(workspace.ID),
				Size:        1,
			},
			"negative size": {
				WorkspaceID: workspace.ID,
				Name:        "unit-test-artifact-negative",
				ObjectKey:   unitTestObjectKey(workspace.ID),
				MIMEType:    "text/plain",
				Size:        -1,
			},
			"missing workspace ID": {
				Name:      "unit-test-artifact-no-workspace",
				ObjectKey: unitTestObjectKey(workspace.ID),
				MIMEType:  "text/plain",
				Size:      1,
			},
		}
		for reason, params := range badParams {
			err := persistence.UseDatabaseInTransaction(
				utCtx, func(ctx context.Context, dbClient db.Database) error {
					_, err := dbClient.DefineNewArtifact(ctx, params)
					return err
				},
			)
			assert.NotNil(err, "%s should be rejected", reason)
			var validationError goutils.ValidationError
			assert.True(
				errors.As(err, &validationError), "%s: expected ValidationError, got %T", reason, err,
			)
		}
	}

	// Case 8: an artifact can't hang off a workspace which does not exist - the parent
	// association is a foreign key
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				_, err := dbClient.DefineNewArtifact(ctx, db.NewArtifactParameter{
					WorkspaceID: uuid.NewString(),
					Name:        "unit-test-artifact-orphan",
					ObjectKey:   unitTestObjectKey(uuid.NewString()),
					MIMEType:    "text/plain",
					Size:        1,
				})
				return err
			},
		)
		assert.NotNil(err)
	}

	// Case 9: only the successful definitions were audited
	{
		var events []models.SystemEventAudit
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				events, err = dbClient.ListSystemEvents(ctx, db.SystemEventQueryFilter{
					EventTypes: []models.SystemEventTypeENUM{models.SystemEventTypeNewArtifact},
				})
				return err
			},
		))
		assert.Len(events, 3)
	}
}

func TestArtifactGetArtifact(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtx := context.Background()
	testDB := fmt.Sprintf("/tmp/cairn_ut_%s.db", ulid.Make().String())
	log.WithField("db", testDB).Debug("Test database")

	persistence := getUnitTestPersistence(utCtx, t, testDB)

	workspace := defineUnitTestWorkspace(utCtx, t, persistence, "unit-test-get-artifact-parent")
	otherWorkspace := defineUnitTestWorkspace(
		utCtx, t, persistence, "unit-test-get-artifact-other-parent",
	)

	// helper: define an artifact under a given workspace
	defineArtifact := func(workspaceID, name string) models.Artifact {
		var artifact models.Artifact
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				artifact, err = dbClient.DefineNewArtifact(ctx, db.NewArtifactParameter{
					WorkspaceID: workspaceID,
					Name:        name,
					ObjectKey:   unitTestObjectKey(workspaceID),
					MIMEType:    "text/plain",
					Size:        16,
				})
				return err
			},
		))
		return artifact
	}

	// the same artifact name in two different workspaces, so a name lookup which ignored the
	// workspace would be ambiguous
	artifact0 := defineArtifact(workspace.ID, "unit-test-shared-name")
	artifact1 := defineArtifact(otherWorkspace.ID, "unit-test-shared-name")

	// Case 0: fetch by ID
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			readBack, err := dbClient.GetArtifact(ctx, artifact0.ID)
			if err != nil {
				return err
			}
			assert.Equal(artifact0.ID, readBack.ID)
			assert.Equal(workspace.ID, readBack.WorkspaceID)
			assert.Equal(artifact0.ObjectKey, readBack.ObjectKey)
			return nil
		},
	))

	// Case 1: fetch by (workspace, name) - backs the MCP layer's name -> ID resolution
	// (DESIGN §3). The shared name resolves to a different entry in each workspace.
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			readBack, err := dbClient.GetArtifactByName(
				ctx, workspace.ID, "unit-test-shared-name",
			)
			if err != nil {
				return err
			}
			assert.Equal(artifact0.ID, readBack.ID)

			readBack, err = dbClient.GetArtifactByName(
				ctx, otherWorkspace.ID, "unit-test-shared-name",
			)
			if err != nil {
				return err
			}
			assert.Equal(artifact1.ID, readBack.ID)
			return nil
		},
	))

	// Case 2: an unknown ID surfaces a NotFoundError, not a SQLError
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				_, err := dbClient.GetArtifact(ctx, ulid.Make().String())
				return err
			},
		)
		assert.NotNil(err)
		var notFound goutils.NotFoundError
		assert.True(errors.As(err, &notFound), "expected NotFoundError, got %T", err)
	}

	// Case 3: an unknown name surfaces a NotFoundError
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				_, err := dbClient.GetArtifactByName(ctx, workspace.ID, "unit-test-no-such-artifact")
				return err
			},
		)
		assert.NotNil(err)
		var notFound goutils.NotFoundError
		assert.True(errors.As(err, &notFound), "expected NotFoundError, got %T", err)
	}

	// Case 4: a name which exists, but under a different workspace, is still not found - the
	// lookup is scoped to the parent
	{
		thirdWorkspace := defineUnitTestWorkspace(
			utCtx, t, persistence, "unit-test-get-artifact-third-parent",
		)
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				_, err := dbClient.GetArtifactByName(
					ctx, thirdWorkspace.ID, "unit-test-shared-name",
				)
				return err
			},
		)
		assert.NotNil(err)
		var notFound goutils.NotFoundError
		assert.True(errors.As(err, &notFound), "expected NotFoundError, got %T", err)
	}

	// Case 5: reading an artifact records no audit event - only the two NEW_ARTIFACT from the
	// setup were ever written
	{
		var events []models.SystemEventAudit
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				events, err = dbClient.ListSystemEvents(ctx, db.SystemEventQueryFilter{
					EventTypes: []models.SystemEventTypeENUM{models.SystemEventTypeNewArtifact},
				})
				return err
			},
		))
		assert.Len(events, 2)
	}
}

func TestArtifactUpdateName(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtx := context.Background()
	testDB := fmt.Sprintf("/tmp/cairn_ut_%s.db", ulid.Make().String())
	log.WithField("db", testDB).Debug("Test database")

	persistence := getUnitTestPersistence(utCtx, t, testDB)
	validate := getUnitTestValidator(t)

	workspace := defineUnitTestWorkspace(utCtx, t, persistence, "unit-test-rename-parent")

	// helper: define an artifact under the parent workspace
	defineArtifact := func(name string) models.Artifact {
		var artifact models.Artifact
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				artifact, err = dbClient.DefineNewArtifact(ctx, db.NewArtifactParameter{
					WorkspaceID: workspace.ID,
					Name:        name,
					ObjectKey:   unitTestObjectKey(workspace.ID),
					MIMEType:    "text/plain",
					Size:        16,
				})
				return err
			},
		))
		return artifact
	}

	// helper: list the recorded artifact rename events
	renameEvents := func() []models.SystemEventAudit {
		var events []models.SystemEventAudit
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				events, err = dbClient.ListSystemEvents(ctx, db.SystemEventQueryFilter{
					EventTypes: []models.SystemEventTypeENUM{models.SystemEventTypeRenameArtifact},
				})
				return err
			},
		))
		return events
	}

	artifact0 := defineArtifact("unit-test-original-artifact")

	// Case 0: rename the artifact
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.UpdateArtifactName(ctx, artifact0.ID, "unit-test-renamed-artifact")
		},
	))

	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			readBack, err := dbClient.GetArtifact(ctx, artifact0.ID)
			if err != nil {
				return err
			}
			assert.Equal("unit-test-renamed-artifact", readBack.Name)
			// the object key carries a random suffix rather than the name, so a rename never
			// touches the object store (DESIGN §2.2)
			assert.Equal(artifact0.ObjectKey, readBack.ObjectKey)
			assert.Equal(models.ArtifactStateRecorded, readBack.State)
			return nil
		},
	))

	// Case 1: the rename event captured both the old and the new name, plus the parent
	{
		events := renameEvents()
		assert.Len(events, 1)
		parsed, err := events[0].ParseMetadata(validate)
		assert.Nil(err)
		meta, ok := parsed.(models.SystemEventRenameArtifact)
		assert.True(ok, "expected SystemEventRenameArtifact, got %T", parsed)
		assert.Equal(workspace.ID, meta.WorkspaceID)
		assert.Equal(artifact0.ID, meta.ArtifactID)
		assert.Equal("unit-test-original-artifact", meta.OldArtifactName)
		assert.Equal("unit-test-renamed-artifact", meta.NewArtifactName)
	}

	// Case 2: the old name is free to be reused within the same workspace
	artifact2 := defineArtifact("unit-test-original-artifact")

	// Case 3: renaming onto a name already taken in the workspace fails, and records no event
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.UpdateArtifactName(ctx, artifact0.ID, artifact2.Name)
			},
		)
		assert.NotNil(err)
		assert.Len(renameEvents(), 1)
	}

	// Case 4: an invalid new name is rejected before the update, and records no event
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.UpdateArtifactName(ctx, artifact0.ID, "not a valid name")
			},
		)
		assert.NotNil(err)
		var validationError goutils.ValidationError
		assert.True(errors.As(err, &validationError), "expected ValidationError, got %T", err)
		assert.Len(renameEvents(), 1)

		// the stored name is untouched
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				readBack, err := dbClient.GetArtifact(ctx, artifact0.ID)
				if err != nil {
					return err
				}
				assert.Equal("unit-test-renamed-artifact", readBack.Name)
				return nil
			},
		))
	}

	// Case 5: renaming an unknown artifact surfaces a NotFoundError
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.UpdateArtifactName(
					ctx, ulid.Make().String(), "unit-test-orphan-rename",
				)
			},
		)
		assert.NotNil(err)
		var notFound goutils.NotFoundError
		assert.True(errors.As(err, &notFound), "expected NotFoundError, got %T", err)
		assert.Len(renameEvents(), 1)
	}
}

func TestArtifactUpdateDescription(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtx := context.Background()
	testDB := fmt.Sprintf("/tmp/cairn_ut_%s.db", ulid.Make().String())
	log.WithField("db", testDB).Debug("Test database")

	persistence := getUnitTestPersistence(utCtx, t, testDB)

	workspace := defineUnitTestWorkspace(utCtx, t, persistence, "unit-test-describe-parent")

	originalDescription := "unit test original artifact description"
	var artifact0 models.Artifact
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			var err error
			artifact0, err = dbClient.DefineNewArtifact(ctx, db.NewArtifactParameter{
				WorkspaceID: workspace.ID,
				Name:        "unit-test-described-artifact",
				Description: &originalDescription,
				ObjectKey:   unitTestObjectKey(workspace.ID),
				MIMEType:    "text/plain",
				Size:        16,
			})
			return err
		},
	))

	// helper: read the artifact's current description
	readDescription := func() *string {
		var artifact models.Artifact
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				artifact, err = dbClient.GetArtifact(ctx, artifact0.ID)
				return err
			},
		))
		return artifact.Description
	}

	// Case 0: change the description
	newDescription := "unit test new artifact description"
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.UpdateArtifactDescription(ctx, artifact0.ID, &newDescription)
		},
	))
	readBack := readDescription()
	assert.NotNil(readBack)
	assert.Equal(newDescription, *readBack)

	// Case 1: a nil description clears the column
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.UpdateArtifactDescription(ctx, artifact0.ID, nil)
		},
	))
	assert.Nil(readDescription())

	// Case 2: the description change left the backing object alone
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			artifact, err := dbClient.GetArtifact(ctx, artifact0.ID)
			if err != nil {
				return err
			}
			assert.Equal(artifact0.ObjectKey, artifact.ObjectKey)
			assert.Equal(artifact0.Size, artifact.Size)
			assert.Equal(models.ArtifactStateRecorded, artifact.State)
			return nil
		},
	))

	// Case 3: describing an unknown artifact surfaces a NotFoundError
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.UpdateArtifactDescription(
					ctx, ulid.Make().String(), &newDescription,
				)
			},
		)
		assert.NotNil(err)
		var notFound goutils.NotFoundError
		assert.True(errors.As(err, &notFound), "expected NotFoundError, got %T", err)
	}

	// Case 4: a description change is not an audited event, so only the NEW_WORKSPACE and
	// NEW_ARTIFACT from the setup were ever recorded
	{
		var events []models.SystemEventAudit
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				events, err = dbClient.ListSystemEvents(ctx, db.SystemEventQueryFilter{})
				return err
			},
		))
		assert.Len(events, 2)
		assert.Equal(models.SystemEventTypeNewWorkspace, events[0].EventType)
		assert.Equal(models.SystemEventTypeNewArtifact, events[1].EventType)
	}
}

func TestArtifactUpdateObject(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtx := context.Background()
	testDB := fmt.Sprintf("/tmp/cairn_ut_%s.db", ulid.Make().String())
	log.WithField("db", testDB).Debug("Test database")

	persistence := getUnitTestPersistence(utCtx, t, testDB)
	validate := getUnitTestValidator(t)

	workspace := defineUnitTestWorkspace(utCtx, t, persistence, "unit-test-update-object-parent")

	originalKey := unitTestObjectKey(workspace.ID)
	var artifact0 models.Artifact
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			var err error
			artifact0, err = dbClient.DefineNewArtifact(ctx, db.NewArtifactParameter{
				WorkspaceID: workspace.ID,
				Name:        "unit-test-update-object-artifact",
				ObjectKey:   originalKey,
				MIMEType:    "text/plain",
				Size:        100,
			})
			return err
		},
	))

	// helper: list the recorded object update events
	updateEvents := func() []models.SystemEventAudit {
		var events []models.SystemEventAudit
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				events, err = dbClient.ListSystemEvents(ctx, db.SystemEventQueryFilter{
					EventTypes: []models.SystemEventTypeENUM{
						models.SystemEventTypeUpdateArtifactObject,
					},
				})
				return err
			},
		))
		return events
	}

	// helper: read the artifact back
	readArtifact := func() models.Artifact {
		var artifact models.Artifact
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				artifact, err = dbClient.GetArtifact(ctx, artifact0.ID)
				return err
			},
		))
		return artifact
	}

	// Case 0: repoint the artifact at a new object. The update writes a NEW key and flips the
	// row over to it - the old object is orphaned for the reaping GC (DESIGN §6.3).
	secondKey := unitTestObjectKey(workspace.ID)
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.UpdateArtifactObject(
				ctx, artifact0.ID, secondKey, "application/json", 250,
			)
		},
	))

	{
		readBack := readArtifact()
		assert.Equal(secondKey, readBack.ObjectKey)
		assert.NotEqual(originalKey, readBack.ObjectKey)
		assert.Equal("application/json", readBack.MIMEType)
		assert.Equal(int64(250), readBack.Size)
		assert.Equal(models.ArtifactStateRecorded, readBack.State)
		// the update repoints the object only - the identity of the artifact is unchanged
		assert.Equal(artifact0.ID, readBack.ID)
		assert.Equal(artifact0.Name, readBack.Name)
		assert.Equal(artifact0.WorkspaceID, readBack.WorkspaceID)
	}

	// Case 1: the update event captured BOTH the outgoing and the incoming object. The old key
	// recorded here is the audit trail's only record of what the orphan was.
	{
		events := updateEvents()
		assert.Len(events, 1)
		parsed, err := events[0].ParseMetadata(validate)
		assert.Nil(err)
		meta, ok := parsed.(models.SystemEventUpdateArtifactObject)
		assert.True(ok, "expected SystemEventUpdateArtifactObject, got %T", parsed)
		assert.Equal(workspace.ID, meta.WorkspaceID)
		assert.Equal(artifact0.ID, meta.ArtifactID)
		assert.Equal(artifact0.Name, meta.ArtifactName)
		assert.Equal(originalKey, meta.OldObjectKey)
		assert.Equal("text/plain", meta.OldMIMEType)
		assert.Equal(int64(100), meta.OldSize)
		assert.Equal(secondKey, meta.NewObjectKey)
		assert.Equal("application/json", meta.NewMIMEType)
		assert.Equal(int64(250), meta.NewSize)
	}

	// Case 2: a second update chains off the first - the old values are now the previous
	// update's new values
	thirdKey := unitTestObjectKey(workspace.ID)
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.UpdateArtifactObject(
				ctx, artifact0.ID, thirdKey, "text/csv", 0,
			)
		},
	))
	{
		events := updateEvents()
		assert.Len(events, 2)
		// ListSystemEvents orders by id (ULID), so the last entry is the most recent
		parsed, err := events[1].ParseMetadata(validate)
		assert.Nil(err)
		meta, ok := parsed.(models.SystemEventUpdateArtifactObject)
		assert.True(ok, "expected SystemEventUpdateArtifactObject, got %T", parsed)
		assert.Equal(secondKey, meta.OldObjectKey)
		assert.Equal("application/json", meta.OldMIMEType)
		assert.Equal(int64(250), meta.OldSize)
		assert.Equal(thirdKey, meta.NewObjectKey)
		assert.Equal("text/csv", meta.NewMIMEType)
		assert.Equal(int64(0), meta.NewSize)
	}

	// Case 3: an update restores a quarantined artifact to RECORDED, so re-uploading the bytes
	// of an artifact whose object went missing brings it back into service
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.MarkArtifactMissingObject(ctx, artifact0.ID)
		},
	))
	assert.Equal(models.ArtifactStateMissingObject, readArtifact().State)

	recoveryKey := unitTestObjectKey(workspace.ID)
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.UpdateArtifactObject(
				ctx, artifact0.ID, recoveryKey, "text/plain", 42,
			)
		},
	))
	{
		readBack := readArtifact()
		assert.Equal(models.ArtifactStateRecorded, readBack.State)
		assert.Equal(recoveryKey, readBack.ObjectKey)
		assert.Equal(int64(42), readBack.Size)
		assert.Len(updateEvents(), 3)
	}

	// Case 4: an invalid new object is rejected before the update, and records no event
	{
		badUpdates := map[string]struct {
			objectKey string
			mimeType  string
			size      int64
		}{
			"missing object key": {objectKey: "", mimeType: "text/plain", size: 1},
			"missing MIME type": {
				objectKey: unitTestObjectKey(workspace.ID), mimeType: "", size: 1,
			},
			"negative size": {
				objectKey: unitTestObjectKey(workspace.ID), mimeType: "text/plain", size: -1,
			},
		}
		for reason, update := range badUpdates {
			err := persistence.UseDatabaseInTransaction(
				utCtx, func(ctx context.Context, dbClient db.Database) error {
					return dbClient.UpdateArtifactObject(
						ctx, artifact0.ID, update.objectKey, update.mimeType, update.size,
					)
				},
			)
			assert.NotNil(err, "%s should be rejected", reason)
			var validationError goutils.ValidationError
			assert.True(
				errors.As(err, &validationError), "%s: expected ValidationError, got %T", reason, err,
			)
		}
		assert.Len(updateEvents(), 3)

		// the stored object reference is untouched
		readBack := readArtifact()
		assert.Equal(recoveryKey, readBack.ObjectKey)
		assert.Equal("text/plain", readBack.MIMEType)
		assert.Equal(int64(42), readBack.Size)
	}

	// Case 5: updating an unknown artifact surfaces a NotFoundError and records nothing
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.UpdateArtifactObject(
					ctx, ulid.Make().String(), unitTestObjectKey(workspace.ID), "text/plain", 1,
				)
			},
		)
		assert.NotNil(err)
		var notFound goutils.NotFoundError
		assert.True(errors.As(err, &notFound), "expected NotFoundError, got %T", err)
		assert.Len(updateEvents(), 3)
	}
}

func TestArtifactMarkMissingObject(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtx := context.Background()
	testDB := fmt.Sprintf("/tmp/cairn_ut_%s.db", ulid.Make().String())
	log.WithField("db", testDB).Debug("Test database")

	persistence := getUnitTestPersistence(utCtx, t, testDB)
	validate := getUnitTestValidator(t)

	workspace := defineUnitTestWorkspace(utCtx, t, persistence, "unit-test-missing-object-parent")

	originalKey := unitTestObjectKey(workspace.ID)
	var artifact0 models.Artifact
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			var err error
			artifact0, err = dbClient.DefineNewArtifact(ctx, db.NewArtifactParameter{
				WorkspaceID: workspace.ID,
				Name:        "unit-test-missing-object-artifact",
				ObjectKey:   originalKey,
				MIMEType:    "text/plain",
				Size:        512,
			})
			return err
		},
	))

	// helper: list the recorded missing object events
	missingEvents := func() []models.SystemEventAudit {
		var events []models.SystemEventAudit
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				events, err = dbClient.ListSystemEvents(ctx, db.SystemEventQueryFilter{
					EventTypes: []models.SystemEventTypeENUM{
						models.SystemEventTypeArtifactMissingObject,
					},
				})
				return err
			},
		))
		return events
	}

	// helper: read the artifact back
	readArtifact := func() models.Artifact {
		var artifact models.Artifact
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				artifact, err = dbClient.GetArtifact(ctx, artifact0.ID)
				return err
			},
		))
		return artifact
	}

	assert.Equal(models.ArtifactStateRecorded, readArtifact().State)
	assert.Empty(missingEvents())

	// Case 0: quarantine the artifact. This is a data-loss signal, not routine garbage, so the
	// row is preserved as evidence rather than removed (DESIGN §8.2.1).
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.MarkArtifactMissingObject(ctx, artifact0.ID)
		},
	))

	{
		readBack := readArtifact()
		assert.Equal(models.ArtifactStateMissingObject, readBack.State)
		// only the state moved - the metadata survives as the record of what was lost, and the
		// key which resolved to nothing is still on the row
		assert.Equal(artifact0.ID, readBack.ID)
		assert.Equal(artifact0.Name, readBack.Name)
		assert.Equal(originalKey, readBack.ObjectKey)
		assert.Equal("text/plain", readBack.MIMEType)
		assert.Equal(int64(512), readBack.Size)
	}

	// Case 1: the event preserves the key which resolved to no object, as evidence
	{
		events := missingEvents()
		assert.Len(events, 1)
		parsed, err := events[0].ParseMetadata(validate)
		assert.Nil(err)
		meta, ok := parsed.(models.SystemEventArtifactMissingObject)
		assert.True(ok, "expected SystemEventArtifactMissingObject, got %T", parsed)
		assert.Equal(workspace.ID, meta.WorkspaceID)
		assert.Equal(artifact0.ID, meta.ArtifactID)
		assert.Equal(artifact0.Name, meta.ArtifactName)
		assert.Equal(originalKey, meta.ObjectKey)
	}

	// Case 2: quarantining an already quarantined artifact is permitted - it is how the object
	// reconciliation re-affirms what it observed in the store (DESIGN §8.2.1) - and is recorded
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.MarkArtifactMissingObject(ctx, artifact0.ID)
		},
	))
	assert.Equal(models.ArtifactStateMissingObject, readArtifact().State)
	assert.Len(missingEvents(), 2)

	// Case 3: a quarantined artifact is still renameable and describable - the row is live
	// metadata awaiting an operator's decision, not a tombstone
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.UpdateArtifactName(ctx, artifact0.ID, "unit-test-quarantined-renamed")
		},
	))
	{
		readBack := readArtifact()
		assert.Equal("unit-test-quarantined-renamed", readBack.Name)
		// the rename did not disturb the quarantine
		assert.Equal(models.ArtifactStateMissingObject, readBack.State)
	}

	// Case 4: quarantining an unknown artifact surfaces a NotFoundError and records nothing
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.MarkArtifactMissingObject(ctx, ulid.Make().String())
			},
		)
		assert.NotNil(err)
		var notFound goutils.NotFoundError
		assert.True(errors.As(err, &notFound), "expected NotFoundError, got %T", err)
		assert.Len(missingEvents(), 2)
	}

	// Case 5: quarantining one artifact leaves its siblings alone
	{
		var sibling models.Artifact
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				sibling, err = dbClient.DefineNewArtifact(ctx, db.NewArtifactParameter{
					WorkspaceID: workspace.ID,
					Name:        "unit-test-missing-object-sibling",
					ObjectKey:   unitTestObjectKey(workspace.ID),
					MIMEType:    "text/plain",
					Size:        8,
				})
				return err
			},
		))

		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				readBack, err := dbClient.GetArtifact(ctx, sibling.ID)
				if err != nil {
					return err
				}
				assert.Equal(models.ArtifactStateRecorded, readBack.State)
				return nil
			},
		))
		assert.Len(missingEvents(), 2)
	}
}

func TestArtifactDeleteArtifact(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtx := context.Background()
	testDB := fmt.Sprintf("/tmp/cairn_ut_%s.db", ulid.Make().String())
	log.WithField("db", testDB).Debug("Test database")

	persistence := getUnitTestPersistence(utCtx, t, testDB)
	validate := getUnitTestValidator(t)

	workspace := defineUnitTestWorkspace(utCtx, t, persistence, "unit-test-delete-artifact-parent")

	// helper: define an artifact under the parent workspace
	defineArtifact := func(name string) models.Artifact {
		var artifact models.Artifact
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				artifact, err = dbClient.DefineNewArtifact(ctx, db.NewArtifactParameter{
					WorkspaceID: workspace.ID,
					Name:        name,
					ObjectKey:   unitTestObjectKey(workspace.ID),
					MIMEType:    "text/plain",
					Size:        64,
				})
				return err
			},
		))
		return artifact
	}

	// helper: list the recorded artifact delete events
	deleteEvents := func() []models.SystemEventAudit {
		var events []models.SystemEventAudit
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				events, err = dbClient.ListSystemEvents(ctx, db.SystemEventQueryFilter{
					EventTypes: []models.SystemEventTypeENUM{models.SystemEventTypeDeleteArtifact},
				})
				return err
			},
		))
		return events
	}

	// Case 0: delete a RECORDED artifact
	artifact0 := defineArtifact("unit-test-delete-recorded")
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.DeleteArtifact(ctx, artifact0.ID)
		},
	))

	// the entry is gone
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				_, err := dbClient.GetArtifact(ctx, artifact0.ID)
				return err
			},
		)
		assert.NotNil(err)
		var notFound goutils.NotFoundError
		assert.True(errors.As(err, &notFound), "expected NotFoundError, got %T", err)
	}

	// Case 1: the delete event outlives the deleted row, and preserves the object key. No
	// object-store interaction happens here - the object is left for the reaping GC
	// (DESIGN §4.1, §8.2.1), so this event is the record of what it was.
	{
		events := deleteEvents()
		assert.Len(events, 1)
		parsed, err := events[0].ParseMetadata(validate)
		assert.Nil(err)
		meta, ok := parsed.(models.SystemEventDeleteArtifact)
		assert.True(ok, "expected SystemEventDeleteArtifact, got %T", parsed)
		assert.Equal(workspace.ID, meta.WorkspaceID)
		assert.Equal(artifact0.ID, meta.ArtifactID)
		assert.Equal(artifact0.Name, meta.ArtifactName)
		assert.Equal(artifact0.ObjectKey, meta.ObjectKey)
	}

	// Case 2: the delete is idempotent - deleting an already deleted entry is a no-op, and
	// records nothing, since nothing was deleted the second time
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.DeleteArtifact(ctx, artifact0.ID)
		},
	))
	assert.Len(deleteEvents(), 1)

	// Case 3: deleting an artifact which never existed is likewise a silent no-op, not a
	// NotFoundError
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.DeleteArtifact(ctx, ulid.Make().String())
		},
	))
	assert.Len(deleteEvents(), 1)

	// Case 4: a quarantined artifact deletes too - this is the operator's disposal path for an
	// artifact whose object went missing (DESIGN §8.2.1)
	artifact4 := defineArtifact("unit-test-delete-quarantined")
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.MarkArtifactMissingObject(ctx, artifact4.ID)
		},
	))
	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.DeleteArtifact(ctx, artifact4.ID)
		},
	))
	{
		events := deleteEvents()
		assert.Len(events, 2)
		parsed, err := events[1].ParseMetadata(validate)
		assert.Nil(err)
		meta, ok := parsed.(models.SystemEventDeleteArtifact)
		assert.True(ok, "expected SystemEventDeleteArtifact, got %T", parsed)
		assert.Equal(artifact4.ID, meta.ArtifactID)
		// the quarantined row's key is preserved, even though it resolved to no object
		assert.Equal(artifact4.ObjectKey, meta.ObjectKey)
	}

	// Case 5: the name of a deleted artifact is free to be reused within the workspace
	artifact5 := defineArtifact("unit-test-delete-recorded")
	assert.NotEqual(artifact0.ID, artifact5.ID)

	// Case 6: deleting one artifact leaves its siblings, and its parent workspace, alone
	{
		sibling := defineArtifact("unit-test-delete-sibling")

		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.DeleteArtifact(ctx, artifact5.ID)
			},
		))

		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				readBack, err := dbClient.GetArtifact(ctx, sibling.ID)
				if err != nil {
					return err
				}
				assert.Equal(sibling.ObjectKey, readBack.ObjectKey)

				// the parent workspace is untouched - deleting an artifact never reaches upward
				parent, err := dbClient.GetWorkspace(ctx, workspace.ID)
				if err != nil {
					return err
				}
				assert.Equal(workspace.Name, parent.Name)
				assert.Equal(workspace.VolumeName, parent.VolumeName)
				return nil
			},
		))
		assert.Len(deleteEvents(), 3)
	}
}

func TestArtifactListArtifacts(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtx := context.Background()
	testDB := fmt.Sprintf("/tmp/cairn_ut_%s.db", ulid.Make().String())
	log.WithField("db", testDB).Debug("Test database")

	persistence := getUnitTestPersistence(utCtx, t, testDB)

	// Two parent workspaces, so the workspace scoping is actually exercised rather than being
	// trivially satisfied by a single-tenant fixture.
	workspaceA := defineUnitTestWorkspace(utCtx, t, persistence, "unit-test-list-parent-alpha")
	workspaceB := defineUnitTestWorkspace(utCtx, t, persistence, "unit-test-list-parent-bravo")

	// helper: run a listing and return it
	listArtifacts := func(filters db.ArtifactQueryFilter) []models.Artifact {
		var artifacts []models.Artifact
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				artifacts, err = dbClient.ListArtifacts(ctx, filters)
				return err
			},
		))
		return artifacts
	}

	// helper: collect the IDs of a listing, for order-insensitive comparison
	idsOf := func(artifacts []models.Artifact) []string {
		ids := []string{}
		for _, artifact := range artifacts {
			ids = append(ids, artifact.ID)
		}
		return ids
	}

	// helper: define an artifact under a given workspace
	defineArtifact := func(workspaceID, name string) models.Artifact {
		var artifact models.Artifact
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				artifact, err = dbClient.DefineNewArtifact(ctx, db.NewArtifactParameter{
					WorkspaceID: workspaceID,
					Name:        name,
					ObjectKey:   unitTestObjectKey(workspaceID),
					MIMEType:    "text/plain",
					Size:        32,
				})
				return err
			},
		))
		return artifact
	}

	// Case 0: an empty store lists nothing - and returns an empty slice, not nil
	{
		listing := listArtifacts(db.ArtifactQueryFilter{})
		assert.NotNil(listing)
		assert.Empty(listing)
	}

	// Define the fixture in a fixed sequence, so `created_at` / ULID ordering is the order
	// defined here. `one` and `three` share a name across the two workspaces, which is legal
	// since artifact names are unique only WITHIN a workspace.
	one := defineArtifact(workspaceA.ID, "unit-test-list-one")
	two := defineArtifact(workspaceA.ID, "unit-test-list-two")
	three := defineArtifact(workspaceB.ID, "unit-test-list-one")
	four := defineArtifact(workspaceB.ID, "unit-test-list-four")
	all := []models.Artifact{one, two, three, four}

	// `two` and `four` are quarantined, so both artifact states are represented in each of the
	// two workspaces.
	for _, artifact := range []models.Artifact{two, four} {
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.MarkArtifactMissingObject(ctx, artifact.ID)
			},
		))
	}

	// Case 1: no filter lists everything, across every workspace, in creation order
	{
		listing := listArtifacts(db.ArtifactQueryFilter{})
		assert.Len(listing, 4)
		assert.Equal(idsOf(all), idsOf(listing))
	}

	// Case 2: WorkspaceID alone scopes the listing to one parent
	{
		listing := listArtifacts(db.ArtifactQueryFilter{WorkspaceID: &workspaceA.ID})
		assert.Len(listing, 2)
		assert.ElementsMatch([]string{one.ID, two.ID}, idsOf(listing))

		listing = listArtifacts(db.ArtifactQueryFilter{WorkspaceID: &workspaceB.ID})
		assert.Len(listing, 2)
		assert.ElementsMatch([]string{three.ID, four.ID}, idsOf(listing))

		// a workspace with no artifacts lists nothing rather than erroring
		empty := defineUnitTestWorkspace(utCtx, t, persistence, "unit-test-list-parent-empty")
		assert.Empty(listArtifacts(db.ArtifactQueryFilter{WorkspaceID: &empty.ID}))
	}

	// Case 3: TargetIDs alone
	{
		listing := listArtifacts(db.ArtifactQueryFilter{
			TargetIDs: []string{one.ID, four.ID},
		})
		assert.Len(listing, 2)
		assert.ElementsMatch([]string{one.ID, four.ID}, idsOf(listing))

		// an ID matching nothing yields an empty listing rather than an error
		assert.Empty(listArtifacts(db.ArtifactQueryFilter{
			TargetIDs: []string{ulid.Make().String()},
		}))
	}

	// Case 4: TargetNames alone. The shared name spans both workspaces, so an unscoped name
	// listing returns an entry from each.
	{
		listing := listArtifacts(db.ArtifactQueryFilter{
			TargetNames: []string{"unit-test-list-one"},
		})
		assert.Len(listing, 2)
		assert.ElementsMatch([]string{one.ID, three.ID}, idsOf(listing))

		listing = listArtifacts(db.ArtifactQueryFilter{
			TargetNames: []string{"unit-test-list-two", "unit-test-list-four"},
		})
		assert.Len(listing, 2)
		assert.ElementsMatch([]string{two.ID, four.ID}, idsOf(listing))
	}

	// Case 5: ArtifactStates alone, each state in turn. The filter is a listing option rather
	// than a hardcoded default - leaving it empty returns every state, and the API layer is
	// what defaults it to RECORDED (DESIGN §7.1).
	{
		recorded := listArtifacts(db.ArtifactQueryFilter{
			ArtifactStates: []models.ArtifactStateENUM{models.ArtifactStateRecorded},
		})
		assert.Len(recorded, 2)
		assert.ElementsMatch([]string{one.ID, three.ID}, idsOf(recorded))

		missing := listArtifacts(db.ArtifactQueryFilter{
			ArtifactStates: []models.ArtifactStateENUM{models.ArtifactStateMissingObject},
		})
		assert.Len(missing, 2)
		assert.ElementsMatch([]string{two.ID, four.ID}, idsOf(missing))

		// both states named is equivalent to no state filter at all
		assert.Len(listArtifacts(db.ArtifactQueryFilter{
			ArtifactStates: []models.ArtifactStateENUM{
				models.ArtifactStateRecorded, models.ArtifactStateMissingObject,
			},
		}), 4)
	}

	// Case 6: an unrecognized artifact state is rejected by the `artifact_state` macro
	{
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				_, err := dbClient.ListArtifacts(ctx, db.ArtifactQueryFilter{
					ArtifactStates: []models.ArtifactStateENUM{"NOT_A_STATE"},
				})
				return err
			},
		)
		assert.NotNil(err)
		var validationError goutils.ValidationError
		assert.True(errors.As(err, &validationError), "expected ValidationError, got %T", err)
	}

	// Case 7: ObjectKeys alone - the hook backing the object-reaping GC's "which of these keys
	// still has a backing row" question (DESIGN §8.3.1)
	{
		listing := listArtifacts(db.ArtifactQueryFilter{
			ObjectKeys: []string{one.ObjectKey, three.ObjectKey},
		})
		assert.Len(listing, 2)
		assert.ElementsMatch([]string{one.ID, three.ID}, idsOf(listing))

		// a key with no backing row - exactly what the reaper is looking for - lists nothing
		assert.Empty(listArtifacts(db.ArtifactQueryFilter{
			ObjectKeys: []string{unitTestObjectKey(workspaceA.ID)},
		}))
	}

	// Case 8: WorkspaceID paired with ArtifactStates - the conditions intersect
	{
		listing := listArtifacts(db.ArtifactQueryFilter{
			WorkspaceID:    &workspaceA.ID,
			ArtifactStates: []models.ArtifactStateENUM{models.ArtifactStateMissingObject},
		})
		assert.Len(listing, 1)
		assert.Equal(two.ID, listing[0].ID)

		// the same workspace narrowed to the other state picks its sibling
		listing = listArtifacts(db.ArtifactQueryFilter{
			WorkspaceID:    &workspaceA.ID,
			ArtifactStates: []models.ArtifactStateENUM{models.ArtifactStateRecorded},
		})
		assert.Len(listing, 1)
		assert.Equal(one.ID, listing[0].ID)
	}

	// Case 9: WorkspaceID paired with TargetNames - this is how the shared name is
	// disambiguated back to a single entry
	{
		listing := listArtifacts(db.ArtifactQueryFilter{
			WorkspaceID: &workspaceA.ID,
			TargetNames: []string{"unit-test-list-one"},
		})
		assert.Len(listing, 1)
		assert.Equal(one.ID, listing[0].ID)

		listing = listArtifacts(db.ArtifactQueryFilter{
			WorkspaceID: &workspaceB.ID,
			TargetNames: []string{"unit-test-list-one"},
		})
		assert.Len(listing, 1)
		assert.Equal(three.ID, listing[0].ID)

		// a name which exists, but not in the named workspace, drops out entirely
		assert.Empty(listArtifacts(db.ArtifactQueryFilter{
			WorkspaceID: &workspaceA.ID,
			TargetNames: []string{"unit-test-list-four"},
		}))
	}

	// Case 10: TargetIDs paired with ArtifactStates - an ID whose state does not match the
	// state filter drops out
	{
		listing := listArtifacts(db.ArtifactQueryFilter{
			TargetIDs:      []string{one.ID, two.ID},
			ArtifactStates: []models.ArtifactStateENUM{models.ArtifactStateRecorded},
		})
		assert.Len(listing, 1)
		assert.Equal(one.ID, listing[0].ID)

		assert.Empty(listArtifacts(db.ArtifactQueryFilter{
			TargetIDs:      []string{two.ID},
			ArtifactStates: []models.ArtifactStateENUM{models.ArtifactStateRecorded},
		}))
	}

	// Case 11: WorkspaceID paired with ObjectKeys - a key belonging to the other workspace
	// matches neither condition
	{
		listing := listArtifacts(db.ArtifactQueryFilter{
			WorkspaceID: &workspaceA.ID,
			ObjectKeys:  []string{one.ObjectKey, three.ObjectKey},
		})
		assert.Len(listing, 1)
		assert.Equal(one.ID, listing[0].ID)
	}

	// Case 12: TargetIDs paired with TargetNames - both conditions must hold, so an ID and a
	// name belonging to different entries match neither
	{
		listing := listArtifacts(db.ArtifactQueryFilter{
			TargetIDs:   []string{one.ID},
			TargetNames: []string{one.Name},
		})
		assert.Len(listing, 1)
		assert.Equal(one.ID, listing[0].ID)

		assert.Empty(listArtifacts(db.ArtifactQueryFilter{
			TargetIDs:   []string{one.ID},
			TargetNames: []string{two.Name},
		}))
	}

	// Case 13: pagination walks the full listing in creation order without gaps or repeats
	{
		limit := 3
		firstPage := listArtifacts(db.ArtifactQueryFilter{
			CommonListEntryQueryFilter: db.CommonListEntryQueryFilter{Limit: &limit},
		})
		assert.Len(firstPage, 3)
		assert.Equal(idsOf(all[:3]), idsOf(firstPage))

		offset := 3
		secondPage := listArtifacts(db.ArtifactQueryFilter{
			CommonListEntryQueryFilter: db.CommonListEntryQueryFilter{
				Limit: &limit, Offset: &offset,
			},
		})
		assert.Len(secondPage, 1)
		assert.Equal(idsOf(all[3:]), idsOf(secondPage))

		// walking past the end is empty, not an error
		offset = 4
		assert.Empty(listArtifacts(db.ArtifactQueryFilter{
			CommonListEntryQueryFilter: db.CommonListEntryQueryFilter{
				Limit: &limit, Offset: &offset,
			},
		}))
	}

	// Case 14: pagination composes with the other filters, paging the filtered set
	{
		limit := 1
		offset := 1
		listing := listArtifacts(db.ArtifactQueryFilter{
			CommonListEntryQueryFilter: db.CommonListEntryQueryFilter{
				Limit: &limit, Offset: &offset,
			},
			ArtifactStates: []models.ArtifactStateENUM{models.ArtifactStateRecorded},
		})
		assert.Len(listing, 1)
		// the RECORDED set is `one` then `three`, so offset 1 is `three`
		assert.Equal(three.ID, listing[0].ID)
	}

	// Case 15: the pagination bounds are validated
	{
		badLimit := 0
		err := persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				_, err := dbClient.ListArtifacts(ctx, db.ArtifactQueryFilter{
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
				_, err := dbClient.ListArtifacts(ctx, db.ArtifactQueryFilter{
					CommonListEntryQueryFilter: db.CommonListEntryQueryFilter{Offset: &badOffset},
				})
				return err
			},
		)
		assert.NotNil(err)
		assert.True(errors.As(err, &validationError), "expected ValidationError, got %T", err)
	}

	// Case 16: a deleted artifact drops out of the listing
	{
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				return dbClient.DeleteArtifact(ctx, two.ID)
			},
		))
		listing := listArtifacts(db.ArtifactQueryFilter{})
		assert.Len(listing, 3)
		assert.NotContains(idsOf(listing), two.ID)
	}

	// Case 17: listing records no audit event - the only events written were by the fixture
	{
		var events []models.SystemEventAudit
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				events, err = dbClient.ListSystemEvents(ctx, db.SystemEventQueryFilter{})
				return err
			},
		))
		// 3 NEW_WORKSPACE, 4 NEW_ARTIFACT, 2 ARTIFACT_MISSING_OBJECT, 1 DELETE_ARTIFACT
		assert.Len(events, 10)
	}
}

func TestArtifactListWorkspaceArtifacts(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtx := context.Background()
	testDB := fmt.Sprintf("/tmp/cairn_ut_%s.db", ulid.Make().String())
	log.WithField("db", testDB).Debug("Test database")

	persistence := getUnitTestPersistence(utCtx, t, testDB)

	workspaceA := defineUnitTestWorkspace(utCtx, t, persistence, "unit-test-ws-list-alpha")
	workspaceB := defineUnitTestWorkspace(utCtx, t, persistence, "unit-test-ws-list-bravo")

	// helper: define an artifact under a given workspace
	defineArtifact := func(workspaceID, name string) models.Artifact {
		var artifact models.Artifact
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				artifact, err = dbClient.DefineNewArtifact(ctx, db.NewArtifactParameter{
					WorkspaceID: workspaceID,
					Name:        name,
					ObjectKey:   unitTestObjectKey(workspaceID),
					MIMEType:    "text/plain",
					Size:        32,
				})
				return err
			},
		))
		return artifact
	}

	// helper: run a workspace-scoped listing and return it
	listWorkspaceArtifacts := func(
		workspaceID string, filters db.ArtifactQueryFilter,
	) []models.Artifact {
		var artifacts []models.Artifact
		assert.Nil(persistence.UseDatabaseInTransaction(
			utCtx, func(ctx context.Context, dbClient db.Database) error {
				var err error
				artifacts, err = dbClient.ListWorkspaceArtifacts(ctx, workspaceID, filters)
				return err
			},
		))
		return artifacts
	}

	// helper: collect the IDs of a listing
	idsOf := func(artifacts []models.Artifact) []string {
		ids := []string{}
		for _, artifact := range artifacts {
			ids = append(ids, artifact.ID)
		}
		return ids
	}

	one := defineArtifact(workspaceA.ID, "unit-test-ws-list-one")
	two := defineArtifact(workspaceA.ID, "unit-test-ws-list-two")
	three := defineArtifact(workspaceB.ID, "unit-test-ws-list-three")

	assert.Nil(persistence.UseDatabaseInTransaction(
		utCtx, func(ctx context.Context, dbClient db.Database) error {
			return dbClient.MarkArtifactMissingObject(ctx, two.ID)
		},
	))

	// Case 0: the workspace scoping is applied
	{
		listing := listWorkspaceArtifacts(workspaceA.ID, db.ArtifactQueryFilter{})
		assert.Len(listing, 2)
		assert.Equal([]string{one.ID, two.ID}, idsOf(listing))

		listing = listWorkspaceArtifacts(workspaceB.ID, db.ArtifactQueryFilter{})
		assert.Len(listing, 1)
		assert.Equal(three.ID, listing[0].ID)
	}

	// Case 1: the remaining filters still apply on top of the scoping
	{
		listing := listWorkspaceArtifacts(workspaceA.ID, db.ArtifactQueryFilter{
			ArtifactStates: []models.ArtifactStateENUM{models.ArtifactStateRecorded},
		})
		assert.Len(listing, 1)
		assert.Equal(one.ID, listing[0].ID)
	}

	// Case 2: the workspace argument overrides any WorkspaceID already on the filter, so a
	// caller can't reach outside the workspace it named
	{
		listing := listWorkspaceArtifacts(workspaceA.ID, db.ArtifactQueryFilter{
			WorkspaceID: &workspaceB.ID,
		})
		assert.Len(listing, 2)
		assert.ElementsMatch([]string{one.ID, two.ID}, idsOf(listing))
	}

	// Case 3: an unknown workspace lists nothing rather than erroring
	assert.Empty(listWorkspaceArtifacts(uuid.NewString(), db.ArtifactQueryFilter{}))
}
