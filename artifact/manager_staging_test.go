package artifact_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alwitt/cairn/artifact"
	"github.com/alwitt/cairn/db"
	mockdb "github.com/alwitt/cairn/mocks/db"
	mocktest "github.com/alwitt/cairn/mocks/test"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	mockgoutils "github.com/alwitt/goutils/mocks/goutils"
	"github.com/apex/log"
	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// ======================================================================================
// Test harness

const (
	// unitTestAppName the application name the harness manager is built with
	unitTestAppName = "unit-test-app"
	// unitTestBucket the bucket the harness manager reads and writes
	unitTestBucket = "unit-test-bucket"
	// unitTestStagingPrefix the staging key prefix the harness manager is configured with
	unitTestStagingPrefix = "staging"
	// unitTestStorePrefix the storage key prefix the harness manager is configured with
	unitTestStorePrefix = "store"
	// unitTestPutURLTTLSec the staging PUT URL TTL the harness manager is configured with
	unitTestPutURLTTLSec = 300
	// unitTestGetURLMaxTTLSec the download GET URL TTL ceiling the harness manager is
	// configured with
	unitTestGetURLMaxTTLSec = 600
	// unitTestMaxObjectSize the single-PUT size cap the harness manager is configured with
	unitTestMaxObjectSize int64 = 4096
)

// unitTestStoreConfig build the storage config the harness manager is constructed with.
func unitTestStoreConfig() models.ArtifactStorageConfig {
	return models.ArtifactStorageConfig{
		Bucket:                   unitTestBucket,
		UploadPutURLTTLSecs:      unitTestPutURLTTLSec,
		DownloadGetURLMaxTTLSecs: unitTestGetURLMaxTTLSec,
		MaxObjectSizeBytes:       unitTestMaxObjectSize,
		Prefix: models.ArtifactKeyConfig{
			StagingPrefix: unitTestStagingPrefix,
			StorePrefix:   unitTestStorePrefix,
		},
	}
}

// unitTestManagerMocks the collaborators the harness manager is built over, so a test can set
// expectations on whichever ones its case exercises.
type unitTestManagerMocks struct {
	persistence *mockdb.Client
	s3          *mockgoutils.S3Client
	s3Manager   *mockgoutils.S3ClientManager
	callbacks   *mocktest.UnitTestCallbackCollector
}

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

// newUnitTestManagerCore build a Manager over mocked persistence, object store client manager,
// and MIME detector. No expectations are set at all - including on the client manager - so a
// case can arrange object store client acquisition itself. Use `newUnitTestManager` unless the
// case is specifically about acquisition.
func newUnitTestManagerCore(t *testing.T) (artifact.Manager, unitTestManagerMocks) {
	assert := assert.New(t)

	mocks := unitTestManagerMocks{
		persistence: mockdb.NewClient(t),
		s3:          mockgoutils.NewS3Client(t),
		s3Manager:   mockgoutils.NewS3ClientManager(t),
		callbacks:   mocktest.NewUnitTestCallbackCollector(t),
	}

	manager, err := artifact.NewManager(
		unitTestAppName,
		mocks.persistence,
		mocks.s3Manager,
		unitTestStoreConfig(),
		mocks.callbacks.EstimateMIMEType,
	)
	assert.Nil(err)
	assert.NotNil(manager)

	return manager, mocks
}

// newUnitTestManager build a Manager as `newUnitTestManagerCore` does, with the client manager
// arranged to hand out the mock object store client on demand. Beyond that no expectations are
// set, so any call a case did not arrange for fails that case.
//
// The acquisition arrangement is optional (`Maybe`) because the manager acquires a client per
// object store call, so a case which never reaches the object store never acquires one - and a
// required expectation would fail it at cleanup.
func newUnitTestManager(t *testing.T) (artifact.Manager, unitTestManagerMocks) {
	manager, mocks := newUnitTestManagerCore(t)

	mocks.s3Manager.EXPECT().
		GetClient(mock.Anything, mock.Anything).
		Return(mocks.s3, nil).
		Maybe()

	return manager, mocks
}

// sampleWorkspace build a workspace entry of the shape persistence returns.
func sampleWorkspace(name string) models.Workspace {
	workspaceID := uuid.NewString()
	return models.Workspace{
		ID:          workspaceID,
		Name:        name,
		VolumeName:  fmt.Sprintf("%s-%s", unitTestAppName, workspaceID),
		VolumeState: models.WorkspaceVolumeStateReady,
	}
}

// stagingKeyFor build a staging object key of the shape the manager mints for a workspace, so a
// test can supply one that passes the ownership check without having minted it.
func stagingKeyFor(workspace models.Workspace) string {
	return fmt.Sprintf(
		"%s/%s/%s", unitTestStagingPrefix, workspace.ID, ulid.Make().String(),
	)
}

// fakeObjectReader a seekable, closeable reader over fixed bytes, standing in for the object
// body GetObject hands back. A real reader rather than a mock, because the code under test does
// genuine bounded reads over it and the point is to observe how much it takes.
type fakeObjectReader struct {
	*bytes.Reader
	closed   bool
	closeErr error
}

func newFakeObjectReader(content []byte) *fakeObjectReader {
	return &fakeObjectReader{Reader: bytes.NewReader(content)}
}

func (r *fakeObjectReader) Close() error {
	r.closed = true
	return r.closeErr
}

// assertManagerError verify an error is an ArtifactMangerError that still carries the original
// failure at the top of the chain.
//
// The innermost error is matched on its rendered message rather than with `errors.Is`: the
// goutils constructors take the core error by value, so the chain carries a copy of it and
// identity comparison would never match.
func assertManagerError(assert *assert.Assertions, err error, wrapped error) {
	assert.NotNil(err)

	var managerErr models.ArtifactMangerError
	assert.True(
		errors.As(err, &managerErr), "expected ArtifactMangerError, got %T: %v", err, err,
	)

	assert.Contains(
		err.Error(), wrapped.Error(), "wrapped error should survive to the top of the chain",
	)
}

// assertBadInputError verify an error is an ArtifactMangerError wrapping a BadInputError. The
// manager reports caller-supplied violations this way so the API layer can map them onto a 4xx.
func assertBadInputError(assert *assert.Assertions, err error) {
	assert.NotNil(err)

	var managerErr models.ArtifactMangerError
	assert.True(
		errors.As(err, &managerErr), "expected ArtifactMangerError, got %T: %v", err, err,
	)

	var badInputErr goutils.BadInputError
	assert.True(errors.As(err, &badInputErr), "expected BadInputError, got %T: %v", err, err)
}

// ======================================================================================
// GetArtifactStagingPutURL

