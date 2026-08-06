package api_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/alwitt/cairn/api"
	"github.com/alwitt/cairn/db"
	mockartifact "github.com/alwitt/cairn/mocks/artifact"
	mockworkspace "github.com/alwitt/cairn/mocks/workspace"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// mcpToolNames every tool DESIGN §7.2 specifies. A tool an agent can see is a tool it will
// try, so the registration test holds the advertised set to exactly this.
var mcpToolNames = []string{
	"get_workspace",
	"list_workspaces",
	"list_artifacts",
	"download_artifact",
	"upload_artifact",
	"update_artifact",
	"delete_artifact",
	"rename_artifact",
}

// mcpToolMocks the mocked collaborators an MCP handler under test drives. Every one fails the
// test on an unarranged call, which is how several cases assert that a tool stopped before it
// reached the next layer.
type mcpToolMocks struct {
	workspaces *mockworkspace.Manager
	artifacts  *mockartifact.Manager
	operator   *mockartifact.Operator
}

// buildMCPToolSession stand up the handler, register every tool against a real MCP server, and
// connect a client to it over an in-memory transport pair.
//
// The tools are driven through the actual protocol rather than by calling their handler
// closures: the closures are unexported, and more to the point a direct call would skip the
// generated input schema, which is the part most likely to be wrong.
func buildMCPToolSession(t *testing.T) (*mcp.ClientSession, mcpToolMocks) {
	assert := assert.New(t)
	ctxt := context.Background()

	mocks := mcpToolMocks{
		workspaces: mockworkspace.NewManager(t),
		artifacts:  mockartifact.NewManager(t),
		operator:   mockartifact.NewOperator(t),
	}

	handler, err := api.NewMCPHandler(
		unitTestAppName,
		mocks.workspaces,
		mocks.artifacts,
		mocks.operator,
		models.HTTPRequestLogging{
			LogLevel:        goutils.HTTPLogLevelWARN,
			HealthLogLevel:  goutils.HTTPLogLevelWARN,
			RequestIDHeader: "unit-test",
			DoNotLogHeaders: []string{},
		},
	)
	assert.Nil(err)

	server := mcp.NewServer(&mcp.Implementation{Name: "cairn", Version: "unit-test"}, nil)
	assert.Nil(handler.RegisterTools(server))

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctxt, serverTransport, nil)
	assert.Nil(err)

	client := mcp.NewClient(&mcp.Implementation{Name: "unit-test-agent", Version: "unit-test"}, nil)
	clientSession, err := client.Connect(ctxt, clientTransport, nil)
	assert.Nil(err)

	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Wait()
	})

	return clientSession, mocks
}

// callMCPTool invoke one tool by name. A transport level failure fails the test outright; a
// tool level refusal comes back on the result as IsError, which is what the cases assert on.
func callMCPTool(
	t *testing.T, session *mcp.ClientSession, name string, args map[string]interface{},
) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(
		context.Background(), &mcp.CallToolParams{Name: name, Arguments: args},
	)
	assert.Nil(t, err)
	assert.NotNil(t, result)
	return result
}

// decodeMCPStructured decode a tool result's structured content into the response type the tool
// declares. It arrives as generic JSON over the wire, so this is the round trip a real MCP
// client performs to get a typed value back.
func decodeMCPStructured(t *testing.T, result *mcp.CallToolResult, out interface{}) {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	assert.Nil(t, err)
	assert.Nil(t, json.Unmarshal(raw, out))
}

// mcpResultText read the single text block an action tool's confirmation is carried in.
func mcpResultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	assert.Len(t, result.Content, 1)
	text, ok := result.Content[0].(*mcp.TextContent)
	assert.True(t, ok)
	return text.Text
}

// TestMCPRegisterTools validates what the agent facing catalog advertises. Everything DESIGN
// §7.2 specifies must be there, nothing else may be, and each entry needs the description and
// input schema an agent forms its call from.
func TestMCPRegisterTools(t *testing.T) {
	assert := assert.New(t)

	session, _ := buildMCPToolSession(t)

	listed, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	assert.Nil(err)

	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		assert.NotEmpty(tool.Description, "tool '%s' has no description", tool.Name)
		assert.NotNil(tool.InputSchema, "tool '%s' has no input schema", tool.Name)
	}
	assert.ElementsMatch(mcpToolNames, names)
}

