package models_test

import (
	"testing"

	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/go-playground/validator/v10"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
)

/*
installUnitTestDeploymentConfigValues supply the settings that InstallDefaultServerConfigValues
deliberately leaves out - the ones that name a specific deployment's object store.

Standing in for them here is what lets the defaults be checked against the validator: without
them a default config is legitimately invalid, and the test could not tell that apart from a
default that is actually wrong.
*/
func installUnitTestDeploymentConfigValues() {
	viper.Set("artifact.s3.endpoint", "127.0.0.1:9000")
	viper.Set("artifact.store.bucket", "unit-test-bucket")
}

/*
installUnitTestCredentials stand in for the credentials the entry point installs onto the config
after reading it.

They are not config file keys - `ObjectStoreConfig.Creds` carries no `mapstructure` tag - so no
amount of viper setting reaches them, and they have to be assigned the same way `main` assigns
them or the config cannot validate.

	@param config *models.ApplicationConfig - the config to install onto
*/
func installUnitTestCredentials(config *models.ApplicationConfig) {
	password := "unit-test-password"
	config.Persistence.SQL.Application.Password = &password
	config.Artifact.ObjectStore.Creds = goutils.S3Credentials{
		AccessKey: "unit-test-access-key", SecretAccessKey: "unit-test-secret-key",
	}
}

/*
readUnitTestDefaultConfig install the defaults, fill in the deployment specific settings, and
read the result back as an ApplicationConfig.

	@param t *testing.T - the running test
	@returns the application config the defaults produce
*/
func readUnitTestDefaultConfig(t *testing.T) models.ApplicationConfig {
	t.Helper()
	assert := assert.New(t)

	// Viper's registry is global, so the test owns it for its duration and hands it back clean.
	viper.Reset()
	t.Cleanup(viper.Reset)

	models.InstallDefaultServerConfigValues()
	installUnitTestDeploymentConfigValues()

	var config models.ApplicationConfig
	assert.Nil(viper.Unmarshal(&config))
	installUnitTestCredentials(&config)

	return config
}

/*
TestInstallDefaultServerConfigValues validates that the defaults actually reach the config
structure and describe a usable deployment.

The keys are hand written dotted strings while the destinations are `mapstructure` tags, and
nothing connects the two at compile time - a misspelled key is not an error, it is a default
that silently never applies. Unmarshalling and validating is the only thing that catches that.
*/
func TestInstallDefaultServerConfigValues(t *testing.T) {
	// Case 1: the defaults plus the settings only a deployment can supply produce a config the
	// application's own validator accepts. This is the assertion that covers every key at once:
	// a default that never landed leaves its field at the zero value, and every field that
	// matters carries `required`.
	t.Run("produces a valid application config", func(t *testing.T) {
		assert := assert.New(t)

		config := readUnitTestDefaultConfig(t)

		validate := validator.New()
		assert.Nil(models.RegisterWithValidator(validate))
		assert.Nil(validate.Struct(&config))
	})

	// Case 2: the values that are derived from one another rather than chosen independently.
	// Asserting the relationships rather than the literals is what keeps a retuned sidecar
	// timeout from quietly outliving the write timeout or its own presigned URLs.
	t.Run("keeps the transfer timeouts consistent", func(t *testing.T) {
		assert := assert.New(t)

		config := readUnitTestDefaultConfig(t)

		sidecarTimeout := config.Artifact.Sidecar.TimeoutSecs

		// A volume based upload runs two sidecars back to back before the response is written.
		assert.Greater(config.API.Server.Timeouts.WriteTimeout, sidecarTimeout*2)
		// A presigned URL has to outlive the transfer it was minted for.
		assert.Greater(config.Artifact.Storage.UploadPutURLTTLSecs, sidecarTimeout)
		assert.Greater(config.Artifact.Storage.DownloadGetURLMaxTTLSecs, sidecarTimeout)
		// The maintenance grace window has to outlast an upload that is still in flight, or a
		// sweep flags the staging object of a transfer that is still running.
		assert.Greater(
			config.Maintenance.OrphanedObjectAgeOutSec, config.API.Server.Timeouts.WriteTimeout,
		)
	})

	// Case 3: the two settings whose default is a security posture rather than a tuning choice.
	// Both are bools, so a default that failed to land reads as the opposite posture without
	// anything else looking wrong.
	t.Run("defaults to the guarded posture", func(t *testing.T) {
		assert := assert.New(t)

		config := readUnitTestDefaultConfig(t)

		// The MCP endpoint is off, but its rebinding guard is on for whoever turns it on.
		assert.False(config.API.APIs.MCP.Enable)
		assert.True(config.API.APIs.MCP.EnableDNSRebindGuard)
		assert.True(config.Artifact.ObjectStore.UseTLS)
	})

	// Case 4: a config file overrides a default rather than being merged around it. Worth
	// pinning because the defaults now cover nearly every key, so an override that did not take
	// would leave a plausible looking value in place instead of an empty one.
	t.Run("yields to an explicit setting", func(t *testing.T) {
		assert := assert.New(t)

		viper.Reset()
		t.Cleanup(viper.Reset)

		models.InstallDefaultServerConfigValues()
		installUnitTestDeploymentConfigValues()
		viper.Set("api.apis.mcp.enable", true)
		viper.Set("api.service.appPort", 12345)

		var config models.ApplicationConfig
		assert.Nil(viper.Unmarshal(&config))
		installUnitTestCredentials(&config)

		assert.True(config.API.APIs.MCP.Enable)
		assert.EqualValues(12345, config.API.Server.Port)
		// The sibling default under the same overridden parent is still there.
		assert.True(config.API.APIs.MCP.EnableDNSRebindGuard)
	})
}
