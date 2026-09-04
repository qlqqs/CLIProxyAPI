package cliproxy

import (
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
)

func (s *Service) refreshRequestAccessProviders() bool {
	if s == nil || s.accessManager == nil {
		return true
	}
	if s.carpoolModule != nil && sdkaccess.ExclusiveProvider() != "" {
		return false
	}
	providers := sdkaccess.RegisteredProviders()
	if s.carpoolModule != nil {
		if provider := s.carpoolModule.Provider(); provider != nil {
			providers = append(providers, provider)
		}
	}
	s.accessManager.SetProviders(providers)
	return true
}
