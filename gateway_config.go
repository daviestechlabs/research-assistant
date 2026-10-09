package main

import (
	"net/url"
	"os"
)

func approvedGatewayOrigin(u *url.URL) bool {
	if u == nil {
		return false
	}
	trusted := os.Getenv("RESEARCH_LITELLM_ORIGIN")
	if trusted == "" {
		return u.Hostname() == "127.0.0.1" && u.Port() != "" && u.Scheme == "http"
	}
	origin, err := url.Parse(trusted)
	return err == nil && (origin.Scheme == "http" || origin.Scheme == "https") && origin.Host != "" && origin.User == nil && origin.Path == "" && origin.RawPath == "" && origin.RawQuery == "" && origin.Fragment == "" && origin.Opaque == "" && u.Scheme == origin.Scheme && u.Host == origin.Host
}
