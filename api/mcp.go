// Package api - application REST API
package api //revive:disable-line:var-naming

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/alwitt/cairn/artifact"
	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/cairn/workspace"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
	"github.com/go-playground/validator/v10"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ======================================================================================
// MCP tool call parameters
//
// These structs are the agent-facing projection of the artifact and workspace operations
// (see DESIGN §7.2). Unlike REST, an MCP tool call carries a single JSON object, so what a
// REST request splits across its path, query string, and body is flattened into one struct
// per tool.
//
// Every one of them addresses entries by NAME rather than by ID (see DESIGN §3): an agent
// cannot reliably hold or echo a ULID. Name -> ID resolution happens once, in the tool
// handler; nothing downstream of it ever sees a name.
//
// Three conventions govern the tags, all of them consequences of how the schema reaches the
// agent:
//
//  1. The `jsonschema` tag is the ONLY thing the agent reads, so every constraint has to be
//     written into it. jsonschema-go folds the entire tag into the property's `description`
//     - no `minimum`, `enum`, `default`, or `format` keyword comes out of it, and it does
//     not read `validate` tags at all. So any constraint a field's `validate` tag expresses
//     is also stated in prose in its `jsonschema` tag: the `validate` tag enforces, the
//     `jsonschema` tag is how the agent finds out.
//
//  2. An ENUM field's description names every valid member and says what each one means.
//     `goutils.MCPInstallEnumSchema` puts the member strings into the schema's `enum`, but
//     nothing describing them is threaded through to the tool listing the agent sees, so the
//     field's own description has to carry it.
//
//  3. The canonical volume mount path is named in the tool's description rather than in
//     these tags. A struct tag takes a compile-time constant only, so writing the path here
//     would be a second copy of `models.WorkspaceMountPath` - which DESIGN §4.4 keeps as a
//     single named constant precisely so it can become configurable later. The fields below
//     describe the constraint ("absolute, within the workspace volume mount"); the tool
//     description, built at registration, names the path from the constant.

// ======================================================================================
// Workspace tool call parameters

// MCPGetWorkspaceParams parameters for the get_workspace tool.
//
// The read-only workspace lookup an agent uses to confirm a workspace exists and to find out
// whether its volume is ready to mount (see DESIGN §7.2).
type MCPGetWorkspaceParams struct {
	// WorkspaceName name of the workspace to fetch
	WorkspaceName string `json:"workspace_name" validate:"required,valid_name" jsonschema:"name of the workspace to fetch; can only contain alphanumeric characters, - and _"`
}

// MCPListWorkspacesParams parameters for the list_workspaces tool.
//
// Mirrors the REST listing filters, minus the ones an agent has no business driving: the
// persistence filter also selects by workspace ID, which would route around the name
// addressing this layer exists to confine (see DESIGN §3).
type MCPListWorkspacesParams struct {
	// WorkspaceNames restrict the listing to these exact workspace names
	WorkspaceNames []string `json:"workspace_names,omitempty" validate:"omitempty,dive,valid_name" jsonschema:"restrict the listing to these exact workspace names, each of which can only contain alphanumeric characters, - and _; omit to list every workspace"`

	// VolumeStates restrict the listing to workspaces whose persistent volume is in one of
	// these states
	VolumeStates []models.WorkspaceVolumeStateENUM `json:"volume_states,omitempty" validate:"omitempty,dive,volume_state" jsonschema:"restrict the listing to workspaces whose persistent volume is in one of these states; omit to list workspaces in every state. READY: the volume exists and can be mounted - only such a workspace can be used to move artifacts to or from a volume. NONE: no volume has been provisioned, and you cannot provision one yourself; an operator must do it."`

	// Limit max number of workspaces to return
	Limit *int `json:"limit,omitempty" validate:"omitempty,gt=0" jsonschema:"max number of workspaces to return; must be > 0. Omit to return every match."`
	// Offset number of leading workspaces to skip
	Offset *int `json:"offset,omitempty" validate:"omitempty,gte=0" jsonschema:"number of leading workspaces to skip, for paging through a long listing; must be >= 0"`
}

/*
ToQueryFilter project the agent-facing listing options onto the persistence query filter.

No state default is applied, unlike the artifact listing: a workspace has no quarantine state
to keep out of an agent's way, and DESIGN §7.2 specifies none.

	@returns the equivalent persistence layer query filter
*/
func (p MCPListWorkspacesParams) ToQueryFilter() db.WorkspaceQueryFilter {
	return db.WorkspaceQueryFilter{
		CommonListEntryQueryFilter: db.CommonListEntryQueryFilter{
			Limit: p.Limit, Offset: p.Offset,
		},
		TargetNames:  p.WorkspaceNames,
		VolumeStates: p.VolumeStates,
	}
}

