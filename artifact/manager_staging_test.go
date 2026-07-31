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
	// unitTestMaxObjectSize the single-PUT size cap the harness manager is configured with
	unitTestMaxObjectSize int64 = 4096
)

// unitTestStoreConfig build the storage config the harness manager is constructed with.
func unitTestStoreConfig() models.ArtifactStorageConfig {
	return models.ArtifactStorageConfig{
		Bucket:             unitTestBucket,
		UploadPutURLTTLSec: unitTestPutURLTTLSec,
		MaxObjectSizeBytes: unitTestMaxObjectSize,
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

// newUnitTestManager build a Manager over mocked persistence, object store, and MIME detector.
// No expectations are set, so any call a case did not arrange for fails that case.
func newUnitTestManager(t *testing.T) (artifact.Manager, unitTestManagerMocks) {
	assert := assert.New(t)

	mocks := unitTestManagerMocks{
		persistence: mockdb.NewClient(t),
		s3:          mockgoutils.NewS3Client(t),
		callbacks:   mocktest.NewUnitTestCallbackCollector(t),
	}

	manager, err := artifact.NewManager(
		unitTestAppName,
		mocks.persistence,
		mocks.s3,
		unitTestStoreConfig(),
		mocks.callbacks.EstimateMIMEType,
	)
	assert.Nil(err)
	assert.NotNil(manager)

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
}
