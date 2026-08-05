package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alwitt/cairn/api"
	"github.com/alwitt/cairn/db"
	mockworkspace "github.com/alwitt/cairn/mocks/workspace"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// unitTestAppName the application name the handler under test is built with.
const unitTestAppName = "unit-test-app"

// The route patterns the handlers are registered under in BuildHTTPServer. The tests serve
// through a real router carrying these, so `{workspaceID}` resolves the way it does in
// production rather than being injected into the request context by hand.
const (
	routeWorkspaces         = "/v1/workspaces"
	routeWorkspace          = "/v1/workspaces/{workspaceID}"
	routeWorkspaceName      = "/v1/workspaces/{workspaceID}/name"
	routeWorkspaceDesc      = "/v1/workspaces/{workspaceID}/description"
	routeWorkspaceVolumeMet = "/v1/workspaces/{workspaceID}/volume-metadata"
	routeWorkspaceVolume    = "/v1/workspaces/{workspaceID}/volume"
)

// buildWorkspaceAPIHandler build the handler under test over a mock workspace manager, returning
// both. The manager mock is bound to `t`, so any manager call a test did not arrange fails it.
func buildWorkspaceAPIHandler(
	assert *assert.Assertions, t *testing.T,
) (api.WorkspaceAPIHandler, *mockworkspace.Manager) {
	mockManager := mockworkspace.NewManager(t)

	uut, err := api.NewWorkspaceAPIHandler(
		unitTestAppName, mockManager, models.HTTPRequestLogging{
			LogLevel:        goutils.HTTPLogLevelWARN,
			HealthLogLevel:  goutils.HTTPLogLevelWARN,
			RequestIDHeader: "unit-test",
			DoNotLogHeaders: []string{},
		}, nil,
	)
	assert.Nil(err)

	return uut, mockManager
}

// jsonBody marshal a request payload for use as a request body.
func jsonBody(assert *assert.Assertions, payload interface{}) io.Reader {
	serialized, err := json.Marshal(payload)
	assert.Nil(err)
	return bytes.NewReader(serialized)
}

// serveOneRequest run a request against a router carrying only the endpoint under test, wrapped
// in the same logging middleware the server installs.
func serveOneRequest(
	uut api.WorkspaceAPIHandler,
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

// sampleWorkspace build a workspace entry of the shape the manager returns.
func sampleWorkspace(name string) models.Workspace {
	workspaceID := uuid.NewString()
	return models.Workspace{
		ID:          workspaceID,
		Name:        name,
		VolumeName:  fmt.Sprintf("%s-%s", unitTestAppName, workspaceID),
		VolumeState: models.WorkspaceVolumeStateNone,
	}
}

// dbFailure build the error shape a manager DB call produces: the manager's own error stacked
// over a PersistenceError, over whichever goutils error the persistence layer raised. The API
// classifies on the innermost one, so the tests must hand it the real nesting.
func dbFailure(core error) error {
	return models.NewWorkspaceMangerError(
		"simulated manager failure",
		goutils.NewPersistenceError("simulated persistence failure", core, true),
		true,
	)
}

// volumeFailure build the error shape a manager volume call produces. The volume path has no
// persistence layer beneath it, so the manager's error sits directly over the docker failure.
func volumeFailure(core error) error {
	return models.NewWorkspaceMangerError("simulated manager failure", core, true)
}

// unknownWorkspace the persistence layer's answer for an ID that has no row.
func unknownWorkspace(workspaceID string) error {
	return goutils.NewNotFoundError(
		fmt.Sprintf("workspace '%s' does not exist", workspaceID), nil, true,
	)
}

// TestNewWorkspaceManagerHandler validates the constructor's input guards.
func TestNewWorkspaceManagerHandler(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	logConfig := models.HTTPRequestLogging{
		LogLevel:        goutils.HTTPLogLevelWARN,
		HealthLogLevel:  goutils.HTTPLogLevelWARN,
		RequestIDHeader: "unit-test",
		DoNotLogHeaders: []string{},
	}

	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)

		uut, err := api.NewWorkspaceAPIHandler(
			unitTestAppName, mockworkspace.NewManager(t), logConfig, nil,
		)
		assert.Nil(err)
		assert.NotNil(uut)
	})

	// The application name lands in every workspace's volume name, so it is held to the same
	// charset a volume name must satisfy - rejected at construction, not on first use.
	t.Run("invalid application name rejected", func(t *testing.T) {
		assert := assert.New(t)

		for _, appName := range []string{"", "has space", "has/slash", "has.dot"} {
			_, err := api.NewWorkspaceAPIHandler(
				appName, mockworkspace.NewManager(t), logConfig, nil,
			)
			assert.NotNil(err, "application name '%s' should be rejected", appName)
		}
	})
}