// ======================================================================================
// Artifact tool call parameters

// MCPListArtifactsParams parameters for the list_artifacts tool.
//
// Mirrors the REST listing filters, minus the ones an agent has no business driving: the
// persistence filter also selects by artifact ID and by backing object key, either of which
// would route around the name addressing this layer exists to confine (see DESIGN §3).
type MCPListArtifactsParams struct {
	// WorkspaceName name of the workspace whose artifacts to list
	WorkspaceName string `json:"workspace_name" validate:"required,valid_name" jsonschema:"name of the workspace whose artifacts to list; can only contain alphanumeric characters, - and _"`

	// ArtifactNames restrict the listing to these exact artifact names
	ArtifactNames []string `json:"artifact_names,omitempty" validate:"omitempty,dive,valid_name" jsonschema:"restrict the listing to these exact artifact names, each of which can only contain alphanumeric characters, - and _; omit to list every artifact in the workspace"`

	// States restrict the listing to artifacts in one of these states
	States []models.ArtifactStateENUM `json:"states,omitempty" validate:"omitempty,dive,artifact_state" jsonschema:"restrict the listing to artifacts in one of these states; omitting this lists only RECORDED artifacts, which is almost always what is wanted. RECORDED: the artifact is registered and its content is stored, the normal usable state. MISSING_OBJECT: the artifact's stored content is gone and only its metadata remains, a quarantine state - ask for it when investigating a missing artifact, not to work with one."`

	// Limit max number of artifacts to return
	Limit *int `json:"limit,omitempty" validate:"omitempty,gt=0" jsonschema:"max number of artifacts to return; must be > 0. Omit to return every match."`
	// Offset number of leading artifacts to skip
	Offset *int `json:"offset,omitempty" validate:"omitempty,gte=0" jsonschema:"number of leading artifacts to skip, for paging through a long listing; must be >= 0"`
}

/*
ToQueryFilter project the agent-facing listing options onto the persistence query filter.

The parent workspace is not carried here: the persistence layer scopes the listing from the
workspace ID passed alongside the filter, so the tool handler resolves the workspace name and
hands that over separately.

An empty state selection means "artifacts in every state" at the persistence layer, so the
`RECORDED` default is applied HERE rather than left empty - an agent that never asked about
quarantined entries should not be shown them (see DESIGN §7.1).

	@returns the equivalent persistence layer query filter
*/
func (p MCPListArtifactsParams) ToQueryFilter() db.ArtifactQueryFilter {
	states := p.States
	if len(states) == 0 {
		states = []models.ArtifactStateENUM{models.ArtifactStateRecorded}
	}

	return db.ArtifactQueryFilter{
		CommonListEntryQueryFilter: db.CommonListEntryQueryFilter{
			Limit: p.Limit, Offset: p.Offset,
		},
		TargetNames:    p.ArtifactNames,
		ArtifactStates: states,
	}
}

// MCPDownloadArtifactParams parameters for the download_artifact tool.
//
// The read direction of the artifact transfer: object -> volume, over a single sidecar (see
// DESIGN §7.2, §7.4).
type MCPDownloadArtifactParams struct {
	// WorkspaceName name of the workspace holding the artifact
	WorkspaceName string `json:"workspace_name" validate:"required,valid_name" jsonschema:"name of the workspace holding the artifact; can only contain alphanumeric characters, - and _"`

	// ArtifactName name of the artifact to download
	ArtifactName string `json:"artifact_name" validate:"required,valid_name" jsonschema:"name of the artifact to download; can only contain alphanumeric characters, - and _"`

	// TargetPath where to write the artifact within the workspace volume
	TargetPath string `json:"target_path" validate:"required" jsonschema:"where to write the artifact's content, as an absolute path within the workspace volume mount. The parent directory must already exist - it is NOT created for you. Any file already at this path is overwritten."`
}

