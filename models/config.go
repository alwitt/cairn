package models

import (
	"path/filepath"
	"time"

	"github.com/alwitt/goutils"
	"github.com/spf13/viper"
)

// ======================================================================================
// HTTP

// HTTPServerTimeoutConfig defines the timeout settings for HTTP server
type HTTPServerTimeoutConfig struct {
	// ReadTimeout is the maximum duration for reading the entire
	// request, including the body in seconds. A zero or negative
	// value means there will be no timeout.
	ReadTimeout int `mapstructure:"read" json:"read" validate:"gte=0"`
	// WriteTimeout is the maximum duration before timing out
	// writes of the response in seconds. A zero or negative value
	// means there will be no timeout.
	WriteTimeout int `mapstructure:"write" json:"write" validate:"gte=0"`
	// IdleTimeout is the maximum amount of time to wait for the
	// next request when keep-alives are enabled in seconds. If
	// IdleTimeout is zero, the value of ReadTimeout is used. If
	// both are zero, there is no timeout.
	IdleTimeout int `mapstructure:"idle" json:"idle" validate:"gte=0"`
}

// HTTPServerConfig defines the HTTP server parameters
type HTTPServerConfig struct {
	// ListenOn is the interface the HTTP server will listen on
	ListenOn string `mapstructure:"listenOn" json:"listenOn" validate:"required,ip"`
	// Port is the port the HTTP server will listen on
	Port uint16 `mapstructure:"appPort" json:"appPort" validate:"required,gt=0,lt=65536"`
	// Timeouts sets the HTTP timeout settings
	Timeouts HTTPServerTimeoutConfig `mapstructure:"timeoutSecs" json:"timeoutSecs" validate:"required"`
}

// HTTPRequestLogging defines HTTP request logging parameters
type HTTPRequestLogging struct {
	// LogLevel output request logs at this level
	LogLevel goutils.HTTPRequestLogLevel `mapstructure:"logLevel" json:"logLevel" validate:"oneof=warn info debug"`
	// HealthLogLevel output health check logs at this level
	HealthLogLevel goutils.HTTPRequestLogLevel `mapstructure:"healthLogLevel" json:"healthLogLevel" validate:"oneof=warn info debug"`
	// RequestIDHeader is the HTTP header containing the API request ID
	RequestIDHeader string `mapstructure:"requestIDHeader" json:"requestIDHeader"`
	// DoNotLogHeaders is the list of headers to not include in logging metadata
	DoNotLogHeaders []string `mapstructure:"skipHeaders" json:"skipHeaders"`
	// LogRequestPayload whether to log request payload
	LogRequestPayload bool `mapstructure:"logRequestPayload" json:"logRequestPayload"`
}

// EndpointConfig defines API endpoint config
type EndpointConfig struct {
	// PathPrefix is the end-point path prefix for the APIs
	PathPrefix string `mapstructure:"pathPrefix" json:"pathPrefix" validate:"required"`
}

// MCPAPIConfig defines the agent facing MCP endpoint's settings
type MCPAPIConfig struct {
	// Enable whether to serve the MCP endpoint. The REST surface is unaffected either way, so
	// this turns the agent facing door off without taking the operator's away.
	Enable bool `mapstructure:"enable" json:"enable"`

	// EnableDNSRebindGuard whether to keep the MCP SDK's DNS rebinding protection, which
	// refuses a request that arrives over a loopback connection carrying a `Host` header that
	// is not itself a loopback address.
	//
	// It keys off the server's own connection address rather than its listen address, so a
	// same host reverse proxy dialing `127.0.0.1` trips it even when this service listens on
	// `0.0.0.0`. Such a deployment either reaches the service over a non-loopback address or
	// turns the guard off, having already placed ingress with the proxy (see DESIGN §2.4).
	EnableDNSRebindGuard bool `mapstructure:"enableDNSRebindGuard" json:"enableDNSRebindGuard"`
}

// APIConfig defines API settings for a submodule
type APIConfig struct {
	// Endpoint sets API endpoint related parameters
	Endpoint EndpointConfig `mapstructure:"endPoint" json:"endPoint" validate:"required"`
	// RequestLogging sets API request logging parameters
	RequestLogging HTTPRequestLogging `mapstructure:"requestLogging" json:"requestLogging" validate:"required"`
	// MCP sets the MCP endpoint's parameters
	//
	// Deliberately not `required`: that means "not the zero value", and an MCPAPIConfig with
	// both flags false is the zero value - which is the ordinary MCP disabled case, not an
	// invalid one.
	MCP MCPAPIConfig `mapstructure:"mcp" json:"mcp"`
}