// TestWorkspaceAPIDefineNewWorkspace validates workspace creation.
func TestWorkspaceAPIDefineNewWorkspace(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		description := "unit test description"
		created := sampleWorkspace("unit-test-workspace")

		mockManager.EXPECT().
			DefineNewWorkspace(
				mock.Anything,
				created.Name,
				&description,
				(*models.WorkspaceVolumeMetadata)(nil),
				nil,
			).
			Return(created, nil).
			Once()

		req, err := http.NewRequest("POST", routeWorkspaces, jsonBody(assert, api.NewWorkspaceRequest{
			Name: created.Name, Description: &description,
		}))
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspaces, uut.DefineNewWorkspace, req)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.WorkspaceEntryResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		assert.Equal(created.ID, parsed.Workspace.ID)
		assert.Equal(created.Name, parsed.Workspace.Name)
	})

	// Case 1: the volume provisioning metadata reaches the manager unaltered, which is the only
	// way a workspace's volume can be given a requested capacity (see DESIGN §4.2).
	t.Run("volume metadata passes through", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		sizeBytes := int64(4096)
		metadata := models.WorkspaceVolumeMetadata{SizeBytes: &sizeBytes}
		created := sampleWorkspace("unit-test-workspace")

		mockManager.EXPECT().
			DefineNewWorkspace(mock.Anything, created.Name, (*string)(nil), &metadata, nil).
			Return(created, nil).
			Once()

		req, err := http.NewRequest("POST", routeWorkspaces, jsonBody(assert, api.NewWorkspaceRequest{
			Name: created.Name, VolumeMetadata: &metadata,
		}))
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspaces, uut.DefineNewWorkspace, req)

		assert.Equal(http.StatusOK, respRecorder.Code)
	})

	t.Run("malformed payload", func(t *testing.T) {
		assert := assert.New(t)
		uut, _ := buildWorkspaceAPIHandler(assert, t)

		req, err := http.NewRequest(
			"POST", routeWorkspaces, bytes.NewReader([]byte("{not-json")),
		)
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspaces, uut.DefineNewWorkspace, req)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	// Case 3: an invalid name is caught before the manager is consulted - the manager mock has
	// no expectations, so any call to it fails the case.
	t.Run("invalid name rejected before the manager", func(t *testing.T) {
		assert := assert.New(t)
		uut, _ := buildWorkspaceAPIHandler(assert, t)

		req, err := http.NewRequest("POST", routeWorkspaces, jsonBody(assert, api.NewWorkspaceRequest{
			Name: "not a valid name!",
		}))
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspaces, uut.DefineNewWorkspace, req)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	// Case 4: the validator descends into the volume metadata, so its own constraints are
	// enforced at the API boundary rather than only at the persistence layer.
	t.Run("invalid volume metadata rejected before the manager", func(t *testing.T) {
		assert := assert.New(t)
		uut, _ := buildWorkspaceAPIHandler(assert, t)

		zeroSize := int64(0)
		req, err := http.NewRequest("POST", routeWorkspaces, jsonBody(assert, api.NewWorkspaceRequest{
			Name:           "unit-test-workspace",
			VolumeMetadata: &models.WorkspaceVolumeMetadata{SizeBytes: &zeroSize},
		}))
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspaces, uut.DefineNewWorkspace, req)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	t.Run("persistence failure is a 500", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		mockManager.EXPECT().
			DefineNewWorkspace(
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
			).
			Return(models.Workspace{}, dbFailure(
				goutils.NewSQLError("insert failed", nil, true),
			)).
			Once()

		req, err := http.NewRequest("POST", routeWorkspaces, jsonBody(assert, api.NewWorkspaceRequest{
			Name: "unit-test-workspace",
		}))
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspaces, uut.DefineNewWorkspace, req)

		assert.Equal(http.StatusInternalServerError, respRecorder.Code)
	})
}