// MCPUploadArtifactParams parameters for the upload_artifact tool.
//
// The write direction of the artifact transfer, creating a NEW artifact: volume -> object,
// over two sidecars, since the content's size and checksum must be derived from the volume
// before an upload URL can be minted for it (see DESIGN §6.4, §7.3).
type MCPUploadArtifactParams struct {
	// WorkspaceName name of the workspace to record the new artifact in
	WorkspaceName string `json:"workspace_name" validate:"required,valid_name" jsonschema:"name of the workspace to record the new artifact in; can only contain alphanumeric characters, - and _"`

	// SourcePath the file to upload, within the workspace volume
	SourcePath string `json:"source_path" validate:"required" jsonschema:"the file to record as the artifact's content, as an absolute path within the workspace volume mount. A symlink is followed, and is rejected if what it points at lies outside the mount."`

	// ArtifactName name to give the new artifact
	ArtifactName string `json:"artifact_name" validate:"required,valid_name" jsonschema:"name to give the new artifact; can only contain alphanumeric characters, - and _, and must not already be taken by another artifact in this workspace"`

	// Description an optional description for the new artifact
	Description *string `json:"description,omitempty" validate:"omitempty" jsonschema:"an optional description for the new artifact"`
}

// MCPUpdateArtifactParams parameters for the update_artifact tool.
//
// The same two-sidecar flow as upload_artifact, but replacing an EXISTING artifact's content
// rather than creating one (see DESIGN §6.3, §7.3). No description: this replaces content
// only.
type MCPUpdateArtifactParams struct {
	// WorkspaceName name of the workspace holding the artifact
	WorkspaceName string `json:"workspace_name" validate:"required,valid_name" jsonschema:"name of the workspace holding the artifact; can only contain alphanumeric characters, - and _"`

	// ArtifactName name of the existing artifact whose content is replaced
	ArtifactName string `json:"artifact_name" validate:"required,valid_name" jsonschema:"name of the existing artifact whose content is replaced; can only contain alphanumeric characters, - and _"`

	// SourcePath the file to upload, within the workspace volume
	SourcePath string `json:"source_path" validate:"required" jsonschema:"the file to become the artifact's new content, as an absolute path within the workspace volume mount. A symlink is followed, and is rejected if what it points at lies outside the mount."`
}

// MCPDeleteArtifactParams parameters for the delete_artifact tool.
type MCPDeleteArtifactParams struct {
	// WorkspaceName name of the workspace holding the artifact
	WorkspaceName string `json:"workspace_name" validate:"required,valid_name" jsonschema:"name of the workspace holding the artifact; can only contain alphanumeric characters, - and _"`

	// ArtifactName name of the artifact to delete
	ArtifactName string `json:"artifact_name" validate:"required,valid_name" jsonschema:"name of the artifact to delete; can only contain alphanumeric characters, - and _"`
}

// MCPRenameArtifactParams parameters for the rename_artifact tool.
type MCPRenameArtifactParams struct {
	// WorkspaceName name of the workspace holding the artifact
	WorkspaceName string `json:"workspace_name" validate:"required,valid_name" jsonschema:"name of the workspace holding the artifact; can only contain alphanumeric characters, - and _"`

	// ArtifactName current name of the artifact to rename
	ArtifactName string `json:"artifact_name" validate:"required,valid_name" jsonschema:"current name of the artifact to rename; can only contain alphanumeric characters, - and _"`

	// NewName the new artifact name
	NewName string `json:"new_name" validate:"required,valid_name" jsonschema:"the new name for the artifact; can only contain alphanumeric characters, - and _, and must not already be taken by another artifact in this workspace"`
}

// ======================================================================================
// Text artifact tool call parameters
//
// The inline text tools: the content travels in the call itself, so none of them names a
// volume path or touches the volume (see DESIGN §6.6, §7.6).

// MCPReadTextArtifactParams parameters for the read_text_artifact tool.
type MCPReadTextArtifactParams struct {
	// WorkspaceName name of the workspace holding the artifact
	WorkspaceName string `json:"workspace_name" validate:"required,valid_name" jsonschema:"name of the workspace holding the artifact; can only contain alphanumeric characters, - and _"`

	// ArtifactName name of the plain text artifact to read
	ArtifactName string `json:"artifact_name" validate:"required,valid_name" jsonschema:"name of the plain text artifact to read; can only contain alphanumeric characters, - and _"`

	// Offset 1-based number of the first line to return
	Offset *int `json:"offset,omitempty" validate:"omitempty,gte=1" jsonschema:"number of the first line to return, counting from 1; must be >= 1. Omit to start at line 1."`

	// Limit max number of lines to return
	Limit *int `json:"limit,omitempty" validate:"omitempty,gte=1" jsonschema:"max number of lines to return; must be >= 1. Omit to return every line from offset to the end."`

	// LineNumbers prefix each returned line with its line number
	LineNumbers bool `json:"line_numbers,omitempty" jsonschema:"prefix each returned line with its line number and one tab; default false. The prefix is not part of the artifact: leave the number and the tab out of any text you copy into an edit's old_text."`
}

