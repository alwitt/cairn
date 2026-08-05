// Package api - application REST API
package api //revive:disable-line:var-naming

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/cairn/workspace"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
	"github.com/go-playground/validator/v10"
	"github.com/gorilla/mux"
)

// WorkspaceAPIHandler workspace management API handler
type WorkspaceAPIHandler struct {
	goutils.RestAPIHandler

	validator *validator.Validate

	// manager core workspace manager
	manager workspace.Manager
}

/*
NewWorkspaceAPIHandler new workspace management API handler

	@param appName string - the per-deployment application name which namespaces this
	    deployment's persistent volumes. A workspace's volume name is derived from it (see
	    DESIGN §2.1).
	@param manager workspace.Manager - workspace manager
	@param logConfig common.HTTPRequestLogging - handler log settings
	@param metrics goutils.HTTPRequestMetricHelper - metric collection agent
	@returns new handler
*/
func NewWorkspaceAPIHandler(
	appName string,
	manager workspace.Manager,
	logConfig models.HTTPRequestLogging,
	metrics goutils.HTTPRequestMetricHelper,
) (WorkspaceAPIHandler, error) {
	validate := validator.New()
	if err := models.RegisterWithValidator(validate); err != nil {
		return WorkspaceAPIHandler{}, goutils.NewRuntimeError(
			"failed to install custom validation macros", err, true,
		)
	}

	// The application name lands in every workspace's volume name, so hold it to the same
	// charset the volume name must satisfy.
	if err := validate.Var(appName, "required,valid_name"); err != nil {
		return WorkspaceAPIHandler{}, goutils.NewValidationError(
			fmt.Sprintf("application name '%s' is not valid", appName), err, true,
		)
	}

	handler := WorkspaceAPIHandler{
		RestAPIHandler: goutils.RestAPIHandler{
			Component: goutils.Component{
				LogTags: log.Fields{
					"package":   "cairn",
					"module":    "api",
					"component": "workspace-api-handler",
					"instance":  appName,
				},
				LogTagModifiers: []goutils.LogMetadataModifier{
					goutils.ModifyLogMetadataByRestRequestParam,
				},
			},
			CallRequestIDHeaderField: &logConfig.RequestIDHeader,
			DoNotLogHeaders: func() map[string]bool {
				result := map[string]bool{}
				for _, v := range logConfig.DoNotLogHeaders {
					result[v] = true
				}
				return result
			}(),
			LogLevel:          logConfig.LogLevel,
			LogRequestPayload: logConfig.LogRequestPayload,
			MetricsHelper:     metrics,
		},
		validator: validate,
		manager:   manager,
	}

	return handler, nil
}

// ======================================================================================
// Workspace CRUD - Create Workspace

// NewWorkspaceRequest parameters to define a new workspace
type NewWorkspaceRequest struct {
	// Name of the workspace, can only contain alphanumeric characters, `-`, and `_`
	Name string `json:"name" validate:"required,valid_name"`
	// Description an optional description for the workspace
	Description *string `json:"description,omitempty" validate:"omitempty"`
	// VolumeMetadata optional provisioning parameters for the workspace's persistent volume.
	// Recorded now and only read when the volume is provisioned; omit it to take the
	// deployment's defaults.
	VolumeMetadata *models.WorkspaceVolumeMetadata `json:"volume_metadata,omitempty" validate:"omitempty"`
}

// WorkspaceEntryResponse response containing one workspace
type WorkspaceEntryResponse struct {
	goutils.RestAPIBaseResponse
	// Workspace the workspace
	Workspace models.Workspace `json:"workspace" validate:"required"`
}

