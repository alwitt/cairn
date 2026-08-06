package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alwitt/cairn/api"
	"github.com/alwitt/cairn/artifact"
	"github.com/alwitt/cairn/db"
	mockartifact "github.com/alwitt/cairn/mocks/artifact"
	mockworkspace "github.com/alwitt/cairn/mocks/workspace"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// unitTestGetURLMaxTTLSecs the download GET URL TTL ceiling the handler under test is built
// with. It is both the default a presign request takes and the cap a requested `?ttl` is held
// to, so it is what the presign tests assert against.
const unitTestGetURLMaxTTLSecs = 300

// The route patterns the artifact handlers are registered under in BuildHTTPServer. Served
// through a real router carrying these, so `{workspaceID}` and `{artifactID}` resolve the way
// they do in production rather than being injected into the request context by hand.
const (
	routeNewStaging        = "/v1/workspaces/{workspaceID}/new-staging"
	routeArtifacts         = "/v1/workspaces/{workspaceID}/artifacts"
	routeArtifactFromVol   = "/v1/workspaces/{workspaceID}/artifact-from-volume"
	routeArtifact          = "/v1/artifacts/{artifactID}"
	routeArtifactName      = "/v1/artifacts/{artifactID}/name"
	routeArtifactDesc      = "/v1/artifacts/{artifactID}/description"
	routeArtifactContent   = "/v1/artifacts/{artifactID}/content"
	routeArtifactLoadVol   = "/v1/artifacts/{artifactID}/load-in-volume"
	routeArtifactUpdateVol = "/v1/artifacts/{artifactID}/update-from-volume"
)

// artifactAPIMocks the three collaborators the artifact handler drives. All are bound to `t`, so
// any call a test did not arrange fails it - which is what pins down the endpoints that must not
// reach a collaborator at all.
type artifactAPIMocks struct {
	workspaces *mockworkspace.Manager
	artifacts  *mockartifact.Manager
	operator   *mockartifact.Operator
}

// buildArtifactAPIHandler build the handler under test over mock collaborators, returning both.
func buildArtifactAPIHandler(
	assert *assert.Assertions, t *testing.T,
) (api.ArtifactAPIHandler, artifactAPIMocks) {
	mocks := artifactAPIMocks{
		workspaces: mockworkspace.NewManager(t),
		artifacts:  mockartifact.NewManager(t),
		operator:   mockartifact.NewOperator(t),
	}

	uut, err := api.NewArtifactAPIHandler(
		unitTestAppName,
		mocks.workspaces,
		mocks.artifacts,
		mocks.operator,
		models.ArtifactStorageConfig{
			Bucket:                   "unit-test-bucket",
			UploadPutURLTTLSecs:      300,
			DownloadGetURLMaxTTLSecs: unitTestGetURLMaxTTLSecs,
			MaxObjectSizeBytes:       1024 * 1024,
		},
		models.HTTPRequestLogging{
			LogLevel:        goutils.HTTPLogLevelWARN,
			HealthLogLevel:  goutils.HTTPLogLevelWARN,
			RequestIDHeader: "unit-test",
			DoNotLogHeaders: []string{},
		},
		nil,
	)
	assert.Nil(err)

	return uut, mocks
}

// serveOneArtifactRequest run a request against a router carrying only the endpoint under test,
// wrapped in the same logging middleware the server installs.
func serveOneArtifactRequest(
	uut api.ArtifactAPIHandler,
	pattern string,
	handler http.HandlerFunc,
	req *http.Request,
) *httptest.ResponseRecorder {
	router := mux.NewRouter()
	respRecorder := httptest.NewRecorder()
	router.HandleFunc(pattern, uut.LoggingMiddleware(handler))
	router.ServeHTTP(respRecorder, req)
	return respRecorder
}

// sampleArtifact build an artifact entry of the shape the manager returns.
func sampleArtifact(workspaceID string, name string) models.Artifact {
	return models.Artifact{
		ID:          uuid.NewString(),
		WorkspaceID: workspaceID,
		Name:        name,
		ObjectKey:   fmt.Sprintf("store/%s/%s", workspaceID, uuid.NewString()),
		MIMEType:    "application/octet-stream",
		Size:        2048,
		State:       models.ArtifactStateRecorded,
	}
}

// artifactDBFailure build the error shape an artifact manager DB call produces: the manager's own
// error stacked over a PersistenceError, over whichever goutils error the persistence layer
// raised. The API classifies on the innermost one, so the tests hand it the real nesting.
func artifactDBFailure(core error) error {
	return models.NewArtifactMangerError(
		"simulated manager failure",
		goutils.NewPersistenceError("simulated persistence failure", core, true),
		true,
	)
}

// artifactMgrFailure build the error shape an artifact manager object store call produces. There
// is no persistence layer beneath it, so the manager's error sits directly over the cause.
func artifactMgrFailure(core error) error {
	return models.NewArtifactMangerError("simulated manager failure", core, true)
}

// artifactOpFailure build the error shape an artifact operator call produces. A sidecar refusal
// and a path outside the volume mount both arrive this way.
func artifactOpFailure(core error) error {
	return models.NewArtifactOperatorError("simulated operator failure", core, true)
}

// unknownArtifact the persistence layer's answer for an artifact ID that has no row.
func unknownArtifact(artifactID string) error {
	return goutils.NewNotFoundError(
		fmt.Sprintf("artifact '%s' does not exist", artifactID), nil, true,
	)
}