// MCPCreateTextArtifactParams parameters for the create_text_artifact tool.
//
// `Content` carries no `required` rule: an empty file is a legitimate artifact. The schema still
// requires the key, so an agent cannot leave it out by accident.
type MCPCreateTextArtifactParams struct {
	// WorkspaceName name of the workspace to record the new artifact in
	WorkspaceName string `json:"workspace_name" validate:"required,valid_name" jsonschema:"name of the workspace to record the new artifact in; can only contain alphanumeric characters, - and _"`

	// ArtifactName name to give the new artifact
	ArtifactName string `json:"artifact_name" validate:"required,valid_name" jsonschema:"name to give the new artifact; can only contain alphanumeric characters, - and _, and must not already be taken by another artifact in this workspace"`

	// Content the full text of the new artifact
	Content string `json:"content" jsonschema:"the full text of the new artifact, stored exactly as given. It must be plain text: valid UTF-8, with no control characters other than tab and line breaks, and one line ending (LF or CRLF) throughout."`

	// Description an optional description for the new artifact
	Description *string `json:"description,omitempty" validate:"omitempty" jsonschema:"an optional description for the new artifact"`
}

// MCPEditTextArtifactParams parameters for the edit_text_artifact tool.
//
// Each edit's own rules - non-empty, different from its replacement, found exactly once - are
// left to the manager, whose errors name the edit that failed (see DESIGN §7.6.4).
type MCPEditTextArtifactParams struct {
	// WorkspaceName name of the workspace holding the artifact
	WorkspaceName string `json:"workspace_name" validate:"required,valid_name" jsonschema:"name of the workspace holding the artifact; can only contain alphanumeric characters, - and _"`

	// ArtifactName name of the plain text artifact to edit
	ArtifactName string `json:"artifact_name" validate:"required,valid_name" jsonschema:"name of the plain text artifact to edit; can only contain alphanumeric characters, - and _"`

	// Edits the replacements to make, in order
	Edits []models.TextEdit `json:"edits" validate:"required,min=1" jsonschema:"the replacements to make, at least one. They apply in order, each to the text the one before it left. All or nothing: if any edit fails, nothing is written."`

	// BaseRevision if set, refuse unless the artifact is still at this revision
	BaseRevision *int64 `json:"base_revision,omitempty" validate:"omitempty,gte=1" jsonschema:"optional strict mode: refuse the edit unless the artifact is still at this revision, the one read_text_artifact reported; must be >= 1. Omit it and a change elsewhere in the artifact since your read lets the edit through, as long as every old_text still matches."`
}

// ======================================================================================
// MCP tool call responses
//
// The read tools carry one, and so do the two text write tools: their result is the new
// revision, a value an agent passes back as a later edit's `base_revision`, so it is given as
// a field alongside the text confirmation. The confirmation is repeated in a `message` field,
// since some clients show an agent only the structured result when there is one.
//
// The other action tools return only a plain text confirmation and signal failure through
// their handler's error return, which the SDK surfaces as `CallToolResult.IsError` - MCP
// already carries the success/error signal and the request correlation at the protocol level,
// so there is nothing for a structured result to add.
//
// These are deliberately NARROW projections of the `models` types rather than the types
// themselves. An agent's whole job here is to pick a workspace and find an artifact by name;
// the entry ULIDs, the backing object key, the derived volume name, and the volume
// provisioning metadata are none of its business and would only invite it to reason about
// identifiers it must not use (see DESIGN §3).
//
// Dropping `models.Workspace.VolumeMetadata` also avoids a concrete failure rather than just
// trimming noise: it is a `datatypes.JSONType`, which jsonschema-go infers as a "null"/"array"
// schema while the marshaled value is the metadata object, and the SDK's output validator
// rejects that discrepancy.
//
// Get and list carry separate entry types even where their fields currently coincide, so
// either tool's shape can move without dragging the other along with it.

// ======================================================================================
// Workspace tool call responses

