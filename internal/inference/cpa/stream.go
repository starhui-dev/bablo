package cpa

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	coreexec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"

	"github.com/starhui-dev/bablo/internal/inference"
)

type stream struct {
	chunks  <-chan coreexec.StreamChunk
	headers map[string][]string
	cancel  context.CancelFunc
	once    sync.Once
	closed  atomic.Bool
}

func newStream(result *coreexec.StreamResult, cancel context.CancelFunc) *stream {
	return &stream{chunks: result.Chunks, headers: fromHTTPHeader(result.Headers), cancel: cancel}
}
func (s *stream) Next(ctx context.Context) (inference.StreamEvent, error) {
	if s == nil {
		return inference.StreamEvent{}, errors.New("cpa stream: stream is nil")
	}
	if s.closed.Load() {
		return inference.StreamEvent{Done: true}, io.EOF
	}
	if s.chunks == nil {
		_ = s.Close()
		return inference.StreamEvent{}, &inference.UpstreamError{Class: "empty_stream", Retryable: true}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		_ = s.Close()
		return inference.StreamEvent{}, ctx.Err()
	case chunk, ok := <-s.chunks:
		if !ok {
			_ = s.Close()
			return inference.StreamEvent{Done: true}, io.EOF
		}
		if chunk.Err != nil {
			_ = s.Close()
			return inference.StreamEvent{}, mapError(chunk.Err)
		}
		return inference.StreamEvent{Payload: cloneBytes(chunk.Payload)}, nil
	}
}

func (s *stream) Headers() map[string][]string {
	if s == nil || len(s.headers) == 0 {
		return nil
	}
	result := make(map[string][]string, len(s.headers))
	for key, values := range s.headers {
		result[key] = append([]string(nil), values...)
	}
	return result
}

func (s *stream) Close() error {
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		s.closed.Store(true)
		if s.cancel != nil {
			s.cancel()
		}
		if s.chunks != nil {
			go func() {
				for range s.chunks {
				}
			}()
		}
	})
	return nil
}

var _ inference.Stream = (*stream)(nil)