// TestWorkspaceAPIListWorkspaces validates listing and its query filter translation.
func TestWorkspaceAPIListWorkspaces(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	// Case 0: every supported query parameter reaches the manager in the right filter field.
	// Names are matched exactly rather than by similarity, since workspace names are unique.
	t.Run("query parameters become filters", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		offset, limit := 3, 10
		entries := []models.Workspace{
			sampleWorkspace("unit-test-one"), sampleWorkspace("unit-test-two"),
		}

		mockManager.EXPECT().
			ListWorkspaces(mock.Anything, db.WorkspaceQueryFilter{
				CommonListEntryQueryFilter: db.CommonListEntryQueryFilter{
					Offset: &offset, Limit: &limit,
				},
				TargetNames: []string{"unit-test-one", "unit-test-two"},
				VolumeStates: []models.WorkspaceVolumeStateENUM{
					models.WorkspaceVolumeStateReady,
				},
			}, nil).
			Return(entries, nil).
			Once()

		req, err := http.NewRequest(
			"GET",
			routeWorkspaces+
				"?name=unit-test-one&name=unit-test-two&volume_state=READY&offset=3&limit=10",
			nil,
		)
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspaces, uut.ListWorkspaces, req)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.WorkspaceListResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		assert.Len(parsed.Workspaces, 2)
	})

	t.Run("no filters", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		mockManager.EXPECT().
			ListWorkspaces(mock.Anything, db.WorkspaceQueryFilter{}, nil).
			Return([]models.Workspace{}, nil).
			Once()

		req, err := http.NewRequest("GET", routeWorkspaces, nil)
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspaces, uut.ListWorkspaces, req)

		assert.Equal(http.StatusOK, respRecorder.Code)
	})

	// Case 2: a non-numeric pagination parameter is answered without consulting the manager.
	t.Run("non-integer pagination rejected", func(t *testing.T) {
		for _, query := range []string{"?limit=many", "?offset=first"} {
			assert := assert.New(t)
			uut, _ := buildWorkspaceAPIHandler(assert, t)

			req, err := http.NewRequest("GET", routeWorkspaces+query, nil)
			assert.Nil(err)

			respRecorder := serveOneRequest(uut, routeWorkspaces, uut.ListWorkspaces, req)

			assert.Equal(http.StatusBadRequest, respRecorder.Code, "query '%s'", query)
		}
	})

	// Case 3: an unknown volume state is rejected by the persistence layer's filter validation,
	// which must reach the caller as a 400 rather than a 500.
	t.Run("filter validation failure is a 400", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		mockManager.EXPECT().
			ListWorkspaces(mock.Anything, mock.Anything, nil).
			Return(nil, dbFailure(
				goutils.NewValidationError("workspace query filter is not valid", nil, true),
			)).
			Once()

		req, err := http.NewRequest("GET", routeWorkspaces+"?volume_state=NOT_A_STATE", nil)
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspaces, uut.ListWorkspaces, req)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})
}

// TestWorkspaceAPIGetWorkspace validates fetching one workspace, including the mount count that
// only this endpoint carries.
func TestWorkspaceAPIGetWorkspace(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		entry := sampleWorkspace("unit-test-workspace")
		entry.VolumeState = models.WorkspaceVolumeStateReady

		mockManager.EXPECT().
			GetWorkspace(mock.Anything, entry.ID, nil).
			Return(entry, 2, nil).
			Once()

		req, err := http.NewRequest("GET", routeWorkspaces+"/"+entry.ID, nil)
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspace, uut.GetWorkspace, req)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.WorkspaceDetailResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		assert.Equal(entry.ID, parsed.Workspace.ID)
		assert.Equal(entry.VolumeName, parsed.Workspace.VolumeName)
		assert.Equal(2, parsed.MountCount)
	})

	// Case 1: docker could not be asked, so the count is the unavailable sentinel. The fetch
	// still answers - it must not fail just because docker is unreachable (see DESIGN §7.1).
	t.Run("unavailable mount count is reported as -1", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		entry := sampleWorkspace("unit-test-workspace")

		mockManager.EXPECT().
			GetWorkspace(mock.Anything, entry.ID, nil).
			Return(entry, -1, nil).
			Once()

		req, err := http.NewRequest("GET", routeWorkspaces+"/"+entry.ID, nil)
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspace, uut.GetWorkspace, req)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.WorkspaceDetailResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		assert.Equal(-1, parsed.MountCount)
	})

	t.Run("unknown workspace is a 404", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		workspaceID := uuid.NewString()
		mockManager.EXPECT().
			GetWorkspace(mock.Anything, workspaceID, nil).
			Return(models.Workspace{}, -1, dbFailure(unknownWorkspace(workspaceID))).
			Once()

		req, err := http.NewRequest("GET", routeWorkspaces+"/"+workspaceID, nil)
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspace, uut.GetWorkspace, req)

		assert.Equal(http.StatusNotFound, respRecorder.Code)
	})
}

