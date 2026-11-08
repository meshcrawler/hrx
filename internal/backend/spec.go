package backend

import (
	"errors"
	"net/netip"
	"regexp"
)

func (s ContainerSpec) Validate() error {
	label := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	template := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*:vztmpl/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	tag := regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	if !label.MatchString(s.Storage) || !label.MatchString(s.Bridge) || !template.MatchString(s.Template) || s.Cores < 1 || s.Cores > 256 || s.MemoryMiB < 1 || s.MemoryMiB > 1048576 || s.DiskGiB < 1 || s.DiskGiB > 16384 || len(s.Tags) > 32 {
		return errors.New("invalid Proxmox container specification")
	}
	for _, value := range s.Tags {
		if !tag.MatchString(value) {
			return errors.New("invalid Proxmox container tag")
		}
	}
	if s.Relay != nil {
		for _, value := range []string{s.Relay.ListenAddress, s.Relay.UpstreamAddress} {
			address, err := netip.ParseAddr(value)
			if err != nil || !address.Is4() || address.IsUnspecified() || address.IsMulticast() || address.IsLoopback() {
				return errors.New("relay requires explicit unicast IPv4 endpoints")
			}
		}
		if s.Relay.UpstreamPort < 1 || s.Relay.UpstreamPort > 65535 {
			return errors.New("invalid relay upstream port")
		}
	}
	return nil
}
