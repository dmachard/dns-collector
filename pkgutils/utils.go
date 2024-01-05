package pkgutils

import (
	"github.com/dmachard/go-dnscollector/dnsutils"
	"github.com/dmachard/go-dnscollector/pkgconfig"
)

type Worker interface {
	AddDefaultRoute(wrk Worker)
	AddDroppedRoute(wrk Worker)
	SetLoggers(loggers []Worker)
	GetName() string
	Stop()
	Run()
	GetInputChannel() chan dnsutils.DNSMessage
	ReadConfig()
	ReloadConfig(config *pkgconfig.Config)
}

// func GetActiveRoutes(routes []Worker) ([]chan dnsutils.DNSMessage, []string) {
// 	channels := []chan dnsutils.DNSMessage{}
// 	names := []string{}
// 	for _, p := range routes {
// 		if c := p.GetInputChannel(); c != nil {
// 			channels = append(channels, c)
// 			names = append(names, p.GetName())
// 		} else {
// 			panic("default routing to stanza=[" + p.GetName() + "] not supported")
// 		}
// 	}
// 	return channels, names
// }
