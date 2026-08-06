package maintenance_test

import (
	"testing"

	"github.com/alwitt/cairn/maintenance"
	mockdb "github.com/alwitt/cairn/mocks/db"
	"github.com/alwitt/cairn/models"
	mockgoutils "github.com/alwitt/goutils/mocks/goutils"
	mockruntime "github.com/alwitt/goutils/mocks/runtime"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// unitTestAppName the application name the harness manager is built with. It forms the prefix
// reconciliation lists volumes on (see DESIGN §2.1), so tests assert it reaches Docker.
const unitTestAppName = "unit-test-app"

// unitTestBucket the bucket the harness manager reconciles against.
const unitTestBucket = "unit-test-bucket"

// unitTestStoreConfig a valid artifact storage config, the shape NewManager must accept.
func unitTestStoreConfig() models.ArtifactStorageConfig {
	return models.ArtifactStorageConfig{
		Bucket:                   unitTestBucket,
		UploadPutURLTTLSecs:      300,
		DownloadGetURLMaxTTLSecs: 600,
		MaxObjectSizeBytes:       1024 * 1024,
		Prefix:                   models.ArtifactKeyConfig{StagingPrefix: "staging", StorePrefix: "store"},
	}
}

// unitTestMaintenanceConfig a valid maintenance config, the shape NewManager must accept.
func unitTestMaintenanceConfig() models.MaintenanceConfig {
	return models.MaintenanceConfig{
		MaintenanceSweepIntSec:  60,
		OrphanedObjectAgeOutSec: 3600,
	}
}

// unitTestMocks the mocked dependencies a harness manager is built over. Every one fails the
// test on an unarranged call, which is how several cases assert that something never happened.
type unitTestMocks struct {
	client   *mockdb.Client
	database *mockdb.Database
	volumes  *mockruntime.VolumeManager
	s3       *mockgoutils.S3ClientManager
	objects  *mockgoutils.S3Client
}

// newUnitTestManager build a Manager over mocked persistence, volume, and object store
// dependencies, returning the mocks so a test can set expectations on any of them.
func newUnitTestManager(t *testing.T) (maintenance.Manager, unitTestMocks) {
	assert := assert.New(t)

	mocks := unitTestMocks{
		client:   mockdb.NewClient(t),
		database: mockdb.NewDatabase(t),
		volumes:  mockruntime.NewVolumeManager(t),
		s3:       mockgoutils.NewS3ClientManager(t),
		objects:  mockgoutils.NewS3Client(t),
	}

	// The manager acquires a client per object store call rather than holding one, so this is
	// arranged unconditionally and left uncounted; a sweep that touches no object store at all
	// simply never asks for it.
	mocks.s3.EXPECT().
		GetClient(mock.Anything, mock.Anything).
		Return(mocks.objects, nil).
		Maybe()

	manager, err := maintenance.NewManager(
		unitTestAppName,
		mocks.client,
		mocks.s3,
		unitTestStoreConfig(),
		unitTestMaintenanceConfig(),
		mocks.volumes,
	)
	assert.Nil(err)
	assert.NotNil(manager)

	return manager, mocks
}

// sampleWorkspace build a workspace entry of the shape persistence returns, with the volume
// name derived the way DefineNewWorkspace derives it (see DESIGN §2.1).
func sampleWorkspace(name string, state models.WorkspaceVolumeStateENUM) models.Workspace {
	workspaceID := uuid.NewString()
	return models.Workspace{
		ID:          workspaceID,
		Name:        name,
		VolumeName:  models.WorkspaceVolumeName(unitTestAppName, workspaceID),
		VolumeState: state,
	}
}

// TestNewManager validates that a maintenance manager refuses to be built over dependencies or
// configuration it cannot work with. Every check here is a failure moved from the first sweep -
// which runs unattended on a timer - to the wiring site, where it is visible.
func TestNewManager(t *testing.T) {
	// Case 1: the full valid set builds.
	t.Run("accepts a valid dependency set", func(t *testing.T) {
		newUnitTestManager(t)
	})

	// Case 2: the application name forms the prefix the sweep lists volumes on, so it is held
	// to the same charset the volume names it must match were built under. A name that can't
	// produce a matching prefix would silently reconcile against nothing.
	t.Run("rejects an invalid application name", func(t *testing.T) {
		assert := assert.New(t)

		for _, badName := range []string{"", "has spaces", "has/slash"} {
			manager, err := maintenance.NewManager(
				badName,
				mockdb.NewClient(t),
				mockgoutils.NewS3ClientManager(t),
				unitTestStoreConfig(),
				unitTestMaintenanceConfig(),
				mockruntime.NewVolumeManager(t),
			)
			assert.Nil(manager)
			assert.NotNil(err, "application name '%s' should be rejected", badName)
		}
	})

	// Case 3: each dependency is required. None has a sensible zero value - a sweep with no
	// persistence, no object store, or no volume view has nothing to reconcile.
	t.Run("rejects a missing dependency", func(t *testing.T) {
		assert := assert.New(t)

		manager, err := maintenance.NewManager(
			unitTestAppName,
			nil,
			mockgoutils.NewS3ClientManager(t),
			unitTestStoreConfig(),
			unitTestMaintenanceConfig(),
			mockruntime.NewVolumeManager(t),
		)
		assert.Nil(manager)
		assert.NotNil(err)

		manager, err = maintenance.NewManager(
			unitTestAppName,
			mockdb.NewClient(t),
			nil,
			unitTestStoreConfig(),
			unitTestMaintenanceConfig(),
			mockruntime.NewVolumeManager(t),
		)
		assert.Nil(manager)
		assert.NotNil(err)

		manager, err = maintenance.NewManager(
			unitTestAppName,
			mockdb.NewClient(t),
			mockgoutils.NewS3ClientManager(t),
			unitTestStoreConfig(),
			unitTestMaintenanceConfig(),
			nil,
		)
		assert.Nil(manager)
		assert.NotNil(err)
	})

	// Case 4: an incomplete storage config fails here rather than partway through a sweep that
	// has already begun deleting.
	t.Run("rejects an invalid storage config", func(t *testing.T) {
		assert := assert.New(t)

		broken := unitTestStoreConfig()
		broken.Bucket = ""

		manager, err := maintenance.NewManager(
			unitTestAppName,
			mockdb.NewClient(t),
			mockgoutils.NewS3ClientManager(t),
			broken,
			unitTestMaintenanceConfig(),
			mockruntime.NewVolumeManager(t),
		)
		assert.Nil(manager)
		assert.NotNil(err)
	})

	// Case 5: the grace window is load-bearing - it is what separates an in-flight upload from
	// an orphan (see DESIGN §8.2.1) - so a zero one is rejected rather than defaulted. A zero
	// window would read every in-flight upload as garbage.
	t.Run("rejects an invalid maintenance config", func(t *testing.T) {
		assert := assert.New(t)

		for _, broken := range []models.MaintenanceConfig{
			{MaintenanceSweepIntSec: 0, OrphanedObjectAgeOutSec: 3600},
			{MaintenanceSweepIntSec: 60, OrphanedObjectAgeOutSec: 0},
		} {
			manager, err := maintenance.NewManager(
				unitTestAppName,
				mockdb.NewClient(t),
				mockgoutils.NewS3ClientManager(t),
				unitTestStoreConfig(),
				broken,
				mockruntime.NewVolumeManager(t),
			)
			assert.Nil(manager)
			assert.NotNil(err, "maintenance config %+v should be rejected", broken)
		}
	})
}
