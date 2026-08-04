package models

import (
	"path/filepath"
	"time"

	"github.com/alwitt/goutils"
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

// APIConfig defines API settings for a submodule
type APIConfig struct {
	// Endpoint sets API endpoint related parameters
	Endpoint EndpointConfig `mapstructure:"endPoint" json:"endPoint" validate:"required"`
	// RequestLogging sets API request logging parameters
	RequestLogging HTTPRequestLogging `mapstructure:"requestLogging" json:"requestLogging" validate:"required"`
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
	// ServerEndpoint S3 server endpoint
	ServerEndpoint string `mapstructure:"endpoint" json:"endpoint" validate:"required"`
	// UseTLS whether to TLS when connecting
	UseTLS bool `mapstructure:"useTLS" json:"useTLS"`
	// AccessKey object store access key
	AccessKey string `mapstructure:"accessID" json:"accessID" validate:"required"`
	// SecretAccessKey object store secret access key
	SecretAccessKey string `mapstructure:"secretID" json:"secretID" validate:"required"`
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

	// UploadPutURLTTLSec number of seconds a artifact staging PUT URL is valid for
	UploadPutURLTTLSec int `mapstructure:"putUrlTTL" json:"putUrlTTL" validate:"required,gte=5"`

	// MaxObjectSizeBytes the single-PUT size cap for an artifact's backing object.
	// Multipart upload is out of scope for the first cut, so an object larger than this is
	// an error rather than something to engineer around (see DESIGN §5.2).
	MaxObjectSizeBytes int64 `mapstructure:"maxObjectSize" json:"maxObjectSize" validate:"required,gt=0"`

	// Prefix object key prefix config
	Prefix ArtifactKeyConfig `mapstructure:"prefix" json:"prefix" validate:"required"`
}

// UploadPutURLTTL convert UploadPutUrlTTLSec to time.Duration
func (c ArtifactStorageConfig) UploadPutURLTTL() time.Duration {
	return time.Second * time.Duration(c.UploadPutURLTTLSec)
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