// TestMCPGetWorkspaceTool validates the get_workspace tool.
func TestMCPGetWorkspaceTool(t *testing.T) {
	// Case 1: the name is resolved through the manager and answered with the narrow
	// projection.
	t.Run("resolves a workspace by name", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateReady)

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, workspace.Name, nil).
			Return(workspace, 3, nil).
			Once()

		result := callMCPTool(
			t, session, "get_workspace", map[string]interface{}{"workspace_name": workspace.Name},
		)
		assert.False(result.IsError)

		var decoded api.MCPGetWorkspaceResp
		decodeMCPStructured(t, result, &decoded)
		assert.Equal(workspace.Name, decoded.Workspace.Name)
		assert.Equal(models.WorkspaceVolumeStateReady, decoded.Workspace.VolumeState)
		assert.Equal(workspace.CreatedAt, decoded.Workspace.CreatedAt)
	})

	// Case 2: the mount count the manager returns alongside the entry is dropped. The agent
	// cannot act on it, and DESIGN §7.2 no longer reports it.
	t.Run("drops the volume mount count", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateReady)

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, workspace.Name, nil).
			Return(workspace, 7, nil).
			Once()

		result := callMCPTool(
			t, session, "get_workspace", map[string]interface{}{"workspace_name": workspace.Name},
		)
		assert.False(result.IsError)

		raw, err := json.Marshal(result.StructuredContent)
		assert.Nil(err)
		assert.NotContains(string(raw), "mount_count")
		assert.NotContains(string(raw), "7")
	})

	// Case 3: an unknown workspace is a tool level refusal carrying the manager's reason, not a
	// transport failure.
	t.Run("refuses an unknown workspace", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, "no-such-ws", nil).
			Return(models.Workspace{}, 0, dbFailure(unknownWorkspace("no-such-ws"))).
			Once()

		result := callMCPTool(
			t, session, "get_workspace", map[string]interface{}{"workspace_name": "no-such-ws"},
		)
		assert.True(result.IsError)
	})

	// Case 4: a name outside the permitted charset never reaches the manager. `valid_name` is
	// not expressible in the generated input schema, so this exercises the handler's own
	// validate pass - and the mocks failing on an unarranged call are what assert it stopped.
	t.Run("rejects a malformed workspace name", func(t *testing.T) {
		assert := assert.New(t)

		session, _ := buildMCPToolSession(t)

		result := callMCPTool(
			t, session, "get_workspace", map[string]interface{}{"workspace_name": "has spaces"},
		)
		assert.True(result.IsError)
		// The handler's own message, not the schema validator's - the schema accepts any
		// string here, so a refusal from it would mean something else went wrong.
		assert.Contains(mcpResultText(t, result), "get_workspace parameters are not valid")
	})
}