// TestNewArtifactAPIHandler validates the constructor's input guards.
func TestNewArtifactAPIHandler(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	logConfig := models.HTTPRequestLogging{
		LogLevel:        goutils.HTTPLogLevelWARN,
		HealthLogLevel:  goutils.HTTPLogLevelWARN,
		RequestIDHeader: "unit-test",
		DoNotLogHeaders: []string{},
	}

	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)

		uut, err := api.NewArtifactAPIHandler(
			unitTestAppName,
			mockworkspace.NewManager(t),
			mockartifact.NewManager(t),
			mockartifact.NewOperator(t),
			models.ArtifactStorageConfig{},
			logConfig,
			nil,
		)
		assert.Nil(err)
		assert.NotNil(uut)
	})

	// The application name lands in every workspace's volume name, so it is held to the same
	// charset a volume name must satisfy - rejected at construction, not on first use.
	t.Run("invalid application name rejected", func(t *testing.T) {
		assert := assert.New(t)

		for _, appName := range []string{"", "has space", "has/slash", "has.dot"} {
			_, err := api.NewArtifactAPIHandler(
				appName,
				mockworkspace.NewManager(t),
				mockartifact.NewManager(t),
				mockartifact.NewOperator(t),
				models.ArtifactStorageConfig{},
				logConfig,
				nil,
			)
			assert.NotNil(err, "application name '%s' should be rejected", appName)
		}
	})
}

// TestArtifactAPINewStagingUpload validates minting a staging upload PUT URL.
func TestArtifactAPINewStagingUpload(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		parent := sampleWorkspace("unit-test-workspace")
		contentType := "application/zip"
		bundle := artifact.StagingUploadBundle{
			StagingObjectKey: fmt.Sprintf("staging/%s/%s", parent.ID, uuid.NewString()),
			PutURL:           "https://unit-test.object.store/staging?signature=abc",
		}

		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, parent.ID, nil).
			Return(parent, -1, nil).
			Once()
		mocks.artifacts.EXPECT().
			GetArtifactStagingPutURL(
				mock.Anything, parent, int64(4096), "dW5pdC10ZXN0", &contentType,
			).
			Return(bundle, nil).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/workspaces/%s/new-staging", parent.ID),
			jsonBody(assert, api.NewStagingUploadRequest{
				Size: 4096, SHA256B64: "dW5pdC10ZXN0", ContentType: &contentType,
			}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeNewStaging, uut.NewStagingUpload, req,
		)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.StagingUploadResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		assert.Equal(bundle.StagingObjectKey, parsed.Staging.StagingObjectKey)
		assert.Equal(bundle.PutURL, parsed.Staging.PutURL)
	})

	// Case 1: a zero byte artifact is legitimate, so `size` is validated `gte=0` rather than
	// `required` - which on an integer would reject exactly this.
	t.Run("zero size accepted", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		parent := sampleWorkspace("unit-test-workspace")

		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, parent.ID, nil).
			Return(parent, -1, nil).
			Once()
		mocks.artifacts.EXPECT().
			GetArtifactStagingPutURL(
				mock.Anything, parent, int64(0), "dW5pdC10ZXN0", (*string)(nil),
			).
			Return(artifact.StagingUploadBundle{}, nil).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/workspaces/%s/new-staging", parent.ID),
			jsonBody(assert, api.NewStagingUploadRequest{Size: 0, SHA256B64: "dW5pdC10ZXN0"}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeNewStaging, uut.NewStagingUpload, req,
		)

		assert.Equal(http.StatusOK, respRecorder.Code)
	})

	// Case 2: the checksum binds the presigned PUT, so a value that is not base64 could never
	// produce a usable URL. Rejected before either collaborator is touched.
	t.Run("non base64 checksum rejected", func(t *testing.T) {
		assert := assert.New(t)
		uut, _ := buildArtifactAPIHandler(assert, t)

		workspaceID := uuid.NewString()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/workspaces/%s/new-staging", workspaceID),
			jsonBody(assert, api.NewStagingUploadRequest{
				Size: 4096, SHA256B64: "not valid base64!!",
			}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeNewStaging, uut.NewStagingUpload, req,
		)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	// Case 3: the staging key is namespaced by workspace, so an unknown parent is answered
	// before the object store is touched at all.
	t.Run("unknown workspace answered before the object store", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		workspaceID := uuid.NewString()

		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, workspaceID, nil).
			Return(models.Workspace{}, -1, dbFailure(unknownWorkspace(workspaceID))).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/workspaces/%s/new-staging", workspaceID),
			jsonBody(assert, api.NewStagingUploadRequest{Size: 1, SHA256B64: "dW5pdC10ZXN0"}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeNewStaging, uut.NewStagingUpload, req,
		)

		assert.Equal(http.StatusNotFound, respRecorder.Code)
	})

	// Case 4: an over-cap size is refused at mint time by the manager, and arrives as a
	// BadInputError - the caller's problem to correct, not to retry.
	t.Run("over cap size relayed as bad request", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		parent := sampleWorkspace("unit-test-workspace")

		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, parent.ID, nil).
			Return(parent, -1, nil).
			Once()
		mocks.artifacts.EXPECT().
			GetArtifactStagingPutURL(mock.Anything, parent, mock.Anything, mock.Anything, mock.Anything).
			Return(artifact.StagingUploadBundle{}, artifactMgrFailure(
				goutils.NewBadInputError("over the size cap", nil, true),
			)).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/workspaces/%s/new-staging", parent.ID),
			jsonBody(assert, api.NewStagingUploadRequest{
				Size: 999999999, SHA256B64: "dW5pdC10ZXN0",
			}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeNewStaging, uut.NewStagingUpload, req,
		)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})
}

