// Package artifact - artifact management code
package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/alwitt/goutils/runtime"
	"github.com/apex/log"
	"github.com/go-playground/validator/v10"
)

/*
SystemCallDockerRuntimeFactory defines a docker container runtime to run a sidecar with.

Injected rather than calling `runtime.NewDockerSystemCallRuntime` directly, so a unit test can
drive the sidecar lifecycle without a docker daemon. Named for docker specifically because it
is `dockerOperatorImpl`'s seam; a runtime driver for another orchestrator would take its own.

	@param ctx context.Context - execution context
	@param name string - container name
	@param command runtime.ContainerCommand - the container entrypoint and arguments
	@param params runtime.DockerRuntimeParams - the container runtime parameters
	@param clearANSIFromOutput bool - whether to strip ANSI escapes from the captured output
	@returns the new container runtime
*/
type SystemCallDockerRuntimeFactory func(
	ctx context.Context,
	name string,
	command runtime.ContainerCommand,
	params runtime.DockerRuntimeParams,
	clearANSIFromOutput bool,
) (runtime.SystemCallRuntime, error)

/*
DefaultSystemCallDockerRuntimeFactory the production docker container runtime factory.

	@param ctx context.Context - execution context
	@param name string - container name
	@param command runtime.ContainerCommand - the container entrypoint and arguments
	@param params runtime.DockerRuntimeParams - the container runtime parameters
	@param clearANSIFromOutput bool - whether to strip ANSI escapes from the captured output
	@returns the new container runtime
*/
func DefaultSystemCallDockerRuntimeFactory(
	ctx context.Context,
	name string,
	command runtime.ContainerCommand,
	params runtime.DockerRuntimeParams,
	clearANSIFromOutput bool,
) (runtime.SystemCallRuntime, error) {
	return runtime.NewDockerSystemCallRuntime(ctx, name, command, params, clearANSIFromOutput)
}

// Operator artifact tasks operations runner
//
// This is the core logic for driving the MCP-tools (and equivalent REST APIs)
// - upload_artifact
// - update_artifact
// - download_artifact
type Operator interface {
	/*
		DownloadArtifact download an artifact's content into a workspace's persistent volume.

		The read direction of the artifact transfer: a single sidecar, and no registration
		step, since a GET binds no size or checksum up front the way the upload path's PUT
		does (see DESIGN §7.4).

		The sidecar mounts the workspace volume and pulls the artifact over a presigned GET
		URL. It never calls back into this service, and holds no object store credential
		beyond that URL - a short-lived bearer token scoped to one key and one operation (see
		DESIGN §5.1, §5.2).

		The destination's parent directory must already exist. Creating it is deliberately
		not attempted: this service does not control the UID the tool containers run as, so
		any directory the sidecar created would be owned by the sidecar's UID and unusable by
		them (see DESIGN §7.5.1).

		A partially written file may remain in the volume after a mid-transfer failure. The
		volume is disposable scratch and the caller is told the download failed, so cleanup
		would add a failure mode without adding a guarantee (see DESIGN §7.5.1).

		The parent workspace and the artifact are taken as already resolved by the caller
		(see DESIGN §3).

			@param ctx context.Context - execution context
			@param workspace models.Workspace - workspace this is for
			@param artifact models.Artifact - the artifact to download
			@param targetPath string - where to write the artifact within the workspace
			    volume. Must be absolute, and within the volume mount.
	*/
	DownloadArtifact(
		ctx context.Context,
		workspace models.Workspace,
		artifact models.Artifact,
		targetPath string,
	) error

	/*
		UploadArtifact record a new artifact from a file in a workspace's persistent volume.

		The write direction of the artifact transfer, and two sidecars rather than one: a
		staging PUT URL is bound to the file's exact size and base64 SHA-256 before it can be
		minted, and the bytes live in the volume where only a sidecar can reach them. So a
		stat/hash sidecar derives the pair, and an upload sidecar sends the file with exactly
		the headers that were signed (see DESIGN §6.4, §7.3).

		Neither sidecar calls back into this service, and only the upload one holds an object
		store credential - a presigned URL scoped to one key and one operation (see DESIGN
		§5.1, §5.2).

		The name is pre-checked as free before either sidecar runs, so a taken name costs no
		container runs. The database's uniqueness constraint remains the real guard for a
		caller that races another (see DESIGN §7.5).

		A file that changes on the volume between the two sidecars fails closed: the uploaded
		bytes no longer match the checksum the PUT was signed for, and the object store
		rejects it (see DESIGN §6.4).

		The parent workspace is taken as already resolved by the caller (see DESIGN §3).

			@param ctx context.Context - execution context
			@param workspace models.Workspace - workspace this is for
			@param sourcePath string - the file to upload, within the workspace volume. Must
			    be absolute, and within the volume mount.
			@param name string - name a user will reference the artifact by
			@param description *string - an optional description for the artifact
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it. Normally pass nil: a transaction handed in here is held open across
			    both container runs, and the only work inside it is the name pre-check and the
			    final insert.
			@returns the new artifact entry
	*/
	UploadArtifact(
		ctx context.Context,
		workspace models.Workspace,
		sourcePath string,
		name string,
		description *string,
		activeSession db.Database,
	) (models.Artifact, error)

	/*
		UpdateArtifact replace an existing artifact's content from a file in a workspace's
		persistent volume.

		The same two-sidecar flow as UploadArtifact over the same staging core, differing only
		at the ends: the target artifact is resolved by the caller rather than named, and the
		staged object updates a row instead of inserting one (see DESIGN §6.3, §7.3).

		An artifact quarantined as `MISSING_OBJECT` is a legitimate target - re-uploading its
		content is how one is repaired - so there is no artifact-state gate here, unlike
		DownloadArtifact (see DESIGN §6.3).

		Concurrent updates to one artifact are last-writer-wins; each writer stages to its own
		key and the final row update decides the winner (see DESIGN §7.5.2).

		The parent workspace and the artifact are taken as already resolved by the caller, and
		the artifact must belong to the workspace (see DESIGN §3).

			@param ctx context.Context - execution context
			@param workspace models.Workspace - workspace this is for
			@param artifact models.Artifact - the artifact whose content is replaced
			@param sourcePath string - the file to upload, within the workspace volume. Must
			    be absolute, and within the volume mount.
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it. Normally pass nil, for the reason given on UploadArtifact.
			@returns the updated artifact entry
	*/
	UpdateArtifact(
		ctx context.Context,
		workspace models.Workspace,
		artifact models.Artifact,
		sourcePath string,
		activeSession db.Database,
	) (models.Artifact, error)
}