// DefineNewWorkspace godoc
// @Summary Define a new workspace
// @Description Define a new workspace. This creates the DB record only; the workspace's
// @Description persistent volume is provisioned separately.
// @tags management
// @Accept json
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param param body NewWorkspaceRequest true "Workspace parameters"
// @Success 200 {object} WorkspaceEntryResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/workspaces [post]
func (h WorkspaceAPIHandler) DefineNewWorkspace(w http.ResponseWriter, r *http.Request) {
	var respCode int
	var response interface{}
	var handlerError error
	var errorMsg string
	logTags := h.GetLogTagsForContext(r.Context())
	defer func() {
		if handlerError != nil {
			logAPIHandlerError(logTags, handlerError, errorMsg)
			response = h.GetStdRESTErrorMsg(
				r.Context(), respCode, errorMsg, handlerError.Error(),
			)
		}
		if err := h.WriteRESTResponse(w, respCode, response, nil); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				Error("Failed to form response")
		}
	}()

	if r.Body == nil {
		msg := "No payload provided to define new workspace"
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = goutils.NewValidationError(msg, nil, true)
		errorMsg, respCode = msg, http.StatusBadRequest
		return
	}

	// Parse the create parameters
	var params NewWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "Unable to parse new workspace parameters from request"
		respCode = http.StatusBadRequest
		return
	}
	defer func() {
		if err := r.Body.Close(); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				Error("Request body close error")
		}
	}()

	{
		t, _ := json.Marshal(&params)
		log.
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			WithField("new-workspace", string(t)).
			Debug("Defining new workspace")
	}

	// Validate parameters. The validator descends into the volume metadata, so its own
	// constraints are enforced here rather than only at the persistence layer.
	if err := h.validator.Struct(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "New workspace parameters not valid"
		respCode = http.StatusBadRequest
		return
	}

	newWorkspace, err := h.manager.DefineNewWorkspace(
		r.Context(), params.Name, params.Description, params.VolumeMetadata, nil,
	)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to define new workspace '%s'", params.Name)
		respCode = mapErrorToStatus(err)
		return
	}

	// Return the new workspace
	respCode = http.StatusOK
	response = WorkspaceEntryResponse{
		RestAPIBaseResponse: h.GetStdRESTSuccessMsg(r.Context()), Workspace: newWorkspace,
	}
}

// ======================================================================================
// Workspace CRUD - List Workspaces

// WorkspaceListResponse response containing a list of workspaces
type WorkspaceListResponse struct {
	goutils.RestAPIBaseResponse
	// Workspaces the list of workspaces
	Workspaces []models.Workspace `json:"workspaces,omitempty" validate:"omitempty,dive"`
}

// ListWorkspaces godoc
// @Summary List workspaces
// @Description List the known workspaces, with optional filtering
// @tags management
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param name query []string false "Filter by workspace name" collectionFormat(multi)
// @Param volume_state query []string false "Filter by persistent volume state" collectionFormat(multi) Enums(NONE, READY)
// @Param offset query int false "Number of leading entries to skip"
// @Param limit query int false "Max number of entries to return"
// @Success 200 {object} WorkspaceListResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/workspaces [get]
func (h WorkspaceAPIHandler) ListWorkspaces(w http.ResponseWriter, r *http.Request) {
	var respCode int
	var response interface{}
	var handlerError error
	var errorMsg string
	logTags := h.GetLogTagsForContext(r.Context())
	defer func() {
		if handlerError != nil {
			logAPIHandlerError(logTags, handlerError, errorMsg)
			response = h.GetStdRESTErrorMsg(
				r.Context(), respCode, errorMsg, handlerError.Error(),
			)
		}
		if err := h.WriteRESTResponse(w, respCode, response, nil); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				Error("Failed to form response")
		}
	}()

	query := r.URL.Query()
	var filters db.WorkspaceQueryFilter

	// Parse pagination parameters
	if raw := query.Get("offset"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			goutils.UpdateCodePositionInTags(logTags)
			handlerError = err
			errorMsg = "Query parameter 'offset' must be an integer"
			respCode = http.StatusBadRequest
			return
		}
		filters.Offset = &parsed
	}
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			goutils.UpdateCodePositionInTags(logTags)
			handlerError = err
			errorMsg = "Query parameter 'limit' must be an integer"
			respCode = http.StatusBadRequest
			return
		}
		filters.Limit = &parsed
	}

	// Parse the name filter list. Workspace names are unique, so this selects an exact set
	// rather than performing a similarity search.
	filters.TargetNames = append(filters.TargetNames, query["name"]...)

	// Parse the volume state filter list. The values are validated by the persistence layer,
	// which reports an unknown state as a validation failure.
	for _, entry := range query["volume_state"] {
		filters.VolumeStates = append(
			filters.VolumeStates, models.WorkspaceVolumeStateENUM(entry),
		)
	}

	{
		t, _ := json.Marshal(&filters)
		log.
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			WithField("filters", string(t)).
			Debug("Listing workspaces")
	}

	workspaces, err := h.manager.ListWorkspaces(r.Context(), filters, nil)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "Failed to list workspaces"
		respCode = mapErrorToStatus(err)
		return
	}

	// Return the workspaces
	respCode = http.StatusOK
	response = WorkspaceListResponse{
		RestAPIBaseResponse: h.GetStdRESTSuccessMsg(r.Context()), Workspaces: workspaces,
	}
}