// MCPWorkspaceDetail the agent-facing projection of one workspace, as returned by the
// get_workspace tool.
//
// Deliberately does NOT report how many containers currently mount the workspace's volume,
// which the REST fetch does return (see DESIGN §7.1). An agent never tears a volume down -
// that is the operator's job - so the count would be a number it could do nothing with.
type MCPWorkspaceDetail struct {
	// Name of the workspace
	Name string `json:"name" jsonschema:"name of the workspace"`

	// VolumeState state of the workspace's persistent volume
	VolumeState models.WorkspaceVolumeStateENUM `json:"volume_state" jsonschema:"state of the workspace's persistent volume. READY: the volume exists and can be mounted - artifacts can be moved to and from it. NONE: no volume has been provisioned, so no artifact transfer is possible; you cannot provision one yourself, an operator must do it."`

	// CreatedAt when the workspace was created
	CreatedAt time.Time `json:"created_at" jsonschema:"when the workspace was created"`
}

// MCPGetWorkspaceResp response for the get_workspace tool.
type MCPGetWorkspaceResp struct {
	// Workspace the requested workspace
	Workspace MCPWorkspaceDetail `json:"workspace" jsonschema:"the requested workspace"`
}

/*
NewMCPGetWorkspaceResp project a stored workspace onto the get_workspace tool response.

	@param workspace models.Workspace - the workspace to project
	@returns the tool response
*/
func NewMCPGetWorkspaceResp(workspace models.Workspace) MCPGetWorkspaceResp {
	return MCPGetWorkspaceResp{
		Workspace: MCPWorkspaceDetail{
			Name:        workspace.Name,
			VolumeState: workspace.VolumeState,
			CreatedAt:   workspace.CreatedAt,
		},
	}
}

// MCPListWorkspacesResp response for the list_workspaces tool.
type MCPListWorkspacesResp struct {
	// Workspaces the workspaces matching the listing options
	Workspaces []MCPWorkspaceDetail `json:"workspaces" jsonschema:"the workspaces matching the listing options"`
}

/*
NewMCPListWorkspacesResp project a set of stored workspaces onto the list_workspaces tool
response.

The slice is always allocated, so an empty listing marshals as `[]` rather than as `null` -
the output schema types the field as an array.

	@param workspaces []models.Workspace - the workspaces to project
	@returns the tool response
*/
func NewMCPListWorkspacesResp(workspaces []models.Workspace) MCPListWorkspacesResp {
	entries := make([]MCPWorkspaceDetail, len(workspaces))
	for idx, workspace := range workspaces {
		entries[idx] = MCPWorkspaceDetail{
			Name:        workspace.Name,
			VolumeState: workspace.VolumeState,
			CreatedAt:   workspace.CreatedAt,
		}
	}
	return MCPListWorkspacesResp{Workspaces: entries}
}

// ======================================================================================
// Artifact tool call responses

// MCPArtifactListEntry the agent-facing projection of one artifact within a listing, as
// returned by the list_artifacts tool.
//
// There is no single-artifact counterpart because DESIGN §7.2 defines no get_artifact tool:
// an agent after one artifact's metadata lists with that name.
type MCPArtifactListEntry struct {
	// Name of the artifact
	Name string `json:"name" jsonschema:"name of the artifact, unique within its workspace"`

	// Description the artifact's description, null when it has none
	Description *string `json:"description" jsonschema:"the artifact's description, null when it has none"`

	// State of the artifact
	State models.ArtifactStateENUM `json:"state" jsonschema:"state of the artifact. RECORDED: the artifact is registered and its content is stored, the normal usable state. MISSING_OBJECT: the artifact's stored content is gone and only its metadata remains, a quarantine state - such an artifact cannot be downloaded, though replacing its content restores it."`

	// Revision content revision of the artifact
	Revision int64 `json:"revision" jsonschema:"content revision of the artifact: 1 when created, and one higher after every content change. A strict edit passes it to edit_text_artifact as base_revision."`

	// UpdatedAt when the artifact was last modified
	UpdatedAt time.Time `json:"updated_at" jsonschema:"when the artifact was last modified, whether its content, name, or description"`
}

// MCPListArtifactsResp response for the list_artifacts tool.
type MCPListArtifactsResp struct {
	// Artifacts the artifacts matching the listing options
	Artifacts []MCPArtifactListEntry `json:"artifacts" jsonschema:"the artifacts matching the listing options"`
}