// APIServerConfig defines HTTP API / server parameters
type APIServerConfig struct {
	// Server defines HTTP server parameters
	Server HTTPServerConfig `mapstructure:"service" json:"service" validate:"required"`
	// APIs defines API settings for a submodule
	APIs APIConfig `mapstructure:"apis" json:"apis" validate:"required"`
}

// ======================================================================================
// Metrics

// MetricsFeatureConfig metrics framework features config
type MetricsFeatureConfig struct {
	// EnableAppMetrics whether to enable Golang application metrics
	EnableAppMetrics bool `mapstructure:"enableAppMetrics" json:"enableAppMetrics"`
	// EnableHTTPMetrics whether to enable HTTP request tracking metrics
	EnableHTTPMetrics bool `mapstructure:"enableHTTPMetrics" json:"enableHTTPMetrics"`
}

// MetricsConfig application metrics config
type MetricsConfig struct {
	// Server defines HTTP server parameters
	Server HTTPServerConfig `mapstructure:"service" json:"service" validate:"required"`
	// MetricsEndpoint path to host the Prometheus metrics endpoint
	MetricsEndpoint string `mapstructure:"metricsEndpoint" json:"metricsEndpoint" validate:"required"`
	// MaxRequests max number of metrics requests in parallel to support
	MaxRequests int `mapstructure:"maxRequests" json:"maxRequests" validate:"gte=1"`
	// Features metrics framework features to enable
	Features MetricsFeatureConfig `mapstructure:"features" json:"features" validate:"required"`
}

// ===============================================================================
// Persistence Configuration Structures

// PostgresSSLConfig Postgres connection SSL config
type PostgresSSLConfig struct {
	// Enabled whether to enable SSL when connecting to Postgres
	Enabled bool `mapstructure:"enabled" json:"enabled"`
	// CAFile the CA cert file to challenge remote with
	CAFile *string `mapstructure:"caFile" json:"caFile,omitempty" validate:"omitempty,file"`
}

// PostgresConfig Postgres connection config
type PostgresConfig struct {
	// DebugLog whether to output ORM layer debug logs
	DebugLog bool `mapstructure:"debugLog" json:"debugLog"`
	// Host Postgres server host
	Host string `mapstructure:"host" json:"host" validate:"required"`
	// Port Postgres server port
	Port uint16 `mapstructure:"port" json:"port" validate:"lte=65535,gte=0"`
	// Database the specific database to use
	Database string `mapstructure:"db" json:"db" validate:"required"`
	// User the user to connect with
	User string `mapstructure:"user" json:"user" validate:"required"`
	// Password the user password
	Password *string `json:"-" validate:"-"`
	// SSL the connection SSL settings
	SSL PostgresSSLConfig `mapstructure:"ssl" json:"ssl" validate:"required"`
}

// SQLPersistenceConfig system SQL persistence config
type SQLPersistenceConfig struct {
	// Application SQL persistence config
	Application PostgresConfig `mapstructure:"app" json:"app" validate:"required"`
}

// PersistenceConfig application persistence config
type PersistenceConfig struct {
	// SQL persistence config
	SQL SQLPersistenceConfig `mapstructure:"sql" json:"sql" validate:"required"`
}

// ======================================================================================
// Workspace Config

// WorkspaceManagerConfig workspace manager config
type WorkspaceManagerConfig struct {
	// VolumeType workspace persistence volume type. This controls the volume management driver
	// implementation used.
	VolumeType WorkspaceVolumeTypeENUM `mapstructure:"volumeType" json:"volumeType" validate:"required,workspace_volume_type"`
}

// ======================================================================================
// Artifact Config

// ObjectStoreConfig object store config
type ObjectStoreConfig struct {
	// ClientTTLSec S3 client TTL. This is provided to the S3 client manager to periodically
	// refresh the S3 client in use.
	ClientTTLSec int `mapstructure:"clientTTL" json:"clientTTL" validate:"required,gte=60"`
	// ServerEndpoint S3 server endpoint
	ServerEndpoint string `mapstructure:"endpoint" json:"endpoint" validate:"required"`
	// Region optional S3 region. When set, it is used as the client region (which,
	// among other things, is the region newly created buckets are placed in). When
	// nil, the region is left for the server / minio to resolve automatically.
	Region *string `mapstructure:"region,omitempty" json:"region,omitempty" validate:"omitempty"`
	// UseTLS whether to TLS when connecting
	UseTLS bool `mapstructure:"useTLS" json:"useTLS"`
	// Creds object store credentials
	Creds goutils.S3Credentials `validate:"required"`
}

