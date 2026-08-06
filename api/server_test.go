package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alwitt/cairn/api"
	mockartifact "github.com/alwitt/cairn/mocks/artifact"
	mockdb "github.com/alwitt/cairn/mocks/db"
	mockworkspace "github.com/alwitt/cairn/mocks/workspace"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
)

// unitTestAPIPathPrefix the endpoint path prefix the harness server is built with.
const unitTestAPIPathPrefix = "/api"

/*
buildUnitTestAPIServer build the application's HTTP server over mocked collaborators and put it
behind a real listener.

A real listener rather than an httptest.NewRecorder because the MCP SDK's DNS rebinding guard
reads the connection's local address out of the request context, which only a served connection
populates - driving the handler directly would skip the check and let the guard cases pass
without exercising anything.

	@param t *testing.T - the running test
	@param mcpConfig models.MCPAPIConfig - the MCP endpoint settings to build with
	@returns the running test server
*/
func buildUnitTestAPIServer(t *testing.T, mcpConfig models.MCPAPIConfig) *httptest.Server {
	assert := assert.New(t)

	httpServer, err := api.BuildHTTPServer(
		unitTestAppName,
		models.APIServerConfig{
			Server: models.HTTPServerConfig{ListenOn: "127.0.0.1", Port: 0},
			APIs: models.APIConfig{
				Endpoint: models.EndpointConfig{PathPrefix: unitTestAPIPathPrefix},
				RequestLogging: models.HTTPRequestLogging{
					LogLevel:        goutils.HTTPLogLevelWARN,
					HealthLogLevel:  goutils.HTTPLogLevelWARN,
					RequestIDHeader: "unit-test",
					DoNotLogHeaders: []string{},
				},
				MCP: mcpConfig,
			},
		},
		models.ArtifactStorageConfig{
			Bucket:                  "unit-test-bucket",
			UploadPutURLTTLSec:      300,
			DownloadGetURLMaxTTLSec: unitTestGetURLMaxTTLSecs,
			MaxObjectSizeBytes:      1024 * 1024,
		},
		mockdb.NewClient(t),
		mockworkspace.NewManager(t),
		mockartifact.NewManager(t),
		mockartifact.NewOperator(t),
		nil,
	)
	assert.Nil(err)
	assert.NotNil(httpServer)

	testServer := httptest.NewServer(httpServer.Handler)
	t.Cleanup(testServer.Close)

	return testServer
}

// unitTestMCPEndpoint the MCP endpoint's URL on a harness server.
func unitTestMCPEndpoint(testServer *httptest.Server) string {
	return testServer.URL + unitTestAPIPathPrefix + "/v1/mcp"
}