// TestManagerGetArtifactStagingPutURL validates the staging PUT URL mint: the key it defines,
// the parameters it binds into the URL, the mint-time size cap, and that it never touches the DB.
func TestManagerGetArtifactStagingPutURL(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("mints URL against a workspace scoped staging key", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		presigned, err := url.Parse("https://s3.unit-test/staged?signature=abc")
		assert.Nil(err)

		var mintedKey string
		mocks.s3.EXPECT().
			GeneratePresignedPutURL(
				mock.Anything,
				unitTestBucket,
				mock.Anything,
				int64(1024),
				"c2FtcGxlLWNoZWNrc3Vt",
				time.Second*unitTestPutURLTTLSec,
				(*string)(nil),
			).
			Run(func(
				_ context.Context, _ string, objectKey string, _ int64, _ string,
				_ time.Duration, _ *string,
			) {
				mintedKey = objectKey
			}).
			Return(presigned, nil).
			Once()

		bundle, err := manager.GetArtifactStagingPutURL(
			context.Background(), workspace, 1024, "c2FtcGxlLWNoZWNrc3Vt", nil,
		)

		assert.Nil(err)
		assert.Equal(presigned.String(), bundle.PutURL)
		assert.Equal(mintedKey, bundle.StagingObjectKey)

		// The key must land under this workspace's staging prefix, which is what later lets
		// RegisterNewArtifact prove ownership by prefix match alone (see DESIGN §8.1).
		assert.True(
			strings.HasPrefix(
				bundle.StagingObjectKey,
				fmt.Sprintf("%s/%s/", unitTestStagingPrefix, workspace.ID),
			),
			"staging key '%s' must be scoped to workspace %s",
			bundle.StagingObjectKey, workspace.ID,
		)
		// The suffix is server-generated, so it carries nothing the caller chose.
		assert.NotContains(bundle.StagingObjectKey, workspace.Name)
	})

	t.Run("signs the content type when one is supplied", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		presigned, err := url.Parse("https://s3.unit-test/staged")
		assert.Nil(err)
		contentType := "application/pdf"

		mocks.s3.EXPECT().
			GeneratePresignedPutURL(
				mock.Anything,
				unitTestBucket,
				mock.Anything,
				int64(64),
				"c2Vjb25kLWNoZWNrc3Vt",
				time.Second*unitTestPutURLTTLSec,
				&contentType,
			).
			Return(presigned, nil).
			Once()

		_, err = manager.GetArtifactStagingPutURL(
			context.Background(), workspace, 64, "c2Vjb25kLWNoZWNrc3Vt", &contentType,
		)

		assert.Nil(err)
	})

	t.Run("mints two distinct keys across calls", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		presigned, err := url.Parse("https://s3.unit-test/staged")
		assert.Nil(err)

		mocks.s3.EXPECT().
			GeneratePresignedPutURL(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything, mock.Anything,
			).
			Return(presigned, nil).
			Twice()

		first, err := manager.GetArtifactStagingPutURL(
			context.Background(), workspace, 16, "Zmlyc3Q=", nil,
		)
		assert.Nil(err)
		second, err := manager.GetArtifactStagingPutURL(
			context.Background(), workspace, 16, "c2Vjb25k", nil,
		)
		assert.Nil(err)

		// Two uploads must never collide on one staging object.
		assert.NotEqual(first.StagingObjectKey, second.StagingObjectKey)
	})

	t.Run("rejects an over cap size before minting", func(t *testing.T) {
		assert := assert.New(t)
		manager, _ := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")

		// No object store expectation is set: the cap must fail fast, before any mint. An
		// unexpected GeneratePresignedPutURL call would fail this case (see DESIGN §5.2).
		bundle, err := manager.GetArtifactStagingPutURL(
			context.Background(),
			workspace,
			unitTestMaxObjectSize+1,
			"b3Zlci1jYXA=",
			nil,
		)

		assertBadInputError(assert, err)
		assert.Equal(artifact.StagingUploadBundle{}, bundle)
	})

	t.Run("accepts a size exactly at the cap", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		presigned, err := url.Parse("https://s3.unit-test/staged")
		assert.Nil(err)

		mocks.s3.EXPECT().
			GeneratePresignedPutURL(
				mock.Anything, mock.Anything, mock.Anything, unitTestMaxObjectSize,
				mock.Anything, mock.Anything, mock.Anything,
			).
			Return(presigned, nil).
			Once()

		// The cap is inclusive - an object of exactly the maximum size is still a single PUT.
		_, err = manager.GetArtifactStagingPutURL(
			context.Background(), workspace, unitTestMaxObjectSize, "YXQtY2Fw", nil,
		)

		assert.Nil(err)
	})

	t.Run("acquires an object store client with the current timestamp", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManagerCore(t)

		workspace := sampleWorkspace("test-workspace")
		presigned, err := url.Parse("https://s3.unit-test/staged?signature=abc")
		assert.Nil(err)

		// A client is acquired per call rather than held, and the client manager ages one out
		// against the timestamp it is given - so a zero or stale one would quietly defeat the
		// TTL that exists to replace a client that has lost its connection.
		var acquiredAt time.Time
		mocks.s3Manager.EXPECT().
			GetClient(mock.Anything, mock.Anything).
			Run(func(_ context.Context, timestamp time.Time) {
				acquiredAt = timestamp
			}).
			Return(mocks.s3, nil).
			Once()

		mocks.s3.EXPECT().
			GeneratePresignedPutURL(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything, mock.Anything,
			).
			Return(presigned, nil).
			Once()

		_, err = manager.GetArtifactStagingPutURL(
			context.Background(), workspace, 32, "c3VjY2Vzcw==", nil,
		)

		assert.Nil(err)
		assert.WithinDuration(time.Now().UTC(), acquiredAt, time.Minute)
	})

	t.Run("surfaces an object store client acquisition failure", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManagerCore(t)

		acquireErr := fmt.Errorf("object store client unavailable")

		// No client to mint with, so no object store expectation: the failure surfaces from
		// the acquisition itself, wrapped the same way a mint failure would be.
		mocks.s3Manager.EXPECT().
			GetClient(mock.Anything, mock.Anything).
			Return(nil, acquireErr).
			Once()

		bundle, err := manager.GetArtifactStagingPutURL(
			context.Background(), sampleWorkspace("test-workspace"), 32, "ZmFpbA==", nil,
		)

		assertManagerError(assert, err, acquireErr)
		assert.Equal(artifact.StagingUploadBundle{}, bundle)
	})

	t.Run("surfaces a mint failure", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		storeErr := fmt.Errorf("presign rejected")

		mocks.s3.EXPECT().
			GeneratePresignedPutURL(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything, mock.Anything,
			).
			Return(nil, storeErr).
			Once()

		bundle, err := manager.GetArtifactStagingPutURL(
			context.Background(), workspace, 32, "ZmFpbA==", nil,
		)

		assertManagerError(assert, err, storeErr)
		assert.Equal(artifact.StagingUploadBundle{}, bundle)
	})
}