// ======================================================================================
// Workspace CRUD - Get One Workspace

// WorkspaceDetailResponse response containing one workspace and its runtime volume detail
type WorkspaceDetailResponse struct {
	goutils.RestAPIBaseResponse
	// Workspace the workspace
	Workspace models.Workspace `json:"workspace" validate:"required"`
	// MountCount estimated number of entities currently mounting the workspace's persistent
	// volume. Only docker can answer this, so `-1` means it could not be determined - which
	// includes the ordinary case of the workspace having no volume yet (see DESIGN §4.3).
	MountCount int `json:"mount_count"`
}

// GetWorkspace godoc
// @Summary Get one workspace
// @Description Fetch one workspace by its ID, along with an estimate of how many entities
// @Description currently mount its persistent volume (-1 when that can't be determined)
// @tags management
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param workspaceID path string true "Workspace ID"
// @Success 200 {object} WorkspaceDetailResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/workspaces/{workspaceID} [get]
func (h WorkspaceAPIHandler) GetWorkspace(w http.ResponseWriter, r *http.Request) {
	var respCode int
	var response interface{}
	var handlerError error
	var errorMsg string
	logTags := h.GetLogTagsForContext(r.Context())
	defer func() {
		if handlerError != nil {
			logAPIHandlerError(logTags, handlerError, errorMsg)
			response = h.GetStdRESTErrorMsg(
				r.Context(), respCode, errorMsg, handlerError.Error(),
			)
		}
		if err := h.WriteRESTResponse(w, respCode, response, nil); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				Error("Failed to form response")
		}
	}()

	workspaceID := mux.Vars(r)["workspaceID"]

	entry, mountCount, err := h.manager.GetWorkspace(r.Context(), workspaceID, nil)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to fetch workspace %s", workspaceID)
		respCode = mapErrorToStatus(err)
		return
	}

	// Return the workspace
	respCode = http.StatusOK
	response = WorkspaceDetailResponse{
		RestAPIBaseResponse: h.GetStdRESTSuccessMsg(r.Context()),
		Workspace:           entry,
		MountCount:          mountCount,
	}
}

// ======================================================================================
// Workspace CRUD - Update Workspace Name