/*
NewMCPListArtifactsResp project a set of stored artifacts onto the list_artifacts tool
response.

The slice is always allocated, for the reason given on NewMCPListWorkspacesResp.

	@param artifacts []models.Artifact - the artifacts to project
	@returns the tool response
*/
func NewMCPListArtifactsResp(artifacts []models.Artifact) MCPListArtifactsResp {
	entries := make([]MCPArtifactListEntry, len(artifacts))
	for idx, artifact := range artifacts {
		entries[idx] = MCPArtifactListEntry{
			Name:        artifact.Name,
			Description: artifact.Description,
			State:       artifact.State,
			Revision:    artifact.Revision,
			UpdatedAt:   artifact.UpdatedAt,
		}
	}
	return MCPListArtifactsResp{Artifacts: entries}
}

// ======================================================================================
// Text artifact tool call responses

// MCPReadTextArtifactResp response for the read_text_artifact tool.
type MCPReadTextArtifactResp struct {
	// Content the requested lines
	Content string `json:"content" jsonschema:"the requested lines, with LF line endings. Without line_numbers this is the artifact's exact text, so any part of it can be copied into an edit's old_text as is."`

	// StartLine number of the first line in Content
	StartLine int `json:"start_line" jsonschema:"number of the first line in content, counting from 1"`

	// TotalLines number of lines in the whole artifact
	TotalLines int `json:"total_lines" jsonschema:"number of lines in the whole artifact, so you can tell whether there is more to read"`

	// Revision the content revision the lines belong to
	Revision int64 `json:"revision" jsonschema:"content revision these lines belong to. Pass it to edit_text_artifact as base_revision to make an edit fail if anything changed since this read."`

	// LineEnding the line ending the artifact itself uses
	LineEnding models.TextLineEndingENUM `json:"line_ending" jsonschema:"the line ending the artifact itself uses. LF: every line ends with LF. CRLF: every line ends with CR LF. Either way content comes back with LF, edits are matched against LF, and an edit keeps the artifact's own line ending."`
}

/*
NewMCPReadTextArtifactResp project the lines read from a text artifact onto the
read_text_artifact tool response.

The revision is taken from the entry the content was read from, which is the revision the
content belongs to even when the read had to start over against a newer one.

	@param read artifact.TextArtifactContent - what was read
	@param lineNumbers bool - whether to prefix each line with its line number
	@returns the tool response
*/
func NewMCPReadTextArtifactResp(
	read artifact.TextArtifactContent, lineNumbers bool,
) MCPReadTextArtifactResp {
	content := read.Content
	if lineNumbers {
		content = numberLines(content, read.StartLine)
	}
	return MCPReadTextArtifactResp{
		Content:    content,
		StartLine:  read.StartLine,
		TotalLines: read.TotalLines,
		Revision:   read.Artifact.Revision,
		LineEnding: read.LineEnding,
	}
}

/*
numberLines prefix each line with its line number and one tab.

No padding: the tab is the separator, which is all an agent needs to strip the prefix (see
DESIGN §7.6.3). A last line with no line ending is still numbered, and empty content stays
empty.

	@param content string - LF text, one or more whole lines
	@param startLine int - the number of the first line
	@returns the numbered text
*/
func numberLines(content string, startLine int) string {
	var builder strings.Builder
	for idx, line := range strings.SplitAfter(content, "\n") {
		// The split leaves an empty element after a final line ending; it is not a line.
		if line == "" {
			continue
		}
		fmt.Fprintf(&builder, "%d\t%s", startLine+idx, line)
	}
	return builder.String()
}

// MCPCreateTextArtifactResp response for the create_text_artifact tool.
type MCPCreateTextArtifactResp struct {
	// Message the confirmation also carried as the tool's text result
	Message string `json:"message" jsonschema:"the same confirmation as the tool's text result, repeated here for clients that show only this structured result"`

	// Revision the new artifact's content revision
	Revision int64 `json:"revision" jsonschema:"content revision of the new artifact, which is always 1"`
}

// MCPEditTextArtifactResp response for the edit_text_artifact tool.
type MCPEditTextArtifactResp struct {
	// Message the confirmation also carried as the tool's text result
	Message string `json:"message" jsonschema:"the same confirmation as the tool's text result, repeated here for clients that show only this structured result"`

	// Revision the artifact's content revision after the edit
	Revision int64 `json:"revision" jsonschema:"content revision of the artifact after the edit. Pass it as base_revision to make a follow-up edit fail if anything changes in between."`
}

// ======================================================================================
// Main MCP Handler

// mcpServerName the implementation name this server announces at handshake.
const mcpServerName = "cairn"

// mcpServerVersion the implementation version this server announces at handshake. This is the
// MCP server implementation's version, not a release version - cairn carries none yet - so it
// is maintained by hand.
const mcpServerVersion = "0.1.0"

