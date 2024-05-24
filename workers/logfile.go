package workers

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dmachard/go-dnscollector/dnsutils"
	"github.com/dmachard/go-dnscollector/pkgconfig"
	"github.com/dmachard/go-dnscollector/transformers"
	"github.com/dmachard/go-logger"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcapgo"

	framestream "github.com/farsightsec/golang-framestream"
)

const (
	compressSuffix = ".gz"
)

func IsValid(mode string) bool {
	switch mode {
	case
		pkgconfig.ModeText,
		pkgconfig.ModeJSON,
		pkgconfig.ModeFlatJSON,
		pkgconfig.ModePCAP,
		pkgconfig.ModeDNSTap:
		return true
	}
	return false
}

type LogFile struct {
	*GenericWorker
	writerPlain                            *bufio.Writer
	writerPcap                             *pcapgo.Writer
	writerDnstap                           *framestream.Encoder
	fileFd                                 *os.File
	fileSize                               int64
	fileDir, fileName, fileExt, filePrefix string
	commpressTimer                         *time.Timer
	textFormat                             []string
}

func NewLogFile(config *pkgconfig.Config, logger *logger.Logger, name string) *LogFile {
	w := &LogFile{GenericWorker: NewGenericWorker(config, logger, name, "file", config.Loggers.LogFile.ChannelBufferSize, pkgconfig.DefaultMonitor)}
	w.ReadConfig()
	if err := w.OpenFile(); err != nil {
		w.LogFatal(pkgconfig.PrefixLogWorker+"["+name+"] file - unable to open output file:", err)
	}
	return w
}

func (w *LogFile) ReadConfig() {
	if !IsValid(w.GetConfig().Loggers.LogFile.Mode) {
		w.LogFatal("["+w.GetName()+"] logger=file - invalid mode: ", w.GetConfig().Loggers.LogFile.Mode)
	}
	w.fileDir = filepath.Dir(w.GetConfig().Loggers.LogFile.FilePath)
	w.fileName = filepath.Base(w.GetConfig().Loggers.LogFile.FilePath)
	w.fileExt = filepath.Ext(w.fileName)
	w.filePrefix = strings.TrimSuffix(w.fileName, w.fileExt)

	if len(w.GetConfig().Loggers.LogFile.TextFormat) > 0 {
		w.textFormat = strings.Fields(w.GetConfig().Loggers.LogFile.TextFormat)
	} else {
		w.textFormat = strings.Fields(w.GetConfig().Global.TextFormat)
	}

	w.LogInfo("running in mode: %s", w.GetConfig().Loggers.LogFile.Mode)
}

func (w *LogFile) Cleanup() error {
	if w.GetConfig().Loggers.LogFile.MaxFiles == 0 {
		return nil
	}

	// remove old files ? keep only max files number
	entries, err := os.ReadDir(w.fileDir)
	if err != nil {
		return err
	}

	logFiles := []int{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		// extract timestamp from filename
		re := regexp.MustCompile(`^` + w.filePrefix + `-(?P<ts>\d+)` + w.fileExt)
		matches := re.FindStringSubmatch(entry.Name())

		if len(matches) == 0 {
			continue
		}

		// convert timestamp to int
		tsIndex := re.SubexpIndex("ts")
		i, err := strconv.Atoi(matches[tsIndex])
		if err != nil {
			continue
		}
		logFiles = append(logFiles, i)
	}
	sort.Ints(logFiles)

	// too much log files ?
	diffNB := len(logFiles) - w.GetConfig().Loggers.LogFile.MaxFiles
	if diffNB > 0 {
		for i := 0; i < diffNB; i++ {
			filename := fmt.Sprintf("%s-%d%s", w.filePrefix, logFiles[i], w.fileExt)
			f := filepath.Join(w.fileDir, filename)
			if _, err := os.Stat(f); os.IsNotExist(err) {
				f = filepath.Join(w.fileDir, filename+compressSuffix)
			}

			// ignore errors on deletion
			os.Remove(f)
		}
	}

	return nil
}

