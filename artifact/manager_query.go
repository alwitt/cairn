package artifact

import (
	"context"
	"fmt"
	"time"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
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
		return models.Artifact{}, models.NewArtifactMangerError(
			fmt.Sprintf("failed to fetch artifact %s", artifactID), err, true,
		)
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
