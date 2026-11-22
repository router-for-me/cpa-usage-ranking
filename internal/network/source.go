// Package network derives registration rate-limit sources, never client identities.
package network

import (
	"errors"
	"net/netip"

	"github.com/gin-gonic/gin"
)

var ErrInvalidSource = errors.New("invalid registration source address")

// RegistrationSource uses Gin's trusted-proxy chain resolution. The router must
// start with no trusted proxies and enable only explicitly configured peers.
func RegistrationSource(c *gin.Context, groupIPv6By64 bool) (string, error) {
	ip, err := netip.ParseAddr(c.ClientIP())
	if err != nil || ip.Zone() != "" {
		return "", ErrInvalidSource
	}
	ip = ip.Unmap()
	if groupIPv6By64 && ip.Is6() {
		return netip.PrefixFrom(ip, 64).Masked().String(), nil
	}
	return ip.String(), nil
}
