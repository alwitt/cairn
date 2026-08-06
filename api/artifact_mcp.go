// Package api - application REST API
package api //revive:disable-line:var-naming

import (
	"context"
	"fmt"

	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpVolumeMountNote the sentence every volume touching tool's description ends with. Built
// from the mount path constant rather than written out, so the canonical location has one
// definition and the tools cannot drift from it (see DESIGN §4.4).
var mcpVolumeMountNote = fmt.Sprintf(
	"The workspace's persistent volume is mounted at %s, in your own container and in the "+
		"transfer container alike, so the path you give is used verbatim. It must be absolute "+
		"and inside that mount. The workspace's volume state must be READY; you cannot "+
		"provision a volume yourself, an operator must do it.",
	models.WorkspaceMountPath,
)

// registerListArtifactsTool register the list_artifacts tool.
func (h MCPHandler) registerListArtifactsTool(server *mcp.Server) error {
	toolName := "list_artifacts"
	toolDescription :=
		"List the artifacts stored in a workspace, optionally filtered by name or state, with " +
			"paging. Metadata only - use download_artifact to get an artifact's content. This is " +
			"also how you check whether a particular artifact exists: list with its name."

	return goutils.MCPAddTool(
		&h.MCPHandler,
		server,
		&mcp.Tool{Name: toolName, Description: toolDescription},
		func(
			ctxt context.Context, _ *mcp.CallToolRequest, in MCPListArtifactsParams,
		) (*mcp.CallToolResult, MCPListArtifactsResp, error) {
			if err := h.validator.Struct(&in); err != nil {
				return nil, MCPListArtifactsResp{}, goutils.NewValidationError(
					"list_artifacts parameters are not valid", err, true,
				)
			}

			workspaceEntry, err := h.resolveWorkspace(ctxt, in.WorkspaceName)
			if err != nil {
				return nil, MCPListArtifactsResp{}, goutils.NewRuntimeError(
					"Failed to fetch workspace '"+in.WorkspaceName+"'", err, true,
				)
			}

			entries, err := h.artifactMgr.ListWorkspaceArtifacts(
				ctxt, workspaceEntry, in.ToQueryFilter(), nil,
			)
			if err != nil {
				return nil, MCPListArtifactsResp{}, goutils.NewRuntimeError(
					"Failed to list artifacts of workspace '"+in.WorkspaceName+"'", err, true,
				)
			}

			return nil, NewMCPListArtifactsResp(entries), nil
		},
	)
}

// registerDownloadArtifactTool register the download_artifact tool.
//
// The read direction of the artifact transfer, run to completion before the call returns (see
// DESIGN §7.3).
func (h MCPHandler) registerDownloadArtifactTool(server *mcp.Server) error {
	toolName := "download_artifact"
	toolDescription :=
		"Write an artifact's content into the workspace's persistent volume, so a tool running " +
			"against that volume can read it. Performed synchronously - the file is in place when " +
			"this returns. The destination's parent directory must already exist; it is NOT " +
			"created for you. Only an artifact in the RECORDED state can be downloaded. " +
			mcpVolumeMountNote

	return goutils.MCPAddTool(
		&h.MCPHandler,
		server,
		&mcp.Tool{Name: toolName, Description: toolDescription},
		func(
			ctxt context.Context, _ *mcp.CallToolRequest, in MCPDownloadArtifactParams,
		) (*mcp.CallToolResult, any, error) {
			if err := h.validator.Struct(&in); err != nil {
				return nil, nil, goutils.NewValidationError(
					"download_artifact parameters are not valid", err, true,
				)
			}

			workspaceEntry, artifactEntry, err := h.resolveArtifact(
				ctxt, in.WorkspaceName, in.ArtifactName,
			)
			if err != nil {
				return nil, nil, goutils.NewRuntimeError(
					fmt.Sprintf(
						"Failed to fetch artifact '%s' of workspace '%s'",
						in.ArtifactName, in.WorkspaceName,
					), err, true,
				)
			}

			// The path check, the volume precondition, and the refusal to serve an artifact
			// with no backing object all live in the operator, so none is repeated here (see
			// DESIGN §7.5).
			if err := h.artifactOperator.DownloadArtifact(
				ctxt, workspaceEntry, artifactEntry, in.TargetPath,
			); err != nil {
				return nil, nil, goutils.NewRuntimeError(
					fmt.Sprintf(
						"Failed to download artifact '%s' of workspace '%s'",
						in.ArtifactName, in.WorkspaceName,
					), err, true,
				)
			}

			return goutils.MCPTextResult(fmt.Sprintf(
				"artifact '%s' of workspace '%s' downloaded to '%s'",
				in.ArtifactName, in.WorkspaceName, in.TargetPath,
			)), nil, nil
		},
	)
}

// registerUploadArtifactTool register the upload_artifact tool.
//
// The write direction, creating a NEW artifact. Two containers run behind this one call - one
// to measure and checksum the file, one to send it - and both have finished by the time it
// returns (see DESIGN §6.4, §7.3).
func (h MCPHandler) registerUploadArtifactTool(server *mcp.Server) error {
	toolName := "upload_artifact"
	toolDescription :=
		"Store a file from the workspace's persistent volume as a NEW artifact, so it outlives " +
			"the volume and can be retrieved later. Performed synchronously. The name must not " +
			"already be taken in this workspace - use update_artifact to replace an existing " +
			"artifact's content instead. Do not modify the file while this runs; a file that " +
			"changes mid-transfer fails the upload. " + mcpVolumeMountNote

	return goutils.MCPAddTool(
		&h.MCPHandler,
		server,
		&mcp.Tool{Name: toolName, Description: toolDescription},
		func(
			ctxt context.Context, _ *mcp.CallToolRequest, in MCPUploadArtifactParams,
		) (*mcp.CallToolResult, any, error) {
			if err := h.validator.Struct(&in); err != nil {
				return nil, nil, goutils.NewValidationError(
					"upload_artifact parameters are not valid", err, true,
				)
			}

			workspaceEntry, err := h.resolveWorkspace(ctxt, in.WorkspaceName)
			if err != nil {
				return nil, nil, goutils.NewRuntimeError(
					"Failed to fetch workspace '"+in.WorkspaceName+"'", err, true,
				)
			}

			// The operator pre-checks the name is free before spending either container, so
			// no availability check is made here (see DESIGN §7.3).
			if _, err := h.artifactOperator.UploadArtifact(
				ctxt, workspaceEntry, in.SourcePath, in.ArtifactName, in.Description, nil,
			); err != nil {
				return nil, nil, goutils.NewRuntimeError(
					fmt.Sprintf(
						"Failed to record artifact '%s' in workspace '%s'",
						in.ArtifactName, in.WorkspaceName,
					), err, true,
				)
			}

			return goutils.MCPTextResult(fmt.Sprintf(
				"artifact '%s' recorded in workspace '%s' from '%s'",
				in.ArtifactName, in.WorkspaceName, in.SourcePath,
			)), nil, nil
		},
	)
}

// registerUpdateArtifactTool register the update_artifact tool.
//
// The same two container flow as upload, replacing an existing artifact's content rather than
// creating one (see DESIGN §6.3, §7.3).
func (h MCPHandler) registerUpdateArtifactTool(server *mcp.Server) error {
	toolName := "update_artifact"
	toolDescription :=
		"Replace an EXISTING artifact's content with a file from the workspace's persistent " +
			"volume. Performed synchronously. The artifact must already exist - use " +
			"upload_artifact to create a new one. Its name and description are left as they " +
			"are; only the content changes. This also repairs an artifact in the MISSING_OBJECT " +
			"state, whose stored content was lost. Do not modify the file while this runs; a " +
			"file that changes mid-transfer fails the upload. " + mcpVolumeMountNote

	return goutils.MCPAddTool(
		&h.MCPHandler,
		server,
		&mcp.Tool{Name: toolName, Description: toolDescription},
		func(
			ctxt context.Context, _ *mcp.CallToolRequest, in MCPUpdateArtifactParams,
		) (*mcp.CallToolResult, any, error) {
			if err := h.validator.Struct(&in); err != nil {
				return nil, nil, goutils.NewValidationError(
					"update_artifact parameters are not valid", err, true,
				)
			}

			// Resolving the artifact is also the pre-container check that the target exists,
			// so an unknown name costs no container runs (see DESIGN §7.3).
			workspaceEntry, artifactEntry, err := h.resolveArtifact(
				ctxt, in.WorkspaceName, in.ArtifactName,
			)
			if err != nil {
				return nil, nil, goutils.NewRuntimeError(
					fmt.Sprintf(
						"Failed to fetch artifact '%s' of workspace '%s'",
						in.ArtifactName, in.WorkspaceName,
					), err, true,
				)
			}

			if _, err := h.artifactOperator.UpdateArtifact(
				ctxt, workspaceEntry, artifactEntry, in.SourcePath, nil,
			); err != nil {
				return nil, nil, goutils.NewRuntimeError(
					fmt.Sprintf(
						"Failed to update artifact '%s' of workspace '%s'",
						in.ArtifactName, in.WorkspaceName,
					), err, true,
				)
			}

			return goutils.MCPTextResult(fmt.Sprintf(
				"artifact '%s' of workspace '%s' updated from '%s'",
				in.ArtifactName, in.WorkspaceName, in.SourcePath,
			)), nil, nil
		},
	)
}

// registerDeleteArtifactTool register the delete_artifact tool.
func (h MCPHandler) registerDeleteArtifactTool(server *mcp.Server) error {
	toolName := "delete_artifact"
	toolDescription :=
		"Delete an artifact from a workspace. This removes the artifact and its stored " +
			"content; it cannot be undone. Files already written into the workspace volume by " +
			"download_artifact are not touched."

	return goutils.MCPAddTool(
		&h.MCPHandler,
		server,
		&mcp.Tool{Name: toolName, Description: toolDescription},
		func(
			ctxt context.Context, _ *mcp.CallToolRequest, in MCPDeleteArtifactParams,
		) (*mcp.CallToolResult, any, error) {
			if err := h.validator.Struct(&in); err != nil {
				return nil, nil, goutils.NewValidationError(
					"delete_artifact parameters are not valid", err, true,
				)
			}

			_, artifactEntry, err := h.resolveArtifact(ctxt, in.WorkspaceName, in.ArtifactName)
			if err != nil {
				return nil, nil, goutils.NewRuntimeError(
					fmt.Sprintf(
						"Failed to fetch artifact '%s' of workspace '%s'",
						in.ArtifactName, in.WorkspaceName,
					), err, true,
				)
			}

			// Deletion is by ID: the name only ever exists at this boundary (see DESIGN §3).
			if err := h.artifactMgr.DeleteArtifact(ctxt, artifactEntry.ID, nil); err != nil {
				return nil, nil, goutils.NewRuntimeError(
					fmt.Sprintf(
						"Failed to delete artifact '%s' of workspace '%s'",
						in.ArtifactName, in.WorkspaceName,
					), err, true,
				)
			}

			return goutils.MCPTextResult(fmt.Sprintf(
				"artifact '%s' deleted from workspace '%s'", in.ArtifactName, in.WorkspaceName,
			)), nil, nil
		},
	)
}

// registerRenameArtifactTool register the rename_artifact tool.
func (h MCPHandler) registerRenameArtifactTool(server *mcp.Server) error {
	toolName := "rename_artifact"
	toolDescription :=
		"Change an artifact's name. Its content and description are untouched, and any file " +
			"already downloaded into the workspace volume keeps the filename it was written " +
			"under. The new name must not already be taken in this workspace."

	return goutils.MCPAddTool(
		&h.MCPHandler,
		server,
		&mcp.Tool{Name: toolName, Description: toolDescription},
		func(
			ctxt context.Context, _ *mcp.CallToolRequest, in MCPRenameArtifactParams,
		) (*mcp.CallToolResult, any, error) {
			if err := h.validator.Struct(&in); err != nil {
				return nil, nil, goutils.NewValidationError(
					"rename_artifact parameters are not valid", err, true,
				)
			}

			_, artifactEntry, err := h.resolveArtifact(ctxt, in.WorkspaceName, in.ArtifactName)
			if err != nil {
				return nil, nil, goutils.NewRuntimeError(
					fmt.Sprintf(
						"Failed to fetch artifact '%s' of workspace '%s'",
						in.ArtifactName, in.WorkspaceName,
					), err, true,
				)
			}

			// The name uniqueness constraint within the workspace is the real guard, so a
			// collision surfaces from the update rather than being pre-checked here.
			if _, err := h.artifactMgr.RenameArtifact(
				ctxt, artifactEntry.ID, in.NewName, nil,
			); err != nil {
				return nil, nil, goutils.NewRuntimeError(
					fmt.Sprintf(
						"Failed to rename artifact '%s' of workspace '%s' to '%s'",
						in.ArtifactName, in.WorkspaceName, in.NewName,
					), err, true,
				)
			}

			return goutils.MCPTextResult(fmt.Sprintf(
				"artifact '%s' of workspace '%s' renamed to '%s'",
				in.ArtifactName, in.WorkspaceName, in.NewName,
			)), nil, nil
		},
	)
}