// TestArtifactAPIRegisterArtifact validates registering an artifact from a staged object.
func TestArtifactAPIRegisterArtifact(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		parent := sampleWorkspace("unit-test-workspace")
		created := sampleArtifact(parent.ID, "unit-test-artifact")
		description := "unit test description"
		stagingKey := fmt.Sprintf("staging/%s/%s", parent.ID, uuid.NewString())

		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, parent.ID, nil).
			Return(parent, -1, nil).
			Once()
		mocks.artifacts.EXPECT().
			RegisterNewArtifact(
				mock.Anything, parent, stagingKey, created.Name, &description, nil,
			).
			Return(created, nil).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/workspaces/%s/artifacts", parent.ID),
			jsonBody(assert, api.RegisterArtifactRequest{
				StagingObjectKey: stagingKey, Name: created.Name, Description: &description,
			}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifacts, uut.RegisterArtifact, req,
		)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.ArtifactEntryResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		assert.Equal(created.ID, parsed.Artifact.ID)
		assert.Equal(created.Name, parsed.Artifact.Name)
		assert.Equal(created.ObjectKey, parsed.Artifact.ObjectKey)
	})

	// Case 1: the name lands in the DB's uniqueness constraint and is user facing, so its
	// charset is enforced before either collaborator is reached.
	t.Run("invalid name rejected before the manager", func(t *testing.T) {
		assert := assert.New(t)
		uut, _ := buildArtifactAPIHandler(assert, t)

		workspaceID := uuid.NewString()

		for _, name := range []string{"", "has space", "has/slash"} {
			req, err := http.NewRequest(
				"POST",
				fmt.Sprintf("/v1/workspaces/%s/artifacts", workspaceID),
				jsonBody(assert, api.RegisterArtifactRequest{
					StagingObjectKey: "staging/key", Name: name,
				}),
			)
			assert.Nil(err)

			respRecorder := serveOneArtifactRequest(
				uut, routeArtifacts, uut.RegisterArtifact, req,
			)

			assert.Equal(
				http.StatusBadRequest, respRecorder.Code, "name '%s' should be rejected", name,
			)
		}
	})

	// Case 2: a staging key issued for another workspace is refused by the manager, which is the
	// check that stops one workspace registering another's staged bytes.
	t.Run("foreign staging key relayed as bad request", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		parent := sampleWorkspace("unit-test-workspace")

		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, parent.ID, nil).
			Return(parent, -1, nil).
			Once()
		mocks.artifacts.EXPECT().
			RegisterNewArtifact(
				mock.Anything, parent, mock.Anything, mock.Anything, mock.Anything, nil,
			).
			Return(models.Artifact{}, artifactMgrFailure(
				goutils.NewBadInputError("staging key was not issued for this workspace", nil, true),
			)).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/workspaces/%s/artifacts", parent.ID),
			jsonBody(assert, api.RegisterArtifactRequest{
				StagingObjectKey: "staging/some-other-workspace/object", Name: "unit-test",
			}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifacts, uut.RegisterArtifact, req,
		)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	// Case 3: the parent workspace must exist before an artifact can be written into it, and the
	// resolve is that check (see DESIGN §7.5).
	t.Run("unknown workspace answered before the manager", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		workspaceID := uuid.NewString()

		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, workspaceID, nil).
			Return(models.Workspace{}, -1, dbFailure(unknownWorkspace(workspaceID))).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/workspaces/%s/artifacts", workspaceID),
			jsonBody(assert, api.RegisterArtifactRequest{
				StagingObjectKey: "staging/key", Name: "unit-test",
			}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifacts, uut.RegisterArtifact, req,
		)

		assert.Equal(http.StatusNotFound, respRecorder.Code)
	})

	// Case 4: a broken database is the caller's problem to retry, not to correct.
	t.Run("SQL failure is a server error", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		parent := sampleWorkspace("unit-test-workspace")

		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, parent.ID, nil).
			Return(parent, -1, nil).
			Once()
		mocks.artifacts.EXPECT().
			RegisterNewArtifact(
				mock.Anything, parent, mock.Anything, mock.Anything, mock.Anything, nil,
			).
			Return(models.Artifact{}, artifactDBFailure(
				goutils.NewSQLError("simulated SQL failure", nil, true),
			)).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/workspaces/%s/artifacts", parent.ID),
			jsonBody(assert, api.RegisterArtifactRequest{
				StagingObjectKey: "staging/key", Name: "unit-test",
			}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifacts, uut.RegisterArtifact, req,
		)

		assert.Equal(http.StatusInternalServerError, respRecorder.Code)
	})
}

