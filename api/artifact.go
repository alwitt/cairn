// Package api - application REST API
package api //revive:disable-line:var-naming

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/alwitt/cairn/artifact"
	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/cairn/workspace"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
	"github.com/go-playground/validator/v10"
	"github.com/gorilla/mux"
)

// ArtifactAPIHandler artifact management API handler
type ArtifactAPIHandler struct {
	goutils.RestAPIHandler

	validator *validator.Validate

	// workspaceMgr workspace manager
	workspaceMgr workspace.Manager

	// artifactMgr artifact manager
	artifactMgr artifact.Manager

	// artifactOperator runs more complex
	artifactOperator artifact.Operator

	// storeConfig artifact storage config. Read for the ceiling on how long a served
	// artifact's presigned GET URL stays valid.
	storeConfig models.ArtifactStorageConfig
}

/*
NewArtifactAPIHandler new artifact management API handler

	@param appName string - the per-deployment application name which namespaces this
	    deployment's persistent volumes. A workspace's volume name is derived from it (see
	    DESIGN §2.1).
	@param workspaceMgr workspace.Manager - workspace manager
	@param artifactMgr artifact.Manager - artifact manager
	@param artifactOperator artifact.Operator - artifact operator
	@param storeConfig models.ArtifactStorageConfig - artifact storage config
	@param logConfig common.HTTPRequestLogging - handler log settings
	@param metrics goutils.HTTPRequestMetricHelper - metric collection agent
	@returns new handler
*/
func NewArtifactAPIHandler(
	appName string,
	workspaceMgr workspace.Manager,
	artifactMgr artifact.Manager,
	artifactOperator artifact.Operator,
	storeConfig models.ArtifactStorageConfig,
	logConfig models.HTTPRequestLogging,
	metrics goutils.HTTPRequestMetricHelper,
) (ArtifactAPIHandler, error) {
	validate := validator.New()
	if err := models.RegisterWithValidator(validate); err != nil {
		return ArtifactAPIHandler{}, goutils.NewRuntimeError(
			"failed to install custom validation macros", err, true,
		)
	}

	// The application name lands in every workspace's volume name, so hold it to the same
	// charset the volume name must satisfy.
	if err := validate.Var(appName, "required,valid_name"); err != nil {
		return ArtifactAPIHandler{}, goutils.NewValidationError(
			fmt.Sprintf("application name '%s' is not valid", appName), err, true,
		)
	}

	handler := ArtifactAPIHandler{
		RestAPIHandler: goutils.RestAPIHandler{
			Component: goutils.Component{
				LogTags: log.Fields{
					"package":   "cairn",
					"module":    "api",
					"component": "artifact-api-handler",
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
		validator:        validate,
		workspaceMgr:     workspaceMgr,
		artifactMgr:      artifactMgr,
		artifactOperator: artifactOperator,
		storeConfig:      storeConfig,
	}

	return handler, nil
}

// ======================================================================================
// Artifact Staging - Generate Staging Upload PUT URL

// NewStagingUploadRequest parameters to mint a staging upload PUT URL
type NewStagingUploadRequest struct {
	// Size the exact byte size of the object about to be staged. Bound into the signed URL as
	// `Content-Length`, so the object store rejects an upload of any other length.
	//
	// Deliberately not `required`: a zero byte artifact is legitimate, and `required` on an
	// integer rejects `0`. An omitted field therefore reads as an empty object rather than as
	// a missing one - which still fails closed, since a non-empty file's checksum cannot match
	// the one signed alongside a zero length (see DESIGN §6.1).
	Size int64 `json:"size" validate:"gte=0"`

	// SHA256B64 base64 SHA-256 of that object. Bound into the signed URL as
	// `x-amz-checksum-sha256`, which is what makes the object store verify the uploaded bytes.
	// This is base64, NOT the hex `sha256sum` prints (see DESIGN §6.4).
	SHA256B64 string `json:"sha256_b64" validate:"required,base64"`

	// ContentType optionally, the `Content-Type` to sign into the URL. Advisory only - the
	// authoritative MIME type is sniffed server side at registration (see DESIGN §6.1).
	ContentType *string `json:"content_type,omitempty" validate:"omitempty"`
}

// StagingUploadResponse response containing a staging upload bundle
type StagingUploadResponse struct {
	goutils.RestAPIBaseResponse
	// Staging where to upload the artifact, and the key to register it from afterwards
	Staging artifact.StagingUploadBundle `json:"staging" validate:"required"`
}

// NewStagingUpload godoc
// @Summary Mint a staging upload PUT URL
// @Description Mint a presigned PUT URL for a server-generated, workspace-scoped staging key.
// @Description The URL binds the supplied size and base64 SHA-256, so the object store verifies
// @Description the uploaded bytes. Purely an object store operation - no artifact entry is
// @Description created, so an abandoned upload leaves only a staging object for the maintenance
// @Description sweep to reclaim. Register the artifact afterwards with the returned key.
// @tags management
// @Accept json
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param workspaceID path string true "Workspace ID"
// @Param param body NewStagingUploadRequest true "Staged object size and checksum"
// @Success 200 {object} StagingUploadResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/workspaces/{workspaceID}/new-staging [post]
func (h ArtifactAPIHandler) NewStagingUpload(w http.ResponseWriter, r *http.Request) {
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
		msg := "No payload provided to mint a staging upload URL"
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = goutils.NewValidationError(msg, nil, true)
		errorMsg, respCode = msg, http.StatusBadRequest
		return
	}

	// Parse the staging parameters
	var params NewStagingUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "Unable to parse staging upload parameters from request"
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
			WithField("staging-upload", string(t)).
			Debug("Minting staging upload URL")
	}

	if err := h.validator.Struct(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "Staging upload parameters not valid"
		respCode = http.StatusBadRequest
		return
	}

	// The staging key is namespaced by workspace, so the parent must be resolved before one can
	// be issued. The resolve doubles as the "parent workspace must exist" gate (see DESIGN
	// §7.5) - the mount count it also returns is of no use here.
	parent, _, err := h.workspaceMgr.GetWorkspace(r.Context(), workspaceID, nil)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to fetch workspace %s", workspaceID)
		respCode = mapErrorToStatus(err)
		return
	}

	// The declared size is checked against the single-PUT cap by the manager before anything is
	// minted, so no cap check is repeated here (see DESIGN §5.2, §6.1).
	bundle, err := h.artifactMgr.GetArtifactStagingPutURL(
		r.Context(), parent, params.Size, params.SHA256B64, params.ContentType,
	)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf(
			"Failed to mint a staging upload URL for workspace %s", workspaceID,
		)
		respCode = mapErrorToStatus(err)
		return
	}

	// Return the staging bundle
	respCode = http.StatusOK
	response = StagingUploadResponse{
		RestAPIBaseResponse: h.GetStdRESTSuccessMsg(r.Context()), Staging: bundle,
	}
}

