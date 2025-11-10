package handler

import (
	"encoding/json"

	"github.com/wtzhang23/xds-backend/api/v1alpha1"
	"github.com/wtzhang23/xds-backend/pkg/types"
)

var _ types.XdsBackendHandler = (*XdsBackendHandler)(nil)

type XdsBackendHandler struct{}

func (h *XdsBackendHandler) Kind() string {
	return "XdsBackend"
}

func (h *XdsBackendHandler) ApiVersion() string {
	return v1alpha1.GroupVersion.String()
}

func (h *XdsBackendHandler) ParseBackendFromBytes(data []byte) (types.XdsBackendConfig, error) {
	var backend v1alpha1.XdsBackend
	if err := json.Unmarshal(data, &backend); err != nil {
		return nil, err
	}
	return &backend, nil
}
