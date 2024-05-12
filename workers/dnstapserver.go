package workers

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dmachard/go-dnscollector/dnsutils"
	"github.com/dmachard/go-dnscollector/netutils"
	"github.com/dmachard/go-dnscollector/pkgconfig"
	"github.com/dmachard/go-dnscollector/pkgutils"
	"github.com/dmachard/go-dnscollector/transformers"
	"github.com/dmachard/go-dnstap-protobuf"
	"github.com/dmachard/go-framestream"
	"github.com/dmachard/go-logger"
	"github.com/segmentio/kafka-go/compress"
	"google.golang.org/protobuf/proto"
)

type DnstapServer struct {
	*pkgutils.GenericWorker
	connCounter uint64
}

func NewDnstapServer(next []pkgutils.Worker, config *pkgconfig.Config, logger *logger.Logger, name string) *DnstapServer {
	w := &DnstapServer{GenericWorker: pkgutils.NewGenericWorker(config, logger, name, "dnstap", pkgutils.DefaultBufferSize)}
	w.SetDefaultRoutes(next)
	w.CheckConfig()
	return w
}

func (w *DnstapServer) CheckConfig() {
	if !pkgconfig.IsValidTLS(w.GetConfig().Collectors.Dnstap.TLSMinVersion) {
		w.LogFatal(pkgutils.PrefixLogCollector + "[" + w.GetName() + "] dnstap - invalid tls min version")
	}
}

func (w *DnstapServer) HandleConn(conn net.Conn, connID uint64, forceClose chan bool, wg *sync.WaitGroup) {
	// close connection on function exit
	defer func() {
		w.LogInfo("conn #%d - connection handler terminated", connID)
		netutils.Close(conn, w.GetConfig().Collectors.Dnstap.ResetConn)
		wg.Done()
	}()

	// get peer address
	peer := conn.RemoteAddr().String()
	peerName := netutils.GetPeerName(peer)
	w.LogInfo("new connection #%d from %s (%s)", connID, peer, peerName)

	// start dnstap processor and run it
	dnstapProcessor := NewDNSTapProcessor(int(connID), peerName, w.GetConfig(), w.GetLogger(), w.GetName(), w.GetConfig().Collectors.Dnstap.ChannelBufferSize)
	go dnstapProcessor.Run(w.GetDefaultRoutes(), w.GetDroppedRoutes())

	// init frame stream library
	fsReader := bufio.NewReader(conn)
	fsWriter := bufio.NewWriter(conn)
	fs := framestream.NewFstrm(fsReader, fsWriter, conn, 5*time.Second, []byte("protobuf:dnstap.Dnstap"), true)

	// framestream as receiver
	if err := fs.InitReceiver(); err != nil {
		w.LogError("conn #%d - stream initialization: %s", connID, err)
	} else {
		w.LogInfo("conn #%d - receiver framestream initialized", connID)
	}

	// process incoming frame and send it to dnstap consumer channel
	var err error
	var frame *framestream.Frame
	cleanup := make(chan struct{})

	// goroutine to close the connection properly
	go func() {
		defer func() {
			dnstapProcessor.Stop()
			w.LogInfo("conn #%d - cleanup connection handler terminated", connID)
		}()

		for {
			select {
			case <-forceClose:
				w.LogInfo("conn #%d - force to cleanup the connection handler", connID)
				netutils.Close(conn, w.GetConfig().Collectors.Dnstap.ResetConn)
				return
			case <-cleanup:
				w.LogInfo("conn #%d - cleanup the connection handler", connID)
				return
			}
		}
	}()

	// handle incoming frame
	for {
		if w.GetConfig().Collectors.Dnstap.Compression == pkgconfig.CompressNone {
			frame, err = fs.RecvFrame(false)
		} else {
			frame, err = fs.RecvCompressedFrame(&compress.GzipCodec, false)
		}
		if err != nil {
			connClosed := false

			var opErr *net.OpError
			if errors.As(err, &opErr) {
				if errors.Is(opErr, net.ErrClosed) {
					connClosed = true
				}
			}
			if errors.Is(err, io.EOF) {
				connClosed = true
			}

			if connClosed {
				w.LogInfo("conn #%d - connection closed with peer %s", connID, peer)
			} else {
				w.LogError("conn #%d - framestream reader error: %s", connID, err)
			}
			// exit goroutine
			close(cleanup)
			break
		}

		if frame.IsControl() {
			if err := fs.ResetReceiver(frame); err != nil {
				if errors.Is(err, io.EOF) {
					w.LogInfo("conn #%d - framestream reseted by sender", connID)
				} else {
					w.LogError("conn #%d - unexpected control framestream: %s", connID, err)
				}

			}

			// exit goroutine
			close(cleanup)
			break
		}

		if w.GetConfig().Collectors.Dnstap.Compression == pkgconfig.CompressNone {
			// send payload to the channel
			select {
			case dnstapProcessor.GetChannel() <- frame.Data(): // Successful send to channel
			default:
				w.ProcessorIsBusy()
			}
		} else {
			// ignore first 4 bytes
			data := frame.Data()[4:]
			validFrame := true
			for len(data) >= 4 {
				// get frame size
				payloadSize := binary.BigEndian.Uint32(data[:4])
				data = data[4:]

				// enough next data ?
				if uint32(len(data)) < payloadSize {
					validFrame = false
					break
				}
				// send payload to the channel
				select {
				case dnstapProcessor.GetChannel() <- data[:payloadSize]: // Successful send to channel
				default:
					w.ProcessorIsBusy()
				}

				// continue for next
				data = data[payloadSize:]
			}
			if !validFrame {
				w.LogError("conn #%d - invalid compressed frame received", connID)
				continue
			}
		}
	}
}

