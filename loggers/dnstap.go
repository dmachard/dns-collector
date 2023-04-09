package loggers

import (
	"bufio"
	"crypto/tls"
	"net"
	"strconv"
	"time"

	"github.com/dmachard/go-dnscollector/dnsutils"
	"github.com/dmachard/go-dnscollector/transformers"
	"github.com/dmachard/go-framestream"
	"github.com/dmachard/go-logger"
)

type DnstapSender struct {
	done    chan bool
	channel chan dnsutils.DnsMessage
	config  *dnsutils.Config
	logger  *logger.Logger
	exit    chan bool
	conn    net.Conn
	fs      *framestream.Fstrm
	fsReady bool
	name    string
}

func NewDnstapSender(config *dnsutils.Config, logger *logger.Logger, name string) *DnstapSender {
	logger.Info("logger dnstap [%s] sender - enabled", name)
	s := &DnstapSender{
		done:    make(chan bool),
		exit:    make(chan bool),
		channel: make(chan dnsutils.DnsMessage, 512),
		logger:  logger,
		config:  config,
		name:    name,
	}

	s.ReadConfig()

	return s
}

func (c *DnstapSender) GetName() string { return c.name }

func (c *DnstapSender) SetLoggers(loggers []dnsutils.Worker) {}

func (o *DnstapSender) ReadConfig() {
	// get hostname or global one
	if o.config.Loggers.Dnstap.ServerId == "" {
		o.config.Loggers.Dnstap.ServerId = o.config.GetServerIdentity()
	}

	if !dnsutils.IsValidTLS(o.config.Loggers.Dnstap.TlsMinVersion) {
		o.logger.Fatal("logger dnstap - invalid tls min version")
	}
}

func (o *DnstapSender) LogInfo(msg string, v ...interface{}) {
	o.logger.Info("["+o.name+"] logger dnstap sender - "+msg, v...)
}

func (o *DnstapSender) LogError(msg string, v ...interface{}) {
	o.logger.Error("["+o.name+"] logger dnstap sender - "+msg, v...)
}

func (o *DnstapSender) Channel() chan dnsutils.DnsMessage {
	return o.channel
}

func (o *DnstapSender) Stop() {
	o.LogInfo("stopping...")

	// exit to close properly
	o.exit <- true

	// read done channel and block until run is terminated
	<-o.done
	close(o.done)
}

func (o *DnstapSender) Disconnect() {
	if o.conn != nil {
		// reset framestream and ignore errors
		o.LogInfo("closing framestream")
		o.fs.ResetSender()

		// closing tcp
		o.LogInfo("closing tcp connection")
		o.conn.Close()
		o.LogInfo("closed")
	}
}

func (o *DnstapSender) Connect() {
	// prepare the address
	var address string
	var transport string
	if len(o.config.Loggers.Dnstap.SockPath) > 0 {
		address = o.config.Loggers.Dnstap.SockPath
		transport = "unix"
	} else {
		address = net.JoinHostPort(
			o.config.Loggers.Dnstap.RemoteAddress,
			strconv.Itoa(o.config.Loggers.Dnstap.RemotePort),
		)
		transport = dnsutils.SOCKET_TCP
	}

	connTimeout := time.Duration(time.Duration(o.config.Loggers.Dnstap.ConnectTimeout) * time.Second)

	// make the connection
	for {
		o.LogInfo("connecting to %s", address)
		var conn net.Conn
		var err error
		if o.config.Loggers.Dnstap.TlsSupport {
			tlsConfig := &tls.Config{
				InsecureSkipVerify: false,
				MinVersion:         tls.VersionTLS12,
			}
			tlsConfig.InsecureSkipVerify = o.config.Loggers.Dnstap.TlsInsecure
			tlsConfig.MinVersion = dnsutils.TLS_VERSION[o.config.Loggers.Dnstap.TlsMinVersion]

			dialer := &net.Dialer{Timeout: connTimeout}
			conn, err = tls.DialWithDialer(dialer, transport, address, tlsConfig)
		} else {

			conn, err = net.DialTimeout(transport, address, connTimeout)
		}

		// something is wrong during connection ?
		if err != nil {
			o.LogError("%s", err)
		}

		if conn != nil {
			o.LogInfo("connected with success")
			o.conn = conn
			// frame stream library
			r := bufio.NewReader(conn)
			w := bufio.NewWriter(conn)
			o.fs = framestream.NewFstrm(r, w, conn, 5*time.Second, []byte("protobuf:dnstap.Dnstap"), true)

			// init framestream protocol
			if err := o.fs.InitSender(); err != nil {
				o.LogError("sender protocol initialization error %s", err)
			} else {
				o.LogInfo("framestream initialized with success")
				o.fsReady = true
			}
			break
		}
		o.LogInfo("retry to connect in %d seconds", o.config.Loggers.Dnstap.RetryInterval)
		time.Sleep(time.Duration(o.config.Loggers.Dnstap.RetryInterval) * time.Second)
	}
}