/*
callUnitTestMCPEndpoint issue one raw request at the MCP endpoint under a given `Host` header.

The Host is what the DNS rebinding guard keys on, so it is the parameter the guard cases vary.
An empty one leaves the listener's own loopback address in place.
*/
func callUnitTestMCPEndpoint(
	t *testing.T, testServer *httptest.Server, hostHeader string,
) *http.Response {
	t.Helper()

	request, err := http.NewRequest(
		http.MethodPost,
		unitTestMCPEndpoint(testServer),
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`),
	)
	assert.Nil(t, err)

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if hostHeader != "" {
		request.Host = hostHeader
	}

	response, err := http.DefaultClient.Do(request)
	assert.Nil(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })

	return response
}

// TestBuildHTTPServerMCPEndpoint validates that the MCP endpoint is mounted, announced, and
// gated the way the configuration says.
func TestBuildHTTPServerMCPEndpoint(t *testing.T) {
	// Case 1: with the endpoint enabled a real MCP client completes the handshake, receives the
	// server instructions, and sees the full tool catalog. The instructions assertion is what
	// proves they are actually wired into the server rather than merely declared.
	t.Run("serves a full MCP session when enabled", func(t *testing.T) {
		assert := assert.New(t)
		ctxt := context.Background()

		testServer := buildUnitTestAPIServer(t, models.MCPAPIConfig{Enable: true})

		client := mcp.NewClient(
			&mcp.Implementation{Name: "unit-test-agent", Version: "unit-test"}, nil,
		)
		session, err := client.Connect(ctxt, &mcp.StreamableClientTransport{
			Endpoint: unitTestMCPEndpoint(testServer),
			// A stateless server does not service the standalone SSE stream, and leaving it
			// unopened keeps the test from parking a goroutine on it.
			DisableStandaloneSSE: true,
		}, nil)
		assert.Nil(err)
		t.Cleanup(func() { _ = session.Close() })

		initialized := session.InitializeResult()
		assert.NotNil(initialized)
		assert.NotEmpty(initialized.Instructions)
		// The agent is told where the volume is mounted, from the one constant that defines it.
		assert.Contains(initialized.Instructions, models.WorkspaceMountPath)
		assert.Equal("cairn", initialized.ServerInfo.Name)

		listed, err := session.ListTools(ctxt, &mcp.ListToolsParams{})
		assert.Nil(err)

		names := make([]string, 0, len(listed.Tools))
		for _, tool := range listed.Tools {
			names = append(names, tool.Name)
		}
		assert.ElementsMatch(mcpToolNames, names)
	})

	// Case 2: with the endpoint disabled the route is not registered at all, so the agent facing
	// door can be shut without taking the operator's REST surface with it.
	t.Run("does not mount the endpoint when disabled", func(t *testing.T) {
		assert := assert.New(t)

		testServer := buildUnitTestAPIServer(t, models.MCPAPIConfig{Enable: false})

		response := callUnitTestMCPEndpoint(t, testServer, "")
		assert.Equal(http.StatusNotFound, response.StatusCode)
	})
}

// TestBuildHTTPServerMCPRebindGuard validates both settings of the DNS rebinding guard against
// the topology it actually concerns: a loopback connection carrying a public `Host` header,
// which is what a same-host reverse proxy produces (see DESIGN §2.4).
func TestBuildHTTPServerMCPRebindGuard(t *testing.T) {
	// Case 1: with the guard on, a non-loopback Host over the loopback test listener is refused.
	t.Run("refuses a non-loopback Host when enabled", func(t *testing.T) {
		assert := assert.New(t)

		testServer := buildUnitTestAPIServer(
			t, models.MCPAPIConfig{Enable: true, EnableDNSRebindGuard: true},
		)

		response := callUnitTestMCPEndpoint(t, testServer, "cairn.example.com")
		assert.Equal(http.StatusForbidden, response.StatusCode)
	})

	// Case 2: with the guard off the same request is served. This is the pair that makes the
	// setting meaningful - one case shows it protects, the other that it can be turned off for a
	// deployment whose proxy owns ingress.
	t.Run("serves a non-loopback Host when disabled", func(t *testing.T) {
		assert := assert.New(t)

		testServer := buildUnitTestAPIServer(
			t, models.MCPAPIConfig{Enable: true, EnableDNSRebindGuard: false},
		)

		response := callUnitTestMCPEndpoint(t, testServer, "cairn.example.com")
		assert.Equal(http.StatusOK, response.StatusCode)
	})

	// Case 3: the guard never applies to a loopback Host, whichever way it is set - the listener's
	// own address is what a client dialing it directly sends.
	t.Run("allows a loopback Host either way", func(t *testing.T) {
		assert := assert.New(t)

		for _, guard := range []bool{true, false} {
			testServer := buildUnitTestAPIServer(
				t, models.MCPAPIConfig{Enable: true, EnableDNSRebindGuard: guard},
			)

			response := callUnitTestMCPEndpoint(t, testServer, "")
			assert.Equal(
				http.StatusOK, response.StatusCode, "guard set to %v", guard,
			)
		}
	})
}

// TestBuildHTTPServerRESTUnaffectedByMCP validates that the REST surface answers in every MCP
// configuration, so mounting the agent facing endpoint cannot have disturbed the operator's.
func TestBuildHTTPServerRESTUnaffectedByMCP(t *testing.T) {
	assert := assert.New(t)

	for _, mcpConfig := range []models.MCPAPIConfig{
		{Enable: false},
		{Enable: true, EnableDNSRebindGuard: false},
		{Enable: true, EnableDNSRebindGuard: true},
	} {
		testServer := buildUnitTestAPIServer(t, mcpConfig)

		response, err := http.Get(testServer.URL + unitTestAPIPathPrefix + "/liveness/alive")
		assert.Nil(err)
		assert.Equal(http.StatusOK, response.StatusCode, "MCP config %+v", mcpConfig)
		assert.Nil(response.Body.Close())
	}
}