// UpdateWorkspaceName godoc
// @Summary Change workspace name
// @Description Change the name of a workspace. This is a pure DB update with no volume guard:
// @Description the volume name is derived from the immutable workspace ID, so a rename never
// @Description affects the volume, even a live mounted one.
// @tags management
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param workspaceID path string true "Workspace ID"
// @Param name query string true "New workspace name, can only contain alphanumeric characters, - and _"
// @Success 200 {object} WorkspaceEntryResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/workspaces/{workspaceID}/name [put]
func (h WorkspaceAPIHandler) UpdateWorkspaceName(w http.ResponseWriter, r *http.Request) {
	var respCode int
	var response interface{}
	var handlerError error
	var errorMsg string
	logTags := h.GetLogTagsForContext(r.Context())
	defer func() {
		if handlerError != nil {
			logAPIHandlerError(logTags, handlerError, errorMsg)
			response = h.GetStdRESTErrorMsg(
				r.Context(), respCode, errorMsg, handlerError.Error(),
			)
		}
		if err := h.WriteRESTResponse(w, respCode, response, nil); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				Error("Failed to form response")
		}
	}()

	workspaceID := mux.Vars(r)["workspaceID"]

	// Parse the new name
	newName := r.URL.Query().Get("name")
	if newName == "" {
		msg := "Query parameter 'name' is required"
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = goutils.NewValidationError(msg, nil, true)
		errorMsg, respCode = msg, http.StatusBadRequest
		return
	}

	// Apply the new name
	updated, err := h.manager.UpdateWorkspaceName(r.Context(), workspaceID, newName, nil)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf(
			"Failed to update workspace %s name to '%s'", workspaceID, newName,
		)
		respCode = mapErrorToStatus(err)
		return
	}

	// Return the updated workspace
	respCode = http.StatusOK
	response = WorkspaceEntryResponse{
		RestAPIBaseResponse: h.GetStdRESTSuccessMsg(r.Context()), Workspace: updated,
	}
}

// ======================================================================================
// Workspace CRUD - Update Workspace Description

// UpdateWorkspaceDescriptionRequest parameters to change a workspace description
type UpdateWorkspaceDescriptionRequest struct {
	// Description new workspace description, set to null to clear
	Description *string `json:"description" validate:"omitempty"`
}

// UpdateWorkspaceDescription godoc
// @Summary Change workspace description
// @Description Change the description of a workspace
// @tags management
// @Accept json
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param workspaceID path string true "Workspace ID"
// @Param param body UpdateWorkspaceDescriptionRequest true "New workspace description"
// @Success 200 {object} WorkspaceEntryResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/workspaces/{workspaceID}/description [put]
func (h WorkspaceAPIHandler) UpdateWorkspaceDescription(w http.ResponseWriter, r *http.Request) {
	var respCode int
	var response interface{}
	var handlerError error
	var errorMsg string
	logTags := h.GetLogTagsForContext(r.Context())
	defer func() {
		if handlerError != nil {
			logAPIHandlerError(logTags, handlerError, errorMsg)
			response = h.GetStdRESTErrorMsg(
				r.Context(), respCode, errorMsg, handlerError.Error(),
			)
		}
		if err := h.WriteRESTResponse(w, respCode, response, nil); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				Error("Failed to form response")
		}
	}()

	workspaceID := mux.Vars(r)["workspaceID"]

	if r.Body == nil {
		msg := "No payload provided to update workspace description"
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = goutils.NewValidationError(msg, nil, true)
		errorMsg, respCode = msg, http.StatusBadRequest
		return
	}

	// Parse the new description. It is carried in a body rather than a query parameter so an
	// explicit null can clear it, which an absent parameter could not express.
	var params UpdateWorkspaceDescriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "Unable to parse new workspace description from request"
		respCode = http.StatusBadRequest
		return
	}
	defer func() {
		if err := r.Body.Close(); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				Error("Request body close error")
		}
	}()

	{
		t, _ := json.Marshal(&params)
		log.
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			WithField("new-description", string(t)).
			Debug("Updating workspace description")
	}

	// Apply the new description
	updated, err := h.manager.UpdateWorkspaceDescription(
		r.Context(), workspaceID, params.Description, nil,
	)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to update workspace %s description", workspaceID)
		respCode = mapErrorToStatus(err)
		return
	}

	// Return the updated workspace
	respCode = http.StatusOK
	response = WorkspaceEntryResponse{
		RestAPIBaseResponse: h.GetStdRESTSuccessMsg(r.Context()), Workspace: updated,
	}
}

// ======================================================================================
// Workspace CRUD - Update Workspace Volume Metadata

// UpdateWorkspaceVolumeMetaRequest parameters to change a workspace's persistent volume
// provisioning metadata
type UpdateWorkspaceVolumeMetaRequest struct {
	// VolumeMetadata the new volume provisioning metadata, set to null to clear it and take the
	// deployment's default provisioning parameters
	VolumeMetadata *models.WorkspaceVolumeMetadata `json:"volume_metadata" validate:"omitempty"`
}