// ClientTTL convert `ClientTTLSec` to time.Duration
func (c ObjectStoreConfig) ClientTTL() time.Duration {
	return time.Second * time.Duration(c.ClientTTLSec)
}

// ToStandard convert to goutils.S3Config
func (c ObjectStoreConfig) ToStandard() goutils.S3Config {
	return goutils.S3Config{
		ServerEndpoint: c.ServerEndpoint,
		UseTLS:         c.UseTLS,
		Region:         c.Region,
		Creds:          c.Creds,
	}
}

// ArtifactKeyConfig artifact object key configs
type ArtifactKeyConfig struct {
	// BasePrefix optional prefix to append to all object keys
	BasePrefix *string `mapstructure:"base,omitempty" json:"base,omitempty" validate:"-"`
	// StagingPrefix prefix to append to a workspace's staging object keys
	StagingPrefix string `mapstructure:"staging" json:"staging" validate:"required"`
	// StorePrefix prefix to append to a workspace's storage object keys
	StorePrefix string `mapstructure:"store" json:"store" validate:"required"`
}

// StagingKeyPrefix helper function to construct the artifact staging object key prefix
// common to all workspaces
func (c ArtifactKeyConfig) StagingKeyPrefix() string {
	pieces := []string{}
	if c.BasePrefix != nil {
		pieces = append(pieces, *c.BasePrefix)
	}
	pieces = append(pieces, c.StagingPrefix)
	return filepath.Join(pieces...)
}

// WorkspaceStagingKeyPrefix helper function to construct the artifact staging object key prefix
// for a particular workspace
func (c ArtifactKeyConfig) WorkspaceStagingKeyPrefix(workspaceID string) string {
	pieces := []string{}
	if c.BasePrefix != nil {
		pieces = append(pieces, *c.BasePrefix)
	}
	pieces = append(pieces, c.StagingPrefix)
	pieces = append(pieces, workspaceID)
	return filepath.Join(pieces...)
}

// StoreKeyPrefix helper function to construct the artifact storage object key prefix
// common to all workspaces
func (c ArtifactKeyConfig) StoreKeyPrefix() string {
	pieces := []string{}
	if c.BasePrefix != nil {
		pieces = append(pieces, *c.BasePrefix)
	}
	pieces = append(pieces, c.StorePrefix)
	return filepath.Join(pieces...)
}

// WorkspaceStoreKeyPrefix helper function to construct the artifact storage object key prefix
// for a particular workspace
func (c ArtifactKeyConfig) WorkspaceStoreKeyPrefix(workspaceID string) string {
	pieces := []string{}
	if c.BasePrefix != nil {
		pieces = append(pieces, *c.BasePrefix)
	}
	pieces = append(pieces, c.StorePrefix)
	pieces = append(pieces, workspaceID)
	return filepath.Join(pieces...)
}

// ArtifactStorageConfig artifact storage config
type ArtifactStorageConfig struct {
	// Bucket to store all artifact and staging objects in
	Bucket string `mapstructure:"bucket" json:"bucket" validate:"required"`

	// UploadPutURLTTLSecs number of seconds a artifact staging PUT URL is valid for
	UploadPutURLTTLSecs int `mapstructure:"putUrlTTLSecs" json:"putUrlTTLSecs" validate:"required,gte=5"`

	// DownloadGetURLMaxTTLSecs the ceiling, in seconds, on how long an artifact download GET
	// URL is valid for. A caller may request a shorter lifetime, never a longer one; asking
	// for nothing in particular takes this value.
	DownloadGetURLMaxTTLSecs int `mapstructure:"getUrlMaxTTLSecs" json:"getUrlMaxTTLSecs" validate:"required,gte=5"`

	// MaxObjectSizeBytes the single-PUT size cap for an artifact's backing object.
	// Multipart upload is out of scope for the first cut, so an object larger than this is
	// an error rather than something to engineer around (see DESIGN §5.2).
	MaxObjectSizeBytes int64 `mapstructure:"maxObjectSize" json:"maxObjectSize" validate:"required,gt=0"`

	// Prefix object key prefix config
	Prefix ArtifactKeyConfig `mapstructure:"prefix" json:"prefix" validate:"required"`
}

