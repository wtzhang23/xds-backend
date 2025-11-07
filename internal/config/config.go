package config

import (
	"github.com/wtzhang23/xds-backend/proto/v1alpha1"
	"go.yaml.in/yaml/v2"
	"google.golang.org/protobuf/encoding/protojson"
)

func ParseConfig(configFile string) (*v1alpha1.Config, error) {
	var configRaw map[string]any
	if err := yaml.Unmarshal([]byte(configFile), &configRaw); err != nil {
		return nil, err
	}
	jsonData, err := yaml.Marshal(configRaw)
	if err != nil {
		return nil, err
	}
	config := &v1alpha1.Config{}
	if err := protojson.Unmarshal(jsonData, config); err != nil {
		return nil, err
	}
	return config, nil
}