// ======================================================================================
// Artifact CRUD - Register Artifact From Staging

// RegisterArtifactRequest parameters to register a new artifact from a staged object
type RegisterArtifactRequest struct {
	// StagingObjectKey the staging object key returned when the upload URL was minted. The
	// bytes are expected to be at this location already.
	StagingObjectKey string `json:"staging_object_key" validate:"required"`
	// Name of the artifact, unique within the workspace, can only contain alphanumeric
	// characters, `-`, and `_`
	Name string `json:"name" validate:"required,valid_name"`
	// Description an optional description for the artifact
	Description *string `json:"description,omitempty" validate:"omitempty"`
}

// ArtifactEntryResponse response containing one artifact
type ArtifactEntryResponse struct {
	goutils.RestAPIBaseResponse
	// Artifact the artifact
	Artifact models.Artifact `json:"artifact" validate:"required"`
}

// RegisterArtifact godoc
// @Summary Register a new artifact from a staged object
// @Description Register a new artifact in a workspace from a previously staged object. The
// @Description staging key is verified as belonging to this workspace, the object is size
// @Description capped, its MIME type is sniffed server side, and it is copied to a final key
// @Description before the entry is created.
// @tags management
// @Accept json
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param workspaceID path string true "Workspace ID"
// @Param param body RegisterArtifactRequest true "Artifact parameters"
// @Success 200 {object} ArtifactEntryResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/workspaces/{workspaceID}/artifacts [post]
func (h ArtifactAPIHandler) RegisterArtifact(w http.ResponseWriter, r *http.Request) {
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
		msg := "No payload provided to register a new artifact"
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = goutils.NewValidationError(msg, nil, true)
		errorMsg, respCode = msg, http.StatusBadRequest
		return
	}

	// Parse the registration parameters
	var params RegisterArtifactRequest
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "Unable to parse new artifact parameters from request"
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
			WithField("new-artifact", string(t)).
			Debug("Registering new artifact")
	}

	if err := h.validator.Struct(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "New artifact parameters not valid"
		respCode = http.StatusBadRequest
		return
	}

	parent, _, err := h.workspaceMgr.GetWorkspace(r.Context(), workspaceID, nil)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to fetch workspace %s", workspaceID)
		respCode = mapErrorToStatus(err)
		return
	}

	newArtifact, err := h.artifactMgr.RegisterNewArtifact(
		r.Context(), parent, params.StagingObjectKey, params.Name, params.Description, nil,
	)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf(
			"Failed to register artifact '%s' in workspace %s", params.Name, workspaceID,
		)
		respCode = mapErrorToStatus(err)
		return
	}

	// Return the new artifact
	respCode = http.StatusOK
	response = ArtifactEntryResponse{
		RestAPIBaseResponse: h.GetStdRESTSuccessMsg(r.Context()), Artifact: newArtifact,
	}
}

