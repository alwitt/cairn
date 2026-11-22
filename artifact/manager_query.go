package artifact

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
)

// ======================================================================================
// Artifact Read Operations

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
func (m *managerImpl) ListWorkspaceArtifacts(
	ctx context.Context,
	workspace models.Workspace,
	filters db.ArtifactQueryFilter,
	activeSession db.Database,
) ([]models.Artifact, error) {
	var entries []models.Artifact

	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			var err error
			entries, err = dbClient.ListWorkspaceArtifacts(dbCtx, workspace.ID, filters)
			if err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to list workspace %s artifacts", workspace.ID), err, true,
				)
			}
			return nil
		},
	); err != nil {
		return nil, models.NewArtifactMangerError(
			fmt.Sprintf("failed to list workspace %s artifacts", workspace.ID), err, true,
		)
	}

	return entries, nil
}

/*
GetArtifact fetch a particular artifact

	@param ctx context.Context - execution context
	@param artifactID string - ID of artifact to fetch
	@param activeSession db.Database - if set, this is an existing open DB persistence
	    layer transaction, and function will perform additional persistence operations
	    within it.
	@returns artifact entry
*/
func (m *managerImpl) GetArtifact(
	ctx context.Context, artifactID string, activeSession db.Database,
) (models.Artifact, error) {
	entry, err := m.readArtifactEntry(ctx, artifactID, activeSession)
	if err != nil {
		return models.Artifact{}, models.NewArtifactMangerError(
			fmt.Sprintf("failed to fetch artifact %s", artifactID), err, true,
		)
	}
	return entry, nil
}

// readArtifactEntry read one artifact entry, for the manager's own use.
//
// The failure is the persistence layer's, wrapped once; a public method wraps it in its own
// manager error, so a helper calling this does not stack a second one.
func (m *managerImpl) readArtifactEntry(
	ctx context.Context, artifactID string, activeSession db.Database,
) (models.Artifact, error) {
	var entry models.Artifact

	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			var err error
			entry, err = dbClient.GetArtifact(dbCtx, artifactID)
			if err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf("failed to read artifact %s", artifactID), err, true,
				)
			}
			return nil
		},
	); err != nil {
		return models.Artifact{}, err
	}

	return entry, nil
}

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
func (m *managerImpl) GetArtifactByName(
	ctx context.Context, workspace models.Workspace, name string, activeSession db.Database,
) (models.Artifact, error) {
	var entry models.Artifact

	if err := db.ActiveSessionWrapper(
		ctx, activeSession, m.persistence, func(dbCtx context.Context, dbClient db.Database) error {
			var err error
			entry, err = dbClient.GetArtifactByName(dbCtx, workspace.ID, name)
			if err != nil {
				return goutils.NewPersistenceError(
					fmt.Sprintf(
						"failed to read artifact '%s' of workspace %s", name, workspace.ID,
					), err, true,
				)
			}
			return nil
		},
	); err != nil {
		return models.Artifact{}, models.NewArtifactMangerError(
			fmt.Sprintf("failed to fetch artifact '%s' of workspace %s", name, workspace.ID),
			err,
			true,
		)
	}

	return entry, nil
}