func (w *DnstapServer) StartCollect() {
	w.LogInfo("worker is starting collection")
	defer w.CollectDone()

	var connWG sync.WaitGroup
	connCleanup := make(chan bool)
	cfg := w.GetConfig().Collectors.Dnstap

	// start to listen
	listener, err := netutils.StartToListen(
		cfg.ListenIP, cfg.ListenPort, cfg.SockPath,
		cfg.TLSSupport, pkgconfig.TLSVersion[cfg.TLSMinVersion],
		cfg.CertFile, cfg.KeyFile)
	if err != nil {
		w.LogFatal(pkgutils.PrefixLogCollector+"["+w.GetName()+"] listen error: ", err)
	}
	w.LogInfo("listening on %s", listener.Addr())

	// goroutine to Accept() blocks waiting for new connection.
	acceptChan := make(chan net.Conn)
	netutils.AcceptConnections(listener, acceptChan)

	// main loop
	for {
		select {
		case <-w.OnStop():
			w.LogInfo("stop to listen...")
			listener.Close()

			w.LogInfo("closing connected peers...")
			close(connCleanup)
			connWG.Wait()
			return

		// save the new config
		case cfg := <-w.NewConfig():
			w.SetConfig(cfg)
			w.CheckConfig()

		// new incoming connection
		case conn, opened := <-acceptChan:
			if !opened {
				return
			}

			if len(cfg.SockPath) == 0 && cfg.RcvBufSize > 0 {
				before, actual, err := netutils.SetSockRCVBUF(conn, cfg.RcvBufSize, cfg.TLSSupport)
				if err != nil {
					w.LogFatal(pkgutils.PrefixLogCollector+"["+w.GetName()+"] unable to set SO_RCVBUF: ", err)
				}
				w.LogInfo("set SO_RCVBUF option, value before: %d, desired: %d, actual: %d", before, cfg.RcvBufSize, actual)
			}

			// handle the connection
			connWG.Add(1)
			connID := atomic.AddUint64(&w.connCounter, 1)
			go w.HandleConn(conn, connID, connCleanup, &connWG)
		}

	}
}

func GetFakeDNSTap(dnsquery []byte) *dnstap.Dnstap {
	dtQuery := &dnstap.Dnstap{}

	dt := dnstap.Dnstap_MESSAGE
	dtQuery.Identity = []byte("dnstap-generator")
	dtQuery.Version = []byte("-")
	dtQuery.Type = &dt

	mt := dnstap.Message_CLIENT_QUERY
	sf := dnstap.SocketFamily_INET
	sp := dnstap.SocketProtocol_UDP

	now := time.Now()
	tsec := uint64(now.Unix())
	tnsec := uint32(uint64(now.UnixNano()) - uint64(now.Unix())*1e9)

	rport := uint32(53)
	qport := uint32(5300)

	msg := &dnstap.Message{Type: &mt}
	msg.SocketFamily = &sf
	msg.SocketProtocol = &sp
	msg.QueryAddress = net.ParseIP("127.0.0.1")
	msg.QueryPort = &qport
	msg.ResponseAddress = net.ParseIP("127.0.0.2")
	msg.ResponsePort = &rport

	msg.QueryMessage = dnsquery
	msg.QueryTimeSec = &tsec
	msg.QueryTimeNsec = &tnsec

	dtQuery.Message = msg
	return dtQuery
}