// ======================================================================================
// Artifact CRUD - List Workspace Artifacts

// ArtifactListResponse response containing a list of artifacts
type ArtifactListResponse struct {
	goutils.RestAPIBaseResponse
	// Artifacts the list of artifacts
	Artifacts []models.Artifact `json:"artifacts,omitempty" validate:"omitempty,dive"`
}

// ListArtifacts godoc
// @Summary List a workspace's artifacts
// @Description List the artifacts in a workspace. Metadata only. The state filter is a listing
// @Description option, not a hardcoded filter: it defaults to RECORDED, and a caller triaging
// @Description quarantined entries asks for MISSING_OBJECT explicitly.
// @tags management
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param workspaceID path string true "Workspace ID"
// @Param name query []string false "Filter by artifact name" collectionFormat(multi)
// @Param state query []string false "Filter by artifact state, defaults to RECORDED" collectionFormat(multi) Enums(RECORDED, MISSING_OBJECT)
// @Param offset query int false "Number of leading entries to skip"
// @Param limit query int false "Max number of entries to return"
// @Success 200 {object} ArtifactListResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/workspaces/{workspaceID}/artifacts [get]
func (h ArtifactAPIHandler) ListArtifacts(w http.ResponseWriter, r *http.Request) {
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

	query := r.URL.Query()
	var filters db.ArtifactQueryFilter

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

	// Parse the name filter list. Artifact names are unique within a workspace, so this selects
	// an exact set rather than performing a similarity search.
	filters.TargetNames = append(filters.TargetNames, query["name"]...)

	// Parse the state filter list. The values are validated by the persistence layer, which
	// reports an unknown state as a validation failure.
	for _, entry := range query["state"] {
		filters.ArtifactStates = append(filters.ArtifactStates, models.ArtifactStateENUM(entry))
	}

	// Default the listing to the live state. An empty selection means "every state" at the
	// persistence layer, which would surface quarantined entries to a caller that did not ask
	// for them, so the default is applied here rather than left to the layer below (see DESIGN
	// §7.1). There are only two states, so naming both is how a caller asks for everything.
	if len(filters.ArtifactStates) == 0 {
		filters.ArtifactStates = []models.ArtifactStateENUM{models.ArtifactStateRecorded}
	}

	{
		t, _ := json.Marshal(&filters)
		log.
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			WithField("filters", string(t)).
			Debug("Listing workspace artifacts")
	}

	parent, _, err := h.workspaceMgr.GetWorkspace(r.Context(), workspaceID, nil)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to fetch workspace %s", workspaceID)
		respCode = mapErrorToStatus(err)
		return
	}

	entries, err := h.artifactMgr.ListWorkspaceArtifacts(r.Context(), parent, filters, nil)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to list workspace %s artifacts", workspaceID)
		respCode = mapErrorToStatus(err)
		return
	}

	// Return the artifacts
	respCode = http.StatusOK
	response = ArtifactListResponse{
		RestAPIBaseResponse: h.GetStdRESTSuccessMsg(r.Context()), Artifacts: entries,
	}
}