// TestArtifactAPIListArtifacts validates listing a workspace's artifacts.
func TestArtifactAPIListArtifacts(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	// The state selection defaults to the live state. An empty selection means "every state" at
	// the persistence layer, which would surface quarantined entries to a caller that never
	// asked for them (see DESIGN §7.1).
	t.Run("state filter defaults to RECORDED", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		parent := sampleWorkspace("unit-test-workspace")
		entries := []models.Artifact{
			sampleArtifact(parent.ID, "artifact-one"),
			sampleArtifact(parent.ID, "artifact-two"),
		}

		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, parent.ID, nil).
			Return(parent, -1, nil).
			Once()
		mocks.artifacts.EXPECT().
			ListWorkspaceArtifacts(
				mock.Anything,
				parent,
				db.ArtifactQueryFilter{
					ArtifactStates: []models.ArtifactStateENUM{models.ArtifactStateRecorded},
				},
				nil,
			).
			Return(entries, nil).
			Once()

		req, err := http.NewRequest(
			"GET", fmt.Sprintf("/v1/workspaces/%s/artifacts", parent.ID), nil,
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(uut, routeArtifacts, uut.ListArtifacts, req)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.ArtifactListResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		assert.Len(parsed.Artifacts, 2)
		assert.Equal(entries[0].ID, parsed.Artifacts[0].ID)
		assert.Equal(entries[1].Name, parsed.Artifacts[1].Name)
	})

	// Case 1: every filter reaches the persistence layer in the shape it expects, and an
	// explicit state selection replaces the default rather than adding to it.
	t.Run("filters translated", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		parent := sampleWorkspace("unit-test-workspace")
		limit, offset := 10, 5

		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, parent.ID, nil).
			Return(parent, -1, nil).
			Once()
		mocks.artifacts.EXPECT().
			ListWorkspaceArtifacts(
				mock.Anything,
				parent,
				db.ArtifactQueryFilter{
					CommonListEntryQueryFilter: db.CommonListEntryQueryFilter{
						Limit: &limit, Offset: &offset,
					},
					TargetNames: []string{"artifact-one", "artifact-two"},
					ArtifactStates: []models.ArtifactStateENUM{
						models.ArtifactStateRecorded, models.ArtifactStateMissingObject,
					},
				},
				nil,
			).
			Return([]models.Artifact{}, nil).
			Once()

		req, err := http.NewRequest(
			"GET",
			fmt.Sprintf(
				"/v1/workspaces/%s/artifacts"+
					"?name=artifact-one&name=artifact-two"+
					"&state=RECORDED&state=MISSING_OBJECT&limit=10&offset=5",
				parent.ID,
			),
			nil,
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(uut, routeArtifacts, uut.ListArtifacts, req)

		assert.Equal(http.StatusOK, respRecorder.Code)
	})

	// Case 2: a malformed pagination parameter is the caller's to fix, and is caught before any
	// collaborator is reached.
	t.Run("non integer limit rejected before the manager", func(t *testing.T) {
		assert := assert.New(t)
		uut, _ := buildArtifactAPIHandler(assert, t)

		workspaceID := uuid.NewString()

		req, err := http.NewRequest(
			"GET",
			fmt.Sprintf("/v1/workspaces/%s/artifacts?limit=not-a-number", workspaceID),
			nil,
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(uut, routeArtifacts, uut.ListArtifacts, req)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	// Case 3: an unknown state value is validated by the persistence layer, which reports it as
	// a validation failure rather than silently returning nothing.
	t.Run("unknown state relayed as bad request", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		parent := sampleWorkspace("unit-test-workspace")

		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, parent.ID, nil).
			Return(parent, -1, nil).
			Once()
		mocks.artifacts.EXPECT().
			ListWorkspaceArtifacts(mock.Anything, parent, mock.Anything, nil).
			Return(nil, artifactDBFailure(
				goutils.NewValidationError("unknown artifact state", nil, true),
			)).
			Once()

		req, err := http.NewRequest(
			"GET",
			fmt.Sprintf("/v1/workspaces/%s/artifacts?state=NOT_A_STATE", parent.ID),
			nil,
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(uut, routeArtifacts, uut.ListArtifacts, req)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})
}

// TestArtifactAPIGetArtifact validates fetching one artifact, with and without a GET URL.
func TestArtifactAPIGetArtifact(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	// Without the flag no URL is minted at all - the mock is bound to `t`, so an unarranged
	// GenerateGetURLForArtifact call would fail this case.
	t.Run("no presign mints no URL", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		entry := sampleArtifact(uuid.NewString(), "unit-test-artifact")

		mocks.artifacts.EXPECT().
			GetArtifact(mock.Anything, entry.ID, nil).
			Return(entry, nil).
			Once()

		req, err := http.NewRequest("GET", fmt.Sprintf("/v1/artifacts/%s", entry.ID), nil)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(uut, routeArtifact, uut.GetArtifact, req)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.ArtifactDetailResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		assert.Equal(entry.ID, parsed.Artifact.ID)
		assert.Nil(parsed.GetURL)
	})

	// Case 1: a bare `?presign` reads as true, and with no `?ttl` the URL takes the configured
	// ceiling as its lifetime.
	t.Run("bare presign mints a URL at the configured maximum TTL", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		entry := sampleArtifact(uuid.NewString(), "unit-test-artifact")
		getURL := "https://unit-test.object.store/artifact?signature=abc"

		mocks.artifacts.EXPECT().
			GetArtifact(mock.Anything, entry.ID, nil).
			Return(entry, nil).
			Once()
		mocks.artifacts.EXPECT().
			GenerateGetURLForArtifact(
				mock.Anything, entry, time.Second*unitTestGetURLMaxTTLSecs,
			).
			Return(getURL, nil).
			Once()

		req, err := http.NewRequest(
			"GET", fmt.Sprintf("/v1/artifacts/%s?presign", entry.ID), nil,
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(uut, routeArtifact, uut.GetArtifact, req)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.ArtifactDetailResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		assert.NotNil(parsed.GetURL)
		assert.Equal(getURL, *parsed.GetURL)
	})

	// A shorter lifetime than the deployment allows is honoured as asked - a reasonable thing to
	// want for a link that is about to be shared.
	t.Run("requested ttl below the maximum is honoured", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		entry := sampleArtifact(uuid.NewString(), "unit-test-artifact")

		mocks.artifacts.EXPECT().
			GetArtifact(mock.Anything, entry.ID, nil).
			Return(entry, nil).
			Once()
		mocks.artifacts.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, time.Second*30).
			Return("https://unit-test.object.store/artifact", nil).
			Once()

		req, err := http.NewRequest(
			"GET", fmt.Sprintf("/v1/artifacts/%s?presign&ttl=30", entry.ID), nil,
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(uut, routeArtifact, uut.GetArtifact, req)

		assert.Equal(http.StatusOK, respRecorder.Code)
	})

	// The configured value is a ceiling, not a suggestion: a request for longer is clamped to it
	// rather than refused, so a caller asking for too much still gets a usable link.
	t.Run("requested ttl above the maximum is clamped", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		entry := sampleArtifact(uuid.NewString(), "unit-test-artifact")

		mocks.artifacts.EXPECT().
			GetArtifact(mock.Anything, entry.ID, nil).
			Return(entry, nil).
			Once()
		mocks.artifacts.EXPECT().
			GenerateGetURLForArtifact(
				mock.Anything, entry, time.Second*unitTestGetURLMaxTTLSecs,
			).
			Return("https://unit-test.object.store/artifact", nil).
			Once()

		req, err := http.NewRequest(
			"GET",
			fmt.Sprintf(
				"/v1/artifacts/%s?presign&ttl=%d", entry.ID, unitTestGetURLMaxTTLSecs*10,
			),
			nil,
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(uut, routeArtifact, uut.GetArtifact, req)

		assert.Equal(http.StatusOK, respRecorder.Code)
	})

	// A non-positive lifetime could only produce a URL that is already expired, so it is the
	// caller's to correct rather than something to clamp into a usable value.
	t.Run("malformed ttl rejected before the manager", func(t *testing.T) {
		assert := assert.New(t)
		uut, _ := buildArtifactAPIHandler(assert, t)

		for _, ttl := range []string{"not-a-number", "0", "-30"} {
			req, err := http.NewRequest(
				"GET",
				fmt.Sprintf("/v1/artifacts/%s?presign&ttl=%s", uuid.NewString(), ttl),
				nil,
			)
			assert.Nil(err)

			respRecorder := serveOneArtifactRequest(uut, routeArtifact, uut.GetArtifact, req)

			assert.Equal(
				http.StatusBadRequest, respRecorder.Code, "ttl '%s' should be rejected", ttl,
			)
		}
	})

	// Case 2: an explicit value is honoured, so `?presign=false` behaves as though absent.
	t.Run("presign=false mints no URL", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		entry := sampleArtifact(uuid.NewString(), "unit-test-artifact")

		mocks.artifacts.EXPECT().
			GetArtifact(mock.Anything, entry.ID, nil).
			Return(entry, nil).
			Once()

		req, err := http.NewRequest(
			"GET", fmt.Sprintf("/v1/artifacts/%s?presign=false", entry.ID), nil,
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(uut, routeArtifact, uut.GetArtifact, req)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.ArtifactDetailResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		assert.Nil(parsed.GetURL)
	})

	t.Run("non boolean presign rejected", func(t *testing.T) {
		assert := assert.New(t)
		uut, _ := buildArtifactAPIHandler(assert, t)

		req, err := http.NewRequest(
			"GET", fmt.Sprintf("/v1/artifacts/%s?presign=maybe", uuid.NewString()), nil,
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(uut, routeArtifact, uut.GetArtifact, req)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	// Case 3: an artifact whose backing object is gone has nothing servable, so the mint is
	// refused rather than the response coming back with the URL quietly missing.
	t.Run("presign on a quarantined artifact is a conflict", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		entry := sampleArtifact(uuid.NewString(), "unit-test-artifact")
		entry.State = models.ArtifactStateMissingObject

		mocks.artifacts.EXPECT().
			GetArtifact(mock.Anything, entry.ID, nil).
			Return(entry, nil).
			Once()
		mocks.artifacts.EXPECT().
			GenerateGetURLForArtifact(mock.Anything, entry, mock.Anything).
			Return("", artifactMgrFailure(
				goutils.NewConsistencyError("no servable object", nil, true),
			)).
			Once()

		req, err := http.NewRequest(
			"GET", fmt.Sprintf("/v1/artifacts/%s?presign=true", entry.ID), nil,
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(uut, routeArtifact, uut.GetArtifact, req)

		assert.Equal(http.StatusConflict, respRecorder.Code)
	})

	t.Run("unknown artifact is not found", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		artifactID := uuid.NewString()

		mocks.artifacts.EXPECT().
			GetArtifact(mock.Anything, artifactID, nil).
			Return(models.Artifact{}, artifactDBFailure(unknownArtifact(artifactID))).
			Once()

		req, err := http.NewRequest("GET", fmt.Sprintf("/v1/artifacts/%s", artifactID), nil)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(uut, routeArtifact, uut.GetArtifact, req)

		assert.Equal(http.StatusNotFound, respRecorder.Code)
	})
}

// TestArtifactAPIDeleteArtifact validates artifact deletion.
func TestArtifactAPIDeleteArtifact(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		artifactID := uuid.NewString()

		mocks.artifacts.EXPECT().
			DeleteArtifact(mock.Anything, artifactID, nil).
			Return(nil).
			Once()

		req, err := http.NewRequest("DELETE", fmt.Sprintf("/v1/artifacts/%s", artifactID), nil)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(uut, routeArtifact, uut.DeleteArtifact, req)

		assert.Equal(http.StatusOK, respRecorder.Code)
	})

	t.Run("SQL failure is a server error", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		artifactID := uuid.NewString()

		mocks.artifacts.EXPECT().
			DeleteArtifact(mock.Anything, artifactID, nil).
			Return(artifactDBFailure(goutils.NewSQLError("simulated SQL failure", nil, true))).
			Once()

		req, err := http.NewRequest("DELETE", fmt.Sprintf("/v1/artifacts/%s", artifactID), nil)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(uut, routeArtifact, uut.DeleteArtifact, req)

		assert.Equal(http.StatusInternalServerError, respRecorder.Code)
	})
}

