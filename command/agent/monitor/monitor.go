package monitor

import (
	"context"
	"io"
	"strings"
	"sync"

	log "github.com/hashicorp/go-hclog"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/helper"
	"github.com/ugorji/go/codec"
)

type StreamWriter struct {
	sync.Mutex
	sink         log.MultiSinkLogger
	logger       log.MultiSinkLogger
	logCh        chan []byte
	index        int
	droppedCount int
}

func NewStreamWriter(buf int, sink log.MultiSinkLogger, opts *log.LoggerOptions) *StreamWriter {
	sw := &StreamWriter{
		sink:  sink,
		logCh: make(chan []byte, buf),
		index: 0,
	}

	opts.Output = sw
	logger := log.New(opts).(log.MultiSinkLogger)
	sw.logger = logger

	return sw
}

func (d *StreamWriter) Monitor(ctx context.Context, cancel context.CancelFunc,
	conn io.ReadWriteCloser, enc *codec.Encoder, dec *codec.Decoder) {
	d.sink.RegisterSink(d.logger)
	defer d.sink.DeregisterSink(d.logger)

	// detect the remote side closing
	go func() {
		if _, err := conn.Read(nil); err != nil {
			cancel()
			return
		}
		select {
		case <-ctx.Done():
			return
		}
	}()

	var streamErr error
OUTER:
	for {
		select {
		case log := <-d.logCh:

			var resp cstructs.StreamErrWrapper
			resp.Payload = log
			if err := enc.Encode(resp); err != nil {
				streamErr = err
				break OUTER
			}
			enc.Reset(conn)
		case <-ctx.Done():
			break OUTER
		}
	}

	if streamErr != nil {
		// Nothing to do as conn is closed
		if streamErr == io.EOF || strings.Contains(streamErr.Error(), "closed") {
			return
		}

		// Attempt to send the error
		enc.Encode(&cstructs.StreamErrWrapper{
			Error: cstructs.NewRpcError(streamErr, helper.Int64ToPtr(500)),
		})
		return
	}
}

// Write attemps to send latest log to logCh
// it drops the log if channel is unavailable to receive
func (d *StreamWriter) Write(p []byte) (n int, err error) {
	d.Lock()
	defer d.Unlock()

	select {
	case d.logCh <- p:
	default:
		d.droppedCount++
		if d.droppedCount > 10 {
			d.logger.Warn("Monitor dropped %d logs during monitor request", d.droppedCount)
			d.droppedCount = 0
		}
	}
	return
}