// ======================================================================================
// Artifact CRUD - Get One Artifact

// ArtifactDetailResponse response containing one artifact, optionally with a download URL
type ArtifactDetailResponse struct {
	goutils.RestAPIBaseResponse
	// Artifact the artifact
	Artifact models.Artifact `json:"artifact" validate:"required"`
	// GetURL a presigned GET URL for the artifact's content, present only when one was
	// requested. Minted forcing `Content-Disposition: attachment`, so a browser opening it
	// downloads the artifact rather than rendering it (see DESIGN §6.5).
	GetURL *string `json:"get_url,omitempty"`
}

// GetArtifact godoc
// @Summary Get one artifact
// @Description Fetch one artifact by its ID. With `?presign`, a short lived GET URL for its
// @Description content is minted alongside the metadata. Only a RECORDED artifact has a
// @Description servable object, so requesting a URL for one in any other state is a conflict.
// @tags management
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param artifactID path string true "Artifact ID"
// @Param presign query bool false "Also mint a presigned GET URL for the artifact content"
// @Param ttl query int false "Requested GET URL lifetime in seconds, capped by the deployment's configured maximum"
// @Success 200 {object} ArtifactDetailResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 409 {object} goutils.RestAPIBaseResponse "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/artifacts/{artifactID} [get]
func (h ArtifactAPIHandler) GetArtifact(w http.ResponseWriter, r *http.Request) {
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

	artifactID := mux.Vars(r)["artifactID"]
	query := r.URL.Query()

	// Parse the presign request. A bare `?presign` reads as true; an explicit value is parsed,
	// so `?presign=false` behaves as though the flag were absent.
	withGetURL := false
	if values, present := query["presign"]; present {
		withGetURL = true
		if len(values) > 0 && values[0] != "" {
			parsed, err := strconv.ParseBool(values[0])
			if err != nil {
				goutils.UpdateCodePositionInTags(logTags)
				handlerError = err
				errorMsg = "Query parameter 'presign' must be a boolean"
				respCode = http.StatusBadRequest
				return
			}
			withGetURL = parsed
		}
	}

	// Parse the requested GET URL lifetime. The configured maximum is both the ceiling and the
	// default: a caller may ask for a shorter lived link than the deployment allows - a
	// reasonable thing to want for one that is about to be shared - but never a longer one.
	getURLTTL := h.storeConfig.DownloadGetURLMaxTTL()
	if raw := query.Get("ttl"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			goutils.UpdateCodePositionInTags(logTags)
			handlerError = err
			errorMsg = "Query parameter 'ttl' must be an integer number of seconds"
			respCode = http.StatusBadRequest
			return
		}
		if parsed <= 0 {
			msg := "Query parameter 'ttl' must be a positive number of seconds"
			goutils.UpdateCodePositionInTags(logTags)
			handlerError = goutils.NewValidationError(msg, nil, true)
			errorMsg, respCode = msg, http.StatusBadRequest
			return
		}
		if requested := time.Second * time.Duration(parsed); requested < getURLTTL {
			getURLTTL = requested
		}
	}

	entry, err := h.artifactMgr.GetArtifact(r.Context(), artifactID, nil)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to fetch artifact %s", artifactID)
		respCode = mapErrorToStatus(err)
		return
	}

	var getURL *string
	if withGetURL {
		// Refused for an artifact whose backing object is gone - it has nothing servable, so a
		// URL minted for it would only resolve to a not-found (see DESIGN §7.1). Reported as a
		// failure rather than answered with the URL quietly missing, which would read as a
		// serialization gap instead of a refusal.
		minted, err := h.artifactMgr.GenerateGetURLForArtifact(r.Context(), entry, getURLTTL)
		if err != nil {
			goutils.UpdateCodePositionInTags(logTags)
			handlerError = err
			errorMsg = fmt.Sprintf("Failed to mint a GET URL for artifact %s", artifactID)
			respCode = mapErrorToStatus(err)
			return
		}
		getURL = &minted
	}

	// Return the artifact
	respCode = http.StatusOK
	response = ArtifactDetailResponse{
		RestAPIBaseResponse: h.GetStdRESTSuccessMsg(r.Context()),
		Artifact:            entry,
		GetURL:              getURL,
	}
}