// TestArtifactAPIUpdateArtifactName validates renaming an artifact.
func TestArtifactAPIUpdateArtifactName(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	// A rename is a pure DB update: the object key carries a random suffix rather than the name,
	// so it comes back untouched (see DESIGN §2.2).
	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		entry := sampleArtifact(uuid.NewString(), "original-name")
		renamed := entry
		renamed.Name = "new-name"

		mocks.artifacts.EXPECT().
			RenameArtifact(mock.Anything, entry.ID, "new-name", nil).
			Return(renamed, nil).
			Once()

		req, err := http.NewRequest(
			"PUT", fmt.Sprintf("/v1/artifacts/%s/name?name=new-name", entry.ID), nil,
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactName, uut.UpdateArtifactName, req,
		)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.ArtifactEntryResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		assert.Equal("new-name", parsed.Artifact.Name)
		assert.Equal(entry.ObjectKey, parsed.Artifact.ObjectKey)
	})

	t.Run("missing name parameter rejected", func(t *testing.T) {
		assert := assert.New(t)
		uut, _ := buildArtifactAPIHandler(assert, t)

		req, err := http.NewRequest(
			"PUT", fmt.Sprintf("/v1/artifacts/%s/name", uuid.NewString()), nil,
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactName, uut.UpdateArtifactName, req,
		)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	// The name charset is enforced by the persistence layer, which reports a bad one as a
	// validation failure.
	t.Run("invalid name relayed as bad request", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		artifactID := uuid.NewString()

		mocks.artifacts.EXPECT().
			RenameArtifact(mock.Anything, artifactID, "has space", nil).
			Return(models.Artifact{}, artifactDBFailure(
				goutils.NewValidationError("invalid artifact name", nil, true),
			)).
			Once()

		req, err := http.NewRequest(
			"PUT", fmt.Sprintf("/v1/artifacts/%s/name?name=has+space", artifactID), nil,
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactName, uut.UpdateArtifactName, req,
		)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	t.Run("unknown artifact is not found", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		artifactID := uuid.NewString()

		mocks.artifacts.EXPECT().
			RenameArtifact(mock.Anything, artifactID, "new-name", nil).
			Return(models.Artifact{}, artifactDBFailure(unknownArtifact(artifactID))).
			Once()

		req, err := http.NewRequest(
			"PUT", fmt.Sprintf("/v1/artifacts/%s/name?name=new-name", artifactID), nil,
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactName, uut.UpdateArtifactName, req,
		)

		assert.Equal(http.StatusNotFound, respRecorder.Code)
	})
}

// TestArtifactAPIUpdateArtifactDescription validates changing an artifact's description.
func TestArtifactAPIUpdateArtifactDescription(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		entry := sampleArtifact(uuid.NewString(), "unit-test-artifact")
		description := "new description"
		updated := entry
		updated.Description = &description

		mocks.artifacts.EXPECT().
			UpdateArtifactDescription(mock.Anything, entry.ID, &description, nil).
			Return(updated, nil).
			Once()

		req, err := http.NewRequest(
			"PUT",
			fmt.Sprintf("/v1/artifacts/%s/description", entry.ID),
			jsonBody(assert, api.UpdateArtifactDescriptionRequest{Description: &description}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactDesc, uut.UpdateArtifactDescription, req,
		)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.ArtifactEntryResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		assert.NotNil(parsed.Artifact.Description)
		assert.Equal(description, *parsed.Artifact.Description)
	})

	// The description travels in a body precisely so an explicit null can clear it, which an
	// absent query parameter could not express.
	t.Run("explicit null clears the description", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		entry := sampleArtifact(uuid.NewString(), "unit-test-artifact")

		mocks.artifacts.EXPECT().
			UpdateArtifactDescription(mock.Anything, entry.ID, (*string)(nil), nil).
			Return(entry, nil).
			Once()

		req, err := http.NewRequest(
			"PUT",
			fmt.Sprintf("/v1/artifacts/%s/description", entry.ID),
			jsonBody(assert, api.UpdateArtifactDescriptionRequest{Description: nil}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactDesc, uut.UpdateArtifactDescription, req,
		)

		assert.Equal(http.StatusOK, respRecorder.Code)
	})

	t.Run("unknown artifact is not found", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		artifactID := uuid.NewString()
		description := "new description"

		mocks.artifacts.EXPECT().
			UpdateArtifactDescription(mock.Anything, artifactID, &description, nil).
			Return(models.Artifact{}, artifactDBFailure(unknownArtifact(artifactID))).
			Once()

		req, err := http.NewRequest(
			"PUT",
			fmt.Sprintf("/v1/artifacts/%s/description", artifactID),
			jsonBody(assert, api.UpdateArtifactDescriptionRequest{Description: &description}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactDesc, uut.UpdateArtifactDescription, req,
		)

		assert.Equal(http.StatusNotFound, respRecorder.Code)
	})
}

