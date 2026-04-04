package esphome

import (
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/mdns"
)

// DiscoverVoicePE browses for _esphomelib._tcp mDNS services and returns the IP
// address of the first entry whose hostname starts with "home-assistant-voice".
func DiscoverVoicePE() (string, error) {
	const (
		service        = "_esphomelib._tcp"
		browseTimeout  = 5 * time.Second
		hostnamePrefix = "home-assistant-voice"
	)

	entries := make(chan *mdns.ServiceEntry, 16)
	params := mdns.DefaultParams(service)
	params.Entries = entries
	params.Timeout = browseTimeout

	log.Printf("browsing for %s mDNS services for %v", service, browseTimeout)

	matches := make(map[string]net.IP)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for entry := range entries {
			host := strings.TrimSuffix(entry.Host, ".")
			if strings.HasPrefix(strings.ToLower(host), hostnamePrefix) {
				addr := entry.AddrV4
				if addr == nil {
					addr = entry.AddrV6
				}
				if addr == nil {
					log.Printf("mDNS found: name=%q host=%q but no IP address, skipping", entry.Name, host)
					continue
				}
				log.Printf("mDNS found: name=%q host=%q addr=%v port=%d", entry.Name, host, addr, entry.Port)
				matches[host] = addr
			}
		}
	}()

	if err := mdns.Query(params); err != nil {
		return "", fmt.Errorf("mDNS query: %w", err)
	}
	close(entries)
	<-done

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no %s device found via mDNS; set DEVICE_HOST to specify the host explicitly", hostnamePrefix)
	case 1:
		for host, addr := range matches {
			log.Printf("discovered device: %s (%s)", host, addr)
			return addr.String(), nil
		}
	default:
		log.Printf("multiple matching devices found: %v", matches)
		var hostnames []string
		for host := range matches {
			hostnames = append(hostnames, host)
		}
		return "", fmt.Errorf("multiple %s devices found (%v); set DEVICE_HOST to specify which one to use", hostnamePrefix, hostnames)
	}
	return "", fmt.Errorf("no %s device found via mDNS; set DEVICE_HOST to specify the host explicitly", hostnamePrefix)
}