// UploadPutURLTTL convert UploadPutUrlTTLSec to time.Duration
func (c ArtifactStorageConfig) UploadPutURLTTL() time.Duration {
	return time.Second * time.Duration(c.UploadPutURLTTLSecs)
}

// DownloadGetURLMaxTTL convert DownloadGetURLMaxTTLSec to time.Duration
func (c ArtifactStorageConfig) DownloadGetURLMaxTTL() time.Duration {
	return time.Second * time.Duration(c.DownloadGetURLMaxTTLSecs)
}

// ArtifactSidecarExtraEnvVar extra ENV variable to add to a sidecar
type ArtifactSidecarExtraEnvVar struct {
	// Name ENV variable name
	Name string `mapstructure:"name" json:"name" validate:"required"`
	// Value ENV variable value
	Value string `mapstructure:"value" json:"value" validate:"required"`
}

// ArtifactSidecarExtraHost extra host-to-IP mapping to inject into the container's /etc/hosts
type ArtifactSidecarExtraHost struct {
	// Host hostname to map to the IP address
	Host string `mapstructure:"host" json:"host" validate:"required"`
	// Address the IP address the hostname resolved to
	Address string `mapstructure:"address" json:"address" validate:"required,ip"`
}

// DefaultSidecarNetworkMode the network mode a transfer sidecar runs with when the config
// does not name one.
//
// Deliberately not the container runtime's own default: that is `none` (see goutils
// `runtime.DefaultDockerNetworkMode`), which suits the stat sidecar but would leave a
// transfer sidecar unable to reach the object store at all.
const DefaultSidecarNetworkMode = "bridge"

// ArtifactSidecarConfig artifact operations sidecar config
type ArtifactSidecarConfig struct {
	// Image the sidecar container image artifact operations run
	Image string `mapstructure:"image" json:"image" validate:"required"`

	// TimeoutSecs wall-clock timeout for a single sidecar run
	TimeoutSecs int `mapstructure:"timeoutSecs" json:"timeoutSecs" validate:"required,gt=0"`

	// NetworkMode the container network a transfer sidecar reaches the object store on.
	// Defaults to DefaultSidecarNetworkMode when unset. The stat sidecar ignores this and
	// always runs with no network at all (see DESIGN §5.1).
	NetworkMode string `mapstructure:"networkMode,omitempty" json:"networkMode,omitempty"`

	// ExtraEnvs extra ENV variables to launch the sidecar containers with
	ExtraEnvs []ArtifactSidecarExtraEnvVar `mapstructure:"envs,omitempty" json:"envs,omitempty" validate:"omitempty,dive"`

	// ExtraHosts extra host-to-IP mapping to inject into sidecar containers
	ExtaHosts []ArtifactSidecarExtraHost `mapstructure:"hosts,omitempty" json:"hosts,omitempty" validate:"omitempty,dive"`
}

// TransferNetworkMode resolve the network mode a transfer sidecar runs with, defaulting when
// the config does not name one.
func (c ArtifactSidecarConfig) TransferNetworkMode() string {
	if c.NetworkMode == "" {
		return DefaultSidecarNetworkMode
	}
	return c.NetworkMode
}

// SidecarTimeout convert TimeoutSecs to time.Duration
func (c ArtifactSidecarConfig) SidecarTimeout() time.Duration {
	return time.Second * time.Duration(c.TimeoutSecs)
}

// ArtifactManagerConfig artifact manager config
type ArtifactManagerConfig struct {
	// ObjectStore object store config
	ObjectStore ObjectStoreConfig `mapstructure:"s3" json:"s3" validate:"required"`

	// Storage artifact storage config
	Storage ArtifactStorageConfig `mapstructure:"store" json:"store" validate:"required"`

	// Sidecar artifact operations sidecar config
	Sidecar ArtifactSidecarConfig `mapstructure:"sidecar" json:"sidecar" validate:"required"`
}

// ======================================================================================
// Maintenance Config

// MaintenanceConfig maintenance system config
type MaintenanceConfig struct {
	// MaintenanceSweepIntSec number of seconds between maintenance sweep
	MaintenanceSweepIntSec int `mapstructure:"sweepIntSec" json:"sweepIntSec" validate:"required,gt=0"`

	// OrphanedObjectAgeOutSec number of seconds after which an orphaned object will be deleted from
	// the object store
	OrphanedObjectAgeOutSec int `mapstructure:"objAgeOutSec" json:"objAgeOutSec" validate:"required,gt=0"`
}

