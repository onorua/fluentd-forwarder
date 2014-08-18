package fluentd_forwarder

import (
	"bytes"
	"github.com/ugorji/go/codec"
	logging "github.com/op/go-logging"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
	"io"
	"os"
	"math/rand"
	"unsafe"
)

var randSource = rand.NewSource(time.Now().UnixNano())

type ForwardOutput struct {
	logger            *logging.Logger
	codec             *codec.MsgpackHandle
	bind              string
	retryInterval     time.Duration
	connectionTimeout time.Duration
	writeTimeout      time.Duration
	enc               *codec.Encoder
	conn              net.Conn
	flushInterval     time.Duration
	wg                sync.WaitGroup
	journalGroup      JournalGroup
	journal           Journal
	emitterChan       chan FluentRecordSet
	spoolerShutdownChan chan struct{}
	isShuttingDown    unsafe.Pointer
}

func encodeRecordSet(encoder *codec.Encoder, recordSet FluentRecordSet) error {
	v := []interface{}{recordSet.Tag, recordSet.Records}
	err := encoder.Encode(v)
	if err != nil {
		return err
	}
	return err
}

func (output *ForwardOutput) ensureConnected() error {
	if output.conn == nil {
		output.logger.Notice("Connecting to %s...", output.bind)
		conn, err := net.DialTimeout("tcp", output.bind, output.connectionTimeout)
		if err != nil {
			output.logger.Error("Failed to connect to %s (reason: %s)", output.bind, err.Error())
			return err
		} else {
			output.conn = conn
		}
	}
	return nil
}

func (output *ForwardOutput) sendBuffer(buf []byte) error {
	for len(buf) > 0 {
		if atomic.LoadPointer(&output.isShuttingDown) != unsafe.Pointer(uintptr(0)) {
			break
		}
		err := output.ensureConnected()
		if err != nil {
			output.logger.Info("Will be retried in %s", output.retryInterval.String())
			time.Sleep(output.retryInterval)
			continue
		}
		startTime := time.Now()
		if output.writeTimeout == 0 {
			output.conn.SetWriteDeadline(time.Time {})
		} else {
			output.conn.SetWriteDeadline(startTime.Add(output.writeTimeout))
		}
		n, err := output.conn.Write(buf)
		buf = buf[n:]
		if err != nil {
			output.logger.Error("Failed to flush buffer (reason: %s, left: %d bytes)", err.Error(), len(buf))
			err_, ok := err.(net.Error)
			if !ok || (!err_.Timeout() && !err_.Temporary()) {
				return err
			}
		}
		if n > 0 {
			elapsed := time.Now().Sub(startTime)
			output.logger.Info("Forwarded %d bytes in %f seconds (%d bytes left)\n", n, elapsed.Seconds(), len(buf))
		}
	}
	return nil
}

func (output *ForwardOutput) spawnSpooler() {
	output.logger.Notice("Spawning spooler")
	output.wg.Add(1)
	go func() {
		ticker := time.NewTicker(output.flushInterval)
		defer func () {
			ticker.Stop()
			output.journal.Dispose()
			if output.conn != nil {
				output.conn.Close()
			}
			output.conn = nil
			output.wg.Done()
		}()
		output.logger.Notice("Spooler started")
		outer: for {
			select {
			case <-ticker.C:
				buf := make([]byte, 16777216)
				output.logger.Notice("Flushing...")
				err := output.journal.Flush(func(chunk JournalChunk) error {
					defer chunk.Dispose()
					output.logger.Info("Flushing chunk %s", chunk.String())
					reader, err := chunk.GetReader()
					if err != nil {
						return err
					}
					for {
						n, err := reader.Read(buf)
						if n > 0 {
							err_ :=output.sendBuffer(buf[:n])
							if err_ != nil {
								return err
							}
						}
						if err != nil {
							if err == io.EOF {
								break
							} else {
								return err
							}
						}
					}
					return nil
				})
				if err != nil {
					output.logger.Error("Error during reading from the journal: %s", err.Error())
				}
			case <-output.spoolerShutdownChan:
				break outer
			}
		}
		output.logger.Notice("Spooler ended")
	}()
}

func (output *ForwardOutput) spawnEmitter() {
	output.logger.Notice("Spawning emitter")
	output.wg.Add(1)
	go func() {
		defer func() {
			output.spoolerShutdownChan <- struct{}{}
			output.wg.Done()
		}()
		output.logger.Notice("Emitter started")
		buffer := bytes.Buffer{}
		for recordSet := range output.emitterChan {
			buffer.Reset()
			encoder := codec.NewEncoder(&buffer, output.codec)
			err := encodeRecordSet(encoder, recordSet)
			if err != nil {
				output.logger.Error("%s", err.Error())
				continue
			}
			output.logger.Debug("Emitter processed %d entries", len(recordSet.Records))
			output.journal.Write(buffer.Bytes())
		}
		output.logger.Notice("Emitter ended")
	}()
}

func (output *ForwardOutput) Emit(recordSets []FluentRecordSet) error {
	defer func() {
		recover()
	}()
	for _, recordSet := range recordSets {
		output.emitterChan <- recordSet
	}
	return nil
}

func (output *ForwardOutput) String() string {
	return "output"
}

func (output *ForwardOutput) Stop() {
	if atomic.CompareAndSwapPointer(&output.isShuttingDown, unsafe.Pointer(uintptr(0)), unsafe.Pointer(uintptr(1))) {
		close(output.emitterChan)
	}
}

func (output *ForwardOutput) WaitForShutdown() {
	output.wg.Wait()
}

func (output *ForwardOutput) Start() {
	output.spawnSpooler()
	output.spawnEmitter()
}

func NewForwardOutput(logger *logging.Logger, bind string, retryInterval time.Duration, connectionTimeout time.Duration, writeTimeout time.Duration, flushInterval time.Duration, journalGroupPath string, maxJournalChunkSize int64) (*ForwardOutput, error) {
	_codec := codec.MsgpackHandle{}
	_codec.MapType = reflect.TypeOf(map[string]interface{}(nil))
	_codec.RawToString = false
	_codec.StructToArray = true

	journalFactory := NewFileJournalGroupFactory(
		logger,
		randSource,
		time.Now,
		".log",
		os.FileMode(0600),
		maxJournalChunkSize,
	)
	output := &ForwardOutput{
		logger:            logger,
		codec:             &_codec,
		bind:              bind,
		retryInterval:     retryInterval,
		connectionTimeout: connectionTimeout,
		writeTimeout:      writeTimeout,
		wg:                sync.WaitGroup{},
		flushInterval:     flushInterval,
		emitterChan:       make(chan FluentRecordSet),
		spoolerShutdownChan: make(chan struct{}),
		isShuttingDown:    unsafe.Pointer(uintptr(0)),
	}
	journalGroup, err := journalFactory.GetJournalGroup(journalGroupPath, output)
	if err != nil {
		return nil, err
	}
	defer func () {
		err := journalGroup.Dispose()
		if err != nil {
			logger.Error("%#v", err)
		}
	}()
	output.journalGroup  = journalGroup
	output.journal       = journalGroup.GetJournal("output")
	return output, nil
}