// UpdateWorkspaceVolumeMeta godoc
// @Summary Change workspace volume provisioning metadata
// @Description Change the provisioning parameters recorded for a workspace's persistent volume.
// @Description Only permitted while the workspace has no volume - the metadata describes how to
// @Description provision one, so it can no longer be edited once one exists.
// @tags management
// @Accept json
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param workspaceID path string true "Workspace ID"
// @Param param body UpdateWorkspaceVolumeMetaRequest true "New volume provisioning metadata"
// @Success 200 {object} WorkspaceEntryResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 409 {object} goutils.RestAPIBaseResponse "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/workspaces/{workspaceID}/volume-metadata [put]
func (h WorkspaceAPIHandler) UpdateWorkspaceVolumeMeta(w http.ResponseWriter, r *http.Request) {
	var respCode int
	var response interface{}
	var handlerError error
	var errorMsg string
	logTags := h.GetLogTagsForContext(r.Context())
	defer func() {
		if handlerError != nil {
			logAPIHandlerError(logTags, handlerError, errorMsg)
			response = h.GetStdRESTErrorMsg(
				r.Context(), respCode, errorMsg, handlerError.Error(),
			)
		}
		if err := h.WriteRESTResponse(w, respCode, response, nil); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				Error("Failed to form response")
		}
	}()

	workspaceID := mux.Vars(r)["workspaceID"]

	if r.Body == nil {
		msg := "No payload provided to update workspace volume metadata"
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = goutils.NewValidationError(msg, nil, true)
		errorMsg, respCode = msg, http.StatusBadRequest
		return
	}

	// Parse the new metadata. As with the description, an explicit null is the clear
	// instruction, so it travels in a body.
	var params UpdateWorkspaceVolumeMetaRequest
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "Unable to parse new workspace volume metadata from request"
		respCode = http.StatusBadRequest
		return
	}
	defer func() {
		if err := r.Body.Close(); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				Error("Request body close error")
		}
	}()

	{
		t, _ := json.Marshal(&params)
		log.
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			WithField("new-volume-metadata", string(t)).
			Debug("Updating workspace volume metadata")
	}

	if err := h.validator.Struct(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "New workspace volume metadata not valid"
		respCode = http.StatusBadRequest
		return
	}

	// Apply the new metadata
	updated, err := h.manager.UpdateWorkspaceVolumeMeta(
		r.Context(), workspaceID, params.VolumeMetadata, nil,
	)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to update workspace %s volume metadata", workspaceID)
		respCode = mapErrorToStatus(err)
		return
	}

	// Return the updated workspace
	respCode = http.StatusOK
	response = WorkspaceEntryResponse{
		RestAPIBaseResponse: h.GetStdRESTSuccessMsg(r.Context()), Workspace: updated,
	}
}

// ======================================================================================
// Workspace CRUD - Delete Workspace

// DeleteWorkspace godoc
// @Summary Delete a workspace
// @Description Delete a workspace, cascading to its artifact records. Refused while the
// @Description workspace still has a persistent volume - delete the volume first. No object
// @Description store interaction: the freed objects are reclaimed later by the maintenance loop.
// @tags management
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param workspaceID path string true "Workspace ID"
// @Success 200 {object} goutils.RestAPIBaseResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 409 {object} goutils.RestAPIBaseResponse "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/workspaces/{workspaceID} [delete]
func (h WorkspaceAPIHandler) DeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	var respCode int
	var response interface{}
	var handlerError error
	var errorMsg string
	logTags := h.GetLogTagsForContext(r.Context())
	defer func() {
		if handlerError != nil {
			logAPIHandlerError(logTags, handlerError, errorMsg)
			response = h.GetStdRESTErrorMsg(
				r.Context(), respCode, errorMsg, handlerError.Error(),
			)
		}
		if err := h.WriteRESTResponse(w, respCode, response, nil); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				Error("Failed to form response")
		}
	}()

	workspaceID := mux.Vars(r)["workspaceID"]

	if err := h.manager.DeleteWorkspace(r.Context(), workspaceID, nil); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to delete workspace %s", workspaceID)
		respCode = mapErrorToStatus(err)
		return
	}

	// Report success
	respCode = http.StatusOK
	response = h.GetStdRESTSuccessMsg(r.Context())
}