// TestMCPListWorkspacesTool validates the list_workspaces tool.
func TestMCPListWorkspacesTool(t *testing.T) {
	// Case 1: the listing options reach the persistence filter, and every entry comes back
	// projected.
	t.Run("translates the listing options", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspaces := []models.Workspace{
			mcpSampleWorkspace("first-ws", models.WorkspaceVolumeStateReady),
			mcpSampleWorkspace("second-ws", models.WorkspaceVolumeStateReady),
		}

		mocks.workspaces.EXPECT().
			ListWorkspaces(mock.Anything, mock.Anything, nil).
			RunAndReturn(func(
				_ context.Context, filters db.WorkspaceQueryFilter, _ db.Database,
			) ([]models.Workspace, error) {
				assert.Equal([]string{"first-ws", "second-ws"}, filters.TargetNames)
				assert.Equal(
					[]models.WorkspaceVolumeStateENUM{models.WorkspaceVolumeStateReady},
					filters.VolumeStates,
				)
				assert.Equal(10, *filters.Limit)
				assert.Equal(5, *filters.Offset)
				return workspaces, nil
			}).
			Once()

		result := callMCPTool(t, session, "list_workspaces", map[string]interface{}{
			"workspace_names": []string{"first-ws", "second-ws"},
			"volume_states":   []string{string(models.WorkspaceVolumeStateReady)},
			"limit":           10,
			"offset":          5,
		})
		assert.False(result.IsError)

		var decoded api.MCPListWorkspacesResp
		decodeMCPStructured(t, result, &decoded)
		assert.Len(decoded.Workspaces, 2)
		assert.Equal("first-ws", decoded.Workspaces[0].Name)
		assert.Equal("second-ws", decoded.Workspaces[1].Name)
	})

	// Case 2: an unfiltered call selects no volume state at all - unlike the artifact listing,
	// there is no state a workspace listing keeps back.
	t.Run("applies no volume state default", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)

		mocks.workspaces.EXPECT().
			ListWorkspaces(mock.Anything, mock.Anything, nil).
			RunAndReturn(func(
				_ context.Context, filters db.WorkspaceQueryFilter, _ db.Database,
			) ([]models.Workspace, error) {
				assert.Empty(filters.VolumeStates)
				assert.Empty(filters.TargetNames)
				return nil, nil
			}).
			Once()

		result := callMCPTool(t, session, "list_workspaces", map[string]interface{}{})
		assert.False(result.IsError)

		var decoded api.MCPListWorkspacesResp
		decodeMCPStructured(t, result, &decoded)
		assert.Empty(decoded.Workspaces)
	})

	// Case 3: a persistence failure is a tool level refusal.
	t.Run("refuses on a persistence failure", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)

		mocks.workspaces.EXPECT().
			ListWorkspaces(mock.Anything, mock.Anything, nil).
			Return(nil, dbFailure(goutils.NewSQLError("simulated", nil, true))).
			Once()

		result := callMCPTool(t, session, "list_workspaces", map[string]interface{}{})
		assert.True(result.IsError)
	})
}