// dockerOperatorImpl implements Operator using docker as the runtime driver
type dockerOperatorImpl struct {
	goutils.Component

	appName string

	validator *validator.Validate

	// sidecarConfig artifact operations sidecar config
	sidecarConfig models.ArtifactSidecarConfig

	// manager core artifact manager
	manager Manager

	// defineRuntime defines the container runtime a sidecar runs in
	defineRuntime SystemCallDockerRuntimeFactory
}

/*
NewDockerOperator define a new docker driven artifact operator

	@param appName string - the per-deployment application name
	@param manager Manager - the core artifact manager the operations are built on
	@param sidecarConfig models.ArtifactSidecarConfig - artifact operations sidecar config
	@param defineRuntime SystemCallDockerRuntimeFactory - defines the container runtime a
	    sidecar runs in. Pass `DefaultSystemCallDockerRuntimeFactory` outside of tests.
	@returns the new artifact operator
*/
func NewDockerOperator(
	appName string,
	manager Manager,
	sidecarConfig models.ArtifactSidecarConfig,
	defineRuntime SystemCallDockerRuntimeFactory,
) (Operator, error) {
	logTags := log.Fields{
		"package": "cairn", "module": "artifact", "component": "operator", "instance": appName,
	}

	validate := validator.New()
	if err := models.RegisterWithValidator(validate); err != nil {
		return nil, goutils.NewRuntimeError(
			"failed to install custom validation macros", err, true,
		)
	}

	if err := validate.Var(appName, "required,valid_name"); err != nil {
		return nil, goutils.NewValidationError(
			fmt.Sprintf("application name '%s' is not valid", appName), err, true,
		)
	}

	if manager == nil {
		return nil, goutils.NewValidationError("artifact manager is required", nil, true)
	}

	// Required rather than defaulted to `DefaultSystemCallDockerRuntimeFactory`, so the
	// choice of runtime driver stays explicit at the wiring site.
	if defineRuntime == nil {
		return nil, goutils.NewValidationError("container runtime factory is required", nil, true)
	}

	// Validate the sidecar config up front so a missing image or timeout fails here rather
	// than at the first artifact operation.
	if err := validate.Struct(&sidecarConfig); err != nil {
		return nil, goutils.NewValidationError(
			"artifact operations sidecar config is not valid", err, true,
		)
	}

	instance := &dockerOperatorImpl{
		Component: goutils.Component{
			LogTags: logTags,
			LogTagModifiers: []goutils.LogMetadataModifier{
				goutils.ModifyLogMetadataByRestRequestParam,
				goutils.ModifyLogMetadataByMCPRequestParam,
			},
		},
		appName:       appName,
		validator:     validate,
		sidecarConfig: sidecarConfig,
		manager:       manager,
		defineRuntime: defineRuntime,
	}

	return instance, nil
}