// TestWorkspaceAPIUpdateWorkspaceName validates renaming.
func TestWorkspaceAPIUpdateWorkspaceName(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		entry := sampleWorkspace("unit-test-original")
		newName := "unit-test-renamed"
		renamed := entry
		renamed.Name = newName

		mockManager.EXPECT().
			UpdateWorkspaceName(mock.Anything, entry.ID, newName, nil).
			Return(renamed, nil).
			Once()

		req, err := http.NewRequest(
			"PUT", routeWorkspaces+"/"+entry.ID+"/name?name="+newName, nil,
		)
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspaceName, uut.UpdateWorkspaceName, req)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.WorkspaceEntryResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		assert.Equal(newName, parsed.Workspace.Name)
		// The volume name is ID-derived, so a rename must leave it untouched (DESIGN §2.1).
		assert.Equal(entry.VolumeName, parsed.Workspace.VolumeName)
	})

	t.Run("missing name parameter rejected before the manager", func(t *testing.T) {
		assert := assert.New(t)
		uut, _ := buildWorkspaceAPIHandler(assert, t)

		req, err := http.NewRequest(
			"PUT", routeWorkspaces+"/"+uuid.NewString()+"/name", nil,
		)
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspaceName, uut.UpdateWorkspaceName, req)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	// Case 2: the charset check lives in the persistence layer, so an invalid new name comes
	// back as a validation failure the API must answer 400 to.
	t.Run("invalid new name is a 400", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		workspaceID := uuid.NewString()
		mockManager.EXPECT().
			UpdateWorkspaceName(mock.Anything, workspaceID, mock.Anything, nil).
			Return(models.Workspace{}, dbFailure(
				goutils.NewValidationError("new name is not valid", nil, true),
			)).
			Once()

		req, err := http.NewRequest(
			"PUT", routeWorkspaces+"/"+workspaceID+"/name?name=not.valid", nil,
		)
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspaceName, uut.UpdateWorkspaceName, req)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	t.Run("unknown workspace is a 404", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		workspaceID := uuid.NewString()
		mockManager.EXPECT().
			UpdateWorkspaceName(mock.Anything, workspaceID, mock.Anything, nil).
			Return(models.Workspace{}, dbFailure(unknownWorkspace(workspaceID))).
			Once()

		req, err := http.NewRequest(
			"PUT", routeWorkspaces+"/"+workspaceID+"/name?name=unit-test-renamed", nil,
		)
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspaceName, uut.UpdateWorkspaceName, req)

		assert.Equal(http.StatusNotFound, respRecorder.Code)
	})
}