// ======================================================================================
// Artifact CRUD - Delete Artifact

// DeleteArtifact godoc
// @Summary Delete an artifact
// @Description Delete an artifact entry. No object store interaction - the freed object is
// @Description reclaimed later by the maintenance loop. Idempotent: deleting an absent entry is
// @Description a no-op.
// @tags management
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param artifactID path string true "Artifact ID"
// @Success 200 {object} goutils.RestAPIBaseResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/artifacts/{artifactID} [delete]
func (h ArtifactAPIHandler) DeleteArtifact(w http.ResponseWriter, r *http.Request) {
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

	artifactID := mux.Vars(r)["artifactID"]

	if err := h.artifactMgr.DeleteArtifact(r.Context(), artifactID, nil); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to delete artifact %s", artifactID)
		respCode = mapErrorToStatus(err)
		return
	}

	// Report success
	respCode = http.StatusOK
	response = h.GetStdRESTSuccessMsg(r.Context())
}

// ======================================================================================
// Artifact CRUD - Update Artifact Name

// UpdateArtifactName godoc
// @Summary Change an artifact's name
// @Description Change the name of an artifact. A pure DB update: the backing object key carries
// @Description a random suffix rather than the name, so a rename never touches the object store.
// @Description Names are unique within the parent workspace.
// @tags management
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param artifactID path string true "Artifact ID"
// @Param name query string true "New artifact name, can only contain alphanumeric characters, - and _"
// @Success 200 {object} ArtifactEntryResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/artifacts/{artifactID}/name [put]
func (h ArtifactAPIHandler) UpdateArtifactName(w http.ResponseWriter, r *http.Request) {
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

	artifactID := mux.Vars(r)["artifactID"]

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
	updated, err := h.artifactMgr.RenameArtifact(r.Context(), artifactID, newName, nil)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf(
			"Failed to update artifact %s name to '%s'", artifactID, newName,
		)
		respCode = mapErrorToStatus(err)
		return
	}

	// Return the updated artifact
	respCode = http.StatusOK
	response = ArtifactEntryResponse{
		RestAPIBaseResponse: h.GetStdRESTSuccessMsg(r.Context()), Artifact: updated,
	}
}

// ======================================================================================
// Artifact CRUD - Update Artifact Description

// UpdateArtifactDescriptionRequest parameters to change an artifact description
type UpdateArtifactDescriptionRequest struct {
	// Description new artifact description, set to null to clear
	Description *string `json:"description" validate:"omitempty"`
}

// UpdateArtifactDescription godoc
// @Summary Change an artifact's description
// @Description Change the description of an artifact
// @tags management
// @Accept json
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param artifactID path string true "Artifact ID"
// @Param param body UpdateArtifactDescriptionRequest true "New artifact description"
// @Success 200 {object} ArtifactEntryResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/artifacts/{artifactID}/description [put]
func (h ArtifactAPIHandler) UpdateArtifactDescription(w http.ResponseWriter, r *http.Request) {
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

	artifactID := mux.Vars(r)["artifactID"]

	if r.Body == nil {
		msg := "No payload provided to update artifact description"
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = goutils.NewValidationError(msg, nil, true)
		errorMsg, respCode = msg, http.StatusBadRequest
		return
	}

	// Parse the new description. It is carried in a body rather than a query parameter so an
	// explicit null can clear it, which an absent parameter could not express.
	var params UpdateArtifactDescriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "Unable to parse new artifact description from request"
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
			Debug("Updating artifact description")
	}

	// Apply the new description
	updated, err := h.artifactMgr.UpdateArtifactDescription(
		r.Context(), artifactID, params.Description, nil,
	)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to update artifact %s description", artifactID)
		respCode = mapErrorToStatus(err)
		return
	}

	// Return the updated artifact
	respCode = http.StatusOK
	response = ArtifactEntryResponse{
		RestAPIBaseResponse: h.GetStdRESTSuccessMsg(r.Context()), Artifact: updated,
	}
}

