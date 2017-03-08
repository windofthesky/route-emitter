package config

import (
	"encoding/json"
	"os"
	"time"

	"code.cloudfoundry.org/debugserver"
	"code.cloudfoundry.org/durationjson"
	"code.cloudfoundry.org/lager/lagerflags"
	"code.cloudfoundry.org/locket"
)

type RoutingAPIConfig struct {
	URI          string `yaml:"uri"`
	Port         int    `yaml:"port"`
	AuthDisabled bool   `yaml:"auth_disabled"`
}

type OAuthConfig struct {
	TokenEndpoint     string `yaml:"token_endpoint"`
	Port              int    `yaml:"port"`
	SkipSSLValidation bool   `yaml:"skip_ssl_validation"`
	ClientName        string `yaml:"client_name"`
	ClientSecret      string `yaml:"client_secret"`
	CACerts           string `yaml:"ca_certs"`
}

type RouteEmitterConfig struct {
	BBSAddress                         string                `json:"bbs_address"`
	BBSCACertFile                      string                `json:"bbs_ca_cert_file"`
	BBSClientCertFile                  string                `json:"bbs_client_cert_file"`
	BBSClientKeyFile                   string                `json:"bbs_client_key_file"`
	BBSClientSessionCacheSize          int                   `json:"bbs_client_session_cache_size,omitempty"`
	BBSMaxIdleConnsPerHost             int                   `json:"bbs_max_idle_conns_per_host,omitempty"`
	CellID                             string                `json:"cell_id,omitempty"`
	CommunicationTimeout               durationjson.Duration `json:"communication_timeout,omitempty"`
	ConsulCluster                      string                `json:"consul_cluster,omitempty"`
	ConsulDownModeNotificationInterval durationjson.Duration `json:"consul_down_mode_notification_interval,omitempty"`
	ConsulSessionName                  string                `json:"consul_session_name,omitempty"`
	DropsondePort                      int                   `json:"dropsonde_port,omitempty"`
	HealthCheckAddress                 string                `json:"healthcheck_address,omitempty"`
	LockRetryInterval                  durationjson.Duration `json:"lock_retry_interval,omitempty"`
	LockTTL                            durationjson.Duration `json:"lock_ttl,omitempty"`
	NATSAddresses                      string                `json:"nats_addresses,omitempty"`
	NATSUsername                       string                `json:"nats_username,omitempty"`
	NATSPassword                       string                `json:"nats_password,omitempty"`
	RouteEmittingWorkers               int                   `json:"route_emitting_workers,omitempty"`
	SyncInterval                       durationjson.Duration `json:"sync_interval,omitempty"`
	OAuth                              OAuthConfig           `yaml:"oauth"`
	RoutingAPI                         RoutingAPIConfig      `yaml:"routing_api"`
	lagerflags.LagerConfig
	debugserver.DebugServerConfig
}

func DefaultRouteEmitterConfig() RouteEmitterConfig {
	return RouteEmitterConfig{
		CommunicationTimeout:               durationjson.Duration(30 * time.Second),
		ConsulDownModeNotificationInterval: durationjson.Duration(time.Minute),
		ConsulSessionName:                  "route-emitter",
		DropsondePort:                      3457,
		LockRetryInterval:                  durationjson.Duration(locket.RetryInterval),
		LockTTL:                            durationjson.Duration(locket.DefaultSessionTTL),
		NATSAddresses:                      "nats://127.0.0.1:4222",
		NATSUsername:                       "nats",
		NATSPassword:                       "nats",
		RouteEmittingWorkers:               20,
		SyncInterval:                       durationjson.Duration(time.Minute),
		LagerConfig:                        lagerflags.DefaultLagerConfig(),
	}
}

func NewRouteEmitterConfig(configPath string) (RouteEmitterConfig, error) {
	routeEmitterConfig := DefaultRouteEmitterConfig()

	configFile, err := os.Open(configPath)
	if err != nil {
		return RouteEmitterConfig{}, err
	}

	decoder := json.NewDecoder(configFile)
	err = decoder.Decode(&routeEmitterConfig)
	if err != nil {
		return RouteEmitterConfig{}, err
	}

	return routeEmitterConfig, nil
}