func (w *LogFile) OpenFile() error {

	fd, err := os.OpenFile(w.GetConfig().Loggers.LogFile.FilePath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	w.fileFd = fd

	fileinfo, err := os.Stat(w.GetConfig().Loggers.LogFile.FilePath)
	if err != nil {
		return err
	}

	w.fileSize = fileinfo.Size()

	switch w.GetConfig().Loggers.LogFile.Mode {
	case pkgconfig.ModeText, pkgconfig.ModeJSON, pkgconfig.ModeFlatJSON:
		bufferSize := 4096
		w.writerPlain = bufio.NewWriterSize(fd, bufferSize)

	case pkgconfig.ModePCAP:
		w.writerPcap = pcapgo.NewWriter(fd)
		if w.fileSize == 0 {
			if err := w.writerPcap.WriteFileHeader(65536, layers.LinkTypeEthernet); err != nil {
				return err
			}
		}

	case pkgconfig.ModeDNSTap:
		fsOptions := &framestream.EncoderOptions{ContentType: []byte("protobuf:dnstap.Dnstap"), Bidirectional: false}
		w.writerDnstap, err = framestream.NewEncoder(fd, fsOptions)
		if err != nil {
			return err
		}

	}

	w.LogInfo("file opened with success: %s", w.GetConfig().Loggers.LogFile.FilePath)
	return nil
}

func (w *LogFile) GetMaxSize() int64 {
	return int64(1024*1024) * int64(w.GetConfig().Loggers.LogFile.MaxSize)
}

func (w *LogFile) CompressFile() {
	entries, err := os.ReadDir(w.fileDir)
	if err != nil {
		w.LogError("unable to list all files: %s", err)
		return
	}

	for _, entry := range entries {
		// ignore folder
		if entry.IsDir() {
			continue
		}

		matched, _ := regexp.MatchString(`^`+w.filePrefix+`-\d+`+w.fileExt+`$`, entry.Name())
		if matched {
			src := filepath.Join(w.fileDir, entry.Name())
			dst := filepath.Join(w.fileDir, entry.Name()+compressSuffix)

			fd, err := os.Open(src)
			if err != nil {
				w.LogError("compress - failed to open file: ", err)
				continue
			}
			defer fd.Close()

			fi, err := os.Stat(src)
			if err != nil {
				w.LogError("compress - failed to stat file: ", err)
				continue
			}

			gzf, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode())
			if err != nil {
				w.LogError("compress - failed to open compressed file: ", err)
				continue
			}
			defer gzf.Close()

			gz := gzip.NewWriter(gzf)

			if _, err := io.Copy(gz, fd); err != nil {
				w.LogError("compress - failed to compress file: ", err)
				os.Remove(dst)
				continue
			}
			if err := gz.Close(); err != nil {
				w.LogError("compress - failed to close gz writer: ", err)
				os.Remove(dst)
				continue
			}
			if err := gzf.Close(); err != nil {
				w.LogError("compress - failed to close gz file: ", err)
				os.Remove(dst)
				continue
			}

			if err := fd.Close(); err != nil {
				w.LogError("compress - failed to close log file: ", err)
				os.Remove(dst)
				continue
			}
			if err := os.Remove(src); err != nil {
				w.LogError("compress - failed to remove log file: ", err)
				os.Remove(dst)
				continue
			}

			// post rotate command?
			w.CompressPostRotateCommand(dst)
		}
	}

	w.commpressTimer.Reset(time.Duration(w.GetConfig().Loggers.LogFile.CompressInterval) * time.Second)
}

func (w *LogFile) PostRotateCommand(filename string) {
	if len(w.GetConfig().Loggers.LogFile.PostRotateCommand) > 0 {
		w.LogInfo("execute postrotate command: %s", filename)
		_, err := exec.Command(w.GetConfig().Loggers.LogFile.PostRotateCommand, filename).Output()
		if err != nil {
			w.LogError("postrotate command error: %s", err)
		} else if w.GetConfig().Loggers.LogFile.PostRotateDelete {
			os.Remove(filename)
		}
	}
}

func (w *LogFile) CompressPostRotateCommand(filename string) {
	if len(w.GetConfig().Loggers.LogFile.CompressPostCommand) > 0 {

		w.LogInfo("execute compress postrotate command: %s", filename)
		_, err := exec.Command(w.GetConfig().Loggers.LogFile.CompressPostCommand, filename).Output()
		if err != nil {
			w.LogError("compress - postcommand error: %s", err)
		}
	}
}

func (w *LogFile) FlushWriters() {
	switch w.GetConfig().Loggers.LogFile.Mode {
	case pkgconfig.ModeText, pkgconfig.ModeJSON, pkgconfig.ModeFlatJSON:
		w.writerPlain.Flush()
	case pkgconfig.ModeDNSTap:
		w.writerDnstap.Flush()
	}
}

func (w *LogFile) RotateFile() error {
	// close writer and existing file
	w.FlushWriters()

	if w.GetConfig().Loggers.LogFile.Mode == pkgconfig.ModeDNSTap {
		w.writerDnstap.Close()
	}

	if err := w.fileFd.Close(); err != nil {
		return err
	}

	// Rename current log file
	bfpath := filepath.Join(w.fileDir, fmt.Sprintf("%s-%d%s", w.filePrefix, time.Now().UnixNano(), w.fileExt))
	err := os.Rename(w.GetConfig().Loggers.LogFile.FilePath, bfpath)
	if err != nil {
		return err
	}

	// post rotate command?
	w.PostRotateCommand(bfpath)

	// keep only max files
	err = w.Cleanup()
	if err != nil {
		w.LogError("unable to cleanup log files: %s", err)
		return err
	}

	// re-create new one
	if err := w.OpenFile(); err != nil {
		w.LogError("unable to re-create file: %s", err)
		return err
	}

	return nil
}

