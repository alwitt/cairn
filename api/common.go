// Package api - application REST API
package api //revive:disable-line:var-naming

import (
	"errors"
	"net/http"

	"github.com/alwitt/goutils"
	"github.com/apex/log"
	"github.com/gorilla/mux"
)

// methodHandlers DICT of method-endpoint handler
type methodHandlers map[string]http.HandlerFunc

// registerPathPrefix registers new method handler for a path prefix
func registerPathPrefix(parent *mux.Router, prefix string, handler methodHandlers) *mux.Router {
	router := parent.PathPrefix(prefix).Subrouter()
	for method, handler := range handler {
		router.Methods(method).Path("").HandlerFunc(handler)
	}
	return router
}

/*
withMiddleware wrap each handler in a method map with its own API handler's request logging and
payload dump middleware.

Applied per route rather than per router because one router tree is shared by several API
handlers, and the middleware logs each request under the log tags of the handler it was built
from - so a router level `Use` would attribute every request beneath it to whichever handler
happened to install it. Nesting a second `Use` on a sub-router does not fix that: gorilla/mux
applies a parent's middleware to a sub-router's matches as well, so both would run and every
request would be logged twice.

	@param handler goutils.RestAPIHandler - the API handler these endpoints belong to
	@param handlers methodHandlers - the method to endpoint handler map to wrap
	@returns the wrapped method to endpoint handler map
*/
func withMiddleware(handler goutils.RestAPIHandler, handlers methodHandlers) methodHandlers {
	wrapped := make(methodHandlers, len(handlers))
	for method, endpoint := range handlers {
		wrapped[method] = handler.LoggingMiddleware(
			handler.RequestPayloadDumpMiddleware(endpoint),
		)
	}
	return wrapped
}

/*
mapErrorToStatus map an error onto the HTTP status that describes it.

A manager or operator stacks its own error over the `goutils` error the layer beneath raised, so
the kind is read with `errors.As` rather than from the outermost type. Anything unrecognized - a
`SQLError` from a broken database, a `DockerError` from an unreachable daemon, an
`ObjectStoreError` - is the caller's problem to retry, not to correct, so it answers 500.

	@param err error - the failure to classify
	@returns the HTTP status code to answer with
*/
func mapErrorToStatus(err error) int {
	var validation goutils.ValidationError
	var badInput goutils.BadInputError
	var notFound goutils.NotFoundError
	var consistency goutils.ConsistencyError

	switch {
	case errors.As(err, &validation):
		return http.StatusBadRequest
	// What the caller supplied is well formed but wrong: a path outside the workspace volume
	// mount, a staging object key issued for another workspace, an object over the single-PUT
	// size cap, an artifact name already taken.
	case errors.As(err, &badInput):
		return http.StatusBadRequest
	case errors.As(err, &notFound):
		return http.StatusNotFound
	// A precondition the caller can act on: the workspace still has a volume (see DESIGN §4.3),
	// the volume is still mounted, the volume metadata can no longer be edited (§4.2), or the
	// artifact has no servable object to mint a GET URL for (§7.1).
	case errors.As(err, &consistency):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// logAPIHandlerError helper function to log errors encountered by the API handler. This is
// meant to be called right before the API handler function exits
//
// The `logTags` should capture the file and line number where the error is first encountered
// using `goutils.UpdateCodePositionInTags(logTags)`.
func logAPIHandlerError(logTags log.Fields, err error, msg string) {
	deepestErrorsWithStack := goutils.AllDeepestErrorsWithTrace(err)
	logEntry := log.WithError(err).WithFields(logTags)
	if deepestErrorsWithStack != nil {
		logEntry.Errorf(
			"[REST API Failure] %s:\n%s", msg, goutils.PrintErrorsWithTrace(deepestErrorsWithStack),
		)
	} else {
		logEntry.Errorf("[REST API Failure] %s", msg)
	}
}
