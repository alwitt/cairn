// Package artifact - artifact management code
package artifact

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
	"github.com/gabriel-vasile/mimetype"
	"github.com/go-playground/validator/v10"
	"github.com/oklog/ulid/v2"
)

// StagingUploadBundle response bundle containing information needed to upload files for staging
type StagingUploadBundle struct {
	// StagingObjectKey staging artifact object key. Artifact staged at this location
	// will later be read and copied to the final storage location.
	StagingObjectKey string `json:"staging_object_key"`
	// PutURL the PUR URL for uploading the artifact
	PutURL string `json:"put_url"`
}

// Manager artifact manager
type Manager interface {
	/*
		GetArtifactStagingPutURL generate a artifact staging PUT URL for a particular workspace.

		This function defines a new staging object key within the namespace of this workspace and
		produces a upload PUT URL for it with a pre-configured TTL.

		Once a user uploads the artifact using the PUT URL, they can then register the artifact in
		the requested workspace by pointing the system at the staging object key returned by this
		function.

		Purely an object-store operation - no DB entry is created here. Deferring the entry to
		registration means an abandoned upload leaves only a staging object, which the maintenance
		sweep reclaims, rather than a dangling row (see DESIGN §6.1).

		The URL binds the upload to the supplied size and checksum, so the HTTP client using it MUST
		send these headers with exactly these values or the object store rejects the request:

		  - `Content-Length` - `artifactSize`
		  - `x-amz-checksum-sha256` - `b64ArtifactSHA256`
		  - `Content-Type` - `contentType`, only when one was supplied here

		Binding the checksum is what makes the object store verify the uploaded bytes, so a size or
		hash mismatch fails the upload rather than storing corrupt content.

			@param ctx context.Context - execution context
			@param workspace models.Workspace - workspace this is for
			@param artifactSize int64 - artifact file size
			@param b64ArtifactSHA256 string - artifact file SHA256SUM in base64 encoding
			@param contentType *string - optionally, specify the Content-Type / MIMEType of the artifact
			@returns parameters for staging the artifact
	*/
	GetArtifactStagingPutURL(
		ctx context.Context,
		workspace models.Workspace,
		artifactSize int64,
		b64ArtifactSHA256 string,
		contentType *string,
	) (StagingUploadBundle, error)

	/*
		RegisterNewArtifact register a new artifact within a workspace given a artifact staging
		object key.

		In preparation, the function verified the staging object key is a valid for objects staged
		for this particular workspace; this is based on object key prefix matching.

		Once verified, it then extract basic information regarding object from the staged object
		  - object size
		  - object MIME type - computed using a portion of the object data with a magic number library

		With that information, a final storage object key for this workspace is defined, and the
		staging object is copied into storage object key with the computed MIME type.

		Finally, a new artifact entry is defined in that workspace.

		The order is load-bearing: copy, then insert, then a best-effort staging delete. Inserting
		before the copy would leave a row pointing at nothing, so it is never done. The accepted
		consequence runs the other way - a failure between the copy and the insert, including a
		rollback of an enclosing `activeSession`, leaves the final object unreferenced. That is by
		design; the object-reaping GC reclaims it as an aged final object with no backing row (see
		DESIGN §6.1, §8.2.1).

		The parent workspace is taken as already resolved by the caller and is not re-read here; the
		foreign key constraint remains the real guard (see DESIGN §7.5).

			@param ctx context.Context - execution context
			@param workspace models.Workspace - workspace this is for
			@param stagingObjKey string - the staging object returned by `GetArtifactStagingPutURL`.
			    The system expects the artifact entry's data file is stored at this location already.
			@param name string - name a user will reference artifact by. This is typically
			    structured as a file system path.
			@param description *string - an optional description for the artifact
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
			@returns the new artifact entry
	*/
	RegisterNewArtifact(
		ctx context.Context,
		workspace models.Workspace,
		stagingObjKey string,
		name string,
		description *string,
		activeSession db.Database,
	) (models.Artifact, error)

	/*
		UpdateArtifactContent replace an existing artifact's content with a newly staged object.

		This is `RegisterNewArtifact` minus the insert: the artifact entry already exists, so the
		function only needs to determine the newly staged object's size and MIME type, copy it
		into a new final storage object key, and repoint the entry at it (see DESIGN §6.3).

		Every pre-copy check `RegisterNewArtifact` performs applies here unchanged - the staging
		object key is verified as belonging to this artifact's workspace, and the object's
		measured size is checked against the single-PUT size cap before anything is copied, so an
		over-cap object never reaches the entry (see DESIGN §7.5).

		The content is always copied to a NEW final object key rather than over the existing one.
		The entry is then flipped to the new key in a single update, so a reader never observes it
		pointing at a half-written object. The old object is left behind by design; the
		object-reaping GC reclaims it as an aged final object with no backing row (see DESIGN
		§6.2, §6.3, §8.2.1).

		Concurrent updates are last-writer-wins. Each writer copies to its own new key, and the
		final entry update decides the winner; the loser's object is reclaimed the same way. There
		is no optimistic locking (see DESIGN §7.5.2).

		An artifact quarantined as `MISSING_OBJECT` is a legitimate target: re-uploading its bytes
		is exactly how it is brought back into service. The state transition is validated by the
		persistence layer, so no state gate is applied here.

			@param ctx context.Context - execution context
			@param artifact models.Artifact - the artifact to update
			@param stagingObjKey string - the staging object returned by `GetArtifactStagingPutURL`.
			    The system expects the artifact entry's new data file is stored at this location
			    already.
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
			@returns the updated artifact entry
	*/
	UpdateArtifactContent(
		ctx context.Context,
		artifact models.Artifact,
		stagingObjKey string,
		activeSession db.Database,
	) (models.Artifact, error)

	/*
		ListWorkspaceArtifacts list artifacts in a particular workspace

		The filter's state selection is a listing option rather than a hardcoded filter: leaving it
		empty returns artifacts in every state, and the caller decides what to surface - typically
		`RECORDED`, or `MISSING_OBJECT` when triaging (see DESIGN §7.1).

			@param ctx context.Context - execution context
			@param workspace models.Workspace - workspace this is for
			@param filters db.ArtifactQueryFilter - query filtering conditions
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
			@returns list of artifacts
	*/
	ListWorkspaceArtifacts(
		ctx context.Context,
		workspace models.Workspace,
		filters db.ArtifactQueryFilter,
		activeSession db.Database,
	) ([]models.Artifact, error)

	/*
		GetArtifact fetch a particular artifact

			@param ctx context.Context - execution context
			@param artifactID string - ID of artifact to fetch
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
			@returns artifact entry
	*/
	GetArtifact(
		ctx context.Context, artifactID string, activeSession db.Database,
	) (models.Artifact, error)

	/*
		GetArtifactByName fetch a particular artifact by name

		Artifact names are unique within a workspace, so this resolves a (workspace, name) pair to
		exactly one entry. It backs the MCP layer's name -> ID resolution (see DESIGN §3).

			@param ctx context.Context - execution context
			@param workspace models.Workspace - workspace this is for
			@param name string - name of the artifact to fetch
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
			@returns artifact entry
	*/
	GetArtifactByName(
		ctx context.Context, workspace models.Workspace, name string, activeSession db.Database,
	) (models.Artifact, error)

	/*
		GenerateGetURLForArtifact generate a GET URL for a particular artifact

		The caller need to specify a valid TTL for the GET URL.

		Only a `RECORDED` artifact can be served: one quarantined as `MISSING_OBJECT` has no backing
		object, so a URL minted for it would only resolve to a not-found at fetch time (see DESIGN
		§7.1).

		Every URL is minted forcing `Content-Disposition: attachment`, with no branch on who
		consumes it. Minting time is what makes this hold - the disposition is a signed query
		parameter, so neither an uploader nor a sidecar can undermine it (see DESIGN §6.5).

			@param ctx context.Context - execution context
			@param artifact models.Artifact - artifact this is for
			@param ttl time.Duration - the TTL for the artifact GET URL
			@returns the GET URL
	*/
	GenerateGetURLForArtifact(
		ctx context.Context, artifact models.Artifact, ttl time.Duration,
	) (string, error)

	/*
		RenameArtifact change an artifact's name.

		A pure DB update - the backing object key carries a random suffix rather than the name, so
		a rename never touches the object store (see DESIGN §2.2).

		Names are unique within the parent workspace, and that constraint is the real guard: a
		collision surfaces as a persistence failure rather than being pre-checked here.

			@param ctx context.Context - execution context
			@param artifactID string - ID of artifact to rename
			@param newName string - the new artifact name
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
			@returns the artifact entry with an updated name
	*/
	RenameArtifact(
		ctx context.Context, artifactID string, newName string, activeSession db.Database,
	) (models.Artifact, error)

	/*
		UpdateArtifactDescription change an artifact's description

			@param ctx context.Context - execution context
			@param artifactID string - ID of artifact to update
			@param newDescription *string - the new description, nil to clear it
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
			@returns the artifact entry with an updated description
	*/
	UpdateArtifactDescription(
		ctx context.Context,
		artifactID string,
		newDescription *string,
		activeSession db.Database,
	) (models.Artifact, error)

	/*
		DeleteArtifact delete an artifact entry.

		Idempotent - deleting an absent entry is a no-op.

		No object-store interaction: the object the entry referenced is left in the store and
		reclaimed later by the object-reaping GC. Deleting it here would place a second reclaimer
		alongside the GC, and the DB is what is authoritative for what exists (see DESIGN §2.2.1,
		§4.1, §8.2.1).

			@param ctx context.Context - execution context
			@param artifactID string - ID of artifact to delete
			@param activeSession db.Database - if set, this is an existing open DB persistence
			    layer transaction, and function will perform additional persistence operations
			    within it.
	*/
	DeleteArtifact(ctx context.Context, artifactID string, activeSession db.Database) error
}