func (w *LogFile) WriteToPcap(dm dnsutils.DNSMessage, pkt []gopacket.SerializableLayer) {
	// create the packet with the layers
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}
	for _, layer := range pkt {
		layer.SerializeTo(buf, opts)
	}

	// rotate pcap file ?
	bufSize := len(buf.Bytes())

	if (w.fileSize + int64(bufSize)) > w.GetMaxSize() {
		if err := w.RotateFile(); err != nil {
			w.LogError("failed to rotate file: %s", err)
			return
		}
	}

	ci := gopacket.CaptureInfo{
		Timestamp:     time.Unix(int64(dm.DNSTap.TimeSec), int64(dm.DNSTap.TimeNsec)),
		CaptureLength: bufSize,
		Length:        bufSize,
	}

	w.writerPcap.WritePacket(ci, buf.Bytes())

	// increase size file
	w.fileSize += int64(bufSize)
}

func (w *LogFile) WriteToPlain(data []byte) {
	dataSize := int64(len(data))

	// rotate file ?
	if (w.fileSize + dataSize) > w.GetMaxSize() {
		if err := w.RotateFile(); err != nil {
			w.LogError("failed to rotate file: %s", err)
			return
		}
	}

	// write log to file
	n, _ := w.writerPlain.Write(data)

	// increase size file
	w.fileSize += int64(n)
}

func (w *LogFile) WriteToDnstap(data []byte) {
	dataSize := int64(len(data))

	// rotate file ?
	if (w.fileSize + dataSize) > w.GetMaxSize() {
		if err := w.RotateFile(); err != nil {
			w.LogError("failed to rotate file: %s", err)
			return
		}
	}

	// write log to file
	n, _ := w.writerDnstap.Write(data)

	// increase size file
	w.fileSize += int64(n)
}

func (w *LogFile) StartCollect() {
	w.LogInfo("worker is starting collection")
	defer w.CollectDone()

	// prepare next channels
	defaultRoutes, defaultNames := GetRoutes(w.GetDefaultRoutes())
	droppedRoutes, droppedNames := GetRoutes(w.GetDroppedRoutes())

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

func (w *LogFile) StartLogging() {
	w.LogInfo("worker is starting logging")
	defer w.LoggingDone()

	// prepare some timers
	flushInterval := time.Duration(w.GetConfig().Loggers.LogFile.FlushInterval) * time.Second
	flushTimer := time.NewTimer(flushInterval)
	w.commpressTimer = time.NewTimer(time.Duration(w.GetConfig().Loggers.LogFile.CompressInterval) * time.Second)

	buffer := new(bytes.Buffer)
	var data []byte
	var err error

	for {
		select {
		case <-w.OnLoggerStopped():
			// stop timer
			flushTimer.Stop()
			w.commpressTimer.Stop()

			// flush writer
			w.FlushWriters()

			// closing file
			w.LogInfo("closing log file")
			if w.GetConfig().Loggers.LogFile.Mode == pkgconfig.ModeDNSTap {
				w.writerDnstap.Close()
			}
			w.fileFd.Close()

			return

		case dm, opened := <-w.GetOutputChannel():
			if !opened {
				w.LogInfo("output channel closed!")
				return
			}

			// write to file
			switch w.GetConfig().Loggers.LogFile.Mode {

			// with basic text mode
			case pkgconfig.ModeText:
				w.WriteToPlain(dm.Bytes(w.textFormat,
					w.GetConfig().Global.TextFormatDelimiter,
					w.GetConfig().Global.TextFormatBoundary))

				var delimiter bytes.Buffer
				delimiter.WriteString("\n")
				w.WriteToPlain(delimiter.Bytes())

			// with json mode
			case pkgconfig.ModeFlatJSON:
				flat, err := dm.Flatten()
				if err != nil {
					w.LogError("flattening DNS message failed: %e", err)
				}
				json.NewEncoder(buffer).Encode(flat)
				w.WriteToPlain(buffer.Bytes())
				buffer.Reset()

			// with json mode
			case pkgconfig.ModeJSON:
				json.NewEncoder(buffer).Encode(dm)
				w.WriteToPlain(buffer.Bytes())
				buffer.Reset()

			// with dnstap mode
			case pkgconfig.ModeDNSTap:
				data, err = dm.ToDNSTap(w.GetConfig().Loggers.LogFile.ExtendedSupport)
				if err != nil {
					w.LogError("failed to encode to DNStap protobuf: %s", err)
					continue
				}
				w.WriteToDnstap(data)

			// with pcap mode
			case pkgconfig.ModePCAP:
				pkt, err := dm.ToPacketLayer()
				if err != nil {
					w.LogError("failed to encode to packet layer: %s", err)
					continue
				}

				// write the packet
				w.WriteToPcap(dm, pkt)
			}

		case <-flushTimer.C:
			// flush writer
			w.FlushWriters()

			// reset flush timer and buffer
			buffer.Reset()
			flushTimer.Reset(flushInterval)

		case <-w.commpressTimer.C:
			if w.GetConfig().Loggers.LogFile.Compress {
				w.CompressFile()
			}

		}
	}
}