// MaintenanceSweepInt convert MaintenanceSweepIntSec to time.Duration
func (c MaintenanceConfig) MaintenanceSweepInt() time.Duration {
	return time.Second * time.Duration(c.MaintenanceSweepIntSec)
}

// OrphanedObjectAgeOut convert OrphanedObjectAgeOutSec to time.Duration
func (c MaintenanceConfig) OrphanedObjectAgeOut() time.Duration {
	return time.Second * time.Duration(c.OrphanedObjectAgeOutSec)
}

// ======================================================================================
// Application Config

// ApplicationConfig application config
type ApplicationConfig struct {
	// AppName application name which is used to construct workspace persistent volume manages
	AppName string `mapstructure:"appName" json:"appName" validate:"required,valid_name"`

	// Persistence system persistence config
	Persistence PersistenceConfig `mapstructure:"persistence" json:"persistence" validate:"required"`

	// Metrics metrics framework configuration
	Metrics MetricsConfig `mapstructure:"metrics" json:"metrics" validate:"required"`

	// API server config
	API APIServerConfig `mapstructure:"api" json:"api" validate:"required"`

	// Workspace manager configuration
	Workspace WorkspaceManagerConfig `mapstructure:"workspace" json:"workspace" validate:"required"`

	// Artifact artifact configuration
	Artifact ArtifactManagerConfig `mapstructure:"artifact" json:"artifact" validate:"required"`

	// Maintenance maintenance system configuration
	Maintenance MaintenanceConfig `mapstructure:"maintenance" json:"maintenance" validate:"required"`
}

// ======================================================================================
// Default Configuration Setter

/*
The defaults describe one coherent single-host development deployment: a loopback Postgres, the
sidecar image this repo's own `sidecar/Makefile` builds, and every tunable set to a value that
works before anything is tuned.

What is deliberately left undefaulted is what names a specific deployment's resources - the
object store endpoint and the bucket. There is no value for those that is right anywhere but
where it was written, and a wrong-but-present default is worse than an absent one:
`validate:"required"` reports the missing field by name at startup, whereas a default silently
points the service at something that was never meant to hold artifacts.

Credentials are not addressed here at all: neither the object store's nor the database's is a
config file key (see ObjectStoreConfig.Creds, which carries no `mapstructure` tag). They are
supplied on the command line or through the environment and installed onto the config after it
is read, so there is nothing here for a default to reach.
*/

// defaultSidecarTimeoutSecs the wall-clock ceiling on a single transfer sidecar run.
//
// Named because three defaults are derived from it rather than chosen independently: the API's
// write timeout has to outlast the sidecars a request launches, and a presigned URL has to
// outlive the transfer it was minted for. Retuning the sidecar timeout in a config file without
// revisiting those two leaves them inconsistent, so the relationship is at least written down
// here for the shipped values.
const defaultSidecarTimeoutSecs = 300

// defaultPresignedURLMarginSecs how much longer than a sidecar's own run a presigned URL lives,
// covering container startup (DESIGN §5.2). Mirrors `getURLTTLMargin` on the download path.
const defaultPresignedURLMarginSecs = 60