// TestWorkspaceAPIUpdateWorkspaceDescription validates description changes, including clearing.
func TestWorkspaceAPIUpdateWorkspaceDescription(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		entry := sampleWorkspace("unit-test-workspace")
		newDescription := "unit test description"
		updated := entry
		updated.Description = &newDescription

		mockManager.EXPECT().
			UpdateWorkspaceDescription(mock.Anything, entry.ID, &newDescription, nil).
			Return(updated, nil).
			Once()

		req, err := http.NewRequest(
			"PUT",
			routeWorkspaces+"/"+entry.ID+"/description",
			jsonBody(assert, api.UpdateWorkspaceDescriptionRequest{Description: &newDescription}),
		)
		assert.Nil(err)

		respRecorder := serveOneRequest(
			uut, routeWorkspaceDesc, uut.UpdateWorkspaceDescription, req,
		)

		assert.Equal(http.StatusOK, respRecorder.Code)
		var parsed api.WorkspaceEntryResponse
		assert.Nil(json.Unmarshal(respRecorder.Body.Bytes(), &parsed))
		assert.NotNil(parsed.Workspace.Description)
		assert.Equal(newDescription, *parsed.Workspace.Description)
	})

	// Case 1: an explicit null is a clear instruction rather than an absent field, which is why
	// the description travels in a body instead of a query parameter.
	t.Run("explicit null clears the description", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		entry := sampleWorkspace("unit-test-workspace")

		mockManager.EXPECT().
			UpdateWorkspaceDescription(mock.Anything, entry.ID, (*string)(nil), nil).
			Return(entry, nil).
			Once()

		req, err := http.NewRequest(
			"PUT",
			routeWorkspaces+"/"+entry.ID+"/description",
			bytes.NewReader([]byte(`{"description": null}`)),
		)
		assert.Nil(err)

		respRecorder := serveOneRequest(
			uut, routeWorkspaceDesc, uut.UpdateWorkspaceDescription, req,
		)

		assert.Equal(http.StatusOK, respRecorder.Code)
	})

	t.Run("malformed payload", func(t *testing.T) {
		assert := assert.New(t)
		uut, _ := buildWorkspaceAPIHandler(assert, t)

		req, err := http.NewRequest(
			"PUT",
			routeWorkspaces+"/"+uuid.NewString()+"/description",
			bytes.NewReader([]byte("{not-json")),
		)
		assert.Nil(err)

		respRecorder := serveOneRequest(
			uut, routeWorkspaceDesc, uut.UpdateWorkspaceDescription, req,
		)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	t.Run("unknown workspace is a 404", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		workspaceID := uuid.NewString()
		mockManager.EXPECT().
			UpdateWorkspaceDescription(mock.Anything, workspaceID, mock.Anything, nil).
			Return(models.Workspace{}, dbFailure(unknownWorkspace(workspaceID))).
			Once()

		req, err := http.NewRequest(
			"PUT",
			routeWorkspaces+"/"+workspaceID+"/description",
			bytes.NewReader([]byte(`{"description": null}`)),
		)
		assert.Nil(err)

		respRecorder := serveOneRequest(
			uut, routeWorkspaceDesc, uut.UpdateWorkspaceDescription, req,
		)

		assert.Equal(http.StatusNotFound, respRecorder.Code)
	})
}

// TestWorkspaceAPIUpdateWorkspaceVolumeMeta validates volume provisioning metadata changes.
func TestWorkspaceAPIUpdateWorkspaceVolumeMeta(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		entry := sampleWorkspace("unit-test-workspace")
		sizeBytes := int64(8192)
		metadata := models.WorkspaceVolumeMetadata{SizeBytes: &sizeBytes}

		mockManager.EXPECT().
			UpdateWorkspaceVolumeMeta(mock.Anything, entry.ID, &metadata, nil).
			Return(entry, nil).
			Once()

		req, err := http.NewRequest(
			"PUT",
			routeWorkspaces+"/"+entry.ID+"/volume-metadata",
			jsonBody(assert, api.UpdateWorkspaceVolumeMetaRequest{VolumeMetadata: &metadata}),
		)
		assert.Nil(err)

		respRecorder := serveOneRequest(
			uut, routeWorkspaceVolumeMet, uut.UpdateWorkspaceVolumeMeta, req,
		)

		assert.Equal(http.StatusOK, respRecorder.Code)
	})

	// Case 1: as with the description, an explicit null clears it - returning the workspace to
	// the deployment's default provisioning parameters.
	t.Run("explicit null clears the metadata", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		entry := sampleWorkspace("unit-test-workspace")

		mockManager.EXPECT().
			UpdateWorkspaceVolumeMeta(
				mock.Anything, entry.ID, (*models.WorkspaceVolumeMetadata)(nil), nil,
			).
			Return(entry, nil).
			Once()

		req, err := http.NewRequest(
			"PUT",
			routeWorkspaces+"/"+entry.ID+"/volume-metadata",
			bytes.NewReader([]byte(`{"volume_metadata": null}`)),
		)
		assert.Nil(err)

		respRecorder := serveOneRequest(
			uut, routeWorkspaceVolumeMet, uut.UpdateWorkspaceVolumeMeta, req,
		)

		assert.Equal(http.StatusOK, respRecorder.Code)
	})

	t.Run("invalid metadata rejected before the manager", func(t *testing.T) {
		assert := assert.New(t)
		uut, _ := buildWorkspaceAPIHandler(assert, t)

		zeroSize := int64(0)
		req, err := http.NewRequest(
			"PUT",
			routeWorkspaces+"/"+uuid.NewString()+"/volume-metadata",
			jsonBody(assert, api.UpdateWorkspaceVolumeMetaRequest{
				VolumeMetadata: &models.WorkspaceVolumeMetadata{SizeBytes: &zeroSize},
			}),
		)
		assert.Nil(err)

		respRecorder := serveOneRequest(
			uut, routeWorkspaceVolumeMet, uut.UpdateWorkspaceVolumeMeta, req,
		)

		assert.Equal(http.StatusBadRequest, respRecorder.Code)
	})

	// Case 3: the metadata only describes how to provision a volume, so the edit is refused once
	// one exists (see DESIGN §4.2) - a precondition the operator can act on, hence 409.
	t.Run("edit refused on an existing volume is a 409", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		workspaceID := uuid.NewString()
		mockManager.EXPECT().
			UpdateWorkspaceVolumeMeta(mock.Anything, workspaceID, mock.Anything, nil).
			Return(models.Workspace{}, dbFailure(goutils.NewConsistencyError(
				"workspace already has a persistent volume", nil, true,
			))).
			Once()

		req, err := http.NewRequest(
			"PUT",
			routeWorkspaces+"/"+workspaceID+"/volume-metadata",
			bytes.NewReader([]byte(`{"volume_metadata": null}`)),
		)
		assert.Nil(err)

		respRecorder := serveOneRequest(
			uut, routeWorkspaceVolumeMet, uut.UpdateWorkspaceVolumeMeta, req,
		)

		assert.Equal(http.StatusConflict, respRecorder.Code)
	})
}