func (o *DnstapSender) FlushBuffer(buf *[]dnsutils.DnsMessage) {
	if !o.fsReady {
		o.LogError("%d messages dropped!", len(*buf))
		*buf = nil
		return
	}

	var data []byte
	var err error
	frame := &framestream.Frame{}
	for _, dm := range *buf {
		// update identity ?
		if o.config.Loggers.Dnstap.OverwriteIdentity {
			dm.DnsTap.Identity = o.config.Loggers.Dnstap.ServerId
		}

		// encode dns message to dnstap protobuf binary
		data, err = dm.ToDnstap()
		if err != nil {
			o.LogError("failed to encode to DNStap protobuf: %s", err)
			continue
		}

		// send the frame
		frame.Write(data)
		if err := o.fs.SendFrame(frame); err != nil {
			o.LogError("send frame error %s", err)
			o.fsReady = false
			go o.Connect()
			break
		}
	}

	// remote conn error ?
	if !o.fsReady {
		o.LogError("%d messages dropped!", len(*buf))
	}

	// reset buffer
	*buf = nil
}

func (o *DnstapSender) Run() {
	o.LogInfo("running in background...")

	// prepare transforms
	listChannel := []chan dnsutils.DnsMessage{}
	listChannel = append(listChannel, o.channel)
	subprocessors := transformers.NewTransforms(&o.config.OutgoingTransformers, o.logger, o.name, listChannel)

	// init buffer
	bufferDm := []dnsutils.DnsMessage{}

	// init flust timer for buffer
	flushInterval := time.Duration(o.config.Loggers.Dnstap.FlushInterval) * time.Second
	flushTimer := time.NewTimer(flushInterval)

	// init remote conn
	go o.Connect()

LOOP:
	for {
		select {
		case dm := <-o.channel:
			// apply tranforms
			if subprocessors.ProcessMessage(&dm) == transformers.RETURN_DROP {
				continue
			}

			// append dns message to buffer
			bufferDm = append(bufferDm, dm)

			// buffer is full ?
			if len(bufferDm) >= 5 {
				o.FlushBuffer(&bufferDm)
			}
		case <-flushTimer.C:
			// force to flush the buffer
			if len(bufferDm) > 0 {
				o.FlushBuffer(&bufferDm)
			}

			// restart timer
			flushTimer.Reset(flushInterval)

		case <-o.exit:
			break LOOP
		}
	}

	o.LogInfo("run terminated")

	// cleanup transformers
	subprocessors.Reset()

	// closing remote connection if exist
	o.Disconnect()

	o.done <- true
}

func (o *DnstapSender) RunV0() {
	o.LogInfo("running in background...")

	// prepare transforms
	listChannel := []chan dnsutils.DnsMessage{}
	listChannel = append(listChannel, o.channel)
	subprocessors := transformers.NewTransforms(&o.config.OutgoingTransformers, o.logger, o.name, listChannel)

	//dt := &dnstap.Dnstap{}
	frame := &framestream.Frame{}

LOOP:
	for {
	LOOP_RECONNECT:
		for {
			select {
			case <-o.exit:
				break LOOP
			default:
				// prepare the address
				var address string
				var transport string
				if len(o.config.Loggers.Dnstap.SockPath) > 0 {
					address = o.config.Loggers.Dnstap.SockPath
					transport = "unix"
				} else {
					address = o.config.Loggers.Dnstap.RemoteAddress + ":" + strconv.Itoa(o.config.Loggers.Dnstap.RemotePort)
					transport = dnsutils.SOCKET_TCP
				}

				// make the connection
				o.LogInfo("connecting to %s", address)
				var conn net.Conn
				var err error
				if o.config.Loggers.Dnstap.TlsSupport {
					tlsConfig := &tls.Config{
						InsecureSkipVerify: false,
						MinVersion:         tls.VersionTLS12,
					}
					tlsConfig.InsecureSkipVerify = o.config.Loggers.Dnstap.TlsInsecure
					tlsConfig.MinVersion = dnsutils.TLS_VERSION[o.config.Loggers.Dnstap.TlsMinVersion]

					conn, err = tls.Dial(transport, address, tlsConfig)
				} else {
					conn, err = net.Dial(transport, address)
				}

				// something is wrong during connection ?
				if err != nil {
					o.LogError("connect error: %s", err)
				}

				if conn != nil {
					o.LogInfo("connected with remote")
					o.conn = conn
					// frame stream library
					r := bufio.NewReader(conn)
					w := bufio.NewWriter(conn)
					fs := framestream.NewFstrm(r, w, conn, 5*time.Second, []byte("protobuf:dnstap.Dnstap"), true)

					// init framestream protocol
					if err := fs.InitSender(); err != nil {
						o.LogError("sender protocol initialization error %s", err)
						break
					} else {
						o.LogInfo("framestream initialized")
					}

					var data []byte
					for {
						select {
						case dm := <-o.channel:
							// update identity ?
							if o.config.Loggers.Dnstap.OverwriteIdentity {
								dm.DnsTap.Identity = o.config.Loggers.Dnstap.ServerId
							}

							// encode dns message to dnstap protobuf binary
							data, err = dm.ToDnstap()
							if err != nil {
								o.LogError("failed to encode to DNStap protobuf: %s", err)
								continue
							}

							// send the frame
							frame.Write(data)
							if err := fs.SendFrame(frame); err != nil {
								o.LogError("send frame error %s", err)
								break LOOP_RECONNECT
							}
						case <-o.exit:
							o.LogInfo("closing framestream")
							// reset and ignore errors
							fs.ResetSender()
							break LOOP
						}
					}

				}
				o.LogInfo("retry to connect in 5 seconds")
				time.Sleep(time.Duration(o.config.Loggers.Dnstap.RetryInterval) * time.Second)
			}
		}
	}

	if o.conn != nil {
		o.LogInfo("closing tcp connection")
		o.conn.Close()
	}

	o.LogInfo("run terminated")

	// cleanup transformers
	subprocessors.Reset()

	o.done <- true
}