// TestMCPListArtifactsTool validates the list_artifacts tool.
func TestMCPListArtifactsTool(t *testing.T) {
	// Case 1: the workspace is resolved first, then its artifacts listed with the translated
	// filter.
	t.Run("resolves the workspace then lists", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateReady)
		description := "artifact description"
		artifacts := []models.Artifact{
			mcpSampleArtifact(workspace.ID, "report-tar", &description),
		}

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, workspace.Name, nil).
			Return(workspace, 0, nil).
			Once()
		mocks.artifacts.EXPECT().
			ListWorkspaceArtifacts(mock.Anything, workspace, mock.Anything, nil).
			RunAndReturn(func(
				_ context.Context,
				_ models.Workspace,
				filters db.ArtifactQueryFilter,
				_ db.Database,
			) ([]models.Artifact, error) {
				assert.Equal([]string{"report-tar"}, filters.TargetNames)
				assert.Equal(20, *filters.Limit)
				return artifacts, nil
			}).
			Once()

		result := callMCPTool(t, session, "list_artifacts", map[string]interface{}{
			"workspace_name": workspace.Name,
			"artifact_names": []string{"report-tar"},
			"limit":          20,
		})
		assert.False(result.IsError)

		var decoded api.MCPListArtifactsResp
		decodeMCPStructured(t, result, &decoded)
		assert.Len(decoded.Artifacts, 1)
		assert.Equal("report-tar", decoded.Artifacts[0].Name)
		assert.Equal(&description, decoded.Artifacts[0].Description)
		assert.Equal(models.ArtifactStateRecorded, decoded.Artifacts[0].State)
	})

	// Case 2: omitting the state selection asks the persistence layer for RECORDED only. An
	// empty selection there means every state, which would show an agent the quarantined
	// entries it never asked about (see DESIGN §7.1).
	t.Run("defaults the state selection to RECORDED", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateReady)

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, workspace.Name, nil).
			Return(workspace, 0, nil).
			Once()
		mocks.artifacts.EXPECT().
			ListWorkspaceArtifacts(mock.Anything, workspace, mock.Anything, nil).
			RunAndReturn(func(
				_ context.Context,
				_ models.Workspace,
				filters db.ArtifactQueryFilter,
				_ db.Database,
			) ([]models.Artifact, error) {
				assert.Equal(
					[]models.ArtifactStateENUM{models.ArtifactStateRecorded}, filters.ArtifactStates,
				)
				return nil, nil
			}).
			Once()

		result := callMCPTool(t, session, "list_artifacts", map[string]interface{}{
			"workspace_name": workspace.Name,
		})
		assert.False(result.IsError)
	})

	// Case 3: an explicit state selection is passed through, which is how a caller asks for the
	// quarantined entries while triaging.
	t.Run("passes an explicit state selection through", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateReady)

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, workspace.Name, nil).
			Return(workspace, 0, nil).
			Once()
		mocks.artifacts.EXPECT().
			ListWorkspaceArtifacts(mock.Anything, workspace, mock.Anything, nil).
			RunAndReturn(func(
				_ context.Context,
				_ models.Workspace,
				filters db.ArtifactQueryFilter,
				_ db.Database,
			) ([]models.Artifact, error) {
				assert.Equal([]models.ArtifactStateENUM{
					models.ArtifactStateRecorded, models.ArtifactStateMissingObject,
				}, filters.ArtifactStates)
				return nil, nil
			}).
			Once()

		result := callMCPTool(t, session, "list_artifacts", map[string]interface{}{
			"workspace_name": workspace.Name,
			"states": []string{
				string(models.ArtifactStateRecorded), string(models.ArtifactStateMissingObject),
			},
		})
		assert.False(result.IsError)
	})

	// Case 4: an unknown workspace is answered before the artifact manager is touched. The
	// artifact mock failing on an unarranged call is the assertion.
	t.Run("refuses an unknown workspace before listing", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, "no-such-ws", nil).
			Return(models.Workspace{}, 0, dbFailure(unknownWorkspace("no-such-ws"))).
			Once()

		result := callMCPTool(t, session, "list_artifacts", map[string]interface{}{
			"workspace_name": "no-such-ws",
		})
		assert.True(result.IsError)
	})
}

// TestMCPDownloadArtifactTool validates the download_artifact tool.
func TestMCPDownloadArtifactTool(t *testing.T) {
	// Case 1: workspace then artifact are resolved, and both entries plus the target path reach
	// the operator.
	t.Run("resolves both entries then downloads", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateReady)
		entry := mcpSampleArtifact(workspace.ID, "report-tar", nil)
		targetPath := models.WorkspaceMountPath + "/out/report.tar"

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, workspace.Name, nil).
			Return(workspace, 0, nil).
			Once()
		mocks.artifacts.EXPECT().
			GetArtifactByName(mock.Anything, workspace, entry.Name, nil).
			Return(entry, nil).
			Once()
		mocks.operator.EXPECT().
			DownloadArtifact(mock.Anything, workspace, entry, targetPath).
			Return(nil).
			Once()

		result := callMCPTool(t, session, "download_artifact", map[string]interface{}{
			"workspace_name": workspace.Name,
			"artifact_name":  entry.Name,
			"target_path":    targetPath,
		})
		assert.False(result.IsError)

		confirmation := mcpResultText(t, result)
		assert.Contains(confirmation, entry.Name)
		assert.Contains(confirmation, workspace.Name)
		assert.Contains(confirmation, targetPath)
	})

	// Case 2: an unknown artifact is answered before the operator runs, so a bad name costs no
	// container.
	t.Run("refuses an unknown artifact before the operator", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateReady)

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, workspace.Name, nil).
			Return(workspace, 0, nil).
			Once()
		mocks.artifacts.EXPECT().
			GetArtifactByName(mock.Anything, workspace, "no-such-artifact", nil).
			Return(models.Artifact{}, artifactDBFailure(unknownArtifact("no-such-artifact"))).
			Once()

		result := callMCPTool(t, session, "download_artifact", map[string]interface{}{
			"workspace_name": workspace.Name,
			"artifact_name":  "no-such-artifact",
			"target_path":    models.WorkspaceMountPath + "/out/report.tar",
		})
		assert.True(result.IsError)
	})

	// Case 3: the operator owns the path and volume preconditions, so its refusal - a path
	// outside the mount, a workspace with no runtime volume - is what the agent is told.
	t.Run("surfaces an operator refusal", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateNone)
		entry := mcpSampleArtifact(workspace.ID, "report-tar", nil)

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, workspace.Name, nil).
			Return(workspace, 0, nil).
			Once()
		mocks.artifacts.EXPECT().
			GetArtifactByName(mock.Anything, workspace, entry.Name, nil).
			Return(entry, nil).
			Once()
		mocks.operator.EXPECT().
			DownloadArtifact(mock.Anything, workspace, entry, mock.Anything).
			Return(artifactOpFailure(goutils.NewBadInputError(
				"workspace has no runtime volume", nil, true,
			))).
			Once()

		result := callMCPTool(t, session, "download_artifact", map[string]interface{}{
			"workspace_name": workspace.Name,
			"artifact_name":  entry.Name,
			"target_path":    models.WorkspaceMountPath + "/out/report.tar",
		})
		assert.True(result.IsError)
	})
}