// mcpServerInstructions the server level explanation returned with the initialize result.
//
// A client MAY fold this into the model's system prompt and MAY ignore it, so nothing
// load-bearing lives here: every precondition an agent must respect is also stated in the
// description of the tool that enforces it. What this carries instead is the two things no
// single tool description can - what a workspace is scoped to, and how its shared volume and
// its durable artifacts relate.
//
// The shared-scratch-space framing is the one an agent is least likely to arrive at on its
// own, so it is stated at length rather than in passing: the default assumption a model brings
// is that storage it writes to is its own. Sizing a scope of work is the operator's call (see
// DESIGN §2.1), so this explains the concept and leaves the boundary to whoever drew it.
//
// The mount path is interpolated from the constant rather than written out, so this does not
// become a second definition of it (see DESIGN §4.4).
const mcpServerInstructions = `cairn stores durable files, called artifacts, for agent workflows
in workspaces. A workspace groups two things: a set of artifacts, and optionally one persistent
volume that tool containers mount at ` + models.WorkspaceMountPath + `.

A workspace is the shared scratch space for a scope of work - not for one tool call, and not
for one agent. A scope of work is whatever unit of activity an operator chose to give a single
workspace to: a chat session, an agent together with its sub-agents, an entire project. Every
participant in that scope mounts the same volume: your tool calls, your sub-agents' tool calls,
and any interactive shell session running alongside you.

Treat that volume the way you would treat a developer's project directory rather than private
storage. Anything you write there can be read, modified, or deleted by every other participant,
and anything you find there may have been put there by one of them. That is intended, not a
hazard to work around - a file one tool writes is meant to be picked up, rewritten, or thrown
away by the next. Two things follow: do not assume a path you wrote earlier is still untouched,
and do not remove or overwrite what you did not create unless the task calls for it.

The volume is scratch space and is discarded when its scope of work ends. Artifacts are durable
and outlive it. So the usual shape of a workflow is: a tool writes a file into the volume, you
upload_artifact it to make it durable, and a later step - possibly against a different volume -
download_artifact's it back.

Plain text artifacts can also be worked on inside the tool call itself: read_text_artifact
returns their lines, create_text_artifact stores text you send as a new artifact, and
edit_text_artifact changes part of one by exact-text replacement, the way you edit a file.
These three never touch the volume, so they work whatever its state. A copy of an artifact
already downloaded into the volume is a separate file and does not change when the artifact
does; download it again to see the new content.

You cannot create a workspace or provision its volume; an operator does that. Use get_workspace
or list_workspaces to find one and check that its volume state is READY, which every tool that
moves a file between the volume and an artifact requires.

Workspaces and artifacts are addressed by name, never by ID. An artifact's name is unique within
its workspace.`

// MCPHandler MCP request handler
type MCPHandler struct {
	goutils.MCPHandler

	validator *validator.Validate

	// workspaceMgr workspace manager
	workspaceMgr workspace.Manager

	// artifactMgr artifact manager
	artifactMgr artifact.Manager

	// artifactOperator runs more complex
	artifactOperator artifact.Operator
}

