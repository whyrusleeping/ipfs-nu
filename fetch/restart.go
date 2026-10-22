package fetch

import (
	"errors"
	"net"

	"github.com/whyrusleeping/nu/p2p/host"
)

// Preserve the connection generation on terminal stream errors. One closed
// connection may fail every concurrent range worker; that is one outage, not
// four failed reconnect attempts. QUIC reset errors themselves are zero-sized
// values, so their pointer identity is not a safe generation key.
type connectionError struct {
	error
	conn *host.Conn
}

func (e *connectionError) Unwrap() error { return e.error }
func connectionFailure(err error) *host.Conn {
	var failure *connectionError
	if errors.As(err, &failure) {
		return failure.conn
	}
	return nil
}

type sessionStream struct{ *host.Stream }

func (s sessionStream) failure(err error) error {
	// quic-go can wake stream readers with a terminal connection error just
	// before canceling Conn.Context. Such errors unwrap to net.ErrClosed.
	if err != nil && (errors.Is(err, net.ErrClosed) || s.Conn().Context().Err() != nil) {
		if s.Conn().Context().Err() == nil {
			// Finish closing this exact failed generation before another range
			// worker asks Sessions to reuse it during the cancellation window.
			_ = s.Conn().Close()
		}
		return &connectionError{error: err, conn: s.Conn()}
	}
	return err
}
func (s sessionStream) Read(b []byte) (int, error) {
	n, err := s.Stream.Read(b)
	return n, s.failure(err)
}
func (s sessionStream) Write(b []byte) (int, error) {
	n, err := s.Stream.Write(b)
	return n, s.failure(err)
}
func (s sessionStream) Confirm() error { return s.failure(s.Stream.Confirm()) }