// TestWorkspaceAPIDeleteWorkspace validates deletion and its volume guard.
func TestWorkspaceAPIDeleteWorkspace(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		workspaceID := uuid.NewString()
		mockManager.EXPECT().
			DeleteWorkspace(mock.Anything, workspaceID, nil).
			Return(nil).
			Once()

		req, err := http.NewRequest("DELETE", routeWorkspaces+"/"+workspaceID, nil)
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspace, uut.DeleteWorkspace, req)

		assert.Equal(http.StatusOK, respRecorder.Code)
	})

	// Case 1: teardown is bottom-up - the volume must be gone before the record can be (see
	// DESIGN §4.3). The refusal is a precondition the operator resolves by deleting the volume.
	t.Run("workspace with a live volume is a 409", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		workspaceID := uuid.NewString()
		mockManager.EXPECT().
			DeleteWorkspace(mock.Anything, workspaceID, nil).
			Return(dbFailure(goutils.NewConsistencyError(
				"workspace still has a persistent volume; delete the volume first", nil, true,
			))).
			Once()

		req, err := http.NewRequest("DELETE", routeWorkspaces+"/"+workspaceID, nil)
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspace, uut.DeleteWorkspace, req)

		assert.Equal(http.StatusConflict, respRecorder.Code)
	})

	t.Run("unknown workspace is a 404", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		workspaceID := uuid.NewString()
		mockManager.EXPECT().
			DeleteWorkspace(mock.Anything, workspaceID, nil).
			Return(dbFailure(unknownWorkspace(workspaceID))).
			Once()

		req, err := http.NewRequest("DELETE", routeWorkspaces+"/"+workspaceID, nil)
		assert.Nil(err)

		respRecorder := serveOneRequest(uut, routeWorkspace, uut.DeleteWorkspace, req)

		assert.Equal(http.StatusNotFound, respRecorder.Code)
	})
}