// validateWorkspacePath verify a caller-supplied path is absolute and lands inside the
// workspace volume mount.
//
// The sidecar performs its own containment check, and that one is authoritative: only it can
// see the filesystem, so only it can resolve a symlink and judge where a path really leads.
// This is the cheaper front gate - it rejects an obvious escape before a container is
// launched at all, and is the handler-level enforcement DESIGN §7.5 asks for. Two independent
// gates, both failing closed.
func validateWorkspacePath(path string) error {
	if !filepath.IsAbs(path) {
		return goutils.NewBadInputError(
			fmt.Sprintf(
				"path '%s' must be absolute, and within the workspace volume mounted at '%s'",
				path, models.WorkspaceMountPath,
			), nil, true,
		)
	}

	// `Clean` folds any `..` traversal, so the check below sees where the path actually
	// lands rather than what it was spelled as.
	cleaned := filepath.Clean(path)

	// The trailing separator is load-bearing: without it a sibling whose name merely starts
	// with the mount's - `/mnt/cairn/wsX` - would read as a match. Comparing against
	// `<mount>/` makes this a path-component test rather than a string-prefix one.
	//
	// The mount root itself fails here too, correctly: it is a directory, never a file to
	// read or write.
	if !strings.HasPrefix(cleaned, models.WorkspaceMountPath+string(filepath.Separator)) {
		return goutils.NewBadInputError(
			fmt.Sprintf(
				"path '%s' resolves to '%s', which is outside the workspace volume mounted at '%s'",
				path, cleaned, models.WorkspaceMountPath,
			), nil, true,
		)
	}

	return nil
}

// findSidecarResultLine scan a sidecar's combined output for its result line.
//
// The container runtime gives sidecars a TTY, so STDOUT and STDERR are merged into one stream
// before this service ever sees them. There is no stream to separate, which rules out a "the
// result is on STDOUT" contract - one warning line from a library and a whole-output parse
// fails. So the sidecar frames its result as a single line of compact JSON instead, and this
// scans for the line that parses.
//
// The LAST decodable object wins rather than the first: a library that happens to log JSON
// would otherwise pre-empt the real record, and the sidecar always emits its result last.
//
// Returns the matched line's raw bytes; the caller unmarshals it into whatever shape its own
// sidecar emits. Keeping the decode at the call site is what lets this one helper serve both a
// transfer sidecar's `{ok, error}` and the stat sidecar's much wider block.
func findSidecarResultLine(output string) ([]byte, bool) {
	var found []byte

	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Decoding into a map rather than `any` is what restricts a match to a JSON
		// *object*. A bare `3` or `"done"` is legal JSON on a line of its own, but it is
		// noise, not a result record.
		var candidate map[string]any
		if err := json.Unmarshal([]byte(trimmed), &candidate); err != nil {
			continue
		}

		found = []byte(trimmed)
	}

	return found, found != nil
}

// maxReportedOutputBytes how much of a sidecar's raw output is quoted back in an error.
//
// Enough to diagnose a broken image from the message alone, bounded so a chatty container
// cannot flood the caller's error with its entire log.
const maxReportedOutputBytes = 2048

// redactionPlaceholder what a redacted secret is replaced with in quoted sidecar output.
const redactionPlaceholder = "[REDACTED]"

// redactSecrets strip any presigned URL out of text about to be quoted back to a caller.
//
// A sidecar that dies mid-transfer may echo its own URL into a traceback, and quoting that
// verbatim would put a live, still-valid credential into whatever logs the resulting error -
// the signature travels in the URL's query string (see DESIGN §5.2).
//
// The URLs are taken from the environment the sidecar was launched with rather than matched by
// pattern, so this redacts exactly the secrets this service handed out and cannot be evaded by
// a URL shape the pattern did not anticipate.
func redactSecrets(output string, env []runtime.ContainerEnvVar) string {
	redacted := output
	for _, entry := range env {
		if entry.Name != sidecarEnvURL || entry.Value == "" {
			continue
		}
		redacted = strings.ReplaceAll(redacted, entry.Value, redactionPlaceholder)
	}
	return redacted
}

// truncateOutput bound a sidecar's raw output for inclusion in an error message.
func truncateOutput(output string) string {
	trimmed := strings.TrimSpace(output)
	if len(trimmed) <= maxReportedOutputBytes {
		return trimmed
	}
	return trimmed[:maxReportedOutputBytes] + "... (truncated)"
}