type DNSTapProcessor struct {
	ConnID                   int
	PeerName                 string
	doneRun, stopRun         chan bool
	doneMonitor, stopMonitor chan bool
	recvFrom                 chan []byte
	logger                   *logger.Logger
	config                   *pkgconfig.Config
	ConfigChan               chan *pkgconfig.Config
	name                     string
	chanSize                 int
	RoutingHandler           pkgutils.RoutingHandler
	dropped                  chan string
	droppedCount             map[string]int
}

func NewDNSTapProcessor(connID int, peerName string, config *pkgconfig.Config, logger *logger.Logger, name string, size int) DNSTapProcessor {
	logger.Info(pkgutils.PrefixLogProcessor+"[%s] dnstap - conn #%d - initialization...", name, connID)
	d := DNSTapProcessor{
		ConnID:         connID,
		PeerName:       peerName,
		doneMonitor:    make(chan bool),
		doneRun:        make(chan bool),
		stopMonitor:    make(chan bool),
		stopRun:        make(chan bool),
		recvFrom:       make(chan []byte, size),
		chanSize:       size,
		logger:         logger,
		config:         config,
		ConfigChan:     make(chan *pkgconfig.Config),
		name:           name,
		dropped:        make(chan string),
		droppedCount:   map[string]int{},
		RoutingHandler: pkgutils.NewRoutingHandler(config, logger, name),
	}

	return d
}

func (d *DNSTapProcessor) LogInfo(msg string, v ...interface{}) {
	var log string
	if d.ConnID == 0 {
		log = fmt.Sprintf(pkgutils.PrefixLogProcessor+"[%s] dnstap - ", d.name)
	} else {
		log = fmt.Sprintf(pkgutils.PrefixLogProcessor+"[%s] dnstap - conn #%d - ", d.name, d.ConnID)
	}
	d.logger.Info(log+msg, v...)
}

func (d *DNSTapProcessor) LogError(msg string, v ...interface{}) {
	var log string
	if d.ConnID == 0 {
		log = fmt.Sprintf(pkgutils.PrefixLogProcessor+"[%s] dnstap - ", d.name)
	} else {
		log = fmt.Sprintf(pkgutils.PrefixLogProcessor+"[%s] dnstap - conn #%d - ", d.name, d.ConnID)
	}
	d.logger.Error(log+msg, v...)
}

func (d *DNSTapProcessor) GetChannel() chan []byte {
	return d.recvFrom
}

func (d *DNSTapProcessor) Stop() {
	d.LogInfo("stopping processor...")
	d.RoutingHandler.Stop()

	d.LogInfo("stopping to process...")
	d.stopRun <- true
	<-d.doneRun

	d.LogInfo("stopping monitor...")
	d.stopMonitor <- true
	<-d.doneMonitor
}

