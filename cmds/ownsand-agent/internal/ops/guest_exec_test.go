// SPDX-License-Identifier: FSL-1.1-ALv2

package ops

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

type execAttemptStream struct {
	sendErr       error
	receive       func() error
	closeResponse func()
	requestClosed chan struct{}
}

func (s *execAttemptStream) Send(*ownsandv1.ServeRequest) error {
	return s.sendErr
}

func (s *execAttemptStream) Receive() (*ownsandv1.ServeResponse, error) {
	return nil, s.receive()
}

func (s *execAttemptStream) CloseRequest() error {
	close(s.requestClosed)
	return nil
}

func (s *execAttemptStream) CloseResponse() error {
	s.closeResponse()
	return nil
}

func TestGuestExecAttemptReleasesStream(t *testing.T) {
	d := &Dispatcher{}
	injected := errors.New("guest stream lost")
	var previous context.Context
	for _, sendErr := range []error{injected, nil} {
		var attempt context.Context
		responseClosed := false
		stream := &execAttemptStream{
			sendErr: sendErr,
			receive: func() error { return injected },
			closeResponse: func() {
				responseClosed = true
				if err := attempt.Err(); err != nil {
					t.Errorf("attempt context ended before response close: %v", err)
				}
			},
			requestClosed: make(chan struct{}),
		}
		done, err := d.runGuestExec(t.Context(), func(ctx context.Context) guestStream {
			if previous != nil && previous.Err() != context.Canceled {
				t.Error("previous attempt still active when the next stream opens")
			}
			attempt = ctx
			guestclient.CloseOnDone(ctx, stream)
			return stream
		}, nil, nil)
		if done || err != injected || !responseClosed {
			t.Fatalf("attempt result: done %t, err %v, response closed %t", done, err, responseClosed)
		}
		if attempt.Err() != context.Canceled || t.Context().Err() != nil {
			t.Fatalf("context errors: attempt %v, exec %v", attempt.Err(), t.Context().Err())
		}
		previous = attempt
		select {
		case <-stream.requestClosed:
		case <-time.After(5 * time.Second):
			t.Fatal("completed attempt did not close the request side")
		}
	}
}

func TestGuestExecAttemptCancellationUnblocksReceive(t *testing.T) {
	d := &Dispatcher{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	receiving := make(chan struct{})
	requestClosed := make(chan struct{})
	stream := &execAttemptStream{
		receive: func() error {
			close(receiving)
			select {
			case <-requestClosed:
				return ctx.Err()
			case <-time.After(5 * time.Second):
				return errors.New("request side stayed open")
			}
		},
		closeResponse: func() {},
		requestClosed: requestClosed,
	}
	result := make(chan error, 1)
	go func() {
		_, err := d.runGuestExec(ctx, func(ctx context.Context) guestStream {
			guestclient.CloseOnDone(ctx, stream)
			return stream
		}, nil, nil)
		result <- err
	}()
	<-receiving
	cancel()
	if err := <-result; err != context.Canceled {
		t.Fatalf("receive after cancel: %v", err)
	}
}