// TestArtifactAPIUpdateArtifactContent validates replacing content from a staged object.
func TestArtifactAPIUpdateArtifactContent(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	// The manager works from the resolved entry, and never from a workspace: this endpoint has
	// no workspace in its path and must not reach the workspace manager at all.
	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		entry := sampleArtifact(uuid.NewString(), "unit-test-artifact")
		stagingKey := fmt.Sprintf("staging/%s/%s", entry.WorkspaceID, uuid.NewString())
		updated := entry
		updated.ObjectKey = fmt.Sprintf("store/%s/%s", entry.WorkspaceID, uuid.NewString())

		mocks.artifacts.EXPECT().
			GetArtifact(mock.Anything, entry.ID, nil).
			Return(entry, nil).
			Once()
		mocks.artifacts.EXPECT().
			UpdateArtifactContent(mock.Anything, entry, stagingKey, nil).
			Return(updated, nil).
			Once()

		req, err := http.NewRequest(
			"PUT",
			fmt.Sprintf("/v1/artifacts/%s/content", entry.ID),
			jsonBody(assert, api.UpdateArtifactContentRequest{StagingObjectKey: stagingKey}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactContent, uut.UpdateArtifactContent, req,
		)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.ArtifactEntryResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		// The content always lands on a NEW final key, so the entry comes back repointed.
		assert.Equal(updated.ObjectKey, parsed.Artifact.ObjectKey)
		assert.NotEqual(entry.ObjectKey, parsed.Artifact.ObjectKey)
	})

	t.Run("missing staging key rejected before the manager", func(t *testing.T) {
		assert := assert.New(t)
		uut, _ := buildArtifactAPIHandler(assert, t)

		req, err := http.NewRequest(
			"PUT",
			fmt.Sprintf("/v1/artifacts/%s/content", uuid.NewString()),
			jsonBody(assert, api.UpdateArtifactContentRequest{}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactContent, uut.UpdateArtifactContent, req,
		)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	t.Run("unknown artifact answered before the update", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		artifactID := uuid.NewString()

		mocks.artifacts.EXPECT().
			GetArtifact(mock.Anything, artifactID, nil).
			Return(models.Artifact{}, artifactDBFailure(unknownArtifact(artifactID))).
			Once()

		req, err := http.NewRequest(
			"PUT",
			fmt.Sprintf("/v1/artifacts/%s/content", artifactID),
			jsonBody(assert, api.UpdateArtifactContentRequest{
				StagingObjectKey: "staging/key",
			}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactContent, uut.UpdateArtifactContent, req,
		)

		assert.Equal(http.StatusNotFound, respRecorder.Code)
	})
}

// TestArtifactAPISaveArtifactFromVolume validates creating an artifact from a volume file.
func TestArtifactAPISaveArtifactFromVolume(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		parent := sampleWorkspace("unit-test-workspace")
		parent.VolumeState = models.WorkspaceVolumeStateReady
		created := sampleArtifact(parent.ID, "unit-test-artifact")
		description := "unit test description"
		sourcePath := models.WorkspaceMountPath + "/results/report.json"

		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, parent.ID, nil).
			Return(parent, 1, nil).
			Once()
		mocks.operator.EXPECT().
			UploadArtifact(
				mock.Anything, parent, sourcePath, created.Name, &description, nil,
			).
			Return(created, nil).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/workspaces/%s/artifact-from-volume", parent.ID),
			jsonBody(assert, api.ArtifactFromVolumeRequest{
				SourcePath: sourcePath, Name: created.Name, Description: &description,
			}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactFromVol, uut.SaveArtifactFromVolume, req,
		)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.ArtifactEntryResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		assert.Equal(created.ID, parsed.Artifact.ID)
	})

	// Containment is the operator's call - it holds the canonical mount and applies the same
	// rule the sidecar does - and it arrives as a BadInputError.
	t.Run("path outside the volume mount is a bad request", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		parent := sampleWorkspace("unit-test-workspace")
		parent.VolumeState = models.WorkspaceVolumeStateReady

		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, parent.ID, nil).
			Return(parent, 1, nil).
			Once()
		mocks.operator.EXPECT().
			UploadArtifact(
				mock.Anything, parent, "/etc/passwd", mock.Anything, mock.Anything, nil,
			).
			Return(models.Artifact{}, artifactOpFailure(
				goutils.NewBadInputError("path is outside the workspace volume", nil, true),
			)).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/workspaces/%s/artifact-from-volume", parent.ID),
			jsonBody(assert, api.ArtifactFromVolumeRequest{
				SourcePath: "/etc/passwd", Name: "unit-test",
			}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactFromVol, uut.SaveArtifactFromVolume, req,
		)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	// A workspace with no volume cannot be read from, and the caller cannot provision one - that
	// is an operator's REST job - so the precondition is surfaced legibly.
	t.Run("workspace without a volume is a bad request", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		parent := sampleWorkspace("unit-test-workspace")

		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, parent.ID, nil).
			Return(parent, -1, nil).
			Once()
		mocks.operator.EXPECT().
			UploadArtifact(
				mock.Anything, parent, mock.Anything, mock.Anything, mock.Anything, nil,
			).
			Return(models.Artifact{}, artifactOpFailure(
				goutils.NewBadInputError("workspace has no runtime volume", nil, true),
			)).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/workspaces/%s/artifact-from-volume", parent.ID),
			jsonBody(assert, api.ArtifactFromVolumeRequest{
				SourcePath: models.WorkspaceMountPath + "/report.json", Name: "unit-test",
			}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactFromVol, uut.SaveArtifactFromVolume, req,
		)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	// An unknown workspace costs no container runs: it is answered before the operator is
	// reached at all.
	t.Run("unknown workspace answered before the operator", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		workspaceID := uuid.NewString()

		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, workspaceID, nil).
			Return(models.Workspace{}, -1, dbFailure(unknownWorkspace(workspaceID))).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/workspaces/%s/artifact-from-volume", workspaceID),
			jsonBody(assert, api.ArtifactFromVolumeRequest{
				SourcePath: models.WorkspaceMountPath + "/report.json", Name: "unit-test",
			}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactFromVol, uut.SaveArtifactFromVolume, req,
		)

		assert.Equal(http.StatusNotFound, respRecorder.Code)
	})
}