// ======================================================================================
// RegisterNewArtifact

// registerFixture the arrangement a successful registration needs, so each case can vary the one
// step it is about rather than restating the whole chain.
type registerFixture struct {
	workspace     models.Workspace
	stagingObjKey string
	content       []byte
	mimeType      string
	reader        *fakeObjectReader
	mockDatabase  *mockdb.Database
}

// expectSuccessfulRegistration arrange the full happy-path chain: stat, sniff, copy, insert, and
// the best-effort staging delete. `copiedKey` receives the final key the copy targeted.
func expectSuccessfulRegistration(
	t *testing.T, mocks unitTestManagerMocks, copiedKey *string,
) registerFixture {
	fixture := registerFixture{
		workspace: sampleWorkspace("test-workspace"),
		content:   []byte("unit test artifact body"),
		mimeType:  "text/plain; charset=utf-8",
	}
	fixture.stagingObjKey = stagingKeyFor(fixture.workspace)
	fixture.reader = newFakeObjectReader(fixture.content)
	fixture.mockDatabase = mockdb.NewDatabase(t)

	mocks.s3.EXPECT().
		GetObjectStat(mock.Anything, unitTestBucket, fixture.stagingObjKey).
		Return(goutils.S3ObjectStat{Size: int64(len(fixture.content))}, nil).
		Once()

	mocks.s3.EXPECT().
		GetObject(mock.Anything, unitTestBucket, fixture.stagingObjKey).
		Return(goutils.S3ObjectStat{}, fixture.reader, nil).
		Once()

	mocks.callbacks.EXPECT().
		EstimateMIMEType(mock.Anything).
		Return(fixture.mimeType).
		Once()

	mocks.s3.EXPECT().
		CopyObject(
			mock.Anything,
			unitTestBucket,
			fixture.stagingObjKey,
			unitTestBucket,
			mock.Anything,
			&fixture.mimeType,
		).
		Run(func(_ context.Context, _ string, _ string, _ string, dstKey string, _ *string) {
			if copiedKey != nil {
				*copiedKey = dstKey
			}
		}).
		Return(nil).
		Once()

	mocks.s3.EXPECT().
		DeleteObject(mock.Anything, unitTestBucket, fixture.stagingObjKey).
		Return(nil).
		Once()

	return fixture
}

