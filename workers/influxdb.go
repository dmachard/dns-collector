package workers

import (
	"time"

	"github.com/dmachard/go-dnscollector/pkgconfig"
	"github.com/dmachard/go-dnscollector/pkgutils"
	"github.com/dmachard/go-dnscollector/transformers"
	"github.com/dmachard/go-logger"

	influxdb2 "github.com/influxdata/influxdb-client-go"
	"github.com/influxdata/influxdb-client-go/api"
)

type InfluxDBClient struct {
	*pkgutils.GenericWorker
	influxdbConn influxdb2.Client
	writeAPI     api.WriteAPI
}

func NewInfluxDBClient(config *pkgconfig.Config, logger *logger.Logger, name string) *InfluxDBClient {
	w := &InfluxDBClient{GenericWorker: pkgutils.NewGenericWorker(config, logger, name, "influxdb", config.Loggers.InfluxDB.ChannelBufferSize)}
	w.ReadConfig()
	return w
}

func (w *InfluxDBClient) StartCollect() {
	w.LogInfo("worker is starting collection")
	defer w.CollectDone()

	// prepare next channels
	defaultRoutes, defaultNames := pkgutils.GetRoutes(w.GetDefaultRoutes())
	droppedRoutes, droppedNames := pkgutils.GetRoutes(w.GetDroppedRoutes())

	// prepare transforms
	subprocessors := transformers.NewTransforms(&w.GetConfig().OutgoingTransformers, w.GetLogger(), w.GetName(), w.GetOutputChannelAsList(), 0)

	// goroutine to process transformed dns messages
	go w.StartLogging()

	// loop to process incoming messages
	for {
		select {
		case <-w.OnStop():
			w.StopLogger()
			subprocessors.Reset()
			return

			// new config provided?
		case cfg := <-w.NewConfig():
			w.SetConfig(cfg)
			w.ReadConfig()
			subprocessors.ReloadConfig(&cfg.OutgoingTransformers)

		case dm, opened := <-w.GetInputChannel():
			if !opened {
				w.LogInfo("input channel closed!")
				return
			}

			// apply tranforms, init dns message with additionnals parts if necessary
			subprocessors.InitDNSMessageFormat(&dm)
			if subprocessors.ProcessMessage(&dm) == transformers.ReturnDrop {
				w.SendTo(droppedRoutes, droppedNames, dm)
				continue
			}

			// send to output channel
			w.GetOutputChannel() <- dm

			// send to next ?
			w.SendTo(defaultRoutes, defaultNames, dm)
		}
	}
}

func (w *InfluxDBClient) StartLogging() {
	w.LogInfo("worker is starting logging")
	defer w.LoggingDone()

	// prepare options for influxdb
	opts := influxdb2.DefaultOptions()
	opts.SetUseGZip(true)
	if w.GetConfig().Loggers.InfluxDB.TLSSupport {
		tlsOptions := pkgconfig.TLSOptions{
			InsecureSkipVerify: w.GetConfig().Loggers.InfluxDB.TLSInsecure,
			MinVersion:         w.GetConfig().Loggers.InfluxDB.TLSMinVersion,
			CAFile:             w.GetConfig().Loggers.InfluxDB.CAFile,
			CertFile:           w.GetConfig().Loggers.InfluxDB.CertFile,
			KeyFile:            w.GetConfig().Loggers.InfluxDB.KeyFile,
		}

		tlsConfig, err := pkgconfig.TLSClientConfig(tlsOptions)
		if err != nil {
			w.LogFatal("logger=influxdb - tls config failed:", err)
		}

		opts.SetTLSConfig(tlsConfig)
	}
	// init the client
	influxClient := influxdb2.NewClientWithOptions(
		w.GetConfig().Loggers.InfluxDB.ServerURL,
		w.GetConfig().Loggers.InfluxDB.AuthToken,
		opts,
	)

	writeAPI := influxClient.WriteAPI(
		w.GetConfig().Loggers.InfluxDB.Organization,
		w.GetConfig().Loggers.InfluxDB.Bucket,
	)

	w.influxdbConn = influxClient
	w.writeAPI = writeAPI

	for {
		select {
		case <-w.OnLoggerStopped():
			// Force all unwritten data to be sent
			w.writeAPI.Flush()
			// Ensures background processes finishes
			w.influxdbConn.Close()
			return

			// incoming dns message to process
		case dm, opened := <-w.GetOutputChannel():
			if !opened {
				w.LogInfo("output channel closed!")
				return
			}

			p := influxdb2.NewPointWithMeasurement("dns").
				AddTag("Identity", dm.DNSTap.Identity).
				AddTag("QueryIP", dm.NetworkInfo.QueryIP).
				AddTag("Qname", dm.DNS.Qname).
				AddField("Operation", dm.DNSTap.Operation).
				AddField("Family", dm.NetworkInfo.Family).
				AddField("Protocol", dm.NetworkInfo.Protocol).
				AddField("Qtype", dm.DNS.Qtype).
				AddField("Rcode", dm.DNS.Rcode).
				SetTime(time.Unix(int64(dm.DNSTap.TimeSec), int64(dm.DNSTap.TimeNsec)))

			// write asynchronously
			w.writeAPI.WritePoint(p)
		}
	}
}