// TestArtifactAPIUpdateArtifactFromVolume validates replacing content from a volume file.
func TestArtifactAPIUpdateArtifactFromVolume(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	// The operator needs both halves, and the parent comes from the entry rather than the
	// caller: the artifact supplies the row that gets rewritten, its workspace the volume that
	// gets mounted and read.
	t.Run("happy path resolves artifact then workspace", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		parent := sampleWorkspace("unit-test-workspace")
		parent.VolumeState = models.WorkspaceVolumeStateReady
		entry := sampleArtifact(parent.ID, "unit-test-artifact")
		sourcePath := models.WorkspaceMountPath + "/results/report.json"
		updated := entry
		updated.ObjectKey = fmt.Sprintf("store/%s/%s", parent.ID, uuid.NewString())

		mocks.artifacts.EXPECT().
			GetArtifact(mock.Anything, entry.ID, nil).
			Return(entry, nil).
			Once()
		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, parent.ID, nil).
			Return(parent, 1, nil).
			Once()
		mocks.operator.EXPECT().
			UpdateArtifact(mock.Anything, parent, entry, sourcePath, nil).
			Return(updated, nil).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/artifacts/%s/update-from-volume", entry.ID),
			jsonBody(assert, api.UpdateArtifactFromVolumeRequest{SourcePath: sourcePath}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactUpdateVol, uut.UpdateArtifactFromVolume, req,
		)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.ArtifactEntryResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		assert.Equal(updated.ObjectKey, parsed.Artifact.ObjectKey)
	})

	// A quarantined artifact is a legitimate target - re-uploading its content is how one is
	// repaired - so there is no state gate on this path.
	t.Run("quarantined artifact is a legitimate target", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		parent := sampleWorkspace("unit-test-workspace")
		parent.VolumeState = models.WorkspaceVolumeStateReady
		entry := sampleArtifact(parent.ID, "unit-test-artifact")
		entry.State = models.ArtifactStateMissingObject
		sourcePath := models.WorkspaceMountPath + "/report.json"
		repaired := entry
		repaired.State = models.ArtifactStateRecorded

		mocks.artifacts.EXPECT().
			GetArtifact(mock.Anything, entry.ID, nil).
			Return(entry, nil).
			Once()
		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, parent.ID, nil).
			Return(parent, 1, nil).
			Once()
		mocks.operator.EXPECT().
			UpdateArtifact(mock.Anything, parent, entry, sourcePath, nil).
			Return(repaired, nil).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/artifacts/%s/update-from-volume", entry.ID),
			jsonBody(assert, api.UpdateArtifactFromVolumeRequest{SourcePath: sourcePath}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactUpdateVol, uut.UpdateArtifactFromVolume, req,
		)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.ArtifactEntryResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		assert.Equal(models.ArtifactStateRecorded, parsed.Artifact.State)
	})

	// An unknown artifact never reaches the workspace lookup, let alone a container run.
	t.Run("unknown artifact answered before the workspace lookup", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		artifactID := uuid.NewString()

		mocks.artifacts.EXPECT().
			GetArtifact(mock.Anything, artifactID, nil).
			Return(models.Artifact{}, artifactDBFailure(unknownArtifact(artifactID))).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/artifacts/%s/update-from-volume", artifactID),
			jsonBody(assert, api.UpdateArtifactFromVolumeRequest{
				SourcePath: models.WorkspaceMountPath + "/report.json",
			}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactUpdateVol, uut.UpdateArtifactFromVolume, req,
		)

		assert.Equal(http.StatusNotFound, respRecorder.Code)
	})

	// A source that changed on the volume between the two sidecars fails closed at the object
	// store, and the sidecar's reason is relayed as a bad request.
	t.Run("sidecar rejection is a bad request", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		parent := sampleWorkspace("unit-test-workspace")
		parent.VolumeState = models.WorkspaceVolumeStateReady
		entry := sampleArtifact(parent.ID, "unit-test-artifact")

		mocks.artifacts.EXPECT().
			GetArtifact(mock.Anything, entry.ID, nil).
			Return(entry, nil).
			Once()
		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, parent.ID, nil).
			Return(parent, 1, nil).
			Once()
		mocks.operator.EXPECT().
			UpdateArtifact(mock.Anything, parent, entry, mock.Anything, nil).
			Return(models.Artifact{}, artifactOpFailure(
				goutils.NewBadInputError("source file not found", nil, true),
			)).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/artifacts/%s/update-from-volume", entry.ID),
			jsonBody(assert, api.UpdateArtifactFromVolumeRequest{
				SourcePath: models.WorkspaceMountPath + "/gone.json",
			}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactUpdateVol, uut.UpdateArtifactFromVolume, req,
		)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})
}