// ======================================================================================
// Artifact Content - Update Content From Staging

// UpdateArtifactContentRequest parameters to replace an artifact's content from a staged object
type UpdateArtifactContentRequest struct {
	// StagingObjectKey the staging object key returned when the upload URL was minted. The new
	// bytes are expected to be at this location already.
	StagingObjectKey string `json:"staging_object_key" validate:"required"`
}

// UpdateArtifactContent godoc
// @Summary Replace an artifact's content from a staged object
// @Description Replace an existing artifact's content with a previously staged object. The same
// @Description pre-copy checks as registration apply. The bytes are copied to a NEW final key
// @Description and the entry is flipped to it in one update, so a reader never sees a half
// @Description written object; the old object is reclaimed later by the maintenance loop.
// @Description Concurrent updates are last-writer-wins.
// @tags management
// @Accept json
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param artifactID path string true "Artifact ID"
// @Param param body UpdateArtifactContentRequest true "Staged object to take the content from"
// @Success 200 {object} ArtifactEntryResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/artifacts/{artifactID}/content [put]
func (h ArtifactAPIHandler) UpdateArtifactContent(w http.ResponseWriter, r *http.Request) {
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

	artifactID := mux.Vars(r)["artifactID"]

	if r.Body == nil {
		msg := "No payload provided to update artifact content"
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = goutils.NewValidationError(msg, nil, true)
		errorMsg, respCode = msg, http.StatusBadRequest
		return
	}

	// Parse the update parameters
	var params UpdateArtifactContentRequest
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "Unable to parse new artifact content parameters from request"
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
			WithField("new-content", string(t)).
			Debug("Updating artifact content")
	}

	if err := h.validator.Struct(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "New artifact content parameters not valid"
		respCode = http.StatusBadRequest
		return
	}

	// The manager works from the resolved entry - it carries the parent workspace the staging
	// key is verified against, and the object key that gets repointed.
	entry, err := h.artifactMgr.GetArtifact(r.Context(), artifactID, nil)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to fetch artifact %s", artifactID)
		respCode = mapErrorToStatus(err)
		return
	}

	updated, err := h.artifactMgr.UpdateArtifactContent(
		r.Context(), entry, params.StagingObjectKey, nil,
	)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to update artifact %s content", artifactID)
		respCode = mapErrorToStatus(err)
		return
	}

	// Return the updated artifact
	respCode = http.StatusOK
	response = ArtifactEntryResponse{
		RestAPIBaseResponse: h.GetStdRESTSuccessMsg(r.Context()), Artifact: updated,
	}
}

// ======================================================================================
// Artifact Volume Transfer - Save Artifact From Volume

// ArtifactFromVolumeRequest parameters to create a new artifact from a file in a workspace's
// persistent volume
type ArtifactFromVolumeRequest struct {
	// SourcePath the file to upload, within the workspace volume. Must be absolute and within
	// the volume mount; a symlink is resolved, and is rejected if its target escapes the mount.
	SourcePath string `json:"source_path" validate:"required"`
	// Name of the artifact, unique within the workspace, can only contain alphanumeric
	// characters, `-`, and `_`
	Name string `json:"name" validate:"required,valid_name"`
	// Description an optional description for the artifact
	Description *string `json:"description,omitempty" validate:"omitempty"`
}

