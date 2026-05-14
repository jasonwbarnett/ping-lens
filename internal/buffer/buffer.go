// Package buffer is a bounded in-memory channel-backed sink that fans samples
// out to a spool writer and a rollup accumulator without blocking probes.
package buffer

import (
	"context"
	"log"

	"github.com/jasonwbarnett/ping-lens/internal/sample"
)

// Sink consumes samples. Implementations must be safe for sequential use.
type Sink interface {
	Consume(sample.Sample)
}

// Run reads from in and fans every sample out to each sink. Logs and drops
// nothing — backpressure is upstream of the channel.
func Run(ctx context.Context, in <-chan sample.Sample, sinks ...Sink) {
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-in:
			if !ok {
				return
			}
			for _, sk := range sinks {
				safeConsume(sk, s)
			}
		}
	}
}

func safeConsume(sk Sink, s sample.Sample) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("sink panic: %v", r)
		}
	}()
	sk.Consume(s)
}

// SpoolSink wraps a spool.Append-style callback as a Sink.
type SpoolSink struct {
	Append func(sample.Sample) error
}

func (s *SpoolSink) Consume(samp sample.Sample) {
	if err := s.Append(samp); err != nil {
		log.Printf("spool append: %v", err)
	}
}

// FuncSink adapts a plain function to the Sink interface.
type FuncSink func(sample.Sample)

func (f FuncSink) Consume(s sample.Sample) { f(s) }