// TestMCPUploadArtifactTool validates the upload_artifact tool.
func TestMCPUploadArtifactTool(t *testing.T) {
	// Case 1: the workspace is resolved and the operator is handed the source path, the new
	// name, and the description. No artifact lookup happens - the name is meant to be free, and
	// the operator pre-checks that itself.
	t.Run("resolves the workspace then uploads", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateReady)
		description := "collected scan output"
		sourcePath := models.WorkspaceMountPath + "/work/scan.json"

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, workspace.Name, nil).
			Return(workspace, 0, nil).
			Once()
		mocks.operator.EXPECT().
			UploadArtifact(mock.Anything, workspace, sourcePath, "scan-json", &description, nil).
			Return(mcpSampleArtifact(workspace.ID, "scan-json", &description), nil).
			Once()

		result := callMCPTool(t, session, "upload_artifact", map[string]interface{}{
			"workspace_name": workspace.Name,
			"source_path":    sourcePath,
			"artifact_name":  "scan-json",
			"description":    description,
		})
		assert.False(result.IsError)

		confirmation := mcpResultText(t, result)
		assert.Contains(confirmation, "scan-json")
		assert.Contains(confirmation, workspace.Name)
		assert.Contains(confirmation, sourcePath)
	})

	// Case 2: the description is optional, and its absence reaches the operator as nil rather
	// than as an empty string.
	t.Run("carries an absent description through as nil", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateReady)
		sourcePath := models.WorkspaceMountPath + "/work/scan.json"

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, workspace.Name, nil).
			Return(workspace, 0, nil).
			Once()
		mocks.operator.EXPECT().
			UploadArtifact(
				mock.Anything, workspace, sourcePath, "scan-json", (*string)(nil), nil,
			).
			Return(mcpSampleArtifact(workspace.ID, "scan-json", nil), nil).
			Once()

		result := callMCPTool(t, session, "upload_artifact", map[string]interface{}{
			"workspace_name": workspace.Name,
			"source_path":    sourcePath,
			"artifact_name":  "scan-json",
		})
		assert.False(result.IsError)
	})

	// Case 3: a name outside the permitted charset never reaches the operator, which is the
	// whole point of validating here - the schema cannot express the charset, and the database
	// would only catch it after both containers had already run.
	t.Run("rejects a malformed artifact name before the operator", func(t *testing.T) {
		assert := assert.New(t)

		session, _ := buildMCPToolSession(t)

		result := callMCPTool(t, session, "upload_artifact", map[string]interface{}{
			"workspace_name": "unit-test-ws",
			"source_path":    models.WorkspaceMountPath + "/work/scan.json",
			"artifact_name":  "not a valid name",
		})
		assert.True(result.IsError)
		assert.Contains(mcpResultText(t, result), "upload_artifact parameters are not valid")
	})

	// Case 4: the operator's refusal of a taken name is what the agent is told.
	t.Run("surfaces an operator refusal", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateReady)

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, workspace.Name, nil).
			Return(workspace, 0, nil).
			Once()
		mocks.operator.EXPECT().
			UploadArtifact(
				mock.Anything, workspace, mock.Anything, "scan-json", mock.Anything, nil,
			).
			Return(models.Artifact{}, artifactOpFailure(goutils.NewBadInputError(
				"artifact name is already taken", nil, true,
			))).
			Once()

		result := callMCPTool(t, session, "upload_artifact", map[string]interface{}{
			"workspace_name": workspace.Name,
			"source_path":    models.WorkspaceMountPath + "/work/scan.json",
			"artifact_name":  "scan-json",
		})
		assert.True(result.IsError)
	})
}

