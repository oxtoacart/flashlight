// package cloudflare provides an implementation of a host-spoofing protocol for
// CloudFlare.
package cloudflare

const (
	CF_PREFIX        = "Cf-"
	X_LANTERN_PREFIX = "X-Lantern-"
	X_LANTERN_URL    = X_LANTERN_PREFIX + "URL"
)