// SaveArtifactFromVolume godoc
// @Summary Save a new artifact from a file in the workspace volume
// @Description Create a new artifact from a file resident in the workspace's persistent volume.
// @Description Runs a stat/hash sidecar to bind the file's exact size and checksum into a
// @Description staging PUT URL, then an upload sidecar to send it, then the same staging
// @Description registration path. Synchronous and blocking. Requires a provisioned volume, and
// @Description fails if the artifact name is already taken.
// @tags management
// @Accept json
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param workspaceID path string true "Workspace ID"
// @Param param body ArtifactFromVolumeRequest true "Source file and artifact parameters"
// @Success 200 {object} ArtifactEntryResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/workspaces/{workspaceID}/artifact-from-volume [post]
func (h ArtifactAPIHandler) SaveArtifactFromVolume(w http.ResponseWriter, r *http.Request) {
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
		msg := "No payload provided to save an artifact from the workspace volume"
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = goutils.NewValidationError(msg, nil, true)
		errorMsg, respCode = msg, http.StatusBadRequest
		return
	}

	// Parse the upload parameters
	var params ArtifactFromVolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "Unable to parse volume artifact parameters from request"
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
			WithField("volume-artifact", string(t)).
			Debug("Saving artifact from workspace volume")
	}

	// The source path itself is checked by the operator, which holds the canonical mount and
	// applies the same containment rule the sidecar does (see DESIGN §7.5).
	if err := h.validator.Struct(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "Volume artifact parameters not valid"
		respCode = http.StatusBadRequest
		return
	}

	parent, _, err := h.workspaceMgr.GetWorkspace(r.Context(), workspaceID, nil)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to fetch workspace %s", workspaceID)
		respCode = mapErrorToStatus(err)
		return
	}

	newArtifact, err := h.artifactOperator.UploadArtifact(
		r.Context(), parent, params.SourcePath, params.Name, params.Description, nil,
	)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf(
			"Failed to save artifact '%s' from workspace %s volume", params.Name, workspaceID,
		)
		respCode = mapErrorToStatus(err)
		return
	}

	// Return the new artifact
	respCode = http.StatusOK
	response = ArtifactEntryResponse{
		RestAPIBaseResponse: h.GetStdRESTSuccessMsg(r.Context()), Artifact: newArtifact,
	}
}

// ======================================================================================
// Artifact Volume Transfer - Update Artifact From Volume

// UpdateArtifactFromVolumeRequest parameters to replace an artifact's content from a file in a
// workspace's persistent volume
type UpdateArtifactFromVolumeRequest struct {
	// SourcePath the file to upload, within the workspace volume. Must be absolute and within
	// the volume mount; a symlink is resolved, and is rejected if its target escapes the mount.
	SourcePath string `json:"source_path" validate:"required"`
}

// UpdateArtifactFromVolume godoc
// @Summary Replace an artifact's content from a file in the workspace volume
// @Description Replace an existing artifact's content from a file resident in its workspace's
// @Description persistent volume. The same two sidecar flow as saving a new one, over the
// @Description update core path. Synchronous and blocking. Requires a provisioned volume. An
// @Description artifact quarantined as MISSING_OBJECT is a legitimate target - re-uploading its
// @Description content is how one is repaired.
// @tags management
// @Accept json
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param artifactID path string true "Artifact ID"
// @Param param body UpdateArtifactFromVolumeRequest true "Source file within the workspace volume"
// @Success 200 {object} ArtifactEntryResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/artifacts/{artifactID}/update-from-volume [post]
func (h ArtifactAPIHandler) UpdateArtifactFromVolume(w http.ResponseWriter, r *http.Request) {
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

	artifactID := mux.Vars(r)["artifactID"]

	if r.Body == nil {
		msg := "No payload provided to update artifact content from the workspace volume"
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = goutils.NewValidationError(msg, nil, true)
		errorMsg, respCode = msg, http.StatusBadRequest
		return
	}

	// Parse the update parameters
	var params UpdateArtifactFromVolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "Unable to parse volume artifact update parameters from request"
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
			WithField("volume-artifact-update", string(t)).
			Debug("Updating artifact content from workspace volume")
	}

	if err := h.validator.Struct(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "Volume artifact update parameters not valid"
		respCode = http.StatusBadRequest
		return
	}

	// The operator needs both halves: the artifact supplies the entry that gets rewritten, its
	// parent workspace supplies the volume that gets mounted and read.
	entry, err := h.artifactMgr.GetArtifact(r.Context(), artifactID, nil)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to fetch artifact %s", artifactID)
		respCode = mapErrorToStatus(err)
		return
	}

	parent, _, err := h.workspaceMgr.GetWorkspace(r.Context(), entry.WorkspaceID, nil)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to fetch workspace %s", entry.WorkspaceID)
		respCode = mapErrorToStatus(err)
		return
	}

	updated, err := h.artifactOperator.UpdateArtifact(
		r.Context(), parent, entry, params.SourcePath, nil,
	)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf(
			"Failed to update artifact %s content from workspace %s volume",
			artifactID, entry.WorkspaceID,
		)
		respCode = mapErrorToStatus(err)
		return
	}

	// Return the updated artifact
	respCode = http.StatusOK
	response = ArtifactEntryResponse{
		RestAPIBaseResponse: h.GetStdRESTSuccessMsg(r.Context()), Artifact: updated,
	}
}