// TestMCPUpdateArtifactTool validates the update_artifact tool.
func TestMCPUpdateArtifactTool(t *testing.T) {
	// Case 1: unlike upload, the artifact is resolved first - update replaces an existing
	// entry, and resolving it is the pre-container check that it exists (see DESIGN §7.3).
	t.Run("resolves both entries then updates", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateReady)
		entry := mcpSampleArtifact(workspace.ID, "scan-json", nil)
		sourcePath := models.WorkspaceMountPath + "/work/scan.json"

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, workspace.Name, nil).
			Return(workspace, 0, nil).
			Once()
		mocks.artifacts.EXPECT().
			GetArtifactByName(mock.Anything, workspace, entry.Name, nil).
			Return(entry, nil).
			Once()
		mocks.operator.EXPECT().
			UpdateArtifact(mock.Anything, workspace, entry, sourcePath, nil).
			Return(entry, nil).
			Once()

		result := callMCPTool(t, session, "update_artifact", map[string]interface{}{
			"workspace_name": workspace.Name,
			"artifact_name":  entry.Name,
			"source_path":    sourcePath,
		})
		assert.False(result.IsError)

		confirmation := mcpResultText(t, result)
		assert.Contains(confirmation, entry.Name)
		assert.Contains(confirmation, sourcePath)
	})

	// Case 2: an unknown artifact is refused before the operator, so the two containers are
	// never spent on a target that does not exist.
	t.Run("refuses an unknown artifact before the operator", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateReady)

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, workspace.Name, nil).
			Return(workspace, 0, nil).
			Once()
		mocks.artifacts.EXPECT().
			GetArtifactByName(mock.Anything, workspace, "no-such-artifact", nil).
			Return(models.Artifact{}, artifactDBFailure(unknownArtifact("no-such-artifact"))).
			Once()

		result := callMCPTool(t, session, "update_artifact", map[string]interface{}{
			"workspace_name": workspace.Name,
			"artifact_name":  "no-such-artifact",
			"source_path":    models.WorkspaceMountPath + "/work/scan.json",
		})
		assert.True(result.IsError)
	})
}