/*
MIMETypeDetector derives an object's MIME type from its leading bytes.

Injected rather than called directly so the manager holds no dependency on any particular
detection library, and so a unit test can drive the sniff without crafting bytes a real
magic-number detector would recognize.

	@param data []byte - the object's leading bytes
	@returns the detected MIME type
*/
type MIMETypeDetector func(data []byte) string

/*
DefaultMIMETypeDetector the production MIME type detector, backed by a magic-number library.

	@param data []byte - the object's leading bytes
	@returns the detected MIME type
*/
func DefaultMIMETypeDetector(data []byte) string {
	return mimetype.Detect(data).String()
}

// mimeSniffWindowBytes the number of leading bytes read from an object to detect its MIME type.
const mimeSniffWindowBytes = 3072

// getURLContentDisposition the `Content-Disposition` every presigned GET URL is minted with.
//
// A browser honors `attachment` by downloading rather than rendering, which neutralizes a
// stored-XSS payload independent of the object's `Content-Type` — which is why the sniffed
// MIME type can be demoted to advisory metadata (see DESIGN §6.5).
const getURLContentDisposition = "attachment"

// managerImpl implements Manager
type managerImpl struct {
	goutils.Component

	appName string

	validator *validator.Validate

	persistence db.Client

	s3 goutils.S3Client

	// storeConfig artifact storage config
	storeConfig models.ArtifactStorageConfig

	// detectMIMEType derives an object's MIME type from its leading bytes
	detectMIMEType MIMETypeDetector
}