// TestManagerRegisterNewArtifact validates the register path: the staging-key ownership guard,
// the authoritative size cap, the server-side sniff, and the copy-then-insert ordering.
func TestManagerRegisterNewArtifact(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("promotes a staged object and records the entry", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		var copiedKey string
		fixture := expectSuccessfulRegistration(t, mocks, &copiedKey)
		description := "a described artifact"
		expected := models.Artifact{
			ID:          ulid.Make().String(),
			WorkspaceID: fixture.workspace.ID,
			Name:        "report-txt",
			State:       models.ArtifactStateRecorded,
		}

		var recorded db.NewArtifactParameter
		fixture.mockDatabase.EXPECT().
			DefineNewArtifact(mock.Anything, mock.Anything).
			Run(func(_ context.Context, params db.NewArtifactParameter) {
				recorded = params
			}).
			Return(expected, nil).
			Once()

		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(fixture.mockDatabase)).
			Once()

		entry, err := manager.RegisterNewArtifact(
			context.Background(),
			fixture.workspace,
			fixture.stagingObjKey,
			"report-txt",
			&description,
			nil,
		)

		assert.Nil(err)
		assert.Equal(expected, entry)

		// The entry must describe the object that was actually copied, measured server-side -
		// not anything the caller asserted.
		assert.Equal(fixture.workspace.ID, recorded.WorkspaceID)
		assert.Equal("report-txt", recorded.Name)
		assert.Equal(&description, recorded.Description)
		assert.Equal(copiedKey, recorded.ObjectKey)
		assert.Equal(fixture.mimeType, recorded.MIMEType)
		assert.Equal(int64(len(fixture.content)), recorded.Size)

		// The final key is a fresh key under the storage prefix, never the staging key.
		assert.True(
			strings.HasPrefix(
				recorded.ObjectKey,
				fmt.Sprintf("%s/%s/", unitTestStorePrefix, fixture.workspace.ID),
			),
			"final key '%s' must be scoped to workspace %s",
			recorded.ObjectKey, fixture.workspace.ID,
		)
		assert.NotEqual(fixture.stagingObjKey, recorded.ObjectKey)
	})

	t.Run("sniffs only the leading bytes and closes the body", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		stagingObjKey := stagingKeyFor(workspace)
		// Larger than the detection window, so a full read would be observable.
		content := bytes.Repeat([]byte("x"), int(unitTestMaxObjectSize))
		reader := newFakeObjectReader(content)
		mimeType := "application/octet-stream"

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{Size: int64(len(content))}, nil).
			Once()
		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{}, reader, nil).
			Once()

		var sniffed []byte
		mocks.callbacks.EXPECT().
			EstimateMIMEType(mock.Anything).
			Run(func(data []byte) { sniffed = data }).
			Return(mimeType).
			Once()

		mocks.s3.EXPECT().
			CopyObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				&mimeType,
			).
			Return(nil).
			Once()
		mocks.s3.EXPECT().
			DeleteObject(mock.Anything, mock.Anything, mock.Anything).
			Return(nil).
			Once()

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			DefineNewArtifact(mock.Anything, mock.Anything).
			Return(models.Artifact{ID: ulid.Make().String()}, nil).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		_, err := manager.RegisterNewArtifact(
			context.Background(), workspace, stagingObjKey, "big-blob", nil, nil,
		)

		assert.Nil(err)

		// The read is bounded at the detection window, so the rest of a large object never
		// transits just to identify its type.
		assert.Len(sniffed, 3072)
		assert.Less(len(sniffed), len(content))
		assert.True(reader.closed, "the object body must be closed after the sniff")
	})

	t.Run("sniffs a short object in full", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		var copiedKey string
		fixture := expectSuccessfulRegistration(t, mocks, &copiedKey)

		mockDatabase := fixture.mockDatabase
		mockDatabase.EXPECT().
			DefineNewArtifact(mock.Anything, mock.Anything).
			Return(models.Artifact{ID: ulid.Make().String()}, nil).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		_, err := manager.RegisterNewArtifact(
			context.Background(), fixture.workspace, fixture.stagingObjKey, "short", nil, nil,
		)

		assert.Nil(err)
		// An object shorter than the detection window is normal; the resulting EOF is not an
		// error, and the whole object is handed to the detector.
		assert.True(fixture.reader.closed)
	})

	t.Run("registers within an active session", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		var copiedKey string
		fixture := expectSuccessfulRegistration(t, mocks, &copiedKey)

		activeSession := mockdb.NewDatabase(t)
		activeSession.EXPECT().
			DefineNewArtifact(mock.Anything, mock.Anything).
			Return(models.Artifact{ID: ulid.Make().String()}, nil).
			Once()

		// No UseDatabaseInTransaction expectation: with an active session the manager must
		// reuse the caller's transaction rather than opening its own.
		_, err := manager.RegisterNewArtifact(
			context.Background(),
			fixture.workspace,
			fixture.stagingObjKey,
			"in-session",
			nil,
			activeSession,
		)

		assert.Nil(err)
	})

	t.Run("rejects a staging key issued for another workspace", func(t *testing.T) {
		assert := assert.New(t)
		manager, _ := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		foreign := stagingKeyFor(sampleWorkspace("other-workspace"))

		// No object store or DB expectations: the ownership check must reject before any work
		// happens, so a key aimed at another workspace never reads its object (DESIGN §6.1).
		entry, err := manager.RegisterNewArtifact(
			context.Background(), workspace, foreign, "stolen", nil, nil,
		)

		assertBadInputError(assert, err)
		assert.Equal(models.Artifact{}, entry)
	})

	t.Run("rejects a key merely prefixed by the workspace ID", func(t *testing.T) {
		assert := assert.New(t)
		manager, _ := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		// A sibling workspace whose ID starts with this one's would otherwise slip through a
		// separator-less prefix match.
		lookalike := fmt.Sprintf(
			"%s/%s-evil/%s", unitTestStagingPrefix, workspace.ID, ulid.Make().String(),
		)

		entry, err := manager.RegisterNewArtifact(
			context.Background(), workspace, lookalike, "lookalike", nil, nil,
		)

		assertBadInputError(assert, err)
		assert.Equal(models.Artifact{}, entry)
	})

	t.Run("rejects a staged object over the cap before copying", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		stagingObjKey := stagingKeyFor(workspace)

		// The measured size is authoritative: the mint-time check trusted a declared size, so
		// an object that landed over-cap must still be rejected here (see DESIGN §6.1 step 2.2).
		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{Size: unitTestMaxObjectSize + 1}, nil).
			Once()

		// No CopyObject or DefineNewArtifact expectation - neither may run.
		entry, err := manager.RegisterNewArtifact(
			context.Background(), workspace, stagingObjKey, "too-big", nil, nil,
		)

		assertBadInputError(assert, err)
		assert.Equal(models.Artifact{}, entry)
	})

	t.Run("surfaces an object store client acquisition failure", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManagerCore(t)

		workspace := sampleWorkspace("test-workspace")
		acquireErr := fmt.Errorf("object store client unavailable")

		// Acquisition precedes the authoritative stat, so nothing is measured, copied, or
		// recorded - no object store or persistence expectation is set.
		mocks.s3Manager.EXPECT().
			GetClient(mock.Anything, mock.Anything).
			Return(nil, acquireErr).
			Once()

		entry, err := manager.RegisterNewArtifact(
			context.Background(), workspace, stagingKeyFor(workspace), "unregistered", nil, nil,
		)

		assertManagerError(assert, err, acquireErr)
		assert.Equal(models.Artifact{}, entry)
	})

	t.Run("surfaces a stat failure", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		stagingObjKey := stagingKeyFor(workspace)
		storeErr := fmt.Errorf("no such staged object")

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{}, storeErr).
			Once()

		entry, err := manager.RegisterNewArtifact(
			context.Background(), workspace, stagingObjKey, "missing", nil, nil,
		)

		assertManagerError(assert, err, storeErr)
		assert.Equal(models.Artifact{}, entry)
	})

	t.Run("surfaces a sniff read failure", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		stagingObjKey := stagingKeyFor(workspace)
		storeErr := fmt.Errorf("object body unreadable")

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{Size: 16}, nil).
			Once()
		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{}, nil, storeErr).
			Once()

		// No CopyObject expectation: an unsniffable object must not be promoted, since the
		// MIME type is written onto the copy.
		entry, err := manager.RegisterNewArtifact(
			context.Background(), workspace, stagingObjKey, "unreadable", nil, nil,
		)

		assertManagerError(assert, err, storeErr)
		assert.Equal(models.Artifact{}, entry)
	})

	t.Run("surfaces a copy failure without recording an entry", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		stagingObjKey := stagingKeyFor(workspace)
		storeErr := fmt.Errorf("copy rejected")
		mimeType := "text/plain; charset=utf-8"

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{Size: 8}, nil).
			Once()
		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{}, newFakeObjectReader([]byte("contents")), nil).
			Once()
		mocks.callbacks.EXPECT().
			EstimateMIMEType(mock.Anything).
			Return(mimeType).
			Once()
		mocks.s3.EXPECT().
			CopyObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything,
			).
			Return(storeErr).
			Once()

		// No DefineNewArtifact expectation: inserting after a failed copy would leave a row
		// pointing at nothing, which the ordering exists to prevent (see DESIGN §6.1).
		entry, err := manager.RegisterNewArtifact(
			context.Background(), workspace, stagingObjKey, "uncopied", nil, nil,
		)

		assertManagerError(assert, err, storeErr)
		assert.Equal(models.Artifact{}, entry)
	})

	t.Run("surfaces an insert failure and leaves the staging object", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		stagingObjKey := stagingKeyFor(workspace)
		dbErr := fmt.Errorf("artifact name already taken")
		mimeType := "text/plain; charset=utf-8"

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{Size: 8}, nil).
			Once()
		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{}, newFakeObjectReader([]byte("contents")), nil).
			Once()
		mocks.callbacks.EXPECT().
			EstimateMIMEType(mock.Anything).
			Return(mimeType).
			Once()
		mocks.s3.EXPECT().
			CopyObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything,
			).
			Return(nil).
			Once()

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			DefineNewArtifact(mock.Anything, mock.Anything).
			Return(models.Artifact{}, dbErr).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		// No DeleteObject expectation: the staging cleanup is reached only after a successful
		// insert, so a failed one leaves both objects for the maintenance sweep to reclaim.
		entry, err := manager.RegisterNewArtifact(
			context.Background(), workspace, stagingObjKey, "duplicate", nil, nil,
		)

		assertManagerError(assert, err, dbErr)

		var persistenceErr goutils.PersistenceError
		assert.True(
			errors.As(err, &persistenceErr), "expected PersistenceError, got %T: %v", err, err,
		)
		assert.Equal(models.Artifact{}, entry)
	})

	t.Run("succeeds despite a failed staging cleanup", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		stagingObjKey := stagingKeyFor(workspace)
		mimeType := "text/plain; charset=utf-8"
		expected := models.Artifact{ID: ulid.Make().String(), Name: "kept"}

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{Size: 8}, nil).
			Once()
		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{}, newFakeObjectReader([]byte("contents")), nil).
			Once()
		mocks.callbacks.EXPECT().
			EstimateMIMEType(mock.Anything).
			Return(mimeType).
			Once()
		mocks.s3.EXPECT().
			CopyObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything,
			).
			Return(nil).
			Once()
		mocks.s3.EXPECT().
			DeleteObject(mock.Anything, unitTestBucket, stagingObjKey).
			Return(fmt.Errorf("staging delete rejected")).
			Once()

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			DefineNewArtifact(mock.Anything, mock.Anything).
			Return(expected, nil).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		entry, err := manager.RegisterNewArtifact(
			context.Background(), workspace, stagingObjKey, "kept", nil, nil,
		)

		// The entry is already committed by this point, so failing the call over leftover
		// staging debris would be strictly worse than leaving it (see DESIGN §6.1 step 7).
		assert.Nil(err)
		assert.Equal(expected, entry)
	})

	t.Run("succeeds when the cleanup cannot acquire a client", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManagerCore(t)

		workspace := sampleWorkspace("test-workspace")
		stagingObjKey := stagingKeyFor(workspace)
		mimeType := "text/plain; charset=utf-8"
		expected := models.Artifact{ID: ulid.Make().String(), Name: "kept"}

		// Registration acquires four times - stat, sniff, copy, then the best-effort delete.
		// The first three succeed and the last fails, so acquisition breaks only once the
		// entry is already committed.
		mocks.s3Manager.EXPECT().
			GetClient(mock.Anything, mock.Anything).
			Return(mocks.s3, nil).
			Times(3)
		mocks.s3Manager.EXPECT().
			GetClient(mock.Anything, mock.Anything).
			Return(nil, fmt.Errorf("object store client unavailable")).
			Once()

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{Size: 8}, nil).
			Once()
		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{}, newFakeObjectReader([]byte("contents")), nil).
			Once()
		mocks.callbacks.EXPECT().
			EstimateMIMEType(mock.Anything).
			Return(mimeType).
			Once()
		mocks.s3.EXPECT().
			CopyObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything,
			).
			Return(nil).
			Once()
		// No DeleteObject expectation: without a client the delete is never attempted.

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			DefineNewArtifact(mock.Anything, mock.Anything).
			Return(expected, nil).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		entry, err := manager.RegisterNewArtifact(
			context.Background(), workspace, stagingObjKey, "kept", nil, nil,
		)

		// Failing to acquire a client for the cleanup is the same class of failure as the
		// delete itself failing, so it is logged and the committed entry is still returned.
		assert.Nil(err)
		assert.Equal(expected, entry)
	})
}