// ======================================================================================
// Artifact Volume Transfer - Load Artifact Into Volume

// LoadArtifactInVolumeRequest parameters to download an artifact into a workspace's persistent
// volume
type LoadArtifactInVolumeRequest struct {
	// TargetPath where to write the artifact within the workspace volume. Must be absolute and
	// within the volume mount, and its parent directory must already exist - intermediate
	// directories are never created (see DESIGN §7.5.1).
	TargetPath string `json:"target_path" validate:"required"`
}

// LoadArtifactInVolume godoc
// @Summary Load an artifact into the workspace volume
// @Description Download an artifact's content into its workspace's persistent volume. A single
// @Description sidecar mounts the volume and pulls the content over a presigned GET URL.
// @Description Synchronous and blocking. Requires a provisioned volume. The destination's parent
// @Description directory must already exist; an existing file is overwritten, a symlink at the
// @Description target is replaced rather than followed, and a directory is refused.
// @tags management
// @Accept json
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Param artifactID path string true "Artifact ID"
// @Param param body LoadArtifactInVolumeRequest true "Destination within the workspace volume"
// @Success 200 {object} goutils.RestAPIBaseResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 403 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /v1/artifacts/{artifactID}/load-in-volume [post]
func (h ArtifactAPIHandler) LoadArtifactInVolume(w http.ResponseWriter, r *http.Request) {
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

	artifactID := mux.Vars(r)["artifactID"]

	if r.Body == nil {
		msg := "No payload provided to load an artifact into the workspace volume"
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = goutils.NewValidationError(msg, nil, true)
		errorMsg, respCode = msg, http.StatusBadRequest
		return
	}

	// Parse the download parameters
	var params LoadArtifactInVolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "Unable to parse artifact load parameters from request"
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
			WithField("artifact-load", string(t)).
			Debug("Loading artifact into workspace volume")
	}

	if err := h.validator.Struct(&params); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = "Artifact load parameters not valid"
		respCode = http.StatusBadRequest
		return
	}

	entry, err := h.artifactMgr.GetArtifact(r.Context(), artifactID, nil)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to fetch artifact %s", artifactID)
		respCode = mapErrorToStatus(err)
		return
	}

	parent, _, err := h.workspaceMgr.GetWorkspace(r.Context(), entry.WorkspaceID, nil)
	if err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf("Failed to fetch workspace %s", entry.WorkspaceID)
		respCode = mapErrorToStatus(err)
		return
	}

	if err := h.artifactOperator.DownloadArtifact(
		r.Context(), parent, entry, params.TargetPath,
	); err != nil {
		goutils.UpdateCodePositionInTags(logTags)
		handlerError = err
		errorMsg = fmt.Sprintf(
			"Failed to load artifact %s into workspace %s volume", artifactID, entry.WorkspaceID,
		)
		respCode = mapErrorToStatus(err)
		return
	}

	// Report success
	respCode = http.StatusOK
	response = h.GetStdRESTSuccessMsg(r.Context())
}
