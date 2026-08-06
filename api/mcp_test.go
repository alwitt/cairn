package api_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/alwitt/cairn/api"
	"github.com/alwitt/cairn/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// mcpSampleWorkspace build a workspace entry with every field an agent must NOT be shown
// populated, so a projection test can assert those fields do not reach the response.
func mcpSampleWorkspace(name string, state models.WorkspaceVolumeStateENUM) models.Workspace {
	workspaceID := uuid.NewString()
	description := "workspace description"
	return models.Workspace{
		ID:          workspaceID,
		Name:        name,
		Description: &description,
		VolumeName:  models.WorkspaceVolumeName(unitTestAppName, workspaceID),
		VolumeState: state,
		CreatedAt:   time.Date(2026, time.March, 14, 9, 26, 53, 0, time.UTC),
		UpdatedAt:   time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC),
	}
}

// mcpSampleArtifact build an artifact entry with every field an agent must NOT be shown
// populated, for the same reason as mcpSampleWorkspace. The parent workspace is taken from the
// caller so a fixture actually belongs to the workspace a tool fetched it from.
func mcpSampleArtifact(workspaceID string, name string, description *string) models.Artifact {
	return models.Artifact{
		ID:          uuid.NewString(),
		WorkspaceID: workspaceID,
		Name:        name,
		Description: description,
		ObjectKey:   "store/01JQ/01JR",
		MIMEType:    "application/octet-stream",
		Size:        4096,
		State:       models.ArtifactStateRecorded,
		CreatedAt:   time.Date(2026, time.March, 14, 9, 26, 53, 0, time.UTC),
		UpdatedAt:   time.Date(2026, time.May, 9, 17, 11, 2, 0, time.UTC),
	}
}

// marshalToMap round-trip a value through JSON into a generic map, which is what the tool
// result the agent receives is shaped from. Asserting on the key set here is what pins the
// projection: a field added to the DTO by accident shows up as an unexpected key.
func marshalToMap(t *testing.T, value interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(value)
	assert.Nil(t, err)

	var decoded map[string]interface{}
	assert.Nil(t, json.Unmarshal(raw, &decoded))
	return decoded
}

// TestNewMCPGetWorkspaceResp validates the get_workspace projection. The point of the
// projection is what it leaves behind, so the field mapping and the resulting key set are both
// asserted - the entry ID, the derived volume name, and the description must not reach an
// agent that addresses everything by name (see DESIGN §3).
func TestNewMCPGetWorkspaceResp(t *testing.T) {
	assert := assert.New(t)

	workspace := mcpSampleWorkspace("unit-test-ws", models.WorkspaceVolumeStateReady)

	response := api.NewMCPGetWorkspaceResp(workspace)

	assert.Equal(workspace.Name, response.Workspace.Name)
	assert.Equal(workspace.VolumeState, response.Workspace.VolumeState)
	assert.Equal(workspace.CreatedAt, response.Workspace.CreatedAt)

	// The response carries exactly three workspace fields and nothing else.
	entry, ok := marshalToMap(t, response)["workspace"].(map[string]interface{})
	assert.True(ok)
	assert.Len(entry, 3)
	for _, field := range []string{"name", "volume_state", "created_at"} {
		assert.Contains(entry, field)
	}
	// Specifically not the ID or the volume name: an agent that could read either might try to
	// address by it.
	for _, field := range []string{"id", "volume_name", "description", "volume_metadata"} {
		assert.NotContains(entry, field)
	}
}