// ======================================================================================
// UpdateArtifactContent

// updateFixture the arrangement a successful content update needs, so each case can vary the one
// step it is about rather than restating the whole chain.
type updateFixture struct {
	workspace     models.Workspace
	artifact      models.Artifact
	stagingObjKey string
	content       []byte
	mimeType      string
	reader        *fakeObjectReader
	mockDatabase  *mockdb.Database
}

// expectSuccessfulContentUpdate arrange the full happy-path object-store chain: stat, sniff,
// copy, and the best-effort staging delete. `copiedKey` receives the final key the copy targeted.
// The persistence expectations are left to the caller, since that is what most cases vary.
func expectSuccessfulContentUpdate(
	t *testing.T, mocks unitTestManagerMocks, copiedKey *string,
) updateFixture {
	fixture := updateFixture{
		workspace: sampleWorkspace("test-workspace"),
		content:   []byte("replacement artifact body"),
		mimeType:  "application/json",
	}
	fixture.artifact = sampleArtifact(fixture.workspace, "report-txt")
	fixture.stagingObjKey = stagingKeyFor(fixture.workspace)
	fixture.reader = newFakeObjectReader(fixture.content)
	fixture.mockDatabase = mockdb.NewDatabase(t)

	mocks.s3.EXPECT().
		GetObjectStat(mock.Anything, unitTestBucket, fixture.stagingObjKey).
		Return(goutils.S3ObjectStat{Size: int64(len(fixture.content))}, nil).
		Once()

	mocks.s3.EXPECT().
		GetObject(mock.Anything, unitTestBucket, fixture.stagingObjKey).
		Return(goutils.S3ObjectStat{}, fixture.reader, nil).
		Once()

	mocks.callbacks.EXPECT().
		EstimateMIMEType(mock.Anything).
		Return(fixture.mimeType).
		Once()

	mocks.s3.EXPECT().
		CopyObject(
			mock.Anything,
			unitTestBucket,
			fixture.stagingObjKey,
			unitTestBucket,
			mock.Anything,
			&fixture.mimeType,
		).
		Run(func(_ context.Context, _ string, _ string, _ string, dstKey string, _ *string) {
			if copiedKey != nil {
				*copiedKey = dstKey
			}
		}).
		Return(nil).
		Once()

	mocks.s3.EXPECT().
		DeleteObject(mock.Anything, unitTestBucket, fixture.stagingObjKey).
		Return(nil).
		Once()

	return fixture
}