// TestArtifactAPILoadArtifactInVolume validates downloading an artifact into the volume.
func TestArtifactAPILoadArtifactInVolume(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("happy path resolves artifact then workspace", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		parent := sampleWorkspace("unit-test-workspace")
		parent.VolumeState = models.WorkspaceVolumeStateReady
		entry := sampleArtifact(parent.ID, "unit-test-artifact")
		targetPath := models.WorkspaceMountPath + "/inputs/report.json"

		mocks.artifacts.EXPECT().
			GetArtifact(mock.Anything, entry.ID, nil).
			Return(entry, nil).
			Once()
		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, parent.ID, nil).
			Return(parent, 1, nil).
			Once()
		mocks.operator.EXPECT().
			DownloadArtifact(mock.Anything, parent, entry, targetPath).
			Return(nil).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/artifacts/%s/load-in-volume", entry.ID),
			jsonBody(assert, api.LoadArtifactInVolumeRequest{TargetPath: targetPath}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactLoadVol, uut.LoadArtifactInVolume, req,
		)

		assert.Equal(http.StatusOK, respRecorder.Code)
	})

	t.Run("missing target path rejected before the manager", func(t *testing.T) {
		assert := assert.New(t)
		uut, _ := buildArtifactAPIHandler(assert, t)

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/artifacts/%s/load-in-volume", uuid.NewString()),
			jsonBody(assert, api.LoadArtifactInVolumeRequest{}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactLoadVol, uut.LoadArtifactInVolume, req,
		)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	// The sidecar never creates intermediate directories - it does not control the UID the tool
	// containers run as - so a missing parent is a legible failure, not a mkdir (see §7.5.1).
	t.Run("missing destination directory is a bad request", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		parent := sampleWorkspace("unit-test-workspace")
		parent.VolumeState = models.WorkspaceVolumeStateReady
		entry := sampleArtifact(parent.ID, "unit-test-artifact")

		mocks.artifacts.EXPECT().
			GetArtifact(mock.Anything, entry.ID, nil).
			Return(entry, nil).
			Once()
		mocks.workspaces.EXPECT().
			GetWorkspace(mock.Anything, parent.ID, nil).
			Return(parent, 1, nil).
			Once()
		mocks.operator.EXPECT().
			DownloadArtifact(mock.Anything, parent, entry, mock.Anything).
			Return(artifactOpFailure(
				goutils.NewBadInputError("destination directory does not exist", nil, true),
			)).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/artifacts/%s/load-in-volume", entry.ID),
			jsonBody(assert, api.LoadArtifactInVolumeRequest{
				TargetPath: models.WorkspaceMountPath + "/no/such/dir/report.json",
			}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactLoadVol, uut.LoadArtifactInVolume, req,
		)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	t.Run("unknown artifact answered before the workspace lookup", func(t *testing.T) {
		assert := assert.New(t)
		uut, mocks := buildArtifactAPIHandler(assert, t)

		artifactID := uuid.NewString()

		mocks.artifacts.EXPECT().
			GetArtifact(mock.Anything, artifactID, nil).
			Return(models.Artifact{}, artifactDBFailure(unknownArtifact(artifactID))).
			Once()

		req, err := http.NewRequest(
			"POST",
			fmt.Sprintf("/v1/artifacts/%s/load-in-volume", artifactID),
			jsonBody(assert, api.LoadArtifactInVolumeRequest{
				TargetPath: models.WorkspaceMountPath + "/report.json",
			}),
		)
		assert.Nil(err)

		respRecorder := serveOneArtifactRequest(
			uut, routeArtifactLoadVol, uut.LoadArtifactInVolume, req,
		)

		assert.Equal(http.StatusNotFound, respRecorder.Code)
	})
}
