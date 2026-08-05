// Package api - application REST API
package api //revive:disable-line:var-naming

import (
	"context"
	"net/http"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
)

// LivenessHandler liveness API handlers
type LivenessHandler struct {
	goutils.RestAPIHandler
	// persistence layer client
	persistence db.Client
}

/*
NewLivenessHandler define a new liveness REST API handler

	@param persistence db.Client - DB persistence layer
	@param logConfig common.HTTPRequestLogging - handler log settings
	@param metrics goutils.HTTPRequestMetricHelper - metric collection agent
	@returns new handler
*/
func NewLivenessHandler(
	persistence db.Client,
	logConfig models.HTTPRequestLogging,
	metrics goutils.HTTPRequestMetricHelper,
) LivenessHandler {
	return LivenessHandler{
		RestAPIHandler: goutils.RestAPIHandler{
			Component: goutils.Component{
				LogTags: log.Fields{"module": "api", "component": "liveness-handler"},
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
			LogLevel:      logConfig.HealthLogLevel,
			MetricsHelper: metrics,
		},
		persistence: persistence,
	}
}

// Alive godoc
// @Summary API liveness check
// @Description Will return success to indicate REST API module is live
// @tags util
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Success 200 {object} goutils.RestAPIBaseResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /liveness/alive [get]
func (h LivenessHandler) Alive(w http.ResponseWriter, r *http.Request) {
	logTags := h.GetLogTagsForContext(r.Context())
	if err := h.WriteRESTResponse(
		w, http.StatusOK, h.GetStdRESTSuccessMsg(r.Context()), nil,
	); err != nil {
		log.
			WithError(err).
			WithFields(goutils.UpdateCodePositionInTags(logTags)).
			Error("Failed to form response")
	}
}

// Ready godoc
// @Summary API readiness check
// @Description Will return success to indicate REST API module is ready
// @tags util
// @Produce json
// @Param X-Request-ID header string false "Request ID"
// @Success 200 {object} goutils.RestAPIBaseResponse "success"
// @Failure 400 {object} goutils.RestAPIBaseResponse "error"
// @Failure 404 {string} string "error"
// @Failure 500 {object} goutils.RestAPIBaseResponse "error"
// @Router /liveness/ready [get]
func (h LivenessHandler) Ready(w http.ResponseWriter, r *http.Request) {
	var respCode int
	var response interface{}
	logTags := h.GetLogTagsForContext(r.Context())
	defer func() {
		if err := h.WriteRESTResponse(w, respCode, response, nil); err != nil {
			log.
				WithError(err).
				WithFields(goutils.UpdateCodePositionInTags(logTags)).
				Error("Failed to form response")
		}
	}()

	if err := h.persistence.UseDatabaseInTransaction(
		r.Context(), func(_ context.Context, dbClient db.Database) error {
			return dbClient.Ready()
		},
	); err != nil {
		respCode = http.StatusInternalServerError
		response = h.GetStdRESTErrorMsg(
			r.Context(), http.StatusInternalServerError, "not ready", err.Error(),
		)
	} else {
		respCode = http.StatusOK
		response = h.GetStdRESTSuccessMsg(r.Context())
	}
}