/*
NewManager define a new artifact manager

	@param appName string - the per-deployment application name
	@param persistence db.Client - persistence client
	@param s3 goutils.S3Client - object store client holding this deployment's credentials.
	    Its lifecycle is the caller's responsibility.
	@param storeConfig models.ArtifactStorageConfig - artifact storage config
	@param detectMIMEType MIMETypeDetector - derives an object's MIME type from its leading
	    bytes. Pass `DefaultMIMETypeDetector` outside of tests.
	@returns the new artifact manager
*/
func NewManager(
	appName string,
	persistence db.Client,
	s3 goutils.S3Client,
	storeConfig models.ArtifactStorageConfig,
	detectMIMEType MIMETypeDetector,
) (Manager, error) {
	logTags := log.Fields{
		"package": "cairn", "module": "artifact", "component": "manager", "instance": appName,
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

	if persistence == nil {
		return nil, goutils.NewValidationError("persistence client is required", nil, true)
	}

	if s3 == nil {
		return nil, goutils.NewValidationError("object store client is required", nil, true)
	}

	// Required rather than defaulted to `DefaultMIMETypeDetector`, so the choice of detector
	// stays explicit at the wiring site.
	if detectMIMEType == nil {
		return nil, goutils.NewValidationError("MIME type detector is required", nil, true)
	}

	// Validate the storage config up front so a missing bucket, prefix, or size cap fails
	// here rather than at the first upload.
	if err := validate.Struct(&storeConfig); err != nil {
		return nil, goutils.NewValidationError(
			"artifact storage config is not valid", err, true,
		)
	}

	instance := &managerImpl{
		Component: goutils.Component{
			LogTags: logTags,
			LogTagModifiers: []goutils.LogMetadataModifier{
				goutils.ModifyLogMetadataByRestRequestParam,
			},
		},
		appName:        appName,
		validator:      validate,
		persistence:    persistence,
		s3:             s3,
		storeConfig:    storeConfig,
		detectMIMEType: detectMIMEType,
	}

	return instance, nil
}

// newStagingObjectKey define a new staging object key within a workspace's namespace.
//
// The suffix is a ULID rather than the artifact's name, so the key is stable under a rename
// and carries no caller-controlled content (see DESIGN §2.2, §8.1).
func (m *managerImpl) newStagingObjectKey(workspaceID string) string {
	return filepath.Join(
		m.storeConfig.Prefix.StagingKeyPrefix(workspaceID), ulid.Make().String(),
	)
}

// newStoreObjectKey define a new final storage object key within a workspace's namespace.
//
// Every write mints a fresh key — an update copies to a NEW key and flips the row over to
// it, never editing an object in place (see DESIGN §6.2, §6.3).
func (m *managerImpl) newStoreObjectKey(workspaceID string) string {
	return filepath.Join(
		m.storeConfig.Prefix.StoreKeyPrefix(workspaceID), ulid.Make().String(),
	)
}

// verifyStagingKeyOwnership verify a caller-supplied staging object key was issued for this
// particular workspace.
//
// Staging keys are server-generated and workspace-scoped by construction (see DESIGN §8.1),
// so a prefix match proves the key was minted for this workspace and rejects one aimed at
// another (see DESIGN §6.1 step 2.1).
func (m *managerImpl) verifyStagingKeyOwnership(workspaceID string, stagingObjKey string) error {
	prefix := m.storeConfig.Prefix.StagingKeyPrefix(workspaceID) + "/"
	if !strings.HasPrefix(stagingObjKey, prefix) {
		return goutils.NewBadInputError(
			fmt.Sprintf(
				"staging object key '%s' was not issued for workspace %s",
				stagingObjKey, workspaceID,
			), nil, true,
		)
	}
	return nil
}

// enforceSizeCap reject an object larger than the single-PUT size cap.
//
// Multipart upload is out of scope for the first cut, so "too big" is an error rather than
// something to engineer around (see DESIGN §5.2). Both the mint-time fail-fast and the
// authoritative re-check at register route through here, so the two can never disagree.
func (m *managerImpl) enforceSizeCap(size int64, subject string) error {
	if size > m.storeConfig.MaxObjectSizeBytes {
		return goutils.NewBadInputError(
			fmt.Sprintf(
				"%s is %d bytes, over the %d byte single-PUT size cap",
				subject, size, m.storeConfig.MaxObjectSizeBytes,
			), nil, true,
		)
	}
	return nil
}

// sniffObjectMIMEType derive an object's MIME type by reading its leading bytes.
//
// The upload source is not trusted to declare its own MIME type, so the server derives it
// from the bytes (see DESIGN §6). Only the leading bytes are read: the body is closed as
// soon as the detection window is filled, so the rest never transits.
func (m *managerImpl) sniffObjectMIMEType(
	ctx context.Context, objectKey string,
) (string, error) {
	_, reader, err := m.s3.GetObject(ctx, m.storeConfig.Bucket, objectKey)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := reader.Close(); err != nil {
			log.
				WithError(err).
				WithFields(m.GetLogTagsForContext(ctx)).
				WithField("object_key", objectKey).
				Warn("Failed to close object reader after MIME type sniff")
		}
	}()

	// An object shorter than the detection window is normal, and `ReadAll` already folds the
	// resulting `io.EOF` into a nil error.
	header, err := io.ReadAll(io.LimitReader(reader, mimeSniffWindowBytes))
	if err != nil {
		return "", goutils.NewObjectStoreError(
			fmt.Sprintf(
				"failed to read s3://%s/%s to detect its MIME type", m.storeConfig.Bucket, objectKey,
			), err, true,
		)
	}

	return m.detectMIMEType(header), nil
}