// TestWorkspaceAPISetupWorkspaceVolume validates volume provisioning.
func TestWorkspaceAPISetupWorkspaceVolume(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		entry := sampleWorkspace("unit-test-workspace")

		mockManager.EXPECT().
			GetWorkspace(mock.Anything, entry.ID, nil).
			Return(entry, -1, nil).
			Once()
		mockManager.EXPECT().
			SetupWorkspaceVolume(mock.Anything, entry, nil).
			Return(nil).
			Once()

		req, err := http.NewRequest("POST", routeWorkspaces+"/"+entry.ID+"/volume", nil)
		assert.Nil(err)

		respRecorder := serveOneRequest(
			uut, routeWorkspaceVolume, uut.SetupWorkspaceVolume, req,
		)

		assert.Equal(http.StatusOK, respRecorder.Code)
	})

	// Case 1: the workspace record carries the volume name and provisioning metadata, so an
	// unknown workspace is answered before docker is touched - the volume call is unarranged,
	// so making it fails the case.
	t.Run("unknown workspace is answered before docker", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		workspaceID := uuid.NewString()
		mockManager.EXPECT().
			GetWorkspace(mock.Anything, workspaceID, nil).
			Return(models.Workspace{}, -1, dbFailure(unknownWorkspace(workspaceID))).
			Once()

		req, err := http.NewRequest("POST", routeWorkspaces+"/"+workspaceID+"/volume", nil)
		assert.Nil(err)

		respRecorder := serveOneRequest(
			uut, routeWorkspaceVolume, uut.SetupWorkspaceVolume, req,
		)

		assert.Equal(http.StatusNotFound, respRecorder.Code)
	})

	// Case 2: an unreachable docker daemon is not something the caller can correct.
	t.Run("docker failure is a 500", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		entry := sampleWorkspace("unit-test-workspace")

		mockManager.EXPECT().
			GetWorkspace(mock.Anything, entry.ID, nil).
			Return(entry, -1, nil).
			Once()
		mockManager.EXPECT().
			SetupWorkspaceVolume(mock.Anything, entry, nil).
			Return(volumeFailure(goutils.NewDockerError("failed to define volume", nil, true))).
			Once()

		req, err := http.NewRequest("POST", routeWorkspaces+"/"+entry.ID+"/volume", nil)
		assert.Nil(err)

		respRecorder := serveOneRequest(
			uut, routeWorkspaceVolume, uut.SetupWorkspaceVolume, req,
		)

		assert.Equal(http.StatusInternalServerError, respRecorder.Code)
	})
}

// TestWorkspaceAPITeardownWorkspaceVolume validates volume deletion, whose in-use refusal comes
// from the docker daemon rather than from any check cairn performs.
func TestWorkspaceAPITeardownWorkspaceVolume(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("happy path", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		entry := sampleWorkspace("unit-test-workspace")
		entry.VolumeState = models.WorkspaceVolumeStateReady

		mockManager.EXPECT().
			GetWorkspace(mock.Anything, entry.ID, nil).
			Return(entry, 0, nil).
			Once()
		mockManager.EXPECT().
			TeardownWorkspaceVolume(mock.Anything, entry, nil).
			Return(nil).
			Once()

		req, err := http.NewRequest("DELETE", routeWorkspaces+"/"+entry.ID+"/volume", nil)
		assert.Nil(err)

		respRecorder := serveOneRequest(
			uut, routeWorkspaceVolume, uut.TeardownWorkspaceVolume, req,
		)

		assert.Equal(http.StatusOK, respRecorder.Code)
	})

	// Case 1: the daemon refuses to remove a mounted volume, and that refusal arrives as a
	// ConsistencyError (see DESIGN §4.3). The operator resolves it by stopping the mounters.
	t.Run("volume still mounted is a 409", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		entry := sampleWorkspace("unit-test-workspace")
		entry.VolumeState = models.WorkspaceVolumeStateReady

		mockManager.EXPECT().
			GetWorkspace(mock.Anything, entry.ID, nil).
			Return(entry, 3, nil).
			Once()
		mockManager.EXPECT().
			TeardownWorkspaceVolume(mock.Anything, entry, nil).
			Return(volumeFailure(goutils.NewConsistencyError(
				"volume is in use", nil, true,
			))).
			Once()

		req, err := http.NewRequest("DELETE", routeWorkspaces+"/"+entry.ID+"/volume", nil)
		assert.Nil(err)

		respRecorder := serveOneRequest(
			uut, routeWorkspaceVolume, uut.TeardownWorkspaceVolume, req,
		)

		assert.Equal(http.StatusConflict, respRecorder.Code)
	})

	t.Run("unknown workspace is answered before docker", func(t *testing.T) {
		assert := assert.New(t)
		uut, mockManager := buildWorkspaceAPIHandler(assert, t)

		workspaceID := uuid.NewString()
		mockManager.EXPECT().
			GetWorkspace(mock.Anything, workspaceID, nil).
			Return(models.Workspace{}, -1, dbFailure(unknownWorkspace(workspaceID))).
			Once()

		req, err := http.NewRequest("DELETE", routeWorkspaces+"/"+workspaceID+"/volume", nil)
		assert.Nil(err)

		respRecorder := serveOneRequest(
			uut, routeWorkspaceVolume, uut.TeardownWorkspaceVolume, req,
		)

		assert.Equal(http.StatusNotFound, respRecorder.Code)
	})
}