/*
verifyArtifactNameFree confirm no artifact by this name exists in the workspace yet.

A fail-fast before any bytes are staged. The volume upload would otherwise spend two
container runs (see DESIGN §7.3), and the inline text create a staging write, a sniff, and a
copy, only to be rejected by the database's uniqueness constraint. The constraint is still the
real guard - this loses to a caller that races another between here and the insert, and is
meant to.

The three-way branch is the point of the function. Treating any error as "the name is free"
would turn a database outage into wasted staging work followed by a constraint violation,
which is exactly the outcome being avoided.

	@param ctx context.Context - execution context
	@param manager Manager - the artifact manager to look the name up through
	@param workspace models.Workspace - workspace this is for
	@param name string - the artifact name to check
	@param activeSession db.Database - if set, an existing open DB transaction to work within
*/
func verifyArtifactNameFree(
	ctx context.Context,
	manager Manager,
	workspace models.Workspace,
	name string,
	activeSession db.Database,
) error {
	_, err := manager.GetArtifactByName(ctx, workspace, name, activeSession)

	if err == nil {
		return goutils.NewBadInputError(
			fmt.Sprintf(
				"workspace %s already has an artifact named '%s'", workspace.ID, name,
			), nil, true,
		)
	}

	// Not-found is the success case here. It survives the manager's wrapping because the
	// goutils error types unwrap, so matching on the type reaches it through the chain.
	var notFound goutils.NotFoundError
	if errors.As(err, &notFound) {
		return nil
	}

	return err
}

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
func (m *managerImpl) GenerateGetURLForArtifact(
	ctx context.Context, artifact models.Artifact, ttl time.Duration,
) (string, error) {
	if artifact.State != models.ArtifactStateRecorded {
		return "", models.NewArtifactMangerError(
			fmt.Sprintf("failed to generate GET URL for artifact %s", artifact.ID),
			goutils.NewConsistencyError(
				fmt.Sprintf(
					"artifact %s is '%s'; only a '%s' artifact has a servable object",
					artifact.ID, artifact.State, models.ArtifactStateRecorded,
				), nil, true,
			),
			true,
		)
	}

	disposition := getURLContentDisposition

	s3Client, err := m.s3Client(ctx)
	if err != nil {
		return "", models.NewArtifactMangerError(
			fmt.Sprintf("failed to generate GET URL for artifact %s", artifact.ID), err, true,
		)
	}

	// The TTL bounds are validated by the object store client, so they are not re-checked here.
	getURL, err := s3Client.GeneratePresignedGetURL(
		ctx, m.storeConfig.Bucket, artifact.ObjectKey, ttl, &disposition,
	)
	if err != nil {
		return "", models.NewArtifactMangerError(
			fmt.Sprintf("failed to generate GET URL for artifact %s", artifact.ID), err, true,
		)
	}

	return getURL.String(), nil
}

/*
ReadTextArtifactContent read a range of lines from a plain text artifact.

Only a plain text artifact can be read: its MIME type must name a text format, checked
before anything is fetched, and its content must be plain text (see DESIGN §7.6.1). The
lines are returned with LF line endings, whatever the artifact uses, and the artifact's
own line ending is reported alongside (see DESIGN §7.6.2).

The content returned always belongs to the revision of the entry returned with it. If
the artifact moves to a new object while its content is being read, the read starts over
against the newer entry, up to the configured number of attempts.

	@param ctx context.Context - execution context
	@param artifact models.Artifact - the artifact to read
	@param offset int - 1-based number of the first line to return
	@param limit *int - if set, the most lines to return; otherwise every line from offset
	@returns the selected lines and the entry they were read from
*/
func (m *managerImpl) ReadTextArtifactContent(
	ctx context.Context, artifact models.Artifact, offset int, limit *int,
) (TextArtifactContent, error) {
	failure := func(err error) (TextArtifactContent, error) {
		return TextArtifactContent{}, models.NewArtifactMangerError(
			fmt.Sprintf("failed to read text artifact %s", artifact.ID), err, true,
		)
	}

	if offset < 1 {
		return failure(goutils.NewValidationError(
			fmt.Sprintf("offset %d is not valid; lines are numbered from 1", offset), nil, true,
		))
	}
	if limit != nil && *limit < 1 {
		return failure(goutils.NewValidationError(
			fmt.Sprintf("limit %d is not valid; it must be at least 1", *limit), nil, true,
		))
	}

	entry := artifact
	for attempt := 1; ; attempt++ {
		raw, err := m.readTextObject(ctx, entry, nil)
		if err != nil {
			if !isRevisionConflict(err) || attempt >= m.storeConfig.TextMaxAttempts {
				return failure(err)
			}
			log.
				WithError(err).
				WithFields(m.GetLogTagsForContext(ctx)).
				WithField("artifact", artifact.ID).
				WithField("attempt", attempt).
				Debug("Artifact changed while its text was being read; starting over")
			if entry, err = m.readArtifactEntry(ctx, artifact.ID, nil); err != nil {
				return failure(err)
			}
			continue
		}

		lineEnding, err := checkPlainText(raw)
		if err != nil {
			return failure(goutils.NewBadInputError(
				fmt.Sprintf("artifact %s content is not plain text", entry.ID), err, true,
			))
		}

		content, totalLines := sliceLines(toLF(string(raw)), offset, limit)
		return TextArtifactContent{
			Artifact:   entry,
			Content:    content,
			StartLine:  offset,
			TotalLines: totalLines,
			LineEnding: lineEnding,
		}, nil
	}
}