func (d *DNSTapProcessor) Run(defaultWorkers []pkgutils.Worker, droppedworkers []pkgutils.Worker) {
	dt := &dnstap.Dnstap{}
	edt := &dnsutils.ExtendedDnstap{}

	// prepare next channels
	defaultRoutes, defaultNames := pkgutils.GetRoutes(defaultWorkers)
	droppedRoutes, droppedNames := pkgutils.GetRoutes(droppedworkers)

	// prepare enabled transformers
	transforms := transformers.NewTransforms(&d.config.IngoingTransformers, d.logger, d.name, defaultRoutes, d.ConnID)

	// start goroutine to count dropped messsages
	go d.MonitorLoggers()

	// read incoming dns message
	d.LogInfo("waiting dns message to process...")
RUN_LOOP:
	for {
		select {
		case cfg := <-d.ConfigChan:
			d.config = cfg
			transforms.ReloadConfig(&cfg.IngoingTransformers)

		case <-d.stopRun:
			transforms.Reset()
			d.doneRun <- true
			break RUN_LOOP

		case data, opened := <-d.recvFrom:
			if !opened {
				d.LogInfo("channel closed, exit")
				return
			}

			err := proto.Unmarshal(data, dt)
			if err != nil {
				continue
			}

			// init dns message
			dm := dnsutils.DNSMessage{}
			dm.Init()

			dm.DNSTap.PeerName = d.PeerName

			// init dns message with additionnals parts
			transforms.InitDNSMessageFormat(&dm)

			identity := dt.GetIdentity()
			if len(identity) > 0 {
				dm.DNSTap.Identity = string(identity)
			}
			version := dt.GetVersion()
			if len(version) > 0 {
				dm.DNSTap.Version = string(version)
			}
			dm.DNSTap.Operation = dt.GetMessage().GetType().String()

			// extended extra field ?
			if d.config.Collectors.Dnstap.ExtendedSupport {
				err := proto.Unmarshal(dt.GetExtra(), edt)
				if err != nil {
					continue
				}

				// get original extra value
				originalExtra := string(edt.GetOriginalDnstapExtra())
				if len(originalExtra) > 0 {
					dm.DNSTap.Extra = originalExtra
				}

				// get atags
				atags := edt.GetAtags()
				if atags != nil {
					dm.ATags = &dnsutils.TransformATags{
						Tags: atags.GetTags(),
					}
				}

				// get public suffix
				norm := edt.GetNormalize()
				if norm != nil {
					dm.PublicSuffix = &dnsutils.TransformPublicSuffix{}
					if len(norm.GetTld()) > 0 {
						dm.PublicSuffix.QnamePublicSuffix = norm.GetTld()
					}
					if len(norm.GetEtldPlusOne()) > 0 {
						dm.PublicSuffix.QnameEffectiveTLDPlusOne = norm.GetEtldPlusOne()
					}
				}

				// filtering
				sampleRate := edt.GetFiltering()
				if sampleRate != nil {
					dm.Filtering = &dnsutils.TransformFiltering{}
					dm.Filtering.SampleRate = int(sampleRate.SampleRate)
				}
			} else {
				extra := string(dt.GetExtra())
				if len(extra) > 0 {
					dm.DNSTap.Extra = extra
				}
			}

			if ipVersion, valid := netutils.IPVersion[dt.GetMessage().GetSocketFamily().String()]; valid {
				dm.NetworkInfo.Family = ipVersion
			} else {
				dm.NetworkInfo.Family = pkgconfig.StrUnknown
			}

			dm.NetworkInfo.Protocol = dt.GetMessage().GetSocketProtocol().String()

			// decode query address and port
			queryip := dt.GetMessage().GetQueryAddress()
			if len(queryip) > 0 {
				dm.NetworkInfo.QueryIP = net.IP(queryip).String()
			}
			queryport := dt.GetMessage().GetQueryPort()
			if queryport > 0 {
				dm.NetworkInfo.QueryPort = strconv.FormatUint(uint64(queryport), 10)
			}

			// decode response address and port
			responseip := dt.GetMessage().GetResponseAddress()
			if len(responseip) > 0 {
				dm.NetworkInfo.ResponseIP = net.IP(responseip).String()
			}
			responseport := dt.GetMessage().GetResponsePort()
			if responseport > 0 {
				dm.NetworkInfo.ResponsePort = strconv.FormatUint(uint64(responseport), 10)
			}

			// get dns payload and timestamp according to the type (query or response)
			op := dnstap.Message_Type_value[dm.DNSTap.Operation]
			if op%2 == 1 {
				dnsPayload := dt.GetMessage().GetQueryMessage()
				dm.DNS.Payload = dnsPayload
				dm.DNS.Length = len(dnsPayload)
				dm.DNS.Type = dnsutils.DNSQuery
				dm.DNSTap.TimeSec = int(dt.GetMessage().GetQueryTimeSec())
				dm.DNSTap.TimeNsec = int(dt.GetMessage().GetQueryTimeNsec())
			} else {
				dnsPayload := dt.GetMessage().GetResponseMessage()
				dm.DNS.Payload = dnsPayload
				dm.DNS.Length = len(dnsPayload)
				dm.DNS.Type = dnsutils.DNSReply
				dm.DNSTap.TimeSec = int(dt.GetMessage().GetResponseTimeSec())
				dm.DNSTap.TimeNsec = int(dt.GetMessage().GetResponseTimeNsec())
			}

			// policy
			policyType := dt.GetMessage().GetPolicy().GetType()
			if len(policyType) > 0 {
				dm.DNSTap.PolicyType = policyType
			}

			policyRule := string(dt.GetMessage().GetPolicy().GetRule())
			if len(policyRule) > 0 {
				dm.DNSTap.PolicyRule = policyRule
			}

			policyAction := dt.GetMessage().GetPolicy().GetAction().String()
			if len(policyAction) > 0 {
				dm.DNSTap.PolicyAction = policyAction
			}

			policyMatch := dt.GetMessage().GetPolicy().GetMatch().String()
			if len(policyMatch) > 0 {
				dm.DNSTap.PolicyMatch = policyMatch
			}

			policyValue := string(dt.GetMessage().GetPolicy().GetValue())
			if len(policyValue) > 0 {
				dm.DNSTap.PolicyValue = policyValue
			}

			// decode query zone if provided
			queryZone := dt.GetMessage().GetQueryZone()
			if len(queryZone) > 0 {
				qz, _, err := dnsutils.ParseLabels(0, queryZone)
				if err != nil {
					d.LogError("invalid query zone: %v - %v", err, queryZone)
				}
				dm.DNSTap.QueryZone = qz
			}

			// compute timestamp
			ts := time.Unix(int64(dm.DNSTap.TimeSec), int64(dm.DNSTap.TimeNsec))
			dm.DNSTap.Timestamp = ts.UnixNano()
			dm.DNSTap.TimestampRFC3339 = ts.UTC().Format(time.RFC3339Nano)

			// decode payload if provided
			if !d.config.Collectors.Dnstap.DisableDNSParser && len(dm.DNS.Payload) > 0 {
				// decode the dns payload to get id, rcode and the number of question
				// number of answer, ignore invalid packet
				dnsHeader, err := dnsutils.DecodeDNS(dm.DNS.Payload)
				if err != nil {
					dm.DNS.MalformedPacket = true
					d.LogInfo("dns header parser stopped: %s", err)
					if d.config.Global.Trace.LogMalformed {
						d.LogError("%v", dm)
						d.LogError("dump invalid dns headr: %v", dm.DNS.Payload)
					}
				}

				if err = dnsutils.DecodePayload(&dm, &dnsHeader, d.config); err != nil {
					dm.DNS.MalformedPacket = true
					d.LogInfo("dns payload parser stopped: %s", err)
					if d.config.Global.Trace.LogMalformed {
						d.LogError("%v", dm)
						d.LogError("dump invalid dns payload: %v", dm.DNS.Payload)
					}
				}
			}

			// apply all enabled transformers
			if transforms.ProcessMessage(&dm) == transformers.ReturnDrop {
				for i := range droppedRoutes {
					select {
					case droppedRoutes[i] <- dm: // Successful send to logger channel
					default:
						d.dropped <- droppedNames[i]
					}
				}
				continue
			}

			// convert latency to human
			dm.DNSTap.LatencySec = fmt.Sprintf("%.6f", dm.DNSTap.Latency)

			// dispatch dns message to connected routes
			for i := range defaultRoutes {
				select {
				case defaultRoutes[i] <- dm: // Successful send to logger channel
				default:
					d.dropped <- defaultNames[i]
				}
			}

		}
	}

	d.LogInfo("processing terminated")
}

func (d *DNSTapProcessor) MonitorLoggers() {
	watchInterval := 10 * time.Second
	bufferFull := time.NewTimer(watchInterval)
MONITOR_LOOP:
	for {
		select {
		case <-d.stopMonitor:
			close(d.dropped)
			bufferFull.Stop()
			d.doneMonitor <- true
			break MONITOR_LOOP

		case loggerName := <-d.dropped:
			if _, ok := d.droppedCount[loggerName]; !ok {
				d.droppedCount[loggerName] = 1
			} else {
				d.droppedCount[loggerName]++
			}

		case <-bufferFull.C:
			for v, k := range d.droppedCount {
				if k > 0 {
					d.LogError("logger[%s] buffer is full, %d packet(s) dropped", v, k)
					d.droppedCount[v] = 0
				}
			}
			bufferFull.Reset(watchInterval)

		}
	}
	d.LogInfo("monitor terminated")
}