/*
NewMCPHandler new MCP request API handler

	@param appName string - the per-deployment application name which namespaces this
	    deployment's persistent volumes. A workspace's volume name is derived from it (see
	    DESIGN §2.1).
	@param workspaceMgr workspace.Manager - workspace manager
	@param artifactMgr artifact.Manager - artifact manager
	@param artifactOperator artifact.Operator - artifact operator
	@param logConfig common.HTTPRequestLogging - handler log settings
	@returns new handler
*/
func NewMCPHandler(
	appName string,
	workspaceMgr workspace.Manager,
	artifactMgr artifact.Manager,
	artifactOperator artifact.Operator,
	logConfig models.HTTPRequestLogging,
) (MCPHandler, error) {
	validate := validator.New()
	if err := models.RegisterWithValidator(validate); err != nil {
		return MCPHandler{}, goutils.NewRuntimeError(
			"failed to install custom validation macros", err, true,
		)
	}

	// The application name lands in every workspace's volume name, so hold it to the same
	// charset the volume name must satisfy.
	if err := validate.Var(appName, "required,valid_name"); err != nil {
		return MCPHandler{}, goutils.NewValidationError(
			fmt.Sprintf("application name '%s' is not valid", appName), err, true,
		)
	}

	handler := MCPHandler{
		MCPHandler: goutils.MCPHandler{
			Component: goutils.Component{
				LogTags: log.Fields{
					"package":   "cairn",
					"module":    "api",
					"component": "mcp-handler",
					"instance":  appName,
				},
				LogTagModifiers: []goutils.LogMetadataModifier{
					goutils.ModifyLogMetadataByRestRequestParam,
					goutils.ModifyLogMetadataByMCPRequestParam,
				},
			},
			LogLevel:        logConfig.LogLevel,
			EnumTypeSchemas: map[reflect.Type]*jsonschema.Schema{},
		},
		validator:        validate,
		workspaceMgr:     workspaceMgr,
		artifactMgr:      artifactMgr,
		artifactOperator: artifactOperator,
	}

	// Register the enumerated schema for every models ENUM that can appear in a tool's input, so
	// the tool schemas advertise the permitted members rather than a bare string. Keep this list
	// in lock-step with the const blocks in the models package.
	goutils.MCPInstallEnumSchema[models.ArtifactStateENUM](&handler.MCPHandler)
	goutils.MCPInstallEnumSchema[models.SystemEventTypeENUM](&handler.MCPHandler)
	goutils.MCPInstallEnumSchema[models.TextLineEndingENUM](&handler.MCPHandler)
	goutils.MCPInstallEnumSchema[models.WorkspaceVolumeStateENUM](&handler.MCPHandler)
	goutils.MCPInstallEnumSchema[models.WorkspaceVolumeTypeENUM](&handler.MCPHandler)

	return handler, nil
}

/*
RegisterTools register every agent facing tool against the given MCP server.

	@param server *mcp.Server - target MCP server
	@returns error if any tool could not be registered
*/
func (h MCPHandler) RegisterTools(server *mcp.Server) error {
	registrations := []func(*mcp.Server) error{
		// Workspace tools
		h.registerGetWorkspaceTool,
		h.registerListWorkspacesTool,
		// Artifact tools
		h.registerListArtifactsTool,
		h.registerDownloadArtifactTool,
		h.registerUploadArtifactTool,
		h.registerUpdateArtifactTool,
		h.registerDeleteArtifactTool,
		h.registerRenameArtifactTool,
		// Text artifact tools
		h.registerReadTextArtifactTool,
		h.registerCreateTextArtifactTool,
		h.registerEditTextArtifactTool,
	}
	for _, register := range registrations {
		if err := register(server); err != nil {
			return err
		}
	}
	return nil
}

// ======================================================================================
// Name resolution
//
// The agent facing surface is name addressed and everything below it is ID addressed, so
// every tool that names an entry turns that name into an entry exactly once, here (see
// DESIGN §3).

/*
resolveWorkspace resolve a workspace name to its entry.

The lookup doubles as the "parent workspace must exist" precondition every artifact write is
gated on: an unknown name fails here, before any object store or container work (see DESIGN
§7.5).

	@param ctxt context.Context - execution context
	@param name string - the workspace name to resolve
	@returns the workspace entry
*/
func (h MCPHandler) resolveWorkspace(
	ctxt context.Context, name string,
) (models.Workspace, error) {
	// The mount count the manager reports alongside the entry is dropped: an agent never tears
	// a volume down, so it is an operator concern (see DESIGN §7.2).
	entry, _, err := h.workspaceMgr.GetWorkspaceByName(ctxt, name, nil)
	if err != nil {
		return models.Workspace{}, err
	}
	return entry, nil
}

/*
resolveArtifact resolve a workspace name and an artifact name to both entries.

The workspace is resolved first, so an unknown workspace is reported as such rather than as a
missing artifact within one that does not exist. For the tools that act on an existing
artifact this is also the pre-sidecar existence check DESIGN §7.3 calls for.

	@param ctxt context.Context - execution context
	@param workspaceName string - the parent workspace name
	@param artifactName string - the artifact name within that workspace
	@returns the workspace entry
	@returns the artifact entry
*/
func (h MCPHandler) resolveArtifact(
	ctxt context.Context, workspaceName string, artifactName string,
) (models.Workspace, models.Artifact, error) {
	workspaceEntry, err := h.resolveWorkspace(ctxt, workspaceName)
	if err != nil {
		return models.Workspace{}, models.Artifact{}, err
	}

	artifactEntry, err := h.artifactMgr.GetArtifactByName(ctxt, workspaceEntry, artifactName, nil)
	if err != nil {
		return models.Workspace{}, models.Artifact{}, err
	}

	return workspaceEntry, artifactEntry, nil
}