// ======================================================================================
// Workspace Volume - Create Volume

// SetupWorkspaceVolume godoc
// @Summary Provision a workspace's persistent volume
// @Description Provision the workspace's persistent volume and record that it is ready.
// @Description Synchronous and blocking: the call returns only once the volume exists (or the
// @Description attempt failed). Idempotent - an existing volume is adopted rather than recreated.
// @tags management
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param workspaceID path string true "Workspace ID"
// @Success 200 {object} goutils.RestAPIBaseResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/workspaces/{workspaceID}/volume [post]
func (h WorkspaceAPIHandler) SetupWorkspaceVolume(w http.ResponseWriter, r *http.Request) {
	var respCode int
	var response interface{}
	var handlerError error
	var errorMsg string
	logTags := h.GetLogTagsForContext(r.Context())
	defer func() {
		if handlerError != nil {
			logAPIHandlerError(logTags, handlerError, errorMsg)
			response = h.GetStdRESTErrorMsg(
				r.Context(), respCode, errorMsg, handlerError.Error(),
			)
		}
		if err := h.WriteRESTResponse(w, respCode, response, nil); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				Error("Failed to form response")
		}
	}()

	workspaceID := mux.Vars(r)["workspaceID"]

	// The volume operations work from the workspace record - it carries the volume name and the
	// provisioning metadata - so an unknown workspace is answered before docker is touched.
	entry, _, err := h.manager.GetWorkspace(r.Context(), workspaceID, nil)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to fetch workspace %s", workspaceID)
		respCode = mapErrorToStatus(err)
		return
	}

	if err := h.manager.SetupWorkspaceVolume(r.Context(), entry, nil); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to set up workspace %s persistent volume", workspaceID)
		respCode = mapErrorToStatus(err)
		return
	}

	// Report success
	respCode = http.StatusOK
	response = h.GetStdRESTSuccessMsg(r.Context())
}

// ======================================================================================
// Workspace Volume - Delete Volume

// TeardownWorkspaceVolume godoc
// @Summary Delete a workspace's persistent volume
// @Description Delete the workspace's persistent volume and record that it is gone. Synchronous
// @Description and blocking. The docker daemon is the authoritative in-use gate and refuses while
// @Description anything still mounts the volume, which is reported as a conflict. Idempotent - a
// @Description volume that is already gone is not an error.
// @tags management
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param workspaceID path string true "Workspace ID"
// @Success 200 {object} goutils.RestAPIBaseResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 409 {object} goutils.RestAPIBaseResponse "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/workspaces/{workspaceID}/volume [delete]
func (h WorkspaceAPIHandler) TeardownWorkspaceVolume(w http.ResponseWriter, r *http.Request) {
	var respCode int
	var response interface{}
	var handlerError error
	var errorMsg string
	logTags := h.GetLogTagsForContext(r.Context())
	defer func() {
		if handlerError != nil {
			logAPIHandlerError(logTags, handlerError, errorMsg)
			response = h.GetStdRESTErrorMsg(
				r.Context(), respCode, errorMsg, handlerError.Error(),
			)
		}
		if err := h.WriteRESTResponse(w, respCode, response, nil); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				Error("Failed to form response")
		}
	}()

	workspaceID := mux.Vars(r)["workspaceID"]

	entry, _, err := h.manager.GetWorkspace(r.Context(), workspaceID, nil)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to fetch workspace %s", workspaceID)
		respCode = mapErrorToStatus(err)
		return
	}

	if err := h.manager.TeardownWorkspaceVolume(r.Context(), entry, nil); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to tear down workspace %s persistent volume", workspaceID)
		respCode = mapErrorToStatus(err)
		return
	}

	// Report success
	respCode = http.StatusOK
	response = h.GetStdRESTSuccessMsg(r.Context())
}