// TestMCPDeleteArtifactTool validates the delete_artifact tool.
func TestMCPDeleteArtifactTool(t *testing.T) {
	// Case 1: the name is resolved to an entry and the deletion is issued by ID - the name only
	// ever exists at this boundary (see DESIGN §3).
	t.Run("resolves the artifact then deletes by ID", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateReady)
		entry := mcpSampleArtifact(workspace.ID, "report-tar", nil)

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, workspace.Name, nil).
			Return(workspace, 0, nil).
			Once()
		mocks.artifacts.EXPECT().
			GetArtifactByName(mock.Anything, workspace, entry.Name, nil).
			Return(entry, nil).
			Once()
		mocks.artifacts.EXPECT().
			DeleteArtifact(mock.Anything, entry.ID, nil).
			Return(nil).
			Once()

		result := callMCPTool(t, session, "delete_artifact", map[string]interface{}{
			"workspace_name": workspace.Name,
			"artifact_name":  entry.Name,
		})
		assert.False(result.IsError)

		confirmation := mcpResultText(t, result)
		assert.Contains(confirmation, entry.Name)
		assert.Contains(confirmation, workspace.Name)
	})

	// Case 2: an unknown artifact is refused before the delete is issued.
	t.Run("refuses an unknown artifact", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateReady)

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, workspace.Name, nil).
			Return(workspace, 0, nil).
			Once()
		mocks.artifacts.EXPECT().
			GetArtifactByName(mock.Anything, workspace, "no-such-artifact", nil).
			Return(models.Artifact{}, artifactDBFailure(unknownArtifact("no-such-artifact"))).
			Once()

		result := callMCPTool(t, session, "delete_artifact", map[string]interface{}{
			"workspace_name": workspace.Name,
			"artifact_name":  "no-such-artifact",
		})
		assert.True(result.IsError)
	})
}

// TestMCPRenameArtifactTool validates the rename_artifact tool.
func TestMCPRenameArtifactTool(t *testing.T) {
	// Case 1: the current name is resolved to an entry and the rename is issued by ID.
	t.Run("resolves the artifact then renames by ID", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateReady)
		entry := mcpSampleArtifact(workspace.ID, "report-tar", nil)

		renamed := entry
		renamed.Name = "final-report-tar"

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, workspace.Name, nil).
			Return(workspace, 0, nil).
			Once()
		mocks.artifacts.EXPECT().
			GetArtifactByName(mock.Anything, workspace, entry.Name, nil).
			Return(entry, nil).
			Once()
		mocks.artifacts.EXPECT().
			RenameArtifact(mock.Anything, entry.ID, "final-report-tar", nil).
			Return(renamed, nil).
			Once()

		result := callMCPTool(t, session, "rename_artifact", map[string]interface{}{
			"workspace_name": workspace.Name,
			"artifact_name":  entry.Name,
			"new_name":       "final-report-tar",
		})
		assert.False(result.IsError)

		confirmation := mcpResultText(t, result)
		assert.Contains(confirmation, entry.Name)
		assert.Contains(confirmation, "final-report-tar")
	})

	// Case 2: a malformed new name is rejected before either manager call. The charset is not
	// expressible in the input schema, so only the handler's validate pass catches it.
	t.Run("rejects a malformed new name", func(t *testing.T) {
		assert := assert.New(t)

		session, _ := buildMCPToolSession(t)

		result := callMCPTool(t, session, "rename_artifact", map[string]interface{}{
			"workspace_name": "unit-test-ws",
			"artifact_name":  "report-tar",
			"new_name":       "has/slash",
		})
		assert.True(result.IsError)
		assert.Contains(mcpResultText(t, result), "rename_artifact parameters are not valid")
	})

	// Case 3: names are unique within a workspace and that constraint is the real guard, so a
	// collision surfaces from the rename rather than from a pre-check.
	t.Run("surfaces a name collision from the rename", func(t *testing.T) {
		assert := assert.New(t)

		session, mocks := buildMCPToolSession(t)
		workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateReady)
		entry := mcpSampleArtifact(workspace.ID, "report-tar", nil)

		mocks.workspaces.EXPECT().
			GetWorkspaceByName(mock.Anything, workspace.Name, nil).
			Return(workspace, 0, nil).
			Once()
		mocks.artifacts.EXPECT().
			GetArtifactByName(mock.Anything, workspace, entry.Name, nil).
			Return(entry, nil).
			Once()
		mocks.artifacts.EXPECT().
			RenameArtifact(mock.Anything, entry.ID, "taken-name", nil).
			Return(models.Artifact{}, artifactDBFailure(
				goutils.NewSQLError("unique constraint violated", nil, true),
			)).
			Once()

		result := callMCPTool(t, session, "rename_artifact", map[string]interface{}{
			"workspace_name": workspace.Name,
			"artifact_name":  entry.Name,
			"new_name":       "taken-name",
		})
		assert.True(result.IsError)
	})
}