// InstallDefaultServerConfigValues setup default server configs
func InstallDefaultServerConfigValues() {
	// Default application config
	viper.SetDefault("appName", "cairn")

	// Default persistence config
	viper.SetDefault("persistence.sql.app.debugLog", false)
	viper.SetDefault("persistence.sql.app.host", "127.0.0.1")
	viper.SetDefault("persistence.sql.app.port", 5432)
	viper.SetDefault("persistence.sql.app.db", "cairn")
	viper.SetDefault("persistence.sql.app.user", "cairn")
	// Off to match the loopback host above, where there is no network to protect. A deployment
	// that moves the database off the host is the one that turns this on. The password is not a
	// config key at all - it is supplied out of band and handed to GetPostgresDialector.
	viper.SetDefault("persistence.sql.app.ssl.enabled", false)

	// Default metrics config
	viper.SetDefault("metrics.metricsEndpoint", "/metrics")
	viper.SetDefault("metrics.maxRequests", 4)
	// Default metrics features config
	viper.SetDefault("metrics.features.enableAppMetrics", false)
	viper.SetDefault("metrics.features.enableHTTPMetrics", true)
	// Default metrics HTTP server config
	viper.SetDefault("metrics.service.listenOn", "0.0.0.0")
	viper.SetDefault("metrics.service.appPort", 3001)
	viper.SetDefault("metrics.service.timeoutSecs.read", 60)
	viper.SetDefault("metrics.service.timeoutSecs.write", 60)
	viper.SetDefault("metrics.service.timeoutSecs.idle", 60)

	// Default API HTTP server config
	viper.SetDefault("api.service.listenOn", "0.0.0.0")
	viper.SetDefault("api.service.appPort", 44123)
	viper.SetDefault("api.service.timeoutSecs.read", 60)
	// The slowest request the API serves is a volume based upload: two sidecar runs back to back
	// - stat then transfer (DESIGN §6.4) - plus the object store copy. A shorter write timeout
	// would cut the caller off from a transfer that is still running and will still complete,
	// leaving the artifact recorded but the caller told nothing.
	viper.SetDefault(
		"api.service.timeoutSecs.write", (defaultSidecarTimeoutSecs*2)+defaultPresignedURLMarginSecs,
	)
	viper.SetDefault("api.service.timeoutSecs.idle", 60)

	// Default API config
	viper.SetDefault("api.apis.endPoint.pathPrefix", "/")
	viper.SetDefault("api.apis.requestLogging.logLevel", "warn")
	viper.SetDefault("api.apis.requestLogging.healthLogLevel", "debug")
	viper.SetDefault("api.apis.requestLogging.requestIDHeader", "X-Request-ID")
	viper.SetDefault("api.apis.requestLogging.skipHeaders", []string{
		"WWW-Authenticate", "Authorization", "Proxy-Authenticate", "Proxy-Authorization",
	})
	viper.SetDefault("api.apis.requestLogging.logRequestPayload", false)

	// Default MCP API config
	viper.SetDefault("api.apis.mcp.enable", false)
	// On, so that a deployment opts out of the SDK's own protection deliberately rather than by
	// never naming the key. The one deployment that has to opt out - a same host reverse proxy
	// dialing loopback - is also the one whose operator knows they placed ingress with the proxy
	// (see MCPAPIConfig.EnableDNSRebindGuard and DESIGN §2.4).
	viper.SetDefault("api.apis.mcp.enableDNSRebindGuard", true)

	// Default workspace config
	viper.SetDefault("workspace.volumeType", string(WorkspaceVolumeTypeDocker))

	// Default artifact object store config
	//
	// How long a client is used before the manager replaces it. An hour is short enough that a
	// rotated credential is picked up on a timescale an operator would call prompt, and long
	// enough that the rebuild is never on a request's path in practice.
	viper.SetDefault("artifact.s3.clientTTL", 3600)
	// The transport defaults to the secure setting. The endpoint has no default, so an operator
	// is editing this block regardless and can say so if their object store is plaintext.
	// Getting this wrong fails to connect rather than quietly sending credentials in the clear.
	viper.SetDefault("artifact.s3.useTLS", true)

	// Default artifact storage config
	//
	// The PUT URL only has to outlive the transfer it was minted for (DESIGN §5.2). The GET
	// URL's ceiling is longer because it also bounds the link the REST API hands an operator,
	// who may not follow it immediately; the download sidecar asks for far less than the
	// ceiling and is not bound by it.
	viper.SetDefault(
		"artifact.store.putUrlTTLSecs", defaultSidecarTimeoutSecs+defaultPresignedURLMarginSecs,
	)
	viper.SetDefault("artifact.store.getUrlMaxTTLSecs", 900)
	// 1 GiB. Multipart upload is out of scope, so this is a single PUT and the cap has to stay
	// well under the object store's own single PUT limit - and low enough that an object of
	// this size still transfers inside a sidecar run.
	viper.SetDefault("artifact.store.maxObjectSize", 1024*1024*1024)
	viper.SetDefault("artifact.store.prefix.staging", "staging")
	viper.SetDefault("artifact.store.prefix.store", "store")

	// Default artifact sidecar config
	viper.SetDefault("artifact.sidecar.image", "alwitt/cairn-sidecar:latest")
	viper.SetDefault("artifact.sidecar.timeoutSecs", defaultSidecarTimeoutSecs)
	viper.SetDefault("artifact.sidecar.networkMode", DefaultSidecarNetworkMode)

	// Default maintenance config
	viper.SetDefault("maintenance.sweepIntSec", 300)
	// The grace window has to comfortably exceed the longest in-flight upload, or a sweep would
	// flag the staging object of a transfer that is still running (DESIGN §8.2.1).
	viper.SetDefault("maintenance.objAgeOutSec", 3600)
}