// TestManagerUpdateArtifactContent validates the update path: it repeats every pre-copy guard
// the register path applies, always writes to a new final key, and repoints the entry without
// creating one.
func TestManagerUpdateArtifactContent(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("repoints the entry at a new object", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		var copiedKey string
		fixture := expectSuccessfulContentUpdate(t, mocks, &copiedKey)

		expected := fixture.artifact
		expected.ObjectKey = "read-back-object-key"
		expected.MIMEType = fixture.mimeType
		expected.Size = int64(len(fixture.content))

		var (
			gotID       string
			gotKey      string
			gotMIMEType string
			gotSize     int64
		)
		fixture.mockDatabase.EXPECT().
			UpdateArtifactObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Run(func(_ context.Context, id string, key string, mimeType string, size int64) {
				gotID, gotKey, gotMIMEType, gotSize = id, key, mimeType, size
			}).
			Return(nil).
			Once()
		fixture.mockDatabase.EXPECT().
			GetArtifact(mock.Anything, fixture.artifact.ID).
			Return(expected, nil).
			Once()

		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(fixture.mockDatabase)).
			Once()

		entry, err := manager.UpdateArtifactContent(
			context.Background(), fixture.artifact, fixture.stagingObjKey, nil,
		)

		assert.Nil(err)
		// The returned entry is the read-back, not the caller's stale copy.
		assert.Equal(expected, entry)

		// The entry is repointed at the object that was actually copied, described by what was
		// measured and sniffed server-side - not by anything the caller asserted.
		assert.Equal(fixture.artifact.ID, gotID)
		assert.Equal(copiedKey, gotKey)
		assert.Equal(fixture.mimeType, gotMIMEType)
		assert.Equal(int64(len(fixture.content)), gotSize)
	})

	t.Run("always writes to a new final key", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		var copiedKey string
		fixture := expectSuccessfulContentUpdate(t, mocks, &copiedKey)

		fixture.mockDatabase.EXPECT().
			UpdateArtifactObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(nil).
			Once()
		fixture.mockDatabase.EXPECT().
			GetArtifact(mock.Anything, mock.Anything).
			Return(fixture.artifact, nil).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(fixture.mockDatabase)).
			Once()

		_, err := manager.UpdateArtifactContent(
			context.Background(), fixture.artifact, fixture.stagingObjKey, nil,
		)
		assert.Nil(err)

		// Never over the object the entry already points at: an in-place overwrite would make
		// the update non-atomic, and a same-key copy is its own hazard (see DESIGN §6.2, §6.3).
		assert.NotEqual(fixture.artifact.ObjectKey, copiedKey)
		assert.NotEqual(fixture.stagingObjKey, copiedKey)
		assert.True(
			strings.HasPrefix(
				copiedKey, fmt.Sprintf("%s/%s/", unitTestStorePrefix, fixture.workspace.ID),
			),
			"final key '%s' must be scoped to workspace %s", copiedKey, fixture.workspace.ID,
		)
	})

	t.Run("never deletes the object the entry previously held", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		var copiedKey string
		fixture := expectSuccessfulContentUpdate(t, mocks, &copiedKey)

		fixture.mockDatabase.EXPECT().
			UpdateArtifactObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(nil).
			Once()
		fixture.mockDatabase.EXPECT().
			GetArtifact(mock.Anything, mock.Anything).
			Return(fixture.artifact, nil).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(fixture.mockDatabase)).
			Once()

		_, err := manager.UpdateArtifactContent(
			context.Background(), fixture.artifact, fixture.stagingObjKey, nil,
		)

		assert.Nil(err)
		// The only DeleteObject arranged by the fixture is the staging cleanup. The old final
		// object is orphaned by design and left for the object-reaping GC; deleting it here
		// would place a second reclaimer alongside the GC (see DESIGN §6.3, §8.2.1).
		mocks.s3.AssertNotCalled(
			t, "DeleteObject", mock.Anything, unitTestBucket, fixture.artifact.ObjectKey,
		)
	})

	t.Run("re-sniffs rather than reusing the entry's MIME type", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		// The entry currently holds text; the replacement bytes are something else entirely.
		entry := sampleArtifact(workspace, "was-text")
		stagingObjKey := stagingKeyFor(workspace)
		newMIMEType := "image/png"

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{Size: 32}, nil).
			Once()
		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{}, newFakeObjectReader([]byte("\x89PNG\r\n\x1a\n")), nil).
			Once()
		mocks.callbacks.EXPECT().
			EstimateMIMEType(mock.Anything).
			Return(newMIMEType).
			Once()

		// The copy carries the newly sniffed type, so the stored object's Content-Type tracks
		// its actual content rather than what the artifact used to hold.
		mocks.s3.EXPECT().
			CopyObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				&newMIMEType,
			).
			Return(nil).
			Once()
		mocks.s3.EXPECT().
			DeleteObject(mock.Anything, mock.Anything, mock.Anything).
			Return(nil).
			Once()

		var recordedMIMEType string
		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateArtifactObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Run(func(_ context.Context, _ string, _ string, mimeType string, _ int64) {
				recordedMIMEType = mimeType
			}).
			Return(nil).
			Once()
		mockDatabase.EXPECT().
			GetArtifact(mock.Anything, mock.Anything).
			Return(entry, nil).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		_, err := manager.UpdateArtifactContent(
			context.Background(), entry, stagingObjKey, nil,
		)

		assert.Nil(err)
		assert.Equal(newMIMEType, recordedMIMEType)
		assert.NotEqual(entry.MIMEType, recordedMIMEType)
	})

	t.Run("updates a MISSING_OBJECT artifact without a state gate", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		var copiedKey string
		fixture := expectSuccessfulContentUpdate(t, mocks, &copiedKey)

		// Re-uploading the bytes is exactly how a quarantined artifact is brought back into
		// service, so the manager must not refuse one (unlike GenerateGetURLForArtifact, which
		// must). The transition itself is the persistence layer's to validate.
		quarantined := fixture.artifact
		quarantined.State = models.ArtifactStateMissingObject

		restored := quarantined
		restored.State = models.ArtifactStateRecorded

		fixture.mockDatabase.EXPECT().
			UpdateArtifactObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(nil).
			Once()
		fixture.mockDatabase.EXPECT().
			GetArtifact(mock.Anything, quarantined.ID).
			Return(restored, nil).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(fixture.mockDatabase)).
			Once()

		entry, err := manager.UpdateArtifactContent(
			context.Background(), quarantined, fixture.stagingObjKey, nil,
		)

		assert.Nil(err)
		assert.Equal(models.ArtifactStateRecorded, entry.State)
	})

	t.Run("updates within an active session", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		var copiedKey string
		fixture := expectSuccessfulContentUpdate(t, mocks, &copiedKey)

		activeSession := mockdb.NewDatabase(t)
		activeSession.EXPECT().
			UpdateArtifactObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(nil).
			Once()
		activeSession.EXPECT().
			GetArtifact(mock.Anything, fixture.artifact.ID).
			Return(fixture.artifact, nil).
			Once()

		// No UseDatabaseInTransaction expectation: with an active session the manager must
		// reuse the caller's transaction rather than opening its own, so the update and its
		// read-back stay inside the caller's unit of work.
		_, err := manager.UpdateArtifactContent(
			context.Background(), fixture.artifact, fixture.stagingObjKey, activeSession,
		)

		assert.Nil(err)
	})

	t.Run("sniffs only the leading bytes and closes the body", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "big-blob")
		stagingObjKey := stagingKeyFor(workspace)
		// Larger than the detection window, so a full read would be observable.
		content := bytes.Repeat([]byte("y"), int(unitTestMaxObjectSize))
		reader := newFakeObjectReader(content)
		mimeType := "application/octet-stream"

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{Size: int64(len(content))}, nil).
			Once()
		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{}, reader, nil).
			Once()

		var sniffed []byte
		mocks.callbacks.EXPECT().
			EstimateMIMEType(mock.Anything).
			Run(func(data []byte) { sniffed = data }).
			Return(mimeType).
			Once()

		mocks.s3.EXPECT().
			CopyObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything,
			).
			Return(nil).
			Once()
		mocks.s3.EXPECT().
			DeleteObject(mock.Anything, mock.Anything, mock.Anything).
			Return(nil).
			Once()

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateArtifactObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(nil).
			Once()
		mockDatabase.EXPECT().
			GetArtifact(mock.Anything, mock.Anything).
			Return(entry, nil).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		_, err := manager.UpdateArtifactContent(
			context.Background(), entry, stagingObjKey, nil,
		)

		assert.Nil(err)

		// The read is bounded at the detection window on this path too, so the rest of a large
		// replacement object never transits just to identify its type.
		assert.Len(sniffed, 3072)
		assert.Less(len(sniffed), len(content))
		assert.True(reader.closed, "the object body must be closed after the sniff")
	})

	t.Run("rejects a staging key issued for another workspace", func(t *testing.T) {
		assert := assert.New(t)
		manager, _ := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "target")
		foreign := stagingKeyFor(sampleWorkspace("other-workspace"))

		// No object store or DB expectations: the ownership guard applies to the update path
		// exactly as it does to register, so a key aimed at another workspace never reads its
		// object and never reaches the entry (see DESIGN §6.3).
		updated, err := manager.UpdateArtifactContent(
			context.Background(), entry, foreign, nil,
		)

		assertBadInputError(assert, err)
		assert.Equal(models.Artifact{}, updated)
	})

	t.Run("rejects a key merely prefixed by the workspace ID", func(t *testing.T) {
		assert := assert.New(t)
		manager, _ := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "target")
		// A sibling workspace whose ID starts with this one's would otherwise slip through a
		// separator-less prefix match.
		lookalike := fmt.Sprintf(
			"%s/%s-evil/%s", unitTestStagingPrefix, workspace.ID, ulid.Make().String(),
		)

		updated, err := manager.UpdateArtifactContent(
			context.Background(), entry, lookalike, nil,
		)

		assertBadInputError(assert, err)
		assert.Equal(models.Artifact{}, updated)
	})

	t.Run("rejects a staged object over the cap before copying", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "target")
		stagingObjKey := stagingKeyFor(workspace)

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{Size: unitTestMaxObjectSize + 1}, nil).
			Once()

		// No CopyObject or UpdateArtifactObject expectation - neither may run. Rejecting before
		// the copy is what leaves the entry pointing at its existing content (see DESIGN §7.5).
		updated, err := manager.UpdateArtifactContent(
			context.Background(), entry, stagingObjKey, nil,
		)

		assertBadInputError(assert, err)
		assert.Equal(models.Artifact{}, updated)
	})

	t.Run("accepts a staged object exactly at the cap", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "at-cap")
		stagingObjKey := stagingKeyFor(workspace)
		mimeType := "application/octet-stream"

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{Size: unitTestMaxObjectSize}, nil).
			Once()
		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{}, newFakeObjectReader([]byte("at cap")), nil).
			Once()
		mocks.callbacks.EXPECT().
			EstimateMIMEType(mock.Anything).
			Return(mimeType).
			Once()
		mocks.s3.EXPECT().
			CopyObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything,
			).
			Return(nil).
			Once()
		mocks.s3.EXPECT().
			DeleteObject(mock.Anything, mock.Anything, mock.Anything).
			Return(nil).
			Once()

		var recordedSize int64
		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateArtifactObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Run(func(_ context.Context, _ string, _ string, _ string, size int64) {
				recordedSize = size
			}).
			Return(nil).
			Once()
		mockDatabase.EXPECT().
			GetArtifact(mock.Anything, mock.Anything).
			Return(entry, nil).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		// The cap is inclusive - an object of exactly the maximum size is still a single PUT.
		_, err := manager.UpdateArtifactContent(
			context.Background(), entry, stagingObjKey, nil,
		)

		assert.Nil(err)
		assert.Equal(unitTestMaxObjectSize, recordedSize)
	})

	t.Run("surfaces an object store client acquisition failure", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManagerCore(t)

		workspace := sampleWorkspace("test-workspace")
		existing := sampleArtifact(workspace, "report-txt")
		acquireErr := fmt.Errorf("object store client unavailable")

		// Acquisition precedes the authoritative stat, so the entry keeps pointing at the
		// content it already held - no object store or persistence expectation is set.
		mocks.s3Manager.EXPECT().
			GetClient(mock.Anything, mock.Anything).
			Return(nil, acquireErr).
			Once()

		updated, err := manager.UpdateArtifactContent(
			context.Background(), existing, stagingKeyFor(workspace), nil,
		)

		assertManagerError(assert, err, acquireErr)
		assert.Equal(models.Artifact{}, updated)
	})

	t.Run("surfaces a stat failure", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "target")
		stagingObjKey := stagingKeyFor(workspace)
		storeErr := fmt.Errorf("no such staged object")

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{}, storeErr).
			Once()

		updated, err := manager.UpdateArtifactContent(
			context.Background(), entry, stagingObjKey, nil,
		)

		assertManagerError(assert, err, storeErr)
		assert.Equal(models.Artifact{}, updated)
	})

	t.Run("surfaces a sniff read failure", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "target")
		stagingObjKey := stagingKeyFor(workspace)
		storeErr := fmt.Errorf("object body unreadable")

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{Size: 16}, nil).
			Once()
		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{}, nil, storeErr).
			Once()

		// No CopyObject expectation: an unsniffable object must not be promoted, since the
		// MIME type is written onto the copy.
		updated, err := manager.UpdateArtifactContent(
			context.Background(), entry, stagingObjKey, nil,
		)

		assertManagerError(assert, err, storeErr)
		assert.Equal(models.Artifact{}, updated)
	})

	t.Run("surfaces a copy failure without touching the entry", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "target")
		stagingObjKey := stagingKeyFor(workspace)
		storeErr := fmt.Errorf("copy rejected")
		mimeType := "text/plain; charset=utf-8"

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{Size: 8}, nil).
			Once()
		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{}, newFakeObjectReader([]byte("contents")), nil).
			Once()
		mocks.callbacks.EXPECT().
			EstimateMIMEType(mock.Anything).
			Return(mimeType).
			Once()
		mocks.s3.EXPECT().
			CopyObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything,
			).
			Return(storeErr).
			Once()

		// No UpdateArtifactObject expectation: repointing after a failed copy would leave the
		// entry aimed at nothing, which the ordering exists to prevent (see DESIGN §6.3).
		updated, err := manager.UpdateArtifactContent(
			context.Background(), entry, stagingObjKey, nil,
		)

		assertManagerError(assert, err, storeErr)
		assert.Equal(models.Artifact{}, updated)
	})

	t.Run("surfaces an update failure and leaves the staging object", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "target")
		stagingObjKey := stagingKeyFor(workspace)
		dbErr := fmt.Errorf("artifact can't transition to 'RECORDED'")
		mimeType := "text/plain; charset=utf-8"

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{Size: 8}, nil).
			Once()
		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{}, newFakeObjectReader([]byte("contents")), nil).
			Once()
		mocks.callbacks.EXPECT().
			EstimateMIMEType(mock.Anything).
			Return(mimeType).
			Once()
		mocks.s3.EXPECT().
			CopyObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything,
			).
			Return(nil).
			Once()

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateArtifactObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(dbErr).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		// No GetArtifact expectation: the read-back is reached only after a successful update.
		// No DeleteObject expectation either, so both objects are left for the maintenance sweep.
		updated, err := manager.UpdateArtifactContent(
			context.Background(), entry, stagingObjKey, nil,
		)

		assertManagerError(assert, err, dbErr)

		var persistenceErr goutils.PersistenceError
		assert.True(
			errors.As(err, &persistenceErr), "expected PersistenceError, got %T: %v", err, err,
		)
		assert.Equal(models.Artifact{}, updated)
	})

	t.Run("surfaces a read back failure", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "target")
		stagingObjKey := stagingKeyFor(workspace)
		dbErr := fmt.Errorf("artifact vanished")
		mimeType := "text/plain; charset=utf-8"

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{Size: 8}, nil).
			Once()
		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{}, newFakeObjectReader([]byte("contents")), nil).
			Once()
		mocks.callbacks.EXPECT().
			EstimateMIMEType(mock.Anything).
			Return(mimeType).
			Once()
		mocks.s3.EXPECT().
			CopyObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything,
			).
			Return(nil).
			Once()

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateArtifactObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(nil).
			Once()
		mockDatabase.EXPECT().
			GetArtifact(mock.Anything, entry.ID).
			Return(models.Artifact{}, dbErr).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		// The read-back is inside the transaction, so its failure fails the whole call rather
		// than returning a half-known entry.
		updated, err := manager.UpdateArtifactContent(
			context.Background(), entry, stagingObjKey, nil,
		)

		assertManagerError(assert, err, dbErr)
		assert.Equal(models.Artifact{}, updated)
	})

	t.Run("succeeds despite a failed staging cleanup", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManager(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "kept")
		stagingObjKey := stagingKeyFor(workspace)
		mimeType := "text/plain; charset=utf-8"

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{Size: 8}, nil).
			Once()
		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{}, newFakeObjectReader([]byte("contents")), nil).
			Once()
		mocks.callbacks.EXPECT().
			EstimateMIMEType(mock.Anything).
			Return(mimeType).
			Once()
		mocks.s3.EXPECT().
			CopyObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything,
			).
			Return(nil).
			Once()
		mocks.s3.EXPECT().
			DeleteObject(mock.Anything, unitTestBucket, stagingObjKey).
			Return(fmt.Errorf("staging delete rejected")).
			Once()

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateArtifactObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(nil).
			Once()
		mockDatabase.EXPECT().
			GetArtifact(mock.Anything, entry.ID).
			Return(entry, nil).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		updated, err := manager.UpdateArtifactContent(
			context.Background(), entry, stagingObjKey, nil,
		)

		// The entry already points at the new object by this point, so failing the call over
		// leftover staging debris would be strictly worse than leaving it (see DESIGN §8.2.1).
		assert.Nil(err)
		assert.Equal(entry, updated)
	})

	t.Run("succeeds when the cleanup cannot acquire a client", func(t *testing.T) {
		assert := assert.New(t)
		manager, mocks := newUnitTestManagerCore(t)

		workspace := sampleWorkspace("test-workspace")
		entry := sampleArtifact(workspace, "report-txt")
		stagingObjKey := stagingKeyFor(workspace)
		mimeType := "application/json"

		// An update acquires four times - stat, sniff, copy, then the best-effort delete. The
		// first three succeed and the last fails, so acquisition breaks only once the entry
		// already points at its new object.
		mocks.s3Manager.EXPECT().
			GetClient(mock.Anything, mock.Anything).
			Return(mocks.s3, nil).
			Times(3)
		mocks.s3Manager.EXPECT().
			GetClient(mock.Anything, mock.Anything).
			Return(nil, fmt.Errorf("object store client unavailable")).
			Once()

		mocks.s3.EXPECT().
			GetObjectStat(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{Size: 8}, nil).
			Once()
		mocks.s3.EXPECT().
			GetObject(mock.Anything, unitTestBucket, stagingObjKey).
			Return(goutils.S3ObjectStat{}, newFakeObjectReader([]byte("contents")), nil).
			Once()
		mocks.callbacks.EXPECT().
			EstimateMIMEType(mock.Anything).
			Return(mimeType).
			Once()
		mocks.s3.EXPECT().
			CopyObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
				mock.Anything,
			).
			Return(nil).
			Once()
		// No DeleteObject expectation: without a client the delete is never attempted.

		mockDatabase := mockdb.NewDatabase(t)
		mockDatabase.EXPECT().
			UpdateArtifactObject(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(nil).
			Once()
		mockDatabase.EXPECT().
			GetArtifact(mock.Anything, entry.ID).
			Return(entry, nil).
			Once()
		mocks.persistence.EXPECT().
			UseDatabaseInTransaction(mock.Anything, mock.Anything).
			RunAndReturn(runTxForManager(mockDatabase)).
			Once()

		updated, err := manager.UpdateArtifactContent(
			context.Background(), entry, stagingObjKey, nil,
		)

		// Failing to acquire a client for the cleanup is the same class of failure as the
		// delete itself failing, so it is logged and the updated entry is still returned.
		assert.Nil(err)
		assert.Equal(entry, updated)
	})
}