// TestNewMCPListWorkspacesResp validates the list_workspaces projection, including the empty
// case - the output schema types the field as an array, so an empty listing has to marshal as
// `[]` rather than as `null`.
func TestNewMCPListWorkspacesResp(t *testing.T) {
	// Case 1: every entry is projected, in the order given.
	t.Run("projects each workspace in order", func(t *testing.T) {
		assert := assert.New(t)

		workspaces := []models.Workspace{
			mcpSampleWorkspace("first-ws", models.WorkspaceVolumeStateReady),
			mcpSampleWorkspace("second-ws", models.WorkspaceVolumeStateNone),
		}

		response := api.NewMCPListWorkspacesResp(workspaces)

		assert.Len(response.Workspaces, 2)
		for idx, entry := range response.Workspaces {
			assert.Equal(workspaces[idx].Name, entry.Name)
			assert.Equal(workspaces[idx].VolumeState, entry.VolumeState)
			assert.Equal(workspaces[idx].CreatedAt, entry.CreatedAt)
		}

		// Same key set as the get projection, and nothing more.
		listing, ok := marshalToMap(t, response)["workspaces"].([]interface{})
		assert.True(ok)
		assert.Len(listing, 2)
		first, ok := listing[0].(map[string]interface{})
		assert.True(ok)
		assert.Len(first, 3)
		for _, field := range []string{"name", "volume_state", "created_at"} {
			assert.Contains(first, field)
		}
	})

	// Case 2: an empty result is an empty array, not null. A nil slice would marshal as `null`
	// and contradict the array type the output schema advertises.
	t.Run("marshals an empty listing as an array", func(t *testing.T) {
		assert := assert.New(t)

		for _, empty := range [][]models.Workspace{nil, {}} {
			raw, err := json.Marshal(api.NewMCPListWorkspacesResp(empty))
			assert.Nil(err)
			assert.JSONEq(`{"workspaces":[]}`, string(raw))
		}
	})
}

// TestNewMCPListArtifactsResp validates the list_artifacts projection. The object key is the
// field that matters most here: it is the object store address of the artifact's content, and
// an agent has no business holding it (see DESIGN §3, §5.1).
func TestNewMCPListArtifactsResp(t *testing.T) {
	// Case 1: every entry is projected, in the order given.
	t.Run("projects each artifact in order", func(t *testing.T) {
		assert := assert.New(t)

		description := "artifact description"
		workspaceID := uuid.NewString()
		artifacts := []models.Artifact{
			mcpSampleArtifact(workspaceID, "first-artifact", &description),
			mcpSampleArtifact(workspaceID, "second-artifact", nil),
		}
		artifacts[1].State = models.ArtifactStateMissingObject

		response := api.NewMCPListArtifactsResp(artifacts)

		assert.Len(response.Artifacts, 2)
		for idx, entry := range response.Artifacts {
			assert.Equal(artifacts[idx].Name, entry.Name)
			assert.Equal(artifacts[idx].Description, entry.Description)
			assert.Equal(artifacts[idx].State, entry.State)
			assert.Equal(artifacts[idx].UpdatedAt, entry.UpdatedAt)
		}

		listing, ok := marshalToMap(t, response)["artifacts"].([]interface{})
		assert.True(ok)
		assert.Len(listing, 2)
		first, ok := listing[0].(map[string]interface{})
		assert.True(ok)
		assert.Len(first, 4)
		for _, field := range []string{"name", "description", "state", "updated_at"} {
			assert.Contains(first, field)
		}
		// Neither the entry ID nor the object key backing its content.
		for _, field := range []string{"id", "workspace_id", "object_key", "size", "mime_type"} {
			assert.NotContains(first, field)
		}
	})

	// Case 2: an artifact with no description carries a null through rather than an empty
	// string - the two mean different things, and only one of them is "the operator wrote a
	// description and it was blank".
	t.Run("carries a missing description through as null", func(t *testing.T) {
		assert := assert.New(t)

		response := api.NewMCPListArtifactsResp(
			[]models.Artifact{mcpSampleArtifact(uuid.NewString(), "no-description", nil)},
		)
		assert.Nil(response.Artifacts[0].Description)

		listing, ok := marshalToMap(t, response)["artifacts"].([]interface{})
		assert.True(ok)
		entry, ok := listing[0].(map[string]interface{})
		assert.True(ok)
		assert.Contains(entry, "description")
		assert.Nil(entry["description"])
	})

	// Case 3: an empty result is an empty array, for the reason given on the workspace listing.
	t.Run("marshals an empty listing as an array", func(t *testing.T) {
		assert := assert.New(t)

		for _, empty := range [][]models.Artifact{nil, {}} {
			raw, err := json.Marshal(api.NewMCPListArtifactsResp(empty))
			assert.Nil(err)
			assert.JSONEq(`{"artifacts":[]}`, string(raw))
		}
	})
}

