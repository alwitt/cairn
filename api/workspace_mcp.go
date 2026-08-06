// Package api - application REST API
package api //revive:disable-line:var-naming

import (
	"context"

	"github.com/alwitt/goutils"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerGetWorkspaceTool register the get_workspace tool.
//
// The lookup an agent runs before anything else: it confirms the workspace it was told to use
// exists, and reports whether the volume is actually there to mount (see DESIGN §7.2).
func (h MCPHandler) registerGetWorkspaceTool(server *mcp.Server) error {
	toolName := "get_workspace"
	toolDescription :=
		"Fetch one workspace by name, reporting whether its persistent volume is ready to " +
			"mount. A workspace whose volume state is not READY cannot be used to move artifacts " +
			"to or from a volume, and you cannot provision one yourself - an operator must do it."

	return goutils.MCPAddTool(
		&h.MCPHandler,
		server,
		&mcp.Tool{Name: toolName, Description: toolDescription},
		func(
			ctxt context.Context, _ *mcp.CallToolRequest, in MCPGetWorkspaceParams,
		) (*mcp.CallToolResult, MCPGetWorkspaceResp, error) {
			// The tool call arguments were already checked against the generated input schema,
			// which cannot express the name charset - so the parameters are validated here too
			// (see the tag conventions in mcp.go).
			if err := h.validator.Struct(&in); err != nil {
				return nil, MCPGetWorkspaceResp{}, goutils.NewValidationError(
					"get_workspace parameters are not valid", err, true,
				)
			}

			workspaceEntry, err := h.resolveWorkspace(ctxt, in.WorkspaceName)
			if err != nil {
				return nil, MCPGetWorkspaceResp{}, goutils.NewRuntimeError(
					"Failed to fetch workspace '"+in.WorkspaceName+"'", err, true,
				)
			}

			return nil, NewMCPGetWorkspaceResp(workspaceEntry), nil
		},
	)
}

// registerListWorkspacesTool register the list_workspaces tool.
func (h MCPHandler) registerListWorkspacesTool(server *mcp.Server) error {
	toolName := "list_workspaces"
	toolDescription :=
		"List the workspaces available to you, optionally filtered by name or by whether " +
			"their persistent volume is ready to mount, with paging. Only a workspace whose " +
			"volume state is READY can be used to move artifacts to or from a volume."

	return goutils.MCPAddTool(
		&h.MCPHandler,
		server,
		&mcp.Tool{Name: toolName, Description: toolDescription},
		func(
			ctxt context.Context, _ *mcp.CallToolRequest, in MCPListWorkspacesParams,
		) (*mcp.CallToolResult, MCPListWorkspacesResp, error) {
			if err := h.validator.Struct(&in); err != nil {
				return nil, MCPListWorkspacesResp{}, goutils.NewValidationError(
					"list_workspaces parameters are not valid", err, true,
				)
			}

			entries, err := h.workspaceMgr.ListWorkspaces(ctxt, in.ToQueryFilter(), nil)
			if err != nil {
				return nil, MCPListWorkspacesResp{}, goutils.NewRuntimeError(
					"Failed to list workspaces", err, true,
				)
			}

			return nil, NewMCPListWorkspacesResp(entries), nil
		},
	)
}