// TestMCPListArtifactsParamsToQueryFilter validates the artifact listing projection onto the
// persistence query filter. The state default is the load-bearing part: an empty selection
// means "every state" at the persistence layer, so leaving it empty would show an agent the
// quarantined entries it never asked about (see DESIGN §7.1).
func TestMCPListArtifactsParamsToQueryFilter(t *testing.T) {
	// Case 1: no state selection defaults to RECORDED only.
	t.Run("defaults the state selection to RECORDED", func(t *testing.T) {
		assert := assert.New(t)

		filters := api.MCPListArtifactsParams{WorkspaceName: "unit-test-ws"}.ToQueryFilter()

		assert.Equal([]models.ArtifactStateENUM{models.ArtifactStateRecorded}, filters.ArtifactStates)
	})

	// Case 2: an explicit selection is passed through untouched, including one naming both
	// states - which is how a caller asks for everything, there being only two.
	t.Run("passes an explicit state selection through", func(t *testing.T) {
		assert := assert.New(t)

		requested := []models.ArtifactStateENUM{
			models.ArtifactStateRecorded, models.ArtifactStateMissingObject,
		}
		filters := api.MCPListArtifactsParams{
			WorkspaceName: "unit-test-ws", States: requested,
		}.ToQueryFilter()

		assert.Equal(requested, filters.ArtifactStates)
	})

	// Case 3: the remaining listing options are carried across, and the parent workspace is
	// NOT set - the persistence layer fills that in from the workspace ID passed alongside the
	// filter, so setting it here would be a second write of the same thing.
	t.Run("carries the listing options and leaves the workspace unset", func(t *testing.T) {
		assert := assert.New(t)

		limit := 25
		offset := 50
		filters := api.MCPListArtifactsParams{
			WorkspaceName: "unit-test-ws",
			ArtifactNames: []string{"first-artifact", "second-artifact"},
			Limit:         &limit,
			Offset:        &offset,
		}.ToQueryFilter()

		assert.Equal([]string{"first-artifact", "second-artifact"}, filters.TargetNames)
		assert.Equal(&limit, filters.Limit)
		assert.Equal(&offset, filters.Offset)
		assert.Nil(filters.WorkspaceID)
		// Neither of the ID addressed selectors the agent facing params deliberately omit.
		assert.Nil(filters.TargetIDs)
		assert.Nil(filters.ObjectKeys)
	})
}

// TestMCPListWorkspacesParamsToQueryFilter validates the workspace listing projection. Unlike
// the artifact listing it applies no state default: a workspace has no quarantine state to keep
// out of an agent's way, so an unfiltered listing is the right answer to an unfiltered request.
func TestMCPListWorkspacesParamsToQueryFilter(t *testing.T) {
	// Case 1: no volume state selection stays empty, which the persistence layer reads as
	// "every state".
	t.Run("applies no volume state default", func(t *testing.T) {
		assert := assert.New(t)

		filters := api.MCPListWorkspacesParams{}.ToQueryFilter()

		assert.Empty(filters.VolumeStates)
	})

	// Case 2: every listing option is carried across.
	t.Run("carries the listing options", func(t *testing.T) {
		assert := assert.New(t)

		limit := 10
		offset := 5
		requested := []models.WorkspaceVolumeStateENUM{models.WorkspaceVolumeStateReady}
		filters := api.MCPListWorkspacesParams{
			WorkspaceNames: []string{"first-ws", "second-ws"},
			VolumeStates:   requested,
			Limit:          &limit,
			Offset:         &offset,
		}.ToQueryFilter()

		assert.Equal([]string{"first-ws", "second-ws"}, filters.TargetNames)
		assert.Equal(requested, filters.VolumeStates)
		assert.Equal(&limit, filters.Limit)
		assert.Equal(&offset, filters.Offset)
		assert.Nil(filters.TargetIDs)
	})